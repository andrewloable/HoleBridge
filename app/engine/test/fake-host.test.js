// Tests for the fake host (test/helpers/fake-host.js). The fake host stands in for the Go host in the
// engine tests, so these tests show that it announces on a testnet under the derived host key, answers
// the handshake with its services and forwards streams. The client here is the app role: a Protomux
// channel and a lib/mux.js session in app role.
const test = require('brittle')
const b4a = require('b4a')
const DHT = require('hyperdht')
const createTestnet = require('hyperdht/testnet')
const Protomux = require('protomux')
const { load, hex } = require('./helpers/vectors.js')
const { createFakeHost, createEchoServer } = require('./helpers/fake-host.js')
const protocol = require('../lib/protocol.js')
const { Session, Budget } = require('../lib/mux.js')

const MIB = 1024 * 1024
// SETTLE bounds every wait, so a host that never answers fails its test instead of hanging it.
const SETTLE = 10000

// The test key and application key of the first derivation vector in spec/vectors.
const vector = load('key-derivation.json').keys[0]
const appKey = hex(vector.appKey)
const TEST_KEY = '7KQ-M4X-9TR'

function within(promise, what) {
  let timer
  const timeout = new Promise((resolve, reject) => {
    timer = setTimeout(() => reject(new Error(`${what} did not finish within ${SETTLE} ms`)), SETTLE)
  })
  return Promise.race([promise, timeout]).finally(() => clearTimeout(timer))
}

// dial connects the app role to the fake host with the client key pair. It resolves with the socket, the
// app session and the host's handshake.
async function dial(dht, fake) {
  const socket = dht.connect(fake.publicKey, { keyPair: fake.client })
  await within(
    new Promise((resolve, reject) => {
      socket.once('connect', resolve)
      socket.once('error', reject)
    }),
    'the connection'
  )

  let session = null
  let onHost = null
  const hostHandshake = new Promise((resolve) => {
    onHost = resolve
  })
  const mux = Protomux.from(socket)
  const channel = mux.createChannel({
    protocol: 'holebridge',
    handshake: protocol.handshake,
    messages: protocol.messages.map((encoding, index) => ({
      encoding,
      onmessage: (message) => session.receive(index, message)
    })),
    onopen: onHost
  })
  session = new Session({
    role: 'app',
    send: (index, message) => channel.messages[index].send(message),
    budget: new Budget(64 * MIB)
  })
  channel.open({ version: 1, flags: 0, services: [] })
  return { socket, session, hostHandshake: await within(hostHandshake, 'the host handshake') }
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

test('the fake host announces on the testnet and sends its services in the handshake', async (t) => {
  const testnet = await createTestnet(5)
  const dht = new DHT({ bootstrap: testnet.bootstrap })
  let fake = null
  let socket = null
  try {
    await dht.fullyBootstrapped()
    fake = await createFakeHost({
      testnet,
      key: TEST_KEY,
      appKey,
      services: [
        { name: 'web', kind: 'http', target: { port: 8080 } },
        { name: 'ssh', kind: 'tcp', target: 'refuse' }
      ],
      lan: { addresses: ['192.0.2.10'], port: 8443 }
    })
    t.ok(b4a.equals(fake.publicKey, hex(vector.publicKeys.host)), 'the host public key is the derived one')

    const dialed = await dial(dht, fake)
    socket = dialed.socket
    const hs = dialed.hostHandshake
    t.is(hs.version, 1, 'the host speaks protocol v1')
    t.ok((hs.flags & protocol.FLAG.lan) !== 0, 'the lan flag is set')
    t.alike(hs.lan, { addresses: ['192.0.2.10'], port: 8443 }, 'the lan block is sent')
    t.alike(
      hs.services,
      [
        { name: 'web', kind: protocol.KIND.http, port: 8080, origins: [] },
        { name: 'ssh', kind: protocol.KIND.tcp, port: 0, origins: [] }
      ],
      'the services are sent with their kinds and port hints'
    )
  } finally {
    if (socket) socket.destroy()
    await dht.destroy()
    if (fake) await fake.close()
    await testnet.destroy()
  }
})

test('the fake host forwards a stream to its target and rejects refuse, hang and unknown', async (t) => {
  const testnet = await createTestnet(5)
  const dht = new DHT({ bootstrap: testnet.bootstrap })
  let echo = null
  let fake = null
  let socket = null
  try {
    await dht.fullyBootstrapped()
    echo = await createEchoServer()
    fake = await createFakeHost({
      testnet,
      key: TEST_KEY,
      appKey,
      services: [
        { name: 'web', kind: 'http', target: { port: echo.port } },
        { name: 'refused', kind: 'tcp', target: 'refuse' },
        { name: 'slow', kind: 'tcp', target: 'hang' }
      ]
    })

    const dialed = await dial(dht, fake)
    socket = dialed.socket
    const { session } = dialed

    // 150 KiB is more than one data message (60 KiB), so the bytes cross several messages and credit grants.
    const payload = b4a.alloc(150 * 1024)
    for (let i = 0; i < payload.length; i++) payload[i] = i % 251
    const stream = await within(session.open('web'), 'the open of web')
    stream.write(payload)
    const echoed = await within(readBytes(stream, payload.length), 'the echo')
    t.ok(b4a.equals(echoed, payload), 'the echo target sends back every byte the stream sent, in order')

    const refused = await within(session.open('refused').catch((err) => err), 'the refused open')
    t.is(refused.code, protocol.REJECT.targetRefused, 'refuse rejects with code 3')

    const slow = await within(session.open('slow').catch((err) => err), 'the hung open')
    t.is(slow.code, protocol.REJECT.targetTimedOut, 'hang rejects with code 4')

    const unknown = await within(session.open('nope').catch((err) => err), 'the unknown open')
    t.is(unknown.code, protocol.REJECT.unknownService, 'an unknown name rejects with code 1')
  } finally {
    if (socket) socket.destroy()
    await dht.destroy()
    if (fake) await fake.close()
    if (echo) await echo.close()
    await testnet.destroy()
  }
})
