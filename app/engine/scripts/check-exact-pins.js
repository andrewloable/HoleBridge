// Exits 1 unless every dependency and devDependency in package.json is an exact x.y.z version.
const pkg = require('../package.json')

const exact = /^\d+\.\d+\.\d+$/
const bad = []
for (const field of ['dependencies', 'devDependencies']) {
  for (const [name, version] of Object.entries(pkg[field] || {})) {
    if (!exact.test(version)) bad.push(`${field}.${name}: ${version}`)
  }
}

if (bad.length) {
  console.error('Not exact x.y.z pins (no ^, ~, ranges, tags or URLs):')
  for (const line of bad) console.error('  ' + line)
  process.exit(1)
}
console.log('All dependency and devDependency versions are exact pins.')
