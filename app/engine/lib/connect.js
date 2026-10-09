// Connect over the direct route: dial the host's key-derived public key with HyperDHT, open the
// 'holebridge' channel and exchange handshakes (docs/architecture.md, "Direct route (connection flow)").

const Protomux = require('protomux')
const c = require('compact-encoding')
const keys = require('./keys.js')
const protocol = require('./protocol.js')
const { Session, Budget, Counter } = require('./mux.js')
const { relayPolicy } = require('./relay.js')

const PROTOCOL = 'holebridge'
const VERSION = 1
// The handshake flags this engine supports. Unordered datagrams: the connection carries them, and the
// UDP task routes them. Stream resume is not built yet, so FLAG.resume stays off.
const SUPPORTED_FLAGS = protocol.FLAG.datagrams
// How long a search may take before it gives up (spec/ipc.md: 60 s).
const LOOKUP_TIMEOUT = 60 * 1000
const MIB = 1024 * 1024
// The receive budget (64 MiB in the app) and the stream count (1024) are shared by every session of
// the process (docs/architecture.md, Limits).
const budget = new Budget(64 * MIB)
const counter = new Counter(1024)

// hbError makes an error that carries an HB code and the reason, as keys.js does for its errors.
function hbError(code, reason) {
  return Object.assign(new Error(`${code}: ${reason}`), { name: 'HbError', code, reason })
}

// lookupError is the error a connect rejects with before the handshake: an HB error passes through, and
// anything else (HyperDHT's PEER_NOT_FOUND, a failed punch, a closed connection) means no answer from
// the host.
function lookupError(err) {
  if (err && typeof err.code === 'string' && err.code.startsWith('HB-')) return err
  const why = err && err.code ? `no answer from the host (${err.code})` : 'no answer from the host'
  return hbError('HB-LOOKUP-TIMEOUT', why)
}

// handshake is the protocol's handshake encoding, but the version is read first. A host on another
// version is reported as a mismatch, and its handshake never fails to decode whatever else it carries.
const handshake = {
  preencode: protocol.handshake.preencode,
  encode: protocol.handshake.encode,
  decode(state) {
    const start = state.start
    const version = c.uint.decode(state)
    if (version !== VERSION) return { version, flags: 0, services: [] }
    state.start = start
    return protocol.handshake.decode(state)
  }
}

/**
 * connectDirect({ dht, key, appKey, relayKey, flags, lookupTimeout }) -> Promise<HostSession>
 *
 * dht is a HyperDHT the caller owns and this does not destroy. key is the typed key, appKey the 32-byte
 * application key. relayKey, when set, is the deployment's relay key: the dial may go through that relay
 * (relay.js), and the dht should come from createDht with the same key. flags are the app's handshake
 * flag bits (lan is not allowed here), default the flags this engine supports. lookupTimeout is the ms
 * the search and the handshake may take before HB-LOOKUP-TIMEOUT, default 60000.
 *
 * Rejects with HB-LOOKUP-TIMEOUT when no host answers (HyperDHT's PEER_NOT_FOUND included) or the
 * handshake does not come in time, and with HB-VERSION-MISMATCH when the host speaks another version.
 *
 * The HostSession has services (the host's handshake list), lan (its lan block, or null), flags (its
 * handshake flags), route ('direct'), open(service) -> Promise<Stream>, destroy(), and emits 'close'
 * once the session ends. The route is always 'direct' here: HyperDHT does not say on the connection
 * whether it was relayed, and the engine has no seam that forces the relayed route (HoleBridge-7vk.5).
 */
async function connectDirect({ dht, key, appKey, relayKey, flags = SUPPORTED_FLAGS, lookupTimeout = LOOKUP_TIMEOUT }) {
  const pairs = await keys.derive(key, appKey)
  const opts = { keyPair: pairs.client }
  // With the relay key the dial may go through the relay (relay.js): the policy offers it when forced or
  // when this side's NAT randomizes. Without it, dht.connect gets no relayThrough.
  if (relayKey !== undefined && relayKey !== null) {
    const { server } = await keys.deriveRelay(relayKey, appKey)
    opts.relayThrough = relayPolicy({ relayServerPublicKey: server.publicKey, dht })
  }
  const conn = dht.connect(pairs.host.publicKey, opts)
  const mux = Protomux.from(conn)
  const closeListeners = []
  let pending = null // { resolve, reject } until the host's handshake settles the connect
  let channel = null
  let session = null
  let closed = false

  const connected = new Promise((resolve, reject) => {
    pending = { resolve, reject }
  })

  // close tears the connection down, once. Before the handshake it rejects connectDirect; after it, the
  // close listeners get the error, if any.
  const close = (err) => {
    if (closed) return
    closed = true
    clearTimeout(timer)
    if (session) session.destroy()
    if (channel) channel.close()
    conn.destroy()
    if (pending) pending.reject(lookupError(err))
    else for (const listener of closeListeners) listener(err)
  }

  const timer = setTimeout(
    () => close(hbError('HB-LOOKUP-TIMEOUT', `no answer from the host within ${lookupTimeout} ms`)),
    lookupTimeout
  )
  conn.on('error', close)
  conn.on('close', () => close())

  // A message that the session rejects closes the connection, as a protocol break does on the host.
  const messages = protocol.messages.map((encoding, index) => ({
    encoding,
    onmessage(m) {
      try {
        session.receive(index, m)
      } catch (err) {
        close(err)
      }
    }
  }))

  channel = mux.createChannel({
    protocol: PROTOCOL,
    handshake,
    messages,
    onopen(remote) {
      if (remote.version !== VERSION) {
        close(hbError(
          'HB-VERSION-MISMATCH',
          `this host speaks protocol v${remote.version} and this app speaks v${VERSION}: update the app from the store, or re-run the install script on the host`
        ))
        return
      }
      session = new Session({
        role: 'app',
        send: (index, m) => channel.messages[index].send(m),
        budget,
        counter
      })
      const { resolve } = pending
      pending = null
      clearTimeout(timer)
      resolve({
        services: remote.services,
        lan: remote.lan || null,
        flags: remote.flags,
        route: 'direct',
        open: (service) => session.open(service),
        on(event, listener) {
          if (event !== 'close') throw new Error(`HostSession emits close only, not ${event}`)
          closeListeners.push(listener)
        },
        destroy: () => close()
      })
    },
    onclose: () => close()
  })

  if (channel) channel.open({ version: VERSION, flags, services: [] })
  else close(hbError('HB-LOOKUP-TIMEOUT', 'no connection to open the channel on'))
  return connected
}

module.exports = { connectDirect }
