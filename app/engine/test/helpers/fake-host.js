// Fake host for the engine tests: a stand-in for the Go host, built from the engine's own protocol and
// mux in host role (lib/protocol.js, lib/mux.js), on a hyperdht testnet. Test helper only: it is not in
// the bundle, which index.js and lib/ make up.

const DHT = require('hyperdht')
const Protomux = require('protomux')
const TCP = require('bare-tcp')
const NoiseSecretStream = require('@hyperswarm/secret-stream')
const b4a = require('b4a')
const UDX = require('udx-native')
const keys = require('../../lib/keys.js')
const protocol = require('../../lib/protocol.js')
const { Session, Budget } = require('../../lib/mux.js')

const PROTOCOL = 'holebridge'
const MIB = 1024 * 1024
// The UDP sockets of the flows: one UDX instance for the process, as lib/udp.js uses.
let udx = null
// HANG_MS is how long a 'hang' service holds its open before it is rejected with code 4. The mux answers
// an open at once, so the host holds the open message itself.
const HANG_MS = 300
const TARGETS = ['refuse', 'hang']

function noop() {}

function delay(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms))
}

// udpSocket makes a UDP socket on 127.0.0.1's side of a flow.
function udpSocket() {
  if (udx === null) udx = new UDX()
  return udx.createSocket()
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
 *   (a TCP service at 127.0.0.1:port, or for kind udp a UDP service at 127.0.0.1:port), 'refuse' (the open
 *   is rejected with code 3) or 'hang' (rejected with code 4 after HANG_MS). Default [].
 *   A flow (message 9) to a udp service is relayed to its port, and each reply comes back as a datagram
 *   (message 10) on the flow. The mux serves no flows, so the fake host does this itself.
 * - flags: handshake flag bits, default 0. The lan bit is set by the lan option, not here.
 * - lan: false (default), or { addresses, port } for the handshake's lan block, or true. true also starts
 *   the LAN listener on 127.0.0.1 (a port the OS picks) and advertises it in the lan block with address
 *   127.0.0.1. The listener runs the Noise handshake (IK) under the host key pair, and destroys any remote
 *   key but the client key pair, as the DHT firewall does.
 * - version: protocol version in the handshake, default 1. Another value tests a version mismatch.
 * - onStream: null (default), or a function that receives each admitted app stream, from the DHT or the
 *   LAN, before the channel opens. A test writes raw frames to it, below the host's own mux.
 *
 * The result has publicKey (the host public key to dial), client (the client key pair the firewall
 * admits, for the test to dial with), lanPort (the LAN listener's TCP port, or null unless lan is true)
 * and close(), which stops the servers, their sockets and its DHT node. Nothing here logs keys.
 */
async function createFakeHost({ testnet, key, appKey, services = [], flags = 0, lan = false, version = 1, onStream = null }) {
  const pairs = await keys.derive(keys.normalize(key), appKey)
  const byName = new Map(services.map((s) => [s.name, s]))
  const budget = new Budget(256 * MIB)
  const conns = new Set() // accepted app connections, from the DHT or the LAN
  const targets = new Set() // TCP sockets to service targets
  let hello = null // the handshake the host sends, built once the LAN port is known
  let lanServer = null
  let lanPort = null

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
    if (onStream) onStream(socket)
    const mux = Protomux.from(socket)
    let session = null
    let channel = null
    let queue = Promise.resolve()
    const flows = new Map() // flow id -> { sock, port }: the UDP socket of a flow and its service's port
    socket.once('close', () => {
      conns.delete(socket)
      if (session) session.destroy()
      for (const flow of flows.values()) flow.sock.close().catch(noop)
      flows.clear()
    })

    const isUdp = (name) => byName.get(name)?.kind === 'udp' && typeof byName.get(name).target === 'object'

    // A flow to a udp service opens a UDP socket to the service's port. Its replies go back as datagrams (10).
    const openFlow = ({ flow, service, payload }) => {
      const sock = udpSocket()
      sock.on('error', noop)
      sock.on('message', (reply) => channel.messages[10].send({ flow, payload: b4a.from(reply) }))
      sock.bind(0, '127.0.0.1')
      const port = byName.get(service).target.port
      flows.set(flow, { sock, port })
      sock.trySend(payload, port, '127.0.0.1')
    }

    const deliver = async (index, message) => {
      if (index === 0 && byName.get(message.service)?.target === 'hang') await delay(HANG_MS)
      if (index === 9 && isUdp(message.service)) return openFlow(message)
      if (index === 10 && flows.has(message.flow)) {
        const { sock, port } = flows.get(message.flow)
        sock.trySend(message.payload, port, '127.0.0.1')
        return
      }
      if (!session.closed) session.receive(index, message)
    }

    mux.pair({ protocol: PROTOCOL }, () => {
      channel = mux.createChannel({
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

  // lanAccept runs the LAN listener's side of one TCP connection: the Noise handshake under the host key
  // pair, with the IK pattern the app's initiator uses, then serve for an admitted client. Any other
  // remote key is destroyed.
  const lanAccept = (socket) => {
    conns.add(socket)
    socket.once('close', () => conns.delete(socket))
    socket.on('error', noop)
    const stream = new NoiseSecretStream(false, socket, { keyPair: pairs.host, pattern: 'IK' })
    stream.on('error', noop)
    stream.opened.then((ok) => {
      if (ok && b4a.equals(stream.remotePublicKey, pairs.client.publicKey)) serve(stream)
      else stream.destroy()
    })
  }

  const close = async () => {
    for (const socket of conns) socket.destroy()
    for (const sock of targets) sock.destroy()
    if (lanServer) await new Promise((resolve) => lanServer.close(() => resolve()))
    await server.close()
    await dht.destroy()
  }

  try {
    await dht.fullyBootstrapped()
    if (lan === true) {
      lanServer = TCP.createServer(lanAccept)
      await new Promise((resolve, reject) => {
        lanServer.once('error', reject)
        lanServer.listen(0, '127.0.0.1', 511, {}, resolve)
      })
      lanPort = lanServer.address().port
    }
    const lanBlock = lan === true ? { addresses: ['127.0.0.1'], port: lanPort } : lan
    hello = {
      version,
      flags: lan ? flags | protocol.FLAG.lan : flags & ~protocol.FLAG.lan,
      services: services.map(handshakeEntry),
      ...(lan ? { lan: lanBlock } : {})
    }
    server.on('connection', serve)
    await server.listen(pairs.host)
  } catch (err) {
    await close()
    throw err
  }
  return { publicKey: pairs.host.publicKey, client: pairs.client, lanPort, close }
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
