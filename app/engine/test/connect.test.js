// Tests for connect over the direct route (lib/connect.js). Each test starts a testnet and, where it needs
// one, a fake host on it (test/helpers/fake-host.js). The client is connectDirect with the typed test key
// and the test application key from spec/vectors. Until the IMPL task lands, connectDirect throws 'not
// implemented', so every test fails for that reason.
const test = require('brittle')
const b4a = require('b4a')
const DHT = require('hyperdht')
const createTestnet = require('hyperdht/testnet')
const { load, hex } = require('./helpers/vectors.js')
const { createFakeHost, createEchoServer } = require('./helpers/fake-host.js')
const { connectDirect } = require('../lib/connect.js')
const protocol = require('../lib/protocol.js')

const MIB = 1024 * 1024
// SETTLE bounds each wait, so a step that never finishes fails its test with a message.
const SETTLE = 10000
// LOOKUP_BUDGET is the short lookup budget the wrong-key test passes in. WRONG_KEY_LIMIT is how long that
// test waits: the budget plus slack, far below the 60 s default.
const LOOKUP_BUDGET = 3000
const WRONG_KEY_LIMIT = 15000

// The application key and the typed test key of the first derivation vector, as fake-host.test.js uses them.
const vectors = load('key-derivation.json')
const appKey = hex(vectors.keys[0].appKey)
const TEST_KEY = '7KQ-M4X-9TR'
// A different 9-symbol key from the same vectors. No host stands behind it, so its lookup finds nothing.
const WRONG_KEY = b4a.toString(hex(vectors.keys[2].normalized), 'utf8')

// within rejects if promise has not settled within ms, so a step that never finishes fails its test.
function within(promise, what, ms = SETTLE) {
  let timer
  const timeout = new Promise((resolve, reject) => {
    timer = setTimeout(() => reject(new Error(`${what} did not finish within ${ms} ms`)), ms)
  })
  return Promise.race([promise, timeout]).finally(() => clearTimeout(timer))
}

// settle resolves to what fn throws or rejects with, or null when fn resolves.
async function settle(fn) {
  try {
    await fn()
  } catch (err) {
    return err
  }
  return null
}

// hbRejection resolves to the error fn rejects with, which must carry an HB code. Any other error, such as
// the stub's 'not implemented', is thrown, so the test fails with that message and not with a code check.
async function hbRejection(fn) {
  const err = await settle(fn)
  if (err && !err.code) throw err
  return err
}

// catchThrows runs a test body so that a throw fails only that test. brittle ends the whole run when a body
// throws, and the stub throws 'not implemented' until the IMPL task lands.
function catchThrows(fn) {
  return async (t) => {
    try {
      await fn(t)
    } catch (err) {
      t.fail(err.message)
    }
  }
}

// connect dials the test host with connectDirect and resolves with the HostSession.
async function connect(dht, options = {}) {
  return within(connectDirect({ dht, key: TEST_KEY, appKey, ...options }), 'connectDirect')
}

// withTestnet runs fn with a testnet and a client DHT on it, and destroys both, also when fn throws.
async function withTestnet(fn) {
  const testnet = await createTestnet(5)
  const dht = new DHT({ bootstrap: testnet.bootstrap })
  try {
    await dht.fullyBootstrapped()
    return await fn({ testnet, dht })
  } finally {
    await dht.destroy()
    await testnet.destroy()
  }
}

// withHost runs fn with a fake host on a testnet, serving services, and stops the host afterwards.
async function withHost({ services = [], version = 1, lan = false }, fn) {
  return withTestnet(async ({ testnet, dht }) => {
    const fake = await createFakeHost({ testnet, key: TEST_KEY, appKey, services, version, lan })
    try {
      return await fn({ dht, fake })
    } finally {
      await fake.close()
    }
  })
}

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

test('connectDirect resolves with the host services, lan block and flags', catchThrows(async (t) => {
  await withHost(
    {
      services: [
        { name: 'web', kind: 'http', target: { port: 8080 } },
        { name: 'ssh', kind: 'tcp', target: 'refuse' }
      ],
      lan: { addresses: ['192.0.2.10'], port: 8443 }
    },
    async ({ dht }) => {
      const session = await connect(dht)
      try {
        t.alike(
          session.services,
          [
            { name: 'web', kind: protocol.KIND.http, port: 8080, origins: [] },
            { name: 'ssh', kind: protocol.KIND.tcp, port: 0, origins: [] }
          ],
          'services is the host handshake list'
        )
        t.alike(session.lan, { addresses: ['192.0.2.10'], port: 8443 }, 'lan is the host lan block')
        t.is(session.flags, protocol.FLAG.lan, 'flags are the host handshake flags')
      } finally {
        session.destroy()
      }
    }
  )
}))

test('open web to an echo target echoes 1 MB', catchThrows(async (t) => {
  const echo = await createEchoServer()
  try {
    await withHost({ services: [{ name: 'web', kind: 'http', target: { port: echo.port } }] }, async ({ dht }) => {
      const session = await connect(dht)
      try {
        const payload = payloadOf(MIB)
        const stream = await within(session.open('web'), 'the open of web')
        stream.write(payload)
        const echoed = await within(readBytes(stream, payload.length), 'the 1 MB echo')
        t.ok(b4a.equals(echoed, payload), 'every byte comes back in order')
      } finally {
        session.destroy()
      }
    })
  } finally {
    await echo.close()
  }
}))

test('a wrong key rejects with HB-LOOKUP-TIMEOUT within the short lookup budget', catchThrows(async (t) => {
  await withHost({}, async ({ dht }) => {
    const err = await within(
      hbRejection(() => connectDirect({ dht, key: WRONG_KEY, appKey, lookupTimeout: LOOKUP_BUDGET })),
      'the wrong-key connect',
      WRONG_KEY_LIMIT
    )
    t.is(err && err.code, 'HB-LOOKUP-TIMEOUT', 'the code is HB-LOOKUP-TIMEOUT')
  })
}))

test('a host on protocol version 2 rejects with HB-VERSION-MISMATCH that says to update the app', catchThrows(async (t) => {
  await withHost({ version: 2 }, async ({ dht }) => {
    const err = await within(
      hbRejection(() => connectDirect({ dht, key: TEST_KEY, appKey })),
      'the connect to a version 2 host'
    )
    t.is(err && err.code, 'HB-VERSION-MISMATCH', 'the code is HB-VERSION-MISMATCH')
    const detail = err ? err.reason || err.message : ''
    t.ok(/update the app/i.test(detail), 'the detail says to update the app')
  })
}))

test('the route is direct on the testnet', catchThrows(async (t) => {
  await withHost({ services: [{ name: 'web', kind: 'http', target: { port: 8080 } }] }, async ({ dht }) => {
    const session = await connect(dht)
    try {
      t.is(session.route, 'direct', 'the route is direct')
    } finally {
      session.destroy()
    }
  })
}))

test('destroy closes the session and errors the open streams', catchThrows(async (t) => {
  const echo = await createEchoServer()
  try {
    await withHost({ services: [{ name: 'web', kind: 'http', target: { port: echo.port } }] }, async ({ dht }) => {
      const session = await connect(dht)
      const stream = await within(session.open('web'), 'the open of web')
      const streamError = new Promise((resolve) => stream.once('error', resolve))
      const closed = new Promise((resolve) => session.on('close', resolve))
      session.destroy()
      const err = await within(streamError, 'the open stream to error')
      await within(closed, 'the session close event')
      t.ok(err instanceof Error, 'the open stream errors')
      const openErr = await within(settle(() => session.open('web')), 'open after destroy')
      t.ok(openErr instanceof Error, 'open after destroy rejects')
    })
  } finally {
    await echo.close()
  }
}))

test('a session outlives the lookup budget once the handshake is done', catchThrows(async (t) => {
  const echo = await createEchoServer()
  try {
    await withHost({ services: [{ name: 'web', kind: 'http', target: { port: echo.port } }] }, async ({ dht }) => {
      const session = await within(
        connectDirect({ dht, key: TEST_KEY, appKey, lookupTimeout: 500 }),
        'connectDirect with a short budget'
      )
      let closed = false
      session.on('close', () => { closed = true })
      try {
        // Well past the 500 ms budget: the budget applies to the search and the handshake only.
        await new Promise((resolve) => setTimeout(resolve, 1000))
        t.is(closed, false, 'the session is still open after the budget')
        const payload = payloadOf(1024)
        const stream = await within(session.open('web'), 'the open of web')
        stream.write(payload)
        const echoed = await within(readBytes(stream, payload.length), 'the echo')
        t.ok(b4a.equals(echoed, payload), 'the session still carries bytes')
      } finally {
        session.destroy()
      }
    })
  } finally {
    await echo.close()
  }
}))
