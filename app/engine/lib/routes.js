// Route selection, re-evaluation and reconnect for the app engine (docs/architecture.md, "Choosing a route (app)"
// and "Sessions and reconnects").
//
// new RouteManager({ findLan, connectLan, connectDht, localAddresses, clock }) is an EventEmitter. The caller binds
// the keys and the DHT, so each dependency takes only what a search needs:
// - findLan() -> Promise<{ address, port } | null>: one LAN search (lib/lan-probe.js findHost). null, or a rejection,
//   means no LAN host.
// - connectLan({ address, port }) -> Promise<HostSession>, route 'lan' (lib/lan-session.js connectLan).
// - connectDht() -> Promise<HostSession>, route 'direct' or 'relay' as the session reports it (lib/connect.js).
// - localAddresses() -> string[]: this device's addresses, read every 5 s.
// - clock: { setTimeout(fn, ms), clearTimeout(id) }. The manager waits only through it, so tests run in virtual time
//   and close() can stop every timer.
//
// connect() -> Promise<HostSession> resolves with the first route found. It rejects with the error of the first search
// that fails: HB-LOOKUP-TIMEOUT when no host answers (at 60 s at the latest), HB-VERSION-MISMATCH at once. The manager
// keeps searching after that, except after a version mismatch. spec/ipc.md says that after a timeout the
// host "retries on its own until close".
//
// 'route' events: emit('route', state) for 'looking' and 'unreachable'; emit('route', state, session) for 'lan',
// 'direct' and 'relay', where session is the HostSession that carries the route. An event is emitted only when the
// state or that session changes. lastError is the error of the most recent failed search (its code is an HB code). It
// is null once a search finds a route; a failed re-evaluation while a route is live sets it, and the route stays.
//
// Decisions (HoleBridge-hb5.3.2; the owner may revise them):
// - A search tries the LAN first, then the DHT. A LAN dial that fails, other than with a version mismatch, goes on to
//   the DHT. A LAN probe that rejects counts as no LAN host.
// - A live 'direct' or 'relay' session is kept when an address change finds no LAN host: a fresh DHT dial would
//   replace a healthy session for nothing. A live 'lan' session that finds no LAN host is replaced by a DHT session
//   once that is up (the device walked out of the house).
// - A found route replaces the live session: the new session is announced first, then the old one is destroyed.
// - A failed search with a live session keeps that route and does not report 'unreachable'. Without one, it reports
//   'unreachable' and retries: 30 s after a first connect, or after the reconnect backoff that follows a drop.
// - Every search is a fresh attempt. A new search (an address change, a retry) abandons the one in progress, and a dial
//   that answers after it was abandoned is destroyed, not used. A late rejection of an abandoned dial is ignored.
// - A drop reports 'looking' at once and reconnects after a delay: 1 s doubling to 30 s, each delay a random whole
//   number of ms in [nominal / 2, nominal] (docs/architecture.md says "with jitter" and gives no range). A route that
//   comes up resets the backoff. A search already running after a drop is not joined by a second one.
// - HB-VERSION-MISMATCH is not retried by a timer: waiting does not change the protocol version. An address change still
//   starts a fresh search.
// - A dial's own 60 s timeout and the manager's 60 s limit are one failure: the search is over, 'unreachable' follows,
//   and the retry is 30 s later. The dial's late rejection starts no backoff.
// - close() ends the manager for good. Its contract is stated above close().
//
// bare-events is the EventEmitter that the engine's dependencies use under Bare (hyperdht maps 'events' to it). It is
// pinned in package.json.

const EventEmitter = require('bare-events')

const LOOKUP_LIMIT = 60 * 1000 // a search gives up after 60 s (spec/ipc.md)
const RETRY_AFTER = 30 * 1000 // after 'unreachable' on a first connect, a new search starts 30 s later
const POLL_INTERVAL = 5 * 1000 // local addresses are polled every 5 s
const FIRST_BACKOFF = 1000 // reconnect delays: 1 s doubling to 30 s
const BACKOFF_CAP = 30 * 1000

// hbError makes an error that carries an HB code and the reason, as lib/connect.js does.
function hbError(code, reason) {
  return Object.assign(new Error(`${code}: ${reason}`), { name: 'HbError', code, reason })
}

// asHbError: an HB error passes through; anything else is no answer from the host, as in lib/connect.js.
function asHbError(err) {
  if (err && typeof err.code === 'string' && err.code.startsWith('HB-')) return err
  const why = err && err.code ? `no answer from the host (${err.code})` : 'no answer from the host'
  return hbError('HB-LOOKUP-TIMEOUT', why)
}

// backoffDelay(k): the k-th reconnect delay. Nominal FIRST_BACKOFF * 2^k, capped at BACKOFF_CAP; the delay is a random
// whole number of ms in [nominal / 2, nominal].
function backoffDelay(k) {
  const nominal = Math.min(FIRST_BACKOFF * 2 ** k, BACKOFF_CAP)
  return Math.floor(nominal / 2 + Math.random() * (nominal / 2))
}

const addressKey = (addresses) => [...addresses].sort().join(' ')

class RouteManager extends EventEmitter {
  #findLan
  #connectLan
  #connectDht
  #localAddresses
  #clock
  #closed = false
  #attempt = null // the search in progress, as a token; null when none
  #current = null // the live HostSession
  #currentRoute = null
  #reconnecting = false // true from a drop until a route is up
  #failures = 0 // reconnect delays used since the last route came up
  #lastState = null
  #lastSession = null
  #lastError = null
  #addresses = null
  #connecting = null // { promise, resolve, reject, settled }, set by the first connect()
  #limitTimer = null
  #retryTimer = null
  #pollTimer = null

  constructor({ findLan, connectLan, connectDht, localAddresses, clock }) {
    super()
    this.#findLan = findLan
    this.#connectLan = connectLan
    this.#connectDht = connectDht
    this.#localAddresses = localAddresses
    this.#clock = clock
  }

  // lastError: the error of the most recent failed search. It is null once a search finds a route; a failed
  // re-evaluation while a route is live sets it, and the route stays.
  get lastError() {
    return this.#lastError
  }

  // connect() starts the manager and returns the same promise on every later call.
  connect() {
    if (this.#connecting) return this.#connecting.promise
    const connecting = { settled: false }
    connecting.promise = new Promise((resolve, reject) => {
      connecting.resolve = resolve
      connecting.reject = reject
    })
    this.#connecting = connecting
    if (this.#closed) {
      this.#settleConnect(false, new Error('the route manager is closed'))
      return connecting.promise
    }
    this.#addresses = addressKey(this.#localAddresses())
    this.#poll()
    this.#startSearch()
    return connecting.promise
  }

  // close() stops the manager for good (the tests in test/routes.test.js fix this contract; the IMPL task
  // HoleBridge-hb5.3.2 builds it):
  // - it cancels the 5 s poll of localAddresses() and every pending timer (the 60 s limit, the 30 s retry, the
  //   reconnect backoff);
  // - it destroys the live session, and that close is not a drop: no reconnect starts;
  // - it abandons a pending dial: a dial that succeeds afterwards is destroyed, not used;
  // - after it returns it starts no findLan, connectLan or connectDht, reads no localAddresses() and emits no
  //   'route' event;
  // - calling it twice is harmless.
  // A connect() still pending rejects; a connect() after close() rejects at once.
  close() {
    if (this.#closed) return
    this.#closed = true
    this.#attempt = null
    for (const timer of [this.#pollTimer, this.#limitTimer, this.#retryTimer]) {
      if (timer !== null) this.#clock.clearTimeout(timer)
    }
    this.#pollTimer = null
    this.#limitTimer = null
    this.#retryTimer = null
    const session = this.#current
    this.#current = null
    this.#currentRoute = null
    if (session) session.destroy()
    this.#settleConnect(false, new Error('the route manager is closed'))
  }

  #isActive(attempt) {
    return !this.#closed && this.#attempt === attempt
  }

  #settleConnect(ok, value) {
    const connecting = this.#connecting
    if (!connecting || connecting.settled) return
    connecting.settled = true
    if (ok) connecting.resolve(value)
    else connecting.reject(value)
  }

  #publish(state, session = null) {
    if (this.#closed) return
    if (state === this.#lastState && session === this.#lastSession) return
    this.#lastState = state
    this.#lastSession = session
    if (session) this.emit('route', state, session)
    else this.emit('route', state)
  }

  #clearLimit() {
    if (this.#limitTimer !== null) this.#clock.clearTimeout(this.#limitTimer)
    this.#limitTimer = null
  }

  #scheduleRetry(ms) {
    if (this.#closed) return
    this.#retryTimer = this.#clock.setTimeout(() => {
      this.#retryTimer = null
      this.#startSearch()
    }, ms)
  }

  #nextDelay() {
    return backoffDelay(this.#failures++)
  }

  #startSearch() {
    if (this.#retryTimer !== null) this.#clock.clearTimeout(this.#retryTimer)
    this.#retryTimer = null
    this.#clearLimit()
    const attempt = {}
    this.#attempt = attempt
    this.#limitTimer = this.#clock.setTimeout(() => {
      this.#limitTimer = null
      this.#failSearch(hbError('HB-LOOKUP-TIMEOUT', 'no answer from the host within 60 s'))
    }, LOOKUP_LIMIT)
    this.#publish('looking')
    this.#search(attempt)
  }

  async #search(attempt) {
    if (!this.#isActive(attempt)) return
    let lan = null
    try {
      lan = await this.#findLan()
    } catch {
      lan = null // a probe that fails finds no LAN host
    }
    if (!this.#isActive(attempt)) return
    if (lan) {
      const got = await this.#dial(attempt, () => this.#connectLan(lan))
      if (!this.#isActive(attempt)) return
      if (got.session) return this.#adopt(got.session, 'lan')
      if (got.error.code === 'HB-VERSION-MISMATCH') return this.#failSearch(got.error)
      this.#lastError = got.error // the DHT is the next way in
    }
    if (this.#current && this.#currentRoute !== 'lan') return this.#keepCurrent()
    const got = await this.#dial(attempt, () => this.#connectDht())
    if (!this.#isActive(attempt)) return
    if (got.session) return this.#adopt(got.session, got.session.route)
    this.#failSearch(got.error)
  }

  // #dial runs one dial for the search attempt. A dial that answers after the attempt was abandoned is destroyed here.
  async #dial(attempt, dial) {
    let session
    try {
      session = await dial()
    } catch (err) {
      return { error: asHbError(err) }
    }
    if (!this.#isActive(attempt)) {
      session.destroy()
      return { error: null }
    }
    return { session }
  }

  #adopt(session, route) {
    this.#attempt = null
    this.#clearLimit()
    this.#lastError = null
    this.#reconnecting = false
    this.#failures = 0
    const old = this.#current
    this.#current = session
    this.#currentRoute = route
    session.on('close', () => this.#onClose(session))
    this.#publish(route, session)
    this.#settleConnect(true, session)
    if (old) old.destroy() // the old route closes only now that the new one is up
  }

  // #keepCurrent ends a search that found nothing better than the live session.
  #keepCurrent() {
    this.#attempt = null
    this.#clearLimit()
    this.#publish(this.#currentRoute, this.#current)
  }

  #failSearch(error) {
    this.#attempt = null
    this.#clearLimit()
    this.#lastError = error
    if (this.#current) return this.#publish(this.#currentRoute, this.#current)
    this.#publish('unreachable')
    this.#settleConnect(false, error)
    if (error.code === 'HB-VERSION-MISMATCH') return
    this.#scheduleRetry(this.#reconnecting ? this.#nextDelay() : RETRY_AFTER)
  }

  // #onClose handles a session that ends. A session the manager closed or replaced is not a drop.
  #onClose(session) {
    if (this.#closed || session !== this.#current) return
    this.#current = null
    this.#currentRoute = null
    this.#reconnecting = true
    this.#publish('looking')
    if (this.#attempt === null) this.#scheduleRetry(this.#nextDelay())
  }

  #poll() {
    if (this.#closed) return
    this.#pollTimer = this.#clock.setTimeout(() => this.#tick(), POLL_INTERVAL)
  }

  #tick() {
    this.#pollTimer = null
    const addresses = addressKey(this.#localAddresses())
    if (addresses !== this.#addresses) {
      this.#addresses = addresses
      this.#startSearch()
    }
    this.#poll()
  }
}

module.exports = { RouteManager }
