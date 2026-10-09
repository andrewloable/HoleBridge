// Writes spec/vectors/handoff.json: the plaintext of the handoff box and one sealed box, as
// docs/architecture.md (Adding a host to a TV, Formats) defines them. The plaintext is
// compact-encoded (compact-encoding 3.5.2) in the order version, secret, name, key, appKey. The box
// is crypto_box_seal (sodium-universal 5.0.1) to a key pair seeded from a fixed seed.
//
// Test values only. The key is the test value 7KQM4X9TR, the application key is bytes 0x00 to 0x1f,
// the secrets are fixed patterns, and the key pair comes from a fixed test seed. None of them is a
// deployment's secret.
//
// A sealed box is random: sealing uses a fresh ephemeral key. So the generator keeps the box already
// in the file and seals a new one only when --force is passed. A kept box that no longer opens to the
// first plaintext (the plaintext changed) is an error: run again with --force.
//
// Every plaintext is decoded again and must give back its value and use all its bytes. The box must
// open with the recorded key pair to the first plaintext, and be at most 1 KiB.
//
// Run: cd spec/gen && npm ci && node handoff.js [--force]

const fs = require('fs')
const path = require('path')
const assert = require('assert/strict')
const c = require('compact-encoding')
const sodium = require('sodium-universal')

const FORCE = process.argv.slice(2).includes('--force')
const file = path.join(__dirname, '..', 'vectors', 'handoff.json')

// Test values only.
const KEY = '7KQM4X9TR'
const APP_KEY = '000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f'
const SEED = '404142434445464748494a4b4c4d4e4f505152535455565758595a5b5c5d5e5f'

const PLAINTEXTS = [
  { version: 1, secret: '00112233445566778899aabbccddeeff', name: 'Living room', key: KEY, appKey: APP_KEY },
  { version: 1, secret: 'ffeeddccbbaa99887766554433221100', name: 'Sala café 客厅', key: KEY, appKey: APP_KEY },
  { version: 1, secret: '0f1e2d3c4b5a69788796a5b4c3d2e1f0', name: 'Office', key: KEY, appKey: APP_KEY }
]

// A fixed-size value: lowercase hex in JS and in the JSON, raw bytes on the wire.
function bytesOf(hex, n) {
  assert.match(hex, new RegExp(`^[0-9a-f]{${2 * n}}$`), `a value is ${n} bytes: ${2 * n} lowercase hex digits`)
  return Buffer.from(hex, 'hex')
}
const fixedHex = (n) => {
  const enc = c.fixed(n)
  return {
    preencode(state, hex) {
      enc.preencode(state, bytesOf(hex, n))
    },
    encode(state, hex) {
      enc.encode(state, bytesOf(hex, n))
    },
    decode(state) {
      return enc.decode(state).toString('hex')
    }
  }
}

// A struct encodes its fields in order. Its value is a plain object.
const struct = (fields) => ({
  preencode(state, o) {
    for (const [k, enc] of fields) enc.preencode(state, o[k])
  },
  encode(state, o) {
    for (const [k, enc] of fields) enc.encode(state, o[k])
  },
  decode(state) {
    const o = {}
    for (const [k, enc] of fields) o[k] = enc.decode(state)
    return o
  }
})

const PLAINTEXT = struct([
  ['version', c.uint],
  ['secret', fixedHex(16)],
  ['name', c.string],
  ['key', c.string],
  ['appKey', fixedHex(32)]
])

function decodePlaintext(buf) {
  const state = c.state(0, buf.length, buf)
  const value = PLAINTEXT.decode(state)
  if (state.start !== buf.length) throw new Error(`${buf.length - state.start} bytes unread`)
  return value
}

function keyPairFrom(seedHex) {
  const seed = bytesOf(seedHex, 32)
  const publicKey = Buffer.alloc(sodium.crypto_box_PUBLICKEYBYTES)
  const secretKey = Buffer.alloc(sodium.crypto_box_SECRETKEYBYTES)
  sodium.crypto_box_seed_keypair(publicKey, secretKey, seed)
  return { seed: seedHex, publicKey: publicKey.toString('hex'), secretKey: secretKey.toString('hex') }
}

function seal(plain, publicKeyHex) {
  const box = Buffer.alloc(plain.length + sodium.crypto_box_SEALBYTES)
  sodium.crypto_box_seal(box, plain, Buffer.from(publicKeyHex, 'hex'))
  return box
}

// The opened plaintext, or null when the box does not open with this key pair.
function open(box, keyPair) {
  const plain = Buffer.alloc(box.length - sodium.crypto_box_SEALBYTES)
  const ok = sodium.crypto_box_seal_open(
    plain,
    box,
    Buffer.from(keyPair.publicKey, 'hex'),
    Buffer.from(keyPair.secretKey, 'hex')
  )
  return ok ? plain : null
}

// The box recorded in the file, kept on a plain re-run. Null when there is none or --force is passed.
function recordedBox() {
  if (FORCE || !fs.existsSync(file)) return null
  const previous = JSON.parse(fs.readFileSync(file, 'utf8'))
  return previous.boxes && previous.boxes.length ? previous.boxes[0].hex : null
}

function main() {
  assert.match(KEY, /^[0-9A-HJKMNPQRSTVWXYZ]{9}$/, 'the key is 9 canonical symbols')

  const keyPair = keyPairFrom(SEED)

  const plaintexts = PLAINTEXTS.map((value) => {
    const bytes = c.encode(PLAINTEXT, value)
    assert.deepEqual(decodePlaintext(bytes), value, 'a plaintext decodes to its value')
    return { value, hex: bytes.toString('hex') }
  })

  const recorded = recordedBox()
  const boxHex = recorded !== null ? recorded : seal(Buffer.from(plaintexts[0].hex, 'hex'), keyPair.publicKey).toString('hex')

  const box = Buffer.from(boxHex, 'hex')
  assert.ok(box.length <= 1024, 'a box is at most 1 KiB')
  const opened = open(box, keyPair)
  if (opened === null) {
    throw new Error('the box does not open with the key pair; if the plaintext changed, run again with --force')
  }
  assert.ok(opened.equals(Buffer.from(plaintexts[0].hex, 'hex')), 'the box opens to the first plaintext')

  const out = {
    keyPair,
    plaintexts,
    boxes: [{ plaintextIndex: 0, hex: boxHex }]
  }
  const text = JSON.stringify(out, null, 2) + '\n'
  const existing = fs.existsSync(file) ? fs.readFileSync(file, 'utf8') : null
  if (text !== existing) fs.writeFileSync(file, text)
  console.log(`handoff.json: ${plaintexts.length} plaintexts, 1 box${recorded !== null ? ' (kept from the file)' : ' (sealed now)'}`)
}

main()
