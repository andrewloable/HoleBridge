// Close, reject, limit and malformed-input tests for lib/mux.js: the close handshake, rejects, the stream
// limits, the 60 s memory of finished ids, session destroy and a cancelled open. They mirror the cases of
// internal/mux/close_test.go, the Go host's tests, and must agree with them. They fail until
// HoleBridge-eon.4 is implemented: Counter and Session.destroy are stubs, and the close, limit and cancel
// paths are not written yet. Cases 3, 6 and 7 pass on the code of HoleBridge-eon.2 already; they stay as
// regression guards (the Go twins of cases 3 and 7 pass too).
const test = require('brittle')
const b4a = require('b4a')
const c = require('compact-encoding')
const { messages } = require('../lib/protocol.js')
const { Session, Budget, Counter, SessionError, RejectError } = require('../lib/mux.js')

const MIB = 1024 * 1024
const CHUNK = 64 * 1024
// SETTLE bounds every wait, so a session that never answers fails its test instead of hanging it.
const SETTLE = 10000

// brittle ends the whole run when a test body throws. Until the IMPL task lands, the stubs throw
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

// attempt runs fn and returns the error it throws, or null when it does not throw.
function attempt(fn) {
  try {
    fn()
  } catch (err) {
    return err
  }
  return null
}

// thrownBy names the error attempt returned, for a failure message.
const thrownBy = (err) => (err ? err.message : 'nothing')

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

// recorder is the host's accept callback. It gives each accepted stream to the test through target, which
// the host calls after opened is sent. get(id) waits for the host's side of stream id. An error the host's
// stream emits is kept in errors.
function recorder() {
  const r = { errors: [], streams: new Map(), waiting: new Map() }
  r.accept = () => ({
    target(stream) {
      stream.on('error', (err) => r.errors.push(err))
      r.streams.set(stream.id, stream)
      const wake = r.waiting.get(stream.id)
      if (wake) wake(stream)
    },
  })
  r.get = (id) => new Promise((resolve) => {
    if (r.streams.has(id)) resolve(r.streams.get(id))
    else r.waiting.set(id, resolve)
  })
  return r
}

// link pairs an app session and a host session: what one sends, the other receives. hostOpts and appOpts
// add to each session's options (accept, counter, clock, window, maxStreams). The app has a 64 MiB budget
// and the host 256 MiB, as in docs/architecture.md, "Limits".
function link(hostOpts = {}, appOpts = {}) {
  const up = direction()
  const down = direction()
  const hosted = recorder()
  const app = new Session({ role: 'app', send: up.send, budget: new Budget(64 * MIB), ...appOpts })
  const host = new Session({
    role: 'host',
    send: down.send,
    budget: new Budget(256 * MIB),
    accept: hosted.accept,
    ...hostOpts,
  })
  up.target = host
  down.target = app
  return { app, host, up, down, hosted, failures: [] }
}

// alone is one session with nothing on the other side. The test calls its receive itself and reads what it
// sends in out.sent.
function alone(role, opts = {}) {
  const out = direction()
  const session = new Session({
    role,
    send: out.send,
    budget: new Budget(role === 'app' ? 64 * MIB : 256 * MIB),
    accept: () => ({ code: 0 }),
    ...opts,
  })
  return { session, out }
}

// fakeSignal returns an abort signal stand-in with aborted, addEventListener and removeEventListener, since
// Bare has no AbortController. listeners holds the 'abort' listeners still registered. abort() sets aborted
// and calls them, as an AbortSignal does.
function fakeSignal() {
  const listeners = []
  const signal = {
    aborted: false,
    addEventListener(type, fn) {
      if (type === 'abort') listeners.push(fn)
    },
    removeEventListener(type, fn) {
      const i = listeners.indexOf(fn)
      if (type === 'abort' && i >= 0) listeners.splice(i, 1)
    },
  }
  return {
    signal,
    listeners,
    abort() {
      signal.aborted = true
      for (const fn of [...listeners]) fn()
    },
  }
}

// openStream opens a stream to service on the app. An error the stream emits is kept in failures.
async function openStream(h, service = 'web') {
  const stream = await within(h.app.open(service), 'open()')
  stream.on('error', (err) => h.failures.push(err))
  return stream
}

// writeAll writes data to stream and resolves once the stream has taken all of it.
function writeAll(stream, data) {
  return new Promise((resolve, reject) => {
    stream.write(data, (err) => (err ? reject(err) : resolve()))
  })
}

// readBytes takes bytes from stream as they arrive until n are read, then pauses the stream.
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

// readAll takes every byte of stream until its end event, and resolves with them.
function readAll(stream) {
  return new Promise((resolve, reject) => {
    const parts = []
    stream.on('error', reject)
    stream.on('data', (chunk) => parts.push(chunk))
    stream.on('end', () => resolve(b4a.concat(parts)))
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

// sumData returns the payload bytes of the data messages a side has sent.
const sumData = (side) => side.sent
  .filter((m) => m.index === 3)
  .reduce((n, m) => n + m.message.payload.length, 0)

// sentFor reports whether side has sent a message with index for stream id.
const sentFor = (side, index, id) => side.sent
  .some((m) => m.index === index && m.message.stream === id)

// failuresOf lists the messages of every error the pair's sides saw, so a failed assertion shows them.
const failuresOf = (h) => [...h.up.errors, ...h.down.errors, ...h.hosted.errors, ...h.failures]
  .map((err) => err.message)

// Case 1: end() on the app's side gives the host all its data and then end. The host can still write back,
// and the app reads it (half-close).
test('end() gives the other side all data, then end; the other side can still write', catchThrows(async (t) => {
  const h = link()
  const stream = await openStream(h)
  const host = await within(h.hosted.get(stream.id), 'the host stream')
  const want = pattern(10 * 1024)
  await within(writeAll(stream, want), 'the 10 KiB write')
  stream.end()
  const got = await within(readAll(host), 'the host reading to end')
  t.ok(b4a.equals(got, want), 'the host reads all 10 KiB before end')
  const reply = b4a.from('pong')
  await within(writeAll(host, reply), 'the host writing back after end')
  const back = await within(readBytes(stream, reply.length), 'the app reading the reply')
  t.ok(b4a.equals(back, reply), 'the app reads what the host wrote after the app ended')
  t.alike(failuresOf(h), [], 'no session errors')
}))

// Case 2: both ends close the stream. Each side's stream count is 1 while the stream is open and goes back
// to 0 once both closes have come back. Each session has its own counter, so each end is checked on its own.
test('both sides closing frees the stream on both ends', catchThrows(async (t) => {
  const appCounter = new Counter(1024)
  const hostCounter = new Counter(1024)
  const h = link({ counter: hostCounter }, { counter: appCounter })
  const stream = await openStream(h)
  const host = await within(h.hosted.get(stream.id), 'the host stream')
  t.is(appCounter.used, 1, 'after open, the app holds 1 stream slot')
  t.is(hostCounter.used, 1, 'after open, the host holds 1 stream slot')
  stream.destroy()
  host.destroy()
  await waitFor(() => appCounter.used === 0, 'the app slot given back after both closes')
  await waitFor(() => hostCounter.used === 0, 'the host slot given back after both closes')
  t.alike(failuresOf(h), [], 'no session errors')
}))

// Case 3: the host rejects the service with code 1 (unknown service). open() rejects with a RejectError that
// carries the code and the reason.
test('accept rejecting with code 1 makes open() reject with code 1 and the reason', catchThrows(async (t) => {
  const h = link({ accept: () => ({ code: 1, reason: 'unknown service' }) })
  const err = await within(h.app.open('nope').then(() => null, (e) => e), 'open() of an unknown service')
  t.ok(err instanceof RejectError, 'open rejects with a RejectError')
  t.is(err && err.code, 1, 'the code is 1')
  t.is(err && err.reason, 'unknown service', 'the reason is the one accept gave')
}))

// Case 4: a session admits 128 streams (maxStreams, the default). The 129th open is refused with code 2,
// limit reached.
test('the 129th concurrent stream in a session is rejected with code 2', catchThrows(async (t) => {
  const h = link()
  for (let i = 1; i <= 128; i++) await openStream(h)
  const err = await within(h.app.open('web').then(() => null, (e) => e), 'the 129th open()')
  t.ok(err instanceof RejectError, 'the 129th open rejects with a RejectError')
  t.is(err && err.code, 2, 'the code is 2, limit reached')
}))

// Case 5: two app sessions on one host share a counter of 3 streams. Three streams open across the two
// sessions. A fourth, on the second session, is refused with code 2.
test('a shared counter of 3 across two sessions rejects the fourth stream with code 2', catchThrows(async (t) => {
  const shared = new Counter(3)
  const first = link({ counter: shared })
  const second = link({ counter: shared })
  await openStream(first)
  await openStream(first)
  await openStream(second)
  const err = await within(second.app.open('web').then(() => null, (e) => e), 'the fourth open()')
  t.ok(err instanceof RejectError, 'the fourth stream is refused with a RejectError')
  t.is(err && err.code, 2, 'the code is 2, limit reached')
}))

// Case 6: the host sends data on a stream the app has opened, before opened. That resets this stream only:
// its open() ends with an error, the session stays up, and a second stream opens and carries on.
test('data before opened resets only that stream', catchThrows(async (t) => {
  const { session: app, out } = alone('app')
  const first = app.open('web').then(() => null, (e) => e)
  await waitFor(() => sentFor(out, 0, 1), 'the open for stream 1')
  const early = attempt(() => app.receive(3, { stream: 1, payload: b4a.from('early') }))
  t.ok(!early, `data before opened does not end the session: ${thrownBy(early)}`)
  const err = await within(first, 'the open of stream 1')
  t.ok(err instanceof Error, 'the open of stream 1 ends with an error: the stream was reset')
  t.ok(sentFor(out, 5, 1), 'the reset sends close for stream 1')
  const second = app.open('web').catch((e) => e)
  await waitFor(() => sentFor(out, 0, 2), 'the open for stream 2')
  const stray = attempt(() => app.receive(1, { stream: 2, window: 2 * MIB }))
  t.ok(!stray, `opened for stream 2 is accepted after the reset: ${thrownBy(stray)}`)
  const st = await within(second, 'the open of stream 2')
  t.is(st && st.id, 2, 'stream 2 opens and carries on after the reset')
}))

// Case 7: data for a stream id the host never opened is an error from receive(), a SessionError, which
// closes the session.
test('data for a never-opened stream id throws SessionError from receive()', catchThrows(async (t) => {
  const { session: host } = alone('host')
  const err = attempt(() => host.receive(3, { stream: 9, payload: b4a.from('x') }))
  t.ok(err instanceof SessionError, `receive throws a SessionError, got ${thrownBy(err)}`)
}))

// Case 8: the app's close ends the host's read side of stream 1. The host's own close answers it only when the
// host ends its side, and then the stream is finished. For 60 s after that the id is remembered: a late window
// and a late close for it are ignored. The clock is the session's clock option, moved by the test.
test('window or close for a stream closed less than 60 s ago is ignored', catchThrows(async (t) => {
  let now = Date.UTC(2026, 9, 8, 12, 0, 0)
  let hostSide = null
  const accept = () => ({ target: (st) => { hostSide = st } })
  const { session: host, out } = alone('host', { clock: () => now, accept })
  host.receive(0, { stream: 1, service: 'web', window: 2 * MIB })
  const closed = attempt(() => host.receive(5, { stream: 1 }))
  t.ok(!closed, `the app's close does not end the session: ${thrownBy(closed)}`)
  t.absent(sentFor(out, 5, 1), 'the host holds its close while its side is open')
  hostSide.end()
  await waitFor(() => sentFor(out, 5, 1), 'the host closes stream 1 once its side ends')
  now += 59 * 1000
  const sent = out.sent.length
  const late = attempt(() => host.receive(4, { stream: 1, credit: CHUNK, received: 0 }))
  t.ok(!late, `a window 59 s after the close is ignored: ${thrownBy(late)}`)
  const lateClose = attempt(() => host.receive(5, { stream: 1 }))
  t.ok(!lateClose, `a close 59 s after the close is ignored: ${thrownBy(lateClose)}`)
  t.is(out.sent.length, sent, 'the ignored messages get no answer')
  now += 1000
  const forgotten = attempt(() => host.receive(4, { stream: 1, credit: CHUNK, received: 0 }))
  t.ok(forgotten instanceof SessionError, 'at 60 s the id is forgotten, so a window for it ends the session')
}))

// Case 9: session destroy fails every open stream. One stream sits idle, and another is blocked in write at
// its 2 MiB window because the host does not read. Both end with an error, and the blocked write fails.
test('session.destroy() errors every open stream', catchThrows(async (t) => {
  const h = link()
  const idle = await openStream(h)
  const idleEnd = outcome(idle)
  const blocked = await openStream(h)
  const blockedEnd = outcome(blocked)
  const blockedWrite = new Promise((resolve) => {
    blocked.write(pattern(4 * MIB), (err) => resolve(err || null))
  })
  await waitFor(() => sumData(h.up) >= 2 * MIB, 'the blocked write sends its 2 MiB window')
  await delay(200) // the writer has time to go past the window if it is going to
  t.is(sumData(h.up), 2 * MIB, 'the blocked write stops at the granted 2 MiB')
  h.app.destroy()
  const idleResult = await within(idleEnd, 'the idle stream ending')
  t.ok(idleResult.error, 'the idle stream ends with an error after the session is destroyed')
  const blockedResult = await within(blockedEnd, 'the blocked stream ending')
  t.ok(blockedResult.error, 'the blocked stream ends with an error after the session is destroyed')
  const writeErr = await within(blockedWrite, 'the blocked write callback')
  t.ok(writeErr, 'the blocked write fails with an error after the session is destroyed')
}))

// Cancelled open (not one of the nine cases, as in internal/mux/close_test.go): an open whose signal aborts
// before the host answers gives its stream slot back and sends close for its stream, so the host frees its
// side. A late opened for that stream is ignored and the session stays up. The docs do not fix this
// cleanup; the test records the shape the task notes choose (open takes an optional signal, see the notes).
test('a cancelled open gives its slot back, sends close, and a late opened is ignored', catchThrows(async (t) => {
  const counter = new Counter(1024)
  const { session: app, out } = alone('app', { counter })
  const sig = fakeSignal()
  const result = app.open('web', { signal: sig.signal }).then(() => null, (e) => e)
  await waitFor(() => sentFor(out, 0, 1), 'the open for stream 1')
  t.is(counter.used, 1, 'the open holds a stream slot while it waits')
  sig.abort()
  const err = await within(result, 'the cancelled open()')
  t.ok(err instanceof Error, 'the cancelled open rejects with an error')
  await waitFor(() => counter.used === 0, 'the slot given back after the cancel')
  t.ok(sentFor(out, 5, 1), 'close is sent for the cancelled stream 1')
  const late = attempt(() => app.receive(1, { stream: 1, window: 2 * MIB }))
  t.ok(!late, `a late opened for the cancelled stream is ignored: ${thrownBy(late)}`)
}))

// Half-close with a close between two reads (HoleBridge-hb5.16.13): the app's close lands while the host has
// read the request and is not reading, so no read waits. The host can still write the reply, and the app reads it.
test('a close that lands between two reads leaves the host able to write', catchThrows(async (t) => {
  const h = link()
  const stream = await openStream(h)
  const host = await within(h.hosted.get(stream.id), 'the host stream')
  await within(writeAll(stream, b4a.from('GET')), 'the request')
  await within(readBytes(host, 3), 'the host reads the request')
  stream.end()
  await delay(100) // the close reaches the host while it is between reads
  const reply = b4a.from('resp')
  const writeErr = await within(writeAll(host, reply).then(() => null, (err) => err), 'the host writing the reply')
  t.is(writeErr, null, 'the host can still write the reply after the app closed its side')
  const back = await within(readBytes(stream, reply.length), 'the app reading the reply')
  t.ok(b4a.equals(back, reply), 'the app reads the reply')
  t.alike(failuresOf(h), [], 'no session errors')
}))

// destroy() of a stream whose write waits for credit (HoleBridge-eon.9): the write fails at once, close is sent
// at once, the slot is given back, and nothing more is sent after the destroy.
test('destroy() of a stream blocked on credit sends close at once and frees its slot', catchThrows(async (t) => {
  const appCounter = new Counter(1024)
  const h = link({}, { counter: appCounter })
  const stream = await openStream(h)
  t.is(appCounter.used, 1, 'after open, the app holds 1 stream slot')
  const writeErr = new Promise((resolve) => {
    stream.write(pattern(4 * MIB), (err) => resolve(err || null))
  })
  await waitFor(() => sumData(h.up) >= 2 * MIB, 'the write sends its 2 MiB window')
  await delay(200) // the writer has time to go past the window if it is going to
  stream.destroy()
  await delay(100)
  t.ok(sentFor(h.up, 5, stream.id), 'close is sent at once for the destroyed stream')
  t.is(appCounter.used, 0, 'the slot is given back at once')
  t.ok(stream.destroyed, 'the stream is destroyed')
  t.ok(await within(writeErr, 'the blocked write callback'), 'the blocked write fails with an error')
  t.is(sumData(h.up), 2 * MIB, 'nothing more is sent after the destroy')
  t.alike(failuresOf(h), [], 'no session errors')
}))

// open() removes its abort listener once the open settles (HoleBridge-eon.10): a signal that outlives the open
// keeps no listener, after opened and after reject. An abort after opened cancels nothing.
test('open() removes its abort listener once the open settles', catchThrows(async (t) => {
  const h = link({ accept: (service) => (service === 'nope' ? { code: 1, reason: 'unknown service' } : {}) })
  const sig = fakeSignal()
  const opening = h.app.open('web', { signal: sig.signal })
  t.is(sig.listeners.length, 1, 'the listener waits while the open is pending')
  const stream = await within(opening, 'open() of web')
  t.is(sig.listeners.length, 0, 'no listener is left after opened')
  await within(h.app.open('nope', { signal: sig.signal }).then(() => null, () => null), 'open() of nope')
  t.is(sig.listeners.length, 0, 'no listener is left after reject')
  sig.abort()
  t.absent(sentFor(h.up, 5, stream.id), 'an abort after opened sends no close')
}))
