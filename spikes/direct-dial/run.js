'use strict'

// Spike driver. Runs under node: starts the hyperdht testnet and the host
// server, then runs the client under bare twice (right key, then wrong key).
// Writes raw numbers to results.txt next to this file.

const fs = require('fs')
const path = require('path')
const { spawn } = require('child_process')
const createTestnet = require('hyperdht/testnet')
const DHT = require('hyperdht')
const b4a = require('b4a')
const keys = require('./keys')

const RIGHT_CONNECTS = 20
const WRONG_ATTEMPTS = 3
const CONNECT_WINDOW_MS = 30000
const WRONG_WINDOW_MS = 20000
const CHILD_LIMIT_MS = 240000
const BARE = path.join(__dirname, 'node_modules', '.bin', 'bare')
const RESULTS = path.join(__dirname, process.env.SPIKE_RESULTS || 'results.txt')
// SPIKE_CONTROL=open-firewall admits every key. Control run only: the wrong client should then connect.
const CONTROL_OPEN = process.env.SPIKE_CONTROL === 'open-firewall'

function runClient(bootstrap, mode, count, windowMs) {
  return new Promise((resolve, reject) => {
    const started = Date.now()
    const child = spawn(BARE, [path.join(__dirname, 'client.js'), JSON.stringify(bootstrap), mode, String(count), String(windowMs)], {
      stdio: ['ignore', 'pipe', 'pipe']
    })
    const lines = []
    let stderr = ''
    let buf = ''
    let killed = false
    const killer = setTimeout(() => {
      killed = true
      child.kill('SIGKILL')
    }, CHILD_LIMIT_MS)
    child.stdout.on('data', (d) => {
      buf += d.toString()
      let idx
      while ((idx = buf.indexOf('\n')) >= 0) {
        const line = buf.slice(0, idx).trim()
        buf = buf.slice(idx + 1)
        if (line) lines.push(JSON.parse(line))
      }
    })
    child.stderr.on('data', (d) => {
      stderr += d.toString()
    })
    child.on('error', (err) => {
      clearTimeout(killer)
      reject(err)
    })
    child.on('close', (code, signal) => {
      clearTimeout(killer)
      if (buf.trim()) lines.push(JSON.parse(buf.trim()))
      resolve({ lines, code, signal, killed, stderr, wallMs: Date.now() - started })
    })
  })
}

function percentile(sorted, p) {
  const rank = Math.ceil((p / 100) * sorted.length)
  return sorted[Math.max(0, rank - 1)]
}

function summarize(values) {
  const sorted = [...values].sort((a, b) => a - b)
  const sum = sorted.reduce((a, b) => a + b, 0)
  return {
    n: sorted.length,
    min: sorted[0],
    p50: percentile(sorted, 50),
    p95: percentile(sorted, 95),
    max: sorted[sorted.length - 1],
    mean: Math.round((sum / sorted.length) * 10) / 10
  }
}

async function main() {
  const out = []
  const log = (s) => {
    out.push(s)
    console.log(s)
  }

  const hostKp = keys.host()
  const clientKp = keys.client()
  log('host public key prefix: ' + b4a.toString(hostKp.publicKey, 'hex').slice(0, 8))
  log('right client public key prefix: ' + b4a.toString(clientKp.publicKey, 'hex').slice(0, 8))
  log('wrong client public key prefix: ' + b4a.toString(keys.wrongClient().publicKey, 'hex').slice(0, 8))

  const counters = { admitted: 0, rejected: 0, connections: 0, echoed: 0 }
  let testnet = null
  let serverDht = null
  let server = null
  try {
    let t = Date.now()
    testnet = await createTestnet(10)
    const testnetMs = Date.now() - t
    log('testnet: 10 nodes on loopback, started in ' + testnetMs + ' ms')
    const bootstrap = testnet.bootstrap

    serverDht = new DHT({ bootstrap })
    await serverDht.fullyBootstrapped()

    server = serverDht.createServer({
      firewall: (remotePublicKey) => {
        const allowed = b4a.equals(remotePublicKey, clientKp.publicKey)
        if (allowed) counters.admitted++
        else counters.rejected++
        if (CONTROL_OPEN) return false
        return !allowed
      }
    })
    server.on('connection', (socket) => {
      counters.connections++
      socket.on('error', () => {})
      socket.on('data', (d) => {
        counters.echoed += d.length
        socket.write(d)
      })
    })
    t = Date.now()
    await server.listen(hostKp)
    log('host server listening on the host key pair, listen took ' + (Date.now() - t) + ' ms')

    log('--- right client, ' + RIGHT_CONNECTS + ' connects ---')
    const right = await runClient(bootstrap, 'right', RIGHT_CONNECTS, CONNECT_WINDOW_MS)
    log('right client exit code: ' + right.code + ' signal: ' + right.signal + ' killed: ' + right.killed + ' wall ms: ' + right.wallMs)
    if (right.stderr.trim()) log('right client stderr: ' + right.stderr.trim())
    const rightRaw = right.lines
    const boot = rightRaw.find((l) => l.ev === 'bootstrap')
    if (boot) log('client bootstrap ms: ' + boot.ms)
    const connects = rightRaw.filter((l) => l.ev === 'connect')
    for (const c of connects) {
      log(
        'connect ' + c.i + ': ok=' + c.ok + ' connectMs=' + c.connectMs +
          (c.echoMs !== undefined ? ' echoMs=' + c.echoMs + ' echoBytes=' + c.echoBytes + ' echoOk=' + c.echoOk : '') +
          (c.reason ? ' reason=' + c.reason : '')
      )
    }
    const okCount = connects.filter((c) => c.ok).length
    const connectTimes = connects.filter((c) => c.ok).map((c) => c.connectMs)
    const echoTimes = connects.filter((c) => c.ok).map((c) => c.echoMs)
    const fatal = rightRaw.find((l) => l.ev === 'fatal')
    if (fatal) log('right client fatal: ' + fatal.message)
    log('right client connected ' + okCount + ' of ' + RIGHT_CONNECTS)
    const cs = connectTimes.length ? summarize(connectTimes) : null
    if (cs) log('connect ms over ' + cs.n + ' ok connects: min=' + cs.min + ' p50=' + cs.p50 + ' p95=' + cs.p95 + ' max=' + cs.max + ' mean=' + cs.mean)
    const es = echoTimes.length ? summarize(echoTimes) : null
    if (es) log('1 MB echo ms: min=' + es.min + ' p50=' + es.p50 + ' p95=' + es.p95 + ' max=' + es.max + ' mean=' + es.mean)
    log('server after right run: connections=' + counters.connections + ' admitted handshakes=' + counters.admitted + ' rejected handshakes=' + counters.rejected + ' echoed bytes=' + counters.echoed)

    const before = { ...counters }
    log('--- wrong client, ' + WRONG_ATTEMPTS + ' attempts, ' + WRONG_WINDOW_MS + ' ms window each ---')
    const wrong = await runClient(bootstrap, 'wrong', WRONG_ATTEMPTS, WRONG_WINDOW_MS)
    log('wrong client exit code: ' + wrong.code + ' signal: ' + wrong.signal + ' killed: ' + wrong.killed + ' wall ms: ' + wrong.wallMs)
    if (wrong.stderr.trim()) log('wrong client stderr: ' + wrong.stderr.trim())
    const wrongLines = wrong.lines.filter((l) => l.ev === 'wrong')
    for (const w of wrongLines) {
      log('wrong attempt ' + w.i + ': connected=' + w.connected + ' ms=' + w.ms + ' reason=' + w.reason)
    }
    const wrongConnected = wrongLines.filter((w) => w.connected).length
    log('wrong client connected ' + wrongConnected + ' of ' + WRONG_ATTEMPTS)
    log(
      'server during wrong run: connections=' + (counters.connections - before.connections) +
        ' admitted=' + (counters.admitted - before.admitted) +
        ' rejected=' + (counters.rejected - before.rejected)
    )

    if (CONTROL_OPEN) log('CONTROL RUN: firewall admitted all keys; wrong client connected ' + wrongConnected + ' of ' + WRONG_ATTEMPTS + ' (expected all, so the firewall is the only gate)')
    const pass = okCount === RIGHT_CONNECTS && wrongConnected === 0 && counters.connections === RIGHT_CONNECTS && right.code === 0 && wrong.code === 0
    log('PASS RULE: ' + (pass ? 'PASS' : 'FAIL') + ' (right ' + okCount + '/' + RIGHT_CONNECTS + ', wrong connected ' + wrongConnected + '/' + WRONG_ATTEMPTS + ')')
    log('SUMMARY_JSON ' + JSON.stringify({
      rightOk: okCount,
      rightTotal: RIGHT_CONNECTS,
      wrongConnected,
      wrongTotal: WRONG_ATTEMPTS,
      connectSummary: cs,
      echoSummary: es,
      pass
    }))
  } finally {
    if (server) await server.close()
    if (serverDht) await serverDht.destroy()
    if (testnet) await testnet.destroy()
    log('teardown done')
    fs.writeFileSync(RESULTS, out.join('\n') + '\n')
  }
}

main().catch((err) => {
  console.error('driver error: ' + (err && err.message ? err.message : err))
  process.exitCode = 1
})
