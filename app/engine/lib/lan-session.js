// Session over the LAN: Noise over plain TCP to the host's LAN listener (docs/architecture.md, LAN route:
// Session over the LAN). The app opens TCP to the host's LAN port and runs the Noise handshake over it
// as initiator with the client key pair, expecting the host public key. From there the session is the
// same Protomux channel and protocol as the direct route (lib/connect.js), and the channel setup is shared.
//
// Stub for the TEST task (HoleBridge-hb5.2.3). The IMPL task (HoleBridge-hb5.2.4) makes the tests pass.

// connectLan({ address, port, key, appKey }) -> Promise<HostSession>, with route 'lan'. address and port
// are the host's LAN TCP address (from a probe reply, or the handshake's lan block), key the typed key,
// appKey the 32-byte application key.
async function connectLan({ address, port, key, appKey }) {
  throw new Error('not implemented')
}

module.exports = { connectLan }
