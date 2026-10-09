// Tests for the LAN probes and the sliced sweep (lib/lan-probe.js): the probe and reply codec, and findHost.
// Rules: docs/architecture.md (LAN route: Discovery, Probe and reply bytes) and the vectors in
// spec/vectors/lan-probe.json. The MAC key is the lan seed from lib/keys.js derive (docs/security.md).
//
// Shapes the IMPL task must meet (also stated in lib/lan-probe.js):
// - socketFactory() returns a udx-native socket. The tests use only bind, trySend(buf, port, host),
//   on('message', (msg, from)) and close(). findHost makes one socket per slice and closes each before it
//   resolves.
// - timers is { setTimeout, clearTimeout }. findHost waits only through it.
// - The sweep covers the host addresses .1 to .254 of each local /24, except the local addresses. Only
//   localAddresses sets the range. The fake-socket tests use 192.0.2.x (TEST-NET-1, RFC 5737); the
//   real-socket test uses 127.0.0.x. No real LAN address is ever probed.
const test = require('brittle')
const b4a = require('b4a')
const sodium = require('sodium-universal')
const { load, hex } = require('./helpers/vectors.js')
const { derive } = require('../lib/keys.js')
const { encodeProbe, verifyReply, findHost } = require('../lib/lan-probe.js')

// The key of keys[0] in spec/vectors/key-derivation.json, with that entry's application key.
const TEST_KEY = '7KQM4X9TR'
const OWN = '192.0.2.10' // the device's own address, in the sweep's /24
const LAST_KNOWN = '192.0.2.20' // a host the device knew before
const UDP_PORT = 8000 // the responder's UDP port in the fake network
const LAN_TCP_PORT = 8443 // the LAN TCP port a fake responder reports
const SETTLE = 10000 // real-time bound for the real-socket test

// brittle ends the whole run when a test body throws. Until the IMPL task lands, the code throws
// 'not implemented', so each body runs under catchThrows, which fails only its own test. Bodies are
// async because findHost is a promise.
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

// within waits for promise, and fails with what if it has not settled within SETTLE (real time).
function within(promise, what) {
  let timer
  const timeout = new Promise((resolve, reject) => {
    timer = setTimeout(() => reject(new Error(`${what} did not finish within ${SETTLE} ms`)), SETTLE)
  })
  return Promise.race([promise, timeout]).finally(() => clearTimeout(timer))
}

// The lan probe key: the lan seed of the test key from lib/keys.js derive. Derived once, because Argon2 is
// slow. The vector's lanProbeKey for keys[0] is the same value, checked in keys-derive.test.js.
let lanKeyCache = null
function lanKey() {
  if (!lanKeyCache) {
    const appKey = hex(load('key-derivation.json').keys[0].appKey)
    lanKeyCache = derive(TEST_KEY, appKey).then((pairs) => pairs.lan)
  }
  return lanKeyCache
}

// mac(key, bytes) is the BLAKE2b-256 MAC of bytes keyed with key (docs/architecture.md, Probe and reply bytes).
function mac(key, bytes) {
  const out = b4a.alloc(32)
  sodium.crypto_generichash(out, bytes, key)
  return out
}

function flipped(buf, index, mask) {
  const out = b4a.from(buf)
  out[index] ^= mask
  return out
}

const PROBE_MAGIC = b4a.from('HBLANQ1')
const REPLY_MAGIC = b4a.from('HBLANR1')

// fakeReply(key, probe, port) is the reply a responder sends for a valid probe, or null for one it stays
// silent about. It is written from the byte table in docs/architecture.md, not from lib/lan-probe.js, so
// the fake does not check the codec against itself. It stands in for the Go responder.
function fakeReply(key, probe, port) {
  if (probe.length !== 256 || !b4a.equals(probe.subarray(0, 7), PROBE_MAGIC) || probe[7] !== 1) return null
  if (!b4a.equals(mac(key, probe.subarray(0, 32)), probe.subarray(32, 64))) return null
  const reply = b4a.alloc(64)
  reply.set(REPLY_MAGIC, 0)
  reply[7] = 1
  reply.set(probe.subarray(8, 24), 8)
  reply[24] = port & 0xff
  reply[25] = port >> 8
  reply.set(mac(key, reply.subarray(0, 32)), 32)
  return reply
}

// expectedSweep(own) is the sorted list of the .1 to .254 addresses of own's /24, without own.
function expectedSweep(own) {
  const prefix = own.split('.').slice(0, 3).join('.')
  const out = []
  for (let i = 1; i <= 254; i++) {
    const addr = `${prefix}.${i}`
    if (addr !== own) out.push(addr)
  }
  return out.sort()
}

// fakeClock() -> { timers, now(), runUntil(done) }. Timers run in virtual time order. Between two timers the
// promise chains of the code under test run to completion (flush), so a timer that a resolved promise
// schedules is in place before the next one fires.
function fakeClock() {
  let now = 0
  let lastId = 0
  const pending = new Map() // id -> { at, fn }, in scheduling order
  const flush = async () => {
    for (let i = 0; i < 100; i++) await null
  }
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
  // runUntil(done) fires timers in time order until done() holds or no timer is pending.
  async function runUntil(done) {
    await flush()
    for (let i = 0; i < 100000 && !done() && pending.size > 0; i++) {
      let next = null
      for (const [id, timer] of pending) {
        if (next === null || timer.at < pending.get(next).at) next = id
      }
      const { at, fn } = pending.get(next)
      pending.delete(next)
      now = at
      fn()
      await flush()
    }
  }
  return { timers, now: () => now, runUntil }
}

// fakeNetwork(clock, answer) -> { socketFactory, sockets, sends }. Each socket records its sends with the
// virtual time. answer(host, port, probe) returns the reply a fake responder sends back, or null for
// silence. The reply arrives one microtask after trySend, as a network delivers it after the send returns.
function fakeNetwork(clock, answer) {
  const sockets = []
  const sends = [] // every send, in order: { at, host, port, buf }
  function socketFactory() {
    const socket = {
      closed: false,
      sent: [],
      handlers: [],
      bind() {},
      on(event, fn) {
        if (event === 'message') socket.handlers.push(fn)
      },
      trySend(buf, port, host) {
        const send = { at: clock.now(), host, port, buf }
        socket.sent.push(send)
        sends.push(send)
        const reply = answer(host, port, buf)
        if (reply) {
          Promise.resolve().then(() => {
            for (const fn of socket.handlers) fn(reply, { host, family: 4, port })
          })
        }
      },
      close() {
        socket.closed = true
        return Promise.resolve()
      }
    }
    sockets.push(socket)
    return socket
  }
  return { socketFactory, sockets, sends }
}

// track(promise, clock) records how a promise settles and the virtual time it does so.
function track(promise, clock) {
  const out = { done: false, value: undefined, error: null, at: null }
  const settle = (fields) => Object.assign(out, fields, { done: true, at: clock.now() })
  promise.then(
    (value) => settle({ value }),
    (error) => settle({ error })
  )
  return out
}

test('encodeProbe equals every probe vector', catchThrows(async (t) => {
  const { probes } = load('lan-probe.json')
  t.ok(probes.length >= 3, 'the vector has at least 3 probes')
  for (const [i, p] of probes.entries()) {
    const got = encodeProbe(hex(p.lanKey), hex(p.nonce), p.timestampMs)
    t.is(got.length, 256, `probes[${i}] is 256 bytes`)
    t.ok(b4a.equals(got, hex(p.hex)), `probes[${i}] matches the vector bytes`)
  }
}))

test('verifyReply accepts each reply vector and rejects every invalid entry', catchThrows(async (t) => {
  const { replies, invalid } = load('lan-probe.json')
  t.ok(replies.length >= 3 && invalid.length >= 6, 'the vector has the replies and the invalid cases')
  for (const [i, r] of replies.entries()) {
    t.is(verifyReply(hex(r.lanKey), hex(r.hex), hex(r.nonce)), r.port, `replies[${i}] verifies and gives its port`)
  }
  // Every invalid entry is a probe or a broken probe, never a reply. Each must fail as a reply, with the
  // nonce the probe carries at bytes 8 to 23.
  for (const [i, entry] of invalid.entries()) {
    const buf = hex(entry.hex)
    t.is(verifyReply(hex(entry.lanKey), buf, buf.subarray(8, 24)), null, `invalid[${i}] (${entry.reason}) is rejected`)
  }
  // Single-fault copies of a valid reply: each must be rejected, so a verifier that accepts any 64 bytes fails.
  const [first] = replies
  const key = hex(first.lanKey)
  const nonce = hex(first.nonce)
  const good = hex(first.hex)
  const faults = {
    '63 bytes': good.subarray(0, 63),
    '65 bytes': b4a.concat([good, b4a.alloc(1)]),
    'a changed port byte': flipped(good, 24, 0x01),
    'a flipped MAC bit': flipped(good, 40, 0x01),
    'a changed magic byte': flipped(good, 0, 0x20),
    'version 2': flipped(good, 7, 0x03)
  }
  for (const [name, buf] of Object.entries(faults)) {
    t.is(verifyReply(key, buf, nonce), null, `a reply with ${name} is rejected`)
  }
  t.is(verifyReply(key, good, hex('ff'.repeat(16))), null, 'a reply for another nonce is rejected')
  t.is(verifyReply(hex(replies[2].lanKey), good, nonce), null, 'a reply MACed under another key is rejected')
}))

test('a /24 sweep uses 8 sockets of at most 32 probes, 100 ms apart, never the device own address', catchThrows(async (t) => {
  const key = await lanKey()
  const clock = fakeClock()
  const net = fakeNetwork(clock, () => null)
  const out = track(
    findHost({ lanKey: key, lastKnown: [], localAddresses: [OWN], port: UDP_PORT, socketFactory: net.socketFactory, timers: clock.timers }),
    clock
  )
  await clock.runUntil(() => out.done)
  if (out.error) throw out.error
  t.ok(out.done, 'the sweep finishes')
  t.is(out.value, null, 'no responder gives null')
  t.is(net.sockets.length, 8, '254 addresses in slices of 32 use 8 sockets')
  const hosts = net.sends.map((s) => s.host)
  t.is(hosts.length, 253, 'each address of the /24 except the device own address is probed once')
  t.alike([...new Set(hosts)].sort(), expectedSweep(OWN), 'the probed set is .1 to .254 without the own address')
  t.ok(net.sends.every((s) => s.port === UDP_PORT), 'every probe goes to the responder port')
  for (const [i, socket] of net.sockets.entries()) {
    const at = socket.sent.length > 0 ? socket.sent[0].at : null
    t.ok(socket.sent.length > 0 && socket.sent.length <= 32, `slice ${i} has at most 32 probes`)
    t.ok(socket.sent.every((s) => s.at === at), `slice ${i} is sent at one time`)
    t.is(at - net.sockets[0].sent[0].at, i * 100, `slice ${i} starts 100 ms after slice ${i - 1}`)
    t.ok(socket.closed, `slice ${i} socket is closed`)
  }
}))

test('a last-known address that answers is found without any sweep', catchThrows(async (t) => {
  const key = await lanKey()
  const clock = fakeClock()
  const answer = (host, port, probe) =>
    host === LAST_KNOWN && port === UDP_PORT ? fakeReply(key, probe, LAN_TCP_PORT) : null
  const net = fakeNetwork(clock, answer)
  const out = track(
    findHost({ lanKey: key, lastKnown: [LAST_KNOWN], localAddresses: [OWN], port: UDP_PORT, socketFactory: net.socketFactory, timers: clock.timers }),
    clock
  )
  await clock.runUntil(() => out.done)
  if (out.error) throw out.error
  t.ok(out.done, 'the host is found')
  t.ok(out.done && out.at < 300, 'the verified reply ends the wait before the 300 ms window closes')
  t.alike(out.value, { address: LAST_KNOWN, port: LAN_TCP_PORT }, 'the result is the answering address and its LAN port')
  t.is(net.sends.length, 1, 'only the lone probe was sent: no sweep')
  t.is(net.sends[0] && net.sends[0].host, LAST_KNOWN, 'the lone probe went to the last-known address')
}))

test('against a JS responder on 127.0.0.1 the sweep finds the host', catchThrows(async (t) => {
  const key = await lanKey()
  const UDX = require('udx-native') // loaded here so a broken native addon fails only this test
  const udx = new UDX()
  const opened = []
  const socketFactory = () => {
    const socket = udx.createSocket()
    opened.push(socket)
    return socket
  }
  const responder = udx.createSocket()
  responder.on('message', (msg, from) => {
    const reply = fakeReply(key, msg, LAN_TCP_PORT)
    if (reply) responder.trySend(reply, from.port, from.host)
  })
  responder.bind(0, '127.0.0.1')
  const port = responder.address().port
  try {
    // The own address is 127.0.0.5, so the sweep covers 127.0.0.1 to 127.0.0.254 and reaches the responder.
    const found = await within(
      findHost({ lanKey: key, lastKnown: [], localAddresses: ['127.0.0.5'], port, socketFactory, timers: { setTimeout, clearTimeout } }),
      'findHost'
    )
    t.alike(found, { address: '127.0.0.1', port: LAN_TCP_PORT }, 'the sweep finds the responder on 127.0.0.1')
    t.ok(opened.every((s) => s.closing), 'every sweep socket is closed')
  } finally {
    for (const socket of [responder, ...opened]) {
      if (!socket.closing) await socket.close().catch(noop)
    }
  }
}))

test('with no responder findHost resolves null after the sweep wait', catchThrows(async (t) => {
  const key = await lanKey()
  const clock = fakeClock()
  const net = fakeNetwork(clock, () => null)
  const out = track(
    findHost({ lanKey: key, lastKnown: [LAST_KNOWN], localAddresses: [OWN], port: UDP_PORT, socketFactory: net.socketFactory, timers: clock.timers }),
    clock
  )
  await clock.runUntil(() => out.done)
  if (out.error) throw out.error
  t.ok(out.done, 'findHost settles')
  t.is(out.value, null, 'resolves null')
  const [lone, ...sweep] = net.sends
  t.is(lone && lone.host, LAST_KNOWN, 'the last-known address is probed first, alone')
  t.is(sweep.length > 0 && sweep[0].at - lone.at, 300, 'the sweep starts after the 300 ms wait for the lone probe')
  const last = sweep.length > 0 ? sweep[sweep.length - 1].at : null
  t.is(out.at - last, 800, 'resolves 800 ms after the last slice')
  t.ok(net.sockets.every((s) => s.closed), 'every socket is closed')
}))
