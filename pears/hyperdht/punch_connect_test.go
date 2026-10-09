package hyperdht

import (
	"context"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/pears/dhtrpc"
)

// Expected values come from hyperdht 6.34.1 (lib/connect.js, lib/holepuncher.js, lib/server.js and lib/nat.js), as
// the notes of each test say. The tests run on 127.0.0.1, where no NAT is in the path: they check the punch socket,
// the NAT samples, the server's answer to a probe and the punch that follows, and the connect through a punch. The
// NAT cases are in the in-memory NAT model (natsim_test.go).

// punchWait bounds the wait for a punch datagram. Upstream's responder waits a second before its first round, and an
// unverified address gets its first datagram on the fourth round, so a punch takes about 4 s on loopback.
const punchWait = 15 * time.Second

// probeWait bounds how long a test sends punching probes before the server answers them with its NAT state. The
// server's samples come from a few pings on loopback, so this is far more than they take.
const probeWait = 12 * time.Second

// punchSocketOf returns the punch socket of d. The stub panics, which the test reports as not implemented.
func punchSocketOf(t *testing.T, d *DHT) punchSocket {
	t.Helper()
	var s punchSocket
	callStub(t, "punchSocket", func() { s = d.punchSocket() })
	return s
}

// punchListener returns a UDP socket on 127.0.0.1 that a test plays a peer with. The test closes it when it ends.
func punchListener(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	must(t, err)
	t.Cleanup(func() { conn.Close() })
	return conn
}

// readPunch reads from conn until a one-byte holepunch datagram arrives, and returns the address it came from. It
// returns an error when none arrives within wait. It does not fail the test, so it may run on any goroutine.
func readPunch(conn *net.UDPConn, wait time.Duration) (*net.UDPAddr, error) {
	deadline := time.Now().Add(wait)
	buf := make([]byte, 64)
	for {
		if err := conn.SetReadDeadline(deadline); err != nil {
			return nil, err
		}
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			return nil, err
		}
		if n == 1 && buf[0] == 0 {
			return from, nil
		}
	}
}

// expectPunch is readPunch on the test's goroutine: it fails the test when no punch arrives within wait.
func expectPunch(t *testing.T, conn *net.UDPConn, wait time.Duration) *net.UDPAddr {
	t.Helper()
	from, err := readPunch(conn, wait)
	if err != nil {
		t.Fatalf("no holepunch datagram arrived within %v: %v", wait, err)
	}
	return from
}

// probeServer sends punching probes for the handshake info of a server to the relay it names, and returns the
// decrypted answer once the server answers with no error and with its NAT firewall known. Each probe is a
// PEER_HOLEPUNCH to the relay, addressed to the server, with the payload p encrypted under the holepunch secret and
// the round number of the probe, as upstream's probe rounds send them (lib/connect.js probeRound and roundPunch). An
// answer with an error is sent again, until probeWait ends.
func probeServer(t *testing.T, info *HolepunchInfo, host [32]byte, secret [32]byte, p HolepunchPayload) HolepunchPayload {
	t.Helper()
	if info == nil || len(info.Relays) == 0 {
		t.Fatal("the handshake reply names no holepunch id and relays, so there is nothing to probe")
	}
	var sp *securePayload
	callStub(t, "newSecurePayload", func() { sp = newSecurePayload(secret) })
	relay := info.Relays[0]
	client := newClient(t)
	target := hashKey(host)
	deadline := time.Now().Add(probeWait)
	for round := uint64(0); time.Now().Before(deadline); round++ {
		p.Round = round
		enc, err := sp.encrypt(p)
		must(t, err)
		msg, err := EncodeHolepunch(Holepunch{Mode: holepunchFromClient, ID: info.ID, Payload: enc, PeerAddress: &relay.PeerAddress})
		must(t, err)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		resp, err := client.Request(ctx, udpAddrOf(relay.RelayAddress), dhtrpc.Request{Command: cmdPeerHolepunch, Target: target[:], Value: msg})
		cancel()
		if err == nil && resp.Error == 0 {
			if hp, err := DecodeHolepunch(resp.Value); err == nil && hp.Mode == holepunchReply {
				if reply, ok := sp.decrypt(hp.Payload); ok && reply.Error == 0 && reply.Firewall != firewallUnknown {
					return reply
				}
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatal("the server never answered the punching probe with its NAT state (not implemented: the server's NAT samples and its punch answer)")
	return HolepunchPayload{}
}

// Test case 1: the punch socket of a DHT node is the node's own UDP socket. A punch sent from one node's socket
// reaches the other node's punch handler with the sender's address, the punch socket's local address on loopback
// is the node's address, and its local addresses are that address alone (upstream localAddresses in
// lib/holepuncher.js). The nodes still answer requests after the punch.
func TestDHTPunchSocketSendsAndReceivesPunches(t *testing.T) {
	tn := startTestnet(t, 3)
	a, b := tn.Nodes[0], tn.Nodes[1]
	sa := punchSocketOf(t, a)
	sb := punchSocketOf(t, b)
	ownA := addressOf(nodeAddr(t, a))

	var local *net.UDPAddr
	callStub(t, "Local", func() { local = sa.Local() })
	if addressOf(local) != ownA {
		t.Errorf("punch socket Local = %v, want the node's address %v", local, ownA)
	}
	var lan []Address
	callStub(t, "localAddresses", func() { lan = localAddresses(sa) })
	if len(lan) != 1 || lan[0] != ownA {
		t.Errorf("local addresses of the punch socket = %v, want just %v on loopback", lan, ownA)
	}

	got := make(chan *net.UDPAddr, 4)
	callStub(t, "OnPunch", func() { sb.OnPunch(func(from *net.UDPAddr) { got <- from }) })
	var err error
	callStub(t, "SendPunch", func() { err = sa.SendPunch(nodeAddr(t, b), punchTTLDefault) })
	must(t, err)
	select {
	case from := <-got:
		if addressOf(from) != ownA {
			t.Errorf("punch arrived from %v, want the sender's address %v", from, ownA)
		}
	case <-time.After(punchWait):
		t.Fatal("the punch did not reach the other node's punch handler")
	}

	resp := lookupAt(t, newClient(t), nodeAddr(t, b), testTarget("punch socket"))
	if resp.Error != 0 {
		t.Errorf("LOOKUP after the punch: error %d, want 0", resp.Error)
	}
}

// Test case 2: the holepuncher of a DHT node takes its NAT samples from the answers to its pings. Three observers
// on loopback each see the node at its one address, so the samples decide a consistent NAT with that address
// (upstream lib/nat.js: autoSample pings the nodes it knows, and _updateFirewall and _updateAddresses decide from the
// samples). The puncher is firewalled, as the holepuncher port decides (85m.5.10), so the samples decide.
func TestNATSamplesFromDHTPings(t *testing.T) {
	tn := startTestnet(t, 4)
	d := tn.Nodes[0]
	var pool punchPool
	callStub(t, "punchPool", func() { pool = d.punchPool() })
	p := newPuncher(t, punchConfig{Pool: pool, Initiator: true})
	observers := []*net.UDPAddr{nodeAddr(t, tn.Nodes[1]), nodeAddr(t, tn.Nodes[2]), nodeAddr(t, tn.Nodes[3])}
	ctx, cancel := context.WithTimeout(context.Background(), punchWait)
	defer cancel()
	var answers int
	var err error
	callStub(t, "sampleNATFromPings", func() { answers, err = d.sampleNATFromPings(ctx, p, observers) })
	failIfStub(t, err)
	if err != nil {
		t.Fatalf("sampleNATFromPings: %v", err)
	}
	if answers != len(observers) {
		t.Errorf("answers to the pings = %d, want %d", answers, len(observers))
	}
	var fw uint64
	var addrs []Address
	callStub(t, "natFirewall", func() {
		fw = p.natFirewall()
		addrs = p.natAddresses()
	})
	if fw != firewallConsistent {
		t.Errorf("NAT firewall after three samples = %d, want consistent (%d)", fw, firewallConsistent)
	}
	if own := addressOf(nodeAddr(t, d)); len(addrs) != 1 || addrs[0] != own {
		t.Errorf("NAT addresses = %v, want the node's address %v", addrs, own)
	}
}

// Test case 3: a server that a client's probe reaches answers a punching probe with its own NAT state, and punches
// toward the client. The client's handshake goes through a holder, so the probe reaches the server through the
// relay, and the answer comes back through it. On loopback the server's samples give one consistent address, which
// the answer names, with punching set once the punch has started. The punch goes to the address the client
// named, from the server's own node. Upstream: lib/server.js _onpeerholepunch answers with the puncher's firewall,
// addresses and punching state, after setupHolepuncher has made the puncher for the admitted handshake.
func TestServerAnswersPunchingProbeAndPunches(t *testing.T) {
	tn := startTestnet(t, 10)
	host := testKeyPair(3)
	srv := newServer(t, tn.Nodes[0], ServerOptions{})
	listenOn(t, srv, host)
	relay := holderOtherThanServer(t, tn, host.Public, nodeAddr(t, tn.Nodes[0]))
	ans, secret, ok := punchHandshake(t, relay, host.Public, testKeyPair(5))
	if !ok {
		t.Fatal("the handshake through a holder got no answer")
	}
	client := punchListener(t)
	answer := probeServer(t, ans.payload.Holepunch, host.Public, secret, HolepunchPayload{
		Firewall:  firewallConsistent,
		Punching:  true,
		Addresses: []Address{addressOf(client.LocalAddr().(*net.UDPAddr))},
	})
	if !answer.Punching {
		t.Error("the answer to a punching probe does not say the server is punching")
	}
	serverNode := nodeAddr(t, tn.Nodes[0])
	if answer.Firewall != firewallConsistent {
		t.Errorf("the server's firewall in its answer = %d, want consistent (%d)", answer.Firewall, firewallConsistent)
	}
	if !slices.Contains(answer.Addresses, addressOf(serverNode)) {
		t.Errorf("the server's addresses in its answer = %v, want its address %v", answer.Addresses, addressOf(serverNode))
	}
	if from := expectPunch(t, client, punchWait); addressOf(from) != addressOf(serverNode) {
		t.Errorf("punch came from %v, want the server's node %v", from, serverNode)
	}
}

// Test case 4: each admitted handshake has a puncher of its own. Two clients with admitted handshakes probe the same
// server, and each client's probe socket gets a punch from the server's node.
func TestEachAdmittedHandshakeGetsItsOwnPuncher(t *testing.T) {
	tn := startTestnet(t, 10)
	host := testKeyPair(3)
	srv := newServer(t, tn.Nodes[0], ServerOptions{})
	listenOn(t, srv, host)
	relay := holderOtherThanServer(t, tn, host.Public, nodeAddr(t, tn.Nodes[0]))
	serverNode := addressOf(nodeAddr(t, tn.Nodes[0]))

	var probes []*net.UDPConn
	for i := range 2 {
		ans, secret, ok := punchHandshake(t, relay, host.Public, testKeyPair(byte(5+i)))
		if !ok {
			t.Fatalf("handshake %d through a holder got no answer", i)
		}
		probe := punchListener(t)
		probeServer(t, ans.payload.Holepunch, host.Public, secret, HolepunchPayload{
			Firewall:  firewallConsistent,
			Punching:  true,
			Addresses: []Address{addressOf(probe.LocalAddr().(*net.UDPAddr))},
		})
		probes = append(probes, probe)
	}
	var wg sync.WaitGroup
	for i, probe := range probes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			from, err := readPunch(probe, punchWait)
			if err != nil {
				t.Errorf("client %d got no punch: %v", i, err)
				return
			}
			if addressOf(from) != serverNode {
				t.Errorf("client %d got a punch from %v, want the server's node %v", i, from, serverNode)
			}
		}()
	}
	wg.Wait()
}

// Test case 5: a connect whose claims are made only by a hole punch connects, and data flows both ways. The test seam
// forcePunch on both nodes takes the direct path and the relay out of the claims, as forceRelay does for the relay:
// loopback always has a direct path, so without the seam the connect would not need a punch. The connect probes the
// server through the relay, sends its punch rounds, and connects its UDX stream to the punched address (upstream
// lib/connect.js probeRound, roundPunch and onsocket). The client punches from its own DHT socket, so the first check
// is that the client's punch socket is its node's address.
func TestConnectPunchesToServer(t *testing.T) {
	tn := startTestnet(t, 10)
	client, server := tn.Nodes[9], tn.Nodes[0]
	client.forcePunch = true
	server.forcePunch = true
	host := testKeyPair(3)
	kp := testKeyPair(5)
	srv := newServer(t, server, ServerOptions{})
	listenOn(t, srv, host)

	sock := punchSocketOf(t, client)
	var local *net.UDPAddr
	callStub(t, "Local", func() { local = sock.Local() })
	if addressOf(local) != addressOf(nodeAddr(t, client)) {
		t.Fatalf("the client's punch socket is %v, want its node's address %v", local, addressOf(nodeAddr(t, client)))
	}

	accepted := acceptNext(srv)
	c := dial(t, client, host.Public, ConnectOptions{KeyPair: &kp})
	s := awaitAccept(t, accepted, readWait)
	exchange(t, c, s)
}
