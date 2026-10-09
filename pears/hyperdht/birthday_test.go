package hyperdht

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/pears/dhtrpc"
	"github.com/andrewloable/HoleBridge/pears/noise"
	"github.com/andrewloable/HoleBridge/pears/secretstream"
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

// streamOutcome is the answer to a handshake that named the client's own UDX stream: the server's reply payload, the
// secret stream keys of the handshake, the server's key and the holepunch secret.
type streamOutcome struct {
	payload   HandshakePayload
	keys      secretstream.Keys
	serverKey [32]byte
	secret    [32]byte
}

// streamHandshake sends a raw handshake from kp to the server host through the node at relay, as a connect does, and
// names stream as the client's UDX stream. The client's firewall is randomizing, as the client's probes say. ok is
// false when no answer arrives that decodes as a handshake reply.
func streamHandshake(t *testing.T, relay *net.UDPAddr, host [32]byte, kp noise.KeyPair, stream uint32) (streamOutcome, bool) {
	t.Helper()
	prologue := dhtNamespace(cmdPeerHandshake)
	payload, err := EncodeNoisePayload(NoisePayload{
		Firewall:     firewallRandom,
		UDX:          &UDXInfo{Version: 1, ID: uint64(stream)},
		SecretStream: &SecretStreamInfo{Version: 1},
	})
	must(t, err)
	hs := noise.NewInitiator(kp, host, prologue[:])
	msg1, err := hs.Send(payload)
	must(t, err)
	value, err := EncodeHandshake(Handshake{Mode: handshakeFromClient, Noise: msg1})
	must(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), handshakeWait)
	defer cancel()
	target := hashKey(host)
	resp, err := newClient(t).Request(ctx, relay, dhtrpc.Request{Command: cmdPeerHandshake, Target: target[:], Value: value})
	if err != nil || resp.Error != 0 || len(resp.Value) == 0 {
		return streamOutcome{}, false
	}
	ans, err := DecodeHandshake(resp.Value)
	if err != nil || ans.Mode != handshakeReply || len(ans.Noise) == 0 {
		return streamOutcome{}, false
	}
	body, err := hs.Recv(ans.Noise)
	if err != nil || !hs.Complete() {
		return streamOutcome{}, false
	}
	p, err := DecodeNoisePayload(body)
	if err != nil {
		return streamOutcome{}, false
	}
	_, _, hash, serverKey := hs.Result()
	return streamOutcome{payload: p, keys: keysOf(hs), serverKey: serverKey, secret: holepunchSecret(t, hash)}, true
}

// Test case 6: a connect of a randomizing client, whose punch starts from a birthday socket, connects through the
// server's puncher, and data flows both ways. The handshake reaches the server through a holder, so it is relayed, and
// the address the holder names for the client is not the one the client's birthday socket punches from. The server
// must not claim the stream at admission: upstream claims a relayed handshake at admission only when its client is
// open, and otherwise waits for the puncher or the relay pairing (lib/server.js _addHandshake). A stream claimed at the
// holder's address points at an address the client's punch does not use, and the header exchange never completes. The client is a DHT node whose NAT randomizes: three observers see three
// ports, so its puncher opens birthday sockets toward the server (birthdayProbes). Loopback has no NAT, so the test
// plays the client's side by hand: its probe, its puncher, and the claim of its own stream on the birthday socket it
// connected on (punchedConn), with the secret stream run over that stream.
func TestRandomizedClientConnectsThroughBirthdaySocket(t *testing.T) {
	tn := startTestnet(t, 10)
	hideRemoteAddress(t, tn.Nodes[0]) // the server answers with a holepunch, as a server that does not know its address does
	server, client := tn.Nodes[0], tn.Nodes[9]
	host := testKeyPair(3)
	kp := testKeyPair(5)
	srv := newServer(t, server, ServerOptions{})
	listenOn(t, srv, host)
	accepted := acceptNext(srv)

	// The client's stream is made on its node's socket, and the handshake names its id, as a connect's handshake does.
	st := client.newStream()
	relay := holderOtherThanServer(t, tn, host.Public, nodeAddr(t, server))
	ans, ok := streamHandshake(t, relay, host.Public, kp, st.ID())
	if !ok {
		t.Fatal("the handshake through a holder got no answer")
	}
	info := ans.payload.Holepunch
	if info == nil || len(info.Relays) == 0 {
		t.Fatal("the handshake reply names no holepunch id and relays, so there is no punch to make")
	}

	// The probe tells the server that the client's NAT randomizes and gives its host (a randomizing NAT reports no
	// port). The server's answer carries its own NAT state, and names the server's address for the client's punch.
	clientHost := addressOf(nodeAddr(t, client)).Host
	reply := probeServer(t, info, host.Public, ans.secret, HolepunchPayload{
		Firewall:  firewallRandom,
		Addresses: []Address{{Host: clientHost}},
	})
	serverNode := addressOf(nodeAddr(t, server))

	// The client's puncher: its NAT samples say randomized, and the server is consistent and verified (its token echoed).
	connects := make(chan connectEvent, 4)
	p := newPuncher(t, punchConfig{
		Pool:      client.punchPool(),
		Initiator: true,
		OnConnect: func(s punchSocket, r *net.UDPAddr) { connects <- connectEvent{s, r} },
		Timing:    punchTiming{BirthdaySockets: 4},
	})
	for i, o := range []Address{addr("203.0.113.11", 6881), addr("203.0.113.12", 6881), addr("203.0.113.13", 6881)} {
		p.observe(Address{Host: netip.MustParseAddr("198.51.100.7"), Port: uint16(40000 + i)}, o)
	}
	callStub(t, "updateRemote", func() {
		p.updateRemote(reply.Firewall, true, []Address{serverNode}, serverNode.Host)
	})
	var started bool
	var err error
	callStub(t, "punch", func() { started, err = p.punch() })
	if err != nil || !started {
		t.Fatalf("punch = %v, %v, want a birthday punch started", started, err)
	}

	var ev connectEvent
	select {
	case ev = <-connects:
	case <-time.After(punchWait):
		t.Fatal("the birthday punch did not connect within the punch wait")
	}
	if addressOf(ev.remote) != serverNode {
		t.Errorf("the punch connected to %v, want the server's node %v", ev.remote, serverNode)
	}
	own := birthdayOf(ev.sock)
	if own == nil {
		t.Fatalf("the punch connected on the node's socket %v, want a birthday socket", ev.sock.Local())
	}

	// The client claims its stream on the birthday socket the punch connected on, and runs the secret stream over it.
	conn, err := punchedConn(st, own, uint32(ans.payload.UDX.ID), ev.remote)
	must(t, err)
	c := secretstream.Resume(conn, true, secretstream.Options{RemotePublicKey: &host.Public}, ans.keys)
	t.Cleanup(func() { c.Close() })
	hctx, hcancel := context.WithTimeout(context.Background(), headerExchangeWait)
	defer hcancel()
	must(t, c.Handshake(hctx))
	s := awaitAccept(t, accepted, readWait)
	exchange(t, c, s)
}
