const test = require('brittle')
const b4a = require('b4a')
const c = require('compact-encoding')
const { load, hex } = require('./helpers/vectors.js')
const { handshake, messages, unordered } = require('../lib/protocol.js')

// brittle ends the whole run when a test body throws. Until the IMPL task lands, the code throws
// 'not implemented', so each body runs under catchThrows, which fails only its own test.
function catchThrows(fn) {
  return (t) => {
    try {
      fn(t)
    } catch (err) {
      t.fail(err.message)
    }
  }
}

// A plain try/catch: brittle's t.exception rethrows TypeError and RangeError, so it is not used.
function throws(fn) {
  try {
    fn()
  } catch (err) {
    return true
  }
  return false
}

const frames = load('frames.json')
const valid = frames.filter((f) => !f.error)
const invalid = frames.filter((f) => f.error)

// Handshake frames and the unordered datagram have index null; they are told apart by name.
const BY_NAME = { handshake, 'handshake-lan': handshake, 'unordered-datagram': unordered }
const encodingOf = (f) => (f.index === null ? BY_NAME[f.name] : messages[f.index])

// The vectors write payloads and tokens as hex; the codec takes and returns bytes.
const BYTE_FIELDS = ['payload', 'token']

function toBytes(value) {
  const out = { ...value }
  for (const key of BYTE_FIELDS) if (out[key] !== undefined) out[key] = hex(out[key])
  return out
}

function toHex(value) {
  const out = { ...value }
  for (const key of BYTE_FIELDS) if (out[key] !== undefined) out[key] = b4a.toString(out[key], 'hex')
  return out
}

const encodeHex = (enc, value) => b4a.toString(c.encode(enc, value), 'hex')

test('c.encode gives the vector hex for every valid frame', catchThrows((t) => {
  for (const f of valid) t.is(encodeHex(encodingOf(f), toBytes(f.value)), f.hex, f.name)
}))

test('c.decode of every valid hex deep-equals the value', catchThrows((t) => {
  for (const f of valid) t.alike(toHex(c.decode(encodingOf(f), hex(f.hex))), f.value, f.name)
}))

test('every invalid frame throws', catchThrows((t) => {
  for (const f of invalid) {
    // A codec that throws for everything would pass the next line, so decode a valid frame first.
    c.decode(encodingOf(f), hex(valid.find((v) => v.name === f.name).hex))
    t.ok(throws(() => c.decode(encodingOf(f), hex(f.hex))), `${f.name}: ${f.error}`)
  }
}))

test('messages has 11 entries and messages[3] encodes data', catchThrows((t) => {
  t.is(messages.length, 11, 'one encoding per message index, 0 to 10')
  for (const f of valid.filter((v) => v.name === 'data')) {
    t.is(encodeHex(messages[3], toBytes(f.value)), f.hex, 'data frame')
  }
}))

test('a 65536-byte data payload round-trips; 65537 bytes throws', catchThrows((t) => {
  const { value } = valid.find((f) => f.name === 'data' && f.value.stream === 65536)
  const payload = hex(value.payload)
  t.is(payload.length, 65536, 'the vector payload is 65536 bytes')
  const back = c.decode(messages[3], c.encode(messages[3], { stream: 65536, payload }))
  t.is(back.stream, 65536, 'stream round-trips')
  t.is(b4a.toString(back.payload, 'hex'), value.payload, 'payload round-trips')
  const tooBig = { stream: 65536, payload: b4a.alloc(65537) }
  t.ok(throws(() => c.encode(messages[3], tooBig)), 'a 65537-byte payload throws')
}))

test('the checks hold where the vectors do not reach', catchThrows((t) => {
  // The oversize vector has no bytes after its header, so it throws for lack of bytes. Here the
  // 65537 bytes are present, so only the size check can reject them.
  const oversize = b4a.concat([b4a.from([1, 0xfe, 1, 0, 1, 0]), b4a.alloc(65537)])
  t.ok(throws(() => c.decode(messages[3], oversize)), 'a present 65537-byte payload throws')
  // Encoding checks the service kind too, not only decoding.
  const badKind = { version: 1, flags: 0, services: [{ name: 'x', kind: 5, port: 1, origins: [] }] }
  t.ok(throws(() => c.encode(handshake, badKind)), 'kind 5 throws on encode')
}))
