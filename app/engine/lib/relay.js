// Relay key in the engine: the member key pair as the DHT default key pair, and the relayThrough policy
// that offers the relay (docs/security.md, "Relay keys"; docs/architecture.md, "Relay route").
// Stubs only, from the TEST task. The IMPL task replaces the bodies.

// createDht({ bootstrap, relayKey, appKey }) resolves to a HyperDHT. With a relay key, its defaultKeyPair
// is the member key pair of that relay key, so a relay admits the dial. Without one, the default key pair
// is left as HyperDHT makes it. The key is normalized first, and an invalid key rejects with HB-KEY-INVALID.
async function createDht({ bootstrap, relayKey, appKey }) {
  throw new Error('not implemented')
}

// relayPolicy({ relayServerPublicKey, dht }) returns the relayThrough policy: a function of force that
// returns relayServerPublicKey when force is true or the dht reports a randomized NAT, and null otherwise.
// The policy reads dht.randomized each time it is called.
function relayPolicy({ relayServerPublicKey, dht }) {
  throw new Error('not implemented')
}

module.exports = { createDht, relayPolicy }
