'use strict'
// Spike step 4 peer (throwaway). Runs the pinned udx-native 1.21.3 (the version the app engine
// pins) on localhost, against the Go port, against another JS process (udx-pair.js), or against
// itself.
//   node udx-peer.js recv <bindPort> <peerPort> <localId> <remoteId> <bytes>
//   node udx-peer.js send <bindPort> <peerPort> <localId> <remoteId> <bytes>
//     bindPort 0 picks a free port, which READY reports. send waits for a GO line on stdin.
//   node udx-peer.js self <bytes>                          JS to JS, both ends in this process
// Every mode prints key=value lines. The digest is BLAKE2b-256 (unkeyed), the same as Go's.
// The payload is one random 1 MiB block repeated, so the expected digest is known before timing.

// Node has a global process. Bare has none, so it uses the bare-process builtin.
const process = globalThis.process || require('bare-process')
const sodium = require('sodium-universal')
const b4a = require('b4a')
const UDX = require('udx-native')

// Promise form of an event, with the same role as events.once. Written out so the file also
// runs under bare, which has no 'events' builtin.
function once(emitter, name) {
  return new Promise((resolve, reject) => {
    const onError = (err) => {
      emitter.removeListener(name, onEvent)
      reject(err)
    }
    const onEvent = (...args) => {
      if (name !== 'error') emitter.removeListener('error', onError)
      resolve(args)
    }
    emitter.once(name, onEvent)
    if (name !== 'error') emitter.once('error', onError)
  })
}

const BLOCK = 1 << 20

function makeBlock() {
  const block = b4a.allocUnsafe(BLOCK)
  sodium.randombytes_buf(block)
  return block
}

function digestOf(chunks) {
  const state = b4a.alloc(sodium.crypto_generichash_STATEBYTES)
  sodium.crypto_generichash_init(state, null, 32)
  for (const c of chunks) sodium.crypto_generichash_update(state, c)
  const out = b4a.alloc(32)
  sodium.crypto_generichash_final(state, out)
  return b4a.toString(out, 'hex')
}

// Expected digest of the whole stream, computed before any timing starts.
function expectedDigest(block, total) {
  const state = b4a.alloc(sodium.crypto_generichash_STATEBYTES)
  sodium.crypto_generichash_init(state, null, 32)
  for (let sent = 0; sent < total; sent += BLOCK) sodium.crypto_generichash_update(state, block)
  const out = b4a.alloc(32)
  sodium.crypto_generichash_final(state, out)
  return b4a.toString(out, 'hex')
}

function out(line) {
  return new Promise((resolve) => process.stdout.write(line + '\n', resolve))
}

// Writes the whole block stream, then ends. Resolves once the stream has finished. The finish
// listener is attached before end(), because finish can fire on a microtask right after it.
async function sendAll(stream, block, total) {
  for (let sent = 0; sent < total; sent += BLOCK) {
    if (!stream.write(block)) await once(stream, 'drain')
  }
  const finished = once(stream, 'finish')
  stream.end()
  await finished
  // finish fires once the END is queued, not once the peer has it. flush() waits for every
  // write to be acknowledged, so the caller can destroy the stream safely afterwards.
  if ((await stream.flush()) !== true) throw new Error('flush did not complete')
}

// Hashes what arrives and times first byte to end of stream.
function receiveAll(stream) {
  const state = b4a.alloc(sodium.crypto_generichash_STATEBYTES)
  sodium.crypto_generichash_init(state, null, 32)
  const st = { bytes: 0, t0: 0n, t1: 0n }
  stream.on('data', (d) => {
    if (st.t0 === 0n) st.t0 = process.hrtime.bigint()
    st.bytes += d.byteLength
    sodium.crypto_generichash_update(state, d)
  })
  const done = once(stream, 'end').then(() => {
    st.t1 = process.hrtime.bigint()
    const h = b4a.alloc(32)
    sodium.crypto_generichash_final(state, h)
    st.digest = b4a.toString(h, 'hex')
    return st
  })
  return done
}

function ms(ns) {
  return Number(ns) / 1e6
}

function bindSocket(udx, port) {
  const socket = udx.createSocket()
  // A peer that closes after finishing can reset the other side. That is not a failure here.
  socket.bind(port, '127.0.0.1')
  return socket
}

// Exit after stdout drains and a short grace period, so the last acks leave the socket.
function finish(stream, socket) {
  setTimeout(() => {
    stream.destroy()
    socket.close()
    process.exit(0)
  }, 300)
}

async function runRecv(bindPort, peerPort, localId, remoteId, total) {
  const udx = new UDX()
  const socket = bindSocket(udx, bindPort)
  const stream = udx.createStream(localId)
  stream.on('error', () => {})
  stream.connect(socket, remoteId, peerPort, '127.0.0.1')
  const got = receiveAll(stream)
  await out(`READY port=${socket.address().port}`)
  const st = await got
  await out(`RECV bytes=${st.bytes} digest=${st.digest} span_ms=${ms(st.t1 - st.t0).toFixed(1)}`)
  finish(stream, socket)
}

async function runSend(bindPort, peerPort, localId, remoteId, total) {
  const udx = new UDX()
  const socket = bindSocket(udx, bindPort)
  const stream = udx.createStream(localId)
  stream.on('error', () => {})
  stream.connect(socket, remoteId, peerPort, '127.0.0.1')
  const block = makeBlock()
  const want = expectedDigest(block, total)
  await out(`READY port=${socket.address().port}`)
  // Wait for the Go side to say it is receiving.
  await new Promise((resolve) => {
    process.stdin.once('data', () => resolve())
  })
  const t0 = process.hrtime.bigint()
  await sendAll(stream, block, total)
  const t1 = process.hrtime.bigint()
  await out(`SENT bytes=${total} digest=${want} span_ms=${ms(t1 - t0).toFixed(1)}`)
  finish(stream, socket)
}

async function runSelf(total) {
  const udx = new UDX()
  const sa = udx.createSocket()
  const sb = udx.createSocket()
  sa.bind(0, '127.0.0.1')
  sb.bind(0, '127.0.0.1')
  const a = udx.createStream(101)
  const b = udx.createStream(202)
  a.connect(sa, 202, sb.address().port, '127.0.0.1')
  b.connect(sb, 101, sa.address().port, '127.0.0.1')
  const block = makeBlock()
  const want = expectedDigest(block, total)
  const got = receiveAll(b)
  const t0 = process.hrtime.bigint()
  await sendAll(a, block, total)
  const st = await got
  const t1 = process.hrtime.bigint()
  await out(`SELF bytes=${st.bytes} digest=${st.digest} want=${want} match=${st.digest === want} span_ms=${ms(st.t1 - st.t0).toFixed(1)} wall_ms=${ms(t1 - t0).toFixed(1)}`)
  a.destroy()
  b.destroy()
  sa.close()
  sb.close()
  process.exit(0)
}

async function main() {
  const [mode, ...rest] = process.argv.slice(2)
  if (mode === 'self') return runSelf(Number(rest[0]))
  const [bindPort, peerPort, localId, remoteId, total] = rest.map(Number)
  if (mode === 'recv') return runRecv(bindPort, peerPort, localId, remoteId, total)
  if (mode === 'send') return runSend(bindPort, peerPort, localId, remoteId, total)
  throw new Error(`unknown mode ${mode}`)
}

main().catch((err) => {
  console.error('udx peer failed:', err && err.stack)
  process.exitCode = 1
})
