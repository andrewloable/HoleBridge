// Writes spec/vectors/sodium-secretstream.json: libsodium's crypto_secretstream_xchacha20poly1305
// for one fixed key and header and a message sequence that covers every tag.
//
// libsodium's init_push draws a random header, which would change the output on every run. So the
// push state is set up with init_pull for the fixed header instead: both derive the same state from
// a header. Every ciphertext is then opened with init_pull, which checks that it is what the stream
// produces for that header.
const fs = require('fs')
const path = require('path')
const sodium = require('sodium-universal')

const C = 'crypto_secretstream_xchacha20poly1305_'
const STATEBYTES = sodium[C + 'STATEBYTES']
const ABYTES = sodium[C + 'ABYTES']
const TAG_MESSAGE = sodium[C + 'TAG_MESSAGE']
const TAG_PUSH = sodium[C + 'TAG_PUSH']
const TAG_REKEY = sodium[C + 'TAG_REKEY']
const TAG_FINAL = sodium[C + 'TAG_FINAL']

// Test values derived from fixed labels, so they can be rederived without this file.
function fromLabel(label, size) {
  const out = Buffer.alloc(size)
  sodium.crypto_generichash(out, Buffer.from(label))
  return out
}

const key = fromLabel('holebridge secretstream test key', 32)
const header = fromLabel('holebridge secretstream test header', 24)

// 1 MiB of a repeating pattern whose period does not divide 256.
const big = Buffer.alloc(1 << 20)
for (let i = 0; i < big.length; i++) big[i] = i % 251

const NONE = Buffer.alloc(0)
const AD = Buffer.from('associated data')

const messages = [
  { tag: TAG_MESSAGE, ad: NONE, message: Buffer.from('first message') },
  { tag: TAG_MESSAGE, ad: AD, message: NONE },
  { tag: TAG_PUSH, ad: NONE, message: Buffer.from('end of a push') },
  { tag: TAG_MESSAGE, ad: NONE, message: big },
  { tag: TAG_MESSAGE, ad: AD, message: Buffer.from('before the rekey') },
  { tag: TAG_REKEY, ad: NONE, message: Buffer.from('rekey after this message') },
  { tag: TAG_MESSAGE, ad: NONE, message: Buffer.from('after the rekey') },
  { tag: TAG_MESSAGE, ad: AD, message: Buffer.from('after the rekey, with ad') },
  { tag: TAG_FINAL, ad: NONE, message: NONE },
]

// libsodium treats a null ad as empty.
const adArg = (ad) => (ad.length ? ad : null)

const pushState = Buffer.alloc(STATEBYTES)
sodium[C + 'init_pull'](pushState, header, key)
const ciphertexts = messages.map(({ tag, ad, message }) => {
  const cipher = Buffer.alloc(message.length + ABYTES)
  sodium[C + 'push'](pushState, cipher, message, adArg(ad), tag)
  return cipher
})

const pullState = Buffer.alloc(STATEBYTES)
sodium[C + 'init_pull'](pullState, header, key)
messages.forEach(({ tag, ad, message }, i) => {
  const plain = Buffer.alloc(message.length)
  const gotTag = Buffer.alloc(1)
  sodium[C + 'pull'](pullState, plain, gotTag, ciphertexts[i], adArg(ad))
  if (!plain.equals(message) || gotTag[0] !== tag) {
    throw new Error(`message ${i} does not open to its message and tag`)
  }
})

const { version } = require('sodium-universal/package.json')
const vector = {
  description:
    'libsodium crypto_secretstream_xchacha20poly1305: one key and header, and a message sequence with every tag, an empty message and a 1 MiB message',
  reference: `sodium-universal ${version}`,
  key: key.toString('hex'),
  header: header.toString('hex'),
  messages: messages.map(({ tag, ad, message }, i) => ({
    tag,
    ad: ad.toString('hex'),
    message: message.toString('hex'),
    ciphertext: ciphertexts[i].toString('hex'),
  })),
}

const out = path.join(__dirname, '..', 'vectors', 'sodium-secretstream.json')
fs.writeFileSync(out, JSON.stringify(vector, null, 2) + '\n')
console.log(`wrote sodium-secretstream.json with ${messages.length} messages`)
