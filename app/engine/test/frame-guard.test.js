// Tests for the frame guard (lib/frame-guard.js). A control batch inside a control batch is refused before
// protomux decodes the frame, so the stream closes and the engine keeps running. Upstream protomux 3.12.1
// recurses on such a frame without a limit and overflows the JS stack (a RangeError, which safety-catch
// rethrows, so the process dies). The Go side refuses the same frame (pears/protomux, errFrame). The guard
// sits in openSession (lib/connect.js), which both routes use, so the direct and the LAN route are tested.
const test = require('brittle')
const b4a = require('b4a')
const c = require('compact-encoding')
const DHT = require('hyperdht')
const createTestnet = require('hyperdht/testnet')
const { load, hex } = require('./helpers/vectors.js')
const { createFakeHost, createEchoServer } = require('./helpers/fake-host.js')
const { connectDirect } = require('../lib/connect.js')
const { connectLan } = require('../lib/lan-session.js')
const { nestedControlBatch } = require('../lib/frame-guard.js')

// SETTLE bounds each wait, so a step that never finishes fails its test with a message.
const SETTLE = 10000
// NESTED_DEPTH is how many batches sit inside the first one. 100k levels make a frame of about 700 KB,
// which overflows the JS stack when protomux decodes it without the guard.
const NESTED_DEPTH = 100000

// The application key and the typed test key of the first derivation vector, as connect.test.js uses them.
const vectors = load('key-derivation.json')
const appKey = hex(vectors.keys[0].appKey)
const TEST_KEY = '7KQ-M4X-9TR'

// SINGLE_BATCH is one level: a control batch with two entries, each closing a remote channel that is not
// open (remote ids 9 and 10). Both sides ignore such an entry, and it is the shape upstream sends.
const SINGLE_BATCH = b4a.from([0, 0, 0, 2, 3, 9, 2, 3, 10])

// catchThrows runs a test body so that a throw fails only that test. brittle ends the whole run when a body
// throws.
function catchThrows(fn) {
  return async (t) => {
    try {
      await fn(t)
    } catch (err) {
      t.fail(err.message)
    }
  }
}

// within rejects if promise has not settled within SETTLE, so a step that never finishes fails its test.
function within(promise, what) {
  let timer
  const timeout = new Promise((resolve, reject) => {
    timer = setTimeout(() => reject(new Error(`${what} did not finish within ${SETTLE} ms`)), SETTLE)
  })
  return Promise.race([promise, timeout]).finally(() => clearTimeout(timer))
}

const delay = (ms) => new Promise((resolve) => setTimeout(resolve, ms))

// readBytes resolves with the first n bytes that stream delivers.
function readBytes(stream, n) {
  return new Promise((resolve, reject) => {
    const parts = []
    let total = 0
    stream.on('error', reject)
    stream.on('data', (chunk) => {
      if (total >= n) return
      parts.push(chunk)
      total += chunk.length
      if (total >= n) resolve(b4a.concat(parts))
    })
  })
}

// payloadOf returns n bytes in a pattern that repeats every 251 bytes, so a shifted or lost byte shows.
function payloadOf(n) {
  const out = b4a.alloc(n)
  for (let i = 0; i < n; i++) out[i] = i % 251
  return out
}

// uintSize is the encoded length of n as a compact-encoding uint.
function uintSize(n) {
  const state = { start: 0, end: 0, buffer: null }
  c.uint.preencode(state, n)
  return state.end
}

// nestedBatchFrame(depth) builds one frame: a control batch (remote id 0, type 0) whose entry is a control
// batch, nested depth levels deeper. Depth 0 is an empty batch. Any depth from 1 holds a batch inside a
// batch, which the guard refuses. The sizes are summed first, so the frame is built in linear time.
function nestedBatchFrame(depth) {
  const entries = new Array(depth)
  let body = 1 // the innermost batch: its remote id only
  for (let level = depth - 1; level >= 0; level--) {
    entries[level] = 1 + body // an entry: its type (0, a batch), then the batch inside it
    body = 1 + uintSize(entries[level]) + entries[level] // a batch: its remote id, then its entry
  }
  const frame = b4a.alloc(2 + body)
  const state = { buffer: frame, start: 2, end: frame.length }
  frame[0] = 0 // remote id 0: the control session
  frame[1] = 0 // type 0: a batch
  for (let level = 0; level < depth; level++) {
    frame[state.start++] = 0 // the batch's remote id
    c.uint.encode(state, entries[level]) // the length of its entry
    frame[state.start++] = 0 // the entry's type: a batch
  }
  frame[state.start++] = 0 // the innermost batch's remote id
  return frame
}

// withHost runs fn with a fake host on a testnet, serving web as an echo target, and a client DHT on the
// same testnet. lan also starts the host's LAN listener. fn gets the DHT, the host and streams, the app
// streams the host admitted in order. Everything is stopped afterwards.
async function withHost({ lan = false }, fn) {
  const echo = await createEchoServer()
  const testnet = await createTestnet(5)
  const dht = new DHT({ bootstrap: testnet.bootstrap })
  const streams = []
  let fake = null
  try {
    await dht.fullyBootstrapped()
    fake = await createFakeHost({
      testnet,
      key: TEST_KEY,
      appKey,
      lan,
      services: [{ name: 'web', kind: 'http', target: { port: echo.port } }],
      onStream: (stream) => streams.push(stream)
    })
    return await fn({ dht, fake, streams })
  } finally {
    if (fake) await fake.close()
    await dht.destroy()
    await testnet.destroy()
    await echo.close()
  }
}

// echoes opens web on the session and reports whether 64 KB come back unchanged.
async function echoes(session) {
  const payload = payloadOf(64 * 1024)
  const stream = await within(session.open('web'), 'the open of web')
  stream.write(payload)
  const echoed = await within(readBytes(stream, payload.length), 'the echo')
  return b4a.equals(echoed, payload)
}

// expectNestedBatchCloses has the host write a nested batch frame to its stream, and checks that the
// session closes and the host's stream closes with it.
async function expectNestedBatchCloses(t, session, hostStream) {
  const sessionClosed = new Promise((resolve) => session.on('close', resolve))
  const hostClosed = new Promise((resolve) => hostStream.once('close', resolve))
  hostStream.write(nestedBatchFrame(NESTED_DEPTH))
  await within(sessionClosed, 'the session close after a nested batch')
  await within(hostClosed, 'the host stream close after a nested batch')
  t.pass('the nested batch closed the session and the host stream')
}

test('direct route: a control batch inside a control batch closes the stream and the engine keeps running', catchThrows(async (t) => {
  await withHost({}, async ({ dht, streams }) => {
    const session = await within(connectDirect({ dht, key: TEST_KEY, appKey }), 'connectDirect')
    await expectNestedBatchCloses(t, session, streams[0])
    const second = await within(connectDirect({ dht, key: TEST_KEY, appKey }), 'the second connectDirect')
    try {
      t.ok(await echoes(second), 'a second session on the same host echoes 64 KB')
    } finally {
      second.destroy()
    }
  })
}))

test('LAN route: a control batch inside a control batch closes the stream and the engine keeps running', catchThrows(async (t) => {
  await withHost({ lan: true }, async ({ dht, fake, streams }) => {
    const address = '127.0.0.1'
    const session = await within(connectLan({ address, port: fake.lanPort, key: TEST_KEY, appKey }), 'connectLan')
    await expectNestedBatchCloses(t, session, streams[0])
    const second = await within(connectLan({ address, port: fake.lanPort, key: TEST_KEY, appKey }), 'the second connectLan')
    try {
      t.ok(await echoes(second), 'a second LAN session on the same host echoes 64 KB')
    } finally {
      second.destroy()
    }
  })
}))

test('direct route: a control batch with one level of entries still works', catchThrows(async (t) => {
  await withHost({}, async ({ dht, streams }) => {
    const session = await within(connectDirect({ dht, key: TEST_KEY, appKey }), 'connectDirect')
    try {
      let closed = false
      session.on('close', () => { closed = true })
      streams[0].write(SINGLE_BATCH)
      await delay(300)
      t.absent(closed, 'the session stays open after a one-level batch')
      t.ok(await echoes(session), 'the same session still echoes 64 KB')
    } finally {
      session.destroy()
    }
  })
}))

test('nestedControlBatch flags only a control batch inside a control batch', (t) => {
  t.is(nestedControlBatch(nestedBatchFrame(0)), false, 'an empty batch is not nested')
  t.is(nestedControlBatch(SINGLE_BATCH), false, 'one level of entries is not nested')
  t.is(nestedControlBatch(nestedBatchFrame(1)), true, 'a batch inside a batch is nested')
  t.is(nestedControlBatch(nestedBatchFrame(NESTED_DEPTH)), true, '100k levels are flagged without recursion')
  t.is(nestedControlBatch(b4a.from([1, 0, 5])), false, 'a message frame is not a control batch')
  t.is(nestedControlBatch(b4a.from([0])), false, 'a truncated frame is not nested and does not throw')
  t.is(nestedControlBatch(b4a.alloc(0)), false, 'an empty frame is not nested')
})
