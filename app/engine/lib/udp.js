// UDP services on the app engine (docs/architecture.md, "UDP services" and "Limits"; the spike, docs/spike-m1.md,
// "Unordered datagrams"). Each udp service gets one local UDP socket on 127.0.0.1. Each source address that sends to
// it is a flow, with an id the app chooses. The first datagram of a flow goes in a flow message (9); later ones go as
// unordered datagrams, or as datagram messages (10) when the session has no unordered path. Replies go back to the
// flow's source. Flows close after idleMs with no datagram either way and are never resumed.
//
// new UdpServices({ session, bind, maxDatagram, idleMs, clock, log, createSocket }) takes:
// - session: the HostSession of one connected session (lib/connect.js, lib/lan-session.js). UDP needs these from it,
//   and the IMPL task adds them to the HostSession and to lib/mux.js (messages 9 and 10 are not received there yet):
//   - route: 'direct', 'relay' or 'lan'. flags: the host's handshake flags (protocol.FLAG).
//     A session has unordered datagrams when flags has FLAG.datagrams and the route is not 'lan' (the LAN route is
//     TCP and has no unordered path).
//   - sendFlow({ flow, service, payload }): message 9 on the ordered channel. Always taken.
//   - sendDatagram({ flow, payload }): message 10 on the ordered channel. Returns false when the channel is full and
//     took nothing; the session then emits 'drain' once it can take more.
//   - sendUnordered({ flow, payload }): an unordered datagram.
//   - emits 'datagram' with { flow, payload } for each reply from the host, by either path, and 'drain'.
// - bind: the address the local sockets bind, default 127.0.0.1.
// - maxDatagram: the largest payload in bytes, default 1200 per the DESIGN. A larger datagram is dropped and counted,
//   and HB-UDP-TOO-LARGE is logged once per flow. The architecture doc says 1144 (see the IMPL task notes).
// - idleMs: a flow closes this long after its last datagram either way, default 60 s.
// - clock: { setTimeout(fn, ms), clearTimeout(id) }. The idle rule runs through it.
// - log: a logger as lib/log.js makes it. HB-UDP-TOO-LARGE is written with log.warn(msg, { code, flow }); the payload
//   is never logged.
// - createSocket(): a UDP socket as udx-native makes it (new UDX().createSocket()): bind(port, host), address() ->
//   { host, family, port } or null, trySend(payload, port, host), send(payload, port, host), on('message', fn) with
//   fn(payload, { host, family, port }), close(). Default: udx-native.
//
// set(services, remembered) binds one socket per service whose kind is 'udp' (services is [{ name, kind, port }], as
// lib/listeners.js takes it). remembered is { [name]: port }; a service's socket binds its remembered port first. It
// returns { [name]: boundPort }. close() closes every socket and forgets every flow.
//
// A session replacement is a new UdpServices: the caller closes the old one and constructs a new one with the new
// session, then calls set() with the old ports as remembered. The next datagram from a source opens a fresh flow.
//
// Until the IMPL task (HoleBridge-01p.2) lands, the constructor throws 'not implemented'.

class UdpServices {
  constructor({ session, bind = '127.0.0.1', maxDatagram = 1200, idleMs = 60_000, clock, log, createSocket }) {
    throw new Error('not implemented')
  }

  set(services, remembered) {
    throw new Error('not implemented')
  }

  close() {
    throw new Error('not implemented')
  }
}

module.exports = { UdpServices }
