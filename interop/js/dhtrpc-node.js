// A dht-rpc 6.27.0 node for the Go interop test in interop/dhtrpc_test.go. The test runs it as a child
// process and talks to it over stdio. Nothing here is part of HoleBridge's runtime.
//
// Usage: node dhtrpc-node.js [--port N] [host:port ...]
//
// With host:port arguments the node joins the network those nodes are in, on a random UDP port. With
// none, it is the bootstrap node of a new network, and --port must name its UDP port: upstream's
// bootstrapper needs a fixed port.
//
// Stdio protocol. The first line on stdout is the node's address and id, once it is bootstrapped and
// persistent:
//   {"ready":true,"host":"127.0.0.1","port":N,"id":"<64 hex>"}
// Then the test writes one JSON command per line on stdin, and the node writes one reply line per
// command on stdout:
//   {"cmd":"ping","host":"127.0.0.1","port":N}  ->  {"ok":true,"id":"<hex>"}
//   {"cmd":"find","target":"<64 hex>"}           ->  {"ok":true,"replies":[{"host","port","id"}]}
// A failed command replies {"ok":false,"error":"..."}. Anything else goes to stderr. The node exits when
// stdin closes, so it never outlives the test, or on SIGTERM or SIGINT.

'use strict'

const readline = require('node:readline')
const DHT = require('dht-rpc')

const HOST = '127.0.0.1'
const ID_BYTES = 32

function parseArgs(argv) {
  const boot = []
  let port = 0
  for (let i = 0; i < argv.length; i++) {
    if (argv[i] === '--port') {
      port = Number(argv[++i])
      if (!Number.isInteger(port) || port < 0 || port > 65535) {
        throw new Error('--port needs a UDP port number')
      }
      continue
    }
    const [host, p] = argv[i].split(':')
    const num = Number(p)
    if (!host || !Number.isInteger(num) || num <= 0 || num > 65535) {
      throw new Error(`bootstrap node must be host:port, got ${argv[i]}`)
    }
    boot.push({ host, port: num })
  }
  return { boot, port }
}

function start({ boot, port }) {
  if (boot.length === 0) {
    if (port === 0) throw new Error('a bootstrap node needs --port')
    return DHT.bootstrapper(port, HOST, { host: HOST })
  }
  return new DHT({ host: HOST, port, firewalled: false, ephemeral: false, bootstrap: boot })
}

const hex = (buf) => (buf ? Buffer.from(buf).toString('hex') : null)

let dht = null
let closing = false

function shutdown(code) {
  if (closing) return
  closing = true
  // A hard stop, in case destroy hangs.
  setTimeout(() => process.exit(code), 2000)
  const done = dht ? dht.destroy() : Promise.resolve()
  done.catch(() => {}).finally(() => process.exit(code))
}

async function ping(cmd) {
  const reply = await dht.ping({ host: cmd.host, port: Number(cmd.port) })
  return { ok: true, id: hex(reply.from && reply.from.id) }
}

async function find(cmd) {
  const target = Buffer.from(cmd.target, 'hex')
  if (target.length !== ID_BYTES) throw new Error('target must be 32 bytes of hex')
  const replies = []
  for await (const r of dht.findNode(target)) {
    replies.push({ host: r.from.host, port: r.from.port, id: hex(r.from.id) })
  }
  return { ok: true, replies }
}

async function handle(line) {
  let reply
  try {
    const cmd = JSON.parse(line)
    if (cmd.cmd === 'ping') reply = await ping(cmd)
    else if (cmd.cmd === 'find') reply = await find(cmd)
    else throw new Error(`unknown command ${cmd.cmd}`)
  } catch (err) {
    reply = { ok: false, error: err.message }
  }
  process.stdout.write(JSON.stringify(reply) + '\n')
}

async function main() {
  const opts = parseArgs(process.argv.slice(2))
  dht = start(opts)
  await dht.fullyBootstrapped()
  if (dht.id === null) {
    throw new Error('node is still ephemeral after bootstrap, so it has no id')
  }
  const { port } = dht.address()
  process.stdout.write(
    JSON.stringify({ ready: true, host: HOST, port, id: hex(dht.id) }) + '\n'
  )

  let chain = Promise.resolve()
  const rl = readline.createInterface({ input: process.stdin })
  rl.on('line', (line) => {
    chain = chain.then(() => handle(line))
  })
  rl.on('close', () => shutdown(0))
  process.on('SIGTERM', () => shutdown(0))
  process.on('SIGINT', () => shutdown(0))
}

main().catch((err) => {
  process.stderr.write(`dhtrpc-node: ${err.message}\n`)
  process.exit(1)
})
