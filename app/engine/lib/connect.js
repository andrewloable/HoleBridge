// Connect over the direct route: dial the host's key-derived public key with HyperDHT, open the
// 'holebridge' channel and exchange handshakes (docs/architecture.md, "Direct route (connection flow)").

const c = require('compact-encoding')
const keys = require('./keys.js')
const protocol = require('./protocol.js')
const { Session, Budget, Counter, Keep } = require('./mux.js')
const { relayPolicy } = require('./relay.js')
const { guardedProtomux } = require('./frame-guard.js')

const PROTOCOL = 'holebridge'
const VERSION = 1
// The handshake flags this engine supports. Unordered datagrams: the connection carries them, and the
// UDP task routes them. Stream resume is built (lib/mux.js) but not offered yet: FLAG.resume stays off until
// the Go host interop test covers it.
const SUPPORTED_FLAGS = protocol.FLAG.datagrams
// How long a search may take before it gives up (spec/ipc.md: 60 s).
const LOOKUP_TIMEOUT = 60 * 1000
const MIB = 1024 * 1024
// The receive budget (64 MiB in the app) and the stream count (1024) are shared by every session of
// the process (docs/architecture.md, Limits).
const budget = new Budget(64 * MIB)
const counter = new Counter(1024)
// The bytes kept for resending streams after a reattach: 32 MiB in all over the process (docs/architecture.md, Limits).
const keep = new Keep()
// muxes maps each HostSession to its mux session, so that adopt(prev) can hand prev's streams over.
const muxes = new WeakMap()

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
 * handshake flags), route ('direct'), open(service) -> Promise<Stream>, destroy(), adopt(prev), and the UDP calls
 * sendFlow, sendDatagram and sendUnordered (lib/udp.js). on(event, fn) takes 'close', 'datagram' (a host
 * datagram, { flow, payload }) and 'drain' (the ordered channel can take more). The route is always 'direct' here: HyperDHT does not say on the connection
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
  return openSession(conn, { route: 'direct', flags, lookupTimeout })
}

/**
 * openSession(conn, { route, flags, lookupTimeout }) -> Promise<HostSession>
 *
 * The channel setup both routes share (docs/architecture.md, "Direct route" and "Session over the LAN").
 * conn is the Noise stream to the host, under the client key pair and expecting the host public key: the
 * DHT's stream for the direct route, the LAN stream for lib/lan-session.js. It opens the 'holebridge'
 * channel, exchanges the handshakes and resolves with the HostSession, whose route is route. Rejects, and
 * destroys conn, as connectDirect does. The receive budget and the stream count are shared by every route.
 */
function openSession(conn, { route, flags = SUPPORTED_FLAGS, lookupTimeout = LOOKUP_TIMEOUT }) {
  // The guard refuses a control batch inside a control batch before protomux decodes it (lib/frame-guard.js).
  const mux = guardedProtomux(conn)
  const closeListeners = []
  // The events the UDP layer (lib/udp.js) uses: a datagram from the host, and the ordered channel's drain.
  const datagramListeners = []
  const drainListeners = []
  const eventListeners = { close: closeListeners, datagram: datagramListeners, drain: drainListeners }
  const emit = (event, arg) => {
    for (const listener of eventListeners[event]) listener(arg)
  }
  let pending = null // { resolve, reject } until the host's handshake settles the connect
  let channel = null
  let session = null
  let closed = false

  const connected = new Promise((resolve, reject) => {
    pending = { resolve, reject }
  })

  // close tears the connection down, once. Before the handshake it rejects connectDirect; after it, the
  // close listeners get the error, if any. The transport is gone, so the mux session detaches: a session with
  // resume keeps its streams for the grace period, and any other closes them.
  const close = (err) => {
    if (closed) return
    closed = true
    clearTimeout(timer)
    if (session) session.detach()
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
  // An unordered datagram from the host, on the DHT route (the LAN route is TCP and has none). A frame that does not
  // decode is dropped, as UDP drops it.
  conn.on('message', (buffer) => {
    if (closed || route === 'lan') return
    let m = null
    try {
      m = c.decode(protocol.unordered, buffer)
    } catch {
      return
    }
    emit('datagram', m)
  })

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
    ondrain: () => emit('drain'),
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
        // A closed connection sends nothing: a detached session may still have a write in flight.
        send: (index, m) => {
          if (!closed) channel.messages[index].send(m)
        },
        budget,
        counter,
        keep,
        datagram: (m) => emit('datagram', m)
      })
      // Resume needs both sides: this side asked for it in its flags, and the host set it in its handshake.
      if (flags & remote.flags & protocol.FLAG.resume) session.enableResume()
      const { resolve } = pending
      pending = null
      clearTimeout(timer)
      const hostSession = {
        services: remote.services,
        lan: remote.lan || null,
        flags: remote.flags,
        route,
        open: (service) => session.open(service),
        // The UDP flows (lib/udp.js) send through these. Once the session has ended they send nothing, and a
        // datagram is lost, as on any UDP path.
        sendFlow({ flow, service, payload }) {
          if (closed) return
          channel.messages[9].send({ flow, service, payload })
        },
        // Returns false, and takes nothing, while the ordered channel is full: the caller waits for 'drain'.
        sendDatagram({ flow, payload }) {
          if (closed) return true
          if (!channel.drained) return false
          channel.messages[10].send({ flow, payload })
          return true
        },
        sendUnordered({ flow, payload }) {
          if (closed || route === 'lan') return
          conn.trySend(c.encode(protocol.unordered, { flow, payload }))
        },
        on(event, listener) {
          if (!Object.hasOwn(eventListeners, event)) {
            throw new Error(`HostSession emits close, datagram and drain only, not ${event}`)
          }
          eventListeners[event].push(listener)
        },
        // adopt(prev) takes over the streams that prev, an earlier session of this host, detached (lib/mux.js).
        // It throws when they cannot move, and the caller then destroys prev.
        adopt(prev) {
          session.adopt(muxes.get(prev))
        },
        // destroy ends the streams the session holds, even after its transport has gone, and then the transport.
        destroy() {
          if (session) session.destroy()
          close()
        }
      }
      muxes.set(hostSession, session)
      resolve(hostSession)
    },
    onclose: () => close()
  })

  if (channel) channel.open({ version: VERSION, flags, services: [] })
  else close(hbError('HB-LOOKUP-TIMEOUT', 'no connection to open the channel on'))
  return connected
}

module.exports = { connectDirect, openSession }
