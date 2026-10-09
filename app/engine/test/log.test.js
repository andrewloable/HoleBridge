const test = require('brittle')
const { secret, createLogger } = require('../lib/log.js')

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

test('String() of a secret is [redacted]', catchThrows((t) => {
  t.is(String(secret(Buffer.from([1, 2, 3]))), '[redacted]')
}))

test('JSON.stringify of a secret does not contain the value', catchThrows((t) => {
  const out = JSON.stringify({ k: secret('7KQM4X9TR') })
  t.ok(!out.includes('7KQ'), 'the secret value is not serialized')
}))

test('reveal() returns the original value', catchThrows((t) => {
  t.is(secret('7KQM4X9TR').reveal(), '7KQM4X9TR')
}))

test('a warn logger drops info and writes warn', catchThrows((t) => {
  const lines = []
  const log = createLogger({ level: 'warn', write: (line) => lines.push(line) })
  log.info('dropped')
  log.warn('kept')
  t.is(lines.length, 1, 'one line is written')
  t.is(JSON.parse(lines[0]).msg, 'kept')
}))

test('a written line is valid JSON with level and msg', catchThrows((t) => {
  const lines = []
  const log = createLogger({ write: (line) => lines.push(line) })
  log.error('boom', { code: 7 })
  const entry = JSON.parse(lines[0])
  t.is(entry.level, 'error')
  t.is(entry.msg, 'boom')
}))

test('a secret field is written as [redacted]', catchThrows((t) => {
  const lines = []
  const log = createLogger({ write: (line) => lines.push(line) })
  log.info('connect', { key: secret('x') })
  t.ok(lines[0].includes('"key":"[redacted]"'), 'the field prints as [redacted]')
}))

test('util.inspect shows a secret as [redacted]', catchThrows((t) => {
  const shown = secret('7KQM4X9TR')[Symbol.for('nodejs.util.inspect.custom')]()
  t.is(shown, '[redacted]')
}))

test('a debug logger writes every level', catchThrows((t) => {
  const lines = []
  const log = createLogger({ level: 'debug', write: (line) => lines.push(line) })
  log.error('e')
  log.warn('w')
  log.info('i')
  log.debug('d')
  t.is(lines.map((line) => JSON.parse(line).level).join(), 'error,warn,info,debug')
}))

test('the default level is info', catchThrows((t) => {
  const lines = []
  const log = createLogger({ write: (line) => lines.push(line) })
  log.debug('dropped')
  log.info('kept')
  t.is(lines.length, 1, 'one line is written')
  t.is(JSON.parse(lines[0]).level, 'info')
}))

test('no secret value is written, however it is passed', catchThrows((t) => {
  const lines = []
  const log = createLogger({ write: (line) => lines.push(line) })
  const s = () => secret('7KQM4X9TR')
  log.info(`template ${s()}`)
  log.info('array', { list: [s()] })
  log.info('nested', { a: { b: s() } })
  log.info('fields', s())
  log.info(s())
  t.is(lines.length, 5, 'five lines are written')
  for (const line of lines) t.ok(!line.includes('7KQ'), 'the secret value is not written')
  t.is(JSON.parse(lines[3]).msg, 'fields', 'a secret passed as fields keeps the line a JSON object')
}))

test('the default writer sends each line to console.error', catchThrows((t) => {
  const original = console.error
  const lines = []
  console.error = (line) => lines.push(line)
  try {
    createLogger().info('hello')
  } finally {
    console.error = original
  }
  t.is(lines.length, 1, 'one line is written')
  t.is(JSON.parse(lines[0]).msg, 'hello')
}))
