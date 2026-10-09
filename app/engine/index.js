// HoleBridge app engine entry, loaded in a Bare worklet.
// Desktop runs this worklet as a bare subprocess whose stdout carries IPC frames, so console output
// must go to stderr. These assignments come before anything else loads.
console.log = console.error
console.info = console.error
console.debug = console.error

// Until the IPC task replaces this, echo every frame back to the host. BareKit exists only on
// mobile; the desktop stdin/stdout channel is not wired yet.
if (typeof BareKit !== 'undefined') {
  BareKit.IPC.on('data', (frame) => BareKit.IPC.write(frame))
}
