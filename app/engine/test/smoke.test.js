const test = require('brittle')

test('stdout guard routes console.log, info and debug to stderr', (t) => {
  const { log, info, debug } = console
  require('../index.js')
  t.is(console.log, console.error, 'console.log is console.error')
  t.is(console.info, console.error, 'console.info is console.error')
  t.is(console.debug, console.error, 'console.debug is console.error')
  // Put the originals back so the runner's TAP output still goes to stdout.
  Object.assign(console, { log, info, debug })
})
