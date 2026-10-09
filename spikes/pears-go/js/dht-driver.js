'use strict'
// Spike steps 1 and 2 driver (throwaway). Not product code.
// Step 1: start hyperdht/testnet with 3 nodes on 127.0.0.1 and print the bootstrap host:port.
// Control: a JS client DHT (hyperdht 6.34.1, dht-rpc 6.27.0) pings and FIND_NODEs the bootstrap
// node, and it encodes two fixed requests with dht-rpc's own encoder, for a byte comparison.
// Step 2: run the Go client (go/bin/spike dht) against the bootstrap node as a child process.
// The testnet is destroyed when the child exits. execFile's timeout bounds the child.

const { execFile } = require('child_process')
const path = require('path')
const createTestnet = require('hyperdht/testnet')
const DHT = require('hyperdht')
const b4a = require('b4a')

const GO_BIN = process.env.SPIKE_GO_BIN || path.join(__dirname, '..', 'go', 'bin', 'spike')
const CHILD_TIMEOUT_MS = 90_000
const VEC_TO = { host: '127.0.0.1', port: 4242 }

async function main() {
  const testnet = await createTestnet(3)
  const bs = testnet.bootstrap[0]
  console.log(`step1 testnet nodes=${testnet.nodes.length} bootstrap=${bs.host}:${bs.port}`)

  const client = new DHT({ ephemeral: true, bootstrap: testnet.bootstrap, host: '127.0.0.1' })
  await client.fullyBootstrapped()

  // Fixed-vector encodings, as dht-rpc writes them. Same inputs as the Go check.
  const encode = (command, tid, target) => {
    const req = client.io.createRequest(VEC_TO, null, true, command, target, null, null, 0)
    req.tid = tid
    const buf = req._encodeRequest(null, null, VEC_TO, client.io.clientSocket)
    req.destroy()
    return b4a.toString(buf, 'hex')
  }
  const jsPing = encode(0, 0x1234, null)
  const jsFind = encode(2, 0x1235, b4a.alloc(32, 7))
  console.log(`js vector ping bytes=${jsPing.length / 2} find_node bytes=${jsFind.length / 2}`)

  const ping = await client.ping(bs, { timeout: 5000 })
  console.log(`control js ping ok=${!!ping} sender id checked=${!!(ping.from && ping.from.id)}`)

  const req = client.io.createRequest(bs, null, true, 2, b4a.alloc(32, 7), null, null, 0)
  const fn = await client._requestToPromise(req, { timeout: 5000 })
  console.log(`control js findNode closerNodes=${fn.closerNodes.length} error=${fn.error} sender id checked=${!!(fn.from && fn.from.id)}`)
  await client.destroy()

  const jsPeerID = b4a.toString(require('dht-rpc/lib/peer').id(bs.host, bs.port), 'hex')
  const args = ['dht', '-bootstrap', `${bs.host}:${bs.port}`, '-js-ping', jsPing, '-js-find', jsFind, '-js-peerid', jsPeerID]
  await new Promise((resolve) => {
    execFile(GO_BIN, args, { timeout: CHILD_TIMEOUT_MS, maxBuffer: 16 * 1024 * 1024 }, (err, stdout, stderr) => {
      process.stdout.write(stdout)
      if (stderr) process.stderr.write(stderr)
      if (err) {
        console.log(`go child error: ${err.message}`)
        process.exitCode = 1
      }
      resolve()
    })
  })
  await testnet.destroy()
  console.log('testnet destroyed')
}

main().catch((err) => {
  console.error('driver failed:', err && err.message)
  process.exitCode = 1
})
