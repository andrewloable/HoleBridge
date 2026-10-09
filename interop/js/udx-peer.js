// A udx-native 1.21.3 peer for the Go interop tests in interop/udx_test.go. The test runs it as a child
// process and drives it over stdio. Nothing here is part of HoleBridge's runtime.
//
// Usage:
//   node udx-peer.js echo   <peerPort> <remoteId> <localId>
//   node udx-peer.js sink   <peerPort> <remoteId> <localId>
//   node udx-peer.js source <peerPort> <remoteId> <localId>
//   node udx-peer.js self   <bytes>
// Every role binds its socket to 127.0.0.1 on a free port and connects its stream to 127.0.0.1:<peerPort>.
// echo and sink receive, and echo writes every byte back. source sends. self runs a sender and a receiver
// in this process, both ends udx-native: the JS-to-JS row of the throughput table.
//
// Stdio protocol, one JSON object per line.
//   every role, once its socket is bound:           {"ready":true,"port":N}
//   echo and sink, when the peer ends its side:     {"ended":true,"bytes":N,"hash":"<sha256 hex>","seconds":S}
//     seconds runs from the first byte received to the end of the stream.
//   echo and sink commands, one per line on stdin:
//     {"cmd":"report"}  -> {"ok":true,"messages":N,"valid":V,"bad":B,"dup":D}
//         the unordered messages received so far, checked and counted by index
//     {"cmd":"close"}   -> {"ok":true,"closed":true}, then the process exits with status 0
//   source commands:
//     {"cmd":"send","bytes":N}      -> {"ok":true,"sent":"<sha256 hex>","bytes":N,"seconds":S}
//         writes N bytes (one random 1 MiB block, repeated) and answers once every write is acknowledged
//     {"cmd":"messages","count":N}  -> {"ok":true,"messages":N}
//         sends N unordered messages
//     {"cmd":"close"}               -> as for the receivers
//   self, once both ends are done: {"ok":true,"bytes":N,"sent":"<hex>","received":"<hex>","seconds":S}
//   A failure answers {"ok":false,"error":"..."} and the process exits with status 1.
//
// A message is 1000 bytes: its index as uint32 LE, then byte j from 4 on is (index*31 + j) mod 256. The
// receiver checks each one from its index alone. interop/udx_test.go builds the same layout.

'use strict'

const crypto = require('node:crypto')
const { once } = require('node:events')
const readline = require('node:readline')
const UDX = require('udx-native')

const HOST = '127.0.0.1'
const BLOCK = 1 << 20
const MESSAGE_SIZE = 1000

function out(obj) {
  return new Promise((resolve) => process.stdout.write(JSON.stringify(obj) + '\n', resolve))
}

// fail reports the error and exits with status 1. The Go side fails the test on the line.
function fail(err) {
  process.exitCode = 1
  out({ ok: false, error: String((err && err.message) || err) }).then(() => process.exit(1))
}

function seconds(t0, t1) {
  return Number(t1 - t0) / 1e9
}

function messageBody(index) {
  const b = Buffer.alloc(MESSAGE_SIZE)
  b.writeUInt32LE(index, 0)
  for (let j = 4; j < MESSAGE_SIZE; j++) b[j] = (index * 31 + j) & 0xff
  return b
}

// messageIndex returns the index of a message that matches messageBody, or -1.
function messageIndex(buf) {
  if (buf.length !== MESSAGE_SIZE) return -1
  const index = buf.readUInt32LE(0)
  return messageBody(index).equals(buf) ? index : -1
}

// openStream binds a socket on a free port and makes a stream with localId that connects to the peer. An
// error after the test has asked for a close is expected (the peer may reset it), so only the others fail.
function openStream(peerPort, remoteId, localId, isClosing) {
  const udx = new UDX()
  const socket = udx.createSocket()
  const onError = (err) => {
    if (!isClosing()) fail(err)
  }
  socket.on('error', onError)
  socket.bind(0, HOST)
  const stream = udx.createStream(localId)
  stream.on('error', onError)
  stream.connect(socket, remoteId, peerPort, HOST)
  return { socket, stream }
}

// receiveInto counts what a receiving stream gets: its bytes, their SHA-256, the span from the first byte to
// the end, and the messages. With echo, every chunk goes back to the peer. onEnd runs once the peer has ended.
function receiveInto(stream, { echo, onEnd }) {
  const state = {
    bytes: 0,
    hash: crypto.createHash('sha256'),
    t0: 0n,
    seconds: 0,
    digest: '',
    messages: 0,
    valid: new Set(),
    bad: 0,
    dup: 0,
  }
  stream.on('data', (chunk) => {
    if (state.t0 === 0n) state.t0 = process.hrtime.bigint()
    state.bytes += chunk.length
    state.hash.update(chunk)
    if (echo) stream.write(chunk)
  })
  stream.on('end', () => {
    if (state.t0 !== 0n) state.seconds = seconds(state.t0, process.hrtime.bigint())
    state.digest = state.hash.digest('hex')
    onEnd(state)
  })
  stream.on('message', (buf) => {
    state.messages++
    const index = messageIndex(buf)
    if (index < 0) state.bad++
    else if (state.valid.has(index)) state.dup++
    else state.valid.add(index)
  })
  return state
}

// sendBytes writes total bytes, one random 1 MiB block repeated, then ends the stream. It resolves with the
// SHA-256 of what it wrote once flush() reports every write acknowledged. finish alone does not mean that.
async function sendBytes(stream, total) {
  const block = crypto.randomBytes(BLOCK)
  const hash = crypto.createHash('sha256')
  for (let sent = 0; sent < total; ) {
    const n = Math.min(BLOCK, total - sent)
    const part = n === BLOCK ? block : block.subarray(0, n)
    hash.update(part)
    if (!stream.write(part)) await once(stream, 'drain')
    sent += n
  }
  const finished = once(stream, 'finish')
  stream.end()
  await finished
  if ((await stream.flush()) !== true) throw new Error('flush did not complete')
  return hash.digest('hex')
}

// serveCommands answers one command per stdin line, in order. When stdin closes, the peer is torn down.
function serveCommands(handlers, teardown) {
  const rl = readline.createInterface({ input: process.stdin })
  let chain = Promise.resolve()
  rl.on('line', (line) => {
    chain = chain.then(() => dispatch(line, handlers))
  })
  rl.on('close', () => {
    chain = chain.then(() => {
      teardown()
      process.exit(process.exitCode || 0)
    })
  })
}

async function dispatch(line, handlers) {
  let reply
  try {
    const cmd = JSON.parse(line)
    const handler = handlers[cmd.cmd]
    if (!handler) throw new Error(`unknown command ${cmd.cmd}`)
    reply = await handler(cmd)
  } catch (err) {
    reply = { ok: false, error: err.message }
    process.exitCode = 1
  }
  await out(reply)
  if (reply.closed) process.exit(process.exitCode || 0)
}

async function startReceiver(echo, peerPort, remoteId, localId) {
  let closing = false
  const { socket, stream } = openStream(peerPort, remoteId, localId, () => closing)
  const teardown = () => {
    closing = true
    stream.destroy()
    socket.close()
  }
  const state = receiveInto(stream, {
    echo,
    onEnd: (st) => {
      out({ ended: true, bytes: st.bytes, hash: st.digest, seconds: st.seconds })
      if (echo) stream.end()
    },
  })
  await out({ ready: true, port: socket.address().port })
  serveCommands(
    {
      report: async () => ({
        ok: true,
        messages: state.messages,
        valid: state.valid.size,
        bad: state.bad,
        dup: state.dup,
      }),
      close: async () => {
        teardown()
        return { ok: true, closed: true }
      },
    },
    teardown
  )
}

async function startSource(peerPort, remoteId, localId) {
  let closing = false
  const { socket, stream } = openStream(peerPort, remoteId, localId, () => closing)
  const teardown = () => {
    closing = true
    stream.destroy()
    socket.close()
  }
  await out({ ready: true, port: socket.address().port })
  serveCommands(
    {
      send: async (cmd) => {
        const total = Number(cmd.bytes)
        if (!Number.isSafeInteger(total) || total < 0) throw new Error('bytes must be a non-negative integer')
        const t0 = process.hrtime.bigint()
        const sent = await sendBytes(stream, total)
        return { ok: true, sent, bytes: total, seconds: seconds(t0, process.hrtime.bigint()) }
      },
      messages: async (cmd) => {
        const count = Number(cmd.count)
        if (!Number.isSafeInteger(count) || count < 0) throw new Error('count must be a non-negative integer')
        for (let i = 0; i < count; i++) {
          if (!(await stream.send(messageBody(i)))) throw new Error(`message ${i} was not sent`)
        }
        return { ok: true, messages: count }
      },
      close: async () => {
        teardown()
        return { ok: true, closed: true }
      },
    },
    teardown
  )
}

// runSelf moves total bytes from one udx-native stream to another in this process, both on 127.0.0.1.
async function runSelf(total) {
  const udx = new UDX()
  const sa = udx.createSocket()
  const sb = udx.createSocket()
  sa.bind(0, HOST)
  sb.bind(0, HOST)
  const a = udx.createStream(101)
  const b = udx.createStream(202)
  a.on('error', fail)
  b.on('error', fail)
  a.connect(sa, 202, sb.address().port, HOST)
  b.connect(sb, 101, sa.address().port, HOST)
  const ended = once(b, 'end')
  const state = receiveInto(b, { echo: false, onEnd: () => {} })
  const sent = await sendBytes(a, total)
  await ended
  a.destroy()
  b.destroy()
  sa.close()
  sb.close()
  await out({
    ok: true,
    bytes: state.bytes,
    sent,
    received: state.digest,
    seconds: state.seconds,
  })
  process.exit(0)
}

function integer(v, name) {
  const n = Number(v)
  if (!Number.isSafeInteger(n) || n < 0 || n > 0xffffffff) throw new Error(`${name} must be a uint32`)
  return n
}

async function main(argv) {
  const [role, ...rest] = argv
  if (role === 'self') return runSelf(integer(rest[0], 'bytes'))
  if (role !== 'echo' && role !== 'sink' && role !== 'source') {
    throw new Error('role must be echo, sink, source or self')
  }
  const peerPort = integer(rest[0], 'peerPort')
  if (peerPort < 1 || peerPort > 65535) throw new Error('peerPort must be a UDP port')
  const remoteId = integer(rest[1], 'remoteId')
  const localId = integer(rest[2], 'localId')
  if (role === 'source') return startSource(peerPort, remoteId, localId)
  return startReceiver(role === 'echo', peerPort, remoteId, localId)
}

main(process.argv.slice(2)).catch(fail)
