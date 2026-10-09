const test = require('brittle')
const { load } = require('./helpers/vectors.js')
const { normalize, format } = require('../lib/keys.js')

// brittle ends the whole run when a test body throws. Until the IMPL task lands, the code throws
// 'not implemented', so each body runs under catchThrows, which fails only its own test.
function catchThrows(fn) {
  return (t) => {
    try {
      fn(t)
    } catch (err) {
      t.fail(err.message)
    }
  }
}

// thrownBy(fn) returns what fn throws, or null when fn returns normally.
function thrownBy(fn) {
  try {
    fn()
  } catch (err) {
    return err
  }
  return null
}

test('every valid vector in key.json normalizes to its expected value', catchThrows((t) => {
  const { valid } = load('key.json')
  for (const { input, normalized } of valid) {
    t.is(normalize(input), normalized, `normalize(${JSON.stringify(input)})`)
  }
}))

test('every invalid vector in key.json throws HB-KEY-INVALID with its reason', catchThrows((t) => {
  const { invalid } = load('key.json')
  for (const { input, error } of invalid) {
    const label = `normalize(${JSON.stringify(input)})`
    const err = thrownBy(() => normalize(input))
    if (!err || err.code !== 'HB-KEY-INVALID') {
      t.fail(`${label} must throw HB-KEY-INVALID (reason ${error})`)
      continue
    }
    t.is(err.reason, error, `${label} reason`)
  }
}))

test('format(7KQM4X9TR) is 7KQ-M4X-9TR', catchThrows((t) => {
  t.is(format('7KQM4X9TR'), '7KQ-M4X-9TR')
}))

// Unicode upper-cases the dotless i to I and the long s to S, which would turn them into key symbols.
// Only ASCII letters may fold to upper case, so both must be rejected as characters.
test('no non-ASCII character folds into a key symbol', catchThrows((t) => {
  for (const input of ['7KQM4X9Tı', '7KQM4X9Tſ']) {
    const err = thrownBy(() => normalize(input))
    const label = `normalize(${JSON.stringify(input)})`
    t.is(err && err.code, 'HB-KEY-INVALID', `${label} code`)
    t.is(err && err.reason, 'character', `${label} reason`)
  }
}))

// Length is checked before character, as in the Go and Dart suites: a U is also a character fault.
test('when both faults are present, length is reported first', catchThrows((t) => {
  const err = thrownBy(() => normalize('7KQM4X9TU9'))
  t.is(err && err.code, 'HB-KEY-INVALID', 'code')
  t.is(err && err.reason, 'length', 'reason')
}))

// Length counts code points, as in the Go and Dart suites, so one emoji is one symbol. Counting
// UTF-16 units would make this input 10 symbols, a length fault.
test('a symbol outside the BMP counts as one character', catchThrows((t) => {
  const err = thrownBy(() => normalize('7KQM4X9T\u{1F600}'))
  t.is(err && err.code, 'HB-KEY-INVALID', 'code')
  t.is(err && err.reason, 'character', 'reason')
}))
