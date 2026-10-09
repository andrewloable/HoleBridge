// HoleBridge app engine entry, loaded in a Bare worklet.
// Desktop runs this worklet as a bare subprocess whose stdout carries IPC frames, so console output
// must go to stderr. These assignments come before anything else loads.
console.log = console.error
console.info = console.error
console.debug = console.error

const b4a = require('b4a')

// The channel the frames travel on. BareKit exists only on mobile. Desktop has no BareKit, so this
// process's own stdin (fd 0) and stdout (fd 1) carry the frames, through bare-pipe. Both have the
// shape of BareKit.IPC: write(buf) and on('data', cb).
function stdio() {
  const Pipe = require('bare-pipe')
  const stdin = new Pipe(0)
  const stdout = new Pipe(1)
  return {
    write(buf) {
      stdout.write(buf)
    },
    on(event, cb) {
      stdin.on(event, cb)
      return this
    }
  }
}

// Wraps a byte channel so that it carries whole frames. Each frame goes on the wire as a 4-byte
// big-endian length, then its bytes (spec/ipc.md, Transport). write(frame) sends one frame, and
// on('data', cb) calls cb once per frame, however the channel split or joined the bytes.
function framed(channel) {
  let pending = b4a.alloc(0)
  let onFrame = null
  channel.on('data', (chunk) => {
    pending = b4a.concat([pending, chunk])
    while (pending.byteLength >= 4) {
      const size = new DataView(pending.buffer, pending.byteOffset, 4).getUint32(0)
      if (pending.byteLength < 4 + size) break
      const frame = pending.subarray(4, 4 + size)
      pending = pending.subarray(4 + size)
      if (onFrame) onFrame(frame)
    }
  })
  return {
    write(frame) {
      const head = b4a.alloc(4)
      new DataView(head.buffer, head.byteOffset, 4).setUint32(0, frame.byteLength)
      channel.write(b4a.concat([head, frame]))
    },
    on(event, cb) {
      if (event === 'data') onFrame = cb
      return this
    }
  }
}

const IPC = framed(typeof BareKit !== 'undefined' ? BareKit.IPC : stdio())

// Until the IPC task replaces this, echo every frame back to the host.
IPC.on('data', (frame) => IPC.write(frame))
