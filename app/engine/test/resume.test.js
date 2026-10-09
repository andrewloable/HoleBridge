// Stream resume on the app side: tests for lib/mux.js (Session.enableResume, detach and adopt) and for lib/sessions.js
// (the 60 s grace period, and the new session that takes the open streams over). The rules are in
// docs/architecture.md, "Streams survive a reconnect and a route change (MVP)" and "Sessions and reconnects". The wire
// is protocol v1: opened carries the 16-byte token, reattach is message 6 and reattached message 7. The mux tests run
// their frames through the codec in lib/protocol.js, as test/mux-close.test.js does. Cases 1, 3, 5 and 6 have a Go
// counterpart in internal/mux/resume_test.go (TestTransferSurvivesADropAndReattach,
// TestAppDropsDataBeforeReattached, TestKeptBytesBeyondPerStreamCapCloseTheStream and
// TestWithoutResumeOpenedHasZeroTokenAndDetachCloses) and must agree with it. Cases 2 and 4 are the sessions layer's
// grace period, and cases 7 and 8 its reconnect and route handover. The Go tests cover none of those.
//
// Shapes the IMPL must meet:
// - Session.enableResume() (lib/mux.js): an app session keeps the bytes its streams send until the peer acknowledges
//   them. The connection calls it when the handshake carries FLAG.resume, before the session opens streams. The fake
//   host below is a host-role script, so the host side needs no resume here.
// - Session.detach() (lib/mux.js): the transport died. A resumable session keeps its streams: they stall, their Stream
//   objects stay open (so their local sockets do too) and they wait for adopt. A session without resume closes its
//   streams at once.
// - Session.adopt(prev) (lib/mux.js): a new session takes over the streams prev detached. Each one is sent
//   reattach {stream, token, received, limit}, and the new session ignores its data and window until reattached
//   arrives. The Stream objects stay the same.
// - Sessions (lib/sessions.js) calls newHostSession.adopt(oldHostSession) when a route event hands the host to a new
//   session, and when a new session comes up within 60 s of a drop. When no new session comes within 60 s, the old
//   streams close. The 60 s runs on the clock Sessions was given.
// - The HostSession of lib/connect.js gets the same two seams: adopt(prev) delegates to its mux session, and a
//   transport that dies detaches that mux session before it emits 'close'. fakeHostSession below has that shape.
//
// Until the IMPL task (HoleBridge-9gi.2) lands, the stubs throw 'not implemented', and each test fails on its first
// call to one of them. Bodies run under catchThrows, so one failing test does not end the run.
const test = require('brittle')
const b4a = require('b4a')
const c = require('compact-encoding')
const EventEmitter = require('bare-events')
const { messages, FLAG } = require('../lib/protocol.js')
const { Session, Budget } = require('../lib/mux.js')
const { Sessions } = require('../lib/sessions.js')

const MIB = 1024 * 1024
const WINDOW = 2 * MIB // a stream's window (docs/architecture.md, Flow control)
const KEEP_PER_STREAM = 4 * MIB // the bytes one stream may keep for resending (Limits)
const GRACE = 60 * 1000 // how long a dropped stream waits for its reattach
const SETTLE = 10000 // bounds every wait, so a stalled test fails instead of hanging
const TOKEN = b4a.alloc(16, 7) // a nonzero resume token, as a host issues

// catchThrows runs a test body so that a throw fails only that test. Until the IMPL lands, the stubs throw
// 'not implemented', and brittle would otherwise end the whole run.
function catchThrows(fn) {
  return async (t) => {
    try {
      await fn(t)
    } catch (err) {
      t.fail(err.message)
    }
  }
}

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
    if (Date.now() > deadline) throw new Error(`${what} was not reached within ${SETTLE} ms`)
    await delay(5)
  }
}

// watch(promise) -> { done, value, error }: how promise settles, filled in as it does. Every promise a test can see
// reject goes through watch, so no rejection is left unhandled.
function watch(promise) {
  const out = { done: false, value: undefined, error: undefined }
  promise.then(
    (value) => Object.assign(out, { done: true, value }),
    (error) => Object.assign(out, { done: true, error })
  )
  return out
}

// collect(emitter, name) listens for name and returns the list the payloads go into.
function collect(emitter, name) {
  const payloads = []
  emitter.on(name, (payload) => payloads.push(payload))
  return payloads
}

// pattern returns n bytes that differ from their neighbours, so a dropped or repeated byte shows.
function pattern(n) {
  const b = b4a.alloc(n)
  for (let i = 0; i < n; i++) b[i] = i % 251
  return b
}

// copyOf returns b in a buffer of its own: a decoded payload is a view into its frame.
function copyOf(b) {
  const out = b4a.alloc(b.length)
  out.set(b)
  return out
}

// framesOf returns the messages sent with index among sent, which is a list of { index, message }.
function framesOf(sent, index) {
  return sent.filter((s) => s.index === index).map((s) => s.message)
}

// dataBytes is the payload bytes of the data messages among sent.
function dataBytes(sent) {
  return framesOf(sent, 3).reduce((n, m) => n + m.payload.length, 0)
}

// direction is one side's sending path, as in test/mux-close.test.js: each message is recorded in sent, encoded with
// the codec and decoded again before target receives it. cut() ends the path: what is sent from then on, or still in
// flight, is lost, as a dropped transport loses it.
function direction() {
  const d = { sent: [], errors: [], target: null, queue: [], running: false, dead: false }
  d.send = (index, message) => {
    if (d.dead) return
    const frame = c.encode(messages[index], message)
    d.sent.push({ index, message })
    d.queue.push({ index, frame })
    drain(d)
  }
  d.cut = () => {
    d.dead = true
    d.queue.length = 0
  }
  return d
}

function drain(d) {
  if (d.running || !d.target) return
  d.running = true
  queueMicrotask(() => {
    while (d.queue.length > 0) {
      const { index, frame } = d.queue.shift()
      try {
        d.target.receive(index, c.decode(messages[index], frame))
      } catch (err) {
        d.errors.push(err)
      }
    }
    d.running = false
  })
}

// appSession is one app session whose sent messages are recorded in out, with nothing on the other side. The test
// plays the host by calling session.receive itself.
function appSession() {
  const out = direction()
  const session = new Session({ role: 'app', send: out.send, budget: new Budget(64 * MIB) })
  return { session, out }
}

// connection joins an app session to the fake host over two paths: up carries the app's messages to the host, down
// the host's to the app. The host answers the app it is talking to through host.reply, which is this connection's
// down path. cut() ends both paths.
function connection(host) {
  const up = direction()
  const down = direction()
  const app = new Session({ role: 'app', send: up.send, budget: new Budget(64 * MIB) })
  up.target = host
  down.target = app
  host.reply = down.send
  return {
    app,
    up,
    down,
    cut() {
      up.cut()
      down.cut()
    }
  }
}

// fakeHost plays the host's side of protocol v1 for one app session, the way the Go host answers it. open gets opened
// with a nonzero token and a 2 MiB window. Every data message is taken at once and granted back as window credit,
// with the total received. reattach gets reattached with the total received and the limit, or close when the token
// is not the one issued. It keeps the bytes it takes, so a test can compare them with what was sent.
function fakeHost() {
  const host = {
    token: TOKEN,
    received: 0, // payload bytes taken so far
    limit: WINDOW, // the most the app may send in all: the window plus the credit granted
    chunks: [],
    reattaches: [], // the reattach messages it got
    reply: null,
    receive(index, m) {
      if (index === 0) {
        host.reply(1, { stream: m.stream, window: WINDOW, token: host.token })
      } else if (index === 3) {
        host.chunks.push(copyOf(m.payload))
        host.received += m.payload.length
        host.limit += m.payload.length
        host.reply(4, { stream: m.stream, credit: m.payload.length, received: host.received })
      } else if (index === 6) {
        host.reattaches.push(m)
        if (!b4a.equals(m.token, host.token)) host.reply(5, { stream: m.stream })
        else host.reply(7, { stream: m.stream, received: host.received, limit: host.limit })
      }
    },
    bytes: () => b4a.concat(host.chunks)
  }
  return host
}

// watchStream records a stream's close and its errors from the moment it is taken, so an error it emits is not lost.
function watchStream(stream) {
  const seen = { closed: false, errors: [] }
  stream.on('error', (err) => seen.errors.push(err))
  stream.on('close', () => {
    seen.closed = true
  })
  return seen
}

// writeAll writes data to stream and resolves once the stream has taken all of it.
function writeAll(stream, data) {
  return new Promise((resolve, reject) => {
    stream.write(data, (err) => (err ? reject(err) : resolve()))
  })
}

// readBytes takes bytes from stream as they arrive until n are read, then pauses the stream.
function readBytes(stream, n) {
  return new Promise((resolve, reject) => {
    const parts = []
    let total = 0
    stream.on('error', reject)
    stream.on('data', (chunk) => {
      if (total >= n) return
      parts.push(chunk)
      total += chunk.length
      if (total >= n) {
        stream.pause()
        resolve(b4a.concat(parts))
      }
    })
  })
}

// fakeClock() -> { timers, now(), advance(ms) }, as in test/sessions.test.js. Timers fire in virtual time order, and
// advance(ms) lets the promise chains settle after each one.
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

// deferred() -> { promise, resolve, reject }: a promise the test settles when it chooses.
function deferred() {
  let resolve
  let reject
  const promise = new Promise((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}

// fakeRouteManager() is the RouteManager a host gets, as lib/routes.js hands it out, as in test/sessions.test.js.
// found(session) brings a route up: it emits 'route' with the state and the session, and settles connect(). When the
// live session closes, the manager emits 'route' 'looking'. close() destroys the live session without a route event.
function fakeRouteManager() {
  const manager = new EventEmitter()
  const dial = deferred()
  dial.promise.catch(() => {})
  let current = null
  manager.connectCalls = 0
  manager.closeCalls = 0
  manager.lastError = null
  manager.connect = () => {
    manager.connectCalls++
    return dial.promise
  }
  manager.found = (session) => {
    current = session
    session.on('close', () => {
      if (current !== session) return
      current = null
      manager.emit('route', 'looking')
    })
    manager.emit('route', session.route, session)
    dial.resolve(session)
  }
  manager.close = () => {
    manager.closeCalls++
    const session = current
    current = null
    if (session) session.destroy()
  }
  return manager
}

// fakeHostSession({ route, lan }) is a HostSession as lib/connect.js makes it once the handshake is in. Its streams
// belong to a mux app session (lib/mux.js), as in the real one, so a stream a test gets is a mux stream. The session
// answers each open the way the host does (opened, with a nonzero token), so open() resolves. drop() ends the
// transport the way lib/connect.js does: its mux session detaches first, then the close listeners run. destroy() ends
// the session the way close() does. adopt(prev) hands prev's streams to this session. sent lists the messages the app
// sent on it, and tokenOf(id) is the token it issued for stream id.
function fakeHostSession({ route = 'direct', lan = null } = {}) {
  const listeners = []
  const issued = new Map()
  const session = { route, lan, services: [], flags: FLAG.resume, sent: [], destroyCount: 0 }
  const mux = new Session({
    role: 'app',
    send: (index, message) => {
      session.sent.push({ index, message })
      if (index === 0) {
        const token = b4a.alloc(16, issued.size + 1)
        issued.set(message.stream, token)
        queueMicrotask(() => mux.receive(1, { stream: message.stream, window: WINDOW, token }))
      }
    },
    budget: new Budget(64 * MIB)
  })
  let closed = false
  const end = () => {
    if (closed) return
    closed = true
    for (const listener of listeners) listener()
  }
  Object.assign(session, {
    mux,
    open: (service) => mux.open(service),
    on(event, listener) {
      if (event !== 'close') throw new Error(`HostSession emits close only, not ${event}`)
      listeners.push(listener)
    },
    destroy() {
      session.destroyCount++
      mux.destroy()
      end()
    },
    drop() {
      mux.detach()
      end()
    },
    adopt(prev) {
      mux.adopt(prev.mux)
    },
    tokenOf: (id) => issued.get(id)
  })
  return session
}

// upWith brings the host's route up on session, through ensure(), and waits until ensure() has resolved with it.
async function upWith(clock, sessions, home, session) {
  const ensured = watch(sessions.ensure('home'))
  home.found(session)
  await clock.advance(0)
  return ensured
}

// openStream opens a stream on the host through sessions, with a session already up, and returns it.
async function openStream(clock, sessions, service) {
  const opened = watch(sessions.open('home', service))
  await clock.advance(0)
  if (opened.error) throw opened.error
  return opened.value
}

// Case 1: the app sends 20 MB to the fake host. The connection drops once the host has taken 5 MB, and the app
// reattaches its stream on a new session. The 20 MB arrives at the host byte-exact, the opened token is nonzero, and
// the reattach carries the issued token, what the app received (none) and its 2 MiB window.
test('a 20 MB transfer across a forced drop arrives byte-exact', catchThrows(async (t) => {
  const host = fakeHost()
  const first = connection(host)
  first.app.enableResume()
  const stream = await within(first.app.open('web'), 'open()')
  const seen = watchStream(stream)
  const opened = framesOf(first.down.sent, 1)[0]
  t.ok(!b4a.equals(opened.token, b4a.alloc(16)), 'opened carries a nonzero token with resume on')

  const want = pattern(20 * MIB)
  const written = watch(writeAll(stream, want))
  await waitFor(() => host.received >= 5 * MIB, 'the host takes 5 MB')
  first.cut()
  first.app.detach()

  const second = connection(host)
  second.app.enableResume()
  second.app.adopt(first.app)
  await waitFor(() => host.received === want.length && written.done, 'the host takes all 20 MB and the write ends')

  t.is(written.error, undefined, 'the write ends without error')
  t.ok(b4a.equals(host.bytes(), want), 'the 20 MB arrive byte-exact across the drop')
  t.is(host.reattaches.length, 1, 'one reattach, for the one stream')
  const [reattach] = host.reattaches
  t.ok(b4a.equals(reattach.token, host.token), 'the reattach carries the issued token')
  t.is(reattach.received, 0, 'the app has received nothing from the host')
  t.is(reattach.limit, WINDOW, 'the reattach limit is the 2 MiB window')
  t.alike(seen.errors, [], 'the stream never fails')
  t.is(seen.closed, false, 'the stream never closes')
}))

// Case 2: after a session drop, the stream stays open for the whole grace period. Its local socket is open too, since
// lib/listeners.js closes a socket only when its stream closes. Checked 59 s into the stall, on the fake clock.
test('the local socket stays open during the stall', catchThrows(async (t) => {
  const clock = fakeClock()
  const home = fakeRouteManager()
  const sessions = new Sessions({ routeManagerFor: () => home, clock: clock.timers })
  const first = fakeHostSession({ route: 'direct' })
  await upWith(clock, sessions, home, first)
  const stream = await openStream(clock, sessions, 'ssh')
  const seen = watchStream(stream)

  first.drop() // the path dies: the stream stalls
  await clock.advance(GRACE - 1000)
  await delay(0)
  t.is(seen.closed, false, 'the stream is still open 59 s into the stall')
  t.is(stream.destroyed, false, 'its local socket stays open')
  t.alike(seen.errors, [], 'and nothing has failed')
}))

// Case 3: the app's new session holds the stream but has not been reattached yet. Data and window that arrive on it
// before reattached are dropped, so the reader gets only the data sent after reattached. The reattach carries the
// issued token, the bytes the app received (none) and its 2 MiB window. After reattached, the app sends no more than
// the reattached limit.
test('data and window that arrive before reattached are dropped', catchThrows(async (t) => {
  const first = appSession()
  first.session.enableResume()
  const opening = within(first.session.open('web'), 'open()')
  first.session.receive(1, { stream: 1, window: WINDOW, token: TOKEN })
  const stream = await opening
  first.session.detach()

  // The new session takes the stream over. The test plays the host, which has not answered the reattach yet.
  const second = appSession()
  second.session.enableResume()
  second.session.adopt(first.session)
  const [reattach] = framesOf(second.out.sent, 6)
  t.ok(reattach && reattach.stream === 1, 'adopt sends a reattach for the stream')
  t.ok(reattach && b4a.equals(reattach.token, TOKEN), 'the reattach carries the issued token')
  t.is(reattach && reattach.received, 0, 'the reattach says the app received nothing')
  t.is(reattach && reattach.limit, WINDOW, 'the reattach limit is the 2 MiB window')

  // Stale data and a stale window arrive before reattached.
  second.session.receive(3, { stream: 1, payload: b4a.from('stale') })
  second.session.receive(4, { stream: 1, credit: MIB, received: 0 })
  second.session.receive(7, { stream: 1, received: 0, limit: WINDOW })
  second.session.receive(3, { stream: 1, payload: b4a.from('fresh') })
  const got = await within(readBytes(stream, 5), 'the read after reattached')
  t.is(got.toString(), 'fresh', 'the reader gets only the data sent after reattached')

  // The stale window granted 1 MiB, but the reattached limit is 2 MiB: a 3 MiB write sends 2 MiB and then waits.
  watch(new Promise((resolve) => stream.write(pattern(3 * MIB), (err) => resolve(err))))
  await delay(10)
  t.is(dataBytes(second.out.sent), 2 * MIB, 'after reattached the app sends no more than the 2 MiB limit')
}))

// Case 4: no new session comes within 60 s of the drop, on the fake clock. The local sockets close then, not before.
test('no new session within 60 s closes the local sockets', catchThrows(async (t) => {
  const clock = fakeClock()
  const home = fakeRouteManager()
  const sessions = new Sessions({ routeManagerFor: () => home, clock: clock.timers })
  const first = fakeHostSession({ route: 'direct' })
  await upWith(clock, sessions, home, first)
  const stream = await openStream(clock, sessions, 'ssh')
  const seen = watchStream(stream)

  first.drop()
  await clock.advance(GRACE - 1000)
  await delay(0)
  t.is(seen.closed, false, 'the stream is open 1 s before the grace period ends')
  await clock.advance(1000)
  await delay(0)
  t.ok(seen.closed, 'with no new session by 60 s, the stream closes and its socket with it')
}))

// Case 5: a peer that grants credit and never acknowledges what it is sent makes the app keep more than 4 MiB on one
// stream. Here the peer grants 8 MiB and acknowledges nothing, and the app writes 5 MiB. The write fails, close is sent
// for the stream, and no more than 4 MiB of its bytes leave the app.
test('kept bytes over 4 MiB on a stream close it', catchThrows(async (t) => {
  const app = appSession()
  app.session.enableResume()
  const opening = within(app.session.open('web'), 'open()')
  app.session.receive(1, { stream: 1, window: WINDOW, token: TOKEN })
  const stream = await opening
  app.session.receive(4, { stream: 1, credit: 8 * MIB, received: 0 })

  const wrote = watch(new Promise((resolve) => stream.write(pattern(5 * MIB), (err) => resolve(err))))
  await waitFor(() => wrote.done, 'the 5 MiB write ends')
  t.ok(wrote.value, 'a write that keeps more than 4 MiB unacknowledged fails')
  t.ok(framesOf(app.out.sent, 5).some((m) => m.stream === 1), 'close is sent for the stream')
  t.ok(dataBytes(app.out.sent) <= KEEP_PER_STREAM, 'no more than 4 MiB of its bytes leave the app')
}))

// Case 6: a session without resume closes its streams at once when its transport dies: opened carries a zero token
// (the host sends none without FlagResume), and a Write after the drop fails. No grace period runs.
test('a host without FlagResume: a drop closes its streams at once', catchThrows(async (t) => {
  const app = appSession()
  const opening = within(app.session.open('web'), 'open()')
  app.session.receive(1, { stream: 1, window: WINDOW, token: b4a.alloc(16) })
  const stream = await opening
  const seen = watchStream(stream)

  app.session.detach()
  await delay(0)
  t.ok(seen.closed, 'the stream closes at once: there is no grace period without resume')
  t.ok(stream.destroyed, 'the stream is destroyed')

  const wrote = watch(new Promise((resolve) => stream.write(b4a.from('x'), (err) => resolve(err))))
  await delay(0)
  t.ok(wrote.value, 'a write after the drop fails')
}))

// Case 7 (the sessions layer's reconnect): the transport drops and the route comes back on a new session within 60 s.
// The new session takes the open stream over: it sends reattach for it with the token the old host issued, and the
// stream stays open. Nothing the grace timer runs later closes it.
test('a reconnect within 60 s reattaches the open streams on the new session', catchThrows(async (t) => {
  const clock = fakeClock()
  const home = fakeRouteManager()
  const sessions = new Sessions({ routeManagerFor: () => home, clock: clock.timers })
  const sessionEvents = collect(sessions, 'session')
  const first = fakeHostSession({ route: 'direct' })
  await upWith(clock, sessions, home, first)
  const stream = await openStream(clock, sessions, 'ssh')
  const seen = watchStream(stream)

  first.drop()
  await clock.advance(10 * 1000)
  const second = fakeHostSession({ route: 'relay' })
  home.found(second)
  await clock.advance(0)

  t.alike(sessionEvents.map((e) => e.up), [true, false, true], 'up, down, then up on the new session')
  const reattaches = framesOf(second.sent, 6)
  t.is(reattaches.length, 1, 'the new session sends one reattach, for the one stream')
  t.is(reattaches[0] && reattaches[0].stream, stream.id, 'for that stream')
  t.ok(reattaches[0] && b4a.equals(reattaches[0].token, first.tokenOf(stream.id)), 'with the token the old host issued')
  await clock.advance(GRACE)
  await delay(0)
  t.alike(seen.errors, [], 'the adopted stream does not fail')
  t.is(seen.closed, false, 'and the grace period does not close it')
}))

// Case 8 (the sessions layer's route handover): a route event brings a new session while the old one is still up.
// The open streams move to the new session, which sends reattach for each, and the old session's end closes nothing.
test('a route change hands the open streams to the new session', catchThrows(async (t) => {
  const clock = fakeClock()
  const home = fakeRouteManager()
  const sessions = new Sessions({ routeManagerFor: () => home, clock: clock.timers })
  const direct = fakeHostSession({ route: 'direct' })
  await upWith(clock, sessions, home, direct)
  const stream = await openStream(clock, sessions, 'ssh')
  const seen = watchStream(stream)

  const lan = fakeHostSession({ route: 'lan' })
  home.found(lan) // the LAN route is found first ...
  direct.drop() // ... and then the old session ends
  await clock.advance(0)

  const reattaches = framesOf(lan.sent, 6)
  t.is(reattaches.length, 1, 'the new session sends one reattach, for the one stream')
  t.is(reattaches[0] && reattaches[0].stream, stream.id, 'for that stream')
  t.ok(reattaches[0] && b4a.equals(reattaches[0].token, direct.tokenOf(stream.id)), 'with the token the old host issued')
  await clock.advance(GRACE)
  await delay(0)
  t.alike(seen.errors, [], 'the stream does not fail across the handover')
  t.is(seen.closed, false, 'and nothing closes it later')
}))
