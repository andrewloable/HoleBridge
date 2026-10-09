// Writes spec/vectors/udx-packets.json: UDX packet headers as libudx lays them out (udx.h and
// udx.c init_stream_packet and process_packet), and one dht-rpc PING request, for the Go packet
// codec and socket tests in pears/udx.
//
// udx-native exposes no raw header bytes, so the headers are written here by hand from the layout
// below. The DATA, END, MESSAGE, DESTROY, ACK and SACK headers were also checked byte for byte
// against live udx-native 1.21.3 packets captured on loopback. The dht-rpc request is encoded by
// dht-rpc 6.27.0 itself.
//
// Run: cd spec/gen && npm ci && node udx-packets.js

const fs = require('fs')
const path = require('path')
const DHT = require('dht-rpc')

// Type flags, udx.h UDX_HEADER_*.
const FLAG = { data: 0b00001, end: 0b00010, sack: 0b00100, message: 0b01000, destroy: 0b10000 }

// Receive window that udx-native 1.21.3 puts on the wire. libudx 1.12.0 hardcodes 0xffffffff.
const WINDOW_1_21_3 = 0x00400000

// Layout from udx.c init_stream_packet and process_packet. Integers are little endian.
//   byte 0       magic, UDX_MAGIC_BYTE (255)
//   byte 1       version, UDX_VERSION (1)
//   byte 2       type flags (FLAG above)
//   byte 3       data offset: zero padding between the header and the payload, written by
//                mtu_probeify_packet for MTU probes
//   bytes 4-7    remote id: the stream id on the receiving side
//   bytes 8-11   receive window
//   bytes 12-15  seq
//   bytes 16-19  ack
// A SACK payload is (start, end) uint32 pairs, end exclusive.
function udxPacket({ type, dataOffset, remoteId, recvWindow, seq, ack, payload }) {
  const header = Buffer.alloc(20)
  header[0] = 255
  header[1] = 1
  header[2] = type
  header[3] = dataOffset
  header.writeUInt32LE(remoteId, 4)
  header.writeUInt32LE(recvWindow, 8)
  header.writeUInt32LE(seq, 12)
  header.writeUInt32LE(ack, 16)
  return Buffer.concat([header, Buffer.alloc(dataOffset), payload])
}

function sackPair(start, end) {
  const b = Buffer.alloc(8)
  b.writeUInt32LE(start, 0)
  b.writeUInt32LE(end, 4)
  return b
}

const NONE = Buffer.alloc(0)

// The first six cases are the packets captured from udx-native 1.21.3, in the order they were
// sent. The rest cover the remaining header fields.
const CASES = [
  { name: 'data', type: FLAG.data, remoteId: 7, seq: 0, ack: 0, payload: Buffer.from('hello') },
  { name: 'end_empty', type: FLAG.end, remoteId: 7, seq: 1, ack: 0, payload: NONE },
  { name: 'message', type: FLAG.message, remoteId: 8, seq: 0, ack: 0, payload: Buffer.from('msg') },
  { name: 'destroy', type: FLAG.destroy, remoteId: 8, seq: 0, ack: 0, payload: NONE },
  { name: 'ack_only', type: 0, remoteId: 9, seq: 0, ack: 1, payload: NONE },
  { name: 'sack', type: FLAG.sack, remoteId: 9, seq: 0, ack: 1, payload: sackPair(5, 6) },
  { name: 'data_end', type: FLAG.data | FLAG.end, remoteId: 0x01020304, seq: 8, ack: 3, payload: Buffer.from('!') },
  { name: 'data_offset_probe', type: FLAG.data, dataOffset: 3, remoteId: 7, seq: 2, ack: 1, payload: Buffer.from('abc') },
  { name: 'data_recv_window_1_12', type: FLAG.data, remoteId: 7, recvWindow: 0xffffffff, seq: 0, ack: 0, payload: Buffer.from('a') },
]

// dht-rpc 6.27.0 PING request, encoded by dht-rpc with the transaction id fixed at 0x1234. Byte 0
// is the request id (0x03), then flags, tid (uint16 LE), the destination (IPv4, then port LE) and
// the command (0 = PING).
async function dhtRpcPing() {
  const dht = new DHT({ bootstrap: false, ephemeral: true, host: '127.0.0.1' })
  try {
    const to = { host: '127.0.0.1', port: 4242 }
    const req = dht.io.createRequest(to, null, true, 0, null, null, null, 0)
    req.tid = 0x1234
    const buf = req._encodeRequest(null, null, to, dht.io.clientSocket)
    req.destroy()
    return buf
  } finally {
    await dht.destroy()
  }
}

async function main() {
  const headers = CASES.map((c) => {
    const f = { dataOffset: 0, recvWindow: WINDOW_1_21_3, ...c }
    return {
      name: f.name,
      type: f.type,
      data_offset: f.dataOffset,
      remote_id: f.remoteId,
      recv_window: f.recvWindow,
      seq: f.seq,
      ack: f.ack,
      payload: f.payload.toString('hex'),
      bytes: udxPacket(f).toString('hex'),
    }
  })
  const out = { udx_headers: headers, dht_rpc_ping_request: (await dhtRpcPing()).toString('hex') }
  const file = path.join(__dirname, '..', 'vectors', 'udx-packets.json')
  fs.writeFileSync(file, JSON.stringify(out, null, 2) + '\n')
  console.log(`wrote ${headers.length} header vectors and the dht-rpc PING to ${file}`)
}

main().catch((err) => {
  console.error(err)
  process.exit(1)
})
