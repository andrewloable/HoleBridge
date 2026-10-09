// Key normalization, formatting and derivation for the app engine: a typed key becomes the 9 canonical
// symbols (docs/security.md, "The key"), and derive turns it into key pairs (docs/security.md, "Derivation").

const b4a = require('b4a')
const sodium = require('sodium-universal')

// Crockford Base32 without I, L, O and U. Only the symbols are allowed after O, I and L are read as 0
// and 1.
const SYMBOLS = /^[0-9A-HJKMNP-TV-Z]*$/
const READ_AS = { O: '0', I: '1', L: '1' }

// HbError carries a code from lib/errors.gen.js and the reason for the fault. It never holds the input,
// because the input is a key.
class HbError extends Error {
  constructor(code, reason) {
    super(`${code}: ${reason}`)
    this.name = 'HbError'
    this.code = code
    this.reason = reason
  }
}

// normalize(input) returns the 9 uppercase symbols with no separators. It throws HB-KEY-INVALID
// with reason 'length' (not 9 symbols) or 'character' (a symbol outside the alphabet). Length is
// checked first, as in the Go and Dart suites, and counts code points. Only ASCII letters fold to
// upper case, so no other character can read as a symbol.
function normalize(input) {
  const symbols = input
    .replace(/[a-z]/g, (ch) => ch.toUpperCase())
    .replace(/[ -]/g, '')
    .replace(/[OIL]/g, (ch) => READ_AS[ch])
  if ([...symbols].length !== 9) throw new HbError('HB-KEY-INVALID', 'length')
  if (!SYMBOLS.test(symbols)) throw new HbError('HB-KEY-INVALID', 'character')
  return symbols
}

// format(normalized) returns the key in three groups of three, XXX-XXX-XXX.
function format(normalized) {
  return `${normalized.slice(0, 3)}-${normalized.slice(3, 6)}-${normalized.slice(6, 9)}`
}

// The Argon2id cost (docs/security.md, "Derivation"). Written as numbers, never as library constants,
// because a library default can change and the derivation must not. MEM is in bytes: 64 MiB.
const OPS = 3
const MEM = 67108864

// The labels of each family. Host keys and relay keys differ only in these (docs/security.md).
const HOST = { saltLabel: 'holebridge key v1 salt', roleLabel: 'holebridge key v1 ' }
const RELAY = { saltLabel: 'holebridge relay v1 salt', roleLabel: 'holebridge relay v1 ' }

// checkAppKey throws HB-APPKEY-INVALID unless appKey is 32 bytes. The error never holds the key.
function checkAppKey(appKey) {
  if (!ArrayBuffer.isView(appKey) || appKey.byteLength !== 32) throw new HbError('HB-APPKEY-INVALID', 'length')
}

// stretch(family, normalized, appKey) resolves to the 32-byte root: Argon2id13 of the normalized key,
// under a 16-byte salt that depends only on the application key. The async variant keeps the worklet
// responsive; the sync one is the fallback where sodium has no async pwhash.
async function stretch(family, normalized, appKey) {
  const salt = b4a.alloc(16)
  sodium.crypto_generichash(salt, b4a.concat([b4a.from(family.saltLabel), appKey]))
  const root = b4a.alloc(32)
  const alg = sodium.crypto_pwhash_ALG_ARGON2ID13
  if (typeof sodium.crypto_pwhash_async === 'function') {
    await sodium.crypto_pwhash_async(root, b4a.from(normalized), salt, OPS, MEM, alg)
  } else {
    sodium.crypto_pwhash(root, b4a.from(normalized), salt, OPS, MEM, alg)
  }
  return root
}

// seed(family, role, root, appKey) returns the 32-byte seed of one role: BLAKE2b-256 of the role label
// and the root, keyed with the application key.
function seed(family, role, root, appKey) {
  const out = b4a.alloc(32)
  sodium.crypto_generichash(out, b4a.concat([b4a.from(family.roleLabel + role), root]), appKey)
  return out
}

// keyPair(family, role, root, appKey) returns the Ed25519 key pair of one role, made from its seed.
function keyPair(family, role, root, appKey) {
  const publicKey = b4a.alloc(sodium.crypto_sign_PUBLICKEYBYTES)
  const secretKey = b4a.alloc(sodium.crypto_sign_SECRETKEYBYTES)
  sodium.crypto_sign_seed_keypair(publicKey, secretKey, seed(family, role, root, appKey))
  return { publicKey, secretKey }
}

// derive(key, appKey) resolves to the host and client key pairs and the LAN probe key. The key is
// normalized first, so a typed or formatted spelling derives the same keys, and an invalid key throws
// HB-KEY-INVALID before any Argon2 work. The LAN probe key is the 32-byte seed of the lan role, not a
// key pair: it keys the MAC of LAN probes and replies (docs/architecture.md, LAN probe). appKey must be
// 32 bytes. Derivation: docs/security.md.
async function derive(key, appKey) {
  const normalized = normalize(key)
  checkAppKey(appKey)
  const root = await stretch(HOST, normalized, appKey)
  return {
    host: keyPair(HOST, 'host', root, appKey),
    client: keyPair(HOST, 'client', root, appKey),
    lan: seed(HOST, 'lan', root, appKey)
  }
}

// deriveRelay(key, appKey) resolves to the server and member key pairs of a relay key. Like derive, it
// normalizes the key first and throws HB-KEY-INVALID for an invalid one. appKey must be 32 bytes.
// Derivation: docs/security.md, "Relay keys".
async function deriveRelay(key, appKey) {
  const normalized = normalize(key)
  checkAppKey(appKey)
  const root = await stretch(RELAY, normalized, appKey)
  return {
    server: keyPair(RELAY, 'server', root, appKey),
    member: keyPair(RELAY, 'member', root, appKey)
  }
}

module.exports = { normalize, format, derive, deriveRelay }
