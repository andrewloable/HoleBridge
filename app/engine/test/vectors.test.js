const test = require('brittle')
const b4a = require('b4a')
const { load, hex } = require('./helpers/vectors.js')

test('example vector loads and both fields decode', (t) => {
  const v = load('example.json')
  t.is(v.hello, 'world', 'hello field')
  t.is(v.bytes, '00ff', 'bytes field')
  t.is(b4a.toString(hex(v.bytes), 'hex'), '00ff', 'hex decodes bytes to the same bytes')
})

test('load names the missing vector file', (t) => {
  t.exception(() => load('no-such-vector.json'), /missing vector file spec\/vectors\/no-such-vector\.json/)
})

test('hex rejects uppercase, odd length and non-hex input', (t) => {
  for (const s of ['0A', 'abc', 'zz']) {
    t.exception(() => hex(s), /hex: input must be lowercase hex/, `hex(${s}) throws`)
  }
})
