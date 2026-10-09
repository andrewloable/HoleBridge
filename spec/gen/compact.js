// Writes spec/vectors/compact.json: compact-encoding primitives as the upstream reference encodes and
// decodes them (index.js, 3.5.2), for the Go codec in pears/compact.
//
// cases: each value is encoded by the reference, and its bytes decode back to the value.
// decode_errors: truncated inputs that the reference rejects with 'Out of bounds'.
// decode_only: non-minimal encodings that the reference accepts; the value is what it decodes.
//
// Run: cd spec/gen && npm ci && node compact.js

const fs = require('fs')
const path = require('path')
const c = require('compact-encoding')
const { version } = require('compact-encoding/package.json')

// Bytes 0, 1, 2, ... wrapping at 251, so no run repeats within 300 bytes.
const patternBytes = (n) => Buffer.from(Array.from({ length: n }, (_, i) => i % 251))

const TYPES = {
  uint: c.uint,
  uint8: c.uint8,
  uint16: c.uint16,
  uint24: c.uint24,
  uint32: c.uint32,
  uint64: c.uint64,
  int: c.int,
  bool: c.bool,
  float64: c.float64,
  buffer: c.buffer,
  string: c.string,
  fixed: c.fixed(32)
}

// Buffer and fixed values are hex in the JSON; every other value is the JSON value itself.
const toJS = (type, value) =>
  type === 'buffer' || type === 'fixed' ? Buffer.from(value, 'hex') : value

// Buffers compare by content; other values with ===.
const same = (a, b) => (a instanceof Uint8Array ? Buffer.from(a).equals(Buffer.from(b)) : a === b)

const CASES = [
  ['uint 0', 'uint', 0],
  ['uint 1', 'uint', 1],
  ['uint 252', 'uint', 252],
  ['uint 253', 'uint', 253],
  ['uint 65535', 'uint', 65535],
  ['uint 65536', 'uint', 65536],
  ['uint 4294967295', 'uint', 4294967295],
  ['uint 4294967296', 'uint', 4294967296],
  ['uint 9007199254740991', 'uint', 2 ** 53 - 1],
  ['int -1', 'int', -1],
  ['int 0', 'int', 0],
  ['int 1', 'int', 1],
  ['int -2147483648', 'int', -(2 ** 31)],
  ['uint8 0', 'uint8', 0],
  ['uint8 255', 'uint8', 255],
  ['uint16 0', 'uint16', 0],
  ['uint16 65535', 'uint16', 65535],
  ['uint24 0', 'uint24', 0],
  ['uint24 16777215', 'uint24', 16777215],
  ['uint32 0', 'uint32', 0],
  ['uint32 4294967295', 'uint32', 4294967295],
  ['uint64 0', 'uint64', 0],
  ['uint64 9007199254740991', 'uint64', 2 ** 53 - 1],
  ['bool true', 'bool', true],
  ['bool false', 'bool', false],
  ['float64 0.5', 'float64', 0.5],
  ['float64 -1e300', 'float64', -1e300],
  ['buffer empty', 'buffer', ''],
  ['buffer 300 bytes', 'buffer', patternBytes(300).toString('hex')],
  ['string ascii', 'string', 'hello'],
  ['string unicode', 'string', 'héllo wörld ✓ \u{1d11e}'],
  ['fixed 32', 'fixed', patternBytes(32).toString('hex')]
]

const DECODE_ERRORS = [
  ['uint empty', 'uint', ''],
  ['uint truncated 16-bit', 'uint', 'fd01'],
  ['uint truncated 32-bit', 'uint', 'fe0000'],
  ['buffer truncated', 'buffer', '0501'],
  ['string truncated', 'string', '0568656c']
]

// Non-minimal uints: the reference does not reject them, so its decoded value is recorded.
const DECODE_ONLY = [
  ['uint non-minimal 16-bit', 'uint', 'fd0100'],
  ['uint non-minimal 32-bit', 'uint', 'fe05000000']
]

const out = {
  description:
    'compact-encoding primitives: uint and its fixed-width forms, int, bool, float64, buffer, string and fixed, with boundary values, truncated inputs and non-minimal uints',
  reference: `compact-encoding ${version}`,
  cases: [],
  decode_errors: [],
  decode_only: []
}

for (const [name, type, value] of CASES) {
  const enc = TYPES[type]
  const input = toJS(type, value)
  const bytes = c.encode(enc, input)
  const state = c.state(0, bytes.length, bytes)
  if (!same(enc.decode(state), input) || state.start !== bytes.length) {
    throw new Error(`${name}: the reference does not decode its own encoding back to the value`)
  }
  out.cases.push({ name, type, value, hex: bytes.toString('hex') })
}

for (const [name, type, hex] of DECODE_ERRORS) {
  const bytes = Buffer.from(hex, 'hex')
  let message
  try {
    TYPES[type].decode(c.state(0, bytes.length, bytes))
  } catch (err) {
    message = err.message
  }
  if (message !== 'Out of bounds') {
    throw new Error(`${name}: expected the reference to throw 'Out of bounds', got ${message}`)
  }
  out.decode_errors.push({ name, type, hex, error: message })
}

for (const [name, type, hex] of DECODE_ONLY) {
  const bytes = Buffer.from(hex, 'hex')
  const state = c.state(0, bytes.length, bytes)
  const value = TYPES[type].decode(state)
  if (state.start !== bytes.length) {
    throw new Error(`${name}: the reference leaves bytes unread`)
  }
  out.decode_only.push({ name, type, value, hex })
}

const file = path.join(__dirname, '..', 'vectors', 'compact.json')
fs.writeFileSync(file, JSON.stringify(out, null, 2) + '\n')
console.log(
  `wrote compact.json: ${out.cases.length} cases, ${out.decode_errors.length} decode errors, ${out.decode_only.length} decode-only`
)
