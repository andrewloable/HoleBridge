package dhtrpc

import (
	"context"
	"net"
	"testing"
	"time"
)

// A fed IO runs its requests on a socket that its owner reads: the owner passes each datagram it reads to Feed, and the
// replies to the IO's requests come back to that socket. This is how a request runs from a socket of its own, such as a
// birthday socket of a hole punch, and how its NAT sample is the address of that socket. Upstream sends a request from
// a given socket the same way (dht-rpc request with opts.socket, and nat-sampler's pings from a socket).

// fedFeed reads conn in the background, as the owner of a fed IO does, and feeds each datagram to io until conn closes.
func fedFeed(conn *net.UDPConn, io *IO) {
	buf := make([]byte, maxDatagram)
	for {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			return
		}
		io.Feed(buf[:n], from.(*net.UDPAddr))
	}
}

// A ping from a fed IO goes out from its socket, so the node reports that socket's address, not its own, and the reply
// comes back to the socket the IO runs on.
func TestFedIOObservedIsTheAddressOfItsSocket(t *testing.T) {
	tn := startTestnet(t, 3)
	conn := listen(t)
	var io *IO
	stubCall(t, "NewFedIO", func() { io = NewFedIO(conn) })
	t.Cleanup(func() { io.Close() })
	go fedFeed(conn, io)

	ctx, cancel := context.WithTimeout(context.Background(), punchWait)
	defer cancel()
	var seen Addr
	var err error
	stubCall(t, "Observed", func() { seen, err = io.Observed(ctx, nodeAddr(t, tn.Nodes[1])) })
	checkImplemented(t, "Observed", err)
	if err != nil {
		t.Fatalf("Observed: %v", err)
	}
	if want := dhtAddr(addrOf(conn)); seen != want {
		t.Errorf("Observed = %v:%d, want the fed socket's own address %v:%d", seen.Host, seen.Port, want.Host, want.Port)
	}
}

// The owner of a fed IO reads its socket. Without a reader the reply to a request sits unread, so the request fails:
// the IO never reads the socket itself, which would take the holepunch datagrams and other traffic from its owner.
func TestFedIOLeavesItsSocketToItsOwner(t *testing.T) {
	tn := startTestnet(t, 3)
	conn := listen(t)
	var io *IO
	stubCall(t, "NewFedIO", func() { io = NewFedIO(conn) })
	t.Cleanup(func() { io.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	var err error
	stubCall(t, "Observed", func() { _, err = io.Observed(ctx, nodeAddr(t, tn.Nodes[1])) })
	checkImplemented(t, "Observed", err)
	if err == nil {
		t.Fatal("Observed on a fed IO with no owner reading its socket got a reply: the IO read the socket itself")
	}
}
