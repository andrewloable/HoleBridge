package hyperdht

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/pears/blindrelay"
	"github.com/andrewloable/HoleBridge/pears/noise"
	"github.com/andrewloable/HoleBridge/pears/protomux"
)

// relayPairWait bounds how long a test waits for the relay to admit a dial. Upstream dials the relay as soon as
// the handshake reply arrives, so a few seconds are enough on loopback.
const relayPairWait = 10 * time.Second

// relayQuiet is how long a test watches the relay for a connection that must not come.
const relayQuiet = 2 * time.Second

// relayLog is what a test relay saw: the keys its firewall was asked about, and the keys of the connections it
// admitted. Both lists are guarded by mu.
type relayLog struct {
	mu       sync.Mutex
	asked    [][32]byte
	admitted [][32]byte
}

// seen reports whether pk is in list.
func (r *relayLog) seen(list *[][32]byte, pk [32]byte) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Contains(*list, pk)
}

// awaitAdmitted waits until the relay has admitted a connection from pk, and fails the test when that does not
// happen within d.
func (r *relayLog) awaitAdmitted(t *testing.T, pk [32]byte, d time.Duration) {
	t.Helper()
	waitUntil(t, d, "not implemented: the relay admitted a connection from the DHT's default key pair (Connect does not dial the relay yet)", func() bool {
		return r.seen(&r.admitted, pk)
	})
}

// awaitAsked waits until the relay's firewall has been asked about pk, and fails the test when that does not
// happen within d.
func (r *relayLog) awaitAsked(t *testing.T, pk [32]byte, d time.Duration) {
	t.Helper()
	waitUntil(t, d, "not implemented: the relay's firewall was asked about the other default key pair (Connect does not dial the relay yet)", func() bool {
		return r.seen(&r.asked, pk)
	})
}

// expectNotAdmitted fails the test when the relay admits a connection from pk within d.
func (r *relayLog) expectNotAdmitted(t *testing.T, pk [32]byte, d time.Duration) {
	t.Helper()
	time.Sleep(d)
	if r.seen(&r.admitted, pk) {
		t.Error("the relay admitted a connection from a key pair it must refuse")
	}
}

// startRelay runs a relay on d under kp. Its firewall is firewall, or admits every key when firewall is nil, and
// each key it asks about is recorded. Each connection the relay admits gets a blind-relay server on the node's
// UDX socket, so a client can pair through it. The returned log records what the relay saw. t.Cleanup closes the
// relay.
func startRelay(t *testing.T, d *DHT, kp noise.KeyPair, firewall func([32]byte, HandshakePayload) bool) *relayLog {
	t.Helper()
	rec := &relayLog{}
	srv := newServer(t, d, ServerOptions{Firewall: func(pk [32]byte, p HandshakePayload) bool {
		rec.mu.Lock()
		rec.asked = append(rec.asked, pk)
		rec.mu.Unlock()
		return firewall != nil && firewall(pk, p)
	}})
	listenOn(t, srv, kp)
	bs := blindrelay.NewServer(blindrelay.ServerOptions{Socket: d.node.Socket()})
	go func() {
		for c := range acceptAll(srv) {
			pk := c.RemotePublicKey()
			rec.mu.Lock()
			rec.admitted = append(rec.admitted, pk)
			rec.mu.Unlock()
			bs.Accept(protomux.New(c), pk[:])
		}
	}()
	return rec
}

// newClientDHT starts a DHT node on the testnet tn whose default key pair is kp. The default key pair is the key
// a relay dial is made with (Config.DefaultKeyPair). The node is stopped when the test ends.
func newClientDHT(t *testing.T, tn *Testnet, kp noise.KeyPair) *DHT {
	t.Helper()
	def := kp
	d, err := New(Config{Bootstrap: tn.Bootstrap, DefaultKeyPair: &def})
	must(t, err)
	t.Cleanup(func() { d.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := d.node.Ready(ctx); err != nil {
		t.Fatalf("client node not ready: %v", err)
	}
	return d
}

// always returns a relay policy that offers the relay with public key pk on every call.
func always(pk [32]byte) func(bool) *[32]byte {
	return func(bool) *[32]byte { return &pk }
}

// Test case 1: a client whose RelayThrough always returns the relay connects to a server through it, and data
// flows both ways. The relay admits the client's dial, which upstream makes with the DHT's default key pair.
// On loopback a direct path to the server also works, and upstream prefers the direct path when it is
// there, so the bytes may not cross the relay. The test checks the dial to the relay and the data path.
func TestConnectThroughRelayCarriesData(t *testing.T) {
	tn := startTestnet(t, 10)
	relayKP := testKeyPair(7)
	relayed := startRelay(t, tn.Nodes[1], relayKP, nil)
	host := testKeyPair(3)
	client := testKeyPair(5)
	srv := newServer(t, tn.Nodes[0], ServerOptions{})
	listenOn(t, srv, host)
	accepted := acceptNext(srv)
	c := dial(t, tn.Nodes[9], host.Public, ConnectOptions{KeyPair: &client, RelayThrough: always(relayKP.Public)})
	s := awaitAccept(t, accepted, readWait)
	exchange(t, c, s)
	relayed.awaitAdmitted(t, tn.Nodes[9].keyPair.Public, relayPairWait)
}

// Test case 2: a relay whose firewall admits only one member key pair refuses a peer with another DefaultKeyPair.
// The member's DHT, whose default key pair is the admitted key, is admitted by the relay. The other DHT's dial to
// the relay is asked about and refused, and the relay never admits it. The Connect of the other DHT is not
// asserted: on loopback the direct path can still connect.
func TestRelayRefusesOtherDefaultKeyPair(t *testing.T) {
	tn := startTestnet(t, 10)
	member := testKeyPair(11)
	other := testKeyPair(13)
	// The client nodes are made before the servers, so the servers unannounce while the testnet still holds
	// the client nodes' routes; cleanups run in reverse order.
	memberDHT := newClientDHT(t, tn, member)
	otherDHT := newClientDHT(t, tn, other)
	relayKP := testKeyPair(7)
	relayed := startRelay(t, tn.Nodes[1], relayKP, func(pk [32]byte, _ HandshakePayload) bool {
		return pk != member.Public
	})
	host := testKeyPair(3)
	srv := newServer(t, tn.Nodes[0], ServerOptions{})
	listenOn(t, srv, host)
	accepted := acceptNext(srv)

	c := dial(t, memberDHT, host.Public, ConnectOptions{RelayThrough: always(relayKP.Public)})
	s := awaitAccept(t, accepted, readWait)
	exchange(t, c, s)
	relayed.awaitAdmitted(t, member.Public, relayPairWait)

	ctx, cancel := context.WithTimeout(context.Background(), relayPairWait)
	defer cancel()
	oc, err := otherDHT.Connect(ctx, host.Public, ConnectOptions{RelayThrough: always(relayKP.Public)})
	failIfStub(t, err)
	if oc != nil {
		t.Cleanup(func() { oc.Close() })
	}
	relayed.awaitAsked(t, other.Public, relayPairWait)
	relayed.expectNotAdmitted(t, other.Public, relayQuiet)
}

// Test case 3: with RelayThrough returning nil, connections stay direct. The policy is asked by the connect, and
// never asks for a forced relay, since no punch failed; and no dial from the DHT's default key pair reaches the
// relay.
func TestRelayThroughNilStaysDirect(t *testing.T) {
	tn := startTestnet(t, 10)
	relayKP := testKeyPair(7)
	relayed := startRelay(t, tn.Nodes[1], relayKP, nil)
	host := testKeyPair(3)
	client := testKeyPair(5)
	srv := newServer(t, tn.Nodes[0], ServerOptions{})
	listenOn(t, srv, host)
	accepted := acceptNext(srv)

	var mu sync.Mutex
	var asked []bool // the force flag of each call of the policy
	policy := func(force bool) *[32]byte {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, force)
		return nil
	}
	c := dial(t, tn.Nodes[9], host.Public, ConnectOptions{KeyPair: &client, RelayThrough: policy})
	s := awaitAccept(t, accepted, readWait)
	exchange(t, c, s)

	mu.Lock()
	calls := slices.Clone(asked)
	mu.Unlock()
	if len(calls) == 0 {
		t.Fatal("not implemented: Connect never asked the RelayThrough policy")
	}
	for _, force := range calls {
		if force {
			t.Error("the policy was asked with force true, but no punch failed")
		}
	}
	relayed.expectNotAdmitted(t, tn.Nodes[9].keyPair.Public, relayQuiet)
}

// Test case 4: a relay set on both sides lets the connection succeed. The server's ServerOptions.RelayThrough and
// the client's ConnectOptions.RelayThrough name the same relay; the connection succeeds, data flows, and the relay
// admits the dial from the client's DHT default key pair.
//
// The double-randomized NAT of the owner's case is not simulated here. The Connect path runs over real loopback
// UDP, and the simulated NATs of natsim_test.go drive the holepuncher only. The double-randomized abort of
// upstream is checked in holepunch_test.go (case 3). This test checks that a relay on both sides carries a
// connection on loopback.
func TestRelayOnBothSidesConnects(t *testing.T) {
	tn := startTestnet(t, 10)
	relayKP := testKeyPair(7)
	relayed := startRelay(t, tn.Nodes[1], relayKP, nil)
	host := testKeyPair(3)
	client := testKeyPair(5)
	srv := newServer(t, tn.Nodes[0], ServerOptions{RelayThrough: always(relayKP.Public)})
	listenOn(t, srv, host)
	accepted := acceptNext(srv)
	c := dial(t, tn.Nodes[9], host.Public, ConnectOptions{KeyPair: &client, RelayThrough: always(relayKP.Public)})
	s := awaitAccept(t, accepted, readWait)
	exchange(t, c, s)
	relayed.awaitAdmitted(t, tn.Nodes[9].keyPair.Public, relayPairWait)
}

// TestSelectRelayAsksPolicyOnce checks that selectRelay asks the policy once, with the force flag it is given,
// and returns the answer. A nil policy and a nil answer both mean a direct connection.
func TestSelectRelayAsksPolicyOnce(t *testing.T) {
	relayKP := testKeyPair(7)
	pk := relayKP.Public

	t.Run("a nil policy is direct", func(t *testing.T) {
		var got *[32]byte
		callStub(t, "selectRelay", func() { got = selectRelay(nil, false) })
		if got != nil {
			t.Error("selectRelay with a nil policy returned a relay, want nil")
		}
	})

	t.Run("the policy is asked once, with force, and its key is returned", func(t *testing.T) {
		var asked []bool
		policy := func(force bool) *[32]byte {
			asked = append(asked, force)
			return &pk
		}
		var got *[32]byte
		callStub(t, "selectRelay", func() { got = selectRelay(policy, true) })
		if !slices.Equal(asked, []bool{true}) {
			t.Errorf("the policy was asked with %v, want once with [true]", asked)
		}
		if got == nil || *got != pk {
			t.Error("selectRelay did not return the relay key the policy gave")
		}
	})

	t.Run("a nil answer is direct", func(t *testing.T) {
		var got *[32]byte
		callStub(t, "selectRelay", func() { got = selectRelay(func(bool) *[32]byte { return nil }, false) })
		if got != nil {
			t.Error("selectRelay returned a relay for a policy that answered nil, want nil")
		}
	})
}

// Test seam (HoleBridge-85m.7.7): dht.forceRelay makes a connect claim its stream only through a relay pairing.
// With no relay on either side nothing can claim it, so the connect fails, and the server accepts nothing, although
// the direct path would connect on loopback.
func TestForcedRelayWithoutRelayNeverConnects(t *testing.T) {
	tn := startTestnet(t, 10)
	host := testKeyPair(3)
	srv := newServer(t, tn.Nodes[0], ServerOptions{})
	listenOn(t, srv, host)
	accepted := acceptNext(srv)
	tn.Nodes[9].forceRelay = true
	err := dialErr(t, tn.Nodes[9], host.Public, ConnectOptions{}, dialWait)
	if !errors.Is(err, ErrPeerConnectionFailed) {
		t.Errorf("Connect failed with %v, want ErrPeerConnectionFailed", err)
	}
	select {
	case r := <-accepted:
		t.Errorf("the server accepted a connection (err %v) from a forced connect with no relay", r.err)
	case <-time.After(relayQuiet):
	}
}

// Relay data path (HoleBridge-85m.7.8): with the test seam forcing the relay path on both ends of a loopback
// connection, no stream claims the direct path, so the bytes can only cross the relay. Each side's dial to the relay
// is admitted under its DHT default key pair, and data flows both ways.
func TestForcedRelayPathCarriesData(t *testing.T) {
	tn := startTestnet(t, 10)
	relayKP := testKeyPair(7)
	relayed := startRelay(t, tn.Nodes[1], relayKP, nil)
	host := testKeyPair(3)
	client := testKeyPair(5)
	tn.Nodes[0].forceRelay = true
	srv := newServer(t, tn.Nodes[0], ServerOptions{})
	listenOn(t, srv, host)
	accepted := acceptNext(srv)
	tn.Nodes[9].forceRelay = true
	c := dial(t, tn.Nodes[9], host.Public, ConnectOptions{KeyPair: &client, RelayThrough: always(relayKP.Public)})
	s := awaitAccept(t, accepted, readWait)
	if s.RemotePublicKey() != client.Public {
		t.Error("the server's connection does not name the client's key pair")
	}
	exchange(t, c, s)
	relayed.awaitAdmitted(t, tn.Nodes[9].keyPair.Public, relayPairWait)
	relayed.awaitAdmitted(t, tn.Nodes[0].keyPair.Public, relayPairWait)
}
