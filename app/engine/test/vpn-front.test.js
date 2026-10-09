// Tests for VPN mode's local SOCKS5 front (lib/vpn-front.js). Rules: docs/architecture.md ("VPN mode (Android and
// iOS)"), spec/ipc.md point 7, and RFC 1928 (SOCKS5), which the front speaks: no authentication, CONNECT and UDP
// ASSOCIATE. The network stack (hev-socks5-tunnel, app/native/jni/holebridge_tun.c) is the real client. Here a
// hand-written SOCKS5 client plays it, so the code under test is not used to check itself.
//
// Shapes the IMPL must meet (also stated in lib/vpn-front.js):
// - new VpnFront({ addresses, open, udp, dns }). addresses is a plain object: VPN address -> { host, service }.
// - listen() binds 127.0.0.1 on a port the OS picks, and resolves to that TCP port. close() stops the front and the
//   sockets it accepted. The tests await both, so a plain return value or a promise works.
// - open(host, service) resolves with a stream (a streamx Duplex). Bytes the client sends go to the stream, and bytes
//   the stream emits go back to the client.
// - udp(host, service) returns a flow handle: send(payload) sends a datagram into the service, and the handle emits
//   'message' with the payload of each reply. The design does not spell out the handle; this is the shape assumed here.
//   close() releases the flow. The front calls it for every flow of a UDP association when that association's TCP
//   control connection closes (RFC 1928, section 6).
// - dns is a VpnDns (lib/vpn-dns.js). Its handle(queryBuf) resolves with the reply bytes; a fake stands in here.
// - The limits maxControls, maxFlows, maxFlowsTotal, maxDns, maxDatagram and handshakeMs are optional, and so is log
//   (a logger with warn(msg, fields)); the last tests set them to small values.
// - A UDP reply carries the RFC 1928 header whose source is the datagram's destination: the service address and port,
//   or 198.18.0.1:53 for DNS. The relay sends every reply from the UDP socket whose port it gave in the ASSOCIATE
//   reply (BND.ADDR 127.0.0.1, BND.PORT), because the network stack connect()s its UDP socket to that address.
// - Every successful reply has ATYP 1 (IPv4).
// - A refused CONNECT (any non-zero REP) gets its reply, and then the front closes the TCP connection (RFC 1928).
//
// Until the IMPL task (HoleBridge-hb5.5.4) lands, the constructor throws 'not implemented', so each test fails for
// that reason. Nothing here logs an address, a query or a payload.
const test = require('brittle')
const b4a = require('b4a')
const EventEmitter = require('bare-events')
const TCP = require('bare-tcp')
const UDX = require('udx-native')
const { Duplex, PassThrough } = require('streamx')
const { VpnFront } = require('../lib/vpn-front.js')

const LOOPBACK = '127.0.0.1'
const DNS_ADDRESS = '198.18.0.1'
// SETTLE bounds every wait, so a step that never finishes fails its test with a message instead of hanging the run.
const SETTLE = 10000

// The services the front routes by address: 198.18.0.2 is a TCP web service, 198.18.0.3 a UDP game service.
const ADDRESSES = {
  '198.18.0.2': { host: 'living-room', service: 'web' },
  '198.18.0.3': { host: 'living-room', service: 'game' }
}
// An address no service has, and an address outside the VPN range.
const UNASSIGNED = '198.18.0.99'
const OUTSIDE = '8.8.8.8'

// A DNS query for example.com, type A, class IN, and the reply the fake VpnDns gives for it.
const DNS_QUERY = b4a.from('123401000001000000000000076578616d706c6503636f6d0000010001', 'hex')
const DNS_REPLY = b4a.from('reply from the fake VpnDns')

const SOCKS_VERSION = 5
const METHOD_NONE = 0x00
const METHOD_USERPASS = 0x02
const CMD_CONNECT = 0x01
const CMD_UDP_ASSOCIATE = 0x03
const ATYP_IPV4 = 0x01
const REP_SUCCEEDED = 0x00
const REP_NOT_ALLOWED = 0x02

function noop() {}

function hex(buf) {
  return b4a.toString(buf, 'hex')
}

// dotted(bytes) is the IPv4 address in bytes, as text.
function dotted(bytes) {
  return Array.from(bytes).join('.')
}

function ipv4(address) {
  return b4a.from(address.split('.').map(Number))
}

function port16(n) {
  return b4a.from([(n >> 8) & 0xff, n & 0xff])
}

// within rejects if promise has not settled within SETTLE, so a step that never finishes fails its test.
function within(promise, what) {
  let timer
  const timeout = new Promise((resolve, reject) => {
    timer = setTimeout(() => reject(new Error(`${what} did not finish within ${SETTLE} ms`)), SETTLE)
  })
  return Promise.race([promise, timeout]).finally(() => clearTimeout(timer))
}

// until(check, what) polls check every 5 ms until it is true, and fails with what after SETTLE.
async function until(check, what) {
  const deadline = Date.now() + SETTLE
  while (!check()) {
    if (Date.now() > deadline) throw new Error(`${what} did not happen within ${SETTLE} ms`)
    await new Promise((resolve) => setTimeout(resolve, 5))
  }
}

// brittle ends the whole run when a test body throws. Until the IMPL task lands, the constructor throws
// 'not implemented', so each body runs under catchThrows, which fails only its own test.
function catchThrows(fn) {
  return async (t) => {
    try {
      await fn(t)
    } catch (err) {
      t.fail(err.message)
    }
  }
}

// runCleanup runs the steps a test queued (closing sockets and the front), last queued first. A step that fails does
// not change the test's result.
async function runCleanup(steps) {
  for (const step of steps.reverse()) {
    try {
      await step()
    } catch {
      // ignored: the test has its result already
    }
  }
}

// Reader reads exact byte counts from a TCP socket. take(n, what) resolves with the next n bytes, and rejects when the
// socket ends first. closed resolves when the socket has ended, closed or errored.
class Reader {
  #buf = b4a.alloc(0)
  #wake = null
  #ended = false
  closed

  constructor(socket) {
    this.closed = new Promise((resolve) => {
      const end = () => {
        this.#ended = true
        resolve()
        this.#notify()
      }
      socket.once('end', end)
      socket.once('close', end)
      socket.once('error', end)
    })
    socket.on('data', (chunk) => {
      this.#buf = b4a.concat([this.#buf, chunk])
      this.#notify()
    })
  }

  #notify() {
    const wake = this.#wake
    this.#wake = null
    if (wake) wake()
  }

  async take(n, what) {
    while (this.#buf.length < n) {
      if (this.#ended) throw new Error(`${what}: the connection ended with ${this.#buf.length} of ${n} bytes`)
      await within(
        new Promise((resolve) => {
          this.#wake = resolve
        }),
        what
      )
    }
    const out = this.#buf.subarray(0, n)
    this.#buf = this.#buf.subarray(n)
    return b4a.from(out)
  }
}

// connectTo(port, cleanup) opens a TCP connection to the front, with a Reader on it. It resolves once connected.
async function connectTo(port, cleanup) {
  const socket = TCP.createConnection(port, LOOPBACK)
  socket.on('error', noop)
  cleanup.push(() => socket.destroy())
  const reader = new Reader(socket)
  await within(
    new Promise((resolve, reject) => {
      socket.once('connect', resolve)
      socket.once('error', reject)
    }),
    'the connect to the front'
  )
  return { socket, reader }
}

// openSocks(port, cleanup) connects and sends the greeting for no authentication. It resolves once the front has
// chosen method 0x00.
async function openSocks(port, cleanup) {
  const conn = await connectTo(port, cleanup)
  conn.socket.write(b4a.from([SOCKS_VERSION, 1, METHOD_NONE]))
  const choice = await conn.reader.take(2, 'the method choice')
  if (hex(choice) !== '0500') throw new Error(`the front chose ${hex(choice)}, not 0500 (no authentication)`)
  return conn
}

// readReply(reader) reads one SOCKS5 reply: its REP, and BND.ADDR (dotted when ATYP is IPv4) and BND.PORT.
async function readReply(reader) {
  const head = await reader.take(4, 'a SOCKS reply')
  const atyp = head[3]
  let addr
  if (atyp === 3) {
    const [len] = await reader.take(1, 'a domain length')
    addr = await reader.take(len, 'a domain name')
  } else {
    addr = await reader.take(atyp === 4 ? 16 : 4, 'a bound address')
  }
  const port = await reader.take(2, 'a bound port')
  return {
    version: head[0],
    rep: head[1],
    atyp,
    address: atyp === 1 ? dotted(addr) : hex(addr),
    port: (port[0] << 8) | port[1]
  }
}

// connectRequest(address, port) is a CONNECT request to an IPv4 address.
function connectRequest(address, port) {
  return b4a.concat([b4a.from([SOCKS_VERSION, CMD_CONNECT, 0, ATYP_IPV4]), ipv4(address), port16(port)])
}

// associateRequest() is a UDP ASSOCIATE request with DST 0.0.0.0:0, which is what a client sends when it does not know
// its own UDP address.
function associateRequest() {
  return b4a.from([SOCKS_VERSION, CMD_UDP_ASSOCIATE, 0, ATYP_IPV4, 0, 0, 0, 0, 0, 0])
}

// udpAssociate(port, cleanup) sends UDP ASSOCIATE on a control connection and resolves with the reply.
async function udpAssociate(port, cleanup) {
  const { socket, reader } = await openSocks(port, cleanup)
  socket.write(associateRequest())
  return readReply(reader)
}

// datagram(address, port, payload) is a UDP request or reply with its RFC 1928 header.
function datagram(address, port, payload) {
  return b4a.concat([b4a.from([0, 0, 0, ATYP_IPV4]), ipv4(address), port16(port), payload])
}

// parseDatagram(msg) reads the header of a UDP datagram from the relay. It fails on what RFC 1928 does not allow here:
// nonzero RSV, FRAG other than 0, or an ATYP other than IPv4 (the tests use IPv4 only).
function parseDatagram(msg) {
  if (msg[0] !== 0 || msg[1] !== 0) throw new Error('the relay datagram has RSV bytes that are not 0')
  if (msg[2] !== 0) throw new Error(`the relay datagram has FRAG ${msg[2]}, not 0`)
  if (msg[3] !== ATYP_IPV4) throw new Error(`the relay datagram has ATYP ${msg[3]}, not IPv4`)
  return {
    address: dotted(msg.subarray(4, 8)),
    port: (msg[8] << 8) | msg[9],
    payload: b4a.from(msg.subarray(10))
  }
}

// udpClient(cleanup) is a UDP socket on 127.0.0.1: the client side of the relay. send(buf, port) sends to the relay on
// 127.0.0.1. next(what) resolves with the next { msg, from } the relay sends to it.
function udpClient(cleanup) {
  const sock = new UDX().createSocket()
  const inbox = []
  let wake = null
  sock.on('message', (msg, from) => {
    inbox.push({ msg: b4a.from(msg), from })
    const resume = wake
    wake = null
    if (resume) resume()
  })
  sock.bind(0, LOOPBACK)
  cleanup.push(() => sock.close())
  return {
    send(buf, port) {
      sock.send(buf, port, LOOPBACK).catch(noop)
    },
    async next(what) {
      if (inbox.length === 0) {
        await within(
          new Promise((resolve) => {
            wake = resolve
          }),
          what
        )
      }
      return inbox.shift()
    }
  }
}

// readBytes(readable, n, what) resolves with the next n bytes of a readable stream.
function readBytes(readable, n, what) {
  return within(
    new Promise((resolve, reject) => {
      const parts = []
      let size = 0
      readable.on('data', (chunk) => {
        parts.push(chunk)
        size += chunk.length
        if (size >= n) resolve(b4a.concat(parts).subarray(0, n))
      })
      readable.once('error', reject)
      readable.once('end', () => reject(new Error(`the stream ended before ${n} bytes arrived`)))
    }),
    what
  )
}

// hostPair() is the stream open resolves with, built as test/listeners.test.js builds it: a streamx Duplex. Bytes the
// front writes come out of toHost. The test writes to fromHost to send bytes to the front.
function hostPair() {
  const toHost = new PassThrough()
  const fromHost = new PassThrough()
  const stream = new Duplex({
    write(data, cb) {
      toHost.write(data)
      cb(null)
    }
  })
  fromHost.on('data', (chunk) => stream.push(chunk))
  fromHost.once('end', () => stream.push(null))
  return { stream, toHost, fromHost }
}

// fakeOpen() -> { open, calls, pairs }. open(host, service) records the call in calls and resolves with a hostPair.
// pairs holds each pair, in order.
function fakeOpen() {
  const calls = []
  const pairs = []
  const open = async (host, service) => {
    calls.push({ host, service })
    const pair = hostPair()
    pairs.push(pair)
    return pair.stream
  }
  return { open, calls, pairs }
}

// fakeUdp() -> { udp, calls, flows }. udp(host, service) records the call and returns a flow handle, an EventEmitter.
// flow.send(payload) keeps the payload in flow.sent. flow.close() sets flow.closed. The test emits a reply with
// flow.emit('message', payload).
function fakeUdp() {
  const calls = []
  const flows = []
  const udp = (host, service) => {
    calls.push({ host, service })
    const flow = new EventEmitter()
    flow.sent = []
    flow.closed = false
    flow.send = (payload) => {
      flow.sent.push(b4a.from(payload))
    }
    flow.close = () => {
      flow.closed = true
    }
    flows.push(flow)
    return flow
  }
  return { udp, calls, flows }
}

// fakeDns() -> { dns, queries }. dns.handle(queryBuf) records the query and resolves with DNS_REPLY, as VpnDns does.
function fakeDns() {
  const queries = []
  const dns = {
    async handle(queryBuf) {
      queries.push(b4a.from(queryBuf))
      return DNS_REPLY
    }
  }
  return { dns, queries }
}

// fakeLog() -> { entries, warn }. warn(msg, fields) appends { msg, fields } to entries, as lib/log.js's warn is called.
function fakeLog() {
  const entries = []
  return { entries, warn: (msg, fields) => entries.push({ msg, fields }) }
}

// newFront({ open, udp, dns }) builds the front on the fakes. The stub throws here, so each test fails at this line.
function newFront({ open, udp, dns }) {
  return new VpnFront({ addresses: ADDRESSES, open, udp, dns })
}

test('SOCKS5 CONNECT to 198.18.0.2:80 calls open(host, web), and bytes flow both ways', catchThrows(async (t) => {
  const { open, calls, pairs } = fakeOpen()
  const cleanup = []
  try {
    const front = newFront({ open, udp: fakeUdp().udp, dns: fakeDns().dns })
    cleanup.push(() => front.close())
    const port = await within(front.listen(), 'listen')
    const { socket, reader } = await openSocks(port, cleanup)
    socket.write(connectRequest('198.18.0.2', 80))
    const reply = await readReply(reader)
    t.is(reply.rep, REP_SUCCEEDED, 'CONNECT to a service address succeeds')
    t.is(reply.atyp, ATYP_IPV4, 'BND.ADDR is an IPv4 address: the network stack rejects any other type')
    t.alike(calls, [{ host: 'living-room', service: 'web' }], 'open is called once, with the host and service of 198.18.0.2')
    await until(() => pairs.length === 1, 'open resolves with a stream')

    socket.write(b4a.from('ping'))
    const received = await readBytes(pairs[0].toHost, 4, 'the bytes the service stream receives')
    t.is(b4a.toString(received), 'ping', 'the bytes the client sends reach the service stream')

    pairs[0].fromHost.write(b4a.from('pong'))
    const back = await reader.take(4, 'the bytes the client receives')
    t.is(b4a.toString(back), 'pong', 'the bytes the service stream emits reach the client')
  } finally {
    await runCleanup(cleanup)
  }
}))

test('CONNECT to 198.18.0.99 (unassigned) is refused with connection not allowed, and open is not called', catchThrows(async (t) => {
  const { open, calls } = fakeOpen()
  const cleanup = []
  try {
    const front = newFront({ open, udp: fakeUdp().udp, dns: fakeDns().dns })
    cleanup.push(() => front.close())
    const port = await within(front.listen(), 'listen')
    const { socket, reader } = await openSocks(port, cleanup)
    socket.write(connectRequest(UNASSIGNED, 80))
    const reply = await readReply(reader)
    t.is(reply.rep, REP_NOT_ALLOWED, 'the reply is connection not allowed by ruleset (REP 0x02)')
    await within(reader.closed, 'the front closes the connection after a refusal')
    t.is(calls.length, 0, 'open is not called for an unassigned address')
  } finally {
    await runCleanup(cleanup)
  }
}))

test('CONNECT to 8.8.8.8:443 is refused with connection not allowed, and open is not called', catchThrows(async (t) => {
  const { open, calls } = fakeOpen()
  const cleanup = []
  try {
    const front = newFront({ open, udp: fakeUdp().udp, dns: fakeDns().dns })
    cleanup.push(() => front.close())
    const port = await within(front.listen(), 'listen')
    const { socket, reader } = await openSocks(port, cleanup)
    socket.write(connectRequest(OUTSIDE, 443))
    const reply = await readReply(reader)
    t.is(reply.rep, REP_NOT_ALLOWED, 'the reply is connection not allowed by ruleset (REP 0x02)')
    await within(reader.closed, 'the front closes the connection after a refusal')
    t.is(calls.length, 0, 'open is not called for an address outside the VPN range')
  } finally {
    await runCleanup(cleanup)
  }
}))

test('UDP ASSOCIATE, then a datagram to 198.18.0.1:53, is answered by VpnDns', catchThrows(async (t) => {
  const { dns, queries } = fakeDns()
  const cleanup = []
  try {
    const front = newFront({ open: fakeOpen().open, udp: fakeUdp().udp, dns })
    cleanup.push(() => front.close())
    const port = await within(front.listen(), 'listen')
    const reply = await udpAssociate(port, cleanup)
    t.is(reply.rep, REP_SUCCEEDED, 'UDP ASSOCIATE succeeds')
    t.is(reply.address, LOOPBACK, 'BND.ADDR is 127.0.0.1')
    const client = udpClient(cleanup)
    client.send(datagram(DNS_ADDRESS, 53, DNS_QUERY), reply.port)
    const { msg, from } = await client.next('the DNS reply from the relay')
    t.alike(
      { host: from.host, port: from.port },
      { host: LOOPBACK, port: reply.port },
      'the reply is sent from BND.ADDR:BND.PORT, the address the network stack connects its UDP socket to'
    )
    const answer = parseDatagram(msg)
    t.alike(queries.map((q) => hex(q)), [hex(DNS_QUERY)], 'VpnDns is handed the query bytes once')
    t.is(answer.address, DNS_ADDRESS, 'the reply comes from 198.18.0.1')
    t.is(answer.port, 53, 'the reply comes from port 53')
    t.is(hex(answer.payload), hex(DNS_REPLY), 'the reply payload is the bytes VpnDns returned')
  } finally {
    await runCleanup(cleanup)
  }
}))

test('UDP to a udp service address goes through its flow, and the reply comes back to the client', catchThrows(async (t) => {
  const { udp, calls, flows } = fakeUdp()
  const cleanup = []
  try {
    const front = newFront({ open: fakeOpen().open, udp, dns: fakeDns().dns })
    cleanup.push(() => front.close())
    const port = await within(front.listen(), 'listen')
    const reply = await udpAssociate(port, cleanup)
    t.is(reply.rep, REP_SUCCEEDED, 'UDP ASSOCIATE succeeds')
    const client = udpClient(cleanup)
    const out = b4a.from('game packet out')
    client.send(datagram('198.18.0.3', 5000, out), reply.port)
    await until(() => flows.length === 1 && flows[0].sent.length === 1, 'the service flow gets the datagram')
    t.alike(calls, [{ host: 'living-room', service: 'game' }], 'udp is called with the host and service of 198.18.0.3')
    t.is(hex(flows[0].sent[0]), hex(out), 'the service flow is sent the payload')
    flows[0].emit('message', b4a.from('game packet back'))
    const { msg, from } = await client.next('the reply from the service flow')
    t.alike(
      { host: from.host, port: from.port },
      { host: LOOPBACK, port: reply.port },
      'the reply is sent from BND.ADDR:BND.PORT, the address the network stack connects its UDP socket to'
    )
    const answer = parseDatagram(msg)
    t.is(answer.address, '198.18.0.3', 'the reply comes from the service address')
    t.is(answer.port, 5000, 'the reply comes from the port the client sent to')
    t.is(b4a.toString(answer.payload), 'game packet back', 'the reply payload is the service reply')
  } finally {
    await runCleanup(cleanup)
  }
}))

test('a SOCKS5 greeting that asks for username/password only is refused with 0xFF', catchThrows(async (t) => {
  const cleanup = []
  try {
    const front = newFront({ open: fakeOpen().open, udp: fakeUdp().udp, dns: fakeDns().dns })
    cleanup.push(() => front.close())
    const port = await within(front.listen(), 'listen')
    const { socket, reader } = await connectTo(port, cleanup)
    socket.write(b4a.from([SOCKS_VERSION, 1, METHOD_USERPASS]))
    const choice = await reader.take(2, 'the method choice')
    t.is(hex(choice), '05ff', 'no acceptable method: the reply is 05 FF (RFC 1928, method 0xFF)')
  } finally {
    await runCleanup(cleanup)
  }
}))

test('the UDP flows of an association are closed when its control connection closes', catchThrows(async (t) => {
  const { udp, flows } = fakeUdp()
  const cleanup = []
  try {
    const front = newFront({ open: fakeOpen().open, udp, dns: fakeDns().dns })
    cleanup.push(() => front.close())
    const port = await within(front.listen(), 'listen')
    const control = await openSocks(port, cleanup)
    control.socket.write(associateRequest())
    const reply = await readReply(control.reader)
    t.is(reply.rep, REP_SUCCEEDED, 'UDP ASSOCIATE succeeds')
    const client = udpClient(cleanup)
    client.send(datagram('198.18.0.3', 5000, b4a.from('game packet out')), reply.port)
    await until(() => flows.length === 1 && flows[0].sent.length === 1, 'the service flow gets the datagram')
    t.is(flows[0].closed, false, 'the flow stays open while the control connection is open')
    control.socket.destroy()
    await until(() => flows[0].closed, 'the flow is closed after the control connection closes')
    t.is(flows[0].closed, true, 'the flow is closed: RFC 1928 ends a UDP association with its TCP connection')
  } finally {
    await runCleanup(cleanup)
  }
}))

test('bytes sent behind a CONNECT request reach the stream once it is open', catchThrows(async (t) => {
  const { open, pairs } = fakeOpen()
  const cleanup = []
  try {
    const front = newFront({ open, udp: fakeUdp().udp, dns: fakeDns().dns })
    cleanup.push(() => front.close())
    const port = await within(front.listen(), 'listen')
    const { socket, reader } = await openSocks(port, cleanup)
    socket.write(b4a.concat([connectRequest('198.18.0.2', 80), b4a.from('ping')]))
    const reply = await readReply(reader)
    t.is(reply.rep, REP_SUCCEEDED, 'CONNECT succeeds')
    await until(() => pairs.length === 1, 'open resolves with a stream')
    const received = await readBytes(pairs[0].toHost, 4, 'the bytes behind the request')
    t.is(b4a.toString(received), 'ping', 'the bytes sent with the request reach the service stream')
  } finally {
    await runCleanup(cleanup)
  }
}))

test('a UDP datagram from a second local source is dropped', catchThrows(async (t) => {
  const { udp, flows } = fakeUdp()
  const cleanup = []
  try {
    const front = newFront({ open: fakeOpen().open, udp, dns: fakeDns().dns })
    cleanup.push(() => front.close())
    const port = await within(front.listen(), 'listen')
    const reply = await udpAssociate(port, cleanup)
    const client = udpClient(cleanup)
    const intruder = udpClient(cleanup)
    client.send(datagram('198.18.0.3', 5000, b4a.from('first')), reply.port)
    await until(() => flows.length === 1 && flows[0].sent.length === 1, 'the first datagram reaches its flow')
    intruder.send(datagram('198.18.0.3', 5000, b4a.from('intruder')), reply.port)
    await new Promise((resolve) => setTimeout(resolve, 100))
    client.send(datagram('198.18.0.3', 5000, b4a.from('second')), reply.port)
    await until(() => flows[0].sent.length === 2, 'the second datagram from the client reaches its flow')
    t.alike(
      flows[0].sent.map((p) => b4a.toString(p)),
      ['first', 'second'],
      'a datagram from another local source never reaches the flow'
    )
  } finally {
    await runCleanup(cleanup)
  }
}))

test('a UDP destination past maxFlows is dropped, and the flows already open keep working', catchThrows(async (t) => {
  const { udp, calls, flows } = fakeUdp()
  const cleanup = []
  try {
    const front = new VpnFront({ addresses: ADDRESSES, open: fakeOpen().open, udp, dns: fakeDns().dns, maxFlows: 1 })
    cleanup.push(() => front.close())
    const port = await within(front.listen(), 'listen')
    const reply = await udpAssociate(port, cleanup)
    const client = udpClient(cleanup)
    client.send(datagram('198.18.0.3', 5000, b4a.from('first')), reply.port)
    await until(() => flows.length === 1 && flows[0].sent.length === 1, 'the first flow gets its datagram')
    client.send(datagram('198.18.0.3', 5001, b4a.from('new destination')), reply.port)
    client.send(datagram('198.18.0.3', 5000, b4a.from('second')), reply.port)
    await until(() => flows[0].sent.length === 2, 'the open flow gets its second datagram')
    t.is(calls.length, 1, 'no flow is opened for a new destination past the cap')
    t.alike(flows[0].sent.map((p) => b4a.toString(p)), ['first', 'second'], 'the open flow keeps working')
  } finally {
    await runCleanup(cleanup)
  }
}))

test('a control connection past maxControls is closed at once, and the others are not', catchThrows(async (t) => {
  const cleanup = []
  try {
    const front = new VpnFront({
      addresses: ADDRESSES,
      open: fakeOpen().open,
      udp: fakeUdp().udp,
      dns: fakeDns().dns,
      maxControls: 1
    })
    cleanup.push(() => front.close())
    const port = await within(front.listen(), 'listen')
    const first = await openSocks(port, cleanup)
    const extra = await connectTo(port, cleanup)
    extra.socket.write(b4a.from([SOCKS_VERSION, 1, METHOD_NONE]))
    await within(extra.reader.closed, 'the front closes the connection past the cap')
    first.socket.write(associateRequest())
    const reply = await readReply(first.reader)
    t.is(reply.rep, REP_SUCCEEDED, 'the connection within the cap still works')
  } finally {
    await runCleanup(cleanup)
  }
}))

test('a control connection that sends no request within handshakeMs is closed', catchThrows(async (t) => {
  const cleanup = []
  try {
    const front = new VpnFront({
      addresses: ADDRESSES,
      open: fakeOpen().open,
      udp: fakeUdp().udp,
      dns: fakeDns().dns,
      handshakeMs: 100
    })
    cleanup.push(() => front.close())
    const port = await within(front.listen(), 'listen')
    const { reader } = await connectTo(port, cleanup)
    await within(reader.closed, 'the front closes a connection that stays silent')
    t.pass('the silent connection is closed at the deadline')
  } finally {
    await runCleanup(cleanup)
  }
}))

test('a datagram over maxDatagram is dropped in both directions', catchThrows(async (t) => {
  const { udp, flows } = fakeUdp()
  const cleanup = []
  try {
    const front = new VpnFront({ addresses: ADDRESSES, open: fakeOpen().open, udp, dns: fakeDns().dns, maxDatagram: 8 })
    cleanup.push(() => front.close())
    const port = await within(front.listen(), 'listen')
    const reply = await udpAssociate(port, cleanup)
    const client = udpClient(cleanup)
    client.send(datagram('198.18.0.3', 5000, b4a.from('short')), reply.port)
    await until(() => flows.length === 1 && flows[0].sent.length === 1, 'the short datagram reaches its flow')
    client.send(datagram('198.18.0.3', 5000, b4a.from('0123456789')), reply.port)
    client.send(datagram('198.18.0.3', 5000, b4a.from('again')), reply.port)
    await until(() => flows[0].sent.length === 2, 'the next short datagram reaches its flow')
    t.alike(
      flows[0].sent.map((p) => b4a.toString(p)),
      ['short', 'again'],
      'a datagram over maxDatagram is not sent to the flow'
    )
    flows[0].emit('message', b4a.from('a reply over the limit'))
    flows[0].emit('message', b4a.from('ok'))
    const { msg } = await client.next('the reply from the relay')
    t.is(b4a.toString(parseDatagram(msg).payload), 'ok', 'a reply over maxDatagram is not sent to the client')
  } finally {
    await runCleanup(cleanup)
  }
}))

test('a DNS query past maxDns is dropped, and the slot is given back', catchThrows(async (t) => {
  const queries = []
  const pending = []
  // query(n) is DNS_QUERY with DNS id n, so each query can be told apart.
  const query = (n) => {
    const buf = b4a.from(DNS_QUERY)
    buf[1] = n
    return buf
  }
  const dns = {
    handle(queryBuf) {
      queries.push(b4a.from(queryBuf))
      return new Promise((resolve, reject) => {
        pending.push({ resolve, reject })
      })
    }
  }
  const cleanup = []
  try {
    const front = new VpnFront({ addresses: ADDRESSES, open: fakeOpen().open, udp: fakeUdp().udp, dns, maxDns: 2 })
    cleanup.push(() => front.close())
    const port = await within(front.listen(), 'listen')
    const reply = await udpAssociate(port, cleanup)
    const client = udpClient(cleanup)
    for (const n of [1, 2, 3, 4]) client.send(datagram(DNS_ADDRESS, 53, query(n)), reply.port)
    await until(() => queries.length === 2, 'the first two DNS queries reach VpnDns')
    await new Promise((resolve) => setTimeout(resolve, 100))
    t.is(queries.length, 2, 'the DNS queries past maxDns are dropped, not queued')

    pending[0].resolve(DNS_REPLY)
    const first = await client.next('the reply to the first query')
    t.is(hex(parseDatagram(first.msg).payload), hex(DNS_REPLY), 'the first query is answered once its handle resolves')

    client.send(datagram(DNS_ADDRESS, 53, query(5)), reply.port)
    await until(() => queries.length === 3, 'a DNS query sent after a slot frees reaches VpnDns')
    t.alike(
      queries.map((q) => q[1]),
      [1, 2, 5],
      'VpnDns gets queries 1, 2 and 5: the dropped ones never arrive'
    )

    pending[1].reject(new Error('upstream failed'))
    client.send(datagram(DNS_ADDRESS, 53, query(6)), reply.port)
    await until(() => queries.length === 4, 'a DNS query after a rejected handle reaches VpnDns')
    t.is(queries[3][1], 6, 'the slot of a rejected handle is given back')
  } finally {
    await runCleanup(cleanup)
  }
}))

test('a flow past maxFlowsTotal is dropped, and closing an association gives its flows back', catchThrows(async (t) => {
  const { udp, calls, flows } = fakeUdp()
  const cleanup = []
  try {
    const front = new VpnFront({
      addresses: ADDRESSES,
      open: fakeOpen().open,
      udp,
      dns: fakeDns().dns,
      maxFlowsTotal: 2
    })
    cleanup.push(() => front.close())
    const port = await within(front.listen(), 'listen')

    const a = await openSocks(port, cleanup)
    a.socket.write(associateRequest())
    const replyA = await readReply(a.reader)
    t.is(replyA.rep, REP_SUCCEEDED, 'association A: UDP ASSOCIATE succeeds')
    const clientA = udpClient(cleanup)
    clientA.send(datagram('198.18.0.3', 5000, b4a.from('a first')), replyA.port)
    clientA.send(datagram('198.18.0.3', 5001, b4a.from('a second')), replyA.port)
    await until(
      () => flows.length === 2 && flows[0].sent.length === 1 && flows[1].sent.length === 1,
      'association A opens its two flows'
    )
    t.is(calls.length, 2, 'the two flows of association A are open: the total is at maxFlowsTotal')

    const b = await openSocks(port, cleanup)
    b.socket.write(associateRequest())
    const replyB = await readReply(b.reader)
    t.is(replyB.rep, REP_SUCCEEDED, 'association B: UDP ASSOCIATE succeeds')
    const clientB = udpClient(cleanup)
    clientB.send(datagram('198.18.0.3', 5002, b4a.from('b first')), replyB.port)
    await new Promise((resolve) => setTimeout(resolve, 100))
    t.is(calls.length, 2, 'a flow for association B is dropped while the total is at maxFlowsTotal')

    a.socket.destroy()
    await until(() => flows[0].closed && flows[1].closed, 'association A closes its two flows')
    clientB.send(datagram('198.18.0.3', 5002, b4a.from('b second')), replyB.port)
    await until(() => calls.length === 3, 'association B opens a flow once association A has given its flows back')
    t.alike(
      flows[2].sent.map((p) => b4a.toString(p)),
      ['b second'],
      'the flow opened for B carries the datagram sent after the release'
    )
  } finally {
    await runCleanup(cleanup)
  }
}))

test('an oversize datagram is logged once per association, with the code only', catchThrows(async (t) => {
  const { udp, flows } = fakeUdp()
  const log = fakeLog()
  const cleanup = []
  try {
    const front = new VpnFront({ addresses: ADDRESSES, open: fakeOpen().open, udp, dns: fakeDns().dns, maxDatagram: 8, log })
    cleanup.push(() => front.close())
    const port = await within(front.listen(), 'listen')
    const reply = await udpAssociate(port, cleanup)
    const client = udpClient(cleanup)
    client.send(datagram('198.18.0.3', 5000, b4a.from('oversize-marker one')), reply.port)
    client.send(datagram('198.18.0.3', 5000, b4a.from('oversize-marker two')), reply.port)
    client.send(datagram('198.18.0.3', 5000, b4a.from('short')), reply.port)
    await until(() => flows.length === 1 && flows[0].sent.length === 1, 'the short datagram reaches its flow')
    await until(() => log.entries.length >= 1, 'the first oversize datagram from the client is logged')
    flows[0].emit('message', b4a.from('oversize-marker reply'))
    const sized = log.entries.filter((e) => e.fields.code === 'HB-UDP-TOO-LARGE')
    t.is(sized.length, 1, 'one HB-UDP-TOO-LARGE line for two oversize datagrams and an oversize reply')
    t.is(log.entries.length, 1, 'nothing else is logged')
    t.alike(sized[0].fields, { code: 'HB-UDP-TOO-LARGE' }, 'the fields are the code only')
    const text = JSON.stringify(log.entries)
    t.is(text.includes('oversize-marker'), false, 'the log holds no payload bytes')
    t.is(text.includes('198.18.0.3'), false, 'the log holds no address')
    t.is(text.includes('5000'), false, 'the log holds no port')
  } finally {
    await runCleanup(cleanup)
  }
}))

test('an oversize reply from a flow is logged once, with the code only', catchThrows(async (t) => {
  const { udp, flows } = fakeUdp()
  const log = fakeLog()
  const cleanup = []
  try {
    const front = new VpnFront({ addresses: ADDRESSES, open: fakeOpen().open, udp, dns: fakeDns().dns, maxDatagram: 8, log })
    cleanup.push(() => front.close())
    const port = await within(front.listen(), 'listen')
    const reply = await udpAssociate(port, cleanup)
    const client = udpClient(cleanup)
    client.send(datagram('198.18.0.3', 5000, b4a.from('short')), reply.port)
    await until(() => flows.length === 1 && flows[0].sent.length === 1, 'the short datagram reaches its flow')
    flows[0].emit('message', b4a.from('oversize-marker one'))
    flows[0].emit('message', b4a.from('oversize-marker two'))
    t.alike(log.entries, [{ msg: 'a datagram over the limit is dropped', fields: { code: 'HB-UDP-TOO-LARGE' } }], 'one line, with the code only')
    t.is(JSON.stringify(log.entries).includes('oversize-marker'), false, 'the log holds no payload bytes')
  } finally {
    await runCleanup(cleanup)
  }
}))

test('a UDP flow past maxFlows is logged once, and the log names no address', catchThrows(async (t) => {
  const { udp, flows } = fakeUdp()
  const log = fakeLog()
  const cleanup = []
  try {
    const front = new VpnFront({ addresses: ADDRESSES, open: fakeOpen().open, udp, dns: fakeDns().dns, maxFlows: 1, log })
    cleanup.push(() => front.close())
    const port = await within(front.listen(), 'listen')
    const reply = await udpAssociate(port, cleanup)
    const client = udpClient(cleanup)
    client.send(datagram('198.18.0.3', 5000, b4a.from('first')), reply.port)
    await until(() => flows.length === 1 && flows[0].sent.length === 1, 'the first flow gets its datagram')
    client.send(datagram('198.18.0.3', 5001, b4a.from('new one')), reply.port)
    client.send(datagram('198.18.0.3', 5002, b4a.from('newer one')), reply.port)
    client.send(datagram('198.18.0.3', 5000, b4a.from('second')), reply.port)
    await until(() => flows[0].sent.length === 2, 'the open flow gets its second datagram')
    t.alike(log.entries, [{ msg: 'too many UDP flows, a datagram is dropped', fields: { code: 'HB-LIMIT-REACHED' } }], 'one line for the two drops, with the code only')
    const text = JSON.stringify(log.entries)
    t.is(text.includes('198.18.0.3'), false, 'the log holds no address')
    t.is(text.includes('5001') || text.includes('5002'), false, 'the log holds no port')
  } finally {
    await runCleanup(cleanup)
  }
}))

test('a UDP flow past maxFlowsTotal is logged once per association', catchThrows(async (t) => {
  const { udp, calls, flows } = fakeUdp()
  const log = fakeLog()
  const cleanup = []
  try {
    const front = new VpnFront({ addresses: ADDRESSES, open: fakeOpen().open, udp, dns: fakeDns().dns, maxFlowsTotal: 1, log })
    cleanup.push(() => front.close())
    const port = await within(front.listen(), 'listen')
    const a = await udpAssociate(port, cleanup)
    const clientA = udpClient(cleanup)
    clientA.send(datagram('198.18.0.3', 5000, b4a.from('a first')), a.port)
    await until(() => flows.length === 1 && flows[0].sent.length === 1, 'association A opens the one flow the total allows')
    const b = await udpAssociate(port, cleanup)
    const clientB = udpClient(cleanup)
    clientB.send(datagram('198.18.0.3', 5001, b4a.from('b first')), b.port)
    clientB.send(datagram('198.18.0.3', 5002, b4a.from('b second')), b.port)
    await new Promise((resolve) => setTimeout(resolve, 100))
    t.is(calls.length, 1, 'no flow is opened past maxFlowsTotal')
    t.alike(log.entries, [{ msg: 'too many UDP flows, a datagram is dropped', fields: { code: 'HB-LIMIT-REACHED' } }], 'association B logs its drop once, with the code only')
  } finally {
    await runCleanup(cleanup)
  }
}))

test('a control connection past maxControls is logged, with the code only', catchThrows(async (t) => {
  const log = fakeLog()
  const cleanup = []
  try {
    const front = new VpnFront({ addresses: ADDRESSES, open: fakeOpen().open, udp: fakeUdp().udp, dns: fakeDns().dns, maxControls: 1, log })
    cleanup.push(() => front.close())
    const port = await within(front.listen(), 'listen')
    await openSocks(port, cleanup)
    const extra = await connectTo(port, cleanup)
    extra.socket.write(b4a.from([SOCKS_VERSION, 1, METHOD_NONE]))
    await within(extra.reader.closed, 'the front closes the connection past the cap')
    t.alike(log.entries, [{ msg: 'too many VPN front connections, one is closed', fields: { code: 'HB-LIMIT-REACHED' } }], 'one line, with the code only')
  } finally {
    await runCleanup(cleanup)
  }
}))

test('a control connection past maxControls is logged again once a control connection has closed', catchThrows(async (t) => {
  const log = fakeLog()
  const cleanup = []
  try {
    const front = new VpnFront({
      addresses: ADDRESSES,
      open: fakeOpen().open,
      udp: fakeUdp().udp,
      dns: fakeDns().dns,
      maxControls: 1,
      logRearmMs: 50,
      log
    })
    cleanup.push(() => front.close())
    const port = await within(front.listen(), 'listen')
    const first = await openSocks(port, cleanup)
    for (let i = 0; i < 2; i++) {
      const refused = await connectTo(port, cleanup)
      refused.socket.write(b4a.from([SOCKS_VERSION, 1, METHOD_NONE]))
      await within(refused.reader.closed, 'the front closes a connection past the cap')
    }
    t.is(log.entries.length, 1, 'refusals before any close are logged once')
    first.socket.destroy()
    await new Promise((resolve) => setTimeout(resolve, 100))
    await openSocks(port, cleanup)
    const again = await connectTo(port, cleanup)
    again.socket.write(b4a.from([SOCKS_VERSION, 1, METHOD_NONE]))
    await within(again.reader.closed, 'the front closes the connection past the cap again')
    t.is(log.entries.length, 2, 'a refusal after a control connection has closed is logged again')
    t.alike(log.entries[1].fields, { code: 'HB-LIMIT-REACHED' }, 'the fields are the code only')
  } finally {
    await runCleanup(cleanup)
  }
}))

test('a DNS query past maxDns is logged once per association, with the code only', catchThrows(async (t) => {
  const queries = []
  const dns = {
    handle(queryBuf) {
      queries.push(b4a.from(queryBuf))
      return new Promise(() => {})
    }
  }
  const log = fakeLog()
  const cleanup = []
  try {
    const front = new VpnFront({ addresses: ADDRESSES, open: fakeOpen().open, udp: fakeUdp().udp, dns, maxDns: 1, log })
    cleanup.push(() => front.close())
    const port = await within(front.listen(), 'listen')
    const reply = await udpAssociate(port, cleanup)
    const client = udpClient(cleanup)
    client.send(datagram(DNS_ADDRESS, 53, DNS_QUERY), reply.port)
    await until(() => queries.length === 1, 'the first DNS query reaches VpnDns')
    client.send(datagram(DNS_ADDRESS, 53, DNS_QUERY), reply.port)
    client.send(datagram(DNS_ADDRESS, 53, DNS_QUERY), reply.port)
    await new Promise((resolve) => setTimeout(resolve, 100))
    t.is(queries.length, 1, 'the queries past maxDns are dropped')
    t.alike(log.entries, [{ msg: 'too many DNS queries, a query is dropped', fields: { code: 'HB-LIMIT-REACHED' } }], 'one line, with the code only')
    t.is(JSON.stringify(log.entries).includes('198.18.0.1'), false, 'the log holds no address')
  } finally {
    await runCleanup(cleanup)
  }
}))

test('a DNS query over maxDatagram is answered and not logged as too large: DNS is held to 2048 bytes', catchThrows(async (t) => {
  const { dns, queries } = fakeDns()
  const log = fakeLog()
  const cleanup = []
  try {
    const front = new VpnFront({ addresses: ADDRESSES, open: fakeOpen().open, udp: fakeUdp().udp, dns, maxDatagram: 8, log })
    cleanup.push(() => front.close())
    const port = await within(front.listen(), 'listen')
    const reply = await udpAssociate(port, cleanup)
    const client = udpClient(cleanup)
    client.send(datagram(DNS_ADDRESS, 53, b4a.concat([DNS_QUERY, b4a.alloc(990)])), reply.port)
    const answered = await client.next('the DNS reply')
    t.is(hex(parseDatagram(answered.msg).payload), hex(DNS_REPLY), 'the query over maxDatagram is answered')
    t.is(queries.length, 1, 'VpnDns is handed the query')
    t.is(log.entries.length, 0, 'nothing is logged')
  } finally {
    await runCleanup(cleanup)
  }
}))

test('a client that churns connections at maxControls writes one log line, not one per close', catchThrows(async (t) => {
  const log = fakeLog()
  const cleanup = []
  try {
    const front = new VpnFront({ addresses: ADDRESSES, open: fakeOpen().open, udp: fakeUdp().udp, dns: fakeDns().dns, maxControls: 2, log })
    cleanup.push(() => front.close())
    const port = await within(front.listen(), 'listen')
    await connectTo(port, cleanup) // connection A holds one of the two slots
    for (let i = 0; i < 10; i++) {
      const b = await connectTo(port, cleanup) // accepted: the cap is now full
      const c = await connectTo(port, cleanup) // refused
      await within(c.reader.closed, 'the front closes the connection past the cap')
      b.socket.destroy() // a close re-arms the line, but only after logRearmMs has passed
      await new Promise((resolve) => setTimeout(resolve, 30))
    }
    t.is(log.entries.length, 1, 'ten closes at the cap write one maxControls line, not one per close')
    t.alike(log.entries[0].fields, { code: 'HB-LIMIT-REACHED' }, 'the fields are the code only')
  } finally {
    await runCleanup(cleanup)
  }
}))

test('a client that churns associations with oversize datagrams writes one HB-UDP-TOO-LARGE line', catchThrows(async (t) => {
  const { udp, flows } = fakeUdp()
  const log = fakeLog()
  const cleanup = []
  try {
    const front = new VpnFront({ addresses: ADDRESSES, open: fakeOpen().open, udp, dns: fakeDns().dns, maxDatagram: 8, log })
    cleanup.push(() => front.close())
    const port = await within(front.listen(), 'listen')
    for (let i = 0; i < 10; i++) {
      const control = await openSocks(port, cleanup)
      control.socket.write(associateRequest())
      const reply = await readReply(control.reader)
      const client = udpClient(cleanup)
      client.send(datagram('198.18.0.3', 5000, b4a.from('oversize-marker')), reply.port)
      client.send(datagram('198.18.0.3', 5000, b4a.from('short')), reply.port)
      await until(() => flows.length === i + 1 && flows[i].sent.length === 1, 'the short datagram reaches its flow')
      control.socket.destroy()
      await until(() => flows[i].closed, 'the association closes its flow')
    }
    const sized = log.entries.filter((e) => e.fields.code === 'HB-UDP-TOO-LARGE')
    t.is(sized.length, 1, 'ten associations that each drop an oversize datagram write one HB-UDP-TOO-LARGE line')
    t.is(log.entries.length, 1, 'nothing else is logged')
  } finally {
    await runCleanup(cleanup)
  }
}))

test('a DNS query of 2038 bytes, a 2048-byte datagram with its SOCKS header, is answered whole', catchThrows(async (t) => {
  const { dns, queries } = fakeDns()
  const log = fakeLog()
  const cleanup = []
  try {
    const front = new VpnFront({ addresses: ADDRESSES, open: fakeOpen().open, udp: fakeUdp().udp, dns, log })
    cleanup.push(() => front.close())
    const port = await within(front.listen(), 'listen')
    const reply = await udpAssociate(port, cleanup)
    const client = udpClient(cleanup)
    // udx-native delivers at most 2048 bytes per datagram, the 10-byte SOCKS header included: 2038 bytes of query is
    // the largest query that arrives whole. A query of 2048 bytes would arrive cut to 2038.
    const query = b4a.concat([DNS_QUERY, b4a.alloc(2038 - DNS_QUERY.length)])
    client.send(datagram(DNS_ADDRESS, 53, query), reply.port)
    const answered = await client.next('the DNS reply')
    t.is(hex(parseDatagram(answered.msg).payload), hex(DNS_REPLY), 'the 2038-byte query is answered')
    t.is(queries.length, 1, 'VpnDns is handed the query once')
    t.is(hex(queries[0]), hex(query), 'VpnDns gets the whole query, not a truncated one')
    t.is(log.entries.length, 0, 'nothing is logged: DNS is not held to maxDatagram')
  } finally {
    await runCleanup(cleanup)
  }
}))

test('a DNS reply over 1490 bytes is dropped, and one of 1490 bytes is delivered whole', catchThrows(async (t) => {
  // The fake VpnDns answers by the query's DNS id (byte 1): id 1 gets a 1490-byte reply, id 2 a 1491-byte one, id 3
  // DNS_REPLY.
  const replies = {
    1: b4a.alloc(1490, 7),
    2: b4a.alloc(1491, 7),
    3: DNS_REPLY
  }
  const dns = {
    async handle(queryBuf) {
      return replies[queryBuf[1]]
    }
  }
  const log = fakeLog()
  const cleanup = []
  try {
    const front = new VpnFront({ addresses: ADDRESSES, open: fakeOpen().open, udp: fakeUdp().udp, dns, log })
    cleanup.push(() => front.close())
    const port = await within(front.listen(), 'listen')
    const reply = await udpAssociate(port, cleanup)
    const client = udpClient(cleanup)
    // query(n) is DNS_QUERY with DNS id n, so each query can be told apart.
    const query = (n) => {
      const buf = b4a.from(DNS_QUERY)
      buf[1] = n
      return buf
    }
    client.send(datagram(DNS_ADDRESS, 53, query(1)), reply.port)
    const first = await client.next('the reply to query 1')
    t.is(hex(parseDatagram(first.msg).payload), hex(replies[1]), 'the 1490-byte reply is delivered whole')

    client.send(datagram(DNS_ADDRESS, 53, query(2)), reply.port)
    await new Promise((resolve) => setTimeout(resolve, 100))
    client.send(datagram(DNS_ADDRESS, 53, query(3)), reply.port)
    const next = await client.next('the reply to query 3')
    t.is(
      hex(parseDatagram(next.msg).payload),
      hex(DNS_REPLY),
      'the next reply is DNS_REPLY: the 1491-byte reply was dropped, not sent'
    )
    t.is(log.entries.length, 0, 'nothing is logged')
  } finally {
    await runCleanup(cleanup)
  }
}))
