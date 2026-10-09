// Checks that THIRD_PARTY_NOTICES.md names every pinned third-party package at its exact version:
// each module in go.mod, and each non-dev package in the app engine and spec/gen lockfiles. Exits 1
// and lists what is missing. Usage: node scripts/check-notices.js
'use strict';

const fs = require('node:fs');
const path = require('node:path');

const ROOT = path.resolve(__dirname, '..');

// Modules from the require directives of go.mod, as {name, version, from}.
function goModules(file) {
  const out = [];
  let inBlock = false;
  for (const raw of fs.readFileSync(file, 'utf8').split('\n')) {
    const line = raw.replace(/\/\/.*$/, '').trim();
    if (inBlock) {
      if (line === ')') inBlock = false;
      else if (line) out.push(line.split(/\s+/));
    } else if (line === 'require (') {
      inBlock = true;
    } else if (line.startsWith('require ')) {
      out.push(line.slice('require '.length).trim().split(/\s+/));
    }
  }
  return out.map(([name, version]) => ({ name, version, from: 'go.mod' }));
}

// Packages from an npm lockfile, as {name, version, from}. Dev-only packages are skipped.
function npmPackages(file) {
  const { packages } = JSON.parse(fs.readFileSync(file, 'utf8'));
  return Object.entries(packages)
    .filter(([p, m]) => p !== '' && !m.dev)
    .map(([p, m]) => ({
      name: p.replace(/^.*node_modules\//, ''),
      version: m.version,
      from: path.relative(ROOT, file),
    }));
}

// A table row names the package and its version in its first two cells.
const notices = fs.readFileSync(path.join(ROOT, 'THIRD_PARTY_NOTICES.md'), 'utf8').split('\n');
const listed = ({ name, version }) => notices.some((line) => line.startsWith(`| ${name} | ${version} |`));

const pins = [
  ...goModules(path.join(ROOT, 'go.mod')),
  ...npmPackages(path.join(ROOT, 'app/engine/package-lock.json')),
  ...npmPackages(path.join(ROOT, 'spec/gen/package-lock.json')),
];

const missing = pins.filter((pin) => !listed(pin));
if (missing.length) {
  console.error('check-notices: THIRD_PARTY_NOTICES.md has no row for these pinned packages:');
  for (const pin of missing) console.error(`  ${pin.name} ${pin.version} (${pin.from})`);
  process.exit(1);
}
console.log(`check-notices: all ${pins.length} pinned packages are listed.`);
