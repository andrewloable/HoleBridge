// Tests for UDP services on the app (lib/udp.js). Rules: docs/architecture.md, "UDP services" and "Limits", and the
// unordered-datagram measurements in docs/spike-m1.md. Each test drives a UdpServices object with a fake session
// (fakeSession, which also plays the host's echo target), fake UDP sockets (fakeSockets, shaped as udx-native's
// sockets) and a fake clock (fakeClock). Time moves only when a test advances it, so the tests run in virtual time
// and never open a real socket or wait on a real timer.
//
// Shapes the IMPL must meet (also stated in lib/udp.js):
// - new UdpServices({ session, bind, maxDatagram, idleMs, clock, log, createSocket }). bind defaults to 127.0.0.1,
//   idleMs to 60 s. createSocket() returns a socket with bind(port, host), address(), trySend(payload, port, host),
//   send(payload, port, host), on('message', fn) with fn(payload, { host, family, port }) and close().
// - The session surface: route, flags, sendFlow({ flow, service, payload }), sendDatagram({ flow, payload }) (message
//   10: returns false while the channel is full), sendUnordered({ flow, payload }), and events 'datagram' and 'drain'.
// - set(services, remembered) returns { [name]: port } for the udp services. close() closes every socket.
// - log.warn(msg, { code, flow }) carries HB-UDP-TOO-LARGE. Its fields never hold payload bytes.
//
// Until the IMPL task (HoleBridge-01p.2) lands, the constructor throws 'not implemented', so each test fails for
// that reason.
const test = require('brittle')
const b4a = require('b4a')
const EventEmitter = require('bare-events')
const { FLAG } = require('../lib/protocol.js')
const { UdpServices } = require('../lib/udp.js')

const LOCAL = '127.0.0.1'
const DNS = { name: 'dns', kind: 'udp', port: 53, origins: [] }
// A DNS query for example.com, type A, class IN: a 12-byte header and one question.
const DNS_QUERY = b4a.from('123401000001000000000000076578616d706c6503636f6d0000010001', 'hex')

// catchThrows runs a test body so that a throw fails only that test. brittle ends the whole run when a body
// throws, and the stub throws 'not implemented' until the IMPL task lands.
function catchThrows(fn) {
  return async (t) => {
    try {
      await fn(t)
    } catch (err) {
      t.fail(err.message)
    }
  }
}

// fakeClock() -> { timers, now(), advance(ms) }. Timers fire in virtual time order. advance(ms) fires every timer due
// within ms and lets the promise chains settle after each one, so a timer that a resolved promise schedules is in
// place before the next one fires. Virtual time then moves to the end of the advance.
function fakeClock() {
  let now = 0
  let lastId = 0
  const pending = new Map() // id -> { at, fn }
  const timers = {
    setTimeout(fn, ms) {
      const id = ++lastId
      pending.set(id, { at: now + ms, fn })
      return id
    },
    clearTimeout(id) {
      pending.delete(id)
    }
  }
  const drain = async () => {
    for (let i = 0; i < 1000; i++) await null
  }
  async function advance(ms) {
    const end = now + ms
    await drain()
    for (let fired = 0; ; fired++) {
      if (fired > 100000) throw new Error('fake clock: too many timers fired in one advance')
      let next = null
      for (const [id, timer] of pending) {
        if (timer.at <= end && (next === null || timer.at < pending.get(next).at)) next = id
      }
      if (next === null) break
      const { at, fn } = pending.get(next)
      pending.delete(next)
      now = at
      fn()
      await drain()
    }
    now = end
  }
  return { timers, now: () => now, advance }
}

// fakeSockets() -> { createSocket, sockets, open(port) }. createSocket() returns a fake udx-native socket. bind(port,
// host) takes the port asked for, or picks one when it is 0. trySend and send record { payload, port, host } in sent,
// in order. close() marks the socket closed. open(port) is the open socket bound to port, or null.
function fakeSockets() {
  const sockets = []
  let nextPort = 40000
  const createSocket = () => {
    const socket = new EventEmitter()
    socket.bound = null // { host, family, port } once bound
    socket.closed = false
    socket.sent = []
    socket.bind = (port, host) => {
      if (socket.bound) throw new Error('Already bound')
      socket.bound = { host, family: 4, port: port || nextPort++ }
    }
    socket.address = () => socket.bound
    socket.trySend = (payload, port, host) => {
      socket.sent.push({ payload: b4a.from(payload), port, host })
      return true
    }
    socket.send = async (payload, port, host) => {
      socket.trySend(payload, port, host)
    }
    socket.close = async () => {
      if (socket.closed) return
      socket.closed = true
      socket.emit('close')
    }
    sockets.push(socket)
    return socket
  }
  const open = (port) => sockets.find((s) => !s.closed && s.bound !== null && s.bound.port === port) || null
  return { createSocket, sockets, open }
}

// fakeSession({ route, flags, echo }) is the session UdpServices takes. sent records what UDP sent on the session, in
// order: { kind: 'flow', flow, service, payload } for message 9, { kind: 'datagram', flow, payload } for message 10,
// and { kind: 'unordered', flow, payload } for an unordered datagram. full: true makes sendDatagram take nothing and
// return false, as a full channel does; setFull(false) emits 'drain'. echo: the fake host is a UDP echo target, which
// sends each payload back to its flow one tick later.
function fakeSession({ route = 'direct', flags = FLAG.datagrams, echo = false } = {}) {
  const session = new EventEmitter()
  session.route = route
  session.flags = flags
  session.sent = []
  session.full = false

  const echoBack = (flow, payload) => {
    if (!echo) return
    Promise.resolve().then(() => session.emit('datagram', { flow, payload: b4a.from(payload) }))
  }
  session.sendFlow = ({ flow, service, payload }) => {
    session.sent.push({ kind: 'flow', flow, service, payload: b4a.from(payload) })
    echoBack(flow, payload)
  }
  session.sendDatagram = ({ flow, payload }) => {
    if (session.full) return false
    session.sent.push({ kind: 'datagram', flow, payload: b4a.from(payload) })
    echoBack(flow, payload)
    return true
  }
  session.sendUnordered = ({ flow, payload }) => {
    session.sent.push({ kind: 'unordered', flow, payload: b4a.from(payload) })
    echoBack(flow, payload)
  }
  session.setFull = (full) => {
    session.full = full
    if (!full) session.emit('drain')
  }
  return session
}

// fakeLog() -> { entries, error, warn, info, debug }. Each call appends { level, msg, fields }; fields is {} when the
// caller gives none.
function fakeLog() {
  const entries = []
  const level = (name) => (msg, fields = {}) => entries.push({ level: name, msg, fields })
  return { entries, error: level('error'), warn: level('warn'), info: level('info'), debug: level('debug') }
}

// source(port) is the address of a local sender, as a udx-native socket reports it.
function source(port) {
  return { host: LOCAL, family: 4, port }
}

// oversized(size) is a payload of exactly size bytes that names itself, so a test can look for it in a log.
function oversized(size) {
  const pattern = 'oversize-marker-'
  return b4a.from(pattern.repeat(Math.ceil(size / pattern.length)).slice(0, size))
}

// indexed(i, size) is a payload of size bytes that starts with its index i, so a test can tell which one arrived.
function indexed(i, size) {
  return b4a.from(('d' + String(i).padStart(4, '0')).padEnd(size, '.'))
}

function indexOf(payload) {
  return Number(b4a.toString(payload).slice(1, 5))
}

// start(options) builds a UdpServices on a fresh session, socket factory, clock and logger, and binds services on it.
// It returns what a test needs: the object, the session, the clock, the sockets, the log, the ports and the socket of
// the first service's port (the dns service, by default).
async function start({ session = fakeSession(), services = [DNS], remembered = {} } = {}) {
  const clock = fakeClock()
  const sockets = fakeSockets()
  const log = fakeLog()
  const udp = new UdpServices({ session, clock: clock.timers, log, createSocket: sockets.createSocket })
  const ports = await udp.set(services, remembered)
  const socket = sockets.open(ports[services[0].name])
  return { udp, session, clock, sockets, log, ports, socket }
}

test('a DNS-style request to the local port reaches a UDP echo target through the fake host and the reply returns to the sender', catchThrows(async (t) => {
  const session = fakeSession({ echo: true })
  const { ports, socket, clock } = await start({ session })
  t.ok(Number.isInteger(ports.dns), 'the dns service has a local port')
  socket.emit('message', DNS_QUERY, source(50000))
  await clock.advance(0)
  t.is(session.sent.length, 1, 'the first datagram goes out as one flow message')
  t.is(session.sent[0].kind, 'flow', 'the first datagram is a flow message (message 9)')
  t.is(session.sent[0].service, 'dns', 'the flow names the service')
  t.ok(b4a.equals(session.sent[0].payload, DNS_QUERY), 'the flow carries the query as its first datagram')
  t.is(socket.sent.length, 1, 'the echoed reply is sent once')
  t.ok(b4a.equals(socket.sent[0].payload, DNS_QUERY), 'the reply carries the echoed payload')
  t.is(socket.sent[0].port, 50000, 'the reply goes back to the sender port')
  t.is(socket.sent[0].host, LOCAL, 'and to the sender address')
}))

test('two local sources get two flows with their own replies', catchThrows(async (t) => {
  const session = fakeSession({ echo: true })
  const { socket, clock } = await start({ session })
  socket.emit('message', b4a.from('from-a'), source(50001))
  socket.emit('message', b4a.from('from-b'), source(50002))
  await clock.advance(0)
  const flows = session.sent.filter((m) => m.kind === 'flow')
  t.is(flows.length, 2, 'each source opens its own flow')
  t.ok(flows[0].flow !== flows[1].flow, 'the two flows have different ids')
  t.is(b4a.toString(flows[0].payload), 'from-a', 'the first flow carries source A')
  t.is(b4a.toString(flows[1].payload), 'from-b', 'the second flow carries source B')
  const toA = socket.sent.filter((s) => s.port === 50001).map((s) => b4a.toString(s.payload))
  const toB = socket.sent.filter((s) => s.port === 50002).map((s) => b4a.toString(s.payload))
  t.alike(toA, ['from-a'], 'source A gets only its own reply')
  t.alike(toB, ['from-b'], 'source B gets only its own reply')
}))

test('a 1201-byte datagram is dropped and HB-UDP-TOO-LARGE is logged once for that flow', catchThrows(async (t) => {
  const session = fakeSession()
  const { socket, clock, log } = await start({ session })
  socket.emit('message', b4a.from('hello'), source(50000)) // opens the flow
  socket.emit('message', oversized(1201), source(50000))
  socket.emit('message', oversized(1201), source(50000))
  await clock.advance(0)
  t.is(session.sent.filter((m) => m.payload.length === 1201).length, 0, 'the oversize datagrams are not sent')
  const tooLarge = log.entries.filter((e) => (e.fields || {}).code === 'HB-UDP-TOO-LARGE')
  t.is(tooLarge.length, 1, 'HB-UDP-TOO-LARGE is logged once for the flow')
  t.ok(!JSON.stringify(log.entries).includes('oversize-marker'), 'the log line carries no payload bytes')
  socket.emit('message', b4a.from('again'), source(50000))
  await clock.advance(0)
  const last = session.sent[session.sent.length - 1]
  t.is(b4a.toString(last.payload), 'again', 'a normal datagram after that still goes out')
  t.ok(last.flow === session.sent[0].flow, 'on the same flow')
}))

test('on a session without unordered datagrams, datagrams use message 10; when the queue holds 256 KiB, new ones are dropped', catchThrows(async (t) => {
  for (const [route, flags] of [['lan', FLAG.datagrams], ['direct', 0]]) {
    const session = fakeSession({ route, flags })
    const { socket, clock } = await start({ session })
    socket.emit('message', b4a.from('prime'), source(50000))
    socket.emit('message', b4a.from('second'), source(50000))
    await clock.advance(0)
    t.alike(
      session.sent.map((m) => m.kind),
      ['flow', 'datagram'],
      `${route} route, flags ${flags}: the second datagram is message 10, not an unordered datagram`
    )
  }

  const session = fakeSession({ route: 'lan', flags: FLAG.datagrams })
  const { socket, clock } = await start({ session })
  socket.emit('message', b4a.from('prime'), source(50000))
  await clock.advance(0)
  const datagrams = () => session.sent.filter((m) => m.kind === 'datagram')
  session.setFull(true)
  for (let i = 0; i < 300; i++) socket.emit('message', indexed(i, 1024), source(50000))
  await clock.advance(0)
  t.is(datagrams().length, 0, 'while the channel is full it takes no datagram')
  session.setFull(false)
  await clock.advance(0)
  const delivered = datagrams().map((m) => indexOf(m.payload))
  t.is(delivered.length, 256, '256 KiB of payload is queued and the other 44 datagrams are dropped')
  t.alike(
    delivered,
    Array.from({ length: 256 }, (_, i) => i),
    'the queued datagrams go out in order once the channel drains'
  )
}))

test('a flow idle for 60 s closes (fake clock)', catchThrows(async (t) => {
  const session = fakeSession()
  const { socket, clock } = await start({ session })
  const flowMessages = () => session.sent.filter((m) => m.kind === 'flow').length
  socket.emit('message', b4a.from('q1'), source(50000))
  await clock.advance(0)
  t.is(flowMessages(), 1, 'the first datagram opens a flow')
  await clock.advance(59_000)
  socket.emit('message', b4a.from('q2'), source(50000))
  await clock.advance(0)
  await clock.advance(59_000)
  socket.emit('message', b4a.from('q3'), source(50000))
  await clock.advance(0)
  t.is(flowMessages(), 1, 'a flow stays open until 60 s after its last datagram, and each datagram restarts that time')
  await clock.advance(60_000)
  socket.emit('message', b4a.from('q4'), source(50000))
  await clock.advance(0)
  t.is(flowMessages(), 2, 'after 60 s with no datagram the flow is closed, so the next datagram opens a new flow')
  t.is(b4a.toString(session.sent[session.sent.length - 1].payload), 'q4', 'and that flow carries q4')
}))

test('after a session replacement the next datagram opens a new flow', catchThrows(async (t) => {
  const first = fakeSession()
  const { udp, socket, clock, sockets, ports, log } = await start({ session: first })
  socket.emit('message', b4a.from('before'), source(50000))
  await clock.advance(0)
  t.is(first.sent.length, 1, 'the first session carries the flow')
  await udp.close()

  const second = fakeSession()
  const replacement = new UdpServices({ session: second, clock: clock.timers, log, createSocket: sockets.createSocket })
  const again = await replacement.set([DNS], ports)
  t.is(again.dns, ports.dns, 'the new UdpServices binds the local port the app kept')
  sockets.open(again.dns).emit('message', b4a.from('after'), source(50000))
  await clock.advance(0)
  t.is(first.sent.length, 1, 'the old session gets nothing more')
  t.is(second.sent.length, 1, 'the new session gets one message')
  t.is(second.sent[0].kind, 'flow', 'the next datagram opens a fresh flow (message 9) on the new session')
  t.is(b4a.toString(second.sent[0].payload), 'after', 'and carries that datagram')
}))

test('set() binds one socket per udp service on 127.0.0.1 and returns only their ports', catchThrows(async (t) => {
  const services = [
    DNS,
    { name: 'web', kind: 'https', port: 443, origins: [] },
    { name: 'game', kind: 'udp', port: 27015, origins: [] }
  ]
  const { ports, sockets } = await start({ services })
  t.alike(Object.keys(ports).sort(), ['dns', 'game'], 'only the udp services get a port')
  t.is(sockets.sockets.length, 2, 'one socket per udp service')
  t.ok(sockets.sockets.every((s) => s.bound.host === LOCAL), 'every socket is bound on 127.0.0.1')
  t.is(sockets.open(ports.dns).bound.port, ports.dns, 'the dns port is the one its socket is bound to')
}))

test('close() closes every socket', catchThrows(async (t) => {
  const services = [DNS, { name: 'game', kind: 'udp', port: 27015, origins: [] }]
  const { udp, sockets } = await start({ services })
  t.is(sockets.sockets.length, 2, 'two udp services, two sockets')
  await udp.close()
  t.ok(sockets.sockets.every((s) => s.closed), 'every socket is closed')
}))

test('on a direct or relay session with unordered datagrams, datagrams after the first go out unordered, even while the ordered channel is full', catchThrows(async (t) => {
  for (const route of ['direct', 'relay']) {
    const session = fakeSession({ route, flags: FLAG.datagrams })
    const { socket, clock } = await start({ session })
    socket.emit('message', b4a.from('prime'), source(50000))
    session.setFull(true) // the ordered channel takes nothing: unordered datagrams must not wait for it
    for (let i = 0; i < 300; i++) socket.emit('message', indexed(i, 1024), source(50000))
    await clock.advance(0)
    t.is(session.sent[0].kind, 'flow', `${route}: the first datagram is the flow message`)
    const later = session.sent.slice(1)
    t.is(later.length, 300, `${route}: none of the 300 later datagrams is dropped or held back`)
    t.ok(
      later.every((m) => m.kind === 'unordered' && m.flow === session.sent[0].flow),
      `${route}: each later datagram is an unordered datagram on the flow`
    )
  }
}))

test('a datagram over the limit is dropped on the LAN route too, and HB-UDP-TOO-LARGE is logged once for each flow', catchThrows(async (t) => {
  const session = fakeSession({ route: 'lan', flags: FLAG.datagrams })
  const { socket, clock, log } = await start({ session })
  socket.emit('message', b4a.from('a0'), source(50001))
  socket.emit('message', b4a.from('b0'), source(50002))
  for (const port of [50001, 50002]) {
    socket.emit('message', oversized(1201), source(port))
    socket.emit('message', oversized(1201), source(port))
  }
  await clock.advance(0)
  t.is(session.sent.filter((m) => m.payload.length === 1201).length, 0, 'the oversize datagrams are not sent as message 10')
  const flows = session.sent.filter((m) => m.kind === 'flow').map((m) => m.flow)
  t.is(flows.length, 2, 'two sources, two flows')
  const logged = log.entries.filter((e) => (e.fields || {}).code === 'HB-UDP-TOO-LARGE').map((e) => e.fields.flow)
  t.is(logged.length, 2, 'one HB-UDP-TOO-LARGE line for each flow, not one for the process')
  t.ok(flows.every((id) => logged.includes(id)), 'each line names its own flow')
}))

test('flow ids are unique across the udp services of a session', catchThrows(async (t) => {
  const session = fakeSession()
  const services = [DNS, { name: 'game', kind: 'udp', port: 27015, origins: [] }]
  const { ports, sockets, clock } = await start({ session, services })
  // The same source port on two services' sockets: two sources, two flows. The host finds a datagram's flow by its
  // id alone, across all the session's services, so the two ids must differ.
  sockets.open(ports.dns).emit('message', b4a.from('to-dns'), source(50000))
  sockets.open(ports.game).emit('message', b4a.from('to-game'), source(50000))
  await clock.advance(0)
  const flows = session.sent.filter((m) => m.kind === 'flow')
  t.alike(flows.map((m) => m.service).sort(), ['dns', 'game'], 'each service opens its own flow')
  t.ok(flows.length === 2 && flows[0].flow !== flows[1].flow, 'the two flows have different ids')
}))

test('a reply from the host keeps the flow open, and a reply for a flow that is closed or unknown is dropped', catchThrows(async (t) => {
  const session = fakeSession()
  const { socket, clock } = await start({ session })
  socket.emit('message', b4a.from('request'), source(50000))
  await clock.advance(0)
  const flow = session.sent[0].flow
  await clock.advance(50_000)
  session.emit('datagram', { flow, payload: b4a.from('reply-1') })
  await clock.advance(0)
  t.is(socket.sent.length, 1, 'a reply 50 s in reaches the source')
  // 100 s after the request, 50 s after the reply: open only if a reply restarts the idle time
  await clock.advance(50_000)
  session.emit('datagram', { flow, payload: b4a.from('reply-2') })
  await clock.advance(0)
  t.alike(socket.sent.map((s) => b4a.toString(s.payload)), ['reply-1', 'reply-2'], 'a reply restarts the idle time, so the flow is still open')
  // 60 s with no datagram either way: the flow closes
  await clock.advance(60_000)
  session.emit('datagram', { flow, payload: b4a.from('late') })
  session.emit('datagram', { flow: 987654, payload: b4a.from('stray') })
  await clock.advance(0)
  t.is(socket.sent.length, 2, 'a reply for a closed flow or for a flow id the app never opened is dropped, and does not throw')
}))

test('set() binds the remembered port when there is one, not the service port', catchThrows(async (t) => {
  const { ports, sockets } = await start({ remembered: { dns: 45353 } })
  t.is(ports.dns, 45353, 'the dns service (port 53) binds the port the app remembered')
  t.ok(sockets.open(45353) !== null, 'and its socket is bound there')
}))

test('a new source beyond 256 flows is dropped without sending, and is served again once a flow has expired', catchThrows(async (t) => {
  const session = fakeSession()
  const { socket, clock, log } = await start({ session })
  for (let i = 0; i < 256; i++) socket.emit('message', b4a.from('hello'), source(20000 + i))
  await clock.advance(0)
  t.is(session.sent.filter((m) => m.kind === 'flow').length, 256, 'the first 256 sources each open a flow')
  socket.emit('message', b4a.from('one too many'), source(30000))
  socket.emit('message', b4a.from('another one'), source(30001))
  await clock.advance(0)
  t.is(session.sent.length, 256, 'a new source beyond 256 flows sends nothing')
  const limited = log.entries.filter((e) => (e.fields || {}).code === 'HB-LIMIT-REACHED')
  t.is(limited.length, 1, 'HB-LIMIT-REACHED is logged once, not for each dropped datagram')
  t.ok(!JSON.stringify(log.entries).includes('one too many'), 'the log carries no payload bytes')
  socket.emit('message', b4a.from('known source'), source(20000))
  await clock.advance(0)
  t.is(session.sent.length, 257, 'a source that already has a flow still sends')
  t.is(session.sent[256].kind, 'unordered', 'on its flow, as an unordered datagram')
  await clock.advance(60_000)
  socket.emit('message', b4a.from('now there is room'), source(30000))
  await clock.advance(0)
  const last = session.sent[session.sent.length - 1]
  t.is(last.kind, 'flow', 'after the flows have expired the source opens a flow')
  t.is(b4a.toString(last.payload), 'now there is room', 'and that flow carries its datagram')
}))

test('the flow cap counts the flows of every udp service of the session', catchThrows(async (t) => {
  const session = fakeSession()
  const services = [DNS, { name: 'game', kind: 'udp', port: 27015, origins: [] }]
  const { ports, sockets, clock } = await start({ session, services })
  for (let i = 0; i < 128; i++) sockets.open(ports.dns).emit('message', b4a.from('a'), source(20000 + i))
  for (let i = 0; i < 128; i++) sockets.open(ports.game).emit('message', b4a.from('b'), source(20000 + i))
  await clock.advance(0)
  t.is(session.sent.filter((m) => m.kind === 'flow').length, 256, '128 flows on each service make 256')
  sockets.open(ports.dns).emit('message', b4a.from('c'), source(40000))
  sockets.open(ports.game).emit('message', b4a.from('d'), source(40000))
  await clock.advance(0)
  t.is(session.sent.length, 256, 'a new source on either service is dropped')
}))
