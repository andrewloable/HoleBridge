// Engine side of the IPC between the Flutter app and the engine: frames as spec/ipc.md defines them.
// A frame is type, id, then the body fields in order, all compact-encoding 3.5.2.

const b4a = require('b4a')
const c = require('compact-encoding')

// A struct encodes its fields in order. Its value is a plain object.
function struct(fields) {
  return {
    preencode(state, o) {
      for (const [k, enc] of fields) enc.preencode(state, o[k])
    },
    encode(state, o) {
      for (const [k, enc] of fields) enc.encode(state, o[k])
    },
    decode(state) {
      const o = {}
      for (const [k, enc] of fields) o[k] = enc.decode(state)
      return o
    }
  }
}

// A bool is one byte, 0 or 1. Any other byte does not decode exactly.
const bool = {
  preencode: c.bool.preencode,
  encode: c.bool.encode,
  decode(state) {
    const byte = state.buffer[state.start]
    const value = c.bool.decode(state)
    if (byte > 1) throw new Error('bool is neither 0 nor 1')
    return value
  }
}

// The 32 raw bytes of an application key. preencode refuses any other length. The decoded value is a
// copy, so an async handler does not see the frame's bytes change under it.
const fixed32 = {
  preencode: c.fixed32.preencode,
  encode: c.fixed32.encode,
  decode(state) {
    return b4a.from(c.fixed32.decode(state))
  }
}

const LAN = struct([
  ['addresses', c.array(c.string)],
  ['port', c.uint]
])
const PORT = struct([
  ['service', c.string],
  ['port', c.uint]
])
const NAT = struct([
  ['host', c.string],
  ['port', c.uint],
  ['firewalled', bool],
  ['randomized', bool]
])
const VPN_SERVICE = struct([
  ['host', c.string],
  ['service', c.string],
  ['address', c.string],
  ['port', c.uint]
])
const VPN_ADDRESS = struct([
  ['host', c.string],
  ['service', c.string],
  ['address', c.string],
  ['name', c.string]
])
const NONE = struct([])

// The message table, as spec/ipc.md lists it. Requests are 1 to 99 and come from Dart; events are 100
// and up and come from the engine. Reply is type 0. Numbers are append-only.
const REPLY = {
  type: 0,
  name: 'reply',
  enc: struct([
    ['ok', bool],
    ['code', c.string],
    ['detail', c.string],
    ['route', c.string],
    ['services', c.array(c.string)],
    ['ports', c.array(PORT)]
  ])
}

const REQUESTS = [
  {
    type: 1,
    name: 'connect',
    enc: struct([
      ['host', c.string],
      ['key', c.string],
      ['appKey', fixed32],
      ['lan', LAN],
      ['ports', c.array(PORT)],
      ['bind', c.string]
    ])
  },
  { type: 2, name: 'close', enc: struct([['host', c.string]]) },
  { type: 3, name: 'status', enc: struct([['host', c.string]]) },
  {
    type: 4,
    name: 'relay',
    enc: struct([
      ['key', c.string],
      ['appKey', fixed32]
    ])
  },
  { type: 5, name: 'handoff.listen', enc: NONE },
  { type: 6, name: 'handoff.cancel', enc: NONE },
  {
    type: 7,
    name: 'handoff.send',
    enc: struct([
      ['link', c.string],
      ['name', c.string],
      ['key', c.string],
      ['appKey', fixed32]
    ])
  },
  {
    type: 8,
    name: 'vpn.start',
    enc: struct([
      ['services', c.array(VPN_SERVICE)],
      ['dnsUpstream', c.array(c.string)]
    ])
  },
  { type: 9, name: 'vpn.stop', enc: NONE }
]

const EVENTS = [
  {
    type: 100,
    name: 'route',
    enc: struct([
      ['host', c.string],
      ['route', c.string]
    ])
  },
  {
    type: 101,
    name: 'session',
    enc: struct([
      ['host', c.string],
      ['up', bool]
    ])
  },
  {
    type: 102,
    name: 'status',
    enc: struct([
      ['host', c.string],
      ['route', c.string],
      ['sessions', c.uint],
      ['streams', c.uint],
      ['flows', c.uint],
      ['bytesIn', c.uint],
      ['bytesOut', c.uint],
      ['nat', NAT]
    ])
  },
  {
    type: 103,
    name: 'vpn',
    enc: struct([
      ['port', c.uint],
      ['addresses', c.array(VPN_ADDRESS)]
    ])
  },
  {
    type: 104,
    name: 'services',
    enc: struct([
      ['host', c.string],
      ['list', c.array(c.string)],
      ['ports', c.array(PORT)]
    ])
  },
  {
    type: 105,
    name: 'reject',
    enc: struct([
      ['host', c.string],
      ['service', c.string],
      ['code', c.string],
      ['reason', c.string]
    ])
  },
  {
    type: 106,
    name: 'error',
    enc: struct([
      ['code', c.string],
      ['detail', c.string]
    ])
  },
  { type: 107, name: 'handoff.code', enc: struct([['link', c.string]]) },
  {
    type: 108,
    name: 'handoff.received',
    enc: struct([
      ['name', c.string],
      ['key', c.string],
      ['appKey', fixed32]
    ])
  },
  {
    type: 109,
    name: 'lan',
    enc: struct([
      ['host', c.string],
      ['addresses', c.array(c.string)],
      ['port', c.uint]
    ])
  }
]

const BY_TYPE = new Map([REPLY, ...REQUESTS, ...EVENTS].map((m) => [m.type, m]))

// Events have id 0. A reply and a request have the nonzero id that pairs them.
function checkId(type, id) {
  if ((type >= 100) !== (id === 0)) throw new Error('id does not match the message kind')
}

// The message a name and id stand for on the wire. The name status is both a request and an event,
// so the id tells them apart.
function messageFor(name, id) {
  if (name === 'reply') return REPLY
  const found = (id === 0 ? EVENTS : REQUESTS).find((m) => m.name === name)
  if (!found) throw new Error(`no ${id === 0 ? 'event' : 'request'} named ${name}`)
  return found
}

// encode({ name, id, body }) returns the frame as a Buffer.
function encode(msg) {
  const m = messageFor(msg.name, msg.id)
  checkId(m.type, msg.id)
  return b4a.concat([c.encode(c.uint, m.type), c.encode(c.uint, msg.id), c.encode(m.enc, msg.body)])
}

// Reads one whole frame. It throws on anything that does not decode exactly: a type no message has,
// an id that does not fit the type, a short frame, or bytes left over after the body.
function readFrame(buf) {
  if (!ArrayBuffer.isView(buf)) throw new Error('a frame is bytes')
  const state = c.state(0, buf.byteLength, buf)
  const type = c.uint.decode(state)
  const id = c.uint.decode(state)
  const m = BY_TYPE.get(type)
  if (!m) throw new Error(`no message has type ${type}`)
  checkId(type, id)
  const body = m.enc.decode(state)
  if (state.start !== state.end) throw new Error('bytes after the body')
  return { type, id, name: m.name, body }
}

// decode(buf) returns { name, id, body } and throws on a frame that does not decode exactly.
function decode(buf) {
  const { name, id, body } = readFrame(buf)
  return { name, id, body }
}

// createServer({ send(frame), handlers }) returns { receive(frame), emit(name, body) }.
//
// receive decodes one request from Dart, calls handlers[name](body) and sends the reply with the same
// id. A handler's result fills the connect reply's route, services and ports; any other handler
// resolves to nothing. An error with an HB- code (an HbError) becomes a reply with ok false and that
// code, and its reason as detail. Any other error propagates out of receive, since no catalog code
// names it. A frame that does not decode, a type that is not a request, or a request with no handler
// sends an HB-IPC-DESYNC error event instead.
//
// emit sends an event, with id 0.
function createServer({ send, handlers }) {
  function sendDesync() {
    send(encode({ name: 'error', id: 0, body: { code: 'HB-IPC-DESYNC', detail: 'malformed or unknown frame' } }))
  }

  async function receive(frame) {
    let msg
    try {
      msg = readFrame(frame)
    } catch (err) {
      sendDesync()
      return
    }
    const handler = msg.type >= 1 && msg.type <= 99 ? handlers[msg.name] : undefined
    if (typeof handler !== 'function') {
      sendDesync()
      return
    }

    let reply
    try {
      const r = (await handler(msg.body)) || {}
      reply = { ok: true, code: '', detail: '', route: r.route || '', services: r.services || [], ports: r.ports || [] }
    } catch (err) {
      if (!(err && typeof err.code === 'string' && err.code.startsWith('HB-'))) throw err
      const reason = typeof err.reason === 'string' ? err.reason : ''
      reply = { ok: false, code: err.code, detail: reason, route: '', services: [], ports: [] }
    }
    send(encode({ name: 'reply', id: msg.id, body: reply }))
  }

  function emit(name, body) {
    send(encode({ name, id: 0, body }))
  }

  return { receive, emit }
}

module.exports = { encode, decode, createServer }
