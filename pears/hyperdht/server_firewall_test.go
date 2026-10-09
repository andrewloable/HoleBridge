package hyperdht

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/pears/noise"
	"github.com/andrewloable/HoleBridge/pears/secretstream"
	"github.com/andrewloable/HoleBridge/pears/udx"
)

// natKnownWait bounds the wait for a testnet node's NAT sampling to name its own address.
const natKnownWait = 30 * time.Second

// firstHandshake builds the first handshake message from kp to the server host, with payload p, as a client sends it.
// It returns the message and the client's side of the handshake, which the reply is read with.
func firstHandshake(t *testing.T, kp noise.KeyPair, host [32]byte, p NoisePayload) ([]byte, *noise.Handshake) {
	t.Helper()
	prologue := dhtNamespace(cmdPeerHandshake)
	body, err := EncodeNoisePayload(p)
	must(t, err)
	hs := noise.NewInitiator(kp, host, prologue[:])
	msg, err := hs.Send(body)
	must(t, err)
	return msg, hs
}

// readReply decodes the server's reply to a firstHandshake, from the client's side hs, and returns its payload. The reply
// is the Noise message that answer returns, which the router wraps in a handshake reply on the wire.
func readReply(t *testing.T, hs *noise.Handshake, reply []byte) NoisePayload {
	t.Helper()
	body, err := hs.Recv(reply)
	if err != nil {
		t.Fatalf("the reply does not verify: %v", err)
	}
	p, err := DecodeNoisePayload(body)
	if err != nil {
		t.Fatalf("the reply payload does not decode: %v", err)
	}
	return p
}

// waitKnownAddress waits until the node's NAT sampling names its own address, as upstream's remoteAddress does.
func waitKnownAddress(t *testing.T, d *DHT) {
	t.Helper()
	waitUntil(t, natKnownWait, "the node learns its own address from its peers", func() bool {
		return d.remoteAddress() != nil
	})
}

// An admitted handshake to a node that knows its own address gets an open reply: firewall OPEN, and no holepunch, since
// the client connects straight to the node (upstream lib/server.js, firewall ourRemoteAddr ? OPEN : UNKNOWN).
func TestReplyNamesOpenNodeWhenItKnowsItsAddress(t *testing.T) {
	tn := startTestnet(t, 10)
	host := testKeyPair(3)
	waitKnownAddress(t, tn.Nodes[0])
	srv := newServer(t, tn.Nodes[0], ServerOptions{})
	listenOn(t, srv, host)
	ans, ok := sendHandshake(t, tn, host.Public, testKeyPair(5))
	if !ok {
		t.Fatal("the client got no answer to its handshake")
	}
	if ans.payload.Firewall != firewallOpen {
		t.Errorf("reply firewall = %d, want OPEN (%d)", ans.payload.Firewall, firewallOpen)
	}
	if ans.payload.Holepunch != nil {
		t.Errorf("reply holepunch = %+v, want none for an open node", ans.payload.Holepunch)
	}
}

// An admitted handshake to a node that does not know its own address keeps today's reply: firewall UNKNOWN, and a
// holepunch with an id, so the client punches to the node.
func TestReplyKeepsHolepunchWhenNodeDoesNotKnowItsAddress(t *testing.T) {
	d, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if d.remoteAddress() != nil {
		t.Fatal("a fresh node without peers names an address of its own")
	}
	host := testKeyPair(3)
	srv := newServer(t, d, ServerOptions{})
	srv.mu.Lock()
	srv.keyPair, srv.listening = host, true // answer needs the key pair; no announce, since the node has no peers
	srv.mu.Unlock()

	msg, hs := firstHandshake(t, testKeyPair(5), host.Public, NoisePayload{
		Version: 1, UDX: &UDXInfo{Version: 1, ID: 77}, SecretStream: &SecretStreamInfo{Version: 1},
	})
	reply := srv.answer(msg, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9}, true)
	if reply == nil {
		t.Fatal("the server gave no reply to an admitted handshake")
	}
	p := readReply(t, hs, reply)
	if p.Firewall != firewallUnknown {
		t.Errorf("reply firewall = %d, want UNKNOWN (%d)", p.Firewall, firewallUnknown)
	}
	if p.Holepunch == nil {
		t.Error("reply has no holepunch, want one with an id for a node that does not know its address")
	}
}

// A node under the test seam ForceRelayForTest keeps today's reply, even when it knows its own address: its open reply
// would make the client connect straight to it, which the seam forbids.
func TestForcedNodeReplyStaysUnknown(t *testing.T) {
	tn := startTestnet(t, 10)
	host := testKeyPair(3)
	waitKnownAddress(t, tn.Nodes[0])
	tn.Nodes[0].forceRelay = true
	// A forced node answers only a handshake that names or offers a relay, so the server offers one.
	relayKey := testKeyPair(9).Public
	srv := newServer(t, tn.Nodes[0], ServerOptions{RelayThrough: func(bool) *[32]byte { return &relayKey }})
	listenOn(t, srv, host)
	ans, ok := sendHandshake(t, tn, host.Public, testKeyPair(5))
	if !ok {
		t.Fatal("the client got no answer to its handshake")
	}
	if ans.payload.Firewall != firewallUnknown || ans.payload.Holepunch == nil {
		t.Errorf("forced reply firewall = %d, holepunch = %v, want UNKNOWN with a holepunch", ans.payload.Firewall, ans.payload.Holepunch != nil)
	}
}

// A client that is not open, whose handshake came through a relay, gets an open reply from a node that knows its address.
// Its first UDX packet, sent to the stream the reply names, claims the stream: the stream connects to the packet's
// address, and the secret stream opens over it with no punch.
func TestFirstPacketClaimsStreamOfOpenReply(t *testing.T) {
	tn := startTestnet(t, 10)
	host := testKeyPair(3)
	waitKnownAddress(t, tn.Nodes[0])
	srv := newServer(t, tn.Nodes[0], ServerOptions{})
	listenOn(t, srv, host)
	conns := acceptAll(srv)

	clientKP := testKeyPair(5)
	conn := loopbackUDP(t)
	csock := newTestUDXSocket(t, conn)
	const clientID = 4242
	cst := csock.NewStream(clientID)
	msg, hs := firstHandshake(t, clientKP, host.Public, NoisePayload{
		Version: 1, UDX: &UDXInfo{Version: 1, ID: clientID}, SecretStream: &SecretStreamInfo{Version: 1},
		Firewall: firewallUnknown,
	})
	// The handshake reaches the server as if a relay forwarded it from the client's address.
	reply := srv.answer(msg, conn.LocalAddr().(*net.UDPAddr), false)
	if reply == nil {
		t.Fatal("the server gave no reply to an admitted handshake")
	}
	p := readReply(t, hs, reply)
	if p.Firewall != firewallOpen || p.Holepunch != nil {
		t.Fatalf("reply firewall = %d, holepunch = %v: want an open reply", p.Firewall, p.Holepunch != nil)
	}
	if err := cst.Connect(uint32(p.UDX.ID), tn.Nodes[0].addrOrFail(t)); err != nil {
		t.Fatalf("client stream Connect: %v", err)
	}
	client := secretstream.Resume(cst, true, secretstream.Options{RemotePublicKey: &host.Public}, keysOf(hs))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.Handshake(ctx); err != nil {
		t.Fatalf("client secret stream: %v", err)
	}
	select {
	case ac := <-conns:
		if ac.Relayed() {
			t.Error("the connection is marked relayed, but it was claimed by its first packet on the direct path")
		}
		ac.Close()
	case <-time.After(15 * time.Second):
		t.Fatal("the server did not accept the connection")
	}
	client.Close()
}

// The first-packet claim refuses a packet from port 0, and claims nothing.
func TestFirstPacketClaimRefusesPortZero(t *testing.T) {
	d := newTestDHT(t)
	srv := newServer(t, d, ServerOptions{})
	st := d.newStream()
	cl := &streamClaim{}
	claim := srv.claimOnFirstPacket(st, cl, 9, secretstream.Keys{})
	if claim(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0}) {
		t.Fatal("a packet from port 0 claimed the stream")
	}
	if _, err := st.Write([]byte{1}); err == nil {
		t.Error("the stream is connected after a refused packet, want it unconnected")
	}
	if !cl.take() {
		t.Error("the refused packet took the stream's claim")
	}
}

// The first-packet claim does nothing for a stream that a relay pairing or the puncher claimed first: the claim is taken,
// so the packet is refused and the stream stays where that path put it.
func TestFirstPacketClaimLeavesClaimedStream(t *testing.T) {
	d := newTestDHT(t)
	srv := newServer(t, d, ServerOptions{})
	st := d.newStream()
	cl := &streamClaim{}
	if !cl.take() {
		t.Fatal("a fresh claim is already taken")
	}
	claim := srv.claimOnFirstPacket(st, cl, 9, secretstream.Keys{})
	if claim(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 4000}) {
		t.Fatal("a packet claimed a stream that another path had claimed")
	}
	if _, err := st.Write([]byte{1}); err == nil {
		t.Error("the stream is connected by a refused packet, want it unconnected")
	}
}

// The first-packet claim takes an unclaimed stream from a valid address: the stream connects there.
func TestFirstPacketClaimConnectsUnclaimedStream(t *testing.T) {
	d := newTestDHT(t)
	srv := newServer(t, d, ServerOptions{})
	st := d.newStream()
	cl := &streamClaim{}
	claim := srv.claimOnFirstPacket(st, cl, 9, secretstream.Keys{})
	if !claim(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 4000}) {
		t.Fatal("a valid packet from an unclaimed stream was refused")
	}
	if _, err := st.Write([]byte{1}); err != nil {
		t.Errorf("Write after the claim: %v, want the stream connected", err)
	}
}

// newTestDHT returns a DHT with no peers, closed when the test ends.
func newTestDHT(t *testing.T) *DHT {
	t.Helper()
	d, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// newTestUDXSocket wraps conn in a UDX socket that is closed when the test ends.
func newTestUDXSocket(t *testing.T, conn *net.UDPConn) *udx.Socket {
	t.Helper()
	s, err := udx.NewSocket(conn)
	if err != nil {
		t.Fatalf("udx.NewSocket: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// loopbackUDP opens a UDP socket on 127.0.0.1 with a free port, closed when the test ends.
func loopbackUDP(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("ListenUDP: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// addrOrFail returns the UDP address the node listens on.
func (d *DHT) addrOrFail(t *testing.T) *net.UDPAddr {
	t.Helper()
	a, err := d.addr()
	if err != nil {
		t.Fatalf("node address: %v", err)
	}
	return a
}

// hideRemoteAddress is the test seam hideAddress: the node never names its own address, so its server answers with a
// holepunch. The punch tests need that reply, and on a loopback testnet the node would otherwise learn its address.
func hideRemoteAddress(t testing.TB, d *DHT) {
	t.Helper()
	d.hideAddress = true
}
