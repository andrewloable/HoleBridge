// Relay key in the engine: the member key pair as the DHT default key pair, and the relayThrough policy
// that offers the relay (docs/security.md, "Relay keys"; docs/architecture.md, "Relay route").

const DHT = require('hyperdht')
const keys = require('./keys.js')

// createDht({ bootstrap, relayKey, appKey }) resolves to a HyperDHT. With a relay key, its defaultKeyPair
// is the member key pair of that relay key, so a relay admits the dial. Without one, the default key pair
// is left as HyperDHT makes it. The key is normalized first, and an invalid key rejects with HB-KEY-INVALID.
async function createDht({ bootstrap, relayKey, appKey }) {
  if (relayKey === undefined || relayKey === null) return new DHT({ bootstrap })
  // Derive before the DHT exists, so a rejected key leaves nothing running.
  const { member } = await keys.deriveRelay(relayKey, appKey)
  return new DHT({ bootstrap, keyPair: member })
}

// relayPolicy({ relayServerPublicKey, dht }) returns the relayThrough policy HyperDHT calls on a dial: it
// offers the relay when the dial is forced or the dht reports a randomized NAT, and null otherwise. The
// policy reads dht.randomized at each call, so a later dial sees the NAT as it is then.
function relayPolicy({ relayServerPublicKey, dht }) {
  return (force) => (force || dht.randomized ? relayServerPublicKey : null)
}

module.exports = { createDht, relayPolicy }
