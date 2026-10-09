// The engine entry: the IPC requests of spec/ipc.md handled by the modules (docs/architecture.md, "App engine IPC").
// index.js wires the Bare IPC frames to it. This file is a stub until HoleBridge-hb5.23.7 lands.
//
// createEngine({ send, testnetBootstrap }) builds Sessions, Listeners, UdpServices, the relay DHT, handoff and the
// VPN modules, and registers every request in spec/ipc.md. send(frame) takes one encoded frame (a Buffer) for the IPC
// channel. testnetBootstrap, when set, is the bootstrap of the DHT the engine makes; the tests pass a hyperdht
// testnet's, and the app passes none.
//
// Returns { receive(frame), close() }:
// - receive(frame) takes one request frame from Dart, runs its handler and resolves once its reply has been sent.
//   A frame that does not decode, or names no request, sends an HB-IPC-DESYNC error event (lib/ipc.js createServer).
// - close() ends every session, listener, UDP socket, VPN front and handoff listener, and destroys the DHT.
function createEngine({ send, testnetBootstrap } = {}) {
  throw new Error('not implemented')
}

module.exports = { createEngine }
