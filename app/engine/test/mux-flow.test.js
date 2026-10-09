// Flow tests for lib/mux.js: streams, credit flow control and the receive budget. They mirror the
// cases of internal/mux/flow_test.go, the Go host's tests, and must agree with them.
const test = require('brittle')
const b4a = require('b4a')
const c = require('compact-encoding')
const { Writable } = require('streamx')
const { load } = require('./helpers/vectors.js')
const { messages } = require('../lib/protocol.js')
const { Session, Budget } = require('../lib/mux.js')

const MIB = 1024 * 1024
const CHUNK = 64 * 1024
// SETTLE bounds every wait, so a session that never answers fails its test instead of hanging it.
const SETTLE = 10000

// brittle ends the whole run when a test body throws. Until the IMPL task lands, the code throws
// 'not implemented', so each body runs under catchThrows, which fails only its own test. Bodies are
// async because open and the stream reads are promises.
function catchThrows(fn) {
  return async (t) => {
    try {
      await fn(t)
    } catch (err) {
      t.fail(err.message)
    }
  }
}

const delay = (ms) => new Promise((resolve) => setTimeout(resolve, ms))

// within waits for promise, and fails with what if it has not settled within SETTLE.
function within(promise, what) {
  let timer
  const timeout = new Promise((resolve, reject) => {
    timer = setTimeout(() => reject(new Error(`${what} did not finish within ${SETTLE} ms`)), SETTLE)
  })
  return Promise.race([promise, timeout]).finally(() => clearTimeout(timer))
}

// waitFor polls cond until it holds, and fails with what if it does not hold within SETTLE.
async function waitFor(cond, what) {
  const deadline = Date.now() + SETTLE
  while (!cond()) {
    if (Date.now() > deadline) throw new Error(`${what} was not reached within ${SETTLE} ms`)
    await delay(5)
  }
}

// pattern returns n bytes that differ from their neighbours, so a dropped or repeated byte shows.
function pattern(n) {
  const b = b4a.alloc(n)
  for (let i = 0; i < n; i++) b[i] = i % 251
  return b
}

// direction is one side's sending path. Each message is recorded in sent (its index and message),
// encoded with the protocol codec and decoded again before target receives it, so the tests run the
// frames the wire carries. Frames reach target in order, in a microtask, so a session never receives
// inside its own send. An error from target is recorded in errors.
function direction() {
  const d = { sent: [], errors: [], target: null, queue: [], running: false }
  d.send = (index, message) => {
    const frame = c.encode(messages[index], message)
    d.sent.push({ index, message })
    d.queue.push({ index, frame })
    drain(d)
  }
  return d
}

function drain(d) {
  if (d.running || !d.target) return
  d.running = true
  queueMicrotask(() => {
    while (d.queue.length > 0) {
      const { index, frame } = d.queue.shift()
      try {
        d.target.receive(index, c.decode(messages[index], frame))
      } catch (err) {
        d.errors.push(err)
      }
    }
    d.running = false
  })
}

// recorder is the host's accept callback. It records the service names it is asked for, and it gives
// each accepted stream to the test through target, which the host calls after opened is sent. get(id)
// waits for the host's side of stream id.
function recorder() {
  const r = { services: [], errors: [], streams: new Map(), waiting: new Map() }
  r.accept = (service) => {
    r.services.push(service)
    return {
      target(stream) {
        stream.on('error', (err) => r.errors.push(err))
        r.streams.set(stream.id, stream)
        const wake = r.waiting.get(stream.id)
        if (wake) wake(stream)
      }
    }
  }
  r.get = (id) => new Promise((resolve) => {
    if (r.streams.has(id)) resolve(r.streams.get(id))
    else r.waiting.set(id, resolve)
  })
  return r
}

// pair links an app session and a host session: what one sends, the other receives. The app has a
// 64 MiB budget and the host hostBudget, 256 MiB by default, as in docs/architecture.md, "Limits".
function pair(hostBudget = 256 * MIB) {
  const up = direction()
  const down = direction()
  const hosted = recorder()
  const app = new Session({ role: 'app', send: up.send, budget: new Budget(64 * MIB) })
  const host = new Session({ role: 'host', send: down.send, budget: new Budget(hostBudget), accept: hosted.accept })
  up.target = host
  down.target = app
  return { app, host, up, down, hosted, failures: [] }
}

// alone is a host with nothing on the other side. The test calls its receive itself and reads what it
// sends in down.sent.
function alone(hostBudget) {
  const down = direction()
  const hosted = recorder()
  const host = new Session({ role: 'host', send: down.send, budget: new Budget(hostBudget), accept: hosted.accept })
  return { host, down, hosted }
}

// openStream opens a stream to web on the app. An error the stream emits is kept in failures.
async function openStream(h) {
  const stream = await within(h.app.open('web'), 'open()')
  stream.on('error', (err) => h.failures.push(err))
  return stream
}

// sumData returns the payload bytes of the data messages a side has sent.
const sumData = (side) => side.sent
  .filter((m) => m.index === 3)
  .reduce((n, m) => n + m.message.payload.length, 0)

// windowsFor returns the window messages a side has sent for stream id.
const windowsFor = (side, id) => side.sent
  .filter((m) => m.index === 4 && m.message.stream === id)
  .map((m) => m.message)

// failuresOf lists the messages of every error the pair's sides saw, so a failed assertion shows them.
const failuresOf = (h) => [...h.up.errors, ...h.down.errors, ...h.hosted.errors, ...h.failures]
  .map((err) => err.message)

// readBytes takes bytes from stream as they arrive until n are read, then pauses the stream, so the
// test controls how much it reads. It resolves with the bytes read.
function readBytes(stream, n) {
  return new Promise((resolve, reject) => {
    const parts = []
    let total = 0
    stream.on('error', reject)
    stream.on('data', (chunk) => {
      if (total >= n) return
      parts.push(chunk)
      total += chunk.length
      if (total >= n) {
        stream.pause()
        resolve(b4a.concat(parts))
      }
    })
  })
}

// writeAll writes data to stream and resolves once the stream has taken all of it.
function writeAll(stream, data) {
  return new Promise((resolve, reject) => {
    stream.write(data, (err) => (err ? reject(err) : resolve()))
  })
}

// outcome resolves with the error a stream ends with, or with a null error when it ends cleanly. Its
// listeners are attached at once, so an error that comes early is not lost.
function outcome(stream) {
  return new Promise((resolve) => {
    stream.on('error', (error) => resolve({ error }))
    stream.on('end', () => resolve({ error: null }))
    stream.on('close', () => resolve({ error: null }))
  })
}

// Case 1: open() sends open with a 2 MiB window. The host's accept gets the service, and the host
// answers opened for the same stream, with the 2 MiB window.
test('open() sends open with a 2 MiB window; accept gets the service and opened comes back', catchThrows(async (t) => {
  const h = pair()
  const stream = await openStream(h)
  t.is(h.up.sent.length, 1, 'the app sent one message')
  t.is(h.up.sent[0].index, 0, 'the message is open, index 0')
  t.alike(h.up.sent[0].message, { stream: stream.id, service: 'web', window: 2 * MIB },
    'open carries the stream, the service and a 2 MiB window')
  t.alike(h.hosted.services, ['web'], 'the host accept ran with the service name')
  const answers = h.down.sent.filter((m) => m.index === 1)
  t.is(answers.length, 1, 'the host answered once with opened')
  t.is(answers[0].message.stream, stream.id, 'opened is for the same stream')
  t.is(answers[0].message.window, 2 * MIB, 'opened grants a 2 MiB window')
  t.alike(failuresOf(h), [], 'no session errors')
}))

// Case 2: 10 MB written on the app's side arrives intact at the host's side.
test('10 MB written at the app arrives intact at the host', catchThrows(async (t) => {
  const h = pair()
  const stream = await openStream(h)
  const host = await within(h.hosted.get(stream.id), 'the host stream')
  const want = pattern(10 * MIB)
  const [, got] = await within(
    Promise.all([writeAll(stream, want), readBytes(host, want.length)]),
    'the 10 MB transfer'
  )
  t.is(got.length, want.length, 'all 10 MB arrive')
  t.ok(b4a.equals(got, want), 'the 10 MB arrive unaltered')
  t.alike(failuresOf(h), [], 'no session errors')
}))

// Case 3: with the host not reading, the app's write blocks after exactly the 2 MiB the host granted.
test('without reading, the writer stops after exactly the granted 2 MiB', catchThrows(async (t) => {
  const h = pair()
  const stream = await openStream(h)
  let finished = false
  stream.write(pattern(4 * MIB), () => {
    finished = true
  })
  await waitFor(() => sumData(h.up) >= 2 * MIB, 'the writer sends its 2 MiB window')
  await delay(200) // the writer has time to go past the window if it is going to
  t.is(sumData(h.up), 2 * MIB, 'exactly the granted 2 MiB is sent before the writer blocks')
  t.is(finished, false, 'the write has not finished')
  t.alike(failuresOf(h), [], 'no session errors')
}))

// Case 4: the host's reader takes 64 KiB, so the host sends a window message with credit and its
// received total, and the app's write goes on.
test('reading sends window credit and the writer resumes', catchThrows(async (t) => {
  const h = pair()
  const stream = await openStream(h)
  const host = await within(h.hosted.get(stream.id), 'the host stream')
  stream.write(pattern(4 * MIB), () => {})
  await waitFor(() => sumData(h.up) >= 2 * MIB, 'the writer sends its 2 MiB window')
  await delay(200) // the writer is blocked at 2 MiB
  await within(readBytes(host, CHUNK), 'the host reads 64 KiB')
  await waitFor(() => windowsFor(h.down, stream.id).length > 0, 'the host sends window credit')
  const [w] = windowsFor(h.down, stream.id)
  t.ok(w.credit > 0, 'the window message grants credit')
  t.is(w.received, 2 * MIB, 'the window reports the 2 MiB received so far')
  await waitFor(() => sumData(h.up) > 2 * MIB, 'the writer resumes past its window')
  t.alike(failuresOf(h), [], 'no session errors')
}))

// Case 5: a peer that sends past its credit has that stream reset with an error. The reset also sends
// close for the stream, since the protocol has no reset message. The session stays up, and another
// stream on it carries on.
test('data beyond credit resets only that stream; the session stays up', catchThrows(async (t) => {
  const { host, down, hosted } = alone(256 * MIB)
  host.receive(0, { stream: 1, service: 'web', window: 2 * MIB })
  host.receive(0, { stream: 2, service: 'web', window: 2 * MIB })
  const one = await within(hosted.get(1), 'the host stream 1')
  const ended = outcome(one)
  // Stream 1 gets exactly its 2 MiB of credit, then one byte more.
  for (let sent = 0; sent < 2 * MIB; sent += CHUNK) {
    host.receive(3, { stream: 1, payload: pattern(CHUNK) })
  }
  host.receive(3, { stream: 1, payload: b4a.from([1]) })
  const { error } = await within(ended, 'stream 1 ending')
  t.ok(error, 'stream 1 ends with an error, not a clean end')
  t.ok(down.sent.some((m) => m.index === 5 && m.message.stream === 1), 'the reset sends close for stream 1')
  // Stream 2 carries on.
  const two = await within(hosted.get(2), 'the host stream 2')
  const want = pattern(1024)
  host.receive(3, { stream: 2, payload: want })
  const got = await within(readBytes(two, want.length), 'stream 2 at the host')
  t.ok(b4a.equals(got, want), 'stream 2 arrives unaltered')
}))

// Case 6: with a 4 MiB budget and four open streams, the grants follow the budget. The first stream,
// opened alone, gets its full window. No grant after it exceeds the 1 MiB share, and the grants total
// at most the budget. No window is sent until a stream is drained, and then a read brings a new grant.
test('with a 4 MiB budget and 4 streams, grants follow the budget and draining', catchThrows(async (t) => {
  const { host, down, hosted } = alone(4 * MIB)
  for (let id = 1; id <= 4; id++) host.receive(0, { stream: id, service: 'web', window: 2 * MIB })
  const grants = down.sent.filter((m) => m.index === 1).map((m) => m.message.window)
  t.is(grants.length, 4, 'the host answered all 4 opens')
  t.is(grants[0], 2 * MIB, 'the first stream, opened alone, gets its full window')
  for (const g of grants.slice(1)) t.ok(g <= MIB, `grant ${g} is at most the 1 MiB share`)
  t.ok(grants.reduce((total, g) => total + g, 0) <= 4 * MIB, 'the grants total at most the 4 MiB budget')
  t.is(down.sent.filter((m) => m.index === 4).length, 0, 'no window is sent before a stream is drained')
  // Draining stream 1: the app sends 64 KiB, the host's reader takes it, and the host grants again.
  host.receive(3, { stream: 1, payload: pattern(CHUNK) })
  const one = await within(hosted.get(1), 'the host stream 1')
  await within(readBytes(one, CHUNK), 'the host reads 64 KiB')
  await waitFor(() => windowsFor(down, 1).length > 0, 'the host grants after the read')
  const [w] = windowsFor(down, 1)
  t.ok(w.credit > 0 && w.credit <= MIB, `the grant after the read is ${w.credit}, from 1 byte to 1 MiB`)
}))

// Case 7: a 200 KiB write is sent as data messages of at most 65536 bytes, which together carry all of
// it. The 2 MiB window needs no read, so the write completes.
test('a 200 KiB write is sent as data messages of at most 65536 bytes', catchThrows(async (t) => {
  const h = pair()
  const stream = await openStream(h)
  const payload = pattern(200 * 1024)
  await within(writeAll(stream, payload), 'the 200 KiB write')
  for (const m of h.up.sent.filter((s) => s.index === 3)) {
    t.ok(m.message.payload.length <= CHUNK, `a data message of ${m.message.payload.length} bytes is at most 65536`)
  }
  t.is(sumData(h.up), payload.length, 'the data messages carry all 200 KiB')
  t.alike(failuresOf(h), [], 'no session errors')
}))

// Agreement with spec/vectors/frames.json: the host's answer to an open for stream 253 is the opened
// frame the vectors give for stream 253, byte for byte.
test('the host answer to open is the opened frame in spec/vectors/frames.json', catchThrows(async (t) => {
  const { host, down } = alone(256 * MIB)
  host.receive(0, { stream: 253, service: 'ssh', window: 2 * MIB })
  const answer = down.sent.find((m) => m.index === 1)
  t.ok(answer, 'the host answered with opened')
  const vector = load('frames.json').find((f) => f.name === 'opened' && f.value.stream === 253)
  t.is(b4a.toString(c.encode(messages[1], answer.message), 'hex'), vector.hex,
    'the opened frame is the vector bytes for stream 253')
}))

// A received payload is copied (HoleBridge-eon.7): the stream keeps its own bytes, not a view of the frame or
// of the transport's read buffer, which the transport reuses for the next read.
test('a received payload is copied, so the stream holds no view of the transport buffer', catchThrows(async (t) => {
  const { host, hosted } = alone(256 * MIB)
  host.receive(0, { stream: 1, service: 'web', window: 2 * MIB })
  const stream = await within(hosted.get(1), 'the host stream 1')
  const readBuffer = b4a.alloc(CHUNK, 0x2a)
  host.receive(3, { stream: 1, payload: readBuffer.subarray(100, 101) })
  const [chunk] = stream.chunks
  t.not(chunk.buffer, readBuffer.buffer, 'the buffered payload does not share the read buffer')
  t.is(chunk.buffer.byteLength, 1, 'the buffered payload holds only its own byte')
  readBuffer.fill(0) // the transport reuses its read buffer
  const got = await within(readBytes(stream, 1), 'the host reads the byte')
  t.ok(b4a.equals(got, b4a.from([0x2a])), 'the stream still delivers the byte it received')
}))

// A stream piped into a destination that applies backpressure delivers every byte (HoleBridge-hb5.1.4): streamx
// stops reading a highWaterMark 0 stream when a write returns false, and reads again only when asked.
test('a stream piped into a backpressured destination delivers every byte', catchThrows(async (t) => {
  const h = pair()
  const stream = await openStream(h)
  const host = await within(h.hosted.get(stream.id), 'the host stream')
  const got = []
  const dest = new Writable({
    highWaterMark: 16 * 1024,
    write(chunk, cb) {
      got.push(chunk)
      setTimeout(cb, 1) // a slow socket: each write completes a little later
    }
  })
  host.pipe(dest)
  const want = pattern(150 * 1024)
  await within(writeAll(stream, want), 'the 150 KiB write')
  await waitFor(() => b4a.concat(got).length >= want.length, 'the destination receives 150 KiB')
  t.ok(b4a.equals(b4a.concat(got), want), 'every byte arrives in order')
  t.alike(failuresOf(h), [], 'no session errors')
}))

// A stream admitted with no credit is granted once another stream frees the budget (HoleBridge-eon.8). The host
// budget holds 2 MiB: the first stream takes all of it, so the second is admitted with 0. Closing the first on
// both sides frees its credit, and the second is granted its window.
test('a stream admitted with no credit is granted once another stream frees the budget', catchThrows(async (t) => {
  const h = pair(2 * MIB)
  const first = await openStream(h)
  const second = await openStream(h)
  const hostFirst = await within(h.hosted.get(first.id), 'the host stream 1')
  const hostSecond = await within(h.hosted.get(second.id), 'the host stream 2')
  const admitted = h.down.sent.find((m) => m.index === 1 && m.message.stream === second.id)
  t.is(admitted.message.window, 0, 'the second stream is admitted with no credit')
  const written = writeAll(second, b4a.from('hi'))
  first.destroy()
  hostFirst.destroy()
  await waitFor(() => windowsFor(h.down, second.id).length > 0, 'the host grants the second stream credit')
  const [w] = windowsFor(h.down, second.id)
  t.ok(w.credit > 0, `the grant to the second stream is ${w.credit} bytes`)
  await within(written, 'the write on the second stream')
  const got = await within(readBytes(hostSecond, 2), 'the host reads the write')
  t.ok(b4a.equals(got, b4a.from('hi')), 'the second stream carries its bytes')
  t.alike(failuresOf(h), [], 'no session errors')
}))
