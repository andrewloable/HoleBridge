// Writes spec/vectors/dhtrpc.json: dht-rpc request and response packets as the upstream encoder
// lays them out, for the Go codec in pears/dhtrpc.
//
// Each hex value is written by the reference: requests by Request._encodeRequest and responses by
// Request._sendReply, both in lib/io.js. The IO is built with a stub UDX and no sockets are bound.
// Three inputs are fixed so the output is deterministic: the token (upstream derives it from random
// secrets, so IO.token is overridden), our id (the table id) and the transaction ids below.
//
// Run: cd spec/gen && npm ci && node dhtrpc.js

const fs = require('fs')
const path = require('path')
const IO = require('dht-rpc/lib/io')
const { PING, FIND_NODE, DOWN_HINT, DELAYED_PING } = require('dht-rpc/lib/commands')
const DHTError = require('dht-rpc/lib/errors')
const { version } = require('dht-rpc/package.json')

// n bytes as hex, each one more than the last from seed, wrapping at 256.
const bytesHex = (n, seed) =>
  Buffer.from(Array.from({ length: n }, (_, i) => (seed + i) & 255)).toString('hex')

// Documentation addresses (RFC 5737), so the vectors hold no real network addresses.
const PEER = { host: '198.51.100.20', port: 4001 }
const REQUESTER = { host: '192.0.2.10', port: 49152 }
const CLOSER = [
  { host: '203.0.113.5', port: 3000 },
  { host: '203.0.113.6', port: 4001 }
]

const ID = bytesHex(32, 0x10)
const TOKEN = bytesHex(32, 0x40)
const TARGET = bytesHex(32, 0x80)
const SHORT_VALUE = bytesHex(3, 0xa0)
const LONG_VALUE = bytesHex(300, 0)

const REQUESTS = [
  { name: 'ping', tid: 0x1234, to: PEER, internal: true, command: PING },
  { name: 'ping_with_id', tid: 0x0001, to: PEER, internal: true, command: PING, id: ID },
  { name: 'ping_with_token', tid: 0xbeef, to: PEER, internal: false, command: PING, token: TOKEN },
  { name: 'find_node', tid: 0xffff, to: PEER, internal: true, command: FIND_NODE, target: TARGET },
  {
    name: 'find_node_not_internal',
    tid: 0x0100,
    to: PEER,
    internal: false,
    command: FIND_NODE,
    target: TARGET
  },
  {
    name: 'down_hint_with_value',
    tid: 0x0203,
    to: PEER,
    internal: true,
    command: DOWN_HINT,
    value: SHORT_VALUE
  },
  {
    name: 'delayed_ping_long_value',
    tid: 0x0405,
    to: PEER,
    internal: true,
    command: DELAYED_PING,
    value: LONG_VALUE
  },
  {
    name: 'find_node_all_fields',
    tid: 0x5678,
    to: PEER,
    internal: true,
    command: FIND_NODE,
    id: ID,
    token: TOKEN,
    target: TARGET,
    value: SHORT_VALUE
  }
]

const RESPONSES = [
  { name: 'reply_with_id', tid: 0x1234, to: REQUESTER, id: ID },
  { name: 'reply_with_token', tid: 0x1235, to: REQUESTER, token: TOKEN },
  {
    name: 'find_node_reply',
    tid: 0x2222,
    to: REQUESTER,
    token: TOKEN,
    closer_nodes: CLOSER
  },
  {
    name: 'error_unknown_command',
    tid: 0x3333,
    to: REQUESTER,
    error: DHTError.UNKNOWN_COMMAND,
    closer_nodes: CLOSER
  },
  {
    name: 'error_invalid_token',
    tid: 0x4444,
    to: REQUESTER,
    error: DHTError.INVALID_TOKEN,
    token: TOKEN
  },
  { name: 'value_reply', tid: 0x5555, to: REQUESTER, value: SHORT_VALUE },
  {
    name: 'all_fields_reply',
    tid: 0x6666,
    to: REQUESTER,
    id: ID,
    token: TOKEN,
    closer_nodes: CLOSER,
    value: SHORT_VALUE
  }
]

const b = (s) => (s === undefined ? null : Buffer.from(s, 'hex'))

// The socket stand-ins keep the bytes that trySend is given. Encoding compares the socket with the
// server socket to decide whether the sender id is written.
const capture = () => ({ out: null, trySend(buf) { this.out = buf } })
const clientSocket = capture()
const serverSocket = capture()

const table = { id: b(ID), nodes: [], closest: () => table.nodes }
const io = new IO(table, { watchNetworkInterfaces: () => ({ destroy() {} }) })
io.token = () => b(TOKEN)
io.ephemeral = false
io.serverSocket = serverSocket

function encodeRequest(r) {
  const req = io.createRequest(
    r.to,
    b(r.token),
    r.internal,
    r.command,
    b(r.target),
    b(r.value),
    null,
    0
  )
  req.tid = r.tid
  const socket = r.id ? serverSocket : clientSocket
  return req._encodeRequest(b(r.token), b(r.value), r.to, socket)
}

// A reply carries closer nodes only when the request has a target, so every reply here has one.
// The table returns the closer nodes given for that reply.
function encodeResponse(r) {
  const req = io.createRequest(r.to, null, false, 0, b(TARGET), null, null, 0)
  req.tid = r.tid
  table.nodes = r.closer_nodes ?? []
  const socket = r.id ? serverSocket : clientSocket
  req._sendReply(
    r.error ?? 0,
    b(r.value),
    r.token !== undefined,
    r.closer_nodes !== undefined,
    r.to,
    socket
  )
  return socket.out
}

function requestVector(r) {
  const v = { name: r.name, tid: r.tid, to: r.to, internal: r.internal, command: r.command }
  for (const f of ['id', 'token', 'target', 'value']) if (r[f] !== undefined) v[f] = r[f]
  v.hex = encodeRequest(r).toString('hex')
  return v
}

function responseVector(r) {
  const v = { name: r.name, tid: r.tid, to: r.to, error: r.error ?? 0 }
  for (const f of ['id', 'token', 'closer_nodes', 'value']) if (r[f] !== undefined) v[f] = r[f]
  v.hex = encodeResponse(r).toString('hex')
  return v
}

const out = {
  description:
    'dht-rpc requests and responses: each optional field present and absent, closer nodes, error codes and a value. Byte 0 is (type << 4) | 3, then the flags, tid, the address and the optional fields in the order the flags name',
  reference: `dht-rpc ${version}`,
  requests: REQUESTS.map(requestVector),
  responses: RESPONSES.map(responseVector)
}

const file = path.join(__dirname, '..', 'vectors', 'dhtrpc.json')
fs.writeFileSync(file, JSON.stringify(out, null, 2) + '\n')
console.log(
  `wrote dhtrpc.json: ${out.requests.length} requests, ${out.responses.length} responses`
)
