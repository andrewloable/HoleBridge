// Writes spec/vectors/protomux.json: the frames Protomux 3.12.1 writes for one scripted session, and
// the events a receiving Protomux reports when it reads those frames, for the Go tests in
// pears/protomux.
//
// The stream here keeps each write as one frame. Protomux takes message boundaries from its stream
// (over the Noise secret stream each write arrives whole), so every entry in "frames" is one message.
//
// Sender: channel "holebridge" with no ID opens with the handshake "hello", sends on indexes 0 and
// 3, then channel "echo" with ID "b1" opens with no handshake and sends on index 0. Those three
// messages go out in one batch (cork, uncork). Then "holebridge" closes.
//
// Receiver: pairs with "holebridge" (messages 0 to 3) and "echo" with ID "b1" (message 0), and
// records what Protomux reports. "unknown_index" is a second session where the sender uses index 5,
// which the receiver does not have: the receiver must ignore it and keep the channel open.
//
// Run: cd spec/gen && npm ci && node protomux.js

const fs = require('fs')
const path = require('path')
const { EventEmitter } = require('events')
const Protomux = require('protomux')
const c = require('compact-encoding')

const HANDSHAKE = Buffer.from('hello')
const ECHO_ID = Buffer.from('b1')
const MIB = 1024 * 1024
const { createHash } = require('crypto')

const hex = (b) => (b ? Buffer.from(b).toString('hex') : '')
const tick = () => new Promise((resolve) => setImmediate(resolve))

// The stream under one Protomux. Each write is recorded as one frame, named by the label set
// before the call that writes it.
class Wire extends EventEmitter {
  constructor() {
    super()
    this.destroyed = false
    this.userData = null
    this.frames = []
    this.label = ''
  }

  write(buffer) {
    this.frames.push({ name: this.label, hex: hex(buffer) })
    return true
  }

  pause() {}
  resume() {}
  end() {}
  destroy() {
    this.destroyed = true
  }
}

function senderFrames() {
  const wire = new Wire()
  const mux = new Protomux(wire)

  const a = mux.createChannel({ protocol: 'holebridge', handshake: c.raw })
  const a0 = a.addMessage({})
  a.addMessage({})
  a.addMessage({})
  const a3 = a.addMessage({})
  wire.label = 'open_holebridge'
  a.open(HANDSHAKE)
  wire.label = 'message_0'
  a0.send(Buffer.from('zero'))
  wire.label = 'message_3'
  a3.send(Buffer.from('three'))

  const b = mux.createChannel({ protocol: 'echo', id: ECHO_ID })
  const b0 = b.addMessage({})
  wire.label = 'open_echo'
  b.open()

  wire.label = 'batch'
  mux.cork()
  a0.send(Buffer.from('batch-zero'))
  a3.send(Buffer.from('batch-three'))
  b0.send(Buffer.from('b-zero'))
  mux.uncork()

  wire.label = 'close_holebridge'
  a.close()
  return wire.frames
}

function unknownIndexFrames() {
  const wire = new Wire()
  const mux = new Protomux(wire)

  const a = mux.createChannel({ protocol: 'holebridge', handshake: c.raw })
  const messages = Array.from({ length: 6 }, () => a.addMessage({}))
  wire.label = 'open_holebridge'
  a.open(HANDSHAKE)
  wire.label = 'message_5'
  messages[5].send(Buffer.from('stray'))
  wire.label = 'message_0'
  messages[0].send(Buffer.from('kept'))
  return wire.frames
}

// Feeds frames to a receiving Protomux one at a time, letting it run between frames as frames
// arriving over time would. Returns the events it reported, in order.
async function receive(frames) {
  const events = []
  const wire = new Wire()
  const mux = new Protomux(wire)

  const record = (protocol, id, handlers) => {
    const idHex = hex(id)
    const at = (fields) => events.push({ protocol, id: idHex, ...fields })
    return {
      onopen: (handshake) => at({ event: 'open', handshake: hex(handshake) }),
      onclose: (isRemote) => at({ event: 'close', is_remote: isRemote }),
      messages: handlers.map((index) => ({
        onmessage: (payload) => at({ event: 'message', index, payload: hex(payload) }),
      })),
    }
  }

  mux.pair({ protocol: 'holebridge' }, () => {
    mux.createChannel({
      protocol: 'holebridge',
      handshake: c.raw,
      ...record('holebridge', null, [0, 1, 2, 3]),
    })
  })
  mux.pair({ protocol: 'echo', id: ECHO_ID }, () => {
    mux.createChannel({ protocol: 'echo', id: ECHO_ID, ...record('echo', ECHO_ID, [0]) })
  })

  for (const frame of frames) {
    wire.emit('data', Buffer.from(frame.hex, 'hex'))
    await tick()
  }
  await tick()
  if (wire.frames.length !== 0) throw new Error('the receiver wrote frames; it should only read')
  return events
}

// Reply batching: a sender opens "echo" with no ID and sends three messages on index 0 in one batch. The
// receiver answers each one from its handler (see answer), and upstream corks the received batch while it
// handles it, so the replies go out as one batch.
function replySenderFrames() {
  const wire = new Wire()
  const mux = new Protomux(wire)
  const ch = mux.createChannel({ protocol: 'echo' })
  const msg = ch.addMessage({})
  wire.label = 'reply_open'
  ch.open()
  wire.label = 'reply_batch'
  mux.cork()
  msg.send(Buffer.from('one'))
  msg.send(Buffer.from('two'))
  msg.send(Buffer.from('six'))
  mux.uncork()
  return wire.frames
}

// Feeds frames to a receiving Protomux that answers "echo", and returns the frames it writes.
async function answer(frames) {
  const wire = new Wire()
  const mux = new Protomux(wire)
  mux.pair({ protocol: 'echo' }, () => {
    const ch = mux.createChannel({ protocol: 'echo' })
    let reply = null
    ch.addMessage({ onmessage: (payload) => reply.send(payload) })
    reply = ch.addMessage({})
    ch.open() // this side's open answers the remote one, and gives the channel a local id to reply on
  })
  for (const frame of frames) {
    wire.emit('data', Buffer.from(frame.hex, 'hex'))
    await tick()
  }
  await tick()
  return wire.frames
}

// Frames of a sender whose batch crosses MAX_BATCH: one channel, nine messages of 1 MiB sent corked, so
// upstream sends the first eight as one batch and the ninth as another. The Go test checks each frame's
// length and SHA-256, since the hex of the frames would be tens of megabytes.
function splitScenario() {
  const wire = new Wire()
  const mux = new Protomux(wire)
  const ch = mux.createChannel({ protocol: 'bulk' })
  const msg = ch.addMessage({})
  wire.label = 'split_open'
  ch.open()
  wire.label = 'split_batch'
  mux.cork()
  for (let i = 0; i < 9; i++) msg.send(Buffer.alloc(MIB, i + 1))
  mux.uncork()
  return wire.frames.map((f) => ({
    name: f.name,
    length: Buffer.from(f.hex, 'hex').length,
    sha256: createHash('sha256').update(Buffer.from(f.hex, 'hex')).digest('hex'),
  }))
}

async function main() {
  const frames = senderFrames()
  const unknownFrames = unknownIndexFrames()
  const replyInput = replySenderFrames()
  const replyOut = await answer(replyInput)
  const out = {
    frames,
    events: await receive(frames),
    unknown_index: { frames: unknownFrames, events: await receive(unknownFrames) },
    reply_batch: { input: replyInput, replies: replyOut },
    split_batch: splitScenario(),
  }
  const file = path.join(__dirname, '..', 'vectors', 'protomux.json')
  fs.writeFileSync(file, JSON.stringify(out, null, 2) + '\n')
  console.log(
    `wrote ${frames.length} sender frames, ${out.events.length} receiver events, ` +
      `and ${unknownFrames.length} unknown-index frames to ${file}`
  )
}

main().catch((err) => {
  console.error(err)
  process.exit(1)
})
