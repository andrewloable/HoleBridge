// A protomux 3.12.1 peer over @hyperswarm/secret-stream 6.9.2 for the Go interop test in interop/protomux_test.go.
// The test runs it as a child process. As the responder it serves one connection; as the initiator it dials the Go
// responder. Nothing here is part of HoleBridge's runtime.
//
// Usage:
//   node protomux-peer.js responder <port>
//   node protomux-peer.js initiator <port> <responder public key, 64 hex>
//
// Both roles use the key pairs of secretstream-peer.js and the IK pattern. On connect the peer makes one Protomux on
// the stream and pairs protocol "holebridge-test" for channels Go opens with no ID. Such a channel answers with the
// handshake "js-answer" and echoes every message it receives on index 0 or 1, back on the same index. Channels the
// peer opens (command "open") collect the messages Go sends back, so that a "send" command can check the echoes.
//
// Stdio protocol, one JSON object per line.
//   either role, after the secret-stream handshake: {"connected":true,"handshake":"<128 hex>","remotePublicKey":"<64 hex>"}
//   responder, once listening:                       {"ready":true,"port":N,"publicKey":"<64 hex>"}
//   every command on stdin is answered by exactly one line:
//     {"cmd":"open","protocol":P,"id":ID,"handshake":TEXT}  opens a channel (ID "" for none), answers {"ok":true}
//     {"cmd":"await","protocol":P,"id":ID,"event":"open"}   answers {"ok":true,"event":"open","handshake":"<hex>"}
//     {"cmd":"await","protocol":P,"id":ID,"event":"close"}  answers {"ok":true,"event":"close","isRemote":BOOL}
//     {"cmd":"send","protocol":P,"id":ID,"count":N,"minBytes":A,"maxBytes":B,"batch":BOOL}
//        sends N messages on index i%2, corked as one batch when batch is true, waits for their N echoes, and
//        answers {"ok":true,"count":N,"bytes":M,"sent":"<sha256 hex>","echo":"<sha256 hex>","echoCount":K}
//     {"cmd":"close","protocol":P,"id":ID}                  closes the channel, answers {"ok":true}
//   A failed command answers {"ok":false,"error":"..."}; the process then exits with status 1 once stdin closes.
// The process exits on its own when the connection ends and stdin closes.

'use strict'

const crypto = require('node:crypto')
const net = require('node:net')
const readline = require('node:readline')
const c = require('compact-encoding')
const Protomux = require('protomux')
const SecretStream = require('@hyperswarm/secret-stream')

const HOST = '127.0.0.1'
const PATTERN = 'IK'
const PROTOCOL = 'holebridge-test'
const ANSWER = 'js-answer'

// Fixed Ed25519 seeds, the same as secretstream-peer.js. The Go test derives the same key pairs. They are test values.
const RESPONDER_SEED = Buffer.from('3a5c9834e3a11d3d9f1a8dd610d09b984f19cf3437369e23c0df27fad39300ef', 'hex')
const INITIATOR_SEED = Buffer.from('bbfd093816e33c8e726e19138035ae5b1d5d52874a6c25ce13c7e1b344cf4c93', 'hex')

const hex = (buf) => Buffer.from(buf).toString('hex')

function emit(obj) {
  process.stdout.write(JSON.stringify(obj) + '\n')
}

function fail(err) {
  emit({ ok: false, error: err.message })
  process.exitCode = 1
}

function connectedLine(s) {
  return { connected: true, handshake: hex(s.handshakeHash), remotePublicKey: hex(s.remotePublicKey) }
}

// key names a channel the way the commands do: protocol and ID, with no ID as the empty string.
function key(protocol, id) {
  return `${protocol}#${id || ''}`
}

// Events records what the channels report. An await command answers with an event whether it came before the
// command or after it.
class Events {
  constructor() {
    this.seen = new Map()
    this.waiting = new Map()
  }

  record(name, value) {
    this.seen.set(name, value)
    const resolve = this.waiting.get(name)
    if (resolve !== undefined) {
      this.waiting.delete(name)
      resolve(value)
    }
  }

  wait(name) {
    if (this.seen.has(name)) return Promise.resolve(this.seen.get(name))
    return new Promise((resolve) => {
      this.waiting.set(name, resolve)
    })
  }
}

// Inbox holds the messages a collecting channel receives, so that a send command can wait for its echoes.
class Inbox {
  constructor() {
    this.items = []
    this.want = 0
    this.wake = null
  }

  reset() {
    this.items = []
    this.want = 0
    this.wake = null
  }

  push(index, payload) {
    this.items.push({ index, payload: Buffer.from(payload) })
    if (this.wake !== null && this.items.length >= this.want) {
      const wake = this.wake
      this.wake = null
      wake()
    }
  }

  ready(n) {
    this.want = n
    if (this.items.length >= n) return Promise.resolve()
    return new Promise((resolve) => {
      this.wake = resolve
    })
  }
}

// digest hashes a sequence of messages: for each one, its index, its length as four big-endian bytes, its payload.
function digest(items) {
  const h = crypto.createHash('sha256')
  for (const { index, payload } of items) {
    const head = Buffer.alloc(5)
    head[0] = index
    head.writeUInt32BE(payload.length, 1)
    h.update(head)
    h.update(payload)
  }
  return h.digest('hex')
}

function integer(v, name) {
  const n = Number(v)
  if (!Number.isSafeInteger(n) || n < 0) throw new Error(`${name} must be a non-negative integer`)
  return n
}

// setupMux makes the Protomux for one connection and returns the function that answers its commands.
function setupMux(s) {
  const mux = new Protomux(s)
  const events = new Events()
  const channels = new Map() // key -> { channel, inbox }

  // A channel Go opens with no ID: answer it, and echo whatever arrives on index 0 or 1.
  mux.pair({ protocol: PROTOCOL }, () => {
    const k = key(PROTOCOL, null)
    const messages = [0, 1].map((index) => ({
      encoding: c.raw,
      onmessage: (payload, channel) => channel.messages[index].send(payload),
    }))
    const channel = mux.createChannel({
      protocol: PROTOCOL,
      handshake: c.raw,
      messages,
      onopen: (handshake) => events.record(`open ${k}`, { handshake }),
      onclose: (isRemote) => events.record(`close ${k}`, { isRemote }),
    })
    if (channel === null) throw new Error(`cannot answer the channel Go opened on ${k}`)
    channel.open(Buffer.from(ANSWER))
  })

  function open(protocol, id, handshake) {
    const k = key(protocol, id)
    const inbox = new Inbox()
    const messages = [0, 1].map((index) => ({
      encoding: c.raw,
      onmessage: (payload) => inbox.push(index, payload),
    }))
    const channel = mux.createChannel({
      protocol,
      id: id ? Buffer.from(id) : null,
      handshake: c.raw,
      messages,
      onopen: (h) => events.record(`open ${k}`, { handshake: h }),
      onclose: (isRemote) => events.record(`close ${k}`, { isRemote }),
    })
    if (channel === null) throw new Error(`cannot open ${k}: it is already open, or the stream is closed`)
    channels.set(k, { channel, inbox })
    channel.open(Buffer.from(handshake, 'utf8'))
  }

  function lookup(protocol, id) {
    const entry = channels.get(key(protocol, id))
    if (entry === undefined) throw new Error(`no channel ${key(protocol, id)} opened by this peer`)
    return entry
  }

  async function send(cmd) {
    const { channel, inbox } = lookup(cmd.protocol, cmd.id)
    const count = integer(cmd.count, 'count')
    const minBytes = integer(cmd.minBytes ?? 1, 'minBytes')
    const maxBytes = integer(cmd.maxBytes, 'maxBytes')
    if (minBytes < 1 || maxBytes < minBytes) throw new Error('need 1 <= minBytes <= maxBytes')

    inbox.reset()
    const sent = []
    let bytes = 0
    if (cmd.batch) mux.cork()
    for (let i = 0; i < count; i++) {
      const payload = crypto.randomBytes(crypto.randomInt(minBytes, maxBytes + 1))
      const index = i % 2
      sent.push({ index, payload })
      bytes += payload.length
      channel.messages[index].send(payload)
    }
    if (cmd.batch) mux.uncork()
    await inbox.ready(count)
    return {
      ok: true,
      count,
      bytes,
      sent: digest(sent),
      echo: digest(inbox.items),
      echoCount: inbox.items.length,
    }
  }

  async function handle(cmd) {
    switch (cmd.cmd) {
      case 'open':
        open(cmd.protocol, cmd.id, cmd.handshake)
        return { ok: true }
      case 'await': {
        const value = await events.wait(`${cmd.event} ${key(cmd.protocol, cmd.id)}`)
        if (cmd.event === 'open') return { ok: true, event: 'open', handshake: hex(value.handshake) }
        return { ok: true, event: 'close', isRemote: value.isRemote }
      }
      case 'send':
        return send(cmd)
      case 'close':
        lookup(cmd.protocol, cmd.id).channel.close()
        return { ok: true }
      default:
        throw new Error(`unknown command ${cmd.cmd}`)
    }
  }

  return handle
}

let stream = null // the secret stream of this process, once it exists
let handleCommand = null // set once the stream has connected
let chain = Promise.resolve()

function attach(s) {
  stream = s
  s.on('error', fail)
  s.on('connect', () => {
    handleCommand = setupMux(s)
    emit(connectedLine(s))
  })
  s.on('close', () => rl.close())
}

async function reply(line) {
  let out
  try {
    if (handleCommand === null) throw new Error('no command before the secret stream connects')
    out = await handleCommand(JSON.parse(line))
  } catch (err) {
    out = { ok: false, error: err.message }
    process.exitCode = 1
  }
  emit(out)
}

const rl = readline.createInterface({ input: process.stdin })
rl.on('line', (line) => {
  chain = chain.then(() => reply(line))
})
// Stdin closing means the test is done with this peer. Before a connection there is nothing to end.
rl.on('close', () => {
  if (stream !== null) stream.destroy()
  else process.exit()
})

function runResponder(port) {
  const keyPair = SecretStream.keyPair(RESPONDER_SEED)
  const server = net.createServer({ allowHalfOpen: true }, (socket) => {
    server.close() // one connection only; the listener closes so that the process can exit when it ends
    attach(new SecretStream(false, socket, { keyPair, pattern: PATTERN }))
  })
  server.on('error', fail)
  server.listen(port, HOST, () => {
    emit({ ready: true, port: server.address().port, publicKey: hex(keyPair.publicKey) })
  })
}

function runInitiator(port, responderKey) {
  if (responderKey.length !== 32) throw new Error('the responder public key must be 32 bytes of hex')
  const keyPair = SecretStream.keyPair(INITIATOR_SEED)
  const socket = net.connect({ host: HOST, port, allowHalfOpen: true })
  attach(new SecretStream(true, socket, { keyPair, remotePublicKey: responderKey, pattern: PATTERN }))
}

function main(argv) {
  const [role, portArg, keyArg] = argv
  const port = Number(portArg)
  if (!Number.isInteger(port) || port < 0 || port > 65535) throw new Error('port must be 0 to 65535')
  if (role === 'responder') runResponder(port)
  else if (role === 'initiator') runInitiator(port, Buffer.from(keyArg ?? '', 'hex'))
  else throw new Error('role must be responder or initiator')
}

try {
  main(process.argv.slice(2))
} catch (err) {
  fail(err)
}
