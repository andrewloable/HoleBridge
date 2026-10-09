const test = require('brittle')

test('stdout guard routes console.log, info and debug to stderr', (t) => {
  const { log, info, debug } = console
  // Without BareKit, index.js opens this process's own stdin and stdout as the IPC channel. The
  // runner owns those, so a no-op stand-in takes BareKit's place for this load.
  global.BareKit = { IPC: { write() {}, on() { return this } } }
  require('../index.js')
  delete global.BareKit
  t.is(console.log, console.error, 'console.log is console.error')
  t.is(console.info, console.error, 'console.info is console.error')
  t.is(console.debug, console.error, 'console.debug is console.error')
  // Put the originals back so the runner's TAP output still goes to stdout.
  Object.assign(console, { log, info, debug })
})
