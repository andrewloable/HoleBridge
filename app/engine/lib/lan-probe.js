// LAN probes and the sliced sweep for the app engine (docs/architecture.md, LAN route: Discovery and
// Probe and reply bytes). The probe and reply codec, and findHost, which first probes the host's last
// known addresses alone and then sweeps each local /24 in slices of 32 probes, each slice on its own
// socket. Probes are unicast only; the responder never sees a broadcast.
//
// - socketFactory() returns a udx-native UDP socket. findHost uses only bind, trySend(buf, port, host),
//   on('message', (msg, from)) with from = { host, family, port }, and close() (a promise). It makes one
//   socket for the lone probes and one per sweep slice, and closes every one before it resolves.
// - timers is { setTimeout(fn, ms), clearTimeout(id) }; findHost waits only through it.
// - port is the UDP port of the responder. The result's port is the host's LAN TCP port from the reply.

const b4a = require('b4a')
const sodium = require('sodium-universal')

const PROBE_LENGTH = 256
const REPLY_LENGTH = 64
const NONCE_LENGTH = 16
const VERSION = 1
const PROBE_MAGIC = b4a.from('HBLANQ1')
const REPLY_MAGIC = b4a.from('HBLANR1')

// The timing of findHost (docs/architecture.md, LAN route). Times in milliseconds.
const LONE_WAIT = 300
const SLICE_SIZE = 32
const SLICE_GAP = 100
const SWEEP_WAIT = 800

// mac(key, bytes) is the BLAKE2b-256 MAC of bytes keyed with key.
function mac(key, bytes) {
  const out = b4a.alloc(32)
  sodium.crypto_generichash(out, bytes, key)
  return out
}

// sameBytes compares two equal-length buffers in time that does not depend on where they differ.
function sameBytes(a, b) {
  let diff = 0
  for (let i = 0; i < a.length; i++) diff |= a[i] ^ b[i]
  return diff === 0
}

// encodeProbe(lanKey, nonce, unixMs) -> Buffer(256): magic, version, nonce, the sender's wall clock as a
// little-endian uint64, the MAC over bytes 0 to 31, then zero padding.
function encodeProbe(lanKey, nonce, unixMs) {
  const out = b4a.alloc(PROBE_LENGTH)
  out.set(PROBE_MAGIC, 0)
  out[7] = VERSION
  out.set(nonce, 8)
  new DataView(out.buffer, out.byteOffset, out.byteLength).setBigUint64(24, BigInt(unixMs), true)
  out.set(mac(lanKey, out.subarray(0, 32)), 32)
  return out
}

// verifyReply(lanKey, buf, nonce) -> port | null. A reply is valid when it is 64 bytes, has the magic and
// version, echoes nonce, and its MAC checks. The result is the LAN TCP port at bytes 24 and 25.
function verifyReply(lanKey, buf, nonce) {
  if (buf.length !== REPLY_LENGTH || nonce.length !== NONCE_LENGTH) return null
  if (!b4a.equals(buf.subarray(0, 7), REPLY_MAGIC) || buf[7] !== VERSION) return null
  if (!b4a.equals(buf.subarray(8, 24), nonce)) return null
  if (!sameBytes(mac(lanKey, buf.subarray(0, 32)), buf.subarray(32, 64))) return null
  return buf[24] | (buf[25] << 8)
}

// sweepHosts(localAddresses) lists .1 to .254 of each local IPv4 /24, minus the local addresses, in
// address order. .0 and .255 are never probed, and nothing outside localAddresses' /24s is.
function sweepHosts(localAddresses) {
  const own = new Set(localAddresses)
  const prefixes = new Set()
  for (const addr of localAddresses) {
    const parts = addr.split('.')
    if (parts.length === 4 && parts.every((p) => /^\d{1,3}$/.test(p))) prefixes.add(parts.slice(0, 3).join('.'))
  }
  const hosts = []
  for (const prefix of prefixes) {
    for (let i = 1; i <= 254; i++) {
      const addr = `${prefix}.${i}`
      if (!own.has(addr)) hosts.push(addr)
    }
  }
  return hosts
}

// findHost({ lanKey, lastKnown, localAddresses, port, socketFactory, timers }) -> { address, port } | null.
// It probes lastKnown from one socket and waits up to 300 ms. Then it sweeps the local /24s in slices of
// 32, each on a new socket, 100 ms apart, and waits 800 ms after the last slice. The first verified reply
// ends the search at once. Every socket is closed before the result is returned.
async function findHost({ lanKey, lastKnown, localAddresses, port, socketFactory, timers }) {
  const sockets = []
  // The nonce of every probe sent. A reply counts only if it echoes one of them, and each probe has its
  // own nonce, because the responder drops a repeated nonce as a replay.
  const nonces = new Set()
  let result = null
  let wake = () => {}

  // pause(ms) waits ms, or until a verified reply wakes it.
  const pause = (ms) =>
    new Promise((resolve) => {
      const id = timers.setTimeout(resolve, ms)
      wake = () => {
        timers.clearTimeout(id)
        resolve()
      }
    })

  const open = () => {
    const socket = socketFactory()
    sockets.push(socket)
    socket.bind(0, '0.0.0.0')
    socket.on('message', (msg, from) => {
      if (result !== null) return
      const nonce = msg.subarray(8, 24)
      if (!nonces.has(b4a.toString(nonce, 'hex'))) return
      const replyPort = verifyReply(lanKey, msg, nonce)
      if (replyPort === null) return
      result = { address: from.host, port: replyPort }
      wake()
    })
    return socket
  }

  const probe = (socket, host) => {
    const nonce = b4a.alloc(NONCE_LENGTH)
    sodium.randombytes_buf(nonce)
    nonces.add(b4a.toString(nonce, 'hex'))
    socket.trySend(encodeProbe(lanKey, nonce, Date.now()), port, host)
  }

  try {
    if (lastKnown.length > 0) {
      const socket = open()
      for (const host of lastKnown) probe(socket, host)
      await pause(LONE_WAIT)
    }

    const hosts = sweepHosts(localAddresses)
    for (let start = 0; start < hosts.length; start += SLICE_SIZE) {
      if (start > 0) await pause(SLICE_GAP)
      if (result !== null) break
      const socket = open()
      for (const host of hosts.slice(start, start + SLICE_SIZE)) probe(socket, host)
    }
    if (result === null) await pause(SWEEP_WAIT)
    return result
  } finally {
    await Promise.all(sockets.map((socket) => socket.close()))
  }
}

module.exports = { encodeProbe, verifyReply, findHost }
