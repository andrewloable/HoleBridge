// TV handoff (D36, docs/architecture.md, "Adding a host to a TV (handoff)"). The TV listens for one sealed
// box from a phone on the LAN and adds the host it carries. The phone seals the host to the TV's public key
// and sends it. Stubs only: HoleBridge-hb5.22.3 implements these.

// encodeLink(base, { publicKey, secret, port, addresses }) returns the TV's link,
// base + "/h#1." + publicKey + "." + secret + "." + port + "." + addresses joined with commas.
function encodeLink(base, { publicKey, secret, port, addresses }) {
  throw new Error('not implemented')
}

// parseLink(link) returns { publicKey, secret, port, addresses } and throws for any link that is not a
// valid handoff link.
function parseLink(link) {
  throw new Error('not implemented')
}

// encodePlaintext({ version, secret, name, key, appKey }) returns the box plaintext, compact-encoded.
function encodePlaintext(value) {
  throw new Error('not implemented')
}

// listen({ addresses, clock }) starts the TV's listener. It resolves to { link, received, cancel }:
// received resolves with { name, key, appKey } for the first valid box, and rejects with HB-HANDOFF-EXPIRED
// after 5 minutes. clock supplies setTimeout and clearTimeout.
async function listen({ addresses, clock }) {
  throw new Error('not implemented')
}

// send(link, { name, key, appKey }) seals the host to the TV named in link and sends it. It resolves once
// the TV has added the host, and rejects with HB-HANDOFF-UNREACHABLE when no address connects, or with an
// HbError whose reason is 'refused' when the TV answers 0.
async function send(link, { name, key, appKey }) {
  throw new Error('not implemented')
}

module.exports = { encodeLink, parseLink, encodePlaintext, listen, send }
