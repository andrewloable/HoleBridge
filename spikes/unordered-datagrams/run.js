'use strict'

// Spike driver (Node). Starts a hyperdht testnet on 127.0.0.1 and a host server
// on 127.0.0.1. Route "direct": the host is firewalled, so the client reaches it
// by hole punch. Route "relay": the host also refuses to hole punch, and a
// blind-relay node on 127.0.0.1 carries the connection. The client runs under the
// repo-local bare (client.js). Raw JSON lines go to results-<route>.txt next to
// this file. Nothing leaves the loopback interface.

const fs = require('fs')
const path = require('path')
const { spawn } = require('child_process')
const createTestnet = require('hyperdht/testnet')
const DHT = require('hyperdht')
const relay = require('blind-relay')
const b4a = require('b4a')
const keys = require('./keys')
const { seqOf } = require('./payload')

const route = process.argv[2]
if (route !== 'direct' && route !== 'relay') {
  console.error('usage: node run.js direct|relay')
  process.exit(2)
}

const LOOPBACK = '127.0.0.1'
const BARE = path.join(__dirname, 'node_modules', '.bin', 'bare')
const RUN = process.argv[3] ? '-' + process.argv[3] : ''
const RESULTS = path.join(__dirname, 'results-' + route + RUN + '.txt')
const CHILD_LIMIT_MS = 480000
// Sweep sizes in bytes of plaintext. 1200 is the architecture default maxDatagram.
const SWEEP = [16, 64, 256, 512, 1000, 1100, 1156, 1157, 1176, 1200, 1232, 1300, 1400, 1472, 1500, 1800, 2000, 2040, 2048, 2100, 2500, 4000, 8192, 16000, 60000]

const lines = []
function log(s) {
  lines.push(s)
  console.log(s)
}

function bump(map, size, intact) {
  const e = map.get(size) || { intact: 0, bad: 0 }
  if (intact) e.intact++
  else e.bad++
  map.set(size, e)
}

async function main() {
  log('spike: unordered-datagrams, route ' + route)
  log('loopback only: testnet, host, relay and client all bind ' + LOOPBACK)

  const testnet = await createTestnet(10, { host: LOOPBACK })
  const bootstrap = testnet.bootstrap
  log('testnet: 10 nodes on ' + LOOPBACK)

  let hostDht = null
  let server = null
  let relayDht = null
  let relayNode = null
  let relayServer = null
  let child = null
  const upArrivals = new Map() // payload size -> { intact, bad } (host received)
  const upWire = new Map() // payload size -> bytes of the client->host UDX packet (receive side)
  const relayStats = {}

  try {
    hostDht = new DHT({ bootstrap, host: LOOPBACK, firewalled: true })
    await hostDht.fullyBootstrapped()
    const hostKp = keys.host()
    server = hostDht.createServer({
      shareLocalAddress: false,
      holepunch: route === 'relay' ? false : undefined
    })
    server.on('connection', (s) => {
      s.on('error', () => {})
      s.on('data', () => {})
      const raw = s.rawStream
      let rb = raw.bytesReceived
      s.on('message', (m) => {
        bump(upArrivals, m.length, seqOf(m) >= 0)
        // Wire size of the client->host packet, from the receive side.
        const now = raw.bytesReceived
        upWire.set(m.length, now - rb)
        rb = now
        s.trySend(m) // echo back over the unordered path
      })
    })
    await server.listen(hostKp)
    log('host: firewalled DHT node, server listening, holepunch ' + (route === 'relay' ? 'refused (relay only)' : 'allowed (hole punch)'))

    let relayHex = '-'
    if (route === 'relay') {
      relayDht = new DHT({ bootstrap, host: LOOPBACK })
      await relayDht.fullyBootstrapped()
      const relayKp = keys.relay()
      relayServer = new relay.Server({ createStream: (opts) => relayDht.createRawStream(opts) })
      relayNode = relayDht.createServer()
      relayNode.on('connection', (socket) => {
        socket.on('error', () => {})
        // The session emits 'error' when the client tears the relay link down; that is normal end of run.
        const session = relayServer.accept(socket, { id: socket.remotePublicKey })
        session.on('error', () => {})
      })
      await relayNode.listen(relayKp)
      relayHex = b4a.toString(relayKp.publicKey, 'hex')
      log('relay: blind-relay server on a DHT node, listening on ' + LOOPBACK)
    }

    const sweepArg = JSON.stringify(SWEEP)
    child = spawn(BARE, [path.join(__dirname, 'client.js'), JSON.stringify(bootstrap), route, relayHex, sweepArg], {
      cwd: __dirname,
      stdio: ['ignore', 'pipe', 'pipe']
    })
    let stderr = ''
    let buf = ''
    let readyStats = null
    let doneStats = null
    let measuredStats = null
    const childDone = new Promise((resolve) => {
      child.on('close', (code, signal) => resolve({ code, signal }))
    })
    child.stderr.on('data', (d) => {
      stderr += d.toString()
    })
    child.stdout.on('data', (d) => {
      buf += d.toString()
      let idx
      while ((idx = buf.indexOf('\n')) >= 0) {
        const text = buf.slice(0, idx).trim()
        buf = buf.slice(idx + 1)
        if (!text) continue
        log('client ' + text)
        let obj = null
        try {
          obj = JSON.parse(text)
        } catch (e) {
          continue
        }
        if (obj.ev === 'ready' && relayServer) readyStats = JSON.parse(JSON.stringify(relayServer.stats))
        if (obj.ev === 'measured' && relayServer) measuredStats = JSON.parse(JSON.stringify(relayServer.stats))
        if (obj.ev === 'done' && relayServer) doneStats = JSON.parse(JSON.stringify(relayServer.stats))
      }
    })
    const timer = setTimeout(() => {
      log('child time limit reached, killing client')
      child.kill('SIGKILL')
    }, CHILD_LIMIT_MS)
    const exit = await childDone
    clearTimeout(timer)
    log('client exit code ' + exit.code + ' signal ' + exit.signal)
    if (stderr.trim()) log('client stderr: ' + stderr.trim())

    if (relayServer) {
      relayStats.atReady = readyStats
      relayStats.atDone = doneStats
      log('relay stats after the measurements, before teardown: ' + JSON.stringify(measuredStats))
      log('relay stats at ready: ' + JSON.stringify(readyStats))
      log('relay stats at done: ' + JSON.stringify(doneStats))
    }
  } finally {
    if (child && child.exitCode === null && !child.killed) child.kill('SIGKILL')
    if (server) await server.close().catch(() => {})
    if (relayNode) await relayNode.close().catch(() => {})
    if (relayDht) await relayDht.destroy().catch(() => {})
    if (hostDht) await hostDht.destroy().catch(() => {})
    await testnet.destroy().catch(() => {})
  }

  log('host side, received per payload size (intact / bad): ' + JSON.stringify([...upArrivals.entries()]))
  log('host side, client->host UDX packet bytes per payload size: ' + JSON.stringify([...upWire.entries()]))
  fs.writeFileSync(RESULTS, lines.join('\n') + '\n')
  log('wrote ' + path.basename(RESULTS))
}

main().catch((err) => {
  console.error('FATAL', err && err.stack ? err.stack : err)
  process.exit(1)
})
