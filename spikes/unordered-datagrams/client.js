'use strict'

// Spike client. Runs under the repo-local bare (node_modules/.bin/bare client.js ...),
// the runtime the app engine uses. It dials the host over one route, then measures
// the unordered-datagram path in both directions. Prints one JSON object per line.
// argv: <bootstrap JSON> <direct|relay> <relay public key hex or -> <sweep sizes JSON>
// Bound to 127.0.0.1 only.

const DHT = require('hyperdht')
const b4a = require('b4a')
const { make, seqOf } = require('./payload')
const keys = require('./keys')

const args = Bare.argv.slice(2)
const bootstrap = JSON.parse(args[0])
const route = args[1]
const relayHex = args[2]
const SWEEP = JSON.parse(args[3])

const TRIALS = 4
const ECHO_WAIT_MS = 1500
const WARMUP_LIMIT_MS = 15000

function emit(obj) {
  console.log(JSON.stringify(obj))
}

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms))
}

const pending = new Map() // seq -> { orig, start, resolve }
let seqNext = 1

function onEcho(m) {
  const seq = seqOf(m)
  const entry = pending.get(seq)
  if (!entry) return
  pending.delete(seq)
  entry.resolve({ intact: b4a.equals(m, entry.orig), ms: Date.now() - entry.start })
}

// Sends one message by send (awaited) or trySend, then waits for the host's echo.
// Wire size is read from the receive side: a sent unordered message does not move
// stream.bytesTransmitted, but the echo that comes back does move bytesReceived.
async function trial(sock, size, method) {
  const seq = seqNext++
  const orig = make(seq, size)
  const raw = sock.rawStream
  const rb0 = raw.bytesReceived
  const rp0 = raw.packetsReceived

  const echo = new Promise((resolve) => {
    pending.set(seq, { orig, start: Date.now(), resolve })
    setTimeout(() => {
      if (pending.delete(seq)) resolve(null)
    }, ECHO_WAIT_MS)
  })

  let sendResult = 'void'
  if (method === 'send') sendResult = (await sock.send(orig)) === true
  else sock.trySend(orig)
  const got = await echo

  return {
    ev: 'trial',
    route,
    size,
    method,
    seq,
    sendResult,
    echoed: got !== null,
    echoIntact: got !== null && got.intact,
    echoMs: got !== null ? got.ms : null,
    echoWireBytes: raw.bytesReceived - rb0,
    echoWirePackets: raw.packetsReceived - rp0
  }
}

// A message sent right after the client's 'connect' can be dropped while the host
// side is still finishing its handshake, so resend a 16-byte probe until one echo
// comes back. The elapsed time is itself a measurement.
async function warmup(sock) {
  const start = Date.now()
  let sent = 0
  while (Date.now() - start < WARMUP_LIMIT_MS) {
    const seq = seqNext++
    const orig = make(seq, 16)
    const got = new Promise((resolve) => pending.set(seq, { orig, start: Date.now(), resolve }))
    sock.trySend(orig)
    sent++
    const res = await Promise.race([got, sleep(100).then(() => null)])
    pending.delete(seq)
    if (res) return { warmupMs: Date.now() - start, warmupSent: sent, warmupIntact: res.intact }
  }
  return { warmupMs: null, warmupSent: sent, warmupIntact: false }
}

async function connectOnce(dht) {
  const hostPk = keys.host().publicKey
  const opts = { localConnection: false }
  if (route === 'relay') opts.relayThrough = b4a.from(relayHex, 'hex')
  const start = Date.now()
  const sock = dht.connect(hostPk, opts)
  sock.on('error', (err) => emit({ ev: 'socket-error', route, message: String(err && err.code ? err.code : err && err.message) }))
  sock.on('data', () => {})
  sock.on('message', onEcho)
  await new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error('no connect within 60 s')), 60000)
    sock.once('connect', () => {
      clearTimeout(timer)
      resolve()
    })
  })
  return { sock, connectMs: Date.now() - start }
}

async function main() {
  const dht = new DHT({ bootstrap, host: '127.0.0.1' })
  const boot = Date.now()
  await dht.fullyBootstrapped()
  emit({ ev: 'bootstrap', route, ms: Date.now() - boot })

  const { sock, connectMs } = await connectOnce(dht)
  const raw = sock.rawStream
  emit({
    ev: 'connected',
    route,
    connectMs,
    mtu: raw.mtu,
    secretStreamSend: typeof sock.send,
    secretStreamTrySend: typeof sock.trySend,
    udxSend: typeof raw.send,
    udxTrySend: typeof raw.trySend
  })

  const warm = await warmup(sock)
  emit({ ev: 'ready', route, ...warm })

  // Sweep: TRIALS messages per size, alternating send and trySend.
  const sizeResults = {}
  for (const size of SWEEP) {
    let ok = 0
    for (let i = 0; i < TRIALS; i++) {
      const r = await trial(sock, size, i % 2 === 0 ? 'send' : 'trySend')
      emit(r)
      if (r.echoIntact) ok++
    }
    sizeResults[size] = ok
    emit({ ev: 'size', route, size, intactEchoes: ok, of: TRIALS })
  }

  // Refine the arrival limit between the largest size that always arrives and the
  // first one above it that does not. Each probe is 2 trials: one send, one trySend.
  const passSizes = SWEEP.filter((s) => sizeResults[s] === TRIALS)
  const lowest = passSizes.length ? Math.max(...passSizes) : 0
  const failSizes = SWEEP.filter((s) => sizeResults[s] < TRIALS && s > lowest)
  let lo = lowest
  let hi = failSizes.length ? Math.min(...failSizes) : lo
  while (hi - lo > 1) {
    const mid = Math.floor((lo + hi) / 2)
    const a = await trial(sock, mid, 'send')
    const b = await trial(sock, mid, 'trySend')
    emit(a)
    emit(b)
    if (a.echoIntact && b.echoIntact) lo = mid
    else hi = mid
  }
  emit({ ev: 'arrival-limit', route, largestArriving: lo, firstFailing: hi })

  // maxDatagram: one probe gives the per-packet overhead (UDX header plus the
  // secret-stream envelope). The largest payload whose UDX packet is at most
  // stream.mtu is mtu minus that overhead. One byte more is probed as well.
  const mtu = raw.mtu
  const probe = await trial(sock, 1000, 'send')
  emit(probe)
  const overhead = probe.echoWireBytes - 1000
  const fit = mtu - overhead
  const fitA = await trial(sock, fit, 'send')
  const fitB = await trial(sock, fit + 1, 'send')
  emit(fitA)
  emit(fitB)

  emit({
    ev: 'summary',
    route,
    connectMs,
    warmupMs: warm.warmupMs,
    mtu,
    overheadBytes: overhead,
    overheadPackets: probe.echoWirePackets,
    maxDatagram: fit,
    maxDatagramWireBytes: fitA.echoWireBytes,
    maxDatagramArrives: fitA.echoIntact,
    plusOneWireBytes: fitB.echoWireBytes,
    plusOneArrives: fitB.echoIntact,
    arrivalLimit: lo,
    firstFailing: hi
  })

  emit({ ev: 'measured', route })
  await sock.destroy()
  await dht.destroy()
  emit({ ev: 'done', route })
}

main().catch((err) => {
  emit({ ev: 'fatal', route, message: String(err && err.stack ? err.stack : err) })
  Bare.exit(1)
})
