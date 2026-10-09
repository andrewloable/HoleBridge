// Checks every tracked Markdown link: a relative target must exist, and a #fragment into a
// Markdown file must match a heading there (GitHub's slug rule). Usage: node scripts/check-docs.js
'use strict';

const fs = require('node:fs');
const path = require('node:path');
const { execFileSync } = require('node:child_process');

const ROOT = path.resolve(__dirname, '..');
const FENCE = /^ {0,3}(`{3,}|~{3,})(.*)$/;

// GitHub's heading anchor: link markup reduced to its text, lowercase, drop anything but letters,
// digits, spaces, hyphens and underscores, then each space becomes a hyphen.
function slugify(text) {
  return text
    .replace(/\[([^\]]*)\]\([^)]*\)/g, '$1')
    .toLowerCase()
    .replace(/[^\p{L}\p{N} _-]/gu, '')
    .replace(/ /g, '-');
}

// Lines outside fenced code blocks, as [line number, text].
function proseLines(lines) {
  const out = [];
  let fence = null;
  lines.forEach((text, i) => {
    const m = text.match(FENCE);
    if (fence) {
      if (m && m[1][0] === fence[0] && m[1].length >= fence.length && m[2].trim() === '') fence = null;
    } else if (m && !(m[1][0] === '`' && m[2].includes('`'))) {
      fence = m[1];
    } else {
      out.push([i + 1, text]);
    }
  });
  return out;
}

// Anchors a file offers; repeated headings get -1, -2 like GitHub.
function anchorsOf(lines) {
  const seen = new Map();
  const anchors = new Set();
  for (const [, text] of proseLines(lines)) {
    const h = text.match(/^ {0,3}#{1,6}[ \t]+(.*?)(?:[ \t]+#+)?[ \t]*$/);
    if (!h) continue;
    const base = slugify(h[1]);
    const n = seen.get(base) || 0;
    seen.set(base, n + 1);
    anchors.add(n ? `${base}-${n}` : base);
  }
  return anchors;
}

// Link targets on one line of prose: inline [text](target) and reference "[label]: target".
// Code spans are blanked first so a link written as code is not checked.
function targetsOf(text) {
  const bare = text.replace(/(`+).*?\1/g, (m) => ' '.repeat(m.length));
  const found = [...bare.matchAll(/\]\(\s*(<[^>]*>|[^)\s]+)/g)].map((m) => m[1]);
  const ref = bare.match(/^ {0,3}\[[^\]]+\]:\s*(<[^>]*>|\S+)/);
  if (ref) found.push(ref[1]);
  return found.map((t) => t.replace(/^<(.*)>$/, '$1'));
}

const decode = (s) => {
  try {
    return decodeURIComponent(s);
  } catch {
    return s;
  }
};

// files: paths relative to root. Returns "file:line: message" strings, empty when all resolve.
function check(root, files) {
  const problems = [];
  const anchorCache = new Map();
  const anchorsIn = (abs) => {
    if (!anchorCache.has(abs)) anchorCache.set(abs, anchorsOf(fs.readFileSync(abs, 'utf8').split('\n')));
    return anchorCache.get(abs);
  };
  for (const rel of files) {
    const abs = path.join(root, rel);
    for (const [line, text] of proseLines(fs.readFileSync(abs, 'utf8').split('\n'))) {
      for (const target of targetsOf(text)) {
        if (/^[a-z][a-z0-9+.-]*:/i.test(target)) continue; // http(s), mailto and other URLs
        const hash = target.indexOf('#');
        const rawPath = hash === -1 ? target : target.slice(0, hash);
        const fragment = hash === -1 ? '' : decode(target.slice(hash + 1));
        const base = rawPath.startsWith('/') ? root : path.dirname(abs);
        const dest = rawPath === '' ? abs : path.resolve(base, decode(rawPath).replace(/^\/+/, ''));
        const where = `${rel}:${line}`;
        if (!fs.existsSync(dest)) {
          problems.push(`${where}: broken link ${target}: no such file ${path.relative(root, dest)}`);
        } else if (fragment && /\.md$/i.test(dest) && !anchorsIn(dest).has(fragment)) {
          problems.push(`${where}: broken anchor ${target}: no heading with that slug in ${path.relative(root, dest)}`);
        }
      }
    }
  }
  return problems;
}

function trackedMarkdown() {
  return execFileSync('git', ['ls-files', '-z', '*.md'], { cwd: ROOT, encoding: 'utf8' })
    .split('\0')
    .filter((f) => f && !f.split('/').includes('node_modules'));
}

function main() {
  const files = trackedMarkdown();
  const problems = check(ROOT, files);
  for (const p of problems) console.log(p);
  if (problems.length) {
    process.exitCode = 1;
    return;
  }
  console.log(`${files.length} Markdown files: all relative links and anchors resolve`);
}

if (require.main === module) main();

module.exports = { check, slugify };
