// Ported from hyperdht 6.34.1 lib/holepuncher.js (holepunch, the one-byte probe) and lib/socket-pool.js (the routing of
// a one-byte datagram to the puncher), MIT License, Copyright (c) 2018-2019 Mathias Buus, David Mark Clements &
// Contributors.
//
// The holepunch datagram of the node's UDP socket, for the hole punching of pears/hyperdht. A holepunch datagram
// is one zero byte. It is sent with a TTL of its own, and it goes to the punch handler, never to the dht-rpc
// handlers.
package dhtrpc

import (
	"context"
	"net"
)

// SendPunch sends the one-byte holepunch datagram from the node's UDP socket to to, with the IP TTL ttl for
// this datagram only: the socket's TTL is set for the send and put back after it. The TTL is a property of the
// socket, so a dht-rpc or UDX send that runs in the same few microseconds goes out with ttl too; those senders
// retry a lost packet, so this costs a retry at most. It returns the error of the send.
func (n *Node) SendPunch(to *net.UDPAddr, ttl int) error {
	n.ttlMu.Lock()
	defer n.ttlMu.Unlock()
	return SendPunchFrom(n.udp, to, ttl)
}

// SendPunchFrom sends the one-byte holepunch datagram from conn to to, with the IP TTL ttl for this datagram only,
// as Node.SendPunch does for the node's socket. The caller serializes the sends on conn, since the TTL is set
// for the send and put back after it.
func SendPunchFrom(conn *net.UDPConn, to *net.UDPAddr, ttl int) error {
	if to.IP.To4() == nil {
		return errNotIPv4
	}
	prev, err := ipTTL(conn)
	if err != nil {
		return err
	}
	if err := setIPTTL(conn, ttl); err != nil {
		return err
	}
	_, err = conn.WriteToUDP([]byte{0}, to)
	if rerr := setIPTTL(conn, prev); err == nil {
		err = rerr
	}
	return err
}

// OnPunch sets the handler of the one-byte holepunch datagrams that arrive on the node's UDP socket. The
// handler gets the address the datagram came from. A one-byte datagram is never passed to the dht-rpc
// handlers. The handler runs on the read loop, so it must not block.
func (n *Node) OnPunch(handler func(from *net.UDPAddr)) {
	n.mu.Lock()
	rpc := n.rpc
	n.mu.Unlock()
	rpc.setPunch(handler)
}

// Observed sends a PING from the node's UDP socket to to, and returns the address that to reports for the
// socket: the 'to' field of its reply. It is one NAT sample, as upstream's nat sampler takes them from the
// pings it sends.
func (n *Node) Observed(ctx context.Context, to *net.UDPAddr) (Addr, error) {
	resp, err := n.requestRetry(ctx, to, Request{Internal: true, Command: cmdPing}, requestRetries, nil)
	if err != nil {
		return Addr{}, err
	}
	return resp.To, nil
}
