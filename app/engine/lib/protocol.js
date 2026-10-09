// Protocol v1 codec for the app engine: the handshake, the eleven channel messages and the unordered
// datagram, as compact-encoding encodings. The frames are frozen by spec/vectors/frames.json and
// docs/architecture.md, "Wire protocol v1".

const c = require('compact-encoding')

// Service kinds, handshake flag bits and reject codes, from docs/architecture.md.
const KIND = { unknown: 0, https: 1, http: 2, tcp: 3, udp: 4 }
const FLAG = { resume: 1, lan: 2, datagrams: 4 }
const REJECT = { unknownService: 1, limitReached: 2, targetRefused: 3, targetTimedOut: 4 }

// A struct encodes its fields in order. decode returns a plain object.
function struct(fields) {
  return {
    preencode(state, m) {
      for (const [key, enc] of fields) enc.preencode(state, m[key])
    },
    encode(state, m) {
      for (const [key, enc] of fields) enc.encode(state, m[key])
    },
    decode(state) {
      const m = {}
      for (const [key, enc] of fields) m[key] = enc.decode(state)
      return m
    }
  }
}

// Runs check on the value when it is encoded and when it is decoded.
function checked(enc, check) {
  return {
    preencode(state, v) {
      check(v)
      enc.preencode(state, v)
    },
    encode(state, v) {
      enc.encode(state, v)
    },
    decode(state) {
      const v = enc.decode(state)
      check(v)
      return v
    }
  }
}

// c.decode does not reject trailing bytes, so a whole frame must use every byte it is given.
function exact(enc) {
  return {
    preencode: enc.preencode,
    encode: enc.encode,
    decode(state) {
      const m = enc.decode(state)
      if (state.start !== state.end) throw new Error('trailing bytes after the frame')
      return m
    }
  }
}

const kind = checked(c.uint, (n) => {
  if (n > KIND.udp) throw new Error('unknown service kind')
})

// A data payload is 1 to 65536 bytes.
const data = checked(c.buffer, (b) => {
  if (b.byteLength < 1 || b.byteLength > 65536) {
    throw new Error('data payload must be 1 to 65536 bytes')
  }
})

const service = struct([
  ['name', c.string],
  ['kind', kind],
  ['port', c.uint],
  ['origins', c.array(c.string)]
])

const lan = struct([
  ['addresses', c.array(c.string)],
  ['port', c.uint]
])

const handshakeFields = struct([
  ['version', c.uint],
  ['flags', c.uint],
  ['services', c.array(service)]
])

// The lan block follows the fields only when the lan flag is set.
const handshake = exact({
  preencode(state, m) {
    handshakeFields.preencode(state, m)
    if (m.flags & FLAG.lan) lan.preencode(state, m.lan)
  },
  encode(state, m) {
    handshakeFields.encode(state, m)
    if (m.flags & FLAG.lan) lan.encode(state, m.lan)
  },
  decode(state) {
    const m = handshakeFields.decode(state)
    if (m.flags & FLAG.lan) m.lan = lan.decode(state)
    return m
  }
})

const datagram = struct([
  ['flow', c.uint],
  ['payload', c.buffer]
])

// The eleven channel messages, by index. Indexes are append-only: never renumber, never reuse.
const messages = [
  struct([['stream', c.uint], ['service', c.string], ['window', c.uint]]), // 0 open
  struct([['stream', c.uint], ['window', c.uint], ['token', c.fixed16]]), // 1 opened
  struct([['stream', c.uint], ['code', c.uint], ['reason', c.string]]), // 2 reject
  struct([['stream', c.uint], ['payload', data]]), // 3 data
  struct([['stream', c.uint], ['credit', c.uint], ['received', c.uint]]), // 4 window
  struct([['stream', c.uint]]), // 5 close
  struct([
    ['stream', c.uint], ['token', c.fixed16], ['received', c.uint], ['limit', c.uint]
  ]), // 6 reattach
  struct([['stream', c.uint], ['received', c.uint], ['limit', c.uint]]), // 7 reattached
  struct([['services', c.array(service)]]), // 8 services
  struct([['flow', c.uint], ['service', c.string], ['payload', c.buffer]]), // 9 flow
  datagram // 10 datagram
].map(exact)

const unordered = exact(datagram)

module.exports = { handshake, messages, unordered, KIND, FLAG, REJECT }
