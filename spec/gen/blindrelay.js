// Writes spec/vectors/blindrelay.json: the bytes blind-relay 1.6.1 writes for each pair and unpair
// message, for the Go tests in pears/blindrelay.
//
// Each pair entry gives the fields of one pair message (is_initiator, token, id, seq) and the bytes
// upstream encodes them to. The ids cover the 1, 3 and 5 byte forms of the uint encoding (0, 252,
// 253, 65536 and 4294967295), the largest being the top of the 32-bit stream id range.
//
// Run: cd spec/gen && npm ci && node blindrelay.js

const fs = require('fs')
const path = require('path')
const c = require('compact-encoding')
const { messages } = require('blind-relay')

const hex = (b) => Buffer.from(b).toString('hex')

const TOKENS = {
  ascending: Buffer.from(Array.from({ length: 32 }, (_, i) => i)),
  ones: Buffer.alloc(32, 0xff),
  zeros: Buffer.alloc(32),
}

const PAIRS = [
  { is_initiator: true, token: 'ascending', id: 1, seq: 0 },
  { is_initiator: false, token: 'ascending', id: 1, seq: 0 },
  { is_initiator: true, token: 'ones', id: 252, seq: 1 },
  { is_initiator: false, token: 'zeros', id: 253, seq: 0 },
  { is_initiator: true, token: 'ones', id: 65536, seq: 0 },
  { is_initiator: false, token: 'ascending', id: 4294967295, seq: 2 },
]

const UNPAIRS = [{ token: 'zeros' }, { token: 'ones' }]

const out = {
  pair: PAIRS.map((e) => {
    const token = TOKENS[e.token]
    const bytes = c.encode(messages.pair, {
      isInitiator: e.is_initiator,
      token,
      id: e.id,
      seq: e.seq,
    })
    return { ...e, token: hex(token), hex: hex(bytes) }
  }),
  unpair: UNPAIRS.map((e) => {
    const token = TOKENS[e.token]
    return { token: hex(token), hex: hex(c.encode(messages.unpair, { token })) }
  }),
}

const file = path.join(__dirname, '..', 'vectors', 'blindrelay.json')
fs.writeFileSync(file, JSON.stringify(out, null, 2) + '\n')
console.log(
  `wrote ${out.pair.length} pair and ${out.unpair.length} unpair messages to ${file}`
)
