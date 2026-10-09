// VPN mode's local entry point: the one SOCKS5 front the network stack connects to (docs/architecture.md,
// "VPN mode (Android and iOS)"; spec/ipc.md point 7; RFC 1928). The front listens on 127.0.0.1 only, on the TCP
// port listen() resolves to, and speaks SOCKS5 with no authentication: CONNECT to a service's stream, and UDP
// ASSOCIATE for DNS and for UDP flows. Nothing here logs an address, a name or a payload.
//
// new VpnFront({ addresses, open, udp, dns, maxControls, maxFlows, maxFlowsTotal, maxDns, maxDatagram, handshakeMs,
//   log, logRearmMs }):
// - addresses: a plain object from each VPN address (198.18.x.y) to { host, service }. A CONNECT to an address not
//   in it is refused with "connection not allowed"; a UDP datagram to one is dropped, as UDP has no refusal.
// - open(host, service) -> Promise<Stream>: opens the service's stream, as lib/listeners.js does. A rejection with
//   HB-TARGET-REFUSED or HB-TARGET-TIMEOUT is answered with the matching SOCKS reply, any other with general failure.
// - udp(host, service) -> flow handle: send(payload) sends a datagram to the service, and the handle emits 'message'
//   with each reply payload. close() releases the flow. The front calls it for every flow of a UDP association when
//   that association's TCP control connection closes (RFC 1928, section 6).
// - dns: a VpnDns (lib/vpn-dns.js). Its handle(queryBuf) resolves with the reply bytes. DNS to 198.18.0.1:53 comes
//   through UDP ASSOCIATE.
// - log: optional, a logger as lib/log.js makes it; by default nothing is written. The front calls
//   log.warn(line, { code }) and nothing else, so the fields are { code } only. Each line is logged once per UDP
//   association, the first time it applies, and the front writes at most one line per code in each logRearmMs
//   (default 60 s), over the whole front. HB-UDP-TOO-LARGE ("a datagram over the limit is dropped") is for a service
//   datagram over maxDatagram, sent by the client or replied by a flow. It is per association, not per flow, since an
//   oversize first datagram never makes a flow. A DNS query over 2048 bytes or a DNS reply over 1490 bytes is not
//   logged with this code. A datagram to an address no service has is not logged either: it is dropped for having no
//   service, not for its size, and maxDatagram does not apply to it, since it never enters the tunnel.
//   HB-LIMIT-REACHED is for maxFlows or maxFlowsTotal ("too many UDP flows, a datagram is dropped") and for maxDns
//   ("too many DNS queries, a query is dropped"), each once per association; and for maxControls ("too many VPN front
//   connections, one is closed"), once for the front, again only after some control connection has closed and
//   logRearmMs has passed since the last such line.
// - listen() -> Promise<port>; close() -> Promise. close() destroys every control connection, with its streams and
//   flows, and stops the server.
// - Limits (docs/architecture.md, "Limits"): maxControls control connections (1024, streams in total); maxFlows flows
//   per UDP association (256) and maxFlowsTotal over all associations (4096); maxDns DNS queries in flight (256);
//   maxDatagram bytes per service datagram (1144). A DNS query is held to 2048 bytes and a DNS reply to 1490 bytes
//   instead, since DNS does not ride the tunnel's datagrams. udx-native delivers at most 2048 bytes per datagram, the
//   10-byte SOCKS header included, so a DNS query over 2038 bytes arrives truncated and is not supported. The network
//   stack reads at most 1500 bytes per datagram from the relay, the same header included, so a DNS reply over 1490
//   bytes is dropped with no reply and not logged. handshakeMs (10 s) is how long a control connection may take to send
//   its greeting and request. Past a limit the connection is closed or the datagram dropped, with no reply.

const b4a = require('b4a')
const TCP = require('bare-tcp')
const UDX = require('udx-native')

const LOOPBACK = '127.0.0.1'
const DNS_ADDRESS = '198.18.0.1'
const DNS_PORT = 53
// MAX_DNS is the largest DNS query, in bytes. udx-native's receive limit is 2048 bytes per datagram, the 10-byte SOCKS
// header included, so a longer query arrives truncated and is not supported.
const MAX_DNS = 2048
// MAX_DNS_REPLY is the largest DNS reply payload, in bytes. The network stack (hev-socks5-tunnel) reads each datagram
// from the relay into a 1500-byte buffer, the 10-byte SOCKS header included (UDP_BUF_SIZE in
// app/native/netstack/src/core/src/hev-socks5-udp.c), and cuts a longer one silently, so a longer reply is dropped.
const MAX_DNS_REPLY = 1490
const MAX_CONTROLS = 1024
const MAX_FLOWS = 256
const MAX_FLOWS_TOTAL = 4096
const MAX_DNS_IN_FLIGHT = 256
const MAX_DATAGRAM = 1144
const HANDSHAKE_MS = 10_000
// LOG_REARM_MS is the least time between two log lines with the same code, over the whole front (the header, "log").
const LOG_REARM_MS = 60_000
// MAX_PENDING bounds the bytes a CONNECT may send while its stream is still opening.
const MAX_PENDING = 64 * 1024

const SOCKS_VERSION = 5
const METHOD_NONE = 0x00
const METHOD_NO_ACCEPTABLE = 0xff
const CMD_CONNECT = 0x01
const CMD_UDP_ASSOCIATE = 0x03
const ATYP_IPV4 = 0x01
const ATYP_DOMAIN = 0x03
const ATYP_IPV6 = 0x04

// SOCKS5 reply codes (RFC 1928, section 6).
const REP_SUCCEEDED = 0x00
const REP_GENERAL = 0x01
const REP_NOT_ALLOWED = 0x02
const REP_HOST_UNREACHABLE = 0x04
const REP_CONNECTION_REFUSED = 0x05
const REP_COMMAND = 0x07
const REP_ATYP = 0x08

const EMPTY = b4a.alloc(0)

// The lines the front logs with a code (see the header, "log").
const LOG_TOO_LARGE = 'a datagram over the limit is dropped'
const LOG_CONTROLS = 'too many VPN front connections, one is closed'
const LOG_FLOWS = 'too many UDP flows, a datagram is dropped'
const LOG_DNS = 'too many DNS queries, a query is dropped'

let udx = null

function noop() {}

// NO_LOG is the logger when none is passed: it writes nothing.
const NO_LOG = { warn: noop }

// udpSocket makes a UDP socket from one shared UDX instance, as lib/vpn-dns.js does.
function udpSocket() {
  if (udx === null) udx = new UDX()
  return udx.createSocket()
}

// closeQuietly closes a UDP socket. A socket that will not close is not an error here.
async function closeQuietly(socket) {
  try {
    await socket.close()
  } catch {
    // already closed: nothing is left to do
  }
}

function closeServer(server) {
  return new Promise((resolve) => server.close(() => resolve()))
}

// disarm stops a control connection's handshake timer.
function disarm(conn) {
  if (conn.timer !== null) clearTimeout(conn.timer)
  conn.timer = null
}

function ipText(buf, i) {
  return `${buf[i]}.${buf[i + 1]}.${buf[i + 2]}.${buf[i + 3]}`
}

function ipBytes(address) {
  return b4a.from(address.split('.').map(Number))
}

// socksReply(rep, address, port) is a SOCKS5 reply with an IPv4 BND.ADDR (RFC 1928, section 6). By default the bound
// address is 0.0.0.0:0.
function socksReply(rep, address = '0.0.0.0', port = 0) {
  return b4a.from([SOCKS_VERSION, rep, 0, ATYP_IPV4, ...ipBytes(address), port >> 8, port & 0xff])
}

// repFor(err) is the SOCKS reply for a failed open. The wire's reject codes have their SOCKS replies; any other
// failure is a general failure.
function repFor(err) {
  if (err && err.code === 'HB-TARGET-REFUSED') return REP_CONNECTION_REFUSED
  if (err && err.code === 'HB-TARGET-TIMEOUT') return REP_HOST_UNREACHABLE
  return REP_GENERAL
}

// parseDatagram(msg) reads the RFC 1928 section 7 header of a datagram from the client. It returns null for what the
// front does not carry: a nonzero RSV, a fragment (FRAG other than 0), or an address that is not IPv4.
function parseDatagram(msg) {
  if (msg.length < 10 || msg[0] !== 0 || msg[1] !== 0 || msg[2] !== 0 || msg[3] !== ATYP_IPV4) return null
  return {
    address: ipText(msg, 4),
    port: (msg[8] << 8) | msg[9],
    payload: b4a.from(msg.subarray(10))
  }
}

class VpnFront {
  #addresses
  #open
  #udp
  #dns
  #maxControls
  #maxFlows
  #maxFlowsTotal
  #maxDns
  #maxDatagram
  #handshakeMs
  #log
  #server = null
  #closed = false
  #logRearmMs
  #controls = new Set() // one entry per accepted control connection that is not yet torn down
  #lastLogAt = new Map() // code -> Date.now() of the last line with that code, over the whole front
  #controlLogAt = null // Date.now() of the last maxControls line
  #controlClosed = false // true once a control connection has closed since that line
  #dnsInflight = 0 // DNS queries whose handle has not settled, over the whole front
  #flowCount = 0 // UDP flow handles held, over all associations

  constructor({
    addresses,
    open,
    udp,
    dns,
    maxControls = MAX_CONTROLS,
    maxFlows = MAX_FLOWS,
    maxFlowsTotal = MAX_FLOWS_TOTAL,
    maxDns = MAX_DNS_IN_FLIGHT,
    maxDatagram = MAX_DATAGRAM,
    handshakeMs = HANDSHAKE_MS,
    log = NO_LOG,
    logRearmMs = LOG_REARM_MS
  }) {
    this.#addresses = addresses
    this.#open = open
    this.#udp = udp
    this.#dns = dns
    this.#maxControls = maxControls
    this.#maxFlows = maxFlows
    this.#maxFlowsTotal = maxFlowsTotal
    this.#maxDns = maxDns
    this.#maxDatagram = maxDatagram
    this.#handshakeMs = handshakeMs
    this.#log = log
    this.#logRearmMs = logRearmMs
  }

  // listen() binds 127.0.0.1, never another address, on a port the OS picks, and resolves to that TCP port.
  async listen() {
    if (this.#closed) throw new Error('the VPN front is closed')
    if (this.#server !== null) throw new Error('the VPN front is already listening')
    const server = TCP.createServer((socket) => this.#accept(socket))
    await new Promise((resolve, reject) => {
      server.on('error', reject) // bare-tcp reports a bind failure as an 'error' event, not a throw
      server.listen(0, LOOPBACK, () => resolve())
    })
    server.on('error', noop)
    if (this.#closed) {
      await closeServer(server)
      throw new Error('the VPN front is closed')
    }
    this.#server = server
    return server.address().port
  }

  // close() destroys every control connection, with its stream and its UDP flows, and stops the server. It resolves
  // once the server has closed and the UDP relay sockets are closed.
  async close() {
    this.#closed = true
    const server = this.#server
    this.#server = null
    await Promise.all([...this.#controls].map((conn) => this.#teardown(conn)))
    if (server !== null) await closeServer(server)
  }

  // accept takes a control connection. Past maxControls, or once the front is closed, it is destroyed at once. A
  // refusal for maxControls is logged once, and again only after a control connection has closed and logRearmMs has
  // passed since the last such line.
  #accept(socket) {
    if (this.#closed) {
      socket.destroy()
      return
    }
    if (this.#controls.size >= this.#maxControls) {
      const now = Date.now()
      if (this.#controlLogAt === null || (this.#controlClosed && now - this.#controlLogAt >= this.#logRearmMs)) {
        if (this.#emit(LOG_CONTROLS, 'HB-LIMIT-REACHED')) {
          this.#controlLogAt = now
          this.#controlClosed = false
        }
      }
      socket.destroy()
      return
    }
    const conn = { socket, state: 'greeting', buf: EMPTY, timer: null, gone: false, stream: null, association: null }
    this.#controls.add(conn)
    socket.on('error', noop) // an error is always followed by close, which does the cleanup
    socket.on('data', (chunk) => this.#onData(conn, chunk))
    // A client that ends its side ends the connection, except while its stream relays: then the pipe ends the stream.
    socket.on('end', () => {
      if (conn.state !== 'relay') this.#teardown(conn)
    })
    socket.once('close', () => this.#teardown(conn))
    conn.timer = setTimeout(() => this.#teardown(conn), this.#handshakeMs)
  }

  // onData takes bytes from a control connection. The greeting and the request are parsed as they arrive. While a
  // CONNECT's stream opens, its bytes are kept, up to MAX_PENDING, and sent to the stream once it is open. Bytes that
  // come after an association's request, or during a refusal, are ignored.
  #onData(conn, chunk) {
    if (conn.state === 'greeting' || conn.state === 'request') {
      conn.buf = b4a.concat([conn.buf, chunk])
      this.#parse(conn)
    } else if (conn.state === 'opening') {
      if (conn.buf.length + chunk.length > MAX_PENDING) {
        this.#teardown(conn)
        return
      }
      conn.buf = b4a.concat([conn.buf, chunk])
    }
  }

  #parse(conn) {
    if (conn.state === 'greeting') this.#greeting(conn)
    if (conn.state === 'request') this.#request(conn)
  }

  // greeting reads VER, NMETHODS and METHODS (RFC 1928, section 3). The front takes method 0x00, no authentication. A
  // client that offers no such method gets 0xFF and is closed.
  #greeting(conn) {
    const buf = conn.buf
    if (buf.length < 2) return
    if (buf[0] !== SOCKS_VERSION) {
      this.#teardown(conn)
      return
    }
    const need = 2 + buf[1]
    if (buf.length < need) return
    const methods = buf.subarray(2, need)
    conn.buf = buf.subarray(need)
    if (!methods.includes(METHOD_NONE)) {
      this.#refuse(conn, b4a.from([SOCKS_VERSION, METHOD_NO_ACCEPTABLE]))
      return
    }
    conn.socket.write(b4a.from([SOCKS_VERSION, METHOD_NONE]))
    conn.state = 'request'
  }

  // request reads VER, CMD, RSV, ATYP, DST.ADDR and DST.PORT (RFC 1928, section 4). The whole request is read before
  // any reply, so the client is not reset while it is still sending. A command other than CONNECT or UDP ASSOCIATE
  // gets REP 0x07; an address that is not IPv4 gets REP 0x08.
  #request(conn) {
    const buf = conn.buf
    if (buf.length < 4) return
    if (buf[0] !== SOCKS_VERSION) {
      this.#teardown(conn)
      return
    }
    const atyp = buf[3]
    let need
    if (atyp === ATYP_IPV4) {
      need = 10
    } else if (atyp === ATYP_DOMAIN) {
      if (buf.length < 5) return
      need = 7 + buf[4]
    } else if (atyp === ATYP_IPV6) {
      need = 22
    } else {
      this.#refuse(conn, socksReply(REP_ATYP))
      return
    }
    if (buf.length < need) return
    conn.buf = buf.subarray(need)
    const command = buf[1]
    if (command !== CMD_CONNECT && command !== CMD_UDP_ASSOCIATE) {
      this.#refuse(conn, socksReply(REP_COMMAND))
      return
    }
    if (atyp !== ATYP_IPV4) {
      this.#refuse(conn, socksReply(REP_ATYP))
      return
    }
    const address = ipText(buf, 4)
    if (command === CMD_CONNECT) this.#connect(conn, address)
    else this.#associate(conn)
  }

  // connect opens the service of a CONNECT's address, or refuses an address no service has. The reply goes out once
  // the stream is open.
  #connect(conn, address) {
    if (!Object.hasOwn(this.#addresses, address)) {
      this.#refuse(conn, socksReply(REP_NOT_ALLOWED))
      return
    }
    const { host, service } = this.#addresses[address]
    conn.state = 'opening'
    disarm(conn)
    this.#openStream(conn, host, service)
  }

  // openStream opens the service's stream, then sends the reply and carries bytes both ways. A stream that opens after
  // its connection has gone is destroyed.
  async #openStream(conn, host, service) {
    let stream
    try {
      stream = await this.#open(host, service)
    } catch (err) {
      if (!conn.gone) this.#refuse(conn, socksReply(repFor(err)))
      return
    }
    if (conn.gone) {
      stream.destroy()
      return
    }
    conn.state = 'relay'
    conn.stream = stream
    stream.on('error', () => this.#teardown(conn))
    stream.on('close', () => {
      if (!conn.gone) conn.socket.end()
    })
    conn.socket.write(socksReply(REP_SUCCEEDED))
    const early = conn.buf
    conn.buf = EMPTY
    if (early.length > 0) stream.write(early)
    conn.socket.pipe(stream)
    stream.pipe(conn.socket)
  }

  // associate opens the UDP relay of an association: a UDP socket on 127.0.0.1 whose port goes in the reply. The
  // association lasts as long as its control connection (RFC 1928, section 6).
  #associate(conn) {
    const sock = udpSocket()
    try {
      sock.bind(0, LOOPBACK)
    } catch {
      closeQuietly(sock)
      this.#refuse(conn, socksReply(REP_GENERAL))
      return
    }
    sock.on('error', noop) // a socket always has an error handler, as lib/udp.js gives one
    const assoc = { sock, client: null, flows: new Map(), closed: false, logged: new Set() }
    sock.on('message', (msg, from) => this.#onDatagram(assoc, msg, from))
    conn.association = assoc
    conn.state = 'associate'
    conn.buf = EMPTY
    disarm(conn)
    conn.socket.write(socksReply(REP_SUCCEEDED, LOOPBACK, sock.address().port))
  }

  // onDatagram takes a datagram from the client of an association. The first well-formed one names the client, and
  // only that source is served. A datagram to 198.18.0.1:53 goes to VpnDns if it is at most 2048 bytes; one to a
  // service's address goes to its flow, if it is at most maxDatagram; anything else is dropped without a reply. A DNS
  // query past maxDns queries in flight is dropped the same way. The drops a limit causes are logged, as the header
  // says under "log".
  #onDatagram(assoc, msg, from) {
    if (assoc.closed) return
    const packet = parseDatagram(msg)
    if (packet === null) return
    if (assoc.client === null) assoc.client = { host: from.host, port: from.port }
    else if (from.host !== assoc.client.host || from.port !== assoc.client.port) return
    if (packet.address === DNS_ADDRESS && packet.port === DNS_PORT) {
      if (packet.payload.length > MAX_DNS) return
      if (this.#dnsInflight >= this.#maxDns) {
        this.#warnOnce(assoc.logged, LOG_DNS, 'HB-LIMIT-REACHED')
        return
      }
      this.#dnsInflight++
      this.#answerDns(assoc, packet.payload)
      return
    }
    if (!Object.hasOwn(this.#addresses, packet.address)) return
    if (packet.payload.length > this.#maxDatagram) {
      this.#warnOnce(assoc.logged, LOG_TOO_LARGE, 'HB-UDP-TOO-LARGE')
      return
    }
    this.#sendFlow(assoc, packet)
  }

  // emit writes a line with its code, unless a line with that code was written less than logRearmMs ago, over the
  // whole front. It returns whether it wrote the line. The fields are { code } only.
  #emit(line, code) {
    const now = Date.now()
    const last = this.#lastLogAt.get(code)
    if (last !== undefined && now - last < this.#logRearmMs) return false
    this.#lastLogAt.set(code, now)
    this.#log.warn(line, { code })
    return true
  }

  // warnOnce logs a line with its code once per association: logged is the association's set of the lines it has
  // logged. A line that emit holds back is not added to logged, so the association tries again at its next drop.
  #warnOnce(logged, line, code) {
    if (logged.has(line)) return
    if (!this.#emit(line, code)) return
    logged.add(line)
  }

  // answerDns asks VpnDns for a reply and sends it. The query's slot in dnsInflight is given back when handle settles,
  // whether it resolves or rejects.
  async #answerDns(assoc, query) {
    let answer
    try {
      answer = await this.#dns.handle(query)
    } catch {
      return
    } finally {
      this.#dnsInflight--
    }
    if (answer.length <= MAX_DNS_REPLY) this.#reply(assoc, DNS_ADDRESS, DNS_PORT, answer)
  }

  // sendFlow sends a datagram to the flow of its destination, opening the flow for a new destination. Past maxFlows
  // flows in the association, or maxFlowsTotal flows over all associations, the datagram of a new destination is
  // dropped. A reply over maxDatagram is dropped too. Both drops are logged once per association.
  #sendFlow(assoc, packet) {
    const key = `${packet.address}:${packet.port}`
    let flow = assoc.flows.get(key)
    if (flow === undefined) {
      if (assoc.flows.size >= this.#maxFlows || this.#flowCount >= this.#maxFlowsTotal) {
        this.#warnOnce(assoc.logged, LOG_FLOWS, 'HB-LIMIT-REACHED')
        return
      }
      const { host, service } = this.#addresses[packet.address]
      try {
        flow = this.#udp(host, service)
      } catch {
        return
      }
      const { address, port } = packet
      flow.on('message', (payload) => {
        if (payload.length <= this.#maxDatagram) this.#reply(assoc, address, port, payload)
        else if (!assoc.closed) this.#warnOnce(assoc.logged, LOG_TOO_LARGE, 'HB-UDP-TOO-LARGE')
      })
      assoc.flows.set(key, flow)
      this.#flowCount++
    }
    flow.send(packet.payload)
  }

  // reply sends a datagram to the client from the relay socket whose port went in the ASSOCIATE reply, so the network
  // stack accepts it. The header names where the reply came from (RFC 1928, section 7): the service or DNS.
  #reply(assoc, address, port, payload) {
    if (assoc.closed || assoc.client === null) return
    const head = b4a.from([0, 0, 0, ATYP_IPV4, ...ipBytes(address), port >> 8, port & 0xff])
    assoc.sock.trySend(b4a.concat([head, payload]), assoc.client.port, assoc.client.host)
  }

  // refuse sends a reply that refuses the request, then ends the connection after it (RFC 1928, section 6). The
  // handshake timer still runs, so a client that never closes is removed in time.
  #refuse(conn, bytes) {
    conn.state = 'closing'
    conn.buf = EMPTY
    conn.socket.write(bytes)
    conn.socket.end()
  }

  // teardown ends a control connection and everything it holds: its stream, its handshake timer and its UDP
  // association. A second call does nothing. The returned promise resolves once the association's relay socket has
  // closed.
  #teardown(conn) {
    if (conn.gone) return Promise.resolve()
    conn.gone = true
    disarm(conn)
    this.#controls.delete(conn)
    this.#controlClosed = true // a closed control connection re-arms the maxControls line, after logRearmMs
    conn.buf = EMPTY
    if (conn.stream !== null) conn.stream.destroy()
    conn.socket.destroy()
    return conn.association === null ? Promise.resolve() : this.#closeAssociation(conn.association)
  }

  // closeAssociation closes the flows of an association, then its relay socket. Its flows give back their slots in
  // maxFlowsTotal.
  async #closeAssociation(assoc) {
    if (assoc.closed) return
    assoc.closed = true
    this.#flowCount -= assoc.flows.size
    for (const flow of assoc.flows.values()) {
      try {
        flow.close()
      } catch {
        // a flow that fails to close does not stop the others from closing
      }
    }
    assoc.flows.clear()
    await closeQuietly(assoc.sock)
  }
}

module.exports = { VpnFront }
