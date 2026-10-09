// Loads the shared test vectors in spec/vectors for the engine tests. Bare parses a required .json
// file itself, so no file-system module is needed.
const b4a = require('b4a')

// This file sits in app/engine/test/helpers, four levels below the repo root.
const VECTORS = '../../../../spec/vectors/'

function load(name) {
  try {
    return require(VECTORS + name)
  } catch (err) {
    if (err.code === 'MODULE_NOT_FOUND') {
      throw new Error(`missing vector file spec/vectors/${name}: generate it or check the name`)
    }
    throw err
  }
}

function hex(str) {
  if (!/^([0-9a-f]{2})*$/.test(str)) {
    throw new Error('hex: input must be lowercase hex with an even length')
  }
  return b4a.from(str, 'hex')
}

module.exports = { load, hex }
