// VPN mode DNS (D37, docs/architecture.md, "VPN mode (Android and iOS)"). The app gives every service an
// address in 198.18.0.0/16 and a name, and answers DNS for those names itself. Every other query is
// forwarded unread and unlogged to the network's normal DNS. Wire format: RFC 1035, section 4.1.
// Nothing here logs a name or a query.

const b4a = require('b4a')
const UDX = require('udx-native')

const UPSTREAM_PORT = 53
const TTL = 60
const CLASS_IN = 1
const TYPE_A = 1
const TYPE_AAAA = 28
const RCODE_FORMERR = 1
const RCODE_SERVFAIL = 2
const FIRST_ADDRESS = ipv4ToInt('198.18.0.2')
// A FORMERR reply carries a question, because a malformed query has none the reply can echo. This is the
// root name with type A and class IN.
const ROOT_QUESTION = b4a.from([0, 0, 1, 0, 1])

function ipv4ToInt(address) {
  return address.split('.').reduce((n, octet) => n * 256 + Number(octet), 0)
}

function intToIpv4(n) {
  return [n >>> 24, (n >>> 16) & 0xff, (n >>> 8) & 0xff, n & 0xff].join('.')
}

function bytes16(n) {
  return b4a.from([(n >> 8) & 0xff, n & 0xff])
}

function bytes32(n) {
  return b4a.from([(n >>> 24) & 0xff, (n >>> 16) & 0xff, (n >>> 8) & 0xff, n & 0xff])
}

function u16(buf, i) {
  return (buf[i] << 8) | buf[i + 1]
}

// keyOf(service, host) is the key of a service in previous: 'service.host', lowercased.
function keyOf(service, host) {
  return `${service}.${host}`.toLowerCase()
}

// hostnameOf(origin) is the host part of an origin such as 'https://jellyfin.example:8096', lowercased.
function hostnameOf(origin) {
  return origin.replace(/^[a-z][a-z0-9+.-]*:\/\//i, '').split(/[/:?#]/)[0].toLowerCase()
}

// allocate(entries, previous) gives each service an IPv4 address in 198.18.0.0/16, from 198.18.0.2 upward.
// entries are [{ host, service, origins }]. It returns { name -> address }, where name is
// '<service>.<host>.internal' lowercased, plus the hostname of each origin. A service keeps the address
// that previous gives it under its 'service.host' key, and a new name gets the lowest address not in use.
function allocate(entries, previous = {}) {
  const used = new Set()
  const given = new Map()
  for (const { host, service } of entries) {
    const key = keyOf(service, host)
    const kept = previous[key] === undefined ? NaN : ipv4ToInt(previous[key])
    if (!Number.isNaN(kept) && !used.has(kept)) {
      given.set(key, kept)
      used.add(kept)
    }
  }
  let next = FIRST_ADDRESS
  const names = {}
  for (const { host, service, origins = [] } of entries) {
    const key = keyOf(service, host)
    if (!given.has(key)) {
      while (used.has(next)) next++
      given.set(key, next)
      used.add(next)
    }
    const address = intToIpv4(given.get(key))
    names[`${service}.${host}.internal`.toLowerCase()] = address
    for (const origin of origins) names[hostnameOf(origin)] = address
  }
  return names
}

// readQuestion(buf) reads the first question of a query. It throws on anything malformed: a short header,
// no question, a label that runs past the end, a compression pointer (not allowed in a question), or no
// type and class after the name.
function readQuestion(buf) {
  if (buf.length < 12 || u16(buf, 4) < 1) throw new Error('malformed query')
  const labels = []
  let pos = 12
  for (;;) {
    const len = buf[pos]
    if (len === undefined || len > 63) throw new Error('malformed query')
    pos += 1
    if (len === 0) break
    if (pos + len > buf.length) throw new Error('malformed query')
    labels.push(b4a.toString(buf.subarray(pos, pos + len), 'utf8'))
    pos += len
  }
  if (pos + 4 > buf.length) throw new Error('malformed query')
  return { name: labels.join('.').toLowerCase(), type: u16(buf, pos), class: u16(buf, pos + 2), end: pos + 4 }
}

// reply(query, rcode, question, answers) builds a reply to query: the query's id, its opcode and RD, QR and
// RA set, the rcode, and one question and the answers. The header is padded when the query is shorter.
function reply(query, rcode, question, answers = []) {
  const head = b4a.alloc(12)
  head.set(query.subarray(0, 12))
  const flags = ((0x80 | (head[2] & 0x79)) << 8) | 0x80 | rcode
  return b4a.concat([
    head.subarray(0, 2),
    bytes16(flags),
    bytes16(1),
    bytes16(answers.length),
    bytes16(0),
    bytes16(0),
    question,
    ...answers
  ])
}

// addressRecord(address) is an A answer for the name at offset 12 (the question), with TTL 60.
function addressRecord(address) {
  return b4a.concat([bytes16(0xc00c), bytes16(TYPE_A), bytes16(CLASS_IN), bytes32(TTL), bytes16(4), address])
}

let udx = null

// defaultSocketFactory makes the UDP sockets the forwarding uses, from one shared UDX instance.
function defaultSocketFactory() {
  if (udx === null) udx = new UDX()
  return udx.createSocket()
}

// VpnDns answers queries for the names in names (the output of allocate) and forwards the rest. upstream is
// a list of IPv4 addresses; each is asked in turn on port 53, from a socket made by socketFactory(), and the
// first reply with the query's id from that address wins. handle(queryBuf) resolves with the reply bytes and
// never rejects. Names are matched case-insensitively.
class VpnDns {
  constructor({ names, upstream, socketFactory = defaultSocketFactory, timeoutMs = 2000 }) {
    this.names = new Map(
      Object.entries(names).map(([name, address]) => [name.toLowerCase(), b4a.from(address.split('.').map(Number))])
    )
    this.upstream = upstream
    this.socketFactory = socketFactory
    this.timeoutMs = timeoutMs
  }

  async handle(queryBuf) {
    let question
    try {
      question = readQuestion(queryBuf)
    } catch {
      return reply(queryBuf, RCODE_FORMERR, ROOT_QUESTION)
    }
    const echo = queryBuf.subarray(12, question.end)
    const address = question.class === CLASS_IN ? this.names.get(question.name) : undefined
    if (address && question.type === TYPE_A) return reply(queryBuf, 0, echo, [addressRecord(address)])
    if (address && question.type === TYPE_AAAA) return reply(queryBuf, 0, echo)
    for (const host of this.upstream) {
      const answer = await this.ask(host, queryBuf)
      if (answer !== null) return answer
    }
    return reply(queryBuf, RCODE_SERVFAIL, echo)
  }

  // ask sends queryBuf to one upstream and resolves with its reply, or null on timeout or error. The socket
  // is bound to IPv4 and closed before it resolves.
  async ask(host, queryBuf) {
    let socket = null
    try {
      socket = this.socketFactory()
      socket.bind(0, '0.0.0.0')
      return await new Promise((resolve) => {
        const finish = (value) => {
          clearTimeout(timer)
          resolve(value)
        }
        const timer = setTimeout(() => finish(null), this.timeoutMs)
        socket.on('message', (msg, from) => {
          if (from.host !== host || msg.length < 12 || msg[0] !== queryBuf[0] || msg[1] !== queryBuf[1]) return
          finish(b4a.from(msg))
        })
        socket.send(queryBuf, UPSTREAM_PORT, host).catch(() => finish(null))
      })
    } catch {
      return null
    } finally {
      if (socket !== null) await socket.close().catch(() => {})
    }
  }
}

module.exports = { allocate, VpnDns }
