const test = require('brittle')
const b4a = require('b4a')
const { load, hex } = require('./helpers/vectors.js')
const { encode, decode, createServer } = require('../lib/ipc.js')

const { vectors } = load('ipc.json')

// brittle ends the whole run when a test body throws. Until the IMPL task lands, the code throws
// 'not implemented', so each body runs under catchThrows, which fails only its own test. Async
// bodies are awaited so that a rejected promise also fails only its own test.
function catchThrows(fn) {
  return async (t) => {
    try {
      await fn(t)
    } catch (err) {
      t.fail(err.message)
    }
  }
}

// A vector name is the message name and an example number, such as 'connect 1' or 'handoff.send 2'.
function messageName(vector) {
  return vector.name.replace(/ \d+$/, '')
}

function vectorNamed(name) {
  const found = vectors.find((v) => v.name === name)
  if (!found) throw new Error(`no vector named ${name} in spec/vectors/ipc.json`)
  return found
}

// The vector JSON writes a fixed32 (the application key) as 64 hex digits. The engine body carries
// the 32 raw bytes, so the test converts each appKey before comparing.
function fromJson(value) {
  if (Array.isArray(value)) return value.map(fromJson)
  if (value === null || typeof value !== 'object') return value
  const out = {}
  for (const [key, field] of Object.entries(value)) {
    out[key] = key === 'appKey' ? hex(field) : fromJson(field)
  }
  return out
}

// Same shape as HbError in lib/keys.js, which does not export it: a code and a reason.
class HbError extends Error {
  constructor(code, reason) {
    super(`${code}: ${reason}`)
    this.name = 'HbError'
    this.code = code
    this.reason = reason
  }
}

// The body of the one reply to request id among the frames sent so far.
function replyTo(sent, id) {
  const replies = sent.map((frame) => decode(frame)).filter((msg) => msg.name === 'reply' && msg.id === id)
  if (replies.length !== 1) throw new Error(`expected one reply to id ${id}, got ${replies.length}`)
  return replies[0].body
}

test('encode gives the vector frame for every entry in ipc.json', catchThrows((t) => {
  for (const v of vectors) {
    const frame = encode({ name: messageName(v), id: v.id, body: fromJson(v.value) })
    t.is(b4a.toString(frame, 'hex'), v.hex, `${v.name} encodes to its frame`)
  }
}))

test('decode gives the vector message for every entry in ipc.json', catchThrows((t) => {
  for (const v of vectors) {
    const msg = decode(hex(v.hex))
    t.is(msg.name, messageName(v), `${v.name} has its message name`)
    t.is(msg.id, v.id, `${v.name} keeps its id`)
    t.alike(msg.body, fromJson(v.value), `${v.name} body decodes to the vector value`)
  }
}))

test('a connect request calls handlers.connect and replies ok with the same id', catchThrows(async (t) => {
  const request = vectorNamed('connect 1')
  const sent = []
  let received
  const server = createServer({
    send: (frame) => sent.push(frame),
    handlers: {
      connect: async (body) => {
        received = body
        return {
          route: 'direct',
          services: ['web', 'ssh'],
          ports: [{ service: 'web', port: 8080 }, { service: 'ssh', port: 22 }],
        }
      },
    },
  })
  await server.receive(hex(request.hex))
  t.alike(received, fromJson(request.value), 'handlers.connect gets the connect body')
  t.alike(replyTo(sent, request.id), {
    ok: true,
    code: '',
    detail: '',
    route: 'direct',
    services: ['web', 'ssh'],
    ports: [{ service: 'web', port: 8080 }, { service: 'ssh', port: 22 }],
  }, 'the reply is ok, has the same id, and carries route, services and ports')
}))

test('a handler that throws HB-LOOKUP-TIMEOUT replies ok false with that code', catchThrows(async (t) => {
  const request = vectorNamed('connect 1')
  const sent = []
  const server = createServer({
    send: (frame) => sent.push(frame),
    handlers: {
      connect: async () => {
        throw new HbError('HB-LOOKUP-TIMEOUT', 'no answer within 60 s')
      },
    },
  })
  await server.receive(hex(request.hex))
  const reply = replyTo(sent, request.id)
  t.is(reply.ok, false, 'ok is false')
  t.is(reply.code, 'HB-LOOKUP-TIMEOUT', 'the code is the one the handler threw')
  t.is(typeof reply.detail, 'string', 'detail is text')
}))

test('a truncated frame sends an HB-IPC-DESYNC error event and does not throw', catchThrows(async (t) => {
  const frame = hex(vectorNamed('connect 1').hex)
  const truncated = frame.subarray(0, frame.length - 3)
  const sent = []
  const server = createServer({ send: (f) => sent.push(f), handlers: {} })
  let thrown = null
  try {
    await server.receive(truncated)
  } catch (err) {
    thrown = err
  }
  t.is(thrown, null, 'receive does not throw')
  const errors = sent.map((f) => decode(f)).filter((msg) => msg.name === 'error')
  t.is(errors.length, 1, 'one error event is sent')
  t.is(errors[0].id, 0, 'the error is an event with id 0')
  t.is(errors[0].body.code, 'HB-IPC-DESYNC', 'the code is HB-IPC-DESYNC')
}))

test('an unknown type number sends an HB-IPC-DESYNC error event', catchThrows(async (t) => {
  const sent = []
  const server = createServer({ send: (f) => sent.push(f), handlers: {} })
  await server.receive(b4a.from([50, 1]))
  const errors = sent.map((f) => decode(f)).filter((msg) => msg.name === 'error')
  t.is(errors.length, 1, 'one error event is sent')
  t.is(errors[0].body.code, 'HB-IPC-DESYNC', 'the code is HB-IPC-DESYNC')
}))

test('emit sends an event with id 0 that decodes back to the event', catchThrows(async (t) => {
  const event = vectorNamed('route 1')
  const sent = []
  const server = createServer({ send: (frame) => sent.push(frame), handlers: {} })
  await server.emit('route', fromJson(event.value))
  t.is(sent.length, 1, 'one frame is sent')
  const msg = decode(sent[0])
  t.is(msg.name, 'route', 'the name is route')
  t.is(msg.id, 0, 'events have id 0')
  t.alike(msg.body, fromJson(event.value), 'the body decodes back to the event')
}))
