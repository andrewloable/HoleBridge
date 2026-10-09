// Fake host for the engine tests: a stand-in for the Go host, built from the engine's own protocol and
// mux in host role (lib/protocol.js, lib/mux.js), on a hyperdht testnet. Test helper only: it is not in
// the bundle, which index.js and lib/ make up.

const DHT = require('hyperdht')
const Protomux = require('protomux')
const TCP = require('bare-tcp')
const b4a = require('b4a')
const keys = require('../../lib/keys.js')
const protocol = require('../../lib/protocol.js')
const { Session, Budget } = require('../../lib/mux.js')

const PROTOCOL = 'holebridge'
const MIB = 1024 * 1024
// HANG_MS is how long a 'hang' service holds its open before it is rejected with code 4. The mux answers
// an open at once, so the host holds the open message itself.
const HANG_MS = 300
const TARGETS = ['refuse', 'hang']

function noop() {}

function delay(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms))
}

// describe returns the handshake entry of a service. The port is the hint for the app's listener: the
// target's port, or 0 when the target has none.
function handshakeEntry(service) {
  const kind = protocol.KIND[service.kind]
  if (kind === undefined) throw new Error(`service ${service.name}: unknown kind ${service.kind}`)
  if (typeof service.target !== 'object' && !TARGETS.includes(service.target)) {
    throw new Error(`service ${service.name}: target must be { port }, 'refuse' or 'hang'`)
  }
  const port = typeof service.target === 'object' ? service.target.port : 0
  return { name: service.name, kind, port, origins: [] }
}

/**
 * createFakeHost(options) -> Promise<{ publicKey, client, close }>
 *
 * Starts a host on the testnet and resolves once it listens. Options:
 * - testnet: the testnet from hyperdht/testnet; its bootstrap is used. The caller destroys the testnet.
 * - key: the typed test key, e.g. '7KQ-M4X-9TR'. It is normalized and derived with lib/keys.js.
 * - appKey: the 32-byte application key, as a Buffer.
 * - services: [{ name, kind, target }]. kind is unknown, https, http, tcp or udp. target is { port }
 *   (a TCP service at 127.0.0.1:port), 'refuse' (the open is rejected with code 3) or 'hang' (rejected
 *   with code 4 after HANG_MS). Default [].
 * - flags: handshake flag bits, default 0. The lan bit is set by the lan option, not here.
 * - lan: false (default), or { addresses, port } for the handshake's lan block.
 * - version: protocol version in the handshake, default 1. Another value tests a version mismatch.
 *
 * The result has publicKey (the host public key to dial), client (the client key pair the firewall
 * admits, for the test to dial with) and close(), which stops the server, its sockets and its DHT node.
 * Nothing here logs keys.
 */
async function createFakeHost({ testnet, key, appKey, services = [], flags = 0, lan = false, version = 1 }) {
  const pairs = await keys.derive(keys.normalize(key), appKey)
  const byName = new Map(services.map((s) => [s.name, s]))
  const hello = {
    version,
    flags: lan ? flags | protocol.FLAG.lan : flags & ~protocol.FLAG.lan,
    services: services.map(handshakeEntry),
    ...(lan ? { lan } : {})
  }
  const budget = new Budget(256 * MIB)
  const conns = new Set() // accepted app connections
  const targets = new Set() // TCP sockets to service targets

  const dht = new DHT({ bootstrap: testnet.bootstrap })
  const server = dht.createServer({
    // The firewall drops (returns true for) every remote key except the client key pair.
    firewall: (remotePublicKey) => !b4a.equals(remotePublicKey, pairs.client.publicKey)
  })

  // bridge joins an accepted stream to the target on 127.0.0.1. The stream is read by hand, not with
  // st.pipe: a pipe stalls once the target applies backpressure, because streamx does not read a
  // highWaterMark 0 stream again after the pipe drains. Bytes wait in the stream until the connect is
  // done. A failed connect ends the stream.
  const bridge = (st, port) => {
    const sock = TCP.createConnection(port, '127.0.0.1', () => {
      st.on('data', (chunk) => {
        if (!sock.write(chunk)) {
          st.pause()
          sock.once('drain', () => st.resume())
        }
      })
      st.on('end', () => sock.end()) // the app closed its write side: half-close the target
    })
    targets.add(sock)
    sock.once('close', () => targets.delete(sock))
    sock.on('error', () => st.destroy())
    st.on('error', noop)
    st.on('close', () => sock.destroy())
    sock.pipe(st)
  }

  const accept = (name) => {
    const s = byName.get(name)
    if (!s) return { code: protocol.REJECT.unknownService, reason: 'unknown service' }
    if (s.target === 'refuse') return { code: protocol.REJECT.targetRefused, reason: 'target refused' }
    if (s.target === 'hang') return { code: protocol.REJECT.targetTimedOut, reason: 'target timed out' }
    return { target: (st) => bridge(st, s.target.port) }
  }

  // serve runs one accepted connection: the holebridge channel with the host's handshake, and a host
  // mux session that takes the channel's messages in arrival order.
  const serve = (socket) => {
    conns.add(socket)
    socket.on('error', noop)
    const mux = Protomux.from(socket)
    let session = null
    let queue = Promise.resolve()
    socket.once('close', () => {
      conns.delete(socket)
      if (session) session.destroy()
    })

    const deliver = async (index, message) => {
      if (index === 0 && byName.get(message.service)?.target === 'hang') await delay(HANG_MS)
      if (!session.closed) session.receive(index, message)
    }

    mux.pair({ protocol: PROTOCOL }, () => {
      const channel = mux.createChannel({
        protocol: PROTOCOL,
        handshake: protocol.handshake,
        messages: protocol.messages.map((encoding, index) => ({
          encoding,
          // A message that fails ends the connection, as the Go host does.
          onmessage: (message) => {
            queue = queue.then(() => deliver(index, message)).catch(() => socket.destroy())
          }
        }))
      })
      if (!channel) return
      session = new Session({
        role: 'host',
        send: (index, message) => channel.messages[index].send(message),
        accept,
        budget
      })
      channel.open(hello)
    })
  }

  const close = async () => {
    for (const socket of conns) socket.destroy()
    for (const sock of targets) sock.destroy()
    await server.close()
    await dht.destroy()
  }

  try {
    await dht.fullyBootstrapped()
    server.on('connection', serve)
    await server.listen(pairs.host)
  } catch (err) {
    await close()
    throw err
  }
  return { publicKey: pairs.host.publicKey, client: pairs.client, close }
}

/**
 * createEchoServer() -> Promise<{ port, close }>
 *
 * A TCP server on 127.0.0.1 that writes back every byte it reads. port is its port; close() destroys
 * its open sockets and stops it.
 */
async function createEchoServer() {
  const sockets = new Set()
  const server = TCP.createServer((socket) => {
    sockets.add(socket)
    socket.on('error', noop)
    socket.once('close', () => sockets.delete(socket))
    socket.on('data', (chunk) => socket.write(chunk))
  })
  await new Promise((resolve, reject) => {
    server.once('error', reject)
    server.listen(0, '127.0.0.1', 511, {}, resolve)
  })
  return {
    port: server.address().port,
    close: () =>
      new Promise((resolve) => {
        for (const socket of sockets) socket.destroy()
        server.close(resolve)
      })
  }
}

module.exports = { createFakeHost, createEchoServer }
