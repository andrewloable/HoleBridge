// TV handoff (D36, docs/architecture.md, "Adding a host to a TV (handoff)"). The TV listens for one sealed
// box from a phone on the LAN and adds the host it carries. The phone seals the host to the TV's public key
// and sends it.

const b4a = require('b4a')
const c = require('compact-encoding')
const sodium = require('sodium-universal')
const TCP = require('bare-tcp')
const keys = require('./keys.js')

// The format version of the link and of the box plaintext (docs/architecture.md, "Formats").
const VERSION = 1
// The link's base. The domain is a placeholder (owner decision); parseLink accepts any https host.
const BASE = 'https://holebridge.app'
// Bounds of the listener: one box of at most 1 KiB, 5 s to send it, at most 4 connections at once, and a
// code that lives 5 minutes.
const BOX_MAX = 1024
const READ_DEADLINE = 5 * 1000
const CONNECTIONS_MAX = 4
const LINK_LIFETIME = 5 * 60 * 1000
// How long the phone waits on one address before it tries the next.
const SEND_TIMEOUT = 5 * 1000
// The TV's one-byte answer to a box: 1 when the host is added, 0 when it is refused.
const REPLY = { added: 1, refused: 0 }

// A handoff link is https://<host>/h#1.<public key>.<secret>.<port>.<addresses>: 64 lowercase hex digits,
// then 32, a decimal port without a leading zero, and a comma list of IPv4 addresses.
const LINK = /^https:\/\/[^/?#]+\/h#1\.([0-9a-f]{64})\.([0-9a-f]{32})\.([1-9][0-9]{0,4})\.([0-9.,]+)$/
const IPV4 = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/

// hbError makes an error that carries an HB code and the reason, as lib/connect.js and lib/keys.js do.
function hbError(code, reason) {
  return Object.assign(new Error(`${code}: ${reason}`), { name: 'HbError', code, reason })
}

// bytes returns a hex string as bytes, and any other value as it is.
function bytes(value) {
  return typeof value === 'string' ? b4a.from(value, 'hex') : value
}

// The box plaintext: version, the one-time 16-byte secret, the host's name, its key and the 32-byte
// application key, compact-encoded in that order.
const PLAINTEXT = {
  preencode(state, m) {
    c.uint.preencode(state, m.version)
    c.fixed16.preencode(state, m.secret)
    c.string.preencode(state, m.name)
    c.string.preencode(state, m.key)
    c.fixed32.preencode(state, m.appKey)
  },
  encode(state, m) {
    c.uint.encode(state, m.version)
    c.fixed16.encode(state, m.secret)
    c.string.encode(state, m.name)
    c.string.encode(state, m.key)
    c.fixed32.encode(state, m.appKey)
  },
  decode(state) {
    return {
      version: c.uint.decode(state),
      secret: c.fixed16.decode(state),
      name: c.string.decode(state),
      key: c.string.decode(state),
      appKey: c.fixed32.decode(state)
    }
  }
}

// encodePlaintext({ version, secret, name, key, appKey }) returns the box plaintext. secret and appKey are
// bytes or hex strings.
function encodePlaintext({ version, secret, name, key, appKey }) {
  return c.encode(PLAINTEXT, { version, secret: bytes(secret), name, key, appKey: bytes(appKey) })
}

// decodePlaintext reads a box plaintext and throws when it is malformed, has another version, or carries a
// key that is not valid. The key comes back normalized.
function decodePlaintext(plain) {
  const state = c.state(0, plain.length, plain)
  const m = PLAINTEXT.decode(state)
  if (state.start !== state.end) throw new Error('trailing bytes after the box plaintext')
  if (m.version !== VERSION) throw new Error('unknown box version')
  return { ...m, key: keys.normalize(m.key) }
}

// encodeLink(base, { publicKey, secret, port, addresses }) returns the TV's link.
function encodeLink(base, { publicKey, secret, port, addresses }) {
  return `${base}/h#${VERSION}.${publicKey}.${secret}.${port}.${addresses.join(',')}`
}

// isIPv4 is true for a dotted quad whose octets are all 0 to 255.
function isIPv4(address) {
  const m = IPV4.exec(address)
  return m !== null && m.slice(1).every((octet) => Number(octet) <= 255)
}

// parseLink(link) returns { publicKey, secret, port, addresses } and throws for a link that is not a
// handoff link. The fields come back as encodeLink takes them.
function parseLink(link) {
  const m = LINK.exec(link)
  if (m === null) throw new Error('not a handoff link')
  const port = Number(m[3])
  const addresses = m[4].split(',')
  if (port > 65535 || !addresses.every(isIPv4)) throw new Error('handoff link has a bad port or address')
  return { publicKey: m[1], secret: m[2], port, addresses }
}

// listenOn binds server to port on host and resolves once it listens. bare-tcp reports a bind failure as an
// 'error' event, not a throw.
function listenOn(server, port, host) {
  return new Promise((resolve, reject) => {
    server.on('error', reject)
    server.listen(port, host, () => resolve())
  })
}

/**
 * listen({ addresses, clock }) -> Promise<{ link, received, cancel }>
 *
 * Starts the TV's listener on each LAN address, all on one port. link is the code to show. received
 * resolves with { name, key, appKey } for the first box that opens and carries the one-time secret, and
 * rejects with HB-HANDOFF-EXPIRED after 5 minutes. cancel() stops the listener and its connections; received
 * then stays pending. clock supplies setTimeout and clearTimeout.
 */
async function listen({ addresses, clock }) {
  const publicKey = b4a.alloc(sodium.crypto_box_PUBLICKEYBYTES)
  const secretKey = b4a.alloc(sodium.crypto_box_SECRETKEYBYTES)
  sodium.crypto_box_keypair(publicKey, secretKey)
  const secret = b4a.alloc(16)
  sodium.randombytes_buf(secret)

  let resolveReceived
  let rejectReceived
  const received = new Promise((resolve, reject) => {
    resolveReceived = resolve
    rejectReceived = reject
  })
  const servers = []
  const sockets = new Set()
  let done = false
  const expiry = clock.setTimeout(() => {
    rejectReceived(hbError('HB-HANDOFF-EXPIRED', 'expired'))
    stop()
  }, LINK_LIFETIME)

  // stop closes the listeners and every connection, except keep, the one that is answering.
  function stop(keep = null) {
    if (done) return
    done = true
    clock.clearTimeout(expiry)
    for (const server of servers) server.close()
    for (const socket of sockets) if (socket !== keep) socket.destroy()
  }

  // openBox returns the decoded plaintext of a box sealed to the TV, or null when it does not open or does
  // not decode.
  function openBox(box) {
    if (box.length < sodium.crypto_box_SEALBYTES) return null
    const plain = b4a.alloc(box.length - sodium.crypto_box_SEALBYTES)
    if (!sodium.crypto_box_seal_open(plain, box, publicKey, secretKey)) return null
    try {
      return decodePlaintext(plain)
    } catch {
      return null
    }
  }

  // answer takes the whole box one connection sent. A valid box with the one-time secret adds the host and
  // stops the listener. Anything else gets byte 0, and the listener keeps waiting.
  function answer(socket, box) {
    if (done) return socket.destroy()
    const value = openBox(box)
    if (value === null || !sodium.sodium_memcmp(value.secret, secret)) {
      socket.end(b4a.from([REPLY.refused]))
      return
    }
    socket.end(b4a.from([REPLY.added]))
    stop(socket)
    resolveReceived({ name: value.name, key: value.key, appKey: value.appKey })
  }

  // accept handles one connection: refused at once beyond CONNECTIONS_MAX, otherwise it reads a box of at
  // most BOX_MAX bytes within READ_DEADLINE. Anything else closes it with no reply.
  function accept(socket) {
    if (done || sockets.size >= CONNECTIONS_MAX) return socket.destroy()
    sockets.add(socket)
    const chunks = []
    let size = 0
    const deadline = clock.setTimeout(() => socket.destroy(), READ_DEADLINE)
    socket.on('data', (chunk) => {
      size += chunk.length
      if (size > BOX_MAX) socket.destroy()
      else chunks.push(chunk)
    })
    socket.on('end', () => {
      clock.clearTimeout(deadline)
      answer(socket, b4a.concat(chunks))
    })
    socket.on('error', () => socket.destroy())
    socket.on('close', () => {
      clock.clearTimeout(deadline)
      sockets.delete(socket)
    })
  }

  let port = 0
  try {
    // The first address takes any free port; the others take that same port, so one link serves them all.
    for (const address of addresses) {
      const server = TCP.createServer(accept)
      servers.push(server)
      await listenOn(server, port, address)
      port = server.address().port
    }
  } catch (err) {
    stop()
    throw err
  }

  const link = encodeLink(BASE, {
    publicKey: b4a.toString(publicKey, 'hex'),
    secret: b4a.toString(secret, 'hex'),
    port,
    addresses
  })
  return { link, received, cancel: () => stop() }
}

// exchange dials one address, sends the box and resolves with the first byte of the TV's reply. It resolves
// with null when the address does not connect, closes with no reply, or does not answer in SEND_TIMEOUT.
function exchange(address, port, box) {
  return new Promise((resolve) => {
    const socket = TCP.createConnection(port, address)
    let finished = false
    const finish = (reply) => {
      if (finished) return
      finished = true
      clearTimeout(timer)
      socket.destroy()
      resolve(reply)
    }
    const timer = setTimeout(() => finish(null), SEND_TIMEOUT)
    socket.on('connect', () => socket.end(box))
    socket.on('data', (chunk) => finish(chunk[0]))
    socket.on('error', () => finish(null))
    socket.on('end', () => finish(null))
    socket.on('close', () => finish(null))
  })
}

// send(link, { name, key, appKey }) seals the host to the TV named in link and sends it. It resolves once the
// TV has added the host. It rejects with HB-HANDOFF-UNREACHABLE when no address of the TV answers, and with
// HB-HANDOFF-REFUSED when the TV answers 0.
async function send(link, { name, key, appKey }) {
  const { publicKey, secret, port, addresses } = parseLink(link)
  const plain = encodePlaintext({ version: VERSION, secret, name, key, appKey })
  const box = b4a.alloc(plain.length + sodium.crypto_box_SEALBYTES)
  sodium.crypto_box_seal(box, plain, b4a.from(publicKey, 'hex'))
  for (const address of addresses) {
    const reply = await exchange(address, port, box)
    if (reply === null) continue
    if (reply === REPLY.added) return
    throw hbError('HB-HANDOFF-REFUSED', 'refused')
  }
  throw hbError('HB-HANDOFF-UNREACHABLE', 'unreachable')
}

module.exports = { encodeLink, parseLink, encodePlaintext, listen, send }
