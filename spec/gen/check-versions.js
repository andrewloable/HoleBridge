// Exits 1 unless every Holepunch package the generators use resolves to the same version here
// (spec/gen/package-lock.json) as in the app engine (app/engine/package-lock.json).
const fs = require('fs')
const path = require('path')

const PACKAGES = [
  'hyperdht',
  'compact-encoding',
  'sodium-universal',
  'protomux',
  '@hyperswarm/secret-stream',
  'udx-native',
  'dht-rpc',
  'noise-handshake',
  'noise-curve-ed',
  'blind-relay',
  'kademlia-routing-table',
]

// Top-level resolved versions from a lockfile; a missing package maps to undefined.
function resolved(lockPath) {
  const lock = JSON.parse(fs.readFileSync(lockPath, 'utf8'))
  const versions = {}
  for (const name of PACKAGES) {
    const entry = lock.packages[`node_modules/${name}`]
    versions[name] = entry && entry.version
  }
  return versions
}

const here = resolved(path.join(__dirname, 'package-lock.json'))
const engine = resolved(path.join(__dirname, '..', '..', 'app', 'engine', 'package-lock.json'))

const bad = []
for (const name of PACKAGES) {
  if (!engine[name]) bad.push(`${name}: missing from app/engine/package-lock.json`)
  else if (here[name] !== engine[name]) {
    bad.push(`${name}: spec/gen ${here[name] || 'missing'}, app/engine ${engine[name]}`)
  }
}

if (bad.length) {
  console.error('spec/gen versions differ from app/engine/package-lock.json:')
  for (const line of bad) console.error('  ' + line)
  process.exit(1)
}
console.log(`All ${PACKAGES.length} pinned packages match app/engine/package-lock.json.`)
