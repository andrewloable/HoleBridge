// Tests for the TV handoff (lib/handoff.js): the link, the box plaintext, the listener on the TV and the
// sender on the phone, all over 127.0.0.1. Rules: docs/architecture.md ("Adding a host to a TV (handoff)")
// and decision D36. Expected bytes come from spec/vectors/handoff.json and spec/vectors/links.json, not from
// the code under test. Test values only.
const test = require('brittle')
const b4a = require('b4a')
const sodium = require('sodium-universal')
const TCP = require('bare-tcp')
const { load, hex } = require('./helpers/vectors.js')
const { createEchoServer } = require('./helpers/fake-host.js')
const { encodeLink, parseLink, encodePlaintext, listen, send } = require('../lib/handoff.js')

const LOOPBACK = '127.0.0.1'
const FIVE_SECONDS = 5 * 1000
const FIVE_MINUTES = 5 * 60 * 1000
// SETTLE bounds every wait, so a listener or socket that never answers fails its test instead of hanging it.
const SETTLE = 10000
// FAST bounds a close that must happen at once, without the fake clock moving.
const FAST = 2000

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

const delay = (ms) => new Promise((resolve) => setTimeout(resolve, ms))

// within waits for promise, and fails with what if it has not settled within limit.
function within(promise, what, limit = SETTLE) {
  let timer
  const timeout = new Promise((resolve, reject) => {
    timer = setTimeout(() => reject(new Error(`${what} did not finish within ${limit} ms`)), limit)
  })
  return Promise.race([promise, timeout]).finally(() => clearTimeout(timer))
}

// until polls check until it resolves true, and fails with what if that takes longer than SETTLE.
async function until(check, what) {
  const deadline = Date.now() + SETTLE
  while (!(await check())) {
    if (Date.now() > deadline) throw new Error(`${what} did not happen within ${SETTLE} ms`)
    await delay(20)
  }
}

// settle turns a promise into its outcome, so a rejection is read without an unhandled rejection.
function settle(promise) {
  return promise.then(
    (value) => ({ ok: true, value }),
    (error) => ({ ok: false, error })
  )
}

// watch attaches to a promise at once, so its rejection is handled from the start. done resolves with the
// outcome, and settled() says whether the promise has settled yet.
function watch(promise) {
  let outcome = null
  const done = settle(promise).then((result) => {
    outcome = result
    return result
  })
  return { done, settled: () => outcome !== null }
}

// unwrap returns the value of an outcome, or throws its error.
function unwrap(outcome) {
  if (!outcome.ok) throw outcome.error
  return outcome.value
}

// rejectionOf resolves to the error promise rejects with, and fails if it resolves. A stub's 'not
// implemented' is rethrown, so the test reports the stub and not a mismatched code.
async function rejectionOf(promise, what) {
  const outcome = await within(settle(promise), what)
  if (outcome.ok) throw new Error(`${what} resolved; it should reject`)
  if (outcome.error.message === 'not implemented') throw outcome.error
  return outcome.error
}

// fakeClock() is the clock listen takes: setTimeout and clearTimeout, which run only when the test calls
// advance. pending() lists the delays of the timers still waiting, so a test can see a deadline was set.
function fakeClock() {
  let now = 0
  const timers = new Set()
  return {
    setTimeout(fn, ms) {
      const timer = { due: now + ms, ms, fn }
      timers.add(timer)
      return timer
    },
    clearTimeout(timer) {
      timers.delete(timer)
    },
    advance(ms) {
      now += ms
      const due = [...timers].filter((timer) => timer.due <= now).sort((a, b) => a.due - b.due)
      for (const timer of due) {
        if (timers.delete(timer)) timer.fn()
      }
    },
    pending() {
      return [...timers].map((timer) => timer.ms)
    }
  }
}

// opened holds the sockets a test dialed. hangUp destroys them, so a failed test leaves no socket open.
const opened = new Set()

function hangUp() {
  for (const socket of opened) socket.destroy()
  opened.clear()
}

// dial connects to the listener on 127.0.0.1. connected resolves once the connection is up. closed resolves
// with the bytes the listener wrote, once the connection ends, closes or errors.
function dial(port) {
  const socket = TCP.createConnection(port, LOOPBACK)
  const bytes = []
  opened.add(socket)
  socket.on('data', (chunk) => bytes.push(chunk))
  socket.on('error', noop)
  socket.connected = new Promise((resolve, reject) => {
    socket.once('connect', () => resolve(socket))
    socket.once('error', reject)
  })
  socket.connected.catch(noop)
  socket.closed = new Promise((resolve) => {
    const done = () => resolve(b4a.concat(bytes))
    socket.once('end', done)
    socket.once('close', done)
    socket.once('error', done)
  })
  return socket
}

// listening resolves true when a dial to port connects, and false when it is refused.
function listening(port) {
  return new Promise((resolve) => {
    const socket = TCP.createConnection(port, LOOPBACK)
    socket.on('error', () => resolve(false))
    socket.once('connect', () => {
      socket.destroy()
      resolve(true)
    })
  })
}

// sealTo seals plaintext to the TV's public key, as the phone does (crypto_box_seal).
function sealTo(publicKeyHex, plaintext) {
  const box = b4a.alloc(plaintext.length + sodium.crypto_box_SEALBYTES)
  sodium.crypto_box_seal(box, plaintext, hex(publicKeyHex))
  return box
}

test('encodeLink and parseLink match every handoffLinks entry in spec/vectors/links.json; invalid ones throw', catchThrows(async (t) => {
  const v = load('links.json')
  for (const entry of v.handoffLinks) {
    const { link, ...fields } = entry
    t.is(encodeLink(v.base, fields), link, `encodeLink builds ${link}`)
    t.alike(parseLink(link), fields, `parseLink reads ${link}`)
  }
  for (const entry of v.invalid) {
    t.exception(() => parseLink(entry.link), `parseLink throws: ${entry.reason}`)
  }
}))

test('the box plaintext encoding matches spec/vectors/handoff.json; the vector box opens with the vector key pair', catchThrows(async (t) => {
  const v = load('handoff.json')
  for (const { value, hex: expected } of v.plaintexts) {
    t.is(b4a.toString(encodePlaintext(value), 'hex'), expected, `plaintext for ${value.name}`)
  }
  const pair = v.keyPair
  const publicKey = b4a.alloc(sodium.crypto_box_PUBLICKEYBYTES)
  const secretKey = b4a.alloc(sodium.crypto_box_SECRETKEYBYTES)
  sodium.crypto_box_seed_keypair(publicKey, secretKey, hex(pair.seed))
  t.is(b4a.toString(publicKey, 'hex'), pair.publicKey, 'the seed gives the vector public key')
  t.is(b4a.toString(secretKey, 'hex'), pair.secretKey, 'the seed gives the vector secret key')
  const box = v.boxes[0]
  const sealed = hex(box.hex)
  const opened = b4a.alloc(sealed.length - sodium.crypto_box_SEALBYTES)
  t.ok(sodium.crypto_box_seal_open(opened, sealed, publicKey, secretKey), 'the vector box opens')
  t.is(b4a.toString(opened, 'hex'), v.plaintexts[box.plaintextIndex].hex, 'and holds the vector plaintext')
}))

test('listen + send over 127.0.0.1 delivers name, key and appKey, and the listener closes', catchThrows(async (t) => {
  const v = load('handoff.json')
  const { name, key, appKey: appKeyHex } = v.plaintexts[0].value
  const clock = fakeClock()
  const l = await listen({ addresses: [LOOPBACK], clock })
  const received = watch(l.received)
  try {
    const { port } = parseLink(l.link)
    await within(send(l.link, { name, key, appKey: hex(appKeyHex) }), 'send')
    const value = unwrap(await within(received.done, 'received'))
    t.is(value.name, name, 'name is delivered')
    t.is(value.key, key, 'key is delivered')
    t.is(b4a.toString(value.appKey, 'hex'), appKeyHex, 'appKey is delivered')
    await until(async () => !(await listening(port)), 'the listener closing')
    t.pass('nothing listens on the port after the box is accepted')
  } finally {
    await l.cancel()
    hangUp()
  }
}))

test('A box with the wrong secret gets byte 0 and the listener keeps waiting', catchThrows(async (t) => {
  const v = load('handoff.json')
  const { name, key, appKey: appKeyHex } = v.plaintexts[0].value
  const good = v.plaintexts[0].hex
  // The vector plaintext with another secret: version byte, then the 16 secret bytes, then the rest as is.
  const wrong = hex('01' + 'ff'.repeat(16) + good.slice(2 + 32))
  const clock = fakeClock()
  const l = await listen({ addresses: [LOOPBACK], clock })
  const received = watch(l.received)
  try {
    const { publicKey, port } = parseLink(l.link)
    const raw = dial(port)
    await within(raw.connected, 'connect')
    raw.write(sealTo(publicKey, wrong))
    raw.end()
    const reply = await within(raw.closed, 'the reply to a wrong box')
    t.is(reply.length, 1, 'one reply byte')
    t.is(reply[0], 0, 'byte 0 for a box with the wrong secret')
    t.is(received.settled(), false, 'the listener is still waiting')

    const badLink = encodeLink(load('links.json').base, { publicKey, secret: 'ff'.repeat(16), port, addresses: [LOOPBACK] })
    const refusal = await rejectionOf(send(badLink, { name, key, appKey: hex(appKeyHex) }), 'send with a wrong secret')
    t.is(refusal.name, 'HbError', 'the phone gets an HbError')
    t.is(refusal.reason, 'refused', 'with reason refused')
    t.is(refusal.code, 'HB-HANDOFF-REFUSED', 'with code HB-HANDOFF-REFUSED')

    await within(send(l.link, { name, key, appKey: hex(appKeyHex) }), 'send with the right secret')
    const value = unwrap(await within(received.done, 'received'))
    t.is(value.name, name, 'the right box is still accepted after the wrong one')
  } finally {
    await l.cancel()
    hangUp()
  }
}))

test('a connection sending 2 KiB is closed without a reply', catchThrows(async (t) => {
  const clock = fakeClock()
  const l = await listen({ addresses: [LOOPBACK], clock })
  try {
    const { port } = parseLink(l.link)
    const sock = dial(port)
    await within(sock.connected, 'connect')
    sock.write(b4a.alloc(2048, 0xaa))
    const reply = await within(sock.closed, 'the 2 KiB connection to close', FAST)
    t.is(reply.length, 0, 'closed with no reply byte')
  } finally {
    await l.cancel()
    hangUp()
  }
}))

test('a connection silent for 5 s is closed (the fake clock stands in for the deadline)', catchThrows(async (t) => {
  const clock = fakeClock()
  const l = await listen({ addresses: [LOOPBACK], clock })
  try {
    const { port } = parseLink(l.link)
    const sock = dial(port)
    await within(sock.connected, 'connect')
    await until(async () => clock.pending().includes(FIVE_SECONDS), 'the 5 s read deadline is set')
    clock.advance(FIVE_SECONDS)
    const reply = await within(sock.closed, 'the silent connection to close')
    t.is(reply.length, 0, 'closed with no reply byte')
  } finally {
    await l.cancel()
    hangUp()
  }
}))

test('a 5th simultaneous connection is closed at once', catchThrows(async (t) => {
  const clock = fakeClock()
  const l = await listen({ addresses: [LOOPBACK], clock })
  try {
    const { port } = parseLink(l.link)
    const held = []
    for (let i = 0; i < 4; i++) {
      const sock = dial(port)
      await within(sock.connected, `connection ${i + 1}`)
      held.push(sock)
    }
    let heldClosed = 0
    for (const sock of held) sock.closed.then(() => heldClosed++)
    await until(async () => clock.pending().filter((ms) => ms === FIVE_SECONDS).length === 4, 'four read deadlines set')
    const fifth = dial(port)
    const reply = await within(fifth.closed, 'the 5th connection to close', FAST)
    t.is(reply.length, 0, 'the 5th connection gets no reply')
    await delay(100)
    t.is(heldClosed, 0, 'the first four connections stay open')
  } finally {
    await l.cancel()
    hangUp()
  }
}))

test('after 5 minutes (fake clock) received rejects with HB-HANDOFF-EXPIRED', catchThrows(async (t) => {
  const clock = fakeClock()
  const l = await listen({ addresses: [LOOPBACK], clock })
  const received = watch(l.received)
  try {
    clock.advance(FIVE_MINUTES - 1)
    await delay(10)
    t.is(received.settled(), false, 'still waiting just before 5 minutes')
    clock.advance(1)
    const err = await rejectionOf(l.received, 'received after 5 minutes')
    t.is(err.code, 'HB-HANDOFF-EXPIRED', 'rejects with HB-HANDOFF-EXPIRED')
  } finally {
    await l.cancel()
    hangUp()
  }
}))

test('send to a link whose port is closed rejects with HB-HANDOFF-UNREACHABLE', catchThrows(async (t) => {
  const v = load('links.json')
  const { publicKey, secret } = v.handoffLinks[0]
  const { name, key, appKey } = load('handoff.json').plaintexts[0].value
  // A port nothing listens on: an echo server that is closed again at once.
  const echo = await createEchoServer()
  const port = echo.port
  await echo.close()
  const link = encodeLink(v.base, { publicKey, secret, port, addresses: [LOOPBACK] })
  const err = await rejectionOf(send(link, { name, key, appKey: hex(appKey) }), 'send to a closed port')
  t.is(err.code, 'HB-HANDOFF-UNREACHABLE', 'rejects with HB-HANDOFF-UNREACHABLE')
  hangUp()
}))
