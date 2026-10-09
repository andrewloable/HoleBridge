// Checks the static site in site/: every HTML page and _headers. Node only, no dependencies.
// Usage: node site/test/check.js   (exit 1 on any failure, 0 otherwise)
//
// A page fails when it loads anything from another origin (script, stylesheet, image, frame,
// form action), has a script with a src (inline it instead), uses a script API that can read the
// key-link fragment or send it (location, history, document.URL and the other address properties,
// window.name, window.open, fetch, XMLHttpRequest, sendBeacon, WebSocket, EventSource, Image, src
// or href assignments), or uses inline event handlers or style attributes, which the Content
// Security Policy blocks. The script check matches source text, so it is a tripwire for review and
// not a sandbox: the CSP is what keeps the fragment on the page. _headers must set default-src
// 'none', allow no source in any other directive except the sha256 hash of every inline script
// (script-src) and style (style-src) of every page, and set Referrer-Policy no-referrer. The /k and
// /h pages are copies of the home page. Placeholder store identifiers are reported as warnings.
'use strict'

const fs = require('fs')
const path = require('path')
const crypto = require('crypto')

const SITE = path.resolve(__dirname, '..')
const problems = []
const warnings = []
const rel = (file) => path.relative(SITE, file)

// Every file under dir, skipping node_modules and the test directory, which are not served.
function walk(dir) {
  const out = []
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const abs = path.join(dir, entry.name)
    if (entry.isDirectory()) {
      if (entry.name !== 'node_modules' && entry.name !== 'test') out.push(...walk(abs))
    } else {
      out.push(abs)
    }
  }
  return out
}

// A URL that names another origin: any scheme, or a protocol-relative //host.
const EXTERNAL = /^(?:[a-z][a-z0-9+.-]*:|\/\/)/i
const LOADERS = /<(script|link|img|iframe|frame|embed|object|source|video|audio|form|base)\b([^>]*)>/gi
const ATTRIBUTE = /\b(src|href|data|action|srcset|poster)\s*=\s*["']?([^"'\s>]+)/gi
const SCRIPT = /<script\b([^>]*)>([\s\S]*?)<\/script>/gi
const STYLE = /<style\b[^>]*>([\s\S]*?)<\/style>/gi
// Script access that can read the fragment or send it: bare names (location, history, window.open
// through open, the network APIs, Image), the address properties and src or href (as properties or
// as bracket strings), so a document.URL, document.baseURI or new Image().src edit fails the check.
const FORBIDDEN_IN_SCRIPT = /\b(fetch|XMLHttpRequest|sendBeacon|WebSocket|EventSource|importScripts|Worker|SharedWorker|Image|open|location|history)\b|\.\s*(URL|documentURI|baseURI|referrer|name|src|href)\b|\[\s*['"`](URL|documentURI|baseURI|referrer|name|src|href|location)['"`]\s*\]/
const STORE_LINKS = ['android', 'ios', 'desktop'].map(
  (store) => new RegExp(`<a\\s[^>]*data-store="${store}"[^>]*href="https://[^"]+"`, 'i'),
)

const sha256 = (text) => `sha256-${crypto.createHash('sha256').update(text, 'utf8').digest('base64')}`

// Checks one HTML page. Returns the hashes of its inline scripts and styles.
function checkPage(file, html) {
  const hashes = { script: [], style: [] }
  for (const [, name, attrs] of html.matchAll(LOADERS)) {
    for (const [, attr, value] of attrs.matchAll(ATTRIBUTE)) {
      if (EXTERNAL.test(value)) problems.push(`${rel(file)}: <${name} ${attr}="${value}"> loads from another origin`)
    }
  }
  for (const [, tag, attrs] of html.matchAll(/<(script|style|a|link|img|iframe)\b([^>]*)>/gi)) {
    if (/\son[a-z]+\s*=/i.test(attrs)) problems.push(`${rel(file)}: <${tag}> has an inline event handler`)
    if (/\sstyle\s*=/i.test(attrs)) problems.push(`${rel(file)}: <${tag}> has a style attribute`)
  }
  for (const [, attrs, body] of html.matchAll(SCRIPT)) {
    if (/\bsrc\s*=/i.test(attrs)) {
      problems.push(`${rel(file)}: a <script src> is not allowed; inline the script and hash it`)
      continue
    }
    const hit = body.match(FORBIDDEN_IN_SCRIPT)
    if (hit) problems.push(`${rel(file)}: script uses ${hit[0]}, which the page must not use`)
    hashes.script.push(sha256(body))
  }
  for (const [, body] of html.matchAll(STYLE)) {
    if (/@import|url\(\s*["']?(?:[a-z]+:|\/\/)/i.test(body)) {
      problems.push(`${rel(file)}: a style loads from another origin`)
    }
    hashes.style.push(sha256(body))
  }
  for (const re of STORE_LINKS) {
    if (!re.test(html)) problems.push(`${rel(file)}: a store link is missing from the static HTML`)
  }
  if (!/after installing, scan or open this link again/i.test(html)) {
    problems.push(`${rel(file)}: the 'after installing, scan or open this link again' line is missing`)
  }
  if (/REPLACE_WITH_/.test(html)) warnings.push(`${rel(file)}: placeholder store identifiers (owner decision)`)
  return hashes
}

function checkHeaders(file, hashes) {
  if (!fs.existsSync(file)) {
    problems.push('_headers is missing')
    return
  }
  const text = fs.readFileSync(file, 'utf8')
  const csp = text.match(/^\s*Content-Security-Policy:\s*(.+)$/im)
  if (!csp) {
    problems.push('_headers has no Content-Security-Policy')
  } else {
    const dirs = {}
    for (const part of csp[1].split(';').map((s) => s.trim()).filter(Boolean)) {
      const [name, ...sources] = part.split(/\s+/)
      dirs[name] = sources
    }
    const want = { 'default-src': "'none'", 'frame-ancestors': "'none'", 'base-uri': "'none'", 'form-action': "'none'" }
    for (const [name, value] of Object.entries(want)) {
      if (!(dirs[name] || []).includes(value)) problems.push(`_headers CSP: ${name} must be ${value}`)
    }
    // Only script-src and style-src may name sources, and only the hashes of the pages' inline
    // scripts and styles. Every other directive must be 'none' (or empty), so no image, connection,
    // frame or font origin is allowed; img-src in particular must not be 'self'.
    for (const [name, sources] of Object.entries(dirs)) {
      if (name === 'script-src' || name === 'style-src') continue
      if (sources.length && (sources.length !== 1 || sources[0] !== "'none'")) {
        problems.push(`_headers CSP: ${name} must be 'none', found ${sources.join(' ')}`)
      }
    }
    for (const [name, list] of [['script-src', hashes.script], ['style-src', hashes.style]]) {
      const got = dirs[name] || []
      const expected = [...new Set(list.map((hash) => `'${hash}'`))]
      for (const src of expected) if (!got.includes(src)) problems.push(`_headers CSP: ${name} lacks ${src}`)
      for (const src of got) if (!expected.includes(src)) problems.push(`_headers CSP: ${name} allows ${src}, which no page uses`)
    }
  }
  if (!/^\s*Referrer-Policy:\s*no-referrer\s*$/im.test(text)) problems.push('_headers has no Referrer-Policy: no-referrer')
}

const files = walk(SITE)
const pages = files.filter((f) => f.endsWith('.html'))
const home = path.join(SITE, 'index.html')
const linkPages = ['k', 'h'].map((dir) => path.join(SITE, dir, 'index.html'))
for (const page of [home, ...linkPages]) {
  if (!fs.existsSync(page)) problems.push(`${rel(page)} is missing`)
}

const hashes = { script: [], style: [] }
for (const page of pages) {
  const h = checkPage(page, fs.readFileSync(page, 'utf8'))
  hashes.script.push(...h.script)
  hashes.style.push(...h.style)
}
if (fs.existsSync(home)) {
  const text = fs.readFileSync(home, 'utf8')
  for (const page of linkPages) {
    if (fs.existsSync(page) && fs.readFileSync(page, 'utf8') !== text) {
      problems.push(`${rel(page)} differs from index.html; the link pages are copies of the home page`)
    }
  }
}
checkHeaders(path.join(SITE, '_headers'), hashes)

for (const line of warnings) console.warn(`warning: ${line}`)
if (problems.length) {
  for (const line of problems) console.error(`fail: ${line}`)
  process.exitCode = 1
} else {
  console.log(`site checks passed: ${pages.length} HTML pages, ${hashes.script.length} inline scripts, ${hashes.style.length} inline styles`)
}
