// A HyperDHT 6.34.1 node for the Go interop tests in interop/hyperdht_test.go. The test runs it as a child
// process and talks to it over stdio. Nothing here is part of HoleBridge's runtime.
//
// Usage:
//   node hyperdht-peer.js testnet <size>
//   node hyperdht-peer.js server --bootstrap HOST:PORT[,HOST:PORT...] --seed HEX32 --size BYTES [--relay HEX32]
//   node hyperdht-peer.js client --bootstrap HOST:PORT[,HOST:PORT...] --seed HEX32 [--relay HEX32]
//
// testnet starts <size> HyperDHT nodes on 127.0.0.1, as hyperdht's own testnet does, and reports the bootstrap
// nodes they share. server and client join the network their --bootstrap nodes are in. --seed is the 32-byte
// seed of the node's key pair, the same Ed25519 key the Go side derives from that seed. --relay is the public key
// of a blind relay: the server and the client connect through it (relayThrough). The server's key is announced
// on the network when it listens.
//
// Connections carry a transfer, in both directions at once: each side sends --size (or the command's size)
// random bytes, prefixed by an 8-byte big-endian length and the 32-byte SHA-256 of the bytes. The receiver hashes
// what arrives and checks the digest. Each side closes its write side when its transfer is done and closes the
// connection once the peer has closed its write side too, so no byte is lost at the end.
//
// Stdio protocol, one JSON object per line.
//   testnet, once its nodes are up:  {"ready":true,"role":"testnet","bootstrap":["HOST:PORT", ...]}
//   server, once it is announced:    {"ready":true,"role":"server","publicKey":"<64 hex>"}
//   client, once bootstrapped:       {"ready":true,"role":"client","publicKey":"<64 hex>"}
//   server, per accepted connection: {"event":"transfer","remotePublicKey":"<64 hex>","ok":true|false,"result":{...}}
//       ok is false when the transfer failed, and then result is replaced by "error":"...".
//   client commands, one per line on stdin, one reply line each:
//     {"cmd":"lookup","key":"<64 hex>"}              -> {"ok":true,"found":true|false}
//         found is true when the DHT holds an announce of the key (the lookup walks towards hash(key)).
//     {"cmd":"connect","server":"<64 hex>","size":N} -> {"ok":true,"result":{...}} or {"ok":false,"error":"..."}
//   result: {"remotePublicKey":"<64 hex>","sent":N,"sentSha256":"<hex>","received":N,"receivedSha256":"<hex>",
//            "verified":true|false}
// A failed command replies {"ok":false,"error":"..."}. Anything else goes to stderr. Closing stdin stops the node,
// and so does SIGTERM or SIGINT. The process never logs payload bytes or seeds.

'use strict'

const crypto = require('node:crypto')
const readline = require('node:readline')
const sodium = require('sodium-universal')
const DHT = require('hyperdht')
const createTestnet = require('hyperdht/testnet')

const HOST = '127.0.0.1'
const KEY_BYTES = 32
const HEADER_BYTES = 40 // 8-byte big-endian payload length, then the 32-byte SHA-256 of the payload
const CHUNK_BYTES = 64 * 1024
const MAX_BYTES = 64 * 1024 * 1024 // a declared length above this is refused, so a bad peer cannot stall us
const CLOSE_WAIT_MS = 5000 // how long a side waits for the peer to close its write side
const DIAL_TIMEOUT_MS = 60000 // a connect that does not open in time is an error
const LOOKUP_TIMEOUT_MS = 30000

function parseArgs(argv) {
  const role = argv[0]
  if (role === 'testnet') {
    const size = Number(argv[1])
    if (!Number.isInteger(size) || size < 1) throw new Error('testnet needs a node count')
    return { role, size }
  }
  if (role !== 'server' && role !== 'client') throw new Error(`unknown role ${role}`)
  const opts = { role, bootstrap: [], seed: null, relay: null, size: 0 }
  for (let i = 1; i < argv.length; i++) {
    const flag = argv[i]
    const value = argv[++i]
    if (value === undefined) throw new Error(`${flag} needs a value`)
    if (flag === '--bootstrap') {
      for (const part of value.split(',')) {
        const [host, p] = part.split(':')
        const port = Number(p)
        if (!host || !Number.isInteger(port) || port <= 0 || port > 65535) {
          throw new Error(`bootstrap node must be host:port, got ${part}`)
        }
        opts.bootstrap.push({ host, port })
      }
    } else if (flag === '--seed') {
      opts.seed = keyBytes(value, '--seed')
    } else if (flag === '--relay') {
      opts.relay = keyBytes(value, '--relay')
    } else if (flag === '--size') {
      opts.size = Number(value)
      if (!Number.isInteger(opts.size) || opts.size < 0 || opts.size > MAX_BYTES) {
        throw new Error('--size must be a byte count')
      }
    } else {
      throw new Error(`unknown flag ${flag}`)
    }
  }
  if (opts.bootstrap.length === 0) throw new Error(`${role} needs --bootstrap`)
  if (!opts.seed) throw new Error(`${role} needs --seed`)
  return opts
}

function keyBytes(value, flag) {
  if (!/^[0-9a-f]{64}$/.test(value)) throw new Error(`${flag} must be 64 hex characters`)
  return Buffer.from(value, 'hex')
}

const hex = (buf) => (buf ? Buffer.from(buf).toString('hex') : null)
const sha256 = (buf) => crypto.createHash('sha256').update(buf).digest()

function emit(obj) {
  process.stdout.write(JSON.stringify(obj) + '\n')
}

// newNode starts a HyperDHT node that joins the network of bootstrap. Like the testnet's nodes, it is not
// ephemeral and not firewalled, so it can store records.
function newNode(bootstrap, seed) {
  return new DHT({ bootstrap, host: HOST, ephemeral: false, firewalled: false, seed })
}

// waitFor resolves when the event fires on the emitter, and rejects when the emitter errors or when the
// timeout passes first.
function waitFor(emitter, event, ms, what) {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      cleanup()
      reject(new Error(`no ${what} within ${ms} ms`))
    }, ms)
    const onEvent = (...args) => {
      cleanup()
      resolve(args)
    }
    const onError = (err) => {
      cleanup()
      reject(err)
    }
    function cleanup() {
      clearTimeout(timer)
      emitter.off(event, onEvent)
      emitter.off('error', onError)
    }
    emitter.once(event, onEvent)
    emitter.on('error', onError)
  })
}

// writeAll writes buf in chunks, waiting for 'drain' when the stream asks for it.
function writeAll(socket, buf) {
  return new Promise((resolve, reject) => {
    let off = 0
    const step = () => {
      try {
        while (off < buf.length) {
          const end = Math.min(off + CHUNK_BYTES, buf.length)
          const chunk = buf.subarray(off, end)
          off = end
          if (!socket.write(chunk)) {
            socket.once('drain', step)
            return
          }
        }
        resolve()
      } catch (err) {
        reject(err)
      }
    }
    step()
  })
}

// receive reads one transfer from the socket: the header, then the payload, hashed as it arrives. It resolves
// with the payload's length, its SHA-256 and whether that matches the digest in the header.
function receive(socket) {
  return new Promise((resolve, reject) => {
    let head = Buffer.alloc(0)
    let declared = -1
    let want = null
    let hash = null
    let got = 0
    const onData = (chunk) => {
      let buf = chunk
      if (declared < 0) {
        head = Buffer.concat([head, buf])
        if (head.length < HEADER_BYTES) return
        declared = Number(head.readBigUInt64BE(0))
        want = Buffer.from(head.subarray(8, HEADER_BYTES))
        buf = head.subarray(HEADER_BYTES)
        head = null
        if (declared > MAX_BYTES) {
          finishWith(new Error(`peer declared ${declared} bytes, more than ${MAX_BYTES}`))
          return
        }
        hash = crypto.createHash('sha256')
      }
      const take = Math.min(buf.length, declared - got)
      if (take > 0) {
        hash.update(buf.subarray(0, take))
        got += take
      }
      if (got === declared) {
        const digest = hash.digest()
        finishWith(null, { received: got, receivedSha256: digest, verified: digest.equals(want) })
      }
    }
    const onEnd = () => finishWith(new Error('connection ended before the transfer was complete'))
    const onError = (err) => finishWith(err)
    function finishWith(err, result) {
      socket.off('data', onData)
      socket.off('end', onEnd)
      socket.off('error', onError)
      if (err) reject(err)
      else resolve(result)
    }
    socket.on('data', onData)
    socket.once('end', onEnd)
    socket.on('error', onError)
  })
}

// transfer sends size random bytes and receives the peer's in the same connection, both at once. It resolves
// with the result once both are done.
async function transfer(socket, size) {
  const payload = crypto.randomBytes(size)
  const sentSha256 = sha256(payload)
  const header = Buffer.alloc(HEADER_BYTES)
  header.writeBigUInt64BE(BigInt(size), 0)
  sentSha256.copy(header, 8)

  const received = receive(socket)
  const sent = (async () => {
    await writeAll(socket, header)
    await writeAll(socket, payload)
  })()
  const [got] = await Promise.all([received, sent])
  return {
    sent: size,
    sentSha256: hex(sentSha256),
    received: got.received,
    receivedSha256: hex(got.receivedSha256),
    verified: got.verified,
  }
}

// closeAfterTransfer ends the write side, then waits until the peer has ended its own, so that every byte
// written reaches the peer before the connection goes. It destroys the connection in the end.
async function closeAfterTransfer(socket, peerEnded) {
  socket.end()
  if (!peerEnded.done) {
    await Promise.race([
      peerEnded.promise,
      new Promise((resolve) => setTimeout(resolve, CLOSE_WAIT_MS)),
    ])
  }
  socket.destroy()
}

// trackEnd records whether the peer has ended its write side, so a close after that does not wait.
function trackEnd(socket) {
  const state = { done: false, promise: null }
  state.promise = new Promise((resolve) => {
    socket.once('end', () => {
      state.done = true
      resolve()
    })
  })
  return state
}

// serveConnection runs one accepted connection's transfer and reports it on stdout.
async function serveConnection(socket, size) {
  socket.on('error', () => {}) // an error after the transfer is over is not ours to report
  const peerEnded = trackEnd(socket)
  const base = { event: 'transfer', remotePublicKey: hex(socket.remotePublicKey) }
  try {
    const result = await transfer(socket, size)
    emit({ ...base, ok: result.verified, result })
  } catch (err) {
    emit({ ...base, ok: false, error: err.message })
  }
  await closeAfterTransfer(socket, peerEnded)
}

async function runServer(opts) {
  const dht = newNode(opts.bootstrap, opts.seed)
  let server = null
  const shutdown = async () => {
    if (server) await server.close().catch(() => {})
    await dht.destroy().catch(() => {})
  }
  try {
    await dht.fullyBootstrapped()
    server = dht.createServer(
      { relayThrough: opts.relay ? () => opts.relay : null },
      (socket) => {
        serveConnection(socket, opts.size)
      },
    )
    await server.listen()
    emit({ ready: true, role: 'server', publicKey: hex(server.publicKey) })
  } catch (err) {
    await shutdown()
    throw err
  }
  return shutdown
}

async function lookup(dht, cmd) {
  const key = keyBytes(cmd.key, 'key')
  const target = Buffer.alloc(KEY_BYTES)
  sodium.crypto_generichash(target, key)
  let found = false
  const deadline = Date.now() + LOOKUP_TIMEOUT_MS
  for await (const data of dht.lookup(target)) {
    if (data && data.peers.some((p) => p.publicKey.equals(key))) {
      found = true
      break
    }
    if (Date.now() > deadline) break
  }
  return { ok: true, found }
}

async function connect(dht, opts, cmd) {
  const server = keyBytes(cmd.server, 'server')
  const size = Number(cmd.size)
  if (!Number.isInteger(size) || size < 0 || size > MAX_BYTES) {
    throw new Error('size must be a byte count')
  }
  const socket = dht.connect(server, { relayThrough: opts.relay ? () => opts.relay : null })
  socket.on('error', () => {})
  const peerEnded = trackEnd(socket)
  try {
    await waitFor(socket, 'connect', DIAL_TIMEOUT_MS, 'connection to the server')
    const result = await transfer(socket, size)
    result.remotePublicKey = hex(socket.remotePublicKey)
    return { ok: true, result }
  } finally {
    await closeAfterTransfer(socket, peerEnded)
  }
}

async function runClient(opts) {
  const dht = newNode(opts.bootstrap, opts.seed)
  const shutdown = () => dht.destroy().catch(() => {})
  try {
    await dht.fullyBootstrapped()
  } catch (err) {
    await shutdown()
    throw err
  }
  emit({ ready: true, role: 'client', publicKey: hex(dht.defaultKeyPair.publicKey) })

  // Commands run one after another, in the order they arrive.
  let chain = Promise.resolve()
  const rl = readline.createInterface({ input: process.stdin })
  rl.on('line', (line) => {
    chain = chain.then(() => handle(dht, opts, line))
  })
  return shutdown
}

async function handle(dht, opts, line) {
  let reply
  try {
    const cmd = JSON.parse(line)
    if (cmd.cmd === 'lookup') reply = await lookup(dht, cmd)
    else if (cmd.cmd === 'connect') reply = await connect(dht, opts, cmd)
    else throw new Error(`unknown command ${cmd.cmd}`)
  } catch (err) {
    reply = { ok: false, error: err.message }
  }
  emit(reply)
}

async function runTestnet(size) {
  const testnet = await createTestnet(size, { host: HOST })
  const bootstrap = testnet.bootstrap.map((b) => `${b.host}:${b.port}`)
  emit({ ready: true, role: 'testnet', bootstrap })
  return () => testnet.destroy()
}

async function main() {
  const opts = parseArgs(process.argv.slice(2))
  const shutdown = opts.role === 'testnet'
    ? await runTestnet(opts.size)
    : opts.role === 'server'
      ? await runServer(opts)
      : await runClient(opts)

  let stopping = false
  const stop = async (code) => {
    if (stopping) return
    stopping = true
    // A hard stop, in case destroy hangs.
    setTimeout(() => process.exit(code), 5000).unref()
    await shutdown().catch(() => {})
    process.exit(code)
  }
  process.on('SIGTERM', () => stop(0))
  process.on('SIGINT', () => stop(0))
  // Closing stdin stops every role: the parent test holds stdin open for the whole test.
  process.stdin.on('end', () => stop(0))
  process.stdin.on('close', () => stop(0))
  process.stdin.resume()
}

main().catch((err) => {
  process.stderr.write(`hyperdht-peer: ${err.message}\n`)
  process.exit(1)
})
