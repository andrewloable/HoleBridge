const test = require('brittle')
const b4a = require('b4a')
const { load, hex } = require('./helpers/vectors.js')
const { derive, deriveRelay } = require('../lib/keys.js')

// A test key and its application key are test values from spec/vectors (Derivation, docs/security.md).
const TEST_KEY = '7KQM4X9TR'

// brittle ends the whole run when a test body throws. Until the IMPL task lands, the code throws
// 'not implemented', so each body runs under catchThrows, which fails only its own test. Bodies are
// async because derive and deriveRelay return promises.
function catchThrows(fn) {
  return async (t) => {
    try {
      await fn(t)
    } catch (err) {
      t.fail(err.message)
    }
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

// The vectors store the normalized key as hex of its ASCII. Assertions name only the vector index,
// never a derived value, so a failing test does not print a key.
function keyOf(entry) {
  return b4a.toString(hex(entry.normalized))
}

test('host and client public keys and the LAN probe key match every key vector', catchThrows(async (t) => {
  const { keys } = load('key-derivation.json')
  t.is(keys.length, 6, 'six key vectors')
  for (const [i, entry] of keys.entries()) {
    const got = await derive(keyOf(entry), hex(entry.appKey))
    t.ok(b4a.equals(got.host.publicKey, hex(entry.publicKeys.host)), `keys[${i}] host public key`)
    t.ok(b4a.equals(got.client.publicKey, hex(entry.publicKeys.client)), `keys[${i}] client public key`)
    t.ok(b4a.equals(got.lan, hex(entry.lanProbeKey)), `keys[${i}] LAN probe key`)
  }
}))

test('server and member public keys match every relay-key vector', catchThrows(async (t) => {
  const { relayKeys } = load('key-derivation.json')
  t.is(relayKeys.length, 6, 'six relay-key vectors')
  for (const [i, entry] of relayKeys.entries()) {
    const got = await deriveRelay(keyOf(entry), hex(entry.appKey))
    t.ok(b4a.equals(got.server.publicKey, hex(entry.publicKeys.server)), `relayKeys[${i}] server public key`)
    t.ok(b4a.equals(got.member.publicKey, hex(entry.publicKeys.member)), `relayKeys[${i}] member public key`)
  }
}))

// The 32-byte control keeps a derive that always throws from passing the first assertion.
test('a 31-byte appKey throws, for derive and deriveRelay', catchThrows(async (t) => {
  for (const fn of [derive, deriveRelay]) {
    const err = await thrownByAsync(() => fn(TEST_KEY, b4a.alloc(31)))
    t.ok(err, `${fn.name}: a 31-byte appKey throws`)
    const control = await thrownByAsync(() => fn(TEST_KEY, b4a.alloc(32)))
    t.ok(!control, `${fn.name}: a 32-byte appKey does not throw`)
  }
}))

test('two different appKeys give different host public keys for the same key', catchThrows(async (t) => {
  const first = await derive(TEST_KEY, b4a.alloc(32))
  const second = await derive(TEST_KEY, b4a.alloc(32, 0xff))
  t.ok(!b4a.equals(first.host.publicKey, second.host.publicKey), 'the host public keys differ')
}))

// A typed or formatted spelling must derive the canonical key pairs, as in the Go suite
// (TestDeriveNormalizesItsInput). Without normalization, lowercase and dashed input gives other keys.
test('derive gives the same host, client and LAN keys for a typed or formatted spelling', catchThrows(async (t) => {
  const canonical = await derive('7KQM4X9TR', b4a.alloc(32))
  const spelled = await derive('7kqm-4x9tr', b4a.alloc(32))
  t.ok(b4a.equals(canonical.host.publicKey, spelled.host.publicKey), 'host public keys match')
  t.ok(b4a.equals(canonical.client.publicKey, spelled.client.publicKey), 'client public keys match')
  t.ok(b4a.equals(canonical.lan, spelled.lan), 'LAN probe keys match')
}))

test('deriveRelay gives the same server and member keys for a typed or formatted spelling', catchThrows(async (t) => {
  const canonical = await deriveRelay('7KQM4X9TR', b4a.alloc(32))
  const spelled = await deriveRelay('7kqm-4x9tr', b4a.alloc(32))
  t.ok(b4a.equals(canonical.server.publicKey, spelled.server.publicKey), 'server public keys match')
  t.ok(b4a.equals(canonical.member.publicKey, spelled.member.publicKey), 'member public keys match')
}))

// An invalid key throws HB-KEY-INVALID with the reason normalize gives, before any Argon2 work, as in
// the Go suite (TestDeriveRejectsInvalidKeys). The cases are a 7-symbol key, a 9-symbol key with U and
// the empty key. Labels give the case index only, so a failing test never prints a key.
test('derive and deriveRelay reject an invalid key with HB-KEY-INVALID and its reason', catchThrows(async (t) => {
  const cases = [
    ['7KQM4X9', 'length'],
    ['7KQM4X9TU', 'character'],
    ['', 'length']
  ]
  for (const fn of [derive, deriveRelay]) {
    for (const [i, [key, reason]] of cases.entries()) {
      const err = await thrownByAsync(() => fn(key, b4a.alloc(32)))
      t.is(err && err.code, 'HB-KEY-INVALID', `${fn.name}: case ${i} code`)
      t.is(err && err.reason, reason, `${fn.name}: case ${i} reason`)
    }
  }
}))
