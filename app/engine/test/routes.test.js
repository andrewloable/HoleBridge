// Tests for route selection, re-evaluation and reconnect (lib/routes.js). Rules: docs/architecture.md, "Choosing a
// route (app)" and "Sessions and reconnects". Each test drives a RouteManager with fakes for the three ways in
// (findLan, connectLan, connectDht), a list of local addresses and a fake clock (fakeClock below). Time moves only
// when a test advances it, so the tests run in virtual time and never wait on a real timer.
//
// Shapes the IMPL must meet (also stated in lib/routes.js):
// - new RouteManager({ findLan, connectLan, connectDht, localAddresses, clock }) is an EventEmitter.
// - findLan() -> Promise<{ address, port } | null>. null means no LAN host answered.
// - connectLan({ address, port }) -> Promise<HostSession>, whose route is 'lan'.
// - connectDht() -> Promise<HostSession>, whose route is 'direct' or 'relay' as the session reports it.
// - localAddresses() -> string[], polled every 5 s.
// - clock is { setTimeout(fn, ms), clearTimeout(id) }. The manager waits only through it.
// - connect() -> Promise<HostSession>, resolving once a route is found. Each state change is emitted as 'route'
//   with the state: 'looking', 'lan', 'direct', 'relay' or 'unreachable'. When a session exists for that state, it
//   is the second argument: emit('route', state, session).
// - close() stops the manager for good (see the close() tests at the end of the file)
// - A HostSession's on('close', fn) runs fn when the session ends. destroy() ends it and runs those listeners too,
//   as lib/connect.js does.
//
// Jitter: docs/architecture.md says "with jitter" and gives no range. These tests fix it as: each reconnect delay
// lies between half and all of its nominal value (1 s doubling, capped at 30 s). The cap then holds with jitter.
const test = require('brittle')
const { RouteManager } = require('../lib/routes.js')

const OWN_ADDRESS = '192.0.2.10' // this device's address, in TEST-NET-1 (RFC 5737)
const MOVED_ADDRESS = '192.0.2.30' // the address after the device joins another network
const LAN_HOST = { address: '192.0.2.20', port: 8443 }
const LOOKUP_LIMIT = 60000 // a search gives up after 60 s and reports unreachable
const RETRY_AFTER = 30000 // after unreachable, a new search starts 30 s later
const POLL_INTERVAL = 5000 // local addresses are polled every 5 s
const FIRST_BACKOFF = 1000
const BACKOFF_CAP = 30000

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

// watch(promise) -> { done, value, error }: how promise settles, filled in as it does.
function watch(promise) {
  const out = { done: false, value: undefined, error: undefined }
  promise.then(
    (value) => Object.assign(out, { done: true, value }),
    (error) => Object.assign(out, { done: true, error })
  )
  return out
}

// fakeSession(route, { name, log }) is a HostSession as lib/connect.js makes it, for the fakes to hand out. As in the
// real one, destroy() ends the session and runs its close listeners, and drop() ends it from the path side. Each
// destroy() is recorded in log, as "<name> destroyed", so a test can check the order of events.
function fakeSession(route, { name = route, log = [] } = {}) {
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
    lan: null,
    flags: 0,
    destroyCount: 0,
    open() {
      throw new Error('open is not used by these tests')
    },
    on(event, listener) {
      if (event !== 'close') throw new Error(`HostSession emits close only, not ${event}`)
      listeners.push(listener)
    },
    destroy() {
      session.destroyCount++
      log.push(`${name} destroyed`)
      end()
    },
    drop: end
  }
  return session
}

// lookupFailure() is the error a dial rejects with when no host answers, as lib/connect.js makes it.
function lookupFailure() {
  const reason = 'no answer from the host'
  return Object.assign(new Error(`HB-LOOKUP-TIMEOUT: ${reason}`), { name: 'HbError', code: 'HB-LOOKUP-TIMEOUT', reason })
}

// routesOf(manager, clock) listens for 'route' and returns the list the events go into, each as { state, at }.
function routesOf(manager, clock) {
  const events = []
  manager.on('route', (state) => events.push({ state, at: clock.now() }))
  return events
}

const statesOf = (events) => events.map((e) => e.state)

// A dial that the test never answers: a host that does not reply.
const noAnswer = () => new Promise(() => {})

// connectLan that must not run: the tests with no LAN host never reach it.
function noLanDial() {
  throw new Error('connectLan must not be called without a LAN host')
}

test('LAN found: the route is lan and connectDht is never called', catchThrows(async (t) => {
  const clock = fakeClock()
  const lan = fakeSession('lan')
  const lanCalls = []
  let dhtCalls = 0
  const manager = new RouteManager({
    findLan: async () => LAN_HOST,
    connectLan: async (where) => {
      lanCalls.push(where)
      return lan
    },
    connectDht: () => {
      dhtCalls++
      return noAnswer()
    },
    localAddresses: () => [OWN_ADDRESS],
    clock: clock.timers
  })
  const events = routesOf(manager, clock)
  const result = watch(manager.connect())
  await clock.advance(0)
  t.ok(result.done, 'connect settles')
  t.is(result.value, lan, 'connect resolves with the LAN session')
  t.alike(lanCalls, [LAN_HOST], 'connectLan is given the address and port the search found')
  t.is(dhtCalls, 0, 'connectDht is never called')
  t.alike(statesOf(events), ['looking', 'lan'], 'the route events are looking, then lan')
}))

// LAN not found: the DHT is dialed, and the route is the one the session reports. Direct is the usual case; relay is
// when HyperDHT relayed the dial because both sides are behind randomizing NATs.
for (const route of ['direct', 'relay']) {
  test(`LAN not found: connectDht is dialed and the route is ${route}`, catchThrows(async (t) => {
    const clock = fakeClock()
    const dht = fakeSession(route)
    const dialAt = []
    let lanCalls = 0
    const manager = new RouteManager({
      findLan: async () => null,
      connectLan: () => {
        lanCalls++
        return noLanDial()
      },
      connectDht: async () => {
        dialAt.push(clock.now())
        return dht
      },
      localAddresses: () => [OWN_ADDRESS],
      clock: clock.timers
    })
    const events = routesOf(manager, clock)
    const result = watch(manager.connect())
    await clock.advance(0)
    t.alike(dialAt, [0], 'connectDht is dialed once, at once')
    t.is(lanCalls, 0, 'connectLan is not called without a LAN host')
    t.is(result.value, dht, `connect resolves with the ${route} session`)
    t.is(result.value && result.value.route, route, `the session's route is ${route}`)
    t.alike(statesOf(events), ['looking', route], `the route events are looking, then ${route}`)
  }))
}

test('nothing found within 60 s: unreachable at 60 s, then a new search 30 s later', catchThrows(async (t) => {
  const clock = fakeClock()
  const retried = fakeSession('direct')
  const dialAt = []
  const manager = new RouteManager({
    findLan: async () => null,
    connectLan: noLanDial,
    connectDht: () => {
      dialAt.push(clock.now())
      // The first dial never answers. The search started by the retry finds the host.
      return dialAt.length === 1 ? noAnswer() : Promise.resolve(retried)
    },
    localAddresses: () => [OWN_ADDRESS],
    clock: clock.timers
  })
  const events = routesOf(manager, clock)
  watch(manager.connect())

  await clock.advance(LOOKUP_LIMIT)
  t.alike(
    events.filter((e) => e.state === 'unreachable').map((e) => e.at),
    [LOOKUP_LIMIT],
    'unreachable is reported once, at 60 s'
  )
  t.alike(dialAt, [0], 'no new search has started at 60 s')

  await clock.advance(RETRY_AFTER - 1)
  t.alike(dialAt, [0], 'no new search 29.999 s after unreachable')

  await clock.advance(1)
  t.alike(dialAt, [0, LOOKUP_LIMIT + RETRY_AFTER], 'a new search starts 30 s after unreachable')

  await clock.advance(0)
  t.alike(
    statesOf(events),
    ['looking', 'unreachable', 'looking', 'direct'],
    'the search after the retry reports looking, then the route it finds'
  )
}))

test("'looking' is emitted while a search runs and is never reported as failure before 60 s", catchThrows(async (t) => {
  const clock = fakeClock()
  const manager = new RouteManager({
    findLan: async () => null,
    connectLan: noLanDial,
    // A slow lookup: the DHT is still searching.
    connectDht: noAnswer,
    localAddresses: () => [OWN_ADDRESS],
    clock: clock.timers
  })
  const events = routesOf(manager, clock)
  watch(manager.connect())

  await clock.advance(1000)
  t.alike(statesOf(events), ['looking'], 'looking is emitted when the search starts')

  await clock.advance(LOOKUP_LIMIT - 1000 - 1)
  t.alike(statesOf(events), ['looking'], 'still looking at 59.999 s: no failure is reported')

  await clock.advance(1)
  t.alike(
    events.filter((e) => e.state === 'unreachable').map((e) => e.at),
    [LOOKUP_LIMIT],
    'the first failure report comes at 60 s'
  )
}))

test('a dropped session reconnects with delays 1 s doubling to 30 s, each with jitter in range', catchThrows(async (t) => {
  const clock = fakeClock()
  const first = fakeSession('direct')
  const dialAt = []
  const manager = new RouteManager({
    findLan: async () => null,
    connectLan: noLanDial,
    connectDht: () => {
      dialAt.push(clock.now())
      // The first dial connects. Every reconnect dial fails at once, so only the backoff sets the pace.
      return dialAt.length === 1 ? Promise.resolve(first) : Promise.reject(lookupFailure())
    },
    localAddresses: () => [OWN_ADDRESS],
    clock: clock.timers
  })
  const result = watch(manager.connect())
  await clock.advance(0)
  t.is(result.value, first, 'the first session is up')

  const dropAt = clock.now()
  first.drop()
  await clock.advance(120000)

  const attempts = dialAt.slice(1)
  t.ok(attempts.length >= 7, `at least 7 reconnect attempts in 120 s (${attempts.length} seen)`)
  let previous = dropAt
  const gaps = attempts.slice(0, 7).map((at) => {
    const gap = at - previous
    previous = at
    return gap
  })
  for (let k = 0; k < 7; k++) {
    const nominal = Math.min(FIRST_BACKOFF * 2 ** k, BACKOFF_CAP)
    t.ok(
      gaps[k] >= nominal / 2 && gaps[k] <= nominal,
      `reconnect ${k + 1} waits ${gaps[k]} ms, within [${nominal / 2}, ${nominal}] ms`
    )
  }
}))

test('a local address change while on direct finds the LAN host and switches to lan', catchThrows(async (t) => {
  const clock = fakeClock()
  const order = []
  const direct = fakeSession('direct', { name: 'direct', log: order })
  const lan = fakeSession('lan')
  // The LAN session comes up only when the test lets it, so the test can see the direct session still open.
  const lanUp = deferred()
  let addresses = [OWN_ADDRESS]
  let lanVisible = false
  let searches = 0
  let dhtDials = 0
  const lanCalls = []
  const manager = new RouteManager({
    findLan: async () => {
      searches++
      return lanVisible ? LAN_HOST : null
    },
    connectLan: (where) => {
      lanCalls.push(where)
      return lanUp.promise.then(() => {
        order.push('lan up')
        return lan
      })
    },
    connectDht: async () => {
      dhtDials++
      return direct
    },
    localAddresses: () => addresses,
    clock: clock.timers
  })
  const events = routesOf(manager, clock)
  const result = watch(manager.connect())
  await clock.advance(0)
  t.is(result.value, direct, 'connect resolves over direct')

  // The device joins the host's network: the next poll sees a new address, and the LAN host answers.
  addresses = [MOVED_ADDRESS]
  lanVisible = true
  await clock.advance(POLL_INTERVAL)
  t.alike(lanCalls, [LAN_HOST], 'the change starts a search that finds the LAN host')
  t.is(dhtDials, 1, 'connectDht is not dialed again, because the LAN host was found')
  t.is(direct.destroyCount, 0, 'the direct session stays open while the LAN session is set up')

  lanUp.resolve()
  await clock.advance(0)
  t.alike(statesOf(events), ['looking', 'direct', 'looking', 'lan'], 'the route moves from direct to lan')
  t.alike(order, ['lan up', 'direct destroyed'], 'the direct session closes after the lan session is up')

  // The direct session was closed on purpose, so its close is not a drop: nothing reconnects after it.
  const searchesBefore = searches
  await clock.advance(120000)
  t.is(searches, searchesBefore, 'no reconnect starts after the direct session closes')
  t.is(dhtDials, 1, 'connectDht is still dialed once')
  t.is(lanCalls.length, 1, 'connectLan is still called once')
  t.is(events.length, 4, 'no further route events follow the switch')
}))

test('a search after a change does not reuse a pending dial: the old attempt is abandoned', catchThrows(async (t) => {
  const clock = fakeClock()
  const oldDial = deferred()
  const newDial = deferred()
  const dialAt = []
  let addresses = [OWN_ADDRESS]
  const manager = new RouteManager({
    findLan: async () => null,
    connectLan: noLanDial,
    connectDht: () => {
      dialAt.push(clock.now())
      return dialAt.length === 1 ? oldDial.promise : newDial.promise
    },
    localAddresses: () => addresses,
    clock: clock.timers
  })
  const events = routesOf(manager, clock)
  const result = watch(manager.connect())
  await clock.advance(0)
  t.alike(dialAt, [0], 'the first dial is pending')

  addresses = [MOVED_ADDRESS]
  await clock.advance(POLL_INTERVAL)
  t.alike(dialAt, [0, POLL_INTERVAL], 'the change starts a fresh dial instead of waiting on the old one')

  const fresh = fakeSession('direct')
  newDial.resolve(fresh)
  await clock.advance(0)
  t.is(result.value, fresh, 'connect resolves with the fresh search')

  const abandoned = fakeSession('direct')
  oldDial.resolve(abandoned)
  await clock.advance(0)
  t.is(abandoned.destroyCount, 1, 'the abandoned dial, if it succeeds late, is closed and not used')
  t.is(result.value, fresh, 'connect still resolves with the fresh session')
  t.is(events.filter((e) => e.state === 'direct').length, 1, 'only the fresh search reports a route')
}))

// close() ends a manager for good. docs: spec/ipc.md says `connect` "retries on its own until `close`" and `close`
// "ends the host's session and listeners and unregisters the host"; the idle rule closes a session 5 minutes after
// its last stream, and "with no session [the app] sends no network traffic" (docs/architecture.md, "Sessions and
// reconnects"). Without close(), the 5 s poll and the reconnect would run forever. The contract these tests fix:
// close() stops the poll and every timer, abandons pending dials (one that answers later is destroyed), destroys the
// live session without that counting as a drop, starts no search afterwards and emits no 'route' event afterwards.
// close() twice is harmless. What connect() does for a closed manager is not fixed here, so each test only watches it.
const TEN_MINUTES = 600000

test('close() while connected: the session ends, the poll stops and nothing reconnects', catchThrows(async (t) => {
  const clock = fakeClock()
  const direct = fakeSession('direct')
  let addresses = [OWN_ADDRESS]
  let findCalls = 0
  let dhtDials = 0
  let addressReads = 0
  const manager = new RouteManager({
    findLan: async () => {
      findCalls++
      return null
    },
    connectLan: noLanDial,
    connectDht: async () => {
      dhtDials++
      return direct
    },
    localAddresses: () => {
      addressReads++
      return addresses
    },
    clock: clock.timers
  })
  const events = routesOf(manager, clock)
  const result = watch(manager.connect())
  await clock.advance(0)
  t.is(result.value, direct, 'connect resolves over direct')

  manager.close()
  t.is(direct.destroyCount, 1, 'close() ends the live session')
  const readsAtClose = addressReads
  const eventsAtClose = events.length

  // A new address would start a search on a running manager. After close() it must not.
  addresses = [MOVED_ADDRESS]
  await clock.advance(TEN_MINUTES)
  t.is(addressReads, readsAtClose, 'the poll of local addresses stopped')
  t.is(findCalls, 1, 'no new LAN search')
  t.is(dhtDials, 1, 'no new dial, so the close of the session was not treated as a drop')
  t.is(events.length, eventsAtClose, 'no route event after close()')

  manager.close()
  t.is(direct.destroyCount, 1, 'a second close() changes nothing')
}))

test('close() while a dial is pending: a dial that answers later is destroyed, not used', catchThrows(async (t) => {
  const clock = fakeClock()
  const dial = deferred()
  let dhtDials = 0
  const manager = new RouteManager({
    findLan: async () => null,
    connectLan: noLanDial,
    connectDht: () => {
      dhtDials++
      return dial.promise
    },
    localAddresses: () => [OWN_ADDRESS],
    clock: clock.timers
  })
  const events = routesOf(manager, clock)
  const result = watch(manager.connect())
  await clock.advance(0)
  t.alike(statesOf(events), ['looking'], 'the search is running')

  manager.close()
  const late = fakeSession('direct')
  dial.resolve(late)
  await clock.advance(0)
  t.is(late.destroyCount, 1, 'the late session is destroyed')
  t.not(result.value, late, 'connect does not resolve with the late session')
  await clock.advance(TEN_MINUTES)
  t.is(dhtDials, 1, 'no new dial after close()')
  t.alike(statesOf(events), ['looking'], 'no route event after close()')
}))

test('close() while unreachable: the retry 30 s later does not start', catchThrows(async (t) => {
  const clock = fakeClock()
  let dhtDials = 0
  const manager = new RouteManager({
    findLan: async () => null,
    connectLan: noLanDial,
    connectDht: () => {
      dhtDials++
      return noAnswer()
    },
    localAddresses: () => [OWN_ADDRESS],
    clock: clock.timers
  })
  const events = routesOf(manager, clock)
  watch(manager.connect())
  await clock.advance(LOOKUP_LIMIT)
  t.alike(statesOf(events), ['looking', 'unreachable'], 'the manager is unreachable')

  manager.close()
  await clock.advance(TEN_MINUTES)
  t.is(dhtDials, 1, 'no new search after close()')
  t.alike(statesOf(events), ['looking', 'unreachable'], 'no route event after close()')
}))

test('close() while waiting to reconnect after a drop: no reconnect starts', catchThrows(async (t) => {
  const clock = fakeClock()
  const first = fakeSession('direct')
  let dhtDials = 0
  const manager = new RouteManager({
    findLan: async () => null,
    connectLan: noLanDial,
    connectDht: () => {
      dhtDials++
      return Promise.resolve(first)
    },
    localAddresses: () => [OWN_ADDRESS],
    clock: clock.timers
  })
  const events = routesOf(manager, clock)
  const result = watch(manager.connect())
  await clock.advance(0)
  t.is(result.value, first, 'the first session is up')

  first.drop()
  await clock.advance(0)
  const eventsAtClose = events.length
  manager.close()
  await clock.advance(TEN_MINUTES)
  t.is(dhtDials, 1, 'no reconnect after close()')
  t.is(events.length, eventsAtClose, 'no route event after close()')
}))

// Added by HoleBridge-hb5.3.2: the cases the review of HoleBridge-hb5.3.1 asked for (the notes on that task).

// versionMismatch() is the error a dial rejects with when the host speaks another protocol version (lib/connect.js).
function versionMismatch() {
  const reason = 'this host speaks protocol v2 and this app speaks v1'
  return Object.assign(new Error(`HB-VERSION-MISMATCH: ${reason}`), { name: 'HbError', code: 'HB-VERSION-MISMATCH', reason })
}

test('a successful reconnect after a drop: route events again, the close listener is on the new session, backoff resets', catchThrows(async (t) => {
  const clock = fakeClock()
  const sessions = [fakeSession('direct'), fakeSession('direct'), fakeSession('direct')]
  const dialAt = []
  const carried = []
  const manager = new RouteManager({
    findLan: async () => null,
    connectLan: noLanDial,
    connectDht: async () => {
      dialAt.push(clock.now())
      return sessions[dialAt.length - 1]
    },
    localAddresses: () => [OWN_ADDRESS],
    clock: clock.timers
  })
  const events = routesOf(manager, clock)
  manager.on('route', (state, session) => carried.push(session))
  const result = watch(manager.connect())
  await clock.advance(0)
  t.is(result.value, sessions[0], 'the first session is up')

  sessions[0].drop()
  await clock.advance(FIRST_BACKOFF)
  t.is(dialAt.length, 2, 'one reconnect dial, within 1 s of the drop')
  t.is(carried[carried.length - 1], sessions[1], 'the route event after the reconnect carries the new session')
  t.alike(statesOf(events), ['looking', 'direct', 'looking', 'direct'], 'looking, then the route again')

  // The new session has the close listener too: its drop starts a reconnect, and the backoff has reset, so 1 s again.
  const dropAt = clock.now()
  sessions[1].drop()
  await clock.advance(FIRST_BACKOFF)
  t.is(dialAt.length, 3, 'a drop of the new session starts a reconnect')
  const gap = dialAt[2] - dropAt
  t.ok(gap >= FIRST_BACKOFF / 2 && gap <= FIRST_BACKOFF, `the backoff reset: the reconnect waits ${gap} ms, within 1 s`)
  t.is(carried[carried.length - 1], sessions[2], 'the route event after the second reconnect carries its session')
}))

test('a LAN probe that rejects counts as no LAN host: the DHT is dialed', catchThrows(async (t) => {
  const clock = fakeClock()
  const dht = fakeSession('direct')
  let dhtDials = 0
  const manager = new RouteManager({
    findLan: async () => {
      throw new Error('the probe socket could not open')
    },
    connectLan: noLanDial,
    connectDht: async () => {
      dhtDials++
      return dht
    },
    localAddresses: () => [OWN_ADDRESS],
    clock: clock.timers
  })
  const events = routesOf(manager, clock)
  const result = watch(manager.connect())
  await clock.advance(0)
  t.is(result.value, dht, 'connect resolves over the DHT')
  t.is(dhtDials, 1, 'connectDht is dialed once')
  t.alike(statesOf(events), ['looking', 'direct'], 'the route events are looking, then direct')
}))

test('a LAN dial that fails goes on to the DHT', catchThrows(async (t) => {
  const clock = fakeClock()
  const dht = fakeSession('direct')
  const lanCalls = []
  let dhtDials = 0
  const manager = new RouteManager({
    findLan: async () => LAN_HOST,
    connectLan: async (where) => {
      lanCalls.push(where)
      throw lookupFailure()
    },
    connectDht: async () => {
      dhtDials++
      return dht
    },
    localAddresses: () => [OWN_ADDRESS],
    clock: clock.timers
  })
  const events = routesOf(manager, clock)
  const result = watch(manager.connect())
  await clock.advance(0)
  t.alike(lanCalls, [LAN_HOST], 'the LAN host is dialed once')
  t.is(dhtDials, 1, 'then the DHT is dialed')
  t.is(result.value, dht, 'connect resolves over the DHT')
  t.alike(statesOf(events), ['looking', 'direct'], 'no unreachable: the DHT answered at once')
}))

test('HB-VERSION-MISMATCH: connect rejects at once, no timer retries, and an address change searches again', catchThrows(async (t) => {
  const clock = fakeClock()
  let addresses = [OWN_ADDRESS]
  let dhtDials = 0
  const manager = new RouteManager({
    findLan: async () => null,
    connectLan: noLanDial,
    connectDht: async () => {
      dhtDials++
      throw versionMismatch()
    },
    localAddresses: () => addresses,
    clock: clock.timers
  })
  const events = routesOf(manager, clock)
  const result = watch(manager.connect())
  await clock.advance(0)
  t.ok(result.done && result.error, 'connect rejects at once')
  t.is(result.error && result.error.code, 'HB-VERSION-MISMATCH', 'connect rejects with the mismatch code')
  t.is(manager.lastError && manager.lastError.code, 'HB-VERSION-MISMATCH', 'lastError gives the same code')
  t.alike(statesOf(events), ['looking', 'unreachable'], 'the manager is unreachable')

  await clock.advance(TEN_MINUTES)
  t.is(dhtDials, 1, 'no timer retries the mismatch')

  addresses = [MOVED_ADDRESS]
  await clock.advance(POLL_INTERVAL)
  t.is(dhtDials, 2, 'an address change starts a fresh attempt')
  t.is(manager.lastError && manager.lastError.code, 'HB-VERSION-MISMATCH', 'the new attempt fails the same way')
}))

test('connect() rejects with HB-LOOKUP-TIMEOUT at 60 s, and lastError gives the code', catchThrows(async (t) => {
  const clock = fakeClock()
  const manager = new RouteManager({
    findLan: async () => null,
    connectLan: noLanDial,
    connectDht: noAnswer,
    localAddresses: () => [OWN_ADDRESS],
    clock: clock.timers
  })
  const result = watch(manager.connect())
  await clock.advance(LOOKUP_LIMIT - 1)
  t.ok(!result.done, 'connect is still waiting at 59.999 s')
  await clock.advance(1)
  t.ok(result.done && result.error, 'connect rejects at 60 s')
  t.is(result.error && result.error.code, 'HB-LOOKUP-TIMEOUT', 'with the lookup code')
  t.is(manager.lastError && manager.lastError.code, 'HB-LOOKUP-TIMEOUT', 'lastError gives the same code')
}))

test('an offline host: unreachable at once, not after 60 s, and the next search 30 s later', catchThrows(async (t) => {
  const clock = fakeClock()
  const dht = fakeSession('direct')
  const dialAt = []
  const manager = new RouteManager({
    findLan: async () => null,
    connectLan: noLanDial,
    connectDht: () => {
      dialAt.push(clock.now())
      return dialAt.length === 1 ? Promise.reject(lookupFailure()) : Promise.resolve(dht)
    },
    localAddresses: () => [OWN_ADDRESS],
    clock: clock.timers
  })
  const events = routesOf(manager, clock)
  const result = watch(manager.connect())
  await clock.advance(0)
  t.alike(statesOf(events), ['looking', 'unreachable'], 'the failed search reports unreachable at once')
  t.is(result.error && result.error.code, 'HB-LOOKUP-TIMEOUT', 'connect rejects with the lookup code')

  await clock.advance(RETRY_AFTER - 1)
  t.alike(dialAt, [0], 'no search before 30 s')
  await clock.advance(1)
  t.alike(dialAt, [0, RETRY_AFTER], 'the next search starts 30 s after the failure')
  t.alike(statesOf(events), ['looking', 'unreachable', 'looking', 'direct'], 'and it finds the host')
}))

test("a dial's own 60 s timeout starts no backoff on top of the 30 s retry", catchThrows(async (t) => {
  const clock = fakeClock()
  const dialAt = []
  const manager = new RouteManager({
    findLan: async () => null,
    connectLan: noLanDial,
    connectDht: () => {
      dialAt.push(clock.now())
      if (dialAt.length > 1) return noAnswer()
      // The dial's own timer fires at the same 60 s as the search limit.
      return new Promise((_, reject) => clock.timers.setTimeout(() => reject(lookupFailure()), LOOKUP_LIMIT))
    },
    localAddresses: () => [OWN_ADDRESS],
    clock: clock.timers
  })
  const events = routesOf(manager, clock)
  watch(manager.connect())
  await clock.advance(LOOKUP_LIMIT)
  t.alike(statesOf(events), ['looking', 'unreachable'], 'unreachable at 60 s')
  await clock.advance(RETRY_AFTER - 1)
  t.alike(dialAt, [0], 'no search before the 30 s retry')
  await clock.advance(1)
  t.alike(dialAt, [0, LOOKUP_LIMIT + RETRY_AFTER], 'the retry starts 30 s after unreachable')
}))

test('a late rejection of an abandoned dial starts no search and reports nothing', catchThrows(async (t) => {
  const clock = fakeClock()
  const abandoned = deferred()
  const dialAt = []
  let addresses = [OWN_ADDRESS]
  const manager = new RouteManager({
    findLan: async () => null,
    connectLan: noLanDial,
    connectDht: () => {
      dialAt.push(clock.now())
      return dialAt.length === 1 ? abandoned.promise : noAnswer()
    },
    localAddresses: () => addresses,
    clock: clock.timers
  })
  const events = routesOf(manager, clock)
  watch(manager.connect())
  await clock.advance(0)
  addresses = [MOVED_ADDRESS]
  await clock.advance(POLL_INTERVAL)
  t.alike(dialAt, [0, POLL_INTERVAL], 'the change starts a fresh search')

  const before = statesOf(events)
  abandoned.reject(lookupFailure())
  await clock.advance(1000)
  t.alike(statesOf(events), before, 'the late rejection reports no route and no failure')
  t.alike(dialAt, [0, POLL_INTERVAL], 'and starts no retry or reconnect')
}))

test('a live LAN session whose LAN host is gone on a new address is replaced by a DHT session', catchThrows(async (t) => {
  const clock = fakeClock()
  const order = []
  const lan = fakeSession('lan', { name: 'lan', log: order })
  const dht = fakeSession('direct', { name: 'direct', log: order })
  let addresses = [OWN_ADDRESS]
  let lanVisible = true
  let dhtDials = 0
  const manager = new RouteManager({
    findLan: async () => (lanVisible ? LAN_HOST : null),
    connectLan: async () => lan,
    connectDht: async () => {
      dhtDials++
      return dht
    },
    localAddresses: () => addresses,
    clock: clock.timers
  })
  const events = routesOf(manager, clock)
  const result = watch(manager.connect())
  await clock.advance(0)
  t.is(result.value, lan, 'connect resolves over the LAN')

  // The device leaves the house network: the next poll sees a new address, and no LAN host answers.
  addresses = [MOVED_ADDRESS]
  lanVisible = false
  await clock.advance(POLL_INTERVAL)
  t.is(dhtDials, 1, 'the DHT is dialed')
  t.alike(order, ['lan destroyed'], 'the LAN session closes once the DHT session is up')
  t.alike(statesOf(events), ['looking', 'lan', 'looking', 'direct'], 'the route moves from lan to direct')

  await clock.advance(120000)
  t.is(dhtDials, 1, 'the closed LAN session is not a drop: nothing reconnects')
}))

test('an address change while on direct with no LAN host keeps the direct session and dials no DHT', catchThrows(async (t) => {
  const clock = fakeClock()
  const direct = fakeSession('direct')
  let addresses = [OWN_ADDRESS]
  let dhtDials = 0
  const manager = new RouteManager({
    findLan: async () => null,
    connectLan: noLanDial,
    connectDht: async () => {
      dhtDials++
      return direct
    },
    localAddresses: () => addresses,
    clock: clock.timers
  })
  const events = routesOf(manager, clock)
  const result = watch(manager.connect())
  await clock.advance(0)
  t.is(result.value, direct, 'connect resolves over direct')

  // An IPv6 address rotation, say: no LAN host is found, and a fresh DHT dial would replace a healthy session.
  addresses = [MOVED_ADDRESS]
  await clock.advance(POLL_INTERVAL)
  t.is(dhtDials, 1, 'no second DHT dial')
  t.is(direct.destroyCount, 0, 'the direct session stays open')
  t.alike(statesOf(events), ['looking', 'direct', 'looking', 'direct'], 'the search ends on the direct route it kept')
}))

test("route events carry the session that holds the route; looking and unreachable carry none", catchThrows(async (t) => {
  const clock = fakeClock()
  const dht = fakeSession('direct')
  let dhtDials = 0
  const manager = new RouteManager({
    findLan: async () => null,
    connectLan: noLanDial,
    connectDht: () => {
      dhtDials++
      return dhtDials === 1 ? Promise.reject(lookupFailure()) : Promise.resolve(dht)
    },
    localAddresses: () => [OWN_ADDRESS],
    clock: clock.timers
  })
  const seen = []
  manager.on('route', (state, session) => seen.push({ state, session }))
  watch(manager.connect())
  await clock.advance(RETRY_AFTER)
  t.alike(seen.map((e) => e.state), ['looking', 'unreachable', 'looking', 'direct'], 'the route events')
  t.ok(seen.slice(0, 3).every((e) => e.session === undefined), 'looking and unreachable carry no session')
  t.is(seen[3].session, dht, 'the direct event carries the session that holds the route')
}))

test('reconnect delays are jittered: several are below their nominal value', catchThrows(async (t) => {
  const clock = fakeClock()
  const first = fakeSession('direct')
  const dialAt = []
  const manager = new RouteManager({
    findLan: async () => null,
    connectLan: noLanDial,
    connectDht: () => {
      dialAt.push(clock.now())
      return dialAt.length === 1 ? Promise.resolve(first) : Promise.reject(lookupFailure())
    },
    localAddresses: () => [OWN_ADDRESS],
    clock: clock.timers
  })
  watch(manager.connect())
  await clock.advance(0)
  const dropAt = clock.now()
  first.drop()
  await clock.advance(120000)
  const attempts = dialAt.slice(1, 8)
  t.ok(attempts.length === 7, 'seven reconnect attempts in 120 s')
  let previous = dropAt
  let below = 0
  attempts.forEach((at, k) => {
    const nominal = Math.min(FIRST_BACKOFF * 2 ** k, BACKOFF_CAP)
    if (at - previous < nominal) below++
    previous = at
  })
  t.ok(below >= 2, `${below} of the 7 delays are below their nominal value: the jitter is real`)
}))

test('a drop while a search is running starts no second search: the running search decides', catchThrows(async (t) => {
  const clock = fakeClock()
  const lan = fakeSession('lan')
  const pendingDht = deferred()
  const laterDht = fakeSession('direct')
  const dialAt = []
  let addresses = [OWN_ADDRESS]
  let lanVisible = true
  const manager = new RouteManager({
    findLan: async () => (lanVisible ? LAN_HOST : null),
    connectLan: async () => lan,
    connectDht: () => {
      dialAt.push(clock.now())
      return dialAt.length === 1 ? pendingDht.promise : Promise.resolve(laterDht)
    },
    localAddresses: () => addresses,
    clock: clock.timers
  })
  const events = routesOf(manager, clock)
  const result = watch(manager.connect())
  await clock.advance(0)
  t.is(result.value, lan, 'connect resolves over the LAN')

  addresses = [MOVED_ADDRESS]
  lanVisible = false
  await clock.advance(POLL_INTERVAL)
  t.alike(dialAt, [POLL_INTERVAL], 'the change starts a DHT search that is still pending')

  lan.drop()
  await clock.advance(0)
  t.alike(dialAt, [POLL_INTERVAL], 'the drop starts no second search')

  pendingDht.reject(lookupFailure())
  await clock.advance(FIRST_BACKOFF)
  t.is(dialAt.length, 2, 'the running search fails, then one reconnect dial within 1 s')
  t.ok(dialAt[1] - dialAt[0] >= FIRST_BACKOFF / 2 && dialAt[1] - dialAt[0] <= FIRST_BACKOFF, 'after the 1 s backoff')
  t.alike(statesOf(events), ['looking', 'lan', 'looking', 'unreachable', 'looking', 'direct'], 'the route events')
}))

test('connect() after close() rejects and starts no search', catchThrows(async (t) => {
  const clock = fakeClock()
  let findCalls = 0
  let dhtDials = 0
  const manager = new RouteManager({
    findLan: async () => {
      findCalls++
      return null
    },
    connectLan: noLanDial,
    connectDht: () => {
      dhtDials++
      return noAnswer()
    },
    localAddresses: () => [OWN_ADDRESS],
    clock: clock.timers
  })
  manager.close()
  const result = watch(manager.connect())
  await clock.advance(LOOKUP_LIMIT)
  t.ok(result.done && result.error, 'connect rejects')
  t.is(findCalls, 0, 'no LAN search')
  t.is(dhtDials, 0, 'no DHT dial')
}))
