// Tests for VPN mode DNS (lib/vpn-dns.js): the names and addresses of services, the answers the app gives
// for them, and the forwarding of every other query. Rules: docs/architecture.md ("VPN mode (Android and
// iOS)") and decision D37 (reach one host's services by name; no subnet routing). Wire format: RFC 1035,
// section 4.1. Queries and replies are built and read by hand here, so the code under test is not used to
// check itself. Nothing in these tests logs a name or a query, and the forwarding uses 127.0.0.1 only.
const test = require('brittle')
const b4a = require('b4a')
const UDX = require('udx-native')
const { allocate, VpnDns } = require('../lib/vpn-dns.js')

const LOOPBACK = '127.0.0.1'
const TYPE_A = 1
const TYPE_AAAA = 28
// SETTLE bounds every wait, so a stuck lookup fails its test instead of hanging it.
const SETTLE = 10000

// brittle ends the whole run when a test body throws. Until the IMPL task lands, the code throws
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

function noop() {}

// within waits for promise, and fails with what if it has not settled within SETTLE.
function within(promise, what) {
  let timer
  const timeout = new Promise((resolve, reject) => {
    timer = setTimeout(() => reject(new Error(`${what} did not finish within ${SETTLE} ms`)), SETTLE)
  })
  return Promise.race([promise, timeout]).finally(() => clearTimeout(timer))
}

// u16 and u32 give the big-endian bytes of a number.
function u16(n) {
  return b4a.from([(n >> 8) & 0xff, n & 0xff])
}

function u32(n) {
  return b4a.from([(n >>> 24) & 0xff, (n >>> 16) & 0xff, (n >>> 8) & 0xff, n & 0xff])
}

// encodeName writes a dotted name as labels, ending with the zero-length root label.
function encodeName(name) {
  const labels = name.split('.').map((label) => {
    const bytes = b4a.from(label)
    return b4a.concat([b4a.from([bytes.length]), bytes])
  })
  return b4a.concat([...labels, b4a.from([0])])
}

// buildQuery(id, name, type) is a standard query with recursion desired and one question in class IN.
function buildQuery(id, name, type) {
  return b4a.concat([u16(id), u16(0x0100), u16(1), u16(0), u16(0), u16(0), encodeName(name), u16(type), u16(1)])
}

// readName reads the name at offset, following compression pointers. It returns the dotted name and the
// offset just after the name where it starts in the message. The step limit stops a pointer loop.
function readName(buf, offset) {
  const labels = []
  let pos = offset
  let end = -1
  for (let steps = 0; steps < 256; steps++) {
    const len = buf[pos]
    if (len === undefined) throw new Error('name runs past the end of the message')
    if ((len & 0xc0) === 0xc0) {
      if (end < 0) end = pos + 2
      pos = ((len & 0x3f) << 8) | buf[pos + 1]
    } else if (len === 0) {
      if (end < 0) end = pos + 1
      return { name: labels.join('.'), end }
    } else {
      labels.push(b4a.toString(buf.subarray(pos + 1, pos + 1 + len), 'utf8'))
      pos += 1 + len
    }
  }
  throw new Error('too many labels or a pointer loop')
}

// parseReply reads a reply: the header, the question, and each answer's name, type, class, TTL and rdata.
function parseReply(buf) {
  const u16At = (i) => {
    if (i + 2 > buf.length) throw new Error(`reply is ${buf.length} bytes, too short at ${i}`)
    return (buf[i] << 8) | buf[i + 1]
  }
  const flags = u16At(2)
  const question = readName(buf, 12)
  let pos = question.end + 4
  const answers = []
  for (let i = 0; i < u16At(6); i++) {
    const owner = readName(buf, pos)
    const at = owner.end
    const rdlength = u16At(at + 8)
    answers.push({
      name: owner.name,
      type: u16At(at),
      class: u16At(at + 2),
      ttl: u16At(at + 4) * 65536 + u16At(at + 6),
      rdata: buf.subarray(at + 10, at + 10 + rdlength)
    })
    pos = at + 10 + rdlength
  }
  return {
    id: u16At(0),
    qr: flags >> 15,
    rcode: flags & 0x0f,
    qdcount: u16At(4),
    question: { name: question.name, type: u16At(question.end), class: u16At(question.end + 2) },
    questionBytes: buf.subarray(12, question.end + 4),
    answers
  }
}

// answerFor(query) is what the fake upstream replies: the query's id and question, with QR, RD and RA set and
// one A answer, 192.0.2.44 (a documentation address), with TTL 300.
function answerFor(query) {
  return b4a.concat([
    query.subarray(0, 2),
    u16(0x8180),
    u16(1),
    u16(1),
    u16(0),
    u16(0),
    query.subarray(12),
    u16(0xc00c),
    u16(TYPE_A),
    u16(1),
    u32(300),
    u16(4),
    b4a.from([192, 0, 2, 44])
  ])
}

function ipv4(bytes) {
  return [...bytes].join('.')
}

// fakeUpstream(reply) starts a UDP server on 127.0.0.1 that records each datagram and answers with
// reply(query), or with nothing when reply returns null. port is its port; close() stops it.
function fakeUpstream(reply) {
  const socket = new UDX().createSocket()
  const received = []
  socket.on('message', (msg, from) => {
    const query = b4a.from(msg)
    received.push(query)
    const answer = reply(query)
    if (answer) socket.send(answer, from.port, from.host).catch(noop)
  })
  socket.bind(0, LOOPBACK)
  return { port: socket.address().port, received, close: () => socket.close() }
}

// forwardingFactory(port) is the socketFactory for these tests. The engine sends upstream queries to port 53,
// which a test cannot bind without root, so the sockets it makes send to port instead. The factory keeps the
// sockets it made, so a test can close them, and counts them with made.length.
function forwardingFactory(port) {
  const udx = new UDX()
  const made = []
  const factory = () => {
    const socket = udx.createSocket()
    const send = socket.send.bind(socket)
    socket.send = (buffer, _port, host, ttl) => send(buffer, port, host, ttl)
    made.push(socket)
    return socket
  }
  factory.made = made
  factory.close = () => {
    for (const socket of made) socket.close().catch(noop)
  }
  return factory
}

// noForward is the socketFactory for names the engine must answer itself: a forward is a failure.
function noForward() {
  throw new Error('a name the engine knows must not be forwarded')
}

test('allocate gives 198.18.0.2 and .3 to two services and keeps previous addresses on re-allocation', catchThrows(async (t) => {
  const first = allocate(
    [
      { host: 'living-room', service: 'jellyfin', origins: [] },
      { host: 'living-room', service: 'ssh', origins: [] }
    ],
    {}
  )
  t.is(first['jellyfin.living-room.internal'], '198.18.0.2', 'the first service gets 198.18.0.2')
  t.is(first['ssh.living-room.internal'], '198.18.0.3', 'the second service gets 198.18.0.3')

  // previous is keyed by 'service.host', as the design gives it. The list order changes and a service is added.
  const previous = { 'jellyfin.living-room': '198.18.0.2', 'ssh.living-room': '198.18.0.3' }
  const later = allocate(
    [
      { host: 'living-room', service: 'web', origins: [] },
      { host: 'living-room', service: 'ssh', origins: [] },
      { host: 'living-room', service: 'jellyfin', origins: [] }
    ],
    previous
  )
  t.is(later['jellyfin.living-room.internal'], '198.18.0.2', 'jellyfin keeps 198.18.0.2')
  t.is(later['ssh.living-room.internal'], '198.18.0.3', 'ssh keeps 198.18.0.3')
  t.is(later['web.living-room.internal'], '198.18.0.4', 'web gets the next free address')
}))

test('an origin https://jellyfin.example maps jellyfin.example to that service address', catchThrows(async (t) => {
  const names = allocate([{ host: 'living-room', service: 'jellyfin', origins: ['https://jellyfin.example'] }], {})
  t.is(names['jellyfin.example'], '198.18.0.2', 'the origin hostname is a name')
  t.is(names['jellyfin.example'], names['jellyfin.living-room.internal'], 'it has the service address')
}))

test('a query for WEB.Living-Room.internal (A) returns the address with TTL 60 and the query id', catchThrows(async (t) => {
  const names = allocate([{ host: 'living-room', service: 'web', origins: [] }], {})
  const dns = new VpnDns({ names, upstream: [LOOPBACK], socketFactory: noForward })
  const query = buildQuery(0xbeef, 'WEB.Living-Room.internal', TYPE_A)
  const reply = parseReply(await within(dns.handle(query), 'handle'))
  t.is(reply.id, 0xbeef, 'the reply has the query id')
  t.is(reply.qr, 1, 'it is a reply')
  t.is(reply.rcode, 0, 'NOERROR')
  t.ok(b4a.equals(reply.questionBytes, query.subarray(12)), 'the question is echoed as sent')
  t.is(reply.answers.length, 1, 'one answer')
  const [answer] = reply.answers
  t.is(answer.name.toLowerCase(), 'web.living-room.internal', 'the answer is for the query name')
  t.is(answer.type, TYPE_A, 'type A')
  t.is(answer.class, 1, 'class IN')
  t.is(answer.ttl, 60, 'TTL 60')
  t.is(ipv4(answer.rdata), '198.18.0.2', 'the service address')
}))

test('AAAA for a known name returns NOERROR with zero answers', catchThrows(async (t) => {
  const names = allocate([{ host: 'living-room', service: 'web', origins: [] }], {})
  const dns = new VpnDns({ names, upstream: [LOOPBACK], socketFactory: noForward })
  const reply = parseReply(await within(dns.handle(buildQuery(0x2a2a, 'web.living-room.internal', TYPE_AAAA)), 'handle'))
  t.is(reply.id, 0x2a2a, 'the reply has the query id')
  t.is(reply.qr, 1, 'it is a reply')
  t.is(reply.rcode, 0, 'NOERROR')
  t.is(reply.answers.length, 0, 'no answer')
}))

test('a query for example.com is forwarded to a fake upstream on 127.0.0.1 and its reply returned byte-for-byte', catchThrows(async (t) => {
  const upstream = fakeUpstream((query) => answerFor(query))
  const factory = forwardingFactory(upstream.port)
  try {
    const names = allocate([{ host: 'living-room', service: 'web', origins: [] }], {})
    const dns = new VpnDns({ names, upstream: [LOOPBACK], socketFactory: factory })
    const query = buildQuery(0x4242, 'example.com', TYPE_A)
    const reply = await within(dns.handle(query), 'handle')
    t.is(upstream.received.length, 1, 'the upstream got one query')
    t.ok(b4a.equals(upstream.received[0], query), 'the query was forwarded unchanged')
    t.ok(b4a.equals(reply, answerFor(query)), 'the upstream reply is returned byte-for-byte')
  } finally {
    upstream.close()
    factory.close()
  }
}))

test('no upstream answer within the timeout returns SERVFAIL', catchThrows(async (t) => {
  const upstream = fakeUpstream(() => null)
  const factory = forwardingFactory(upstream.port)
  try {
    const dns = new VpnDns({ names: {}, upstream: [LOOPBACK], socketFactory: factory, timeoutMs: 200 })
    const query = buildQuery(0x5151, 'example.com', TYPE_A)
    const reply = parseReply(await within(dns.handle(query), 'handle'))
    t.is(upstream.received.length, 1, 'the query was forwarded and went unanswered')
    t.is(reply.id, 0x5151, 'the reply has the query id')
    t.is(reply.qr, 1, 'it is a reply')
    t.is(reply.rcode, 2, 'SERVFAIL')
  } finally {
    upstream.close()
    factory.close()
  }
}))

test('a malformed query returns FORMERR and does not throw', catchThrows(async (t) => {
  const upstream = fakeUpstream((query) => answerFor(query))
  const factory = forwardingFactory(upstream.port)
  try {
    const dns = new VpnDns({ names: {}, upstream: [LOOPBACK], socketFactory: factory, timeoutMs: 200 })
    // A header that says one question, then a label of 10 bytes with only 3 bytes after it.
    const truncated = b4a.concat([u16(0x7777), u16(0x0100), u16(1), u16(0), u16(0), u16(0), b4a.from([10, 0x61, 0x62, 0x63])])
    const reply = parseReply(await within(dns.handle(truncated), 'handle a truncated query'))
    t.is(reply.id, 0x7777, 'the reply has the query id')
    t.is(reply.qr, 1, 'it is a reply')
    t.is(reply.rcode, 1, 'FORMERR')
    // A buffer shorter than the 12-byte header.
    const short = parseReply(await within(dns.handle(b4a.from([0x12, 0x34, 0x01])), 'handle a short query'))
    t.is(short.rcode, 1, 'FORMERR for a buffer shorter than the header')
  } finally {
    upstream.close()
    factory.close()
  }
}))

test('handle logs nothing: no name or query reaches a console call', catchThrows(async (t) => {
  const upstream = fakeUpstream((query) => answerFor(query))
  const factory = forwardingFactory(upstream.port)
  const methods = ['log', 'info', 'warn', 'error', 'debug']
  const saved = Object.fromEntries(methods.map((m) => [m, console[m]]))
  const logged = []
  try {
    const names = allocate([{ host: 'living-room', service: 'web', origins: [] }], {})
    const dns = new VpnDns({ names, upstream: [LOOPBACK], socketFactory: factory, timeoutMs: 200 })
    for (const m of methods) console[m] = (...args) => logged.push(args.map(String).join(' '))
    await within(dns.handle(buildQuery(0x0101, 'WEB.living-room.internal', TYPE_A)), 'a known name')
    await within(dns.handle(buildQuery(0x0102, 'example.com', TYPE_A)), 'a forwarded name')
    await within(dns.handle(b4a.from([1, 2, 3])), 'a malformed query')
  } finally {
    Object.assign(console, saved)
    upstream.close()
    factory.close()
  }
  t.ok(logged.every((line) => !/example\.com|living-room|web\./i.test(line)), 'no log line names a host or a query')
}))
