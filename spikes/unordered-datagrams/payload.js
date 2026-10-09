'use strict'

// Test payloads shared by the driver and the bare client. Bytes 0-3 carry the
// sequence number, the rest is a pattern derived from it, so a receiver can
// tell an intact message from a truncated or corrupted one. Only b4a, which
// runs under both Node and Bare.
const b4a = require('b4a')

function make(seq, size) {
  const buf = b4a.alloc(size)
  buf[0] = (seq >>> 24) & 0xff
  buf[1] = (seq >>> 16) & 0xff
  buf[2] = (seq >>> 8) & 0xff
  buf[3] = seq & 0xff
  for (let i = 4; i < size; i++) buf[i] = (seq + i * 7) & 0xff
  return buf
}

// Returns the sequence number when the message is internally consistent, else -1.
function seqOf(buf) {
  if (buf.length < 4) return -1
  const seq = buf[0] * 16777216 + (buf[1] << 16) + (buf[2] << 8) + buf[3]
  for (let i = 4; i < buf.length; i++) {
    if (buf[i] !== ((seq + i * 7) & 0xff)) return -1
  }
  return seq
}

module.exports = { make, seqOf }
