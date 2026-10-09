// Checks that THIRD_PARTY_NOTICES.md names every pinned third-party package at its exact version:
// each module in go.mod, each non-dev package in the app engine, spec/gen and interop/js lockfiles,
// and each hosted and git package in app/pubspec.lock. Exits 1 and lists what is missing.
// Usage: node scripts/check-notices.js
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

// Hosted and git packages from a pub lockfile, as {name, version, from}. SDK packages (Flutter SDK)
// and path packages (code in this repo) are skipped; hosted and git packages are required.
// pubspec.lock does not record which transitive packages are dev-only (the npm lockfiles do), so
// every hosted and git package is required: the dev-only ones are listed under "not shipped" in
// THIRD_PARTY_NOTICES.md.
function pubPackages(file) {
  const out = [];
  let cur = null;
  for (const line of fs.readFileSync(file, 'utf8').split('\n')) {
    let m = line.match(/^ {2}(\S+):$/);
    if (m) {
      cur = { name: m[1] };
      out.push(cur);
    } else if (cur && (m = line.match(/^ {4}source: (\S+)$/))) {
      cur.source = m[1];
    } else if (cur && (m = line.match(/^ {4}version: "(.+)"$/))) {
      cur.version = m[1];
    }
  }
  return out
    .filter((p) => p.source === 'hosted' || p.source === 'git')
    .map(({ name, version }) => ({ name, version, from: path.relative(ROOT, file) }));
}

// A table row names the package and its version in its first two cells.
const notices = fs.readFileSync(path.join(ROOT, 'THIRD_PARTY_NOTICES.md'), 'utf8').split('\n');
const listed = ({ name, version }) => notices.some((line) => line.startsWith(`| ${name} | ${version} |`));

const pins = [
  ...goModules(path.join(ROOT, 'go.mod')),
  ...npmPackages(path.join(ROOT, 'app/engine/package-lock.json')),
  ...npmPackages(path.join(ROOT, 'spec/gen/package-lock.json')),
  ...npmPackages(path.join(ROOT, 'interop/js/package-lock.json')),
  ...pubPackages(path.join(ROOT, 'app/pubspec.lock')),
];

const missing = pins.filter((pin) => !listed(pin));
if (missing.length) {
  console.error('check-notices: THIRD_PARTY_NOTICES.md has no row for these pinned packages:');
  for (const pin of missing) console.error(`  ${pin.name} ${pin.version} (${pin.from})`);
  process.exit(1);
}
console.log(`check-notices: all ${pins.length} pinned packages are listed.`);
