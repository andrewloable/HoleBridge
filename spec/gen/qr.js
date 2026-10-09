// Writes spec/vectors/qr.json: the QR Code symbols that the Go encoder in internal/qr must
// reproduce. Each symbol comes from the npm qrcode package (MIT), in byte mode, with the version
// and the mask chosen automatically. A module matrix is one string per row, '1' for a dark module,
// with x the column and y the row. The byte capacity of version 40 at each level is found by
// search with qrcode itself.
//
// Run: cd spec/gen && npm ci && node qr.js
// qrcode must be pinned exactly in package.json so that npm ci installs it.

const fs = require('fs')
const path = require('path')
const QRCode = require('qrcode')

// Test values only. A 9-symbol key and a 64-digit lowercase hex application key make the 99-character
// key link that the host prints as a QR code (docs/security.md, the application key).
const KEY_LINK = 'https://holebridge.app/k#7KQM4X9TR.' + '0123456789abcdef'.repeat(4)

const TEXTS = [
  'HB',
  '7KQ-M4X-9TR',
  'https://holebridge.app/k#7KQM4X9TR',
  KEY_LINK,
  'The quick brown fox jumps over the lazy dog',
  '0123456789'.repeat(4),
  'hello, world',
  'a',
  'https://holebridge.app/h#7KQM4X9TR',
  'ABCDEFGHIJ'.repeat(30),
]

// One symbol. The segment is forced to byte mode so the output does not depend on qrcode's mode choice.
function symbol(text, level) {
  const qr = QRCode.create([{ data: text, mode: 'byte' }], { errorCorrectionLevel: level })
  // Mode indicator 0100 is byte mode in ISO/IEC 18004.
  if (qr.segments.length !== 1 || qr.segments[0].mode.bit !== 0b0100) throw new Error('segment is not byte mode')
  const size = qr.modules.size
  const rows = []
  for (let y = 0; y < size; y++) {
    let row = ''
    for (let x = 0; x < size; x++) row += qr.modules.get(y, x) ? '1' : '0'
    rows.push(row)
  }
  // ISO/IEC 18004 puts the always-dark module at column 8, row size - 8. This checks the orientation.
  if (rows[size - 8][8] !== '1') throw new Error('dark module is not at column 8, row size - 8')
  return { version: qr.version, mask: qr.maskPattern, modules: rows }
}

// The largest byte-mode text that fits version 40 at this level.
function byteCapacityV40(level) {
  const fits = (n) => {
    try {
      QRCode.create([{ data: 'A'.repeat(n), mode: 'byte' }], { errorCorrectionLevel: level, version: 40 })
      return true
    } catch (err) {
      return false
    }
  }
  let lo = 1
  let hi = 4096
  if (!fits(lo) || fits(hi)) throw new Error(`level ${level}: search bounds do not bracket the capacity`)
  while (hi - lo > 1) {
    const mid = (lo + hi) >> 1
    if (fits(mid)) lo = mid
    else hi = mid
  }
  return lo
}

function main() {
  if (!/^https:\/\/holebridge\.app\/k#[0-9ABCDEFGHJKMNPQRSTVWXYZ]{9}\.[0-9a-f]{64}$/.test(KEY_LINK)) throw new Error('key link is not in the documented format')

  const cases = TEXTS.map((text) => ({ text, ...symbol(text, 'M') }))
  const levels = { text: KEY_LINK }
  for (const level of ['L', 'Q', 'H']) levels[level] = symbol(KEY_LINK, level)

  const capacity = {}
  for (const level of ['L', 'M', 'Q', 'H']) capacity[level] = byteCapacityV40(level)

  const out = {
    generator: `npm qrcode ${require('qrcode/package.json').version}`,
    level: 'M',
    cases,
    levels,
    byte_capacity_v40: capacity,
  }
  const file = path.join(__dirname, '..', 'vectors', 'qr.json')
  fs.writeFileSync(file, JSON.stringify(out, null, 2) + '\n')
  console.log(`wrote ${cases.length} cases, 3 extra levels and the version 40 capacities to ${file}`)
}

main()
