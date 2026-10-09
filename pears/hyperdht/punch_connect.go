// The punch socket of a DHT node, and the NAT samples its pings give, for the connect and server hole punching
// (upstream hyperdht 6.34.1 lib/connect.js holepunch, probeRound and roundPunch, lib/server.js setupHolepuncher and
// _onpeerholepunch, lib/nat.js autoSample). Stubs only: HoleBridge-85m.5.14 (TEST) signals "not implemented" until
// its IMPL task writes the bodies. The punch socket is the DHT's own UDP socket, so a UDX stream connected to a
// punched address uses the port the punch came through, as upstream's holder socket does.
package hyperdht

import (
	"context"
	"errors"
	"net"
)

// dhtPunchSocket is the punch socket of a DHT node: the node's UDP socket, which also carries its UDX streams. It
// sends and receives the one-byte holepunch datagrams through dhtrpc (SendPunch and OnPunch).
type dhtPunchSocket struct {
	d *DHT
}

var _ punchSocket = (*dhtPunchSocket)(nil)

// Local returns the address of the node's socket.
func (s *dhtPunchSocket) Local() *net.UDPAddr {
	panic("not implemented")
}

// SendPunch sends the one-byte holepunch datagram from the node's socket to to, with the TTL ttl.
func (s *dhtPunchSocket) SendPunch(to *net.UDPAddr, ttl int) error {
	return errors.ErrUnsupported
}

// OnPunch sets the handler of the holepunch datagrams that arrive on the node's socket.
func (s *dhtPunchSocket) OnPunch(handler func(from *net.UDPAddr)) {
	panic("not implemented")
}

// punchSocket returns the punch socket of d: its own UDP socket.
func (d *DHT) punchSocket() punchSocket {
	panic("not implemented")
}

// punchPool returns the pool of d's punch sockets: Acquire returns the node's own socket, and Release leaves it
// open. A pool that must hand out several sockets, as a randomizing side's birthday punch needs, is the IMPL's
// choice.
func (d *DHT) punchPool() punchPool {
	panic("not implemented")
}

// sampleNATFromPings sends a PING from d's socket to each observer, and feeds each answer to p.observe with the
// address the observer reports for the socket and the observer's address. It returns how many answers came in,
// and an error when the pings fail for a reason other than a missing answer.
func (d *DHT) sampleNATFromPings(ctx context.Context, p *holepuncher, observers []*net.UDPAddr) (int, error) {
	return 0, errors.ErrUnsupported
}
