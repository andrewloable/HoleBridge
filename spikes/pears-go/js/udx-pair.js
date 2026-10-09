'use strict'
// Spike step 4 baseline (throwaway): JS to JS over localhost with each end in its own process,
// the same topology as Go to JS. Runs udx-peer.js recv and send, picks both ports up front, and
// starts the sender only after the receiver is connected (GO on stdin). Usage:
//   node udx-pair.js <bytes> [runs]
// Prints one line per run and a summary. Every wait has a timeout.
const path = require('path')
const dgram = require('dgram')
const { spawn } = require('child_process')

const PEER = path.join(__dirname, 'udx-peer.js')
// The JS runtime for both peers: node by default, or the repo-local bare (UDX_RUNTIME=...).
const RUNTIME = process.env.UDX_RUNTIME || process.execPath
const TIMEOUT_MS = 120_000

function freePort() {
  return new Promise((resolve, reject) => {
    const s = dgram.createSocket('udp4')
    s.once('error', reject)
    s.bind(0, '127.0.0.1', () => {
      const port = s.address().port
      s.close(() => resolve(port))
    })
  })
}

function runOnce(bytes) {
  return new Promise(async (resolve, reject) => {
    const recvPort = await freePort()
    const sendPort = await freePort()
    const recv = spawn(RUNTIME, [PEER, 'recv', String(recvPort), String(sendPort), '2002', '1001', String(bytes)], { stdio: ['ignore', 'pipe', 'inherit'] })
    const send = spawn(RUNTIME, [PEER, 'send', String(sendPort), String(recvPort), '1001', '2002', String(bytes)], { stdio: ['pipe', 'pipe', 'inherit'] })
    const timer = setTimeout(() => {
      recv.kill('SIGKILL')
      send.kill('SIGKILL')
      reject(new Error('udx-pair timed out'))
    }, TIMEOUT_MS)
    let recvLine = null
    let sendLine = null
    let rbuf = ''
    let sbuf = ''
    let sentGo = false
    recv.stdout.on('data', (d) => {
      rbuf += d
      let i
      while ((i = rbuf.indexOf('\n')) >= 0) {
        const line = rbuf.slice(0, i)
        rbuf = rbuf.slice(i + 1)
        if (line.startsWith('READY') && !sentGo) {
          sentGo = true
          send.stdin.write('GO\n')
        }
        if (line.startsWith('RECV')) recvLine = line
      }
    })
    send.stdout.on('data', (d) => {
      sbuf += d
      let i
      while ((i = sbuf.indexOf('\n')) >= 0) {
        const line = sbuf.slice(0, i)
        sbuf = sbuf.slice(i + 1)
        if (line.startsWith('SENT')) sendLine = line
      }
    })
    // The sender waits for the receiver's READY. The receiver is already bound and connected
    // when it prints READY, so the GO goes out after that. The receiver prints READY once it is
    // connected to the sender port, which is fixed up front.
    let rc = null
    let sc = null
    recv.on('exit', (code) => { rc = code; maybe() })
    send.on('exit', (code) => { sc = code; maybe() })
    function maybe() {
      if (rc === null || sc === null) return
      clearTimeout(timer)
      if (rc !== 0 || sc !== 0 || !recvLine || !sendLine) return reject(new Error(`pair failed rc=${rc} sc=${sc}`))
      resolve({ recvLine, sendLine })
    }
  })
}

async function main() {
  const bytes = Number(process.argv[2] || 104857600)
  const runs = Number(process.argv[3] || 3)
  const results = []
  for (let i = 0; i < runs; i++) {
    const r = await runOnce(bytes)
    const span = Number(/span_ms=([\d.]+)/.exec(r.recvLine)[1])
    const digestOK = /digest=(\w+)/.exec(r.recvLine)[1] === /digest=(\w+)/.exec(r.sendLine)[1]
    const mbit = (bytes * 8) / (span / 1000) / 1e6
    console.log(`js-pair run=${i + 1} recv_span_ms=${span} mbit_s=${mbit.toFixed(1)} digest_match=${digestOK}`)
    results.push(mbit)
  }
  results.sort((a, b) => a - b)
  console.log(`js-pair median_mbit_s=${results[Math.floor(results.length / 2)].toFixed(1)} min=${results[0].toFixed(1)} max=${results[results.length - 1].toFixed(1)}`)
}

main().catch((err) => {
  console.error('udx-pair failed:', err.message)
  process.exitCode = 1
})
