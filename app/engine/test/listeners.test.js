// Tests for the local TCP listeners (lib/listeners.js): one listener per TCP service, on 127.0.0.1 or on
// 0.0.0.0 when the user shares with the network. A connection to a listener opens its service through the
// open callback, and bytes flow both ways. Rules: docs/cli.md (Local ports, Share with my network) and
// docs/architecture.md (Bind, Forward).
const test = require('brittle')
const b4a = require('b4a')
const TCP = require('bare-tcp')
const { Duplex, PassThrough } = require('streamx')
const { createEchoServer } = require('./helpers/fake-host.js')
const { Listeners } = require('../lib/listeners.js')
const { REJECT } = require('../lib/protocol.js')

const MIB = 1024 * 1024
// SETTLE bounds every wait, so a listener that never answers fails its test instead of hanging it.
const SETTLE = 10000

// brittle ends the whole run when a test body throws. Until the IMPL task lands, the code throws
// 'not implemented', so each body runs under catchThrows, which fails only its own test. Bodies are
// async because the listeners and the sockets are promises.
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

const delay = (ms) => new Promise((resolve) => setTimeout(resolve, ms))

// within waits for promise, and fails with what if it has not settled within SETTLE.
function within(promise, what) {
  let timer
  const timeout = new Promise((resolve, reject) => {
    timer = setTimeout(() => reject(new Error(`${what} did not finish within ${SETTLE} ms`)), SETTLE)
  })
  return Promise.race([promise, timeout]).finally(() => clearTimeout(timer))
}

// waitFor polls cond until it holds, and fails with what if it does not hold within SETTLE.
async function waitFor(cond, what) {
  const deadline = Date.now() + SETTLE
  while (!cond()) {
    if (Date.now() > deadline) throw new Error(`${what} did not happen within ${SETTLE} ms`)
    await delay(10)
  }
}

// thrownByAsync(fn) resolves to what fn throws or rejects with, or null when fn resolves.
async function thrownByAsync(fn) {
  try {
    await fn()
  } catch (err) {
    return err
  }
  return null
}

// freePorts(n) returns n distinct ports on 127.0.0.1 that were free when it ran. Each comes from a server
// bound to port 0, and the servers are closed again before the test binds the ports.
async function freePorts(n) {
  const servers = []
  for (let i = 0; i < n; i++) servers.push(await createEchoServer())
  const ports = servers.map((server) => server.port)
  await Promise.all(servers.map((server) => server.close()))
  return ports
}

// connect opens a TCP connection to 127.0.0.1:port and resolves with the socket once it is connected.
// socket.endedAt resolves when the socket closes, ends or errors. It is set up before the connect
// completes, so an early close is not missed.
function connect(port) {
  return within(
    new Promise((resolve, reject) => {
      const socket = TCP.createConnection(port, '127.0.0.1', () => resolve(socket))
      socket.on('error', reject)
      socket.endedAt = new Promise((done) => {
        socket.once('close', done)
        socket.once('end', done)
        socket.once('error', done)
      })
    }),
    `a connection to port ${port}`
  )
}

// collect resolves with the first n bytes that readable emits, as a string. It rejects if readable ends
// first or fails.
function collect(readable, n) {
  return within(
    new Promise((resolve, reject) => {
      const parts = []
      let size = 0
      readable.on('data', (chunk) => {
        parts.push(chunk)
        size += chunk.byteLength
        if (size >= n) resolve(b4a.toString(b4a.concat(parts).subarray(0, n)))
      })
      readable.once('error', reject)
      readable.once('end', () => reject(new Error('the stream ended before its bytes arrived')))
    }),
    `${n} bytes`
  )
}

// hostPair returns the stream that open resolves with, and the two streams behind it. Bytes the app
// sends come out of toHost. The test writes fromHost to send bytes to the app.
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

// recordingOpen returns an open callback that records each call in calls, as { service, stream, toHost,
// fromHost }. The service is the name, as lib/mux.js sends it.
function recordingOpen(calls) {
  return async (service) => {
    const pair = hostPair()
    calls.push({ service, ...pair })
    return pair.stream
  }
}

test('a service with a free port hint binds that port on 127.0.0.1', catchThrows(async (t) => {
  const [hint] = await freePorts(1)
  const listeners = new Listeners({ open: recordingOpen([]), bind: '127.0.0.1' })
  t.teardown(() => listeners.close())
  const ports = await listeners.set([{ name: 'web', kind: 'tcp', port: hint }], {})
  t.is(ports.web, hint, 'the bound port is the hint')
  const bound = listeners.address('web')
  t.is(bound && bound.address, '127.0.0.1', 'the listener is bound to 127.0.0.1')
  const socket = await connect(hint)
  t.teardown(() => socket.destroy())
}))

test('a taken port hint falls back to an OS-assigned port, reported in the result', catchThrows(async (t) => {
  const taken = await createEchoServer()
  t.teardown(() => taken.close())
  const listeners = new Listeners({ open: recordingOpen([]), bind: '127.0.0.1' })
  t.teardown(() => listeners.close())
  const ports = await listeners.set([{ name: 'web', kind: 'tcp', port: taken.port }], {})
  t.ok(ports.web > 0, 'a bound port is reported')
  t.ok(ports.web !== taken.port, 'the taken hint is not the bound port')
  const socket = await connect(ports.web)
  t.teardown(() => socket.destroy())
}))

test('a remembered port wins over the port hint', catchThrows(async (t) => {
  const [hint, remembered] = await freePorts(2)
  const listeners = new Listeners({ open: recordingOpen([]), bind: '127.0.0.1' })
  t.teardown(() => listeners.close())
  const ports = await listeners.set([{ name: 'web', kind: 'tcp', port: hint }], { web: remembered })
  t.is(ports.web, remembered, 'the remembered port is bound, not the hint')
  const socket = await connect(remembered)
  t.teardown(() => socket.destroy())
}))

test('a connection calls open with the service name, and bytes flow both ways', catchThrows(async (t) => {
  const calls = []
  const listeners = new Listeners({ open: recordingOpen(calls), bind: '127.0.0.1' })
  t.teardown(() => listeners.close())
  const ports = await listeners.set([{ name: 'web', kind: 'tcp', port: 0 }], {})
  const socket = await connect(ports.web)
  t.teardown(() => socket.destroy())
  await waitFor(() => calls.length === 1, 'open to be called')
  t.is(calls[0].service, 'web', 'open is called with the service name')
  const toHost = collect(calls[0].toHost, 4)
  socket.write('ping')
  t.is(await toHost, 'ping', 'the app bytes reach the host side')
  const toApp = collect(socket, 4)
  calls[0].fromHost.write('pong')
  t.is(await toApp, 'pong', 'the host bytes reach the app')
}))

test('open rejecting destroys the socket and emits an error with the code', catchThrows(async (t) => {
  const listeners = new Listeners({
    open: async () => {
      throw Object.assign(new Error('target refused'), { code: REJECT.targetRefused })
    },
    bind: '127.0.0.1'
  })
  t.teardown(() => listeners.close())
  const errors = []
  listeners.on('error', (err) => errors.push(err))
  const ports = await listeners.set([{ name: 'web', kind: 'tcp', port: 0 }], {})
  const socket = await connect(ports.web)
  t.teardown(() => socket.destroy())
  await within(socket.endedAt, 'the socket to be destroyed')
  await waitFor(() => errors.length > 0, 'the error event')
  t.is(errors[0].code, REJECT.targetRefused, 'the error event carries the code that open rejected with')
}))

test('set closes the listener of a removed service and opens one for an added service', catchThrows(async (t) => {
  const [webPort, sshPort, dbPort] = await freePorts(3)
  const calls = []
  const listeners = new Listeners({ open: recordingOpen(calls), bind: '127.0.0.1' })
  t.teardown(() => listeners.close())
  const web = { name: 'web', kind: 'tcp', port: webPort }
  const ssh = { name: 'ssh', kind: 'tcp', port: sshPort }
  const db = { name: 'db', kind: 'tcp', port: dbPort }

  const first = await listeners.set([web, ssh], {})
  t.is(first.ssh, sshPort, 'ssh is listening')

  const second = await listeners.set([web], {})
  t.absent(second.ssh, 'a removed service has no bound port')
  const refused = await thrownByAsync(() => connect(sshPort))
  t.is(refused && refused.code, 'ECONNREFUSED', 'the port of the removed service refuses connections')
  const webSocket = await connect(webPort)
  t.teardown(() => webSocket.destroy())

  const third = await listeners.set([web, db], {})
  t.is(third.db, dbPort, 'the added service is bound to its port')
  const dbSocket = await connect(dbPort)
  t.teardown(() => dbSocket.destroy())
}))

test('bind 0.0.0.0 listens on all interfaces', catchThrows(async (t) => {
  const [hint] = await freePorts(1)
  const listeners = new Listeners({ open: recordingOpen([]), bind: '0.0.0.0' })
  t.teardown(() => listeners.close())
  const ports = await listeners.set([{ name: 'web', kind: 'tcp', port: hint }], {})
  const bound = listeners.address('web')
  t.is(bound && bound.address, '0.0.0.0', 'the listener is bound to all interfaces')
  t.is(bound && bound.port, ports.web, 'the bound address has the reported port')
  const socket = await connect(ports.web)
  t.teardown(() => socket.destroy())
}))

// The app closes one connection while the host still sends on it. The listener's next write then fails
// because the peer reset the connection, so its socket errors. That socket and its stream are destroyed,
// and the other connections keep working.
test('a local socket that errors (peer reset) is cleaned up and every other connection keeps working', catchThrows(async (t) => {
  const calls = []
  const listeners = new Listeners({ open: recordingOpen(calls), bind: '127.0.0.1' })
  t.teardown(() => listeners.close())
  // A socket error may also reach the error event. This test does not check it, but must not crash on it.
  listeners.on('error', noop)
  const ports = await listeners.set([{ name: 'web', kind: 'tcp', port: 0 }], {})

  const reset = await connect(ports.web)
  t.teardown(() => reset.destroy())
  await waitFor(() => calls.length === 1, 'open for the reset connection')
  const keep = await connect(ports.web)
  t.teardown(() => keep.destroy())
  await waitFor(() => calls.length === 2, 'open for the other connection')

  const resetStreamClosed = new Promise((resolve) => calls[0].stream.once('close', resolve))
  reset.destroy()
  calls[0].fromHost.write(b4a.alloc(8 * MIB, 0x61))
  await within(resetStreamClosed, 'the stream of the reset connection to be destroyed')

  const toHost = collect(calls[1].toHost, 4)
  keep.write('ping')
  t.is(await toHost, 'ping', 'the other connection still reaches the host')
  const toApp = collect(keep, 4)
  calls[1].fromHost.write('pong')
  t.is(await toApp, 'pong', 'the host still reaches the other connection')

  const again = await connect(ports.web)
  t.teardown(() => again.destroy())
  await waitFor(() => calls.length === 3, 'open for a new connection after the reset')
}))
