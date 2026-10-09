// Writes spec/vectors/key-derivation.json: the key derivation v1 of docs/security.md (Derivation)
// for host keys and relay keys, with every intermediate value, computed by sodium-universal 5.0.1.
// It also checks spec/vectors/key.json, the hand-written normalization cases, against the rules in
// docs/security.md#the-key.
//
// Test values only. The keys are 7KQM4X9TR, HJKMNPQRS and 0123ABCDE, and the application keys are
// bytes 0x00 to 0x1f and 0xff repeated 32 times. None of them is a deployment's secret.
//
// PROVISIONAL: the derivation freezes only when the owner commits these vectors, and that waits for
// HoleBridge-3vg to run or for the owner to accept the proposed Argon2id cost. Every value is
// deterministic, so a second run leaves the file byte-identical.
//
// Run: cd spec/gen && npm ci && node derive.js

'use strict'

const fs = require('fs')
const path = require('path')
const assert = require('assert/strict')
const sodium = require('sodium-universal')

const VECTORS = path.join(__dirname, '..', 'vectors')

// The Argon2id cost, written as numbers. MEM is in bytes: 64 MiB.
const OPS = 3
const MEM = 64 * 1024 * 1024

const ALPHABET = '0123456789ABCDEFGHJKMNPQRSTVWXYZ'
const KEYS = ['7KQM4X9TR', 'HJKMNPQRS', '0123ABCDE']
const APP_KEYS = ['000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f', 'ff'.repeat(32)]

// Host keys and relay keys differ only in their labels and roles.
const HOST = { saltLabel: 'holebridge key v1 salt', roleLabel: 'holebridge key v1 ', roles: ['host', 'client', 'lan'] }
const RELAY = { saltLabel: 'holebridge relay v1 salt', roleLabel: 'holebridge relay v1 ', roles: ['server', 'member'] }

const DESCRIPTION =
  'PROVISIONAL, not frozen: do not commit this file until the owner runs HoleBridge-3vg or accepts the ' +
  'proposed Argon2id cost (docs/security.md, Derivation); committing freezes the derivation. Derivation v1 ' +
  'from docs/security.md: salt = BLAKE2b-128(label salt || appKey); root = Argon2id13(normalized key, salt, ' +
  'OPS, MEM) with OPS 3 and MEM 67108864 bytes (64 MiB); seed(role) = BLAKE2b-256(label role || root), keyed ' +
  'with appKey; public key = Ed25519 seed key pair. lanProbeKey is the lan seed itself, the key that MACs LAN probes and replies (docs/architecture.md, LAN probe). Host keys use the holebridge key v1 labels and the roles ' +
  'host, client and lan; relay keys use the holebridge relay v1 labels and the roles server and member. Every ' +
  'byte value is lowercase hex. normalized is the ASCII of the 9-symbol key, which is 7KQM4X9TR, HJKMNPQRS or ' +
  '0123ABCDE. Application keys: bytes 0x00 to 0x1f, and 0xff repeated 32 times. Test values only. Written by ' +
  'spec/gen/derive.js.'

// The normalization rules of docs/security.md#the-key. Returns { normalized } for a valid input, or
// { error } with its one fault, "character" or "length". An input with both faults has no defined
// error here, so it throws: the vectors must not depend on the order of the checks.
const CONFUSABLE = { O: '0', I: '1', L: '1' }
function normalize(input) {
  // Only ASCII letters fold to upper case, as in the three suites. toUpperCase would read the dotless
  // i and the long s as I and S, which are key symbols.
  const symbols = [...input.replace(/[a-z]/g, (c) => c.toUpperCase()).replace(/[- ]/g, '')].map((c) => CONFUSABLE[c] || c)
  const badChar = symbols.some((c) => !ALPHABET.includes(c))
  const badLength = symbols.length !== 9
  if (badChar && badLength) throw new Error(`a case has two faults, so its error is not defined: ${JSON.stringify(input)}`)
  if (badChar) return { error: 'character' }
  if (badLength) return { error: 'length' }
  return { normalized: symbols.join('') }
}

// Every expected value in key.json must follow the rules above. Returns the number of cases.
function checkKeyCases() {
  const cases = JSON.parse(fs.readFileSync(path.join(VECTORS, 'key.json'), 'utf8'))
  for (const c of cases.valid) {
    assert.deepEqual(normalize(c.input), { normalized: c.normalized }, `valid case ${JSON.stringify(c.input)}`)
  }
  for (const c of cases.invalid) {
    assert.deepEqual(normalize(c.input), { error: c.error }, `invalid case ${JSON.stringify(c.input)}`)
  }
  const count = cases.valid.length + cases.invalid.length
  assert.ok(count >= 30, 'key.json has at least 30 cases')
  return count
}

const hex = (buf) => buf.toString('hex')

// Derivation v1 for one family of labels: a salt and a root from the normalized key, then one Ed25519
// key pair per role.
function derive(family, normalized, appKeyHex) {
  const appKey = Buffer.from(appKeyHex, 'hex')

  const salt = Buffer.alloc(16)
  sodium.crypto_generichash(salt, Buffer.concat([Buffer.from(family.saltLabel, 'ascii'), appKey]))

  const root = Buffer.alloc(32)
  sodium.crypto_pwhash(root, Buffer.from(normalized, 'ascii'), salt, OPS, MEM, sodium.crypto_pwhash_ALG_ARGON2ID13)

  const seeds = {}
  const publicKeys = {}
  for (const role of family.roles) {
    const seed = Buffer.alloc(32)
    sodium.crypto_generichash(seed, Buffer.concat([Buffer.from(family.roleLabel + role, 'ascii'), root]), appKey)
    const publicKey = Buffer.alloc(sodium.crypto_sign_PUBLICKEYBYTES)
    const secretKey = Buffer.alloc(sodium.crypto_sign_SECRETKEYBYTES)
    sodium.crypto_sign_seed_keypair(publicKey, secretKey, seed)
    seeds[role] = hex(seed)
    publicKeys[role] = hex(publicKey)
  }
  return { salt: hex(salt), root: hex(root), seeds, publicKeys }
}

// One vector per key and application key.
function vectorsFor(family) {
  return KEYS.flatMap((key) =>
    APP_KEYS.map((appKey) => ({
      appKey,
      normalized: hex(Buffer.from(normalize(key).normalized, 'ascii')),
      ...derive(family, key, appKey)
    }))
  )
}

function main() {
  const checked = checkKeyCases()
  for (const key of KEYS) assert.deepEqual(normalize(key), { normalized: key }, `test key ${key} is canonical`)

  const out = {
    description: DESCRIPTION,
    reference: 'sodium-universal 5.0.1: crypto_generichash, crypto_pwhash (ALG_ARGON2ID13), crypto_sign_seed_keypair',
    OPS,
    MEM,
    keys: vectorsFor(HOST).map((v) => ({ ...v, lanProbeKey: v.seeds.lan })),
    relayKeys: vectorsFor(RELAY)
  }
  assert.equal(out.keys.length, 6, 'six key vectors')
  assert.equal(out.relayKeys.length, 6, 'six relay-key vectors')

  fs.writeFileSync(path.join(VECTORS, 'key-derivation.json'), JSON.stringify(out, null, 2) + '\n')
  console.log(`key-derivation.json: ${out.keys.length} key and ${out.relayKeys.length} relay-key vectors; key.json: ${checked} cases checked`)
}

main()
