// Writes spec/vectors/frames.json: golden encodings of the handshake, of every protocol v1 message
// and of the unordered datagram, built only from compact-encoding primitives (docs/architecture.md,
// "Encoding"). The Go codec and the JS codec must decode each valid frame to its value and reject
// each invalid one.
//
// Valid entries: {name, index, value, hex}. index is null for the handshake and the unordered
// datagram. Buffers and tokens are hex strings in value.
// Invalid entries: {name, index, hex, error}. A data entry whose payload is left out carries
// payloadLength instead of the payload.
//
// Run: cd spec/gen && npm ci && node frames.js

const assert = require('assert')
const fs = require('fs')
const path = require('path')
const c = require('compact-encoding')
const { version } = require('compact-encoding/package.json')

// Buffers and fixed-size tokens: hex in the vector file, bytes on the wire.
const hexed = (enc) => ({
  preencode(state, hex) {
    enc.preencode(state, Buffer.from(hex, 'hex'))
  },
  encode(state, hex) {
    enc.encode(state, Buffer.from(hex, 'hex'))
  },
  decode(state) {
    return Buffer.from(enc.decode(state)).toString('hex')
  }
})

// A struct encodes its fields in order. A field with a guard is present only when the guard holds
// for the fields before it: the handshake's lan, behind flag bit 1.
const field = (name, enc, when) => ({ name, enc, when })
function struct(...fields) {
  const present = (f, m) => !f.when || f.when(m)
  return {
    preencode(state, m) {
      for (const f of fields) if (present(f, m)) f.enc.preencode(state, m[f.name])
    },
    encode(state, m) {
      for (const f of fields) if (present(f, m)) f.enc.encode(state, m[f.name])
    },
    decode(state) {
      const m = {}
      for (const f of fields) if (present(f, m)) m[f.name] = f.enc.decode(state)
      return m
    }
  }
}

const uint = c.uint
const str = c.string
const hexBuffer = hexed(c.buffer)
const token = hexed(c.fixed(16))

const FLAG_LAN = 2
const service = struct(
  field('name', str),
  field('kind', uint),
  field('port', uint),
  field('origins', c.array(str))
)
const lan = struct(field('addresses', c.array(str)), field('port', uint))
const handshake = struct(
  field('version', uint),
  field('flags', uint),
  field('services', c.array(service)),
  field('lan', lan, (m) => (m.flags & FLAG_LAN) !== 0)
)

const openMsg = struct(field('stream', uint), field('service', str), field('window', uint))
const openedMsg = struct(field('stream', uint), field('window', uint), field('token', token))
const rejectMsg = struct(field('stream', uint), field('code', uint), field('reason', str))
const dataMsg = struct(field('stream', uint), field('payload', hexBuffer))
const windowMsg = struct(field('stream', uint), field('credit', uint), field('received', uint))
const closeMsg = struct(field('stream', uint))
const reattachMsg = struct(
  field('stream', uint),
  field('token', token),
  field('received', uint),
  field('limit', uint)
)
const reattachedMsg = struct(field('stream', uint), field('received', uint), field('limit', uint))
const servicesMsg = struct(field('services', c.array(service)))
const flowMsg = struct(field('flow', uint), field('service', str), field('payload', hexBuffer))
const datagramMsg = struct(field('flow', uint), field('payload', hexBuffer))

const ZERO_TOKEN = '00'.repeat(16)
const TOKEN = '0102030405060708090a0b0c0d0e0f10'
const UNICODE = 'naïve ✓ 日本 🚀'
const WINDOW_2MIB = 2097152
const FOUR_GIB = 4294967296
const DNS_QUERY = 'abcd0100000100000000000003777777076578616d706c6503636f6d0000010001'

const utf8Hex = (s) => Buffer.from(s, 'utf8').toString('hex')
// Bytes 0, 1, ... wrapping at 251, as in compact.js, so no run repeats within 300 bytes.
const patternHex = (n) => Buffer.from(Array.from({ length: n }, (_, i) => i % 251)).toString('hex')

// Two values per frame; together they cover the uint boundaries 252, 253, 65535, 65536 and
// 4294967296, an empty array, a unicode string and the zero token.
const VALID = [
  {
    name: 'handshake',
    index: null,
    enc: handshake,
    values: [
      { version: 1, flags: 4, services: [] },
      {
        version: 1,
        flags: 5,
        services: [
          { name: 'wiki', kind: 1, port: 443, origins: ['https://wiki.example.com', 'https://wiki.example.net'] },
          { name: 'ssh', kind: 3, port: 22, origins: [] }
        ]
      }
    ]
  },
  {
    name: 'handshake-lan',
    index: null,
    enc: handshake,
    values: [
      {
        version: 1,
        flags: 2,
        services: [{ name: 'ssh', kind: 3, port: 22, origins: [] }],
        lan: { addresses: ['192.0.2.10', '198.51.100.7'], port: 65535 }
      },
      { version: 1, flags: 3, services: [], lan: { addresses: ['203.0.113.5'], port: 253 } }
    ]
  },
  {
    name: 'open',
    index: 0,
    enc: openMsg,
    values: [
      { stream: 252, service: 'ssh', window: WINDOW_2MIB },
      { stream: 65535, service: 'wiki', window: 65536 }
    ]
  },
  {
    name: 'opened',
    index: 1,
    enc: openedMsg,
    values: [
      { stream: 253, window: WINDOW_2MIB, token: ZERO_TOKEN },
      { stream: 65536, window: 65536, token: TOKEN }
    ]
  },
  {
    name: 'reject',
    index: 2,
    enc: rejectMsg,
    values: [
      { stream: 252, code: 3, reason: 'ssh refused: connection refused at 127.0.0.1:22' },
      { stream: 253, code: 4, reason: UNICODE }
    ]
  },
  {
    name: 'data',
    index: 3,
    enc: dataMsg,
    values: [
      { stream: 252, payload: 'ff' },
      { stream: 65536, payload: patternHex(65536) }
    ]
  },
  {
    name: 'window',
    index: 4,
    enc: windowMsg,
    values: [
      { stream: 253, credit: 65536, received: 0 },
      { stream: 65535, credit: 1, received: FOUR_GIB }
    ]
  },
  {
    name: 'close',
    index: 5,
    enc: closeMsg,
    values: [{ stream: 0 }, { stream: 253 }]
  },
  {
    name: 'reattach',
    index: 6,
    enc: reattachMsg,
    values: [
      { stream: 252, token: ZERO_TOKEN, received: 0, limit: WINDOW_2MIB },
      { stream: 65536, token: TOKEN, received: FOUR_GIB, limit: FOUR_GIB + WINDOW_2MIB }
    ]
  },
  {
    name: 'reattached',
    index: 7,
    enc: reattachedMsg,
    values: [
      { stream: 252, received: 0, limit: WINDOW_2MIB },
      { stream: 65535, received: FOUR_GIB, limit: FOUR_GIB + WINDOW_2MIB }
    ]
  },
  {
    name: 'services',
    index: 8,
    enc: servicesMsg,
    values: [
      { services: [] },
      {
        services: [
          { name: 'wiki', kind: 1, port: 443, origins: ['https://wiki.example.com', 'https://wiki.example.net'] },
          { name: 'web', kind: 2, port: 80, origins: [] },
          { name: 'ssh', kind: 3, port: 22, origins: [] },
          { name: 'dns', kind: 4, port: 53, origins: [] },
          { name: 'misc', kind: 0, port: 65535, origins: [] }
        ]
      }
    ]
  },
  {
    name: 'flow',
    index: 9,
    enc: flowMsg,
    values: [
      { flow: 252, service: 'dns', payload: DNS_QUERY },
      { flow: 65536, service: 'wiki', payload: patternHex(253) }
    ]
  },
  {
    name: 'datagram',
    index: 10,
    enc: datagramMsg,
    values: [
      { flow: 252, payload: '00' },
      { flow: 65535, payload: utf8Hex('hello') }
    ]
  },
  {
    name: 'unordered-datagram',
    index: null,
    enc: datagramMsg,
    values: [
      { flow: 252, payload: 'c0ffee' },
      { flow: FOUR_GIB, payload: utf8Hex(UNICODE) }
    ]
  }
]

const encodeHex = (enc, value) => c.encode(enc, value).toString('hex')

// Decodes one frame and reports what the reference left: an error message, or the bytes unread.
function inspect(enc, hex) {
  const bytes = Buffer.from(hex, 'hex')
  const state = c.state(0, bytes.length, bytes)
  try {
    const value = enc.decode(state)
    return { value, unread: bytes.length - state.start }
  } catch (err) {
    return { error: err.message }
  }
}

const entries = []
for (const { name, index, enc, values } of VALID) {
  for (const value of values) {
    const hex = encodeHex(enc, value)
    const decoded = inspect(enc, hex)
    assert.ok(!decoded.error && decoded.unread === 0, `${name}: the frame does not decode exactly`)
    assert.deepStrictEqual(decoded.value, value, `${name}: does not decode back to its value`)
    entries.push({ name, index, value, hex })
  }
}

const openV1 = encodeHex(openMsg, { stream: 252, service: 'ssh', window: WINDOW_2MIB })

// Each invalid frame must be malformed for the reason its error names. The reference shows the
// defect: it throws, leaves bytes unread, or decodes a value the protocol does not allow.
const invalid = [
  {
    name: 'open',
    index: 0,
    hex: openV1.slice(0, -2),
    error: 'truncated',
    defect: (hex) => inspect(openMsg, hex).error === 'Out of bounds'
  },
  {
    name: 'open',
    index: 0,
    hex: openV1 + '00',
    error: 'trailing bytes',
    defect: (hex) => inspect(openMsg, hex).unread === 1
  },
  {
    name: 'handshake',
    index: null,
    hex: encodeHex(handshake, {
      version: 1,
      flags: 0,
      services: [{ name: 'ssh', kind: 5, port: 22, origins: [] }]
    }),
    error: 'unknown kind',
    defect: (hex) => {
      const r = inspect(handshake, hex)
      return r.unread === 0 && r.value.services.some((s) => s.kind > 4)
    }
  },
  {
    name: 'data',
    index: 3,
    hex: encodeHex(dataMsg, { stream: 252, payload: '' }),
    error: 'empty payload',
    defect: (hex) => {
      const r = inspect(dataMsg, hex)
      return r.unread === 0 && r.value.payload === ''
    }
  },
  {
    name: 'data',
    index: 3,
    hex: Buffer.concat([c.encode(uint, 252), c.encode(uint, 65537)]).toString('hex'),
    error: 'payload too large',
    payloadLength: 65537,
    defect: (hex) => {
      const bytes = Buffer.from(hex, 'hex')
      const state = c.state(0, bytes.length, bytes)
      const stream = uint.decode(state)
      const length = uint.decode(state)
      return stream === 252 && length === 65537 && state.start === bytes.length
    }
  }
]

for (const entry of invalid) {
  assert.ok(entry.defect(entry.hex), `${entry.name} ${entry.error}: not malformed for that reason`)
}

const out = [...entries, ...invalid.map(({ defect, ...entry }) => entry)]

const file = path.join(__dirname, '..', 'vectors', 'frames.json')
fs.writeFileSync(file, JSON.stringify(out, null, 2) + '\n')
console.log(
  `wrote frames.json: ${entries.length} valid frames, ${invalid.length} invalid, reference compact-encoding ${version}`
)
