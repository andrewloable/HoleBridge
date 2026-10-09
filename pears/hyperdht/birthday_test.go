package hyperdht

import (
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/pears/udx"
)

// birthdayWait bounds the waits of the birthday tests: a birthday socket connects within a datagram or two.
const birthdayWait = 5 * time.Second

// acquireBirthday returns a birthday socket of d's punch pool. It is released when the test ends, and it fails the
// test as not implemented while the stub stands.
func acquireBirthday(t *testing.T, d *DHT) punchSocket {
	t.Helper()
	pool, ok := d.punchPool().(birthdayPool)
	if !ok {
		t.Fatal("the punch pool of a DHT has no birthday sockets")
	}
	s, err := pool.AcquireBirthday()
	failIfStub(t, err)
	must(t, err)
	t.Cleanup(func() { d.punchPool().Release(s) })
	return s
}

// countPunches reads from conn for wait and returns how many one-byte datagrams arrived. It does not fail the test.
func countPunches(conn *net.UDPConn, wait time.Duration) int {
	deadline := time.Now().Add(wait)
	buf := make([]byte, 64)
	n := 0
	for {
		if err := conn.SetReadDeadline(deadline); err != nil {
			return n
		}
		k, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			return n
		}
		if k == 1 {
			n++
		}
	}
}

// Test case 1: two responders share one DHT node's socket, each with its own peer. A one-byte datagram from peer A
// is answered once, to A. Upstream answers on the socket a datagram arrives on, so a responder answers only what its
// own socket receives; on the shared socket the responder of peer B must not answer A's datagram. Without that, each
// live responder answers every datagram (the amplification the birthday change removes).
func TestResponderAnswersOnlyItsPeerDatagram(t *testing.T) {
	tn := startTestnet(t, 2)
	d := tn.Nodes[0]
	peerA := punchListener(t)
	peerB := punchListener(t)
	respA := newPuncher(t, punchConfig{Pool: d.punchPool(), RemoteFirewall: firewallConsistent})
	respB := newPuncher(t, punchConfig{Pool: d.punchPool(), RemoteFirewall: firewallConsistent})
	callStub(t, "updateRemote", func() {
		respA.updateRemote(firewallConsistent, true, []Address{addressOf(peerA.LocalAddr().(*net.UDPAddr))}, netip.Addr{})
		respB.updateRemote(firewallConsistent, true, []Address{addressOf(peerB.LocalAddr().(*net.UDPAddr))}, netip.Addr{})
	})

	_, err := peerA.WriteToUDP([]byte{0}, nodeAddr(t, d))
	must(t, err)
	if n := countPunches(peerA, time.Second); n != 1 {
		t.Errorf("peer A got %d answers to its one datagram, want 1", n)
	}
	if n := countPunches(peerB, 200*time.Millisecond); n != 0 {
		t.Errorf("peer B got %d answers to a datagram that was not its own, want none", n)
	}
}

// Test case 2: the birthday sockets of a DHT's punch pool are real UDP sockets on the node's host, each bound to
// port 0: no two share a port, and none is the node's port. Each takes only the one-byte datagrams sent to its own
// port. Release closes a socket, so its port is free again.
func TestBirthdaySocketsAreRealAndOwnTheirPorts(t *testing.T) {
	tn := startTestnet(t, 2)
	d := tn.Nodes[0]
	nodePort := nodeAddr(t, d).Port
	var socks []punchSocket
	ports := map[int]bool{}
	for range 3 {
		s := acquireBirthday(t, d)
		local := s.Local()
		if local == nil || local.Port == 0 || local.Port == nodePort || ports[local.Port] {
			t.Fatalf("birthday socket local address %v: want a port of its own, not the node's %d", local, nodePort)
		}
		ports[local.Port] = true
		socks = append(socks, s)
	}

	got := make([]chan *net.UDPAddr, len(socks))
	for i, s := range socks {
		ch := make(chan *net.UDPAddr, 8)
		got[i] = ch
		s.OnPunch(func(from *net.UDPAddr) { ch <- from })
	}
	sender := punchListener(t)
	for i, s := range socks {
		_, err := sender.WriteToUDP([]byte{0}, udpAddrOf(addressOf(s.Local())))
		must(t, err)
		select {
		case from := <-got[i]:
			if addressOf(from) != addressOf(sender.LocalAddr().(*net.UDPAddr)) {
				t.Errorf("birthday socket %d got a datagram from %v, want the sender %v", i, from, sender.LocalAddr())
			}
		case <-time.After(birthdayWait):
			t.Fatalf("birthday socket %d got no datagram sent to its port", i)
		}
	}
	for i := range socks {
		if n := len(got[i]); n != 0 {
			t.Errorf("birthday socket %d got %d datagrams that were not sent to it", i, n)
		}
	}

	released := socks[0].Local()
	d.punchPool().Release(socks[0])
	conn, err := net.ListenUDP("udp4", released)
	if err != nil {
		t.Fatalf("the released birthday socket's port %d is still bound: %v", released.Port, err)
	}
	conn.Close()
}

// Test case 3: a randomizing initiator punches with birthday sockets toward a consistent remote. The remote's
// datagram that lands on one of them connects the puncher on that socket: OnConnect gets that socket and the
// remote's address, and the other sockets are released (upstream _onholepunchmessage connects on the holder the
// datagram came to and releases the rest). The NAT is randomized because three observers see three ports.
func TestBirthdayPunchConnectsOnTheSocketItHit(t *testing.T) {
	tn := startTestnet(t, 2)
	local, remote := tn.Nodes[0], tn.Nodes[1]
	acquireBirthday(t, local) // the stub, or the pool's birthday sockets, before the punch needs them
	connects := make(chan connectEvent, 4)
	p := newPuncher(t, punchConfig{
		Pool:           local.punchPool(),
		Initiator:      true,
		RemoteFirewall: firewallConsistent,
		OnConnect:      func(s punchSocket, r *net.UDPAddr) { connects <- connectEvent{s, r} },
		Timing:         punchTiming{BirthdaySockets: 3},
	})
	observers := []Address{addr("203.0.113.11", 6881), addr("203.0.113.12", 6881), addr("203.0.113.13", 6881)}
	for i, o := range observers {
		p.observe(Address{Host: netip.MustParseAddr("198.51.100.7"), Port: uint16(40000 + i)}, o)
	}
	remoteAddr := nodeAddr(t, remote)
	callStub(t, "updateRemote", func() {
		p.updateRemote(firewallConsistent, true, []Address{addressOf(remoteAddr)}, addressOf(remoteAddr).Host)
	})
	var ok bool
	var err error
	callStub(t, "punch", func() { ok, err = p.punch() })
	if err != nil || !ok {
		t.Fatalf("punch = %v, %v, want started", ok, err)
	}
	waitUntil(t, birthdayWait, "three sockets (the node's and two birthday sockets)", func() bool {
		return len(p.holderSockets()) == 3
	})
	hit := p.holderSockets()[1]

	if err := remote.punchSocket().SendPunch(udpAddrOf(addressOf(hit.Local())), punchTTLDefault); err != nil {
		t.Fatalf("remote SendPunch: %v", err)
	}
	select {
	case ev := <-connects:
		if ev.sock != hit {
			t.Errorf("connected on socket %v, want the birthday socket the datagram hit %v", ev.sock.Local(), hit.Local())
		}
		if addressOf(ev.remote) != addressOf(remoteAddr) {
			t.Errorf("connected to %v, want the remote %v", ev.remote, remoteAddr)
		}
	case <-time.After(birthdayWait):
		t.Fatal("the birthday punch did not connect within 5 s")
	}
	if held := p.holderSockets(); len(held) != 1 || held[0] != hit {
		t.Errorf("after the connect the puncher holds %d sockets, want only the one it connected on", len(held))
	}
	// A socket that a stream runs on is kept: a release leaves it open, and the stream closes it. Here the test keeps
	// it itself, as claimPunched does for the stream of a connection.
	hitSock := hit.(*dhtPunchSocket)
	t.Cleanup(hitSock.closeSocket)
	hitSock.keep()
	local.punchPool().Release(hit)
	if conn, err := net.ListenUDP("udp4", hit.Local()); err == nil {
		conn.Close()
		t.Error("a kept birthday socket was closed by Release, want it open for its stream")
	}
}

// Test case 4: a UDX stream made on a birthday socket carries data. rehomeStream moves a stream from the node's socket
// onto the birthday socket: the stream keeps its id and connects to the remote stream, and the remote stream, on its
// node's socket, connects back to the birthday socket's address. Data the remote writes reaches the moved stream.
func TestBirthdayStreamCarriesData(t *testing.T) {
	tn := startTestnet(t, 2)
	local, remote := tn.Nodes[0], tn.Nodes[1]
	bs := acquireBirthday(t, local)
	keep, ok := bs.(*dhtPunchSocket)
	if !ok {
		t.Fatalf("birthday socket is a %T, want the DHT's punch handle", bs)
	}
	st := local.newStream()
	ownID := st.ID()
	const remoteID = 0x5a5a
	peer := remote.Socket().NewStream(remoteID)
	t.Cleanup(func() { peer.Close() })
	must(t, peer.Connect(ownID, udpAddrOf(addressOf(bs.Local()))))

	var moved *udx.Stream
	var err error
	moved, err = rehomeStream(st, keep, remoteID, nodeAddr(t, remote))
	failIfStub(t, err)
	must(t, err)
	t.Cleanup(func() { moved.Close() })

	msg := []byte("birthday socket")
	if _, err := peer.Write(msg); err != nil {
		t.Fatalf("remote write: %v", err)
	}
	got := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 64)
		n, err := moved.Read(buf)
		if err != nil {
			got <- nil
			return
		}
		got <- buf[:n]
	}()
	select {
	case b := <-got:
		if string(b) != string(msg) {
			t.Errorf("the moved stream read %q, want %q", b, msg)
		}
	case <-time.After(birthdayWait):
		t.Fatal("the moved stream read nothing within 5 s")
	}
}

// Test case 5: a connection on a birthday socket owns it. punchedConn keeps the socket, so the puncher's Release leaves
// it open, and closing the connection closes the socket. On the node's own socket, punchedConn keeps the stream where
// it is and closes nothing.
func TestPunchedConnOwnsItsBirthdaySocket(t *testing.T) {
	tn := startTestnet(t, 2)
	local, remote := tn.Nodes[0], tn.Nodes[1]
	bs := acquireBirthday(t, local)
	own := bs.(*dhtPunchSocket)
	port := own.Local().Port
	conn, err := punchedConn(local.newStream(), own, 7, nodeAddr(t, remote))
	failIfStub(t, err)
	must(t, err)
	local.punchPool().Release(bs)
	if c, err := net.ListenUDP("udp4", own.Local()); err == nil {
		c.Close()
		t.Fatalf("birthday socket port %d is free after Release, want it open for the connection", port)
	}
	must(t, conn.Close())
	if c, err := net.ListenUDP("udp4", own.Local()); err != nil {
		t.Errorf("birthday socket port %d is still bound after the connection closed: %v", port, err)
	} else {
		c.Close()
	}
}
