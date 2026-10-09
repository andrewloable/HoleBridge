// Writes spec/vectors/ipc.json: the frames between the Flutter app and the app engine, encoded as
// spec/ipc.md defines them (compact-encoding 3.5.2). Each vector is one whole frame: the type, the
// id and the body fields in order. hex is the frame's bytes. Buffers are lowercase hex in the JSON.
//
// Test values only: the key is a placeholder, the application key is bytes 0x00 to 0x1f, and the
// addresses come from the documentation ranges. None of them is a deployment's secret.
//
// Every frame is decoded again and must give back its value and use all its bytes. Every message
// needs at least one vector.
//
// Run: cd spec/gen && npm ci && node ipc.js

const fs = require('fs')
const path = require('path')
const assert = require('assert/strict')
const c = require('compact-encoding')
const { version } = require('compact-encoding/package.json')

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

// A 32-byte value, lowercase hex in JS and in the JSON.
const fixed32 = {
  preencode(state, hex) {
    c.fixed32.preencode(state, bytes32(hex))
  },
  encode(state, hex) {
    c.fixed32.encode(state, bytes32(hex))
  },
  decode(state) {
    return c.fixed32.decode(state).toString('hex')
  }
}

function bytes32(hex) {
  assert.match(hex, /^[0-9a-f]{64}$/, 'a fixed32 value is 64 lowercase hex digits')
  return Buffer.from(hex, 'hex')
}

const LAN = struct([
  ['addresses', c.array(c.string)],
  ['port', c.uint]
])
const PORT = struct([
  ['service', c.string],
  ['port', c.uint]
])
// A service the host shares: its name, then its kind in the handshake's numbering (0 unknown, 1 https,
// 2 http, 3 tcp, 4 udp; spec/ipc.md, Encoding).
const SERVICE = struct([
  ['name', c.string],
  ['kind', c.uint]
])
const NAT = struct([
  ['host', c.string],
  ['port', c.uint],
  ['firewalled', c.bool],
  ['randomized', c.bool]
])
const VPN_SERVICE = struct([
  ['host', c.string],
  ['service', c.string],
  ['address', c.string],
  ['port', c.uint]
])
const VPN_ADDRESS = struct([
  ['host', c.string],
  ['service', c.string],
  ['address', c.string],
  ['name', c.string]
])
const NONE = struct([])

// Test values. APP_KEY is not a deployment's application key.
const APP_KEY = '000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f'
const OTHER_APP_KEY = 'ff'.repeat(32)
const ZERO_APP_KEY = '00'.repeat(32)
const LINK = 'https://holebridge.app/h#1.' + '11'.repeat(32) + '.' + '22'.repeat(16) + '.9000.192.0.2.10,192.0.2.11'

// Requests: Dart to engine, each answered by a reply with the same id. Numbers 1 to 99.
// Events: engine to Dart, id 0. Numbers 100 and up. Numbers are append-only (spec/ipc.md).
const REQUESTS = [
  {
    type: 1,
    name: 'connect',
    enc: struct([
      ['host', c.string],
      ['key', c.string],
      ['appKey', fixed32],
      ['lan', LAN],
      ['ports', c.array(PORT)],
      ['bind', c.string]
    ]),
    examples: [
      {
        host: 'living-room',
        key: '7KQM4X9TR',
        appKey: APP_KEY,
        lan: { addresses: ['192.0.2.10'], port: 9000 },
        ports: [
          { service: 'web', port: 8080 },
          { service: 'ssh', port: 22 }
        ],
        bind: '127.0.0.1'
      },
      {
        host: 'office',
        key: 'HJKMNPQRS',
        appKey: OTHER_APP_KEY,
        lan: { addresses: [], port: 0 },
        ports: [],
        bind: '0.0.0.0'
      }
    ]
  },
  {
    type: 2,
    name: 'close',
    enc: struct([['host', c.string]]),
    examples: [{ host: 'living-room' }, { host: 'office' }]
  },
  {
    type: 3,
    name: 'status',
    enc: struct([['host', c.string]]),
    examples: [{ host: 'living-room' }, { host: 'office' }]
  },
  {
    type: 4,
    name: 'relay',
    enc: struct([
      ['key', c.string],
      ['appKey', fixed32]
    ]),
    examples: [
      { key: '', appKey: ZERO_APP_KEY },
      { key: 'R4NW8P2KD', appKey: APP_KEY }
    ]
  },
  { type: 5, name: 'handoff.listen', enc: NONE, examples: [{}] },
  { type: 6, name: 'handoff.cancel', enc: NONE, examples: [{}] },
  {
    type: 7,
    name: 'handoff.send',
    enc: struct([
      ['link', c.string],
      ['name', c.string],
      ['key', c.string],
      ['appKey', fixed32]
    ]),
    examples: [
      { link: LINK, name: 'living-room', key: '7KQM4X9TR', appKey: APP_KEY },
      { link: LINK, name: '', key: 'HJKMNPQRS', appKey: OTHER_APP_KEY }
    ]
  },
  {
    type: 8,
    name: 'vpn.start',
    enc: struct([
      ['services', c.array(VPN_SERVICE)],
      ['dnsUpstream', c.array(c.string)]
    ]),
    examples: [
      {
        services: [
          { host: 'living-room', service: 'web', address: '198.18.0.10', port: 5001 },
          { host: 'living-room', service: 'ssh', address: '198.18.0.11', port: 5002 }
        ],
        dnsUpstream: ['192.0.2.1']
      },
      { services: [], dnsUpstream: [] }
    ]
  },
  { type: 9, name: 'vpn.stop', enc: NONE, examples: [{}] }
]

const EVENTS = [
  {
    type: 100,
    name: 'route',
    enc: struct([
      ['host', c.string],
      ['route', c.string]
    ]),
    examples: [
      { host: 'living-room', route: 'lan' },
      { host: 'office', route: 'unreachable' }
    ]
  },
  {
    type: 101,
    name: 'session',
    enc: struct([
      ['host', c.string],
      ['up', c.bool]
    ]),
    examples: [
      { host: 'living-room', up: true },
      { host: 'office', up: false }
    ]
  },
  {
    type: 102,
    name: 'status',
    enc: struct([
      ['host', c.string],
      ['route', c.string],
      ['sessions', c.uint],
      ['streams', c.uint],
      ['flows', c.uint],
      ['bytesIn', c.uint],
      ['bytesOut', c.uint],
      ['nat', NAT]
    ]),
    examples: [
      {
        host: 'living-room',
        route: 'direct',
        sessions: 1,
        streams: 2,
        flows: 1,
        bytesIn: 123456,
        bytesOut: 789,
        nat: { host: '203.0.113.7', port: 49737, firewalled: false, randomized: false }
      },
      {
        host: 'office',
        route: 'looking',
        sessions: 0,
        streams: 0,
        flows: 0,
        bytesIn: 0,
        bytesOut: 0,
        nat: { host: '', port: 0, firewalled: true, randomized: true }
      }
    ]
  },
  {
    type: 103,
    name: 'vpn',
    enc: struct([
      ['port', c.uint],
      ['addresses', c.array(VPN_ADDRESS)]
    ]),
    examples: [
      {
        port: 5353,
        addresses: [{ host: 'living-room', service: 'web', address: '198.18.0.10', name: 'web.living-room.internal' }]
      },
      { port: 5353, addresses: [] }
    ]
  },
  {
    type: 104,
    name: 'services',
    enc: struct([
      ['host', c.string],
      ['list', c.array(SERVICE)],
      ['ports', c.array(PORT)]
    ]),
    examples: [
      {
        host: 'living-room',
        list: [
          { name: 'web', kind: 1 },
          { name: 'ssh', kind: 3 },
          { name: 'dns', kind: 4 }
        ],
        ports: [
          { service: 'web', port: 8080 },
          { service: 'ssh', port: 22 },
          { service: 'dns', port: 53 }
        ]
      },
      { host: 'office', list: [{ name: 'printer', kind: 0 }], ports: [] }
    ]
  },
  {
    type: 105,
    name: 'reject',
    enc: struct([
      ['host', c.string],
      ['service', c.string],
      ['code', c.string],
      ['reason', c.string]
    ]),
    examples: [
      { host: 'living-room', service: 'ssh', code: 'HB-TARGET-REFUSED', reason: 'target refused the connection' },
      { host: 'living-room', service: 'web', code: 'HB-TARGET-TIMEOUT', reason: 'target did not answer in time' }
    ]
  },
  {
    type: 106,
    name: 'error',
    enc: struct([
      ['code', c.string],
      ['detail', c.string]
    ]),
    examples: [
      { code: 'HB-LOOKUP-TIMEOUT', detail: 'no answer from the DHT within 60 s' },
      { code: 'HB-RELAY-REFUSED', detail: 'the relay did not admit this key' }
    ]
  },
  {
    type: 107,
    name: 'handoff.code',
    enc: struct([['link', c.string]]),
    examples: [{ link: LINK }]
  },
  {
    type: 108,
    name: 'handoff.received',
    enc: struct([
      ['name', c.string],
      ['key', c.string],
      ['appKey', fixed32]
    ]),
    examples: [
      { name: 'living-room', key: '7KQM4X9TR', appKey: APP_KEY },
      { name: 'office', key: 'HJKMNPQRS', appKey: OTHER_APP_KEY }
    ]
  },
  {
    type: 109,
    name: 'lan',
    enc: struct([
      ['host', c.string],
      ['addresses', c.array(c.string)],
      ['port', c.uint]
    ]),
    examples: [
      { host: 'living-room', addresses: ['192.0.2.10', '192.0.2.11'], port: 9000 },
      { host: 'office', addresses: [], port: 0 }
    ]
  }
]

// The reply to a request: type 0, the request's id (spec/ipc.md). Only the connect reply fills route,
// services and ports; the others send them empty.
const REPLY = {
  type: 0,
  name: 'reply',
  enc: struct([
    ['ok', c.bool],
    ['code', c.string],
    ['detail', c.string],
    ['route', c.string],
    ['services', c.array(SERVICE)],
    ['ports', c.array(PORT)]
  ]),
  examples: [
    { ok: true, code: '', detail: '', route: '', services: [], ports: [] },
    {
      ok: true,
      code: '',
      detail: '',
      route: 'direct',
      services: [
        { name: 'web', kind: 2 },
        { name: 'ssh', kind: 3 }
      ],
      ports: [
        { service: 'web', port: 8080 },
        { service: 'ssh', port: 22 }
      ]
    },
    { ok: false, code: 'HB-LOOKUP-TIMEOUT', detail: 'no answer from the DHT within 60 s', route: '', services: [], ports: [] },
    { ok: false, code: 'HB-KEY-INVALID', detail: 'the key has 8 symbols', route: '', services: [], ports: [] }
  ]
}

// The numbering rule from spec/ipc.md, checked here so a new message cannot break it silently.
const ALL = [REPLY, ...REQUESTS, ...EVENTS]
const types = new Set()
for (const m of ALL) {
  assert.ok(!types.has(m.type), `type ${m.type} is used twice`)
  types.add(m.type)
}
for (const m of REQUESTS) assert.ok(m.type >= 1 && m.type <= 99, `request ${m.name} has type ${m.type}`)
for (const m of EVENTS) assert.ok(m.type >= 100, `event ${m.name} has type ${m.type}`)

const ENCODINGS = new Map(ALL.map((m) => [m.type, m.enc]))

// One frame: type, id, then the body fields of that type, all to the end of the frame.
function frameBytes(type, id, enc, value) {
  return Buffer.concat([c.encode(c.uint, type), c.encode(c.uint, id), c.encode(enc, value)])
}

function readFrame(buf) {
  const state = c.state(0, buf.length, buf)
  const type = c.uint.decode(state)
  const id = c.uint.decode(state)
  const enc = ENCODINGS.get(type)
  if (!enc) throw new Error(`no message has type ${type}`)
  const value = enc.decode(state)
  if (state.start !== buf.length) throw new Error(`type ${type}: ${buf.length - state.start} bytes unread`)
  return { type, id, value }
}

const vectors = []
for (const m of ALL) {
  m.examples.forEach((value, i) => {
    // Requests and replies use ids 1 and 2; events use id 0.
    const isEvent = m.type >= 100
    const id = isEvent ? 0 : i + 1
    const bytes = frameBytes(m.type, id, m.enc, value)
    const back = readFrame(bytes)
    assert.deepEqual(back, { type: m.type, id, value }, `${m.name} ${i + 1} decodes to its value`)
    vectors.push({ name: `${m.name} ${i + 1}`, type: m.type, id, value, hex: bytes.toString('hex') })
  })
}

// Every message has at least one vector.
for (const m of ALL) {
  assert.ok(vectors.some((v) => v.type === m.type), `no vector for type ${m.type} (${m.name})`)
}

// Every error code in a vector is an entry of the catalog (spec/errors.json).
const catalog = new Set(JSON.parse(fs.readFileSync(path.join(__dirname, '..', 'errors.json'), 'utf8')).codes.map((e) => e.code))
for (const v of vectors) {
  for (const [k, x] of Object.entries(v.value)) {
    if (k === 'code' && x !== '') assert.ok(catalog.has(x), `${v.name}: ${x} is not in spec/errors.json`)
  }
}

const out = {
  description:
    'IPC frames between the Flutter app and the app engine, as spec/ipc.md defines them: one whole frame per vector, with its type, id and body fields',
  reference: `compact-encoding ${version}`,
  vectors
}

const file = path.join(__dirname, '..', 'vectors', 'ipc.json')
fs.writeFileSync(file, JSON.stringify(out, null, 2) + '\n')
console.log(`wrote ipc.json: ${vectors.length} vectors for ${ALL.length} messages`)
