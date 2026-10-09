// Tests for the relay key in the engine (lib/relay.js): the member key pair as the DHT default key pair,
// the relayThrough policy, and a relayed connect through a blind-relay server on a testnet. Until the IMPL
// task lands, createDht and relayPolicy throw 'not implemented', so every test fails for that reason.
const test = require('brittle')
const b4a = require('b4a')
const DHT = require('hyperdht')
const createTestnet = require('hyperdht/testnet')
const BlindRelay = require('blind-relay')
const UDX = require('udx-native')
const { load, hex } = require('./helpers/vectors.js')
const { createFakeHost, createEchoServer } = require('./helpers/fake-host.js')
const keys = require('../lib/keys.js')
const { connectDirect } = require('../lib/connect.js')
const { createDht, relayPolicy } = require('../lib/relay.js')

// SETTLE bounds each wait; CONNECT_SETTLE bounds a connect, which derives keys (Argon2id) before it dials.
const SETTLE = 10000
const CONNECT_SETTLE = 30000

// The first relay vector: its application key, its member and server public keys. Its normalized key is
// 7KQM4X9TR, so RELAY_KEY, typed with dashes, must derive the same keys (normalization, docs/security.md).
const vectors = load('key-derivation.json')
const appKey = hex(vectors.keys[0].appKey)
const RELAY = vectors.relayKeys[0]
const RELAY_KEY = '7KQ-M4X-9TR'
const memberKey = hex(RELAY.publicKeys.member)
const serverKey = hex(RELAY.publicKeys.server)
// The host key the fake host listens under, as connect.test.js uses it.
const TEST_KEY = '7KQ-M4X-9TR'

// within rejects if promise has not settled within ms, so a step that never finishes fails its test.
function within(promise, what, ms = SETTLE) {
  let timer
  const timeout = new Promise((resolve, reject) => {
    timer = setTimeout(() => reject(new Error(`${what} did not finish within ${ms} ms`)), ms)
  })
  return Promise.race([promise, timeout]).finally(() => clearTimeout(timer))
}

// until polls check every 100 ms and rejects if it is still false after ms.
async function until(check, what, ms = SETTLE) {
  const deadline = Date.now() + ms
  while (!check()) {
    if (Date.now() > deadline) throw new Error(`${what} did not happen within ${ms} ms`)
    await new Promise((resolve) => setTimeout(resolve, 100))
  }
}

// hbRejection resolves to the error fn rejects with, which must carry an HB code. Any other error, such as
// the stub's 'not implemented', is thrown, so the test fails with that message and not with a code check.
async function hbRejection(fn) {
  let err = null
  try {
    await fn()
  } catch (e) {
    err = e
  }
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

// sameKey is true when got is the 32-byte key want (a policy returns null when it offers no relay).
function sameKey(got, want) {
  return got !== null && got !== undefined && b4a.equals(got, want)
}

// withTestnet runs fn with a testnet and destroys it afterwards, also when fn throws.
async function withTestnet(fn) {
  const testnet = await createTestnet(5)
  try {
    return await fn({ testnet })
  } finally {
    await testnet.destroy()
  }
}

// startRelay runs a blind-relay server on the testnet under the relay server key pair. Its firewall admits
// only the member key, as a relay does (docs/security.md, Relay keys). Its stats count the sessions it accepted.
async function startRelay(testnet, pairs) {
  const udx = new UDX()
  const server = new BlindRelay.Server({
    createStream: (opts) => udx.createStream(1 + Math.floor(Math.random() * 0x7ffffffe), opts)
  })
  const dht = new DHT({ bootstrap: testnet.bootstrap })
  const node = dht.createServer(
    { firewall: (remotePublicKey) => !b4a.equals(remotePublicKey, pairs.member.publicKey) },
    (socket) => server.accept(socket)
  )
  try {
    await dht.fullyBootstrapped()
    await node.listen(pairs.server)
  } catch (err) {
    await node.close()
    await dht.destroy()
    throw err
  }
  return {
    stats: server.stats,
    close: async () => {
      await server.close()
      await node.close()
      await dht.destroy()
    }
  }
}

// recordConnects wraps dht.connect so that each call's options are kept. The call still goes through.
function recordConnects(dht) {
  const calls = []
  const connect = dht.connect.bind(dht)
  dht.connect = (publicKey, opts) => {
    calls.push(opts || {})
    return connect(publicKey, opts)
  }
  return calls
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

test('with a relay key, the DHT default key pair is the member key pair', catchThrows(async (t) => {
  await withTestnet(async ({ testnet }) => {
    const dht = await createDht({ bootstrap: testnet.bootstrap, relayKey: RELAY_KEY, appKey })
    try {
      t.ok(
        b4a.equals(dht.defaultKeyPair.publicKey, memberKey),
        'defaultKeyPair.publicKey is the member public key of the relay vector'
      )
    } finally {
      await dht.destroy()
    }
  })
}))

test('relayPolicy offers the relay key when forced or when the DHT is randomized, and null otherwise', catchThrows(async (t) => {
  const dht = { randomized: false }
  const policy = relayPolicy({ relayServerPublicKey: serverKey, dht })
  t.is(typeof policy, 'function', 'relayPolicy returns a function')
  t.ok(sameKey(policy(true), serverKey), 'forced: the relay key')
  t.is(policy(false), null, 'not forced and not randomized: null')
  t.is(policy(), null, 'no argument counts as not forced: null')
  // The policy reads dht.randomized at each call, so a connect made later sees the NAT as it is then.
  dht.randomized = true
  t.ok(sameKey(policy(false), serverKey), 'randomized NAT: the relay key')
  t.ok(sameKey(policy(), serverKey), 'randomized NAT with no argument: the relay key')
}))

test('a relayed connect dials the relay under the member key and reaches the fake host', catchThrows(async (t) => {
  const echo = await createEchoServer()
  try {
    await withTestnet(async ({ testnet }) => {
      const pairs = await keys.deriveRelay(RELAY_KEY, appKey)
      const dht = await createDht({ bootstrap: testnet.bootstrap, relayKey: RELAY_KEY, appKey })
      // connectDirect calls the policy with no force, so the DHT reports a randomized NAT: the policy offers
      // the relay. The client DHT is created before the servers, so its cleanup runs last.
      Object.defineProperty(dht, 'randomized', { value: true })
      const relay = await startRelay(testnet, pairs)
      const fake = await createFakeHost({
        testnet,
        key: TEST_KEY,
        appKey,
        services: [{ name: 'web', kind: 'http', target: { port: echo.port } }]
      })
      let session = null
      try {
        session = await within(
          connectDirect({ dht, key: TEST_KEY, appKey, relayKey: RELAY_KEY }),
          'connectDirect with a relay key',
          CONNECT_SETTLE
        )
        await until(() => relay.stats.sessions.accepted >= 1, 'the relay to accept the member key dial')
        const payload = payloadOf(1024)
        const stream = await within(session.open('web'), 'the open of web')
        stream.write(payload)
        const echoed = await within(readBytes(stream, payload.length), 'the echo')
        t.ok(b4a.equals(echoed, payload), 'the data reaches the fake host')
        // On the testnet the direct path wins and the relay pairs nothing (measured with hyperdht 6.34.1), so
        // the route is direct. A relayed route needs a seam that drops the direct path: see the follow-up.
        t.is(session.route, 'direct', 'the route is direct on the testnet')
      } finally {
        if (session) session.destroy()
        await fake.close()
        await relay.close()
        await dht.destroy()
      }
    })
  } finally {
    await echo.close()
  }
}))

test('without a relay key, the default key pair is not the member key and dht.connect gets no relayThrough', catchThrows(async (t) => {
  await withTestnet(async ({ testnet }) => {
    const dht = await createDht({ bootstrap: testnet.bootstrap, appKey })
    const fake = await createFakeHost({ testnet, key: TEST_KEY, appKey })
    const calls = recordConnects(dht)
    let session = null
    try {
      t.ok(!b4a.equals(dht.defaultKeyPair.publicKey, memberKey), 'the default key pair is not the member key pair')
      session = await within(
        connectDirect({ dht, key: TEST_KEY, appKey }),
        'connectDirect without a relay key',
        CONNECT_SETTLE
      )
      t.is(calls.length, 1, 'connectDirect dials once')
      t.absent(calls[0].relayThrough, 'dht.connect gets no relayThrough')
    } finally {
      if (session) session.destroy()
      await fake.close()
      await dht.destroy()
    }
  })
}))

test('createDht rejects a relay key that is not 9 symbols with HB-KEY-INVALID', catchThrows(async (t) => {
  await withTestnet(async ({ testnet }) => {
    const err = await hbRejection(() => createDht({ bootstrap: testnet.bootstrap, relayKey: '7KQ-M4X', appKey }))
    t.is(err && err.code, 'HB-KEY-INVALID', 'the code is HB-KEY-INVALID')
  })
}))
