'use strict'

// Spike fixture only. The seeds are fixed labels hashed with BLAKE2b, so the
// run is repeatable. They are test values, not secrets, and nothing here is
// product code.
const sodium = require('sodium-universal')
const b4a = require('b4a')

function seedFor(label) {
  const seed = b4a.alloc(sodium.crypto_sign_SEEDBYTES)
  sodium.crypto_generichash(seed, b4a.from('holebridge unordered-datagrams spike v0 ' + label))
  return seed
}

function keyPairFor(label) {
  const publicKey = b4a.alloc(sodium.crypto_sign_PUBLICKEYBYTES)
  const secretKey = b4a.alloc(sodium.crypto_sign_SECRETKEYBYTES)
  sodium.crypto_sign_seed_keypair(publicKey, secretKey, seedFor(label))
  return { publicKey, secretKey }
}

module.exports = {
  host: () => keyPairFor('host'),
  relay: () => keyPairFor('relay')
}
