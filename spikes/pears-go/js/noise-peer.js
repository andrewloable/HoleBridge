'use strict'
// Spike step 3 peer (throwaway). Runs the JS side of a Noise IK handshake as hyperdht's
// NoiseWrap does (noise-handshake 4.2.0 with curve noise-curve-ed 2.1.0, prologue NS.PEER_HANDSHAKE),
// over length-framed stdin/stdout with the Go spike as the other end.
//   node noise-peer.js initiator|responder
// Framing: 2-byte big-endian length, then the bytes (upstream noise convention). Logs go to stderr.
// Static keys come from fixed spike labels (BLAKE2b-256 of the label), not secrets.

const NoiseHandshake = require('noise-handshake')
const curve = require('noise-curve-ed')
const sodium = require('sodium-universal')
const b4a = require('b4a')
const { NS } = require('hyperdht/lib/constants')

const role = process.argv[2]
if (role !== 'initiator' && role !== 'responder') {
  console.error('usage: node noise-peer.js initiator|responder')
  process.exit(2)
}
const initiator = role === 'initiator'

const LABEL_GO = 'holebridge spike noise go static'
const LABEL_JS = 'holebridge spike noise js static'
const PAYLOAD_INIT = 'spike initiator payload'
const PAYLOAD_RESP = 'spike responder payload'
const CHECK_INIT = 'initiator-check'

function labelSeed(label) {
  const out = b4a.alloc(32)
  sodium.crypto_generichash(out, b4a.from(label))
  return out
}

function zeroNonce() {
  return b4a.alloc(sodium.crypto_aead_chacha20poly1305_ietf_NPUBBYTES)
}

function seal(key, plaintext) {
  const ct = b4a.alloc(plaintext.byteLength + sodium.crypto_aead_chacha20poly1305_ietf_ABYTES)
  sodium.crypto_aead_chacha20poly1305_ietf_encrypt(ct, plaintext, null, null, zeroNonce(), key)
  return ct
}

// Returns the plaintext, or null when authentication fails.
function open(key, ciphertext) {
  const pt = b4a.alloc(ciphertext.byteLength - sodium.crypto_aead_chacha20poly1305_ietf_ABYTES)
  const ok = sodium.crypto_aead_chacha20poly1305_ietf_decrypt(pt, null, ciphertext, null, zeroNonce(), key)
  return ok === false ? null : pt
}

// Frame reader over stdin: yields one Buffer per frame.
async function* frames(stream) {
  let buf = b4a.alloc(0)
  for await (const chunk of stream) {
    buf = b4a.concat([buf, chunk])
    while (buf.byteLength >= 2) {
      const n = buf.readUInt16BE(0)
      if (buf.byteLength < 2 + n) break
      yield buf.subarray(2, 2 + n)
      buf = buf.subarray(2 + n)
    }
  }
}

function writeFrame(buf) {
  if (buf.byteLength > 0xffff) throw new Error('frame too large')
  const head = b4a.alloc(2)
  head.writeUInt16BE(buf.byteLength, 0)
  process.stdout.write(b4a.concat([head, buf]))
}

async function main() {
  const staticKp = curve.generateKeyPair(labelSeed(LABEL_JS))
  const goSeedPub = curve.generateKeyPair(labelSeed(LABEL_GO)).publicKey
  const hs = new NoiseHandshake('IK', initiator, staticKp, { curve })
  hs.initialise(NS.PEER_HANDSHAKE, initiator ? goSeedPub : undefined)

  const it = frames(process.stdin)[Symbol.asyncIterator]()

  let okLocal = false
  if (initiator) {
    writeFrame(hs.send(b4a.from(PAYLOAD_INIT)))
    const msg2 = (await it.next()).value
    const pt = hs.recv(msg2)
    if (!b4a.equals(pt, b4a.from(PAYLOAD_RESP))) throw new Error('payload 2 mismatch')

    // transport check: initiator sends first, responder answers with its verdict and hash
    writeFrame(seal(hs.tx, b4a.from(CHECK_INIT)))
    const reply = open(hs.rx, (await it.next()).value)
    if (reply === null || reply.byteLength !== 65) throw new Error('transport reply did not authenticate')
    okLocal = reply[0] === 1 && b4a.equals(reply.subarray(1), hs.hash)
    console.error(`js initiator: handshake complete, transport reply authenticated, responder accepted=${reply[0] === 1}, hash equal=${b4a.equals(reply.subarray(1), hs.hash)}`)
  } else {
    const pt = hs.recv((await it.next()).value)
    if (!b4a.equals(pt, b4a.from(PAYLOAD_INIT))) throw new Error('payload 1 mismatch')
    writeFrame(hs.send(b4a.from(PAYLOAD_RESP)))

    const check = (await it.next()).value
    const got = open(hs.rx, check)
    const authed = got !== null && b4a.equals(got, b4a.from(CHECK_INIT))
    const verdict = b4a.alloc(1, authed ? 1 : 0)
    writeFrame(seal(hs.tx, b4a.concat([verdict, hs.hash])))
    okLocal = authed
    console.error(`js responder: handshake complete, initiator check authenticated=${authed}`)
  }
  process.exitCode = okLocal ? 0 : 1
  process.stdin.destroy()
}

main().catch((err) => {
  console.error('js peer failed:', err && err.message)
  process.exitCode = 1
})
