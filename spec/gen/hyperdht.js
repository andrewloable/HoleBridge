// Writes spec/vectors/hyperdht.json: the message encodings of hyperdht 6.34.1 (lib/messages.js) as
// the upstream reference encodes and decodes them, for the Go codec in pears/hyperdht.
//
// Each case is one value of one encoding. value is the JS object the reference encodes, with
// binary fields as base64 (the form Go's encoding/json reads into []byte) and absent fields left
// out. hex is the reference's encoding of value. The "address" type is the IPv4 peer address that
// messages.js builds from compact-encoding's ipv4Address (not exported by messages.js).
//
// Before a case is written, the generator checks it against the reference: value encodes to hex,
// hex decodes back to value, and every strict prefix of hex throws 'Out of bounds'. The one
// exception is a lookupRawReply prefix without its bump: the reference reads a missing bump as 0.
//
// Run: cd spec/gen && npm ci && node hyperdht.js

const fs = require('fs')
const path = require('path')
const c = require('compact-encoding')
const m = require('hyperdht/lib/messages.js')
const { version } = require('hyperdht/package.json')

// As messages.js does: ipv4Address.decode also returns the address family, which is not kept.
const address = {
  ...c.ipv4Address,
  decode(state) {
    const ip = c.ipv4Address.decode(state)
    return { host: ip.host, port: ip.port }
  }
}

const ENCODINGS = {
  address,
  handshake: m.handshake,
  holepunch: m.holepunch,
  holepunchPayload: m.holepunchPayload,
  noisePayload: m.noisePayload,
  peer: m.peer,
  peers: m.peers,
  lookupRawReply: m.lookupRawReply,
  announce: m.announce
}

// Bytes that differ by position and by seed, so no two fields of a case match.
const bytes = (n, seed) =>
  Buffer.from(Array.from({ length: n }, (_, i) => (seed * 31 + i * 7) % 256))

// JSON form of a value: Buffers as base64, undefined fields left out.
const wire = (v) => {
  if (Buffer.isBuffer(v)) return v.toString('base64')
  if (Array.isArray(v)) return v.map(wire)
  if (v && typeof v === 'object') {
    const out = {}
    for (const k of Object.keys(v)) if (v[k] !== undefined) out[k] = wire(v[k])
    return out
  }
  return v
}

// Comparable form of a value: Buffers as hex, keys sorted, and null, undefined and empty-array
// fields dropped, since the reference fills in an absent list as []. Only the decoded side needs it.
const canon = (v) => {
  if (Buffer.isBuffer(v)) return v.toString('hex')
  if (Array.isArray(v)) return v.map(canon)
  if (v && typeof v === 'object') {
    const out = {}
    for (const k of Object.keys(v).sort()) {
      const x = v[k]
      if (x === null || x === undefined) continue
      if (Array.isArray(x) && x.length === 0) continue
      out[k] = canon(x)
    }
    return out
  }
  return v
}

const same = (a, b) => JSON.stringify(canon(a)) === JSON.stringify(canon(b))

// lookupRawReply takes its peers already encoded (c.raw), as the server holds them; decode gives
// them back as objects. Pre-encoding them here gives the same bytes as encoding the objects.
const toEncodeInput = (type, v) =>
  type === 'lookupRawReply' ? { ...v, peers: v.peers.map((p) => c.encode(m.peer, p)) } : v

const A = { host: '203.0.113.7', port: 49737 }
const B = { host: '198.51.100.2', port: 4000 }
const C = { host: '192.0.2.9', port: 33445 }
const D = { host: '255.255.255.255', port: 65535 }
// IPv6 hosts are written in full, as the reference decodes them.
const V6 = { host: '2001:db8:0:0:0:0:0:1', port: 4000 }

const PEER_NO_RELAY = { publicKey: bytes(32, 1), relayAddresses: [] }
const PEER_RELAYS = { publicKey: bytes(32, 2), relayAddresses: [A, B] }

const CASES = [
  ['address 203.0.113.7:49737', 'address', A],
  ['address 255.255.255.255:65535', 'address', D],

  ['handshake with noise only', 'handshake', { mode: 0, noise: bytes(32, 3) }],
  ['handshake with peer address', 'handshake', { mode: 1, noise: bytes(48, 4), peerAddress: B }],
  [
    'handshake with empty noise, peer and relay addresses',
    'handshake',
    { mode: 2, noise: Buffer.alloc(0), peerAddress: A, relayAddress: C }
  ],

  ['holepunch with payload', 'holepunch', { mode: 0, id: 7, payload: bytes(10, 5) }],
  ['holepunch with peer address and id 300', 'holepunch', { mode: 1, id: 300, payload: bytes(40, 6), peerAddress: C }],

  [
    'holepunch payload with no optional fields',
    'holepunchPayload',
    { error: 0, firewall: 1, round: 0, connected: false, punching: false }
  ],
  [
    'holepunch payload with addresses and tokens',
    'holepunchPayload',
    {
      error: 0,
      firewall: 2,
      round: 3,
      connected: true,
      punching: true,
      addresses: [A, B],
      remoteAddress: C,
      token: bytes(32, 7),
      remoteToken: bytes(32, 8)
    }
  ],
  [
    'holepunch payload with empty addresses',
    'holepunchPayload',
    { error: 5, firewall: 0, round: 1, connected: false, punching: true, addresses: [], token: bytes(32, 9) }
  ],

  ['peer with no relay addresses', 'peer', PEER_NO_RELAY],
  ['peer with two relay addresses', 'peer', PEER_RELAYS],
  ['peers empty', 'peers', []],
  ['peers with two entries', 'peers', [PEER_NO_RELAY, PEER_RELAYS]],

  ['lookup reply with no peers and bump 0', 'lookupRawReply', { peers: [], bump: 0 }],
  ['lookup reply with one peer and bump 0', 'lookupRawReply', { peers: [PEER_RELAYS], bump: 0 }],
  ['lookup reply with two peers and bump 1000', 'lookupRawReply', { peers: [PEER_NO_RELAY, PEER_RELAYS], bump: 1000 }],

  [
    'announce with peer, refresh and signature',
    'announce',
    { peer: PEER_RELAYS, refresh: bytes(32, 10), signature: bytes(64, 11), bump: 2 }
  ],
  ['announce with signature only', 'announce', { signature: bytes(64, 12), bump: 0 }],

  ['noise payload with version, error and firewall only', 'noisePayload', { version: 1, error: 0, firewall: 2 }],
  [
    'noise payload with holepunch info',
    'noisePayload',
    {
      version: 1,
      error: 0,
      firewall: 1,
      holepunch: { id: 300, relays: [{ relayAddress: A, peerAddress: B }] }
    }
  ],
  [
    'noise payload with IPv4 and IPv6 addresses and udx info',
    'noisePayload',
    {
      version: 1,
      error: 3,
      firewall: 2,
      addresses4: [A, C],
      addresses6: [V6],
      udx: { version: 1, reusableSocket: true, id: 77, seq: 3 }
    }
  ],
  [
    'noise payload with secret stream, relay through and relay addresses',
    'noisePayload',
    {
      version: 1,
      error: 0,
      firewall: 0,
      secretStream: { version: 1 },
      relayThrough: { version: 1, publicKey: bytes(32, 13), token: bytes(32, 14) },
      relayAddresses: [D]
    }
  ]
]

const out = {
  description:
    'hyperdht message encodings from messages.js: the ipv4 peer address, handshake, holepunch, holepunchPayload, noisePayload, peer, peers, lookupRawReply and announce, with values, hex encodings and truncated inputs',
  reference: `hyperdht ${version}`,
  cases: []
}

for (const [name, type, value] of CASES) {
  const enc = ENCODINGS[type]
  const bytesOut = c.encode(enc, toEncodeInput(type, value))
  const hex = bytesOut.toString('hex')

  const state = c.state(0, bytesOut.length, bytesOut)
  const decoded = enc.decode(state)
  if (state.start !== bytesOut.length) {
    throw new Error(`${name}: the reference leaves bytes unread`)
  }
  if (!same(decoded, value)) {
    throw new Error(`${name}: the reference decodes its encoding to a different value`)
  }
  if (c.encode(enc, toEncodeInput(type, decoded)).toString('hex') !== hex) {
    throw new Error(`${name}: the reference does not re-encode its decoded value to the same bytes`)
  }

  // The one prefix the reference accepts: a lookupRawReply without its bump, which reads as bump 0.
  const skip = type === 'lookupRawReply' ? bytesOut.length - c.encode(c.uint, value.bump).length : -1
  for (let n = 0; n < bytesOut.length; n++) {
    const prefix = bytesOut.subarray(0, n)
    if (n === skip) {
      const d = enc.decode(c.state(0, n, prefix))
      if (d.bump !== 0 || !same(d.peers, value.peers)) {
        throw new Error(`${name}: a prefix without the bump does not decode as bump 0 with the same peers`)
      }
      continue
    }
    let message = 'no error'
    try {
      enc.decode(c.state(0, n, prefix))
    } catch (err) {
      message = err.message
    }
    if (message !== 'Out of bounds') {
      throw new Error(`${name}: prefix of ${n} of ${bytesOut.length} bytes: expected 'Out of bounds', got ${message}`)
    }
  }

  out.cases.push({ name, type, value: wire(value), hex })
}

const file = path.join(__dirname, '..', 'vectors', 'hyperdht.json')
fs.writeFileSync(file, JSON.stringify(out, null, 2) + '\n')
console.log(`wrote hyperdht.json: ${out.cases.length} cases`)
