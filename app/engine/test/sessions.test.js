// Tests for on-demand sessions per host (lib/sessions.js). Rules: docs/architecture.md, "Sessions and reconnects"
// (on demand, idle rule, wake-on-connect) and decision D17. Each test drives a Sessions object with fake RouteManagers
// (one per host, as lib/routes.js hands them out), fake HostSessions and fake Streams, and a fake clock (fakeClock
// below). Time moves only when a test advances it, so the tests run in virtual time and never wait on a real timer or
// a real network.
//
// Shapes the IMPL must meet (also stated in lib/sessions.js):
// - new Sessions({ routeManagerFor, clock, idleMs, wakeWaitMs }) is an EventEmitter. idleMs defaults to 5 min and
//   wakeWaitMs to 30 s; these tests use the defaults.
// - routeManagerFor(host) -> the RouteManager for that host: connect() -> Promise<HostSession>, lastError, close(),
//   and 'route' events emitted as (state, session).
// - clock is { setTimeout(fn, ms), clearTimeout(id) }. The idle rule and the wake wait run through it.
// - ensure(host) -> Promise<HostSession>: reuses a live session or starts one.
// - open(host, service) -> Promise<Stream>: with no session, starts one and waits at most 30 s for it.
// - emit('session', { host, up }) when a session comes up or goes down.
// - emit('lan', { host, addresses, port }) when a handshake carries the host's LAN block.
// - A HostSession has route, lan, open(service) -> Promise<Stream>, on('close', fn) and destroy(). A Stream has
//   on('close', fn) and destroy().
// - routeManagerFor(host) is called each time a host with no live manager gets a session; a manager closed by the
//   idle rule is never reused.
// - The idle rule ends a session with manager.close(), not session.destroy(): a bare destroy is a drop and the
//   manager would reconnect.
// - Sessions takes its sessions from the manager's 'route' events: connect() resolves once, with the first session
//   only.
// - ensure() rejects with the error of the first failed search (the manager keeps searching); open() fails after
//   wakeWaitMs with lastError.code when lastError has an HB code, else HB-LOOKUP-TIMEOUT.
//
// Not covered here: keepalives ("on for every session", docs/architecture.md, Idle). Sessions is handed finished
// HostSessions and builds no connection, so it has no seam for them; they belong to the connection layer
// (lib/connect.js, lib/lan-session.js). No test here pins them.
//
// Until the IMPL task (HoleBridge-hb5.6.2) lands, the constructor throws 'not implemented', so each test fails for
// that reason.
const test = require('brittle')
const EventEmitter = require('bare-events')
const { Sessions } = require('../lib/sessions.js')

const IDLE_LIMIT = 5 * 60 * 1000 // a session closes 5 min after its last stream closes
const WAKE_WAIT = 30 * 1000 // open() waits at most 30 s for a session
const LAN = { addresses: ['192.0.2.20', '192.0.2.21'], port: 8443 } // TEST-NET-1 (RFC 5737)

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

// hbError makes an error that carries an HB code, as lib/routes.js does.
function hbError(code, reason) {
  return Object.assign(new Error(code + ': ' + reason), { name: 'HbError', code, reason })
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

// fakeStream(service) is a Stream as lib/mux.js hands it out: a streamx Duplex, so an EventEmitter that supports on,
// once and off for any event. destroy() ends it and emits 'close', once.
function fakeStream(service) {
  const stream = new EventEmitter()
  stream.service = service
  let closed = false
  stream.destroy = () => {
    if (closed) return
    closed = true
    stream.emit('close')
  }
  return stream
}

// fakeSession({ route, lan }) is a HostSession as lib/connect.js makes it, for the fakes to hand out. As in the real
// one, destroy() ends the session and runs its close listeners, and drop() ends it from the path side. open(service)
// records the service in opened and resolves with a fake stream for it.
function fakeSession({ route = 'direct', lan = null } = {}) {
  const listeners = []
  let closed = false
  const end = () => {
    if (closed) return
    closed = true
    for (const listener of listeners) listener()
  }
  const session = {
    route,
    services: [],
    lan,
    flags: 0,
    opened: [], // the service of each open()
    destroyCount: 0,
    open(service) {
      session.opened.push(service)
      return Promise.resolve(fakeStream(service))
    },
    on(event, listener) {
      if (event !== 'close') throw new Error(`HostSession emits close only, not ${event}`)
      listeners.push(listener)
    },
    destroy() {
      session.destroyCount++
      end()
    },
    drop: end
  }
  return session
}

// fakeRouteManager() is the RouteManager a host gets, as lib/routes.js hands it out. found(session) brings a route up:
// it emits 'route' with the state and the session, and settles connect(). When the live session closes, the manager
// emits 'route' 'looking', as the real one does. close() destroys the live session without a route event, as the real
// close() does, and counts the call in closeCalls. connect() returns one promise for the life of the manager, as the
// real one does. fail(error) is a search that fails: it sets lastError and rejects connect() (the first failure ends
// the connect, as the real manager does; the promise has a catch here, so an unwatched failure is not an unhandled
// rejection). lastError is null until then, as it is while a search is still running.
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
  manager.fail = (error) => {
    manager.lastError = error
    dial.reject(error)
  }
  manager.close = () => {
    manager.closeCalls++
    const session = current
    current = null
    if (session) session.destroy()
  }
  return manager
}

test('ensure() twice for one host starts one session', catchThrows(async (t) => {
  const clock = fakeClock()
  const home = fakeRouteManager()
  const sessions = new Sessions({ routeManagerFor: () => home, clock: clock.timers })
  const first = watch(sessions.ensure('home'))
  const second = watch(sessions.ensure('home'))
  await clock.advance(0)
  t.is(home.connectCalls, 1, 'two ensure() calls before the session is up start one connect')
  const live = fakeSession()
  home.found(live)
  await clock.advance(0)
  t.is(first.value, live, 'the first ensure() resolves with the session')
  t.is(second.value, live, 'the second ensure() resolves with the same session')
  const third = watch(sessions.ensure('home'))
  await clock.advance(0)
  t.is(third.value, live, 'an ensure() on a live session reuses it')
  t.is(home.connectCalls, 1, 'still one connect after the session is up')
}))

test('open() with no session starts one and resolves when it is up', catchThrows(async (t) => {
  const clock = fakeClock()
  const home = fakeRouteManager()
  const sessions = new Sessions({ routeManagerFor: () => home, clock: clock.timers })
  const opened = watch(sessions.open('home', 'ssh'))
  await clock.advance(0)
  t.is(home.connectCalls, 1, 'open() starts one session')
  t.is(opened.done, false, 'open() waits while no session is up')
  const live = fakeSession()
  home.found(live)
  await clock.advance(0)
  t.is(opened.done, true, 'open() settles once the session is up')
  t.is(opened.error, undefined, 'open() resolves, it does not fail')
  t.is(opened.value && opened.value.service, 'ssh', 'the stream is for the service asked for')
  t.alike(live.opened, ['ssh'], 'the stream is opened on the session')
}))

test('open() fails after 30 s with HB-LOOKUP-TIMEOUT when no session forms', catchThrows(async (t) => {
  const clock = fakeClock()
  const home = fakeRouteManager()
  const sessions = new Sessions({ routeManagerFor: () => home, clock: clock.timers })
  const opened = watch(sessions.open('home', 'ssh'))
  await clock.advance(WAKE_WAIT - 1)
  t.is(opened.done, false, 'still waiting 1 ms before 30 s')
  await clock.advance(1)
  t.is(opened.done, true, 'settles at 30 s')
  t.is(opened.value, undefined, 'it does not resolve without a session')
  t.ok(opened.error, 'open() rejects')
  t.is(opened.error && opened.error.code, 'HB-LOOKUP-TIMEOUT', 'it fails with HB-LOOKUP-TIMEOUT')
}))

test('a session with no streams closes 5 minutes after the last stream closed, not before', catchThrows(async (t) => {
  const clock = fakeClock()
  const home = fakeRouteManager()
  const sessions = new Sessions({ routeManagerFor: () => home, clock: clock.timers })
  const sessionEvents = collect(sessions, 'session')
  const opened = watch(sessions.open('home', 'ssh'))
  const live = fakeSession()
  home.found(live)
  await clock.advance(0)
  const stream = opened.value
  await clock.advance(60 * 1000) // the stream is open for a minute
  stream.destroy() // the last stream closes at 1 min
  await clock.advance(IDLE_LIMIT - 1)
  t.is(live.destroyCount, 0, 'still up 1 ms before 5 min after the last stream closed')
  t.alike(sessionEvents, [{ host: 'home', up: true }], 'no down event before then')
  await clock.advance(1)
  t.is(live.destroyCount, 1, 'closed at 5 min after the last stream closed')
  t.alike(sessionEvents, [{ host: 'home', up: true }, { host: 'home', up: false }], 'the session goes down then')
}))

test('an open idle stream keeps the session past 5 minutes', catchThrows(async (t) => {
  const clock = fakeClock()
  const home = fakeRouteManager()
  const sessions = new Sessions({ routeManagerFor: () => home, clock: clock.timers })
  const sessionEvents = collect(sessions, 'session')
  const opened = watch(sessions.open('home', 'ssh'))
  const live = fakeSession()
  home.found(live)
  await clock.advance(0)
  const stream = opened.value
  await clock.advance(10 * 60 * 1000) // quiet for 10 min, the stream still open
  t.is(live.destroyCount, 0, 'the session stays up past 5 min while the stream is open')
  t.alike(sessionEvents, [{ host: 'home', up: true }], 'no down event while the stream is open')
  stream.destroy() // the last stream closes at 10 min
  await clock.advance(IDLE_LIMIT - 1)
  t.is(live.destroyCount, 0, 'the 5 min run from the stream closing, not from opening')
  await clock.advance(1)
  t.is(live.destroyCount, 1, 'closed 5 min after the stream closed')
}))

test("the handshake's lan field produces a 'lan' event", catchThrows(async (t) => {
  const clock = fakeClock()
  const managers = { home: fakeRouteManager(), office: fakeRouteManager() }
  const sessions = new Sessions({ routeManagerFor: (host) => managers[host], clock: clock.timers })
  const lanEvents = collect(sessions, 'lan')
  const ensuredHome = watch(sessions.ensure('home'))
  managers.home.found(fakeSession({ route: 'direct', lan: { addresses: LAN.addresses, port: LAN.port } }))
  await clock.advance(0)
  t.is(ensuredHome.done, true, 'the session for home is up')
  t.alike(
    lanEvents,
    [{ host: 'home', addresses: LAN.addresses, port: LAN.port }],
    "one 'lan' event carries the host's addresses and port"
  )
  watch(sessions.ensure('office'))
  managers.office.found(fakeSession({ route: 'direct', lan: null }))
  await clock.advance(0)
  t.is(lanEvents.length, 1, 'a handshake with no lan block gives no lan event')
}))

test("session up and down produce 'session' events", catchThrows(async (t) => {
  const clock = fakeClock()
  const home = fakeRouteManager()
  const sessions = new Sessions({ routeManagerFor: () => home, clock: clock.timers })
  const sessionEvents = collect(sessions, 'session')
  const ensured = watch(sessions.ensure('home'))
  const live = fakeSession()
  home.found(live)
  await clock.advance(0)
  t.is(ensured.done, true, 'the session is up')
  t.alike(sessionEvents, [{ host: 'home', up: true }], "the session coming up is a 'session' event with up true")
  live.drop()
  await clock.advance(0)
  t.alike(
    sessionEvents,
    [{ host: 'home', up: true }, { host: 'home', up: false }],
    "the session going down is a 'session' event with up false, once"
  )
}))

// ---- Behaviour the docs require, beyond the seven TEST CASES (added by the review of HoleBridge-hb5.6.1) ----

test('a session that no stream ever uses closes 5 minutes after it came up', catchThrows(async (t) => {
  // spec/ipc.md, decided point 2: connect opens the session, and a session no stream ever uses closes 5 min later.
  const clock = fakeClock()
  const home = fakeRouteManager()
  const sessions = new Sessions({ routeManagerFor: () => home, clock: clock.timers })
  const sessionEvents = collect(sessions, 'session')
  const ensured = watch(sessions.ensure('home'))
  const live = fakeSession()
  home.found(live)
  await clock.advance(0)
  t.is(ensured.value, live, 'ensure() resolves with the session, and no stream is ever opened on it')
  await clock.advance(IDLE_LIMIT - 1)
  t.is(live.destroyCount, 0, 'still up 1 ms before 5 min after it came up')
  t.is(home.closeCalls, 0, 'the manager has not been closed yet')
  await clock.advance(1)
  t.ok(live.destroyCount >= 1, 'closed 5 min after it came up')
  t.ok(home.closeCalls >= 1, "the idle close stops the host's manager with close(); a bare session.destroy() would make it reconnect")
  t.alike(sessionEvents, [{ host: 'home', up: true }, { host: 'home', up: false }], 'the session goes down then')
}))

test('the next open() after an idle close starts a new session on a new manager', catchThrows(async (t) => {
  // docs/architecture.md, "Choosing a route (app)": a closed manager cannot try again, so the sessions layer starts a
  // new one. lib/routes.js: connect() after close() rejects at once.
  const clock = fakeClock()
  const managers = []
  const routeManagerFor = () => {
    const manager = fakeRouteManager()
    managers.push(manager)
    return manager
  }
  const sessions = new Sessions({ routeManagerFor, clock: clock.timers })
  const sessionEvents = collect(sessions, 'session')
  const firstOpen = watch(sessions.open('home', 'ssh'))
  const first = fakeSession()
  managers[0].found(first)
  await clock.advance(0)
  firstOpen.value.destroy() // the only stream closes
  await clock.advance(IDLE_LIMIT)
  t.ok(first.destroyCount >= 1, 'the first session idled out')
  t.ok(managers[0].closeCalls >= 1, 'and its manager was closed')
  const secondOpen = watch(sessions.open('home', 'web'))
  await clock.advance(0)
  t.is(managers.length, 2, 'the next open() asks routeManagerFor for a new manager; it does not reuse the closed one')
  t.is(managers[1].connectCalls, 1, 'the new manager is started')
  t.is(secondOpen.done, false, 'open() waits for the new session')
  const second = fakeSession()
  managers[1].found(second)
  await clock.advance(0)
  t.is(secondOpen.value && secondOpen.value.service, 'web', 'the second open() resolves')
  t.alike(second.opened, ['web'], 'on the new session')
  t.alike(
    sessionEvents,
    [{ host: 'home', up: true }, { host: 'home', up: false }, { host: 'home', up: true }],
    'up, down, then up again'
  )
}))

test('with two streams open, closing one does not start the idle clock', catchThrows(async (t) => {
  // "While any stream is open ... the session stays up" (docs/architecture.md, Idle).
  const clock = fakeClock()
  const home = fakeRouteManager()
  const sessions = new Sessions({ routeManagerFor: () => home, clock: clock.timers })
  const ssh = watch(sessions.open('home', 'ssh'))
  const web = watch(sessions.open('home', 'web'))
  const live = fakeSession()
  home.found(live)
  await clock.advance(0)
  t.alike(live.opened, ['ssh', 'web'], 'both streams are open on the one session')
  await clock.advance(60 * 1000)
  ssh.value.destroy() // one of two streams closes at 1 min
  await clock.advance(2 * IDLE_LIMIT)
  t.is(live.destroyCount, 0, 'the other stream is still open, so the session stays up past 5 min after the first close')
  web.value.destroy() // the last stream closes
  await clock.advance(IDLE_LIMIT - 1)
  t.is(live.destroyCount, 0, 'still up 1 ms before 5 min after the last stream closed')
  await clock.advance(1)
  t.ok(live.destroyCount >= 1, 'closed 5 min after the last stream closed')
}))

test('a refused open() leaves no stream, so the session still idles out', catchThrows(async (t) => {
  const clock = fakeClock()
  const home = fakeRouteManager()
  const sessions = new Sessions({ routeManagerFor: () => home, clock: clock.timers })
  const live = fakeSession()
  const refusal = hbError('HB-UNKNOWN-SERVICE', 'the host does not share it')
  live.open = (service) => {
    live.opened.push(service)
    return Promise.reject(refusal)
  }
  const opened = watch(sessions.open('home', 'nope'))
  home.found(live)
  await clock.advance(0)
  t.is(opened.done, true, 'open() settles')
  t.is(opened.value, undefined, 'it does not resolve')
  t.is(opened.error && opened.error.code, 'HB-UNKNOWN-SERVICE', "the host's refusal reaches the caller with its code")
  await clock.advance(IDLE_LIMIT - 1)
  t.is(live.destroyCount, 0, 'still up 1 ms before 5 min')
  await clock.advance(1)
  t.ok(live.destroyCount >= 1, 'a refused open counts as no stream: the session closes at 5 min')
}))

test('open() fails with the code of the last failed search when it is not a timeout', catchThrows(async (t) => {
  // docs/architecture.md: "The error of the last failed search stays available until a search finds a route, so a
  // later open can fail with that code." The app must be able to say "update the app", not "no answer".
  const clock = fakeClock()
  const home = fakeRouteManager()
  const sessions = new Sessions({ routeManagerFor: () => home, clock: clock.timers })
  const opened = watch(sessions.open('home', 'ssh'))
  await clock.advance(0)
  home.fail(hbError('HB-VERSION-MISMATCH', 'this host speaks protocol v2'))
  await clock.advance(WAKE_WAIT)
  t.is(opened.done, true, 'open() has settled by 30 s')
  t.is(opened.value, undefined, 'it does not resolve without a session')
  t.is(opened.error && opened.error.code, 'HB-VERSION-MISMATCH', "it fails with the manager's lastError code, not HB-LOOKUP-TIMEOUT")
}))

test('ensure() rejects with the first failed search, and the host keeps searching', catchThrows(async (t) => {
  // spec/ipc.md, connect: the reply fails with HB-LOOKUP-TIMEOUT, and the host "stays registered and retries on its
  // own until close". docs/architecture.md: the first failed search ends the connect; the manager keeps searching.
  const clock = fakeClock()
  const home = fakeRouteManager()
  const sessions = new Sessions({ routeManagerFor: () => home, clock: clock.timers })
  const sessionEvents = collect(sessions, 'session')
  const ensured = watch(sessions.ensure('home'))
  await clock.advance(0)
  home.fail(hbError('HB-LOOKUP-TIMEOUT', 'no answer from the host within 60 s'))
  await clock.advance(0)
  t.is(ensured.done, true, 'ensure() settles when the first search fails')
  t.is(ensured.value, undefined, 'it does not resolve')
  t.is(ensured.error && ensured.error.code, 'HB-LOOKUP-TIMEOUT', 'with the search error code')
  t.is(home.closeCalls, 0, 'the manager is not closed: it retries on its own')
  const live = fakeSession()
  home.found(live) // the retry 30 s later finds the host
  await clock.advance(0)
  t.alike(sessionEvents, [{ host: 'home', up: true }], 'the session comes up after the failed connect')
  const again = watch(sessions.ensure('home'))
  await clock.advance(0)
  t.is(again.value, live, 'a later ensure() resolves with that session, not with the old failure')
}))

test('after a drop and a reconnect, ensure() and open() use the new session', catchThrows(async (t) => {
  // lib/routes.js: connect() returns one promise for the life of the manager, resolved with the FIRST session. The
  // reconnected session arrives only in a 'route' event.
  const clock = fakeClock()
  const home = fakeRouteManager()
  let made = 0
  const routeManagerFor = () => {
    made++
    return home
  }
  const sessions = new Sessions({ routeManagerFor, clock: clock.timers })
  const sessionEvents = collect(sessions, 'session')
  const first = fakeSession({ route: 'direct' })
  const ensured = watch(sessions.ensure('home'))
  home.found(first)
  await clock.advance(0)
  t.is(ensured.value, first, 'the first session is up')
  first.drop() // the path dies; the manager emits "looking" and reconnects by itself
  await clock.advance(0)
  t.alike(sessionEvents, [{ host: 'home', up: true }, { host: 'home', up: false }], 'the drop is a down event')
  const during = watch(sessions.ensure('home'))
  await clock.advance(1000)
  t.is(during.done, false, 'ensure() waits while the manager is looking: it does not return the dropped session')
  const second = fakeSession({ route: 'relay' })
  home.found(second)
  await clock.advance(0)
  t.is(during.value, second, 'ensure() resolves with the reconnected session')
  t.alike(
    sessionEvents,
    [{ host: 'home', up: true }, { host: 'home', up: false }, { host: 'home', up: true }],
    'up, down, then up again'
  )
  const stream = watch(sessions.open('home', 'ssh'))
  await clock.advance(0)
  t.is(stream.done, true, 'open() resolves')
  t.alike(second.opened, ['ssh'], 'the stream opens on the new session')
  t.alike(first.opened, [], 'and not on the dropped one')
  t.is(made, 1, 'one manager for the host handles the reconnect')
}))

test('a route change hands the host over to the new session', catchThrows(async (t) => {
  // docs/architecture.md, "Route events": the route event carries the session that holds the route, "which the
  // sessions layer uses ... after a reconnect or a LAN switch". lib/routes.js announces the new session first, then
  // destroys the old one, and that close is not a drop.
  const clock = fakeClock()
  const home = fakeRouteManager()
  const sessions = new Sessions({ routeManagerFor: () => home, clock: clock.timers })
  const sessionEvents = collect(sessions, 'session')
  const direct = fakeSession({ route: 'direct' })
  const ensured = watch(sessions.ensure('home'))
  home.found(direct)
  await clock.advance(0)
  t.is(ensured.value, direct, 'the direct session is up')
  const lan = fakeSession({ route: 'lan' })
  home.found(lan) // the LAN host was found: the new session is announced first ...
  direct.drop() // ... and then the old one is ended
  await clock.advance(0)
  const after = watch(sessions.ensure('home'))
  await clock.advance(0)
  t.is(after.value, lan, 'ensure() resolves with the session of the latest route event')
  t.is(sessionEvents[sessionEvents.length - 1].up, true, 'the host ends up with a session')
  t.is(lan.destroyCount, 0, 'ending the old session does not end the new one')
  const stream = watch(sessions.open('home', 'ssh'))
  await clock.advance(0)
  t.is(stream.done, true, 'open() resolves')
  t.alike(lan.opened, ['ssh'], 'a new stream opens on the new session')
  t.alike(direct.opened, [], 'and not on the old one')
}))

// ---- close(host) and the edge cases the IMPL decided (added by HoleBridge-hb5.6.2; see lib/sessions.js) ----

test('close(host) ends a live session without a drop, reports it down once, and stops the idle clock', catchThrows(async (t) => {
  const clock = fakeClock()
  const home = fakeRouteManager()
  const sessions = new Sessions({ routeManagerFor: () => home, clock: clock.timers })
  const sessionEvents = collect(sessions, 'session')
  const ensured = watch(sessions.ensure('home'))
  const live = fakeSession()
  home.found(live)
  await clock.advance(0)
  t.is(ensured.value, live, 'the session is up')
  sessions.close('home')
  await clock.advance(0)
  t.is(home.closeCalls, 1, 'the manager is closed with close()')
  t.is(live.destroyCount, 1, 'the session ends with it')
  t.alike(sessionEvents, [{ host: 'home', up: true }, { host: 'home', up: false }], 'the session is reported down once')
  await clock.advance(2 * IDLE_LIMIT)
  t.is(live.destroyCount, 1, 'the idle timer is stopped: nothing ends the session a second time')
  t.is(home.closeCalls, 1, 'and nothing closes the manager again')
  sessions.close('home')
  t.is(sessionEvents.length, 2, 'a second close(host) reports nothing more')
}))

test('close(host) fails a waiting ensure() and open() with a plain error that has no HB code', catchThrows(async (t) => {
  const clock = fakeClock()
  const home = fakeRouteManager()
  const sessions = new Sessions({ routeManagerFor: () => home, clock: clock.timers })
  const sessionEvents = collect(sessions, 'session')
  const ensured = watch(sessions.ensure('home'))
  const opened = watch(sessions.open('home', 'ssh'))
  await clock.advance(0)
  sessions.close('home')
  await clock.advance(0)
  t.is(ensured.done, true, 'the ensure() has settled')
  t.ok(ensured.error instanceof Error, 'ensure() rejects with an Error')
  t.is(ensured.error && ensured.error.code, undefined, 'with no HB code: the IPC layer picks the reply code')
  t.is(opened.done, true, 'the open() has settled before its 30 s wait')
  t.is(opened.error && opened.error.code, undefined, 'open() rejects with no HB code either')
  t.is(home.closeCalls, 1, 'the manager is closed')
  t.alike(sessionEvents, [], 'no session was up, so no event')
}))

test('close(host) for a host Sessions does not know does nothing', catchThrows(async (t) => {
  const sessions = new Sessions({ routeManagerFor: () => fakeRouteManager(), clock: fakeClock().timers })
  const sessionEvents = collect(sessions, 'session')
  sessions.close('nobody')
  t.alike(sessionEvents, [], 'no event, no error')
}))

test('the first call after close(host) starts a new manager', catchThrows(async (t) => {
  const clock = fakeClock()
  const managers = []
  const routeManagerFor = () => {
    const manager = fakeRouteManager()
    managers.push(manager)
    return manager
  }
  const sessions = new Sessions({ routeManagerFor, clock: clock.timers })
  const first = watch(sessions.ensure('home'))
  managers[0].found(fakeSession())
  await clock.advance(0)
  t.ok(first.value, 'the first session is up')
  sessions.close('home')
  const again = watch(sessions.ensure('home'))
  await clock.advance(0)
  t.is(managers.length, 2, 'a new manager, since the old one is closed for good')
  t.is(managers[1].connectCalls, 1, 'the new manager is started')
  t.is(again.done, false, 'ensure() waits for the new session')
}))

test('a registered search keeps looking, however long no session forms, until close(host)', catchThrows(async (t) => {
  const clock = fakeClock()
  const home = fakeRouteManager()
  const sessions = new Sessions({ routeManagerFor: () => home, clock: clock.timers })
  const ensured = watch(sessions.ensure('home'))
  await clock.advance(10 * IDLE_LIMIT)
  t.is(home.closeCalls, 0, 'the search is not released while the host is registered')
  t.is(ensured.done, false, 'ensure() still waits')
  home.found(fakeSession())
  await clock.advance(0)
  t.is(ensured.done, true, 'and the search finds the host when it answers')
}))

test('a search that open() started and that never forms a session ends 5 minutes after the wait', catchThrows(async (t) => {
  const clock = fakeClock()
  const home = fakeRouteManager()
  const sessions = new Sessions({ routeManagerFor: () => home, clock: clock.timers })
  const sessionEvents = collect(sessions, 'session')
  const opened = watch(sessions.open('home', 'ssh'))
  await clock.advance(WAKE_WAIT)
  t.is(opened.error && opened.error.code, 'HB-LOOKUP-TIMEOUT', 'open() failed at 30 s')
  await clock.advance(IDLE_LIMIT - 1)
  t.is(home.closeCalls, 0, 'the search runs on for the idle time, in case the host answers')
  await clock.advance(1)
  t.is(home.closeCalls, 1, 'then nobody waits and nothing registered: the search is closed')
  t.alike(sessionEvents, [], 'no session ever came up, so no event')
}))

test('a version mismatch stops the manager; the next call with nobody waiting starts a new one', catchThrows(async (t) => {
  const clock = fakeClock()
  const managers = []
  const routeManagerFor = () => {
    const manager = fakeRouteManager()
    managers.push(manager)
    return manager
  }
  const sessions = new Sessions({ routeManagerFor, clock: clock.timers })
  const first = watch(sessions.ensure('home'))
  await clock.advance(0)
  managers[0].fail(hbError('HB-VERSION-MISMATCH', 'this host speaks protocol v2'))
  await clock.advance(0)
  t.is(first.error && first.error.code, 'HB-VERSION-MISMATCH', 'the first ensure() fails with the code')
  const second = watch(sessions.ensure('home'))
  await clock.advance(0)
  t.is(managers.length, 2, 'the next ensure() starts a new manager: the stopped one never retries')
  t.is(managers[0].closeCalls, 1, 'the stopped manager is closed')
  const live = fakeSession()
  managers[1].found(live)
  await clock.advance(0)
  t.is(second.value, live, 'the new manager finds the host')
}))

test('a call made while another waits on a stopped manager joins that wait', catchThrows(async (t) => {
  const clock = fakeClock()
  const managers = []
  const routeManagerFor = () => {
    const manager = fakeRouteManager()
    managers.push(manager)
    return manager
  }
  const sessions = new Sessions({ routeManagerFor, clock: clock.timers })
  const first = watch(sessions.open('home', 'ssh'))
  await clock.advance(0)
  managers[0].fail(hbError('HB-VERSION-MISMATCH', 'this host speaks protocol v2'))
  await clock.advance(0)
  const second = watch(sessions.open('home', 'web'))
  await clock.advance(0)
  t.is(managers.length, 1, 'no new manager while a call waits on the stopped one')
  await clock.advance(WAKE_WAIT)
  t.is(first.error && first.error.code, 'HB-VERSION-MISMATCH', 'the first open() fails at its deadline with the code')
  t.is(second.error && second.error.code, 'HB-VERSION-MISMATCH', 'and so does the second, which joined it')
}))

test('an ensure() made during a reconnect fails with the next failed search', catchThrows(async (t) => {
  const clock = fakeClock()
  const home = fakeRouteManager()
  const sessions = new Sessions({ routeManagerFor: () => home, clock: clock.timers })
  const first = fakeSession()
  const ensured = watch(sessions.ensure('home'))
  home.found(first)
  await clock.advance(0)
  t.is(ensured.value, first, 'the first session is up')
  first.drop()
  await clock.advance(0)
  const during = watch(sessions.ensure('home'))
  await clock.advance(0)
  home.lastError = hbError('HB-LOOKUP-TIMEOUT', 'no answer from the host within 60 s')
  home.emit('route', 'unreachable') // the reconnect's search fails
  await clock.advance(0)
  t.is(during.error && during.error.code, 'HB-LOOKUP-TIMEOUT', 'the waiting ensure() fails with that search error')
  t.is(home.closeCalls, 0, 'the manager keeps retrying')
}))

test('an unreachable route event fails a waiting ensure() with the search error', catchThrows(async (t) => {
  // lib/routes.js reports a failed search that has no live route as 'unreachable', after it sets lastError.
  const clock = fakeClock()
  const home = fakeRouteManager()
  const sessions = new Sessions({ routeManagerFor: () => home, clock: clock.timers })
  const ensured = watch(sessions.ensure('home'))
  await clock.advance(0)
  home.lastError = hbError('HB-LOOKUP-TIMEOUT', 'no answer from the host within 60 s')
  home.emit('route', 'unreachable')
  await clock.advance(0)
  t.is(ensured.error && ensured.error.code, 'HB-LOOKUP-TIMEOUT', 'ensure() fails with the search error')
}))

test('a stream open across a route change keeps the new session up', catchThrows(async (t) => {
  // A stream belongs to the host, not to the route it started on: the handover does not end it.
  const clock = fakeClock()
  const home = fakeRouteManager()
  const sessions = new Sessions({ routeManagerFor: () => home, clock: clock.timers })
  const sessionEvents = collect(sessions, 'session')
  const opened = watch(sessions.open('home', 'ssh'))
  const direct = fakeSession({ route: 'direct' })
  home.found(direct)
  await clock.advance(0)
  const stream = opened.value
  const lan = fakeSession({ route: 'lan' })
  home.found(lan) // a LAN host shows up: the handover
  direct.drop()
  await clock.advance(10 * 60 * 1000)
  t.is(lan.destroyCount, 0, 'the stream is still open, so the new session stays up past 5 min')
  t.alike(sessionEvents, [{ host: 'home', up: true }], 'the handover is not a down event')
  stream.destroy()
  await clock.advance(IDLE_LIMIT - 1)
  t.is(lan.destroyCount, 0, 'still up 1 ms before 5 min after the stream closed')
  await clock.advance(1)
  t.is(lan.destroyCount, 1, 'then it idles out')
}))

test('an open() on a live session counts as a use while its stream opens', catchThrows(async (t) => {
  const clock = fakeClock()
  const home = fakeRouteManager()
  const sessions = new Sessions({ routeManagerFor: () => home, clock: clock.timers })
  const ensured = watch(sessions.ensure('home'))
  const live = fakeSession()
  home.found(live)
  await clock.advance(0)
  t.is(ensured.value, live, 'the session is up with no stream: its idle clock runs')
  await clock.advance(4 * 60 * 1000)
  const pending = deferred()
  live.open = (service) => {
    live.opened.push(service)
    return pending.promise
  }
  const opened = watch(sessions.open('home', 'ssh')) // at 4 min; the stream is still opening at 9 min
  await clock.advance(IDLE_LIMIT)
  t.is(live.destroyCount, 0, 'the caller waiting for its stream keeps the session past 5 min')
  pending.resolve(fakeStream('ssh'))
  await clock.advance(0)
  t.is(opened.value && opened.value.service, 'ssh', 'the stream opens')
  opened.value.destroy()
  await clock.advance(IDLE_LIMIT - 1)
  t.is(live.destroyCount, 0, 'the idle clock starts when the stream closes')
  await clock.advance(1)
  t.is(live.destroyCount, 1, 'closed 5 min after it')
}))

test('the manager announcing the live route again adds no session or lan event', catchThrows(async (t) => {
  // lib/routes.js re-emits the live route after an address change that finds nothing better: 'looking', then the same
  // session.
  const clock = fakeClock()
  const home = fakeRouteManager()
  const sessions = new Sessions({ routeManagerFor: () => home, clock: clock.timers })
  const sessionEvents = collect(sessions, 'session')
  const lanEvents = collect(sessions, 'lan')
  const ensured = watch(sessions.ensure('home'))
  const live = fakeSession({ route: 'direct', lan: LAN })
  home.found(live)
  await clock.advance(0)
  home.emit('route', 'looking')
  home.emit('route', 'direct', live)
  await clock.advance(0)
  t.is(ensured.value, live, 'the session is still the one up')
  t.alike(sessionEvents, [{ host: 'home', up: true }], 'no second up event')
  t.is(lanEvents.length, 1, 'no second lan event')
  t.is(live.destroyCount, 0, 'nothing ended')
}))

test('a search that nobody uses ends 5 minutes after the last caller gave up, however often it reports a failure', catchThrows(async (t) => {
  // lib/routes.js reports every failed search as an 'unreachable' route event and retries 30 s later ('looking'), so a
  // host that stays unreachable produces an event every 90 s. Those events must not restart the idle clock.
  const clock = fakeClock()
  const home = fakeRouteManager()
  const sessions = new Sessions({ routeManagerFor: () => home, clock: clock.timers })
  const opened = watch(sessions.open('home', 'ssh'))
  await clock.advance(WAKE_WAIT)
  t.is(opened.error && opened.error.code, 'HB-LOOKUP-TIMEOUT', 'open() failed at 30 s: the idle clock starts here')
  await clock.advance(30 * 1000) // t = 60 s: the first search fails
  home.lastError = hbError('HB-LOOKUP-TIMEOUT', 'no answer from the host within 60 s')
  home.fail(home.lastError)
  home.emit('route', 'unreachable')
  for (let i = 0; i < 2; i++) { // looking at 90 s and 180 s, unreachable at 150 s and 240 s
    await clock.advance(30 * 1000)
    home.emit('route', 'looking')
    await clock.advance(60 * 1000)
    home.emit('route', 'unreachable')
  }
  await clock.advance(IDLE_LIMIT + WAKE_WAIT - 240 * 1000 - 1) // to 1 ms before 30 s + 5 min
  t.is(home.closeCalls, 0, 'still searching 1 ms before 5 min after open() gave up')
  await clock.advance(1)
  t.is(home.closeCalls, 1, 'closed 5 min after open() gave up, whatever the manager reported meanwhile')
}))

test('an ensure() that arrives while an open() waits on a stopped manager fails at once with the code', catchThrows(async (t) => {
  const clock = fakeClock()
  const managers = []
  const routeManagerFor = () => {
    const manager = fakeRouteManager()
    managers.push(manager)
    return manager
  }
  const sessions = new Sessions({ routeManagerFor, clock: clock.timers })
  const opened = watch(sessions.open('home', 'ssh'))
  await clock.advance(0)
  managers[0].fail(hbError('HB-VERSION-MISMATCH', 'this host speaks protocol v2'))
  await clock.advance(1000)
  const ensured = watch(sessions.ensure('home'))
  await clock.advance(0)
  t.is(ensured.done, true, 'ensure() does not wait on a manager that will never retry')
  t.is(ensured.error && ensured.error.code, 'HB-VERSION-MISMATCH', 'it fails with the mismatch code')
  await clock.advance(WAKE_WAIT)
  t.is(opened.error && opened.error.code, 'HB-VERSION-MISMATCH', 'the open() still fails at its own deadline')
  const next = watch(sessions.ensure('home'))
  await clock.advance(0)
  t.is(managers.length, 2, 'once nobody waits, the next call starts a new manager')
  const live = fakeSession()
  managers[1].found(live)
  await clock.advance(0)
  t.is(next.value, live, 'and that manager finds the host')
}))
