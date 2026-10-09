'use strict'

// Spike step 4: a plain TCP secret-stream (the LAN route) has no unordered path.
// Two secret streams run over loopback TCP. Each side calls send and trySend and
// counts 'message' events for a window. A control ordered write proves the link
// works. Run under Node it writes results-tcp.txt next to this file, and under bare
// (node_modules/.bin/bare tcp.js) it prints the same JSON lines to stdout. Loopback only.

// Under bare the LAN route uses bare-tcp, the same socket type the app engine gets.
const isBare = typeof Bare !== 'undefined'
const net = isBare ? require('bare-tcp') : require('net')
const SecretStream = require('@hyperswarm/secret-stream')
const b4a = require('b4a')

const WINDOW_MS = 1500
const lines = []
function emit(obj) {
  const text = JSON.stringify(obj)
  lines.push(text)
  console.log(text)
}
function exit(code) {
  if (isBare) Bare.exit(code)
  else process.exit(code)
}

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms))
}

function once(stream, event) {
  return new Promise((resolve) => stream.once(event, resolve))
}

async function main() {
  const server = net.createServer()
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))
  const port = server.address().port

  const serverSide = new Promise((resolve) => {
    server.once('connection', (socket) => {
      const s = new SecretStream(false, socket)
      resolve({ s, socket })
    })
  })

  const clientSock = net.connect(port, '127.0.0.1')
  const client = new SecretStream(true, clientSock)
  const { s: srv, socket: srvSocket } = await serverSide
  await Promise.all([once(client, 'connect'), once(srv, 'connect')])

  emit({
    ev: 'setup',
    transport: (isBare ? 'bare-tcp' : 'node net') + ' socket over loopback TCP',
    runtime: isBare ? 'bare ' + Bare.version : 'node ' + process.version,
    rawStreamIsUdx: typeof clientSock.send === 'function' || typeof clientSock.trySend === 'function',
    rawStreamHasSend: typeof clientSock.send,
    rawStreamHasTrySend: typeof clientSock.trySend,
    secretStreamSendIsFunction: typeof client.send,
    secretStreamTrySendIsFunction: typeof client.trySend
  })

  const counts = { clientMessages: 0, serverMessages: 0, clientData: 0, serverData: 0 }
  client.on('message', () => counts.clientMessages++)
  srv.on('message', () => counts.serverMessages++)
  client.on('data', (d) => (counts.clientData += d.length))
  srv.on('data', (d) => (counts.serverData += d.length))

  const returned = []
  for (let i = 0; i < 4; i++) {
    returned.push(['client.send', typeof client.send(b4a.alloc(100, i + 1))])
    returned.push(['client.trySend', typeof client.trySend(b4a.alloc(100, i + 1))])
    returned.push(['server.send', typeof srv.send(b4a.alloc(100, i + 1))])
    returned.push(['server.trySend', typeof srv.trySend(b4a.alloc(100, i + 1))])
  }
  const returnTypes = {}
  for (const [name, type] of returned) {
    returnTypes[name] = returnTypes[name] || new Set()
    returnTypes[name].add(type)
  }
  emit({
    ev: 'unordered-calls',
    sent: { perSide: 8, sizeBytes: 100 },
    returnTypes: Object.fromEntries(Object.entries(returnTypes).map(([k, v]) => [k, [...v]]))
  })

  await sleep(WINDOW_MS)
  emit({ ev: 'unordered-window', windowMs: WINDOW_MS, ...counts })

  client.write(b4a.from('control ordered write'))
  await sleep(300)
  emit({ ev: 'ordered-control', serverDataBytes: counts.serverData })

  client.destroy()
  srv.destroy()
  clientSock.destroy()
  srvSocket.destroy()
  server.close()
  emit({ ev: 'done' })

  const verdict =
    counts.clientMessages === 0 && counts.serverMessages === 0 && counts.serverData > 0
      ? 'NO unordered path on plain TCP: message events 0 both ways, ordered control arrived'
      : 'UNEXPECTED: see counts above'
  emit({ ev: 'verdict', text: verdict })
  if (!isBare) {
    const path = require('path')
    require('fs').writeFileSync(path.join(__dirname, 'results-tcp.txt'), lines.join('\n') + '\n')
  }
  exit(0)
}

main().catch((err) => {
  emit({ ev: 'fatal', message: String(err && err.stack ? err.stack : err) })
  exit(1)
})
