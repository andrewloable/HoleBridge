// UDP services on the app engine (docs/architecture.md, "UDP services" and "Limits"; the unordered-datagram
// measurements in docs/spike-m1.md). Each udp service gets one local UDP socket on 127.0.0.1. Each source address
// that sends to it is a flow, with an id the app chooses. The first datagram of a flow goes in a flow message (9);
// later ones go as unordered datagrams, or as datagram messages (10) when the session has no unordered path. Replies
// go back to the flow's source. A flow closes after idleMs with no datagram either way and is never resumed.
//
// new UdpServices({ session, bind, maxDatagram, maxFlows, idleMs, clock, log, createSocket }) takes:
// - session: the HostSession of one connected session (lib/connect.js, which lib/lan-session.js also uses). It has:
//   - route: 'direct', 'relay' or 'lan'. flags: the host's handshake flags (protocol.FLAG). A session has unordered
//     datagrams when flags has FLAG.datagrams and the route is not 'lan', since the LAN route is TCP.
//   - sendFlow({ flow, service, payload }): message 9, always taken.
//   - sendDatagram({ flow, payload }): message 10. Returns false and takes nothing while the ordered channel is full;
//     the session then emits 'drain' once it can take more. A closed session takes it and sends nothing.
//   - sendUnordered({ flow, payload }): an unordered datagram, on the direct and relay routes only.
//   - on('datagram', fn) and on('drain', fn): fn({ flow, payload }) for each reply from the host, by either path.
// - bind: the address the local sockets bind, default 127.0.0.1.
// - maxDatagram: the largest payload in bytes, default 1144 (docs/architecture.md, "Limits"). A larger datagram is
//   dropped in either direction, and HB-UDP-TOO-LARGE is logged once per flow.
// - maxFlows: the most flows the session holds across all its udp services, default 256 (docs/architecture.md,
//   "Limits"). The datagram of a new source beyond that is dropped and nothing is sent.
// - idleMs: a flow closes this long after its last datagram either way, default 60 s.
// - clock: { setTimeout(fn, ms), clearTimeout(id) }. The idle rule runs through it.
// - log: a logger as lib/log.js makes it. HB-UDP-TOO-LARGE is written with log.warn(msg, { code, flow }); the payload
//   is never logged.
// - createSocket(): a UDP socket as udx-native makes it: bind(port, host), address() -> { host, family, port },
//   trySend(payload, port, host), on('message', fn) with fn(payload, { host, family, port }), on('error', fn) and
//   close() (a promise). Default: udx-native.
//
// set(services, remembered) binds one socket per service whose kind is 'udp' (services is [{ name, kind, port }], as
// lib/listeners.js takes it). A socket binds its remembered port first, then the service's port hint, then a port the
// OS picks. It returns { [name]: boundPort }. close() closes every socket and forgets every flow.
//
// A session replacement is a new UdpServices: the caller closes the old one, constructs a new one on the new session,
// and calls set() with the old ports as remembered. The next datagram from a source then opens a fresh flow.

const b4a = require('b4a')
const UDX = require('udx-native')
const { FLAG } = require('./protocol.js')

// QUEUE_BYTES is the queue of datagrams the ordered channel has not taken yet, on a session without unordered
// datagrams. It holds 256 KiB of payload; a datagram that does not fit is dropped.
const QUEUE_BYTES = 256 * 1024

// MAX_FLOWS is the most flows a session holds across all its udp services (docs/architecture.md, "Limits").
const MAX_FLOWS = 256

let udx = null

// defaultSocketFactory makes a UDP socket from one shared UDX instance, as lib/vpn-dns.js does.
function defaultSocketFactory() {
  if (udx === null) udx = new UDX()
  return udx.createSocket()
}

function noop() {}

// closeQuietly closes a socket and resolves once it is closed. A socket that will not close is not an error here.
async function closeQuietly(socket) {
  try {
    await socket.close()
  } catch {
    // already closed or never bound: nothing is left to do
  }
}

class UdpServices {
  #session
  #bind
  #maxDatagram
  #idleMs
  #clock
  #log
  #createSocket
  #services = new Map() // service name -> { name, socket, port, sources: Map(source key -> flow) }
  #flows = new Map() // flow id -> flow, for the replies of the session
  #nextId = 1
  #queue = [] // { flow, payload } the ordered channel has not taken yet, in order
  #queued = 0 // bytes in #queue
  #closed = false
  #maxFlows
  #limitLogged = false // HB-LIMIT-REACHED is logged once, until a flow has expired

  constructor({
    session,
    bind = '127.0.0.1',
    maxDatagram = 1144,
    maxFlows = MAX_FLOWS,
    idleMs = 60_000,
    clock,
    log,
    createSocket = defaultSocketFactory
  }) {
    this.#session = session
    this.#bind = bind
    this.#maxDatagram = maxDatagram
    this.#maxFlows = maxFlows
    this.#idleMs = idleMs
    this.#clock = clock
    this.#log = log
    this.#createSocket = createSocket
    session.on('datagram', (m) => this.#onReply(m))
    session.on('drain', () => this.#onDrain())
  }

  // set(services, remembered) makes the sockets match services: a socket for each udp service that has none, and
  // the sockets of services no longer listed are closed, with their flows. It returns { [name]: port } for the udp
  // services.
  set(services, remembered = {}) {
    if (this.#closed) throw new Error('UDP services are closed')
    const wanted = new Map()
    for (const service of services) if (service.kind === 'udp') wanted.set(service.name, service)
    for (const name of [...this.#services.keys()]) if (!wanted.has(name)) this.#stop(name)
    const ports = {}
    for (const [name, service] of wanted) {
      if (!this.#services.has(name)) this.#start(service, remembered[name])
      ports[name] = this.#services.get(name).port
    }
    return ports
  }

  // close() closes every socket and forgets every flow. Nothing is sent: flows are not resumed.
  async close() {
    this.#closed = true
    for (const flow of this.#flows.values()) this.#clear(flow)
    this.#flows.clear()
    const sockets = [...this.#services.values()].map((svc) => svc.socket)
    this.#services.clear()
    this.#queue = []
    this.#queued = 0
    await Promise.all(sockets.map(closeQuietly))
  }

  // start binds a socket for service: the remembered port, then the port hint, then a port the OS picks. A port
  // that is taken or not allowed falls back to the next choice, as lib/listeners.js does.
  #start(service, remembered) {
    const choices = [remembered, service.port, 0].filter(
      (port, i, all) => Number.isInteger(port) && all.indexOf(port) === i
    )
    let failure = null
    for (const port of choices) {
      const socket = this.#createSocket()
      try {
        socket.bind(port, this.#bind)
      } catch (err) {
        failure = err
        closeQuietly(socket)
        continue
      }
      socket.on('error', noop) // every socket has an error handler (docs/architecture.md, "Malformed input")
      const svc = { name: service.name, socket, port: socket.address().port, sources: new Map() }
      socket.on('message', (payload, from) => this.#onSocket(svc, payload, from))
      this.#services.set(service.name, svc)
      return
    }
    throw failure
  }

  // stop closes the socket of a service that is no longer listed, and forgets its flows.
  #stop(name) {
    const svc = this.#services.get(name)
    this.#services.delete(name)
    for (const flow of svc.sources.values()) {
      this.#clear(flow)
      this.#flows.delete(flow.id)
    }
    closeQuietly(svc.socket)
  }

  // onSocket takes a datagram from a local source. A new source opens its flow, unless the session already holds
  // maxFlows flows: then the datagram is dropped before any state is made for it. The datagram is then sent: the
  // first one in a flow message, and the rest unordered or as message 10. One over the limit is dropped and does not
  // restart the idle time, as the host never sees it.
  #onSocket(svc, payload, from) {
    if (this.#closed) return
    const key = `${from.host}:${from.port}`
    let flow = svc.sources.get(key)
    if (flow === undefined) {
      if (this.#flows.size >= this.#maxFlows) {
        if (!this.#limitLogged) {
          this.#limitLogged = true
          this.#log.warn('too many UDP flows, a datagram is dropped', { code: 'HB-LIMIT-REACHED' })
        }
        return
      }
      flow = {
        id: this.#nextId++,
        svc,
        key,
        source: { host: from.host, port: from.port },
        opened: false,
        logged: false,
        timer: null
      }
      svc.sources.set(key, flow)
      this.#flows.set(flow.id, flow)
      this.#touch(flow)
    }
    if (payload.length > this.#maxDatagram) {
      this.#tooLarge(flow)
      return
    }
    this.#touch(flow)
    if (!flow.opened) {
      flow.opened = true
      this.#session.sendFlow({ flow: flow.id, service: svc.name, payload })
    } else if (this.#unordered()) {
      this.#session.sendUnordered({ flow: flow.id, payload })
    } else {
      this.#sendOrdered(flow.id, payload)
    }
  }

  // onReply takes a reply from the host and sends it to the flow's source. A reply for a flow that is closed or was
  // never opened is dropped without a throw: the host is an untrusted peer, and a late reply races the idle close.
  // A reply restarts the idle time, as the host's own reply does.
  #onReply({ flow: id, payload }) {
    const flow = this.#flows.get(id)
    if (this.#closed || flow === undefined || !flow.opened) return
    this.#touch(flow)
    if (payload.length > this.#maxDatagram) {
      this.#tooLarge(flow)
      return
    }
    flow.svc.socket.trySend(payload, flow.source.port, flow.source.host)
  }

  // unordered reports whether datagrams go on the unordered path of the session.
  #unordered() {
    return (this.#session.flags & FLAG.datagrams) !== 0 && this.#session.route !== 'lan'
  }

  // sendOrdered sends a datagram as message 10. While the channel is full, or datagrams are already queued ahead of
  // it, the datagram waits in the queue; one that does not fit in the queue is dropped.
  #sendOrdered(id, payload) {
    if (this.#queue.length === 0 && this.#session.sendDatagram({ flow: id, payload })) return
    if (this.#queued + payload.length > QUEUE_BYTES) return
    this.#queue.push({ flow: id, payload: b4a.from(payload) })
    this.#queued += payload.length
  }

  // onDrain sends the queued datagrams, in order, for as long as the channel takes them.
  #onDrain() {
    if (this.#closed) return
    while (this.#queue.length > 0) {
      const { flow, payload } = this.#queue[0]
      if (!this.#session.sendDatagram({ flow, payload })) return
      this.#queue.shift()
      this.#queued -= payload.length
    }
  }

  // tooLarge logs HB-UDP-TOO-LARGE once for a flow. The payload is not logged.
  #tooLarge(flow) {
    if (flow.logged) return
    flow.logged = true
    this.#log.warn('a datagram over the limit is dropped', { code: 'HB-UDP-TOO-LARGE', flow: flow.id })
  }

  // touch restarts a flow's idle time: the flow closes idleMs after its last datagram either way.
  #touch(flow) {
    this.#clear(flow)
    flow.timer = this.#clock.setTimeout(() => this.#expire(flow), this.#idleMs)
  }

  #clear(flow) {
    if (flow.timer !== null) this.#clock.clearTimeout(flow.timer)
    flow.timer = null
  }

  // expire closes an idle flow. Its id is not reused, and the next datagram from its source opens a new flow.
  #expire(flow) {
    flow.timer = null
    flow.svc.sources.delete(flow.key)
    this.#flows.delete(flow.id)
    this.#limitLogged = false
  }
}

module.exports = { UdpServices }
