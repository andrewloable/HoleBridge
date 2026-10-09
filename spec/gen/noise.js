// Writes spec/vectors/noise.json: one Noise IK handshake on the Ed25519 curve, as hyperdht runs it
// between peers (noise-handshake 4.2.0 with noise-curve-ed 2.1.0, prologue NS.PEER_HANDSHAKE).
//
// The payloads are raw bytes. hyperdht wraps them in its noisePayload encoding, which the vector
// leaves out.
//
// The static keys come from fixed labels. The ephemeral keys are fixed by wrapping the curve's
// generateKeyPair, so each side draws the key the vector records. The handshake zeroes key and
// message bytes in place when it finishes (_clear), so every value is copied before a handshake runs.
const fs = require('fs')
const path = require('path')
const sodium = require('sodium-universal')
const curve = require('noise-curve-ed')
const NoiseHandshake = require('noise-handshake')
const { NS } = require('hyperdht/lib/constants')

// Test values derived from fixed labels, so they can be rederived without this file.
function fromLabel(label) {
  const out = Buffer.alloc(32)
  sodium.crypto_generichash(out, Buffer.from(label))
  return out
}

const hex = (b) => Buffer.from(b).toString('hex')

// A key pair as the vector records it: the 32-byte public key and the 64-byte secret, libsodium's layout.
const keyJSON = (kp) => ({ public: hex(kp.publicKey), secret: hex(kp.secretKey) })

// The curve with generateKeyPair fixed to kp. Each call returns copies, so the handshake cannot zero kp.
const withEphemeral = (kp) => ({
  ...curve,
  generateKeyPair: () => ({
    publicKey: Buffer.from(kp.publicKey),
    secretKey: Buffer.from(kp.secretKey),
  }),
})

const initiatorStatic = curve.generateKeyPair(fromLabel('holebridge noise vector initiator static'))
const responderStatic = curve.generateKeyPair(fromLabel('holebridge noise vector responder static'))
const initiatorEphemeral = curve.generateKeyPair(fromLabel('holebridge noise vector initiator ephemeral'))
const responderEphemeral = curve.generateKeyPair(fromLabel('holebridge noise vector responder ephemeral'))

const initiatorPayload = Buffer.from('noise vector initiator payload')
const responderPayload = Buffer.from('noise vector responder payload')
const prologue = NS.PEER_HANDSHAKE

// Recorded before any handshake runs, since the handshake zeroes the ephemeral secrets it used.
const initiatorKeys = {
  static: keyJSON(initiatorStatic),
  ephemeral: keyJSON(initiatorEphemeral),
  payload: hex(initiatorPayload),
}
const responderKeys = {
  static: keyJSON(responderStatic),
  ephemeral: keyJSON(responderEphemeral),
  payload: hex(responderPayload),
}

const initiator = new NoiseHandshake('IK', true, initiatorStatic, { curve: withEphemeral(initiatorEphemeral) })
initiator.initialise(prologue, responderStatic.publicKey)
const message1 = initiator.send(initiatorPayload)

// recv keeps references into the buffer it reads, which the responder zeroes in send, so it gets a copy.
const responder = new NoiseHandshake('IK', false, responderStatic, { curve: withEphemeral(responderEphemeral) })
responder.initialise(prologue)
const gotPayload1 = responder.recv(Buffer.from(message1))
const message2 = responder.send(responderPayload)

const gotPayload2 = initiator.recv(Buffer.from(message2))

if (!gotPayload1.equals(initiatorPayload)) throw new Error('responder read a different payload 1')
if (!gotPayload2.equals(responderPayload)) throw new Error('initiator read a different payload 2')
if (!initiator.hash.equals(responder.hash)) throw new Error('the two sides derived different hashes')
if (!initiator.tx.equals(responder.rx) || !initiator.rx.equals(responder.tx)) {
  throw new Error('the two sides derived different transport keys')
}

const { version: nhVersion } = require('noise-handshake/package.json')
const { version: ncVersion } = require('noise-curve-ed/package.json')
const vector = {
  description:
    'Noise IK on the Ed25519 curve as hyperdht runs it: fixed static and ephemeral keys, the hyperdht prologue, raw payloads, both handshake messages and the transport keys each side derives',
  reference: `noise-handshake ${nhVersion} with noise-curve-ed ${ncVersion}`,
  prologue: hex(prologue),
  hash: hex(initiator.hash),
  initiator: { ...initiatorKeys, tx: hex(initiator.tx), rx: hex(initiator.rx) },
  responder: { ...responderKeys, tx: hex(responder.tx), rx: hex(responder.rx) },
  message1: hex(message1),
  message2: hex(message2),
}

const out = path.join(__dirname, '..', 'vectors', 'noise.json')
fs.writeFileSync(out, JSON.stringify(vector, null, 2) + '\n')
console.log(`wrote noise.json: message 1 is ${message1.length} bytes, message 2 is ${message2.length} bytes`)
