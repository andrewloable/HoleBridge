// LAN probes and the sliced sweep for the app engine (docs/architecture.md, LAN route: Discovery and
// Probe and reply bytes). STUB from the TEST task HoleBridge-hb5.2.1: every body only signals 'not
// implemented'. The IMPL task HoleBridge-hb5.2.2 replaces the bodies.
//
// Shapes the tests rely on:
// - socketFactory() returns a udx-native UDP socket. findHost uses only bind(port, host),
//   trySend(buf, port, host), on('message', (msg, from)) with from = { host, family, port }, and close()
//   (a promise). It makes one socket per slice and closes each one before it resolves.
// - timers is { setTimeout(fn, ms), clearTimeout(id) }; findHost waits only through it.
// - port is the UDP port of the responder. The result's port is the host's LAN TCP port from the reply.

// encodeProbe(lanKey, nonce, unixMs) -> Buffer(256)
function encodeProbe(lanKey, nonce, unixMs) {
  throw new Error('not implemented')
}

// verifyReply(lanKey, buf, nonce) -> port | null
function verifyReply(lanKey, buf, nonce) {
  throw new Error('not implemented')
}

// findHost({ lanKey, lastKnown, localAddresses, port, socketFactory, timers }) -> { address, port } | null
async function findHost({ lanKey, lastKnown, localAddresses, port, socketFactory, timers }) {
  throw new Error('not implemented')
}

module.exports = { encodeProbe, verifyReply, findHost }
