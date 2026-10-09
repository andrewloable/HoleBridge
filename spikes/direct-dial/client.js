'use strict'

// Spike client. Runs under bare (node_modules/.bin/bare client.js ...).
// argv: <bootstrap JSON> <right|wrong> <count> <window ms>
// Prints one JSON object per line on stdout. Never prints key material.

const DHT = require('hyperdht')
const b4a = require('b4a')
const sodium = require('sodium-universal')
const keys = require('./keys')

const args = Bare.argv.slice(2)
const bootstrap = JSON.parse(args[0])
const mode = args[1]
const count = parseInt(args[2], 10)
const windowMs = parseInt(args[3], 10)
const PAYLOAD_BYTES = 1024 * 1024
const TRANSFER_WINDOW_MS = 60000

const hostKp = keys.host()
const clientKp = mode === 'right' ? keys.client() : keys.wrongClient()

function emit(obj) {
  console.log(JSON.stringify(obj))
}

// Dial the host public key. Resolves with the socket and an ok flag. Times
// from the dial call to the secret stream's 'connect' (handshake done).
function dialOnce(dht, hostPk, kp, windowMs) {
  const start = Date.now()
  const socket = dht.connect(hostPk, { keyPair: kp })
  let errorText = null
  socket.on('error', (err) => {
    errorText = err && (err.code || err.message) ? String(err.code || err.message) : 'error'
  })
  return new Promise((resolve) => {
    let settled = false
    let timer = null
    const settle = (result) => {
      if (settled) return
      settled = true
      if (timer) clearTimeout(timer)
      resolve({ socket, ...result })
    }
    timer = setTimeout(() => {
      settle({ ok: false, ms: Date.now() - start, reason: 'no connect within ' + windowMs + ' ms' })
    }, windowMs)
    socket.once('connect', () => {
      settle({ ok: true, ms: Date.now() - start })
    })
    socket.once('close', () => {
      settle({ ok: false, ms: Date.now() - start, reason: 'closed before connect: ' + (errorText || 'no error') })
    })
  })
}

// Send 1 MB; the host echoes it back. Verifies the echoed bytes match.
function echoMegabyte(socket, payload) {
  const start = Date.now()
  return new Promise((resolve) => {
    const chunks = []
    let got = 0
    let settled = false
    const settle = (result) => {
      if (settled) return
      settled = true
      clearTimeout(timer)
      resolve(result)
    }
    const timer = setTimeout(() => {
      settle({ ok: false, ms: Date.now() - start, bytes: got, reason: 'echo timed out' })
    }, TRANSFER_WINDOW_MS)
    socket.on('data', (d) => {
      chunks.push(d)
      got += d.length
      if (got >= payload.length) {
        const echoed = b4a.concat(chunks)
        settle({ ok: b4a.equals(echoed, payload), ms: Date.now() - start, bytes: got })
      }
    })
    socket.write(payload)
  })
}

async function runRight(dht, payload) {
  for (let i = 1; i <= count; i++) {
    const dial = await dialOnce(dht, hostKp.publicKey, clientKp, windowMs)
    if (!dial.ok) {
      emit({ ev: 'connect', i, ok: false, connectMs: dial.ms, reason: dial.reason })
      continue
    }
    const xfer = await echoMegabyte(dial.socket, payload)
    emit({
      ev: 'connect',
      i,
      ok: xfer.ok,
      connectMs: dial.ms,
      echoMs: xfer.ms,
      echoBytes: xfer.bytes,
      echoOk: xfer.ok,
      reason: xfer.ok ? undefined : xfer.reason || 'echo mismatch'
    })
    dial.socket.destroy()
  }
}

async function runWrong(dht) {
  for (let i = 1; i <= count; i++) {
    const dial = await dialOnce(dht, hostKp.publicKey, clientKp, windowMs)
    emit({ ev: 'wrong', i, connected: dial.ok, ms: dial.ms, reason: dial.ok ? undefined : dial.reason })
    dial.socket.destroy()
  }
}

async function main() {
  const payload = b4a.alloc(PAYLOAD_BYTES)
  sodium.randombytes_buf(payload)

  const dht = new DHT({ bootstrap })
  const boot = Date.now()
  await dht.fullyBootstrapped()
  emit({ ev: 'bootstrap', ms: Date.now() - boot })

  if (mode === 'right') await runRight(dht, payload)
  else await runWrong(dht)

  await dht.destroy()
  emit({ ev: 'done' })
}

main().catch((err) => {
  emit({ ev: 'fatal', message: String(err && err.message ? err.message : err) })
  Bare.exit(1)
})
