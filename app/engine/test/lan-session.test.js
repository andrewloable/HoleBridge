// Tests for the LAN session: Noise over TCP to the host (lib/lan-session.js). Rules: docs/architecture.md
// (LAN route: Session over the LAN). The host is the fake host's LAN listener on 127.0.0.1
// (test/helpers/fake-host.js, lan: true). Until the IMPL task (HoleBridge-hb5.2.4) lands, connectLan throws
// 'not implemented', so each test fails for that reason.
//
// Shapes the IMPL must meet (also stated in lib/lan-session.js):
// - connectLan({ address, port, key, appKey }) dials TCP to address:port and runs the Noise handshake with
//   @hyperswarm/secret-stream as initiator, with the IK pattern, the client key pair and the host public key
//   derived from key. IK checks the host's static key inside the handshake, so a host with another key is
//   rejected before any channel opens.
// - A connect that fails before the handshake completes rejects with HB-LOOKUP-TIMEOUT, as connectDirect
//   does. Its reason names the TCP error code, ECONNREFUSED for a closed port.
const test = require('brittle')
const b4a = require('b4a')
const createTestnet = require('hyperdht/testnet')
const NoiseSecretStream = require('@hyperswarm/secret-stream')
const TCP = require('bare-tcp')
const { load, hex } = require('./helpers/vectors.js')
const { createFakeHost, createEchoServer } = require('./helpers/fake-host.js')
const { connectLan } = require('../lib/lan-session.js')
const protocol = require('../lib/protocol.js')

const MIB = 1024 * 1024
const LOOPBACK = '127.0.0.1'
// SETTLE bounds each wait, so a step that never finishes fails its test with a message. PROMPT is the bound
// for a refused connect, which the kernel answers at once.
const SETTLE = 10000
const PROMPT = 5000

// The application key and the typed test key of the first derivation vector, as connect.test.js uses them.
const vectors = load('key-derivation.json')
const appKey = hex(vectors.keys[0].appKey)
const TEST_KEY = '7KQ-M4X-9TR'

function noop() {}

// within rejects if promise has not settled within ms, so a step that never finishes fails its test.
function within(promise, what, ms = SETTLE) {
  let timer
  const timeout = new Promise((resolve, reject) => {
    timer = setTimeout(() => reject(new Error(`${what} did not finish within ${ms} ms`)), ms)
  })
  return Promise.race([promise, timeout]).finally(() => clearTimeout(timer))
}

// hbRejection resolves to the error fn rejects with, which must carry an HB code, or null when fn resolves.
// Any other error, such as the stub's 'not implemented', is thrown, so the test fails with that message.
async function hbRejection(fn) {
  let err = null
  try {
    await fn()
  } catch (caught) {
    err = caught
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

// listenTcp runs a TCP server on 127.0.0.1 that hands each accepted socket to onSocket. close() destroys the
// open sockets and stops the server.
async function listenTcp(onSocket) {
  const sockets = new Set()
  const server = TCP.createServer((socket) => {
    sockets.add(socket)
    socket.on('error', noop)
    socket.once('close', () => sockets.delete(socket))
    onSocket(socket)
  })
  await new Promise((resolve, reject) => {
    server.once('error', reject)
    server.listen(0, LOOPBACK, 511, {}, resolve)
  })
  return {
    port: server.address().port,
    close: () =>
      new Promise((resolve) => {
        for (const socket of sockets) socket.destroy()
        server.close(() => resolve())
      })
  }
}

// hostPresenting runs a LAN listener that answers the Noise handshake under keyPair and nothing more: a host
// that presents another static key than the one the client expects. It uses the default XX pattern, so the
// client's check of the host key is what must reject it (an XX client without that check would reach the
// channel, and this host never answers it). It never sends a channel handshake.
function hostPresenting(keyPair) {
  return listenTcp((socket) => {
    const stream = new NoiseSecretStream(false, socket, { keyPair })
    stream.on('error', noop)
  })
}

// closedPort resolves with a loopback port that nothing listens on: a port a server had, then closed.
async function closedPort() {
  const listener = await listenTcp(noop)
  await listener.close()
  return listener.port
}

// withLanHost runs fn with a fake host whose LAN listener is on, and stops the host afterwards.
async function withLanHost({ services = [] }, fn) {
  const testnet = await createTestnet(5)
  try {
    const fake = await createFakeHost({ testnet, key: TEST_KEY, appKey, services, lan: true })
    try {
      return await fn({ fake })
    } finally {
      await fake.close()
    }
  } finally {
    await testnet.destroy()
  }
}

// lanConnect dials the fake host's LAN listener at port with connectLan and resolves with the HostSession.
function lanConnect(port) {
  return within((async () => connectLan({ address: LOOPBACK, port, key: TEST_KEY, appKey }))(), 'connectLan')
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

test('connectLan resolves with the services and route lan against the LAN listener', catchThrows(async (t) => {
  await withLanHost({ services: [{ name: 'web', kind: 'http', target: { port: 8080 } }] }, async ({ fake }) => {
    const session = await lanConnect(fake.lanPort)
    try {
      t.alike(
        session.services,
        [{ name: 'web', kind: protocol.KIND.http, port: 8080, origins: [] }],
        'services is the host handshake list'
      )
      t.is(session.route, 'lan', 'the route is lan')
    } finally {
      session.destroy()
    }
  })
}))

test('a host presenting another static key is rejected', catchThrows(async (t) => {
  const host = await hostPresenting(NoiseSecretStream.keyPair())
  let session = null
  try {
    const err = await within(
      hbRejection(async () => {
        session = await connectLan({ address: LOOPBACK, port: host.port, key: TEST_KEY, appKey })
      }),
      'the connect to a host with another static key'
    )
    t.is(err && err.code, 'HB-LOOKUP-TIMEOUT', 'the code is HB-LOOKUP-TIMEOUT')
    t.is(session, null, 'no session is returned')
  } finally {
    if (session) session.destroy()
    await host.close()
  }
}))

test('open over the LAN session echoes 1 MB', catchThrows(async (t) => {
  const echo = await createEchoServer()
  try {
    await withLanHost({ services: [{ name: 'web', kind: 'http', target: { port: echo.port } }] }, async ({ fake }) => {
      const session = await lanConnect(fake.lanPort)
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

test('a closed port rejects promptly with HB-LOOKUP-TIMEOUT naming the refused connection', catchThrows(async (t) => {
  const port = await closedPort()
  let session = null
  const err = await within(
    hbRejection(async () => {
      session = await connectLan({ address: LOOPBACK, port, key: TEST_KEY, appKey })
    }),
    'the connect to a closed port',
    PROMPT
  )
  if (session) session.destroy()
  t.is(err && err.code, 'HB-LOOKUP-TIMEOUT', 'the code is HB-LOOKUP-TIMEOUT')
  const reason = err ? err.reason || err.message : ''
  t.ok(/ECONNREFUSED/.test(reason), 'the reason names the refused connection')
}))
