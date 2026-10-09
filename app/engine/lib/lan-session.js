// Session over the LAN: Noise over plain TCP to the host's LAN listener (docs/architecture.md, LAN route:
// Session over the LAN). The app opens TCP to the host's LAN port and runs the Noise handshake over it
// as initiator with the client key pair, expecting the host public key. From there the session is the
// same Protomux channel and protocol as the direct route (lib/connect.js), and the channel setup is shared.

const TCP = require('bare-tcp')
const NoiseSecretStream = require('@hyperswarm/secret-stream')
const keys = require('./keys.js')
const { openSession } = require('./connect.js')

// KEEPALIVE is the interval of empty keepalive frames while the stream is idle: the DHT route's interval
// (hyperdht's connectionKeepAlive, 5 s) and the Go LAN listener's. Each side sees a dead path through it.
const KEEPALIVE = 5000

// connectLan({ address, port, key, appKey }) -> Promise<HostSession>, with route 'lan'. address and port
// are the host's LAN TCP address (from a probe reply, or the handshake's lan block), key the typed key,
// appKey the 32-byte application key. Rejects as connectDirect does: HB-LOOKUP-TIMEOUT when the host does
// not answer, its reason naming the TCP error (ECONNREFUSED for a closed port), and HB-VERSION-MISMATCH.
async function connectLan({ address, port, key, appKey }) {
  const pairs = await keys.derive(key, appKey)
  const socket = TCP.createConnection(port, address)
  // The IK pattern checks the host's static key inside the handshake, so a host with another key is
  // rejected before any channel opens. XX would not check it.
  const conn = new NoiseSecretStream(true, socket, {
    keyPair: pairs.client,
    remotePublicKey: pairs.host.publicKey,
    pattern: 'IK',
    keepAlive: KEEPALIVE
  })
  // bare-tcp keeps our write side open after the host's FIN, and the Noise stream reads nothing past its
  // handshake until it ends, so a host that closes the connection mid-handshake would leave the connect
  // waiting for the lookup timeout. Before the handshake completes, a host close rejects the connect now.
  socket.once('end', () => {
    if (!conn.connected) conn.destroy()
  })
  return openSession(conn, { route: 'lan' })
}

module.exports = { connectLan }
