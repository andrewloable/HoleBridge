// VPN mode DNS (D37, docs/architecture.md, "VPN mode (Android and iOS)"). The app gives every service an
// address in 198.18.0.0/16 and a name, and answers DNS for those names itself. Every other query is
// forwarded unread and unlogged to the network's normal DNS. Stubs only: HoleBridge-hb5.5.2 implements these.

// allocate(entries, previous) gives each service an IPv4 address in 198.18.0.0/16, from 198.18.0.2 upward.
// entries are [{ host, service, origins }]. It returns { name -> address }, where name is
// '<service>.<host>.internal' lowercased, plus the hostname of each origin. A service keeps the address
// that previous gives it under its 'service.host' key, and a new name gets the lowest address not in use.
function allocate(entries, previous) {
  throw new Error('not implemented')
}

// VpnDns answers queries for the names in names (the output of allocate) and forwards the rest. upstream is
// a list of IPv4 addresses; the engine sends upstream queries to port 53 with socketFactory(), a UDP socket
// with send(buffer, port, host) and on('message', (buffer, from)). handle(queryBuf) resolves with the
// reply bytes and never rejects. Names are matched case-insensitively. Nothing is logged.
class VpnDns {
  constructor({ names, upstream, socketFactory, timeoutMs = 2000 }) {
    throw new Error('not implemented')
  }

  async handle(queryBuf) {
    throw new Error('not implemented')
  }
}

module.exports = { allocate, VpnDns }
