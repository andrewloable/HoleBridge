// On-demand sessions per host for the app engine (docs/architecture.md, "Sessions and reconnects"; decision D17).
//
// new Sessions({ routeManagerFor, clock, idleMs = 5 * 60 * 1000, wakeWaitMs = 30 * 1000 }) is an EventEmitter. The
// caller passes what it owns:
// - routeManagerFor(host) -> the RouteManager for that host (lib/routes.js): connect() -> Promise<HostSession>,
//   lastError, close(), and 'route' events emitted as (state, session).
// - clock: { setTimeout(fn, ms), clearTimeout(id) }. The idle rule and the wake wait run through it, so tests run in
//   virtual time.
// - idleMs: a session closes this long after its last stream closes. Open streams, even idle ones, keep it up.
// - wakeWaitMs: how long open() waits for a session before it fails.
//
// ensure(host) -> Promise<HostSession> reuses a live session or starts one. open(host, service) -> Promise<Stream>
// opens a stream on the host's session. With no session it starts one and waits at most wakeWaitMs for it; if none
// forms, it fails with the manager's lastError code when that has an HB code, else HB-LOOKUP-TIMEOUT.
// close(host) ends the host: its manager is closed, its session ends, its waiting calls fail, and it is forgotten.
//
// Events:
// - 'session' with { host, up }, when a session comes up or goes down, once each. A route change that hands the host
//   to a new session is not an event: the host stays up.
// - 'lan' with { host, addresses, port }, when a session that comes up carries the host's LAN block. Dart stores these.
//   It is emitted before the 'session' up event.
//
// The contract (pinned by test/sessions.test.js):
// - routeManagerFor(host) is called when a host with no live manager gets a call that needs one. A manager closed by
//   Sessions is never reused.
// - Sessions ends a session with manager.close(), not session.destroy(): a bare destroy is a drop and the manager
//   would reconnect.
// - Sessions takes sessions from the manager's 'route' events. connect() resolves once, with the first session only.
// - ensure() rejects with the error of the first failed search (the manager keeps searching).
//
// Decisions the tests do not pin (HoleBridge-hb5.6.2; the owner may revise):
// - Keepalives are not set here. Sessions is handed finished HostSessions; the connection layer owns them
//   (lib/connect.js and lib/lan-session.js).
// - Registration. ensure() registers the host, as the IPC connect does (spec/ipc.md). A registered host's search runs
//   until close(host), or until its session idles out, which also ends the registration.
// - Uses. A waiting ensure() or open() is a use, and so is an open() whose stream is still opening. A use or an open
//   stream keeps the host: the idle clock does not run under it.
// - Idle. With no use and no open stream, a live session ends idleMs after it came up or after its last use or stream
//   ended (spec/ipc.md, point 2, for a session no stream uses).
// - A search that a local connection started, and that holds no session (it never formed one, or its session dropped
//   and did not come back), ends idleMs after its last caller gave up. Failed searches the manager reports in the
//   meantime do not restart the idle time.
// - A session that ends with an idle close or close(host) is ended by manager.close(). The manager destroys the
//   session without a drop, so no reconnect starts.
// - ensure() calls that wait during a reconnect fail with the next failed search. open() calls keep to their deadline.
// - HB-VERSION-MISMATCH stops the manager's retries. The next ensure() or open() with no session replaces that manager,
//   unless a call is still waiting on it. While an open() still waits on it, that open() waits and fails at its own
//   deadline, and an ensure() that arrives then fails at once with the code.
// - close(host): the waiting calls reject with a plain Error that has no HB code, so the IPC layer chooses the reply
//   code (spec/ipc.md). Sessions passes no HB code through from that close.

const EventEmitter = require('bare-events')

const LOOKUP_TIMEOUT = 'HB-LOOKUP-TIMEOUT'
const VERSION_MISMATCH = 'HB-VERSION-MISMATCH'
const CONNECTED_ROUTES = ['lan', 'direct', 'relay']

// hbError makes an error that carries an HB code and the reason, as lib/routes.js does.
function hbError(code, reason) {
  return Object.assign(new Error(`${code}: ${reason}`), { name: 'HbError', code, reason })
}

function isHbError(err) {
  return Boolean(err) && typeof err.code === 'string' && err.code.startsWith('HB-')
}

// stopped(manager): a manager that failed with a version mismatch stops retrying (lib/routes.js).
function stopped(manager) {
  return isHbError(manager.lastError) && manager.lastError.code === VERSION_MISMATCH
}

// newEntry is the state Sessions keeps for one host.
function newEntry(host) {
  return {
    host,
    manager: null, // the RouteManager in use, or null
    session: null, // the live HostSession, or null
    streams: 0, // open streams on the host, whichever session carries them
    uses: 0, // calls in flight: waiting for a session, or opening a stream
    waiters: new Set(), // calls waiting for a session: { kind: 'ensure' | 'open', service, resolve, reject, timer }
    registered: false, // ensure() asked for the host: its search runs until close(host) or the idle end
    idleTimer: null,
    closed: false // close(host) ran: the entry takes no more timers
  }
}

class Sessions extends EventEmitter {
  #routeManagerFor
  #clock
  #idleMs
  #wakeWaitMs
  #hosts = new Map() // host -> entry

  constructor({ routeManagerFor, clock, idleMs = 5 * 60 * 1000, wakeWaitMs = 30 * 1000 }) {
    super()
    this.#routeManagerFor = routeManagerFor
    this.#clock = clock
    this.#idleMs = idleMs
    this.#wakeWaitMs = wakeWaitMs
  }

  ensure(host) {
    const entry = this.#entry(host)
    entry.registered = true
    if (entry.session) return Promise.resolve(entry.session)
    return new Promise((resolve, reject) => {
      this.#wait(entry, { kind: 'ensure', service: null, resolve, reject, timer: null })
    })
  }

  open(host, service) {
    const entry = this.#entry(host)
    if (entry.session) {
      entry.uses++
      this.#evaluateIdle(entry)
      return this.#openOn(entry, entry.session, service)
    }
    return new Promise((resolve, reject) => {
      this.#wait(entry, { kind: 'open', service, resolve, reject, timer: null })
    })
  }

  // close(host) ends the host, as the IPC close does. Its manager is closed (the session ends without a drop), its
  // waiting calls fail with a plain Error, and the host is forgotten, so a later call starts afresh.
  close(host) {
    const entry = this.#hosts.get(host)
    if (!entry) return
    this.#hosts.delete(host)
    entry.closed = true
    const waiters = [...entry.waiters]
    for (const waiter of waiters) this.#drop(entry, waiter)
    this.#release(entry)
    for (const waiter of waiters) waiter.reject(new Error('the host was closed before a session formed'))
  }

  #entry(host) {
    let entry = this.#hosts.get(host)
    if (!entry) {
      entry = newEntry(host)
      this.#hosts.set(host, entry)
    }
    return entry
  }

  // #wait queues a call that needs a session, starts a search when none runs, and counts the call as a use.
  #wait(entry, waiter) {
    if (entry.manager && !entry.session && entry.uses === 0 && stopped(entry.manager)) this.#release(entry)
    // A stopped manager sends no more events and never settles an ensure(), which has no deadline. The first failure
    // ends the connect (docs/architecture.md, Choosing a route), so the ensure() fails with the code now. open() joins
    // the wait and fails at its own deadline.
    if (waiter.kind === 'ensure' && entry.manager && !entry.session && stopped(entry.manager)) {
      waiter.reject(entry.manager.lastError)
      return
    }
    entry.uses++
    entry.waiters.add(waiter)
    if (waiter.kind === 'open') {
      waiter.timer = this.#clock.setTimeout(() => this.#wakeExpired(entry, waiter), this.#wakeWaitMs)
    }
    if (!entry.manager) this.#startManager(entry)
    this.#evaluateIdle(entry)
  }

  #startManager(entry) {
    const manager = this.#routeManagerFor(entry.host)
    entry.manager = manager
    manager.on('route', (state, session) => this.#onRoute(entry, manager, state, session))
    manager.connect().then(
      () => {}, // the session itself arrives in a 'route' event
      (error) => this.#onSearchFailed(entry, manager, error)
    )
  }

  #onRoute(entry, manager, state, session) {
    if (entry.manager !== manager) return // a manager Sessions has let go of
    if (CONNECTED_ROUTES.includes(state) && session) this.#sessionUp(entry, session)
    else if (state === 'unreachable') this.#failEnsures(entry, manager.lastError)
    // 'looking' needs no action: the session's own 'close' says when a session goes down.
  }

  #onSearchFailed(entry, manager, error) {
    if (entry.manager === manager) this.#failEnsures(entry, error)
  }

  // #failEnsures fails the ensure() calls waiting now with the error of the search that failed. open() calls keep
  // waiting for their own deadline. The idle clock restarts only when an ensure() was failed: a failed search that
  // rejects no waiter changes neither uses nor streams, so the running idle timer keeps running.
  #failEnsures(entry, cause) {
    const error = isHbError(cause) ? cause : hbError(LOOKUP_TIMEOUT, 'no answer from the host')
    let failed = 0
    for (const waiter of [...entry.waiters]) {
      if (waiter.kind !== 'ensure') continue
      this.#drop(entry, waiter)
      entry.uses--
      failed++
      waiter.reject(error)
    }
    if (failed > 0) this.#evaluateIdle(entry)
  }

  #sessionUp(entry, session) {
    if (entry.session === session) return // the manager announced the live route again
    const handover = entry.session !== null
    entry.session = session
    session.on('close', () => this.#sessionClosed(entry, session))
    if (session.lan) this.emit('lan', { host: entry.host, addresses: session.lan.addresses, port: session.lan.port })
    if (!handover) this.emit('session', { host: entry.host, up: true })
    for (const waiter of [...entry.waiters]) {
      this.#drop(entry, waiter)
      if (waiter.kind === 'ensure') {
        entry.uses--
        waiter.resolve(session)
      } else {
        waiter.resolve(this.#openOn(entry, session, waiter.service))
      }
    }
    this.#evaluateIdle(entry)
  }

  // #sessionClosed handles a session that ends on its own: a drop. The manager reconnects by itself.
  #sessionClosed(entry, session) {
    if (entry.session !== session) return // handed over, or ended by Sessions itself
    entry.session = null
    this.emit('session', { host: entry.host, up: false })
    this.#evaluateIdle(entry)
  }

  // #openOn opens a stream on a session. The caller counted the call as a use; this ends that use.
  #openOn(entry, session, service) {
    return new Promise((resolve) => resolve(session.open(service))).then(
      (stream) => {
        entry.uses--
        entry.streams++
        stream.on('close', () => this.#streamClosed(entry))
        this.#evaluateIdle(entry)
        return stream
      },
      (error) => {
        entry.uses--
        this.#evaluateIdle(entry)
        throw error
      }
    )
  }

  #streamClosed(entry) {
    entry.streams--
    this.#evaluateIdle(entry)
  }

  #wakeExpired(entry, waiter) {
    this.#drop(entry, waiter)
    entry.uses--
    const last = entry.manager ? entry.manager.lastError : null
    waiter.reject(isHbError(last) ? last : hbError(LOOKUP_TIMEOUT, 'no session formed within the wait'))
    this.#evaluateIdle(entry)
  }

  // #drop takes a waiting call out of the queue and stops its wake timer.
  #drop(entry, waiter) {
    entry.waiters.delete(waiter)
    if (waiter.timer !== null) this.#clock.clearTimeout(waiter.timer)
    waiter.timer = null
  }

  // #evaluateIdle restarts the idle clock from this moment. Any use or open stream keeps the host, so the clock stops.
  // Otherwise a live session ends idleMs later, and a search nobody waits on or registered for ends idleMs later too.
  #evaluateIdle(entry) {
    this.#cancelIdle(entry)
    if (entry.closed || entry.uses > 0 || entry.streams > 0) return
    if (!entry.session && (entry.registered || !entry.manager)) return
    entry.idleTimer = this.#clock.setTimeout(() => this.#expire(entry), this.#idleMs)
  }

  #expire(entry) {
    entry.idleTimer = null
    if (entry.uses > 0 || entry.streams > 0) return
    entry.registered = false
    this.#release(entry)
  }

  // #release ends the host's manager. manager.close() ends a session without a drop, so no reconnect starts. A session
  // that was up is reported down, once.
  #release(entry) {
    this.#cancelIdle(entry)
    const manager = entry.manager
    const wasUp = entry.session !== null
    entry.manager = null
    entry.session = null
    if (manager) manager.close()
    if (wasUp) this.emit('session', { host: entry.host, up: false })
  }

  #cancelIdle(entry) {
    if (entry.idleTimer !== null) this.#clock.clearTimeout(entry.idleTimer)
    entry.idleTimer = null
  }
}

module.exports = { Sessions }
