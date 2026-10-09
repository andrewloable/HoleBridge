package hyperdht

import (
	"context"

	"github.com/andrewloable/HoleBridge/pears/dhtrpc"
	"github.com/andrewloable/HoleBridge/pears/udx"
)

// Socket returns the UDP socket the node runs on. A blind-relay server opens its relay streams on it, so
// the relay's pairings share the node's port.
func (d *DHT) Socket() *udx.Socket {
	return d.node.Socket()
}

// Ready waits until the node has bootstrapped, or until ctx ends. Listen and Connect need the routing table
// that bootstrap fills, so a program that starts a node waits here before it uses the node.
func (d *DHT) Ready(ctx context.Context) error {
	return d.node.Ready(ctx)
}

// NAT returns the node's address and firewall state, as its peers report them (dhtrpc.Node.NAT).
func (d *DHT) NAT() dhtrpc.NATInfo {
	return d.node.NAT()
}
