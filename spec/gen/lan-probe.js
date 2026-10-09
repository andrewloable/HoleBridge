// Writes spec/vectors/lan-probe.json: the LAN probes and replies of docs/architecture.md (LAN route,
// "Probe and reply bytes"), and six probes the responder must refuse. The MAC key of each probe and
// reply is the LAN probe key of a host key in spec/vectors/key-derivation.json (lanProbeKey, which is
// the lan seed). The MAC is BLAKE2b-256 keyed with that key, over bytes 0 to 31, by sodium-universal.
//
// Test values only: the keys come from key-derivation.json, and the nonces, timestamps and ports are
// fixed here. Every byte is deterministic, so a second run leaves the file byte-identical.
//
// Run: cd spec/gen && npm ci && node lan-probe.js

'use strict'

const fs = require('fs')
const path = require('path')
const assert = require('assert/strict')
const sodium = require('sodium-universal')

const VECTORS = path.join(__dirname, '..', 'vectors')
const PROBE_SIZE = 256
const REPLY_SIZE = 64
const PROBE_MAGIC = 'HBLANQ1'
const REPLY_MAGIC = 'HBLANR1'

const derivation = JSON.parse(fs.readFileSync(path.join(VECTORS, 'key-derivation.json'), 'utf8'))
const [KEY_A, KEY_B] = derivation.keys.slice(0, 2).map((k) => k.lanProbeKey)

const NONCE_1 = '00112233445566778899aabbccddeeff'
const NONCE_2 = 'f0e1d2c3b4a5968778695a4b3c2d1e0f'
const NONCE_3 = '0f1e2d3c4b5a69788796a5b4c3d2e1f0'
const TIME_1 = 1767225600000 // 2026-01-01T00:00:00Z
const TIME_2 = 1767225612345
const TIME_3 = 1767225698765

// BLAKE2b-256 keyed with the LAN probe key over bytes 0 to 31 of the message.
function mac(lanKey, bytes) {
  const out = Buffer.alloc(32)
  sodium.crypto_generichash(out, bytes.subarray(0, 32), Buffer.from(lanKey, 'hex'))
  return out
}

// A probe: magic, version 1 (or the given one), nonce, timestamp, MAC, zero padding. The options
// build the invalid cases: a size other than 256, another magic or version, a MAC from another key,
// and a flipped MAC bit. Bytes 64 to 255 stay zero.
function buildProbe({ lanKey, nonce, timestampMs, size = PROBE_SIZE, magic = PROBE_MAGIC, version = 1, macKey = lanKey, flipMacBit = false }) {
  const b = Buffer.alloc(size)
  b.write(magic, 0, 'ascii')
  b[7] = version
  Buffer.from(nonce, 'hex').copy(b, 8)
  b.writeBigUInt64LE(BigInt(timestampMs), 24)
  mac(macKey, b).copy(b, 32)
  if (flipMacBit) b[32] ^= 0x01
  return b
}

// A reply: magic, version 1, the probe's nonce, the host's LAN TCP port (uint16, little-endian),
// zero bytes 26 to 31, and the MAC.
function buildReply(lanKey, nonce, port) {
  const b = Buffer.alloc(REPLY_SIZE)
  b.write(REPLY_MAGIC, 0, 'ascii')
  b[7] = 1
  Buffer.from(nonce, 'hex').copy(b, 8)
  b.writeUInt16LE(port, 24)
  mac(lanKey, b).copy(b, 32)
  return b
}

// The first fault in a probe under lanKey, or null when a host would answer it.
function probeFault(lanKey, bytes) {
  if (bytes.length !== PROBE_SIZE) return 'length'
  if (bytes.subarray(0, 7).toString('ascii') !== PROBE_MAGIC) return 'magic'
  if (bytes[7] !== 1) return 'version'
  if (!mac(lanKey, bytes).equals(bytes.subarray(32, 64))) return 'mac'
  return null
}

// A reply under lanKey, checked the way the app checks it: size, magic, version, nonce, MAC.
function replyFault(lanKey, nonce, bytes) {
  if (bytes.length !== REPLY_SIZE) return 'length'
  if (bytes.subarray(0, 7).toString('ascii') !== REPLY_MAGIC) return 'magic'
  if (bytes[7] !== 1) return 'version'
  if (!bytes.subarray(8, 24).equals(Buffer.from(nonce, 'hex'))) return 'nonce'
  if (!mac(lanKey, bytes).equals(bytes.subarray(32, 64))) return 'mac'
  return null
}

const probes = [
  { lanKey: KEY_A, nonce: NONCE_1, timestampMs: TIME_1 },
  { lanKey: KEY_A, nonce: NONCE_2, timestampMs: TIME_2 },
  { lanKey: KEY_B, nonce: NONCE_3, timestampMs: TIME_3 },
]
for (const p of probes) {
  assert.equal(probeFault(p.lanKey, buildProbe(p)), null)
}

const replies = [
  { lanKey: KEY_A, nonce: NONCE_1, port: 8443 },
  { lanKey: KEY_A, nonce: NONCE_2, port: 65535 },
  { lanKey: KEY_B, nonce: NONCE_3, port: 7000 },
]
for (const r of replies) {
  assert.equal(replyFault(r.lanKey, r.nonce, buildReply(r.lanKey, r.nonce, r.port)), null)
}

// Each invalid probe is built from the first valid probe and changes one thing. Each must fail.
const base = probes[0]
const invalid = [
  { lanKey: KEY_A, bytes: buildProbe({ ...base, size: 255 }), reason: 'length 255, not 256' },
  { lanKey: KEY_A, bytes: buildProbe({ ...base, size: 257 }), reason: 'length 257, not 256' },
  { lanKey: KEY_A, bytes: buildProbe({ ...base, magic: 'XBLANQ1' }), reason: 'wrong magic' },
  { lanKey: KEY_A, bytes: buildProbe({ ...base, version: 2 }), reason: 'version 2, not 1' },
  { lanKey: KEY_A, bytes: buildProbe({ ...base, flipMacBit: true }), reason: 'one MAC bit flipped' },
  { lanKey: KEY_A, bytes: buildProbe({ ...base, macKey: KEY_B }), reason: 'MAC made with the other host key' },
]
const expectedFaults = ['length', 'length', 'magic', 'version', 'mac', 'mac']
invalid.forEach((c, i) => assert.notEqual(probeFault(c.lanKey, c.bytes), null, `invalid case ${i} is accepted`))
invalid.forEach((c, i) => assert.equal(probeFault(c.lanKey, c.bytes), expectedFaults[i], `invalid case ${i} fails for another reason`))

const out = {
  probes: probes.map((p) => ({ lanKey: p.lanKey, nonce: p.nonce, timestampMs: p.timestampMs, hex: buildProbe(p).toString('hex') })),
  replies: replies.map((r) => ({ lanKey: r.lanKey, nonce: r.nonce, port: r.port, hex: buildReply(r.lanKey, r.nonce, r.port).toString('hex') })),
  invalid: invalid.map((c) => ({ lanKey: c.lanKey, hex: c.bytes.toString('hex'), reason: c.reason })),
}
const file = path.join(VECTORS, 'lan-probe.json')
fs.writeFileSync(file, JSON.stringify(out, null, 2) + '\n')
console.log(`wrote ${out.probes.length} probes, ${out.replies.length} replies and ${out.invalid.length} invalid probes to ${file}`)
