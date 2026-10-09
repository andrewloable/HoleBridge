'use strict';

const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { check, slugify } = require('./check-docs.js');

// Writes the files into a temp repo, checks them all, and removes the repo again.
function run(files) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'check-docs-'));
  try {
    for (const [name, text] of Object.entries(files)) {
      fs.mkdirSync(path.dirname(path.join(root, name)), { recursive: true });
      fs.writeFileSync(path.join(root, name), text);
    }
    return check(root, Object.keys(files));
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
}

test('slugify follows the GitHub rule, em dash included', () => {
  assert.equal(slugify('Setup — quick start'), 'setup--quick-start');
  assert.equal(slugify('VPN mode (Android and iOS)'), 'vpn-mode-android-and-ios');
  assert.equal(slugify('Use `go get` and [docs](x.md)'), 'use-go-get-and-docs');
});

test('good links, fragments and external URLs pass', () => {
  const problems = run({
    'a.md': '# A\n\n[b](b.md#setup--quick-start) [same](#a) [web](https://example.com/x) [mail](mailto:x@example.com)\n',
    'b.md': '# B\n\n## Setup — quick start\n\n## Usage\n\n## Usage\n\n[up](a.md#a) [again](#usage-1)\n',
  });
  assert.deepEqual(problems, []);
});

test('a missing file is reported with file and line', () => {
  const problems = run({
    'a.md': 'intro\n\n[gone](nope.md)\n',
  });
  assert.equal(problems.length, 1);
  assert.match(problems[0], /^a\.md:3: broken link nope\.md: no such file nope\.md$/);
});

test('a bad anchor is reported', () => {
  const problems = run({
    'a.md': '[b](b.md#missing)\n',
    'b.md': '## Real heading\n',
  });
  assert.equal(problems.length, 1);
  assert.match(problems[0], /^a\.md:1: broken anchor b\.md#missing: /);
});

test('an em dash heading is matched only by its GitHub slug', () => {
  const problems = run({
    'a.md': '[ok](b.md#setup--quick-start) [wrong](b.md#setup-quick-start)\n',
    'b.md': '## Setup — quick start\n',
  });
  assert.equal(problems.length, 1);
  assert.match(problems[0], /broken anchor b\.md#setup-quick-start/);
});

test('headings inside code fences do not count as anchors', () => {
  const problems = run({
    'a.md': '[x](b.md#not-a-heading)\n',
    'b.md': '```\n# Not a heading\n```\n',
  });
  assert.equal(problems.length, 1);
  assert.match(problems[0], /broken anchor b\.md#not-a-heading/);
});
