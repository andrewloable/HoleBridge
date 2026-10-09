// The holepunch datagram of the node's UDP socket, for the hole punching of pears/hyperdht (upstream hyperdht
// 6.34.1 lib/holepuncher.js holepunch, and lib/socket-pool.js, which routes it to the puncher). A holepunch
// datagram is one zero byte. It is sent with a TTL of its own, and it goes to the punch handler, never to the
// dht-rpc handlers. Stubs only: HoleBridge-85m.5.14 (TEST) signals "not implemented" until its IMPL task writes
// the bodies.
package dhtrpc

import (
	"context"
	"errors"
	"net"
)

// SendPunch sends the one-byte holepunch datagram from the node's UDP socket to to, with the IP TTL ttl for
// this datagram only: a later send keeps its own TTL. It returns the error of the send.
func (n *Node) SendPunch(to *net.UDPAddr, ttl int) error {
	return errors.ErrUnsupported
}

// OnPunch sets the handler of the one-byte holepunch datagrams that arrive on the node's UDP socket. The
// handler gets the address the datagram came from. A one-byte datagram is never passed to the dht-rpc
// handlers.
func (n *Node) OnPunch(handler func(from *net.UDPAddr)) {
	panic("not implemented")
}

// Observed sends a PING from the node's UDP socket to to, and returns the address that to reports for the
// socket: the 'to' field of its reply. It is one NAT sample, as upstream's nat sampler takes them from the
// pings it sends.
func (n *Node) Observed(ctx context.Context, to *net.UDPAddr) (Addr, error) {
	return Addr{}, errors.ErrUnsupported
}
