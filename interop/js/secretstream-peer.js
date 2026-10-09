// A @hyperswarm/secret-stream 6.9.2 peer for the Go interop test in interop/secretstream_test.go. The test
// runs it as a child process. As the responder it echoes everything it receives. As the initiator it sends
// what the test asks for and checks the echo. Nothing here is part of HoleBridge's runtime.
//
// Usage:
//   node secretstream-peer.js responder <port>
//   node secretstream-peer.js initiator <port> <responder public key, 64 hex>
//
// The responder listens on 127.0.0.1:<port> (0 picks a free port) and serves one connection. The initiator
// connects to 127.0.0.1:<port>. Both use the fixed key pairs below. The handshake is IK: upstream defaults to
// XX, so the pattern is passed explicitly, as hyperdht does and as the Go side speaks.
//
// Stdio protocol, one JSON object per line.
//   responder, once listening:  {"ready":true,"port":N,"publicKey":"<64 hex>"}
//   either role, after handshake: {"connected":true,"handshake":"<128 hex>","remotePublicKey":"<64 hex>"}
//   responder, when the peer ends its side: {"ended":true,"received":"<sha256 hex>","bytes":N}
//     (it has echoed every byte, then ends its own side)
//   initiator commands, one per line on stdin, each answered by one line:
//     {"cmd":"send","count":N,"minBytes":A,"maxBytes":B}  sends N random messages of A to B bytes, waits for
//       their echo, and answers {"ok":true,"sent":"<sha256 hex>","bytes":N,"echo":"<sha256 hex>","echoBytes":M}
//     {"cmd":"close"}  ends its side and answers {"ok":true,"ended":true} once the peer ends its side
//   A failure answers {"ok":false,"error":"..."} and the process exits with status 1.
// The process exits on its own when the connection is closed, or when stdin closes.

'use strict'

const crypto = require('node:crypto')
const net = require('node:net')
const readline = require('node:readline')
const SecretStream = require('@hyperswarm/secret-stream')

const HOST = '127.0.0.1'
const PATTERN = 'IK'

// Fixed Ed25519 seeds. The Go test derives the same key pairs from the same seeds. They are test values.
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

function runResponder(port) {
  const keyPair = SecretStream.keyPair(RESPONDER_SEED)
  const server = net.createServer({ allowHalfOpen: true }, (socket) => {
    server.close() // one connection only; the listener closes so that the process can exit when it ends
    serve(socket, keyPair)
  })
  server.on('error', fail)
  server.listen(port, HOST, () => {
    emit({ ready: true, port: server.address().port, publicKey: hex(keyPair.publicKey) })
  })
}

// serve echoes every byte the responder's peer sends, then ends its own side when the peer ends.
function serve(socket, keyPair) {
  const s = new SecretStream(false, socket, { keyPair, pattern: PATTERN })
  const received = crypto.createHash('sha256')
  let bytes = 0
  s.on('error', fail)
  s.on('connect', () => emit(connectedLine(s)))
  s.on('data', (chunk) => {
    received.update(chunk)
    bytes += chunk.length
    s.write(chunk)
  })
  s.on('end', () => {
    emit({ ended: true, received: received.digest('hex'), bytes })
    s.end()
  })
}

function runInitiator(port, responderKey) {
  if (responderKey.length !== 32) throw new Error('the responder public key must be 32 bytes of hex')
  const keyPair = SecretStream.keyPair(INITIATOR_SEED)
  const socket = net.connect({ host: HOST, port, allowHalfOpen: true })
  const s = new SecretStream(true, socket, { keyPair, remotePublicKey: responderKey, pattern: PATTERN })

  // The echo of the current send command: its hash, its byte count, and the count it waits for.
  let echo = crypto.createHash('sha256')
  let echoBytes = 0
  let target = 0
  let onEcho = null
  let onPeerEnd = null
  const peerEnded = new Promise((resolve) => {
    onPeerEnd = resolve
  })

  s.on('error', fail)
  s.on('connect', () => emit(connectedLine(s)))
  s.on('data', (chunk) => {
    echo.update(chunk)
    echoBytes += chunk.length
    if (onEcho !== null && echoBytes >= target) {
      const done = onEcho
      onEcho = null
      done()
    }
  })
  s.on('end', () => onPeerEnd())

  async function send(cmd) {
    const count = integer(cmd.count, 'count')
    const minBytes = integer(cmd.minBytes ?? 1, 'minBytes')
    const maxBytes = integer(cmd.maxBytes, 'maxBytes')
    if (minBytes < 1 || maxBytes < minBytes) throw new Error('need 1 <= minBytes <= maxBytes')

    const sent = crypto.createHash('sha256')
    echo = crypto.createHash('sha256')
    echoBytes = 0
    let total = 0
    for (let i = 0; i < count; i++) {
      const size = crypto.randomInt(minBytes, maxBytes + 1)
      const msg = crypto.randomBytes(size)
      sent.update(msg)
      total += size
      s.write(msg)
    }
    target = total
    if (echoBytes < target) {
      await new Promise((resolve) => {
        onEcho = resolve
      })
    }
    return {
      ok: true,
      sent: sent.digest('hex'),
      bytes: total,
      echo: echo.digest('hex'),
      echoBytes,
    }
  }

  async function close() {
    s.end()
    await peerEnded
    return { ok: true, ended: true }
  }

  let chain = Promise.resolve()
  const rl = readline.createInterface({ input: process.stdin })
  rl.on('line', (line) => {
    chain = chain.then(() => handle(line))
  })
  // Stdin closing means the test is done with this peer.
  rl.on('close', () => s.destroy())
  s.on('close', () => rl.close())

  async function handle(line) {
    let reply
    try {
      const cmd = JSON.parse(line)
      if (cmd.cmd === 'send') reply = await send(cmd)
      else if (cmd.cmd === 'close') reply = await close()
      else throw new Error(`unknown command ${cmd.cmd}`)
    } catch (err) {
      reply = { ok: false, error: err.message }
      process.exitCode = 1
    }
    emit(reply)
  }
}

function integer(v, name) {
  const n = Number(v)
  if (!Number.isSafeInteger(n) || n < 0) throw new Error(`${name} must be a non-negative integer`)
  return n
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
