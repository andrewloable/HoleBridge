// Tests for the engine entry (lib/engine.js, which index.js wires to the Bare IPC): the requests of spec/ipc.md
// handled by the modules, against the fake host (test/helpers/fake-host.js) on a hyperdht testnet. Every test drives
// the engine with frames only: lib/ipc.js encodes the requests and decodes what the engine sends. Until the IMPL task
// lands, createEngine throws 'not implemented', so every test fails for that reason. catchThrows keeps one failing
// body from ending the run.
const test = require('brittle')
const b4a = require('b4a')
const DHT = require('hyperdht')
const createTestnet = require('hyperdht/testnet')
const TCP = require('bare-tcp')
const UDX = require('udx-native')
const { load, hex } = require('./helpers/vectors.js')
const { createFakeHost, createEchoServer } = require('./helpers/fake-host.js')
const ipc = require('../lib/ipc.js')
const { createEngine } = require('../lib/engine.js')

// SETTLE bounds a wait for a message or a socket. REQUEST_SETTLE bounds a request, which derives keys (Argon2id) and
// may dial. RECONNECT_SETTLE bounds the wait for a dropped session to come back.
const SETTLE = 15000
const REQUEST_SETTLE = 30000
const RECONNECT_SETTLE = 20000

// The application key and the key of the first key-derivation vector. Its relay key is the first relay vector's:
// the relay key is typed as TEST_KEY here, and the member public key is the one the relay test checks.
const vectors = load('key-derivation.json')
const appKey = hex(vectors.keys[0].appKey)
const TEST_KEY = '7KQ-M4X-9TR'
const memberKey = hex(vectors.relayKeys[0].publicKeys.member)

// SOCKS5 values (RFC 1928): the commands and the reply codes the tests check.
const CMD_CONNECT = 0x01
const CMD_UDP_ASSOCIATE = 0x03
const REP_SUCCEEDED = 0x00
const REP_HOST_UNREACHABLE = 0x04
const REP_CONNECTION_REFUSED = 0x05

// LISTED is the service list the connect reply and the services event carry for hostServices: name and kind, with the
// kinds of spec/ipc.md (3 tcp, 4 udp).
const LISTED = [
  { name: 'web', kind: 3 },
  { name: 'game', kind: 4 },
  { name: 'refused', kind: 3 },
  { name: 'hung', kind: 3 }
]

const udx = new UDX()

function noop() {}

function delay(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms))
}

// within rejects if promise has not settled within ms, so a step that never finishes fails its test.
function within(promise, what, ms = SETTLE) {
  let timer
  const timeout = new Promise((resolve, reject) => {
    timer = setTimeout(() => reject(new Error(`${what} did not finish within ${ms} ms`)), ms)
  })
  return Promise.race([promise, timeout]).finally(() => clearTimeout(timer))
}

// poll checks every 50 ms until check returns a truthy value, and resolves with it. It rejects after ms.
async function poll(check, what, ms = SETTLE) {
  const deadline = Date.now() + ms
  while (true) {
    const value = check()
    if (value) return value
    if (Date.now() > deadline) throw new Error(`${what} did not happen within ${ms} ms`)
    await delay(50)
  }
}

// catchThrows runs a test body so that a throw fails only that test. brittle ends the whole run when a body throws,
// and the stub throws 'not implemented' until the IMPL task lands.
function catchThrows(fn) {
  return async (t) => {
    try {
      await fn(t)
    } catch (err) {
      t.fail(err.message)
    }
  }
}

// quiet runs a cleanup step and ignores its failure, so a cleanup never replaces the error of the test.
async function quiet(step) {
  try {
    await step()
  } catch {
    // already closed, or never started
  }
}

// hostServices are the services of the test host: a TCP service to an echo server, a UDP service to a UDP echo
// server, and two services the host refuses (reject code 3) or holds (reject code 4) when they are opened.
function hostServices(echo, udpEcho) {
  return [
    { name: 'web', kind: 'tcp', target: { port: echo.port } },
    { name: 'game', kind: 'udp', target: { port: udpEcho.port } },
    { name: 'refused', kind: 'tcp', target: 'refuse' },
    { name: 'hung', kind: 'tcp', target: 'hang' }
  ]
}

// connectBody is the connect request for the test host. The app knows no LAN address for it, so the search goes to
// the DHT and the route is direct (the testnet has no LAN).
function connectBody(ports = []) {
  return { host: 'home', key: TEST_KEY, appKey, lan: { addresses: [], port: 0 }, ports, bind: '127.0.0.1' }
}

// portOf is the local port the connect reply bound for a service.
function portOf(reply, service) {
  const entry = reply.ports.find((p) => p.service === service)
  return entry ? entry.port : 0
}

// createUdpEcho starts a UDP server on 127.0.0.1 that sends back every datagram it gets.
function createUdpEcho() {
  const socket = udx.createSocket()
  socket.on('error', noop)
  socket.on('message', (msg, from) => socket.trySend(msg, from.port, from.host))
  socket.bind(0, '127.0.0.1')
  return {
    port: socket.address().port,
    close: () => socket.close()
  }
}

// udpClient opens a local UDP socket on 127.0.0.1. next() resolves with the datagrams it receives, in order.
function udpClient() {
  const socket = udx.createSocket()
  const inbox = []
  let waiting = null
  socket.on('error', noop)
  socket.on('message', (msg, from) => {
    if (waiting !== null) {
      const resolve = waiting
      waiting = null
      resolve({ msg, from })
    } else {
      inbox.push({ msg, from })
    }
  })
  socket.bind(0, '127.0.0.1')
  return {
    port: socket.address().port,
    send(payload, port) {
      socket.trySend(b4a.from(payload), port, '127.0.0.1')
    },
    next(ms = SETTLE) {
      if (inbox.length > 0) return Promise.resolve(inbox.shift())
      return within(new Promise((resolve) => (waiting = resolve)), 'a datagram', ms)
    },
    close: () => quiet(() => socket.close())
  }
}

// connectTcp opens a TCP connection to 127.0.0.1:port.
function connectTcp(port) {
  return new Promise((resolve, reject) => {
    const socket = TCP.createConnection(port, '127.0.0.1')
    socket.once('connect', () => {
      socket.on('error', noop)
      resolve(socket)
    })
    socket.once('error', reject)
  })
}

// byteReader reads exact byte counts from a TCP socket. Bytes that arrive before a read wait for it.
function byteReader(socket) {
  let buf = b4a.alloc(0)
  let want = null
  const settle = () => {
    if (want === null || buf.length < want.n) return
    const bytes = b4a.from(buf.subarray(0, want.n))
    buf = buf.subarray(want.n)
    const { resolve } = want
    want = null
    resolve(bytes)
  }
  socket.on('data', (chunk) => {
    buf = b4a.concat([buf, chunk])
    settle()
  })
  socket.on('error', noop)
  return {
    read: (n, what = 'bytes') => within(new Promise((resolve) => {
      want = { n, resolve }
      settle()
    }), what)
  }
}

// echoTcp sends text to a local port and resolves with the bytes that come back.
async function echoTcp(port, text) {
  const socket = await within(connectTcp(port), 'the local connection')
  try {
    const reader = byteReader(socket)
    socket.write(b4a.from(text))
    return b4a.toString(await reader.read(b4a.byteLength(text), 'the echo'))
  } finally {
    socket.destroy()
  }
}

// stopsListening resolves true once a connection to 127.0.0.1:port is refused, which it is when nothing listens there.
async function stopsListening(port) {
  const deadline = Date.now() + SETTLE
  while (Date.now() < deadline) {
    const refused = await new Promise((resolve) => {
      const socket = TCP.createConnection(port, '127.0.0.1')
      socket.once('connect', () => {
        socket.destroy()
        resolve(false)
      })
      socket.once('error', () => resolve(true))
    })
    if (refused) return true
    await delay(50)
  }
  return false
}

// socksClient connects to the VPN front and asks for the no-authentication method (RFC 1928, section 3).
// The reply's method bytes are in method.
async function socksClient(port) {
  const socket = await within(connectTcp(port), 'the VPN front connection')
  const reader = byteReader(socket)
  socket.write(b4a.from([5, 1, 0]))
  const method = await reader.read(2, 'the method reply')
  return { socket, method, read: reader.read }
}

function ipv4(address) {
  return address.split('.').map(Number)
}

// socksRequest sends a SOCKS5 request for an IPv4 destination and resolves with the reply's REP code and bound address.
async function socksRequest(client, command, address, port) {
  client.socket.write(b4a.from([5, command, 0, 1, ...ipv4(address), port >> 8, port & 0xff]))
  const reply = await client.read(10, 'the SOCKS reply')
  return {
    rep: reply[1],
    bound: { address: [...reply.subarray(4, 8)].join('.'), port: (reply[8] << 8) | reply[9] }
  }
}

// socksDatagram wraps payload in the SOCKS5 UDP header for an IPv4 destination (RFC 1928, section 7).
function socksDatagram(address, port, payload) {
  return b4a.concat([b4a.from([0, 0, 0, 1, ...ipv4(address), port >> 8, port & 0xff]), b4a.from(payload)])
}

// watchDefaultKeyPairs records the default key pair of every HyperDHT made until stop() is called. HyperDHT sets
// defaultKeyPair in its constructor (hyperdht/index.js), so a setter on its prototype sees each one. The route on the
// testnet is direct, so a relayed dial would not show the member key; the default key pair does.
function watchDefaultKeyPairs() {
  const seen = []
  const slot = Symbol('defaultKeyPair')
  Object.defineProperty(DHT.prototype, 'defaultKeyPair', {
    configurable: true,
    get() {
      return this[slot]
    },
    set(pair) {
      this[slot] = pair
      seen.push(pair)
    }
  })
  return {
    seen,
    stop: () => {
      delete DHT.prototype.defaultKeyPair
    }
  }
}

// startRig creates an engine whose frames are decoded into out. request sends a request with the next id and resolves
// with the body of its reply. event resolves with the body of the first event of that name that match accepts, whether
// it came before or after the call.
function startRig(testnet) {
  const out = []
  const engine = createEngine({
    send: (frame) => {
      out.push(ipc.decode(frame))
    },
    testnetBootstrap: testnet.bootstrap
  })
  let nextId = 1
  return {
    engine,
    async request(name, body) {
      const id = nextId++
      await within(engine.receive(ipc.encode({ name, id, body })), `the ${name} request`, REQUEST_SETTLE)
      const reply = await poll(
        () => out.find((m) => m.name === 'reply' && m.id === id),
        `the reply to ${name}`
      )
      return reply.body
    },
    async event(name, match = () => true) {
      const message = await poll(
        () => out.find((m) => m.id === 0 && m.name === name && match(m.body)),
        `the ${name} event`
      )
      return message.body
    },
    close: () => engine.close()
  }
}

// withTestnet runs fn with a testnet and destroys it afterwards, also when fn throws.
async function withTestnet(fn) {
  const testnet = await createTestnet(5)
  try {
    await fn({ testnet })
  } finally {
    await quiet(() => testnet.destroy())
  }
}

// withHost runs fn with a testnet and a fake host that serves hostServices. Every resource is closed afterwards, also
// when fn throws. onStream is passed to the fake host (test/helpers/fake-host.js).
async function withHost(fn, { onStream = null } = {}) {
  await withTestnet(async ({ testnet }) => {
    const echo = await createEchoServer()
    const udpEcho = createUdpEcho()
    let host = null
    try {
      host = await createFakeHost({
        testnet,
        key: TEST_KEY,
        appKey,
        services: hostServices(echo, udpEcho),
        onStream
      })
      await fn({ testnet, host })
    } finally {
      if (host !== null) await quiet(() => host.close())
      await quiet(() => udpEcho.close())
      await quiet(() => echo.close())
    }
  })
}

// withEngine runs fn with a host and an engine (startRig) on the testnet, and closes the engine afterwards.
async function withEngine(fn, options) {
  await withHost(async (ctx) => {
    const rig = startRig(ctx.testnet)
    try {
      await fn({ ...ctx, rig })
    } finally {
      await quiet(() => rig.close())
    }
  }, options)
}

test('connect replies ok with the route, the services and their bound ports, then emits route and services', catchThrows(async (t) => {
  await withEngine(async ({ rig }) => {
    const reply = await rig.request('connect', connectBody())
    t.ok(reply.ok, 'the connect reply is ok')
    t.is(reply.code, '', 'and carries no code')
    t.is(reply.route, 'direct', 'the route is direct, since the testnet has no LAN for the host')
    t.alike(reply.services.map(({ name, kind }) => ({ name, kind })), LISTED, 'the services the host shares, with their kinds')
    t.alike(reply.ports.map((p) => p.service).sort(), LISTED.map((s) => s.name).sort(), 'a bound port for each service')
    t.ok(reply.ports.every((p) => p.port > 0), 'each bound port is a real port')
    const route = await rig.event('route', (b) => b.host === 'home' && b.route === 'direct')
    t.is(route.route, 'direct', 'a route event announces the route')
    const services = await rig.event('services', (b) => b.host === 'home')
    t.alike(services.list, LISTED, 'the services event lists the services')
    t.alike(services.ports, reply.ports, 'and the ports bound for them')
  })
}))

test('a local TCP connection to a bound port reaches the service', catchThrows(async (t) => {
  await withEngine(async ({ rig }) => {
    const reply = await rig.request('connect', connectBody())
    t.ok(reply.ok, 'the connect reply is ok')
    const echoed = await echoTcp(portOf(reply, 'web'), 'hello, service')
    t.is(echoed, 'hello, service', 'the bytes come back from the service through the session')
  })
}))

test('close ends the session and listeners, unregisters the host and emits session up false', catchThrows(async (t) => {
  await withEngine(async ({ rig }) => {
    const reply = await rig.request('connect', connectBody())
    t.ok(reply.ok, 'the connect reply is ok')
    await rig.event('session', (b) => b.host === 'home' && b.up === true)
    const closed = await rig.request('close', { host: 'home' })
    t.ok(closed.ok, 'close replies ok')
    const down = await rig.event('session', (b) => b.host === 'home' && b.up === false)
    t.is(down.up, false, 'the session goes down')
    t.ok(await stopsListening(portOf(reply, 'web')), 'the local listener of the service is closed')
    const status = await rig.request('status', { host: 'home' })
    t.is(status.ok, false, 'status for the closed host fails')
    t.is(status.code, 'HB-USAGE', 'with HB-USAGE, since the host is no longer connected')
    const again = await rig.request('close', { host: 'home' })
    t.ok(again.ok, 'close of a host that is not connected replies ok')
  })
}))

test('status replies ok, then emits the route and the session, stream and flow counts', catchThrows(async (t) => {
  await withEngine(async ({ rig }) => {
    const reply = await rig.request('connect', connectBody())
    t.ok(reply.ok, 'the connect reply is ok')
    // One stream open to web, and one flow to game, so the counts are not all zero.
    const stream = await connectTcp(portOf(reply, 'web'))
    const reader = byteReader(stream)
    stream.write(b4a.from('a'))
    t.is(b4a.toString(await reader.read(1, 'the first echo')), 'a', 'the stream is open')
    const udp = udpClient()
    udp.send('ping', portOf(reply, 'game'))
    t.is(b4a.toString((await udp.next()).msg), 'ping', 'the flow to game is open')

    const status = await rig.request('status', { host: 'home' })
    t.ok(status.ok, 'status replies ok')
    const counts = await rig.event('status', (b) => b.host === 'home')
    t.is(counts.route, 'direct', 'the route')
    t.is(counts.sessions, 1, 'one session')
    t.is(counts.streams, 1, 'one open stream')
    t.is(counts.flows, 1, 'one UDP flow')

    stream.destroy()
    await udp.close()
  })
}))

test('relay with a key, then connect, uses the member key pair', catchThrows(async (t) => {
  await withHost(async ({ testnet }) => {
    const rig = startRig(testnet)
    const defaults = watchDefaultKeyPairs()
    try {
      const relay = await rig.request('relay', { key: TEST_KEY, appKey })
      t.ok(relay.ok, 'relay replies ok')
      t.ok(
        defaults.seen.some((pair) => b4a.equals(pair.publicKey, memberKey)),
        'the engine DHT takes the member public key of the relay vector as its default key pair'
      )
      const reply = await rig.request('connect', connectBody())
      t.ok(reply.ok, 'connect replies ok once the relay key is set')
      t.is(await echoTcp(portOf(reply, 'web'), 'relayed'), 'relayed', 'and the service is reached')
    } finally {
      defaults.stop()
      await quiet(() => rig.close())
    }
  })
}))

test('handoff.listen emits a handoff code, and a handoff.send from a second engine delivers handoff.received', catchThrows(async (t) => {
  await withTestnet(async ({ testnet }) => {
    const tv = startRig(testnet)
    const phone = startRig(testnet)
    try {
      const listen = await tv.request('handoff.listen', {})
      t.ok(listen.ok, 'handoff.listen replies ok')
      const code = await tv.event('handoff.code')
      t.ok(/^https:\/\//.test(code.link), 'the code is an https link')
      const sent = await phone.request('handoff.send', { link: code.link, name: 'home', key: TEST_KEY, appKey })
      t.ok(sent.ok, 'the phone sends the host, and the TV adds it')
      const received = await tv.event('handoff.received')
      t.is(received.name, 'home', 'the TV receives the host name')
      t.is(received.key, TEST_KEY, 'and its key')
      t.ok(b4a.equals(received.appKey, appKey), 'and its application key')
    } finally {
      await quiet(() => tv.close())
      await quiet(() => phone.close())
    }
  })
}))

test('vpn.start starts the front and reports its port in the vpn event, and vpn.stop closes it', catchThrows(async (t) => {
  await withEngine(async ({ rig }) => {
    const reply = await rig.request('connect', connectBody())
    t.ok(reply.ok, 'the connect reply is ok')
    const start = await rig.request('vpn.start', {
      services: [{ host: 'home', service: 'web', address: '198.18.0.2', port: portOf(reply, 'web') }],
      dnsUpstream: []
    })
    t.ok(start.ok, 'vpn.start replies ok')
    // The port travels in the vpn event: a reply carries no port (spec/ipc.md, Reply).
    const vpn = await rig.event('vpn')
    t.ok(vpn.port > 0, 'the vpn event carries the port of the front')
    t.alike(
      vpn.addresses,
      [{ host: 'home', service: 'web', address: '198.18.0.2', name: 'web.home.internal' }],
      'and the addresses, each with its name'
    )
    const client = await socksClient(vpn.port)
    t.alike([...client.method], [5, 0], 'the front accepts the no-authentication method')
    const answer = await socksRequest(client, CMD_CONNECT, '198.18.0.2', 80)
    t.is(answer.rep, REP_SUCCEEDED, 'CONNECT to the service succeeds through the front')
    client.socket.write(b4a.from('via vpn'))
    t.is(b4a.toString(await client.read(7, 'the echo through the front')), 'via vpn', 'and the bytes come back')
    client.socket.destroy()

    const stop = await rig.request('vpn.stop', {})
    t.ok(stop.ok, 'vpn.stop replies ok')
    t.ok(await stopsListening(vpn.port), 'the front no longer listens')
  })
}))

test('an unknown request type replies with HB-IPC-DESYNC', catchThrows(async (t) => {
  await withTestnet(async ({ testnet }) => {
    const rig = startRig(testnet)
    try {
      // Type 50 is no message (spec/ipc.md, Type numbers), and the frame is type 50, id 1, with no body.
      await within(rig.engine.receive(b4a.from([50, 1])), 'the unknown frame')
      const error = await rig.event('error', (b) => b.code === 'HB-IPC-DESYNC')
      t.is(error.code, 'HB-IPC-DESYNC', 'the engine answers with the desync code')
    } finally {
      await quiet(() => rig.close())
    }
  })
}))

test('a datagram sent while the host session is down reaches the service once it is back', catchThrows(async (t) => {
  // The host's side of each connection is kept, so the test can drop the connection without stopping the host.
  const connections = []
  await withEngine(async ({ rig }) => {
    const reply = await rig.request('connect', connectBody())
    t.ok(reply.ok, 'the connect reply is ok')
    const game = portOf(reply, 'game')
    const udp = udpClient()
    try {
      udp.send('before', game)
      t.is(b4a.toString((await udp.next()).msg), 'before', 'a datagram reaches the service over the live session')

      // The host drops the connection. The engine's session goes down, and the route manager reconnects by itself.
      connections[connections.length - 1].destroy()
      await rig.event('session', (b) => b.host === 'home' && b.up === false)
      // The same source sends again while the session is down. The engine must hold the datagram until a session is
      // up, and open a fresh flow on the new session, since the old flow belongs to the dropped one.
      udp.send('after', game)
      t.is(b4a.toString((await udp.next(RECONNECT_SETTLE)).msg), 'after', 'the datagram reaches the service once the session is back')
    } finally {
      await udp.close()
    }
  }, { onStream: (socket) => connections.push(socket) })
}))

test('a VPN UDP association reaches a UDP service through a flow, and a second association works after the first closes', catchThrows(async (t) => {
  await withEngine(async ({ rig }) => {
    const reply = await rig.request('connect', connectBody())
    t.ok(reply.ok, 'the connect reply is ok')
    const start = await rig.request('vpn.start', {
      services: [{ host: 'home', service: 'game', address: '198.18.0.3', port: portOf(reply, 'game') }],
      dnsUpstream: []
    })
    t.ok(start.ok, 'vpn.start replies ok')
    const vpn = await rig.event('vpn')

    const first = await socksClient(vpn.port)
    const relay = await socksRequest(first, CMD_UDP_ASSOCIATE, '0.0.0.0', 0)
    t.is(relay.rep, REP_SUCCEEDED, 'UDP ASSOCIATE succeeds')
    const client = udpClient()
    client.send(socksDatagram('198.18.0.3', 7, 'vpn'), relay.bound.port)
    const answer = await client.next()
    t.ok(b4a.equals(answer.msg.subarray(0, 10), b4a.from([0, 0, 0, 1, 198, 18, 0, 3, 0, 7])), 'the reply comes from the service address')
    t.is(b4a.toString(answer.msg.subarray(10)), 'vpn', 'and carries the echoed payload')

    // The first association ends with its control connection. A second one must work from a fresh flow.
    first.socket.destroy()
    const second = await socksClient(vpn.port)
    const relayAgain = await socksRequest(second, CMD_UDP_ASSOCIATE, '0.0.0.0', 0)
    const again = udpClient()
    again.send(socksDatagram('198.18.0.3', 7, 'again'), relayAgain.bound.port)
    t.is(b4a.toString((await again.next()).msg.subarray(10)), 'again', 'the second association gets its reply')

    second.socket.destroy()
    await client.close()
    await again.close()
  })
}))

test('the VPN front maps reject codes 3 and 4 to REP 0x05 and 0x04', catchThrows(async (t) => {
  await withEngine(async ({ rig }) => {
    const reply = await rig.request('connect', connectBody())
    t.ok(reply.ok, 'the connect reply is ok')
    const start = await rig.request('vpn.start', {
      services: [
        { host: 'home', service: 'refused', address: '198.18.0.4', port: portOf(reply, 'refused') },
        { host: 'home', service: 'hung', address: '198.18.0.5', port: portOf(reply, 'hung') }
      ],
      dnsUpstream: []
    })
    t.ok(start.ok, 'vpn.start replies ok')
    const vpn = await rig.event('vpn')

    // Each CONNECT gets its own control connection: a refused CONNECT ends its connection.
    const refusedClient = await socksClient(vpn.port)
    const refused = await socksRequest(refusedClient, CMD_CONNECT, '198.18.0.4', 80)
    t.is(refused.rep, REP_CONNECTION_REFUSED, 'a refusal (reject code 3, HB-TARGET-REFUSED) is connection refused')
    refusedClient.socket.destroy()

    const hungClient = await socksClient(vpn.port)
    const hung = await socksRequest(hungClient, CMD_CONNECT, '198.18.0.5', 80)
    t.is(hung.rep, REP_HOST_UNREACHABLE, 'a timeout (reject code 4, HB-TARGET-TIMEOUT) is host unreachable')
    hungClient.socket.destroy()
  })
}))

test('the VPN front logs through the engine logger, one line with its code and no address', catchThrows(async (t) => {
  await withEngine(async ({ rig }) => {
    const reply = await rig.request('connect', connectBody())
    t.ok(reply.ok, 'the connect reply is ok')
    const start = await rig.request('vpn.start', {
      services: [{ host: 'home', service: 'game', address: '198.18.0.3', port: portOf(reply, 'game') }],
      dnsUpstream: []
    })
    t.ok(start.ok, 'vpn.start replies ok')
    const vpn = await rig.event('vpn')

    // The engine logger writes each line with console.error, so the lines are captured here.
    const lines = []
    const logError = console.error
    console.error = (...args) => lines.push(args.map(String).join(' '))
    const client = await socksClient(vpn.port)
    const udp = udpClient()
    try {
      const relay = await socksRequest(client, CMD_UDP_ASSOCIATE, '0.0.0.0', 0)
      // 1200 bytes is over the 1144-byte limit of a service datagram, so the front drops it and logs the code.
      udp.send(socksDatagram('198.18.0.3', 7, b4a.alloc(1200, 1)), relay.bound.port)
      const line = await poll(() => lines.find((l) => l.includes('HB-UDP-TOO-LARGE')), 'the HB-UDP-TOO-LARGE line')
      const entry = JSON.parse(line)
      t.is(entry.level, 'warn', 'the line is a warning')
      t.is(entry.code, 'HB-UDP-TOO-LARGE', 'it carries the code')
      t.is(entry.msg, 'a datagram over the limit is dropped', 'and the front message')
      t.ok(!line.includes('198.18.0.3'), 'the line names no VPN address')
    } finally {
      console.error = logError
      client.socket.destroy()
      await udp.close()
    }
  })
}))
