// Runs index.js as a bare process, the way the desktop host does, and checks the IPC channel on
// its stdin and stdout. Each frame is a 4-byte big-endian length and its bytes. The engine echoes
// every frame, so stdout must come back byte for byte equal to what was written. That also shows
// that nothing else reaches stdout. Run with node (npm test does): bare has no child_process to
// spawn it.
'use strict'

const { spawn } = require('node:child_process')
const path = require('node:path')

const root = path.join(__dirname, '..')
const bare = path.join(root, 'node_modules', '.bin', 'bare')

function frame(bytes) {
  const head = Buffer.alloc(4)
  head.writeUInt32BE(bytes.length, 0)
  return Buffer.concat([head, Buffer.from(bytes)])
}

const big = Array.from({ length: 70000 }, (_, i) => i % 251)
const input = Buffer.concat([frame([1, 2, 3]), frame([]), frame([9]), frame(big)])

// Writes split the stream in places that cut a length prefix and a frame body, and one frame
// (the big one) is longer than a single read.
const cuts = [0, 2, 3, 10, 4106, 34106, input.length]

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))

async function main() {
  // The bare script starts the runtime as its own child, so the child runs in its own process group
  // and the group is killed as a whole.
  const child = spawn(bare, [path.join(root, 'index.js')], {
    cwd: root,
    detached: true,
    stdio: ['pipe', 'pipe', 'pipe']
  })
  const out = []
  let received = 0
  let stderr = ''
  let full = null
  const complete = new Promise((resolve) => {
    full = resolve
  })
  child.stdout.on('data', (chunk) => {
    out.push(chunk)
    received += chunk.length
    if (received >= input.length) full()
  })
  child.stderr.on('data', (chunk) => {
    stderr += chunk
  })

  let timer = null
  const deadline = new Promise((_, reject) => {
    timer = setTimeout(() => {
      reject(new Error(`timed out: ${received} of ${input.length} bytes echoed; stderr: ${stderr}`))
    }, 10000)
  })
  try {
    for (let i = 1; i < cuts.length; i++) {
      child.stdin.write(input.subarray(cuts[i - 1], cuts[i]))
      await sleep(20)
    }
    await Promise.race([complete, deadline])
    // Any stray byte on stdout would show up as extra output after the echo.
    await sleep(200)
    const echoed = Buffer.concat(out)
    if (!echoed.equals(input)) {
      throw new Error(
        `stdout is not the echoed frames: ${echoed.length} bytes, want ${input.length}`
      )
    }
  } finally {
    clearTimeout(timer)
    try {
      process.kill(-child.pid, 'SIGKILL')
    } catch {
      // The group already exited, so there is nothing left to stop.
    }
  }
  console.log(`ok desktop IPC: ${input.length} bytes of frames echoed over stdin and stdout`)
}

main().catch((err) => {
  console.error(err.message)
  process.exit(1)
})
