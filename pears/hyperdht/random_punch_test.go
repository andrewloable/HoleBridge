package hyperdht

import (
	"bytes"
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/pears/dhtrpc"
)

// Expected values come from hyperdht 6.34.1: lib/connect.js roundPunch and tryLater (the randomized-punch wait and
// its 10 s to 20 s pause), lib/server.js _onpeerholepunch (TRY_LATER when randomized punches run, or abort when the
// handshake has no relay), and index.js (_randomPunchLimit 1, _randomPunchInterval 20 s).

// probeAnswer sends one punching probe for the handshake info of a server through the relay it names, from the
// client whose request function is send, and returns the decrypted answer. ok is false when the relay gives no
// answer that decrypts. Unlike probeServer, it does not retry an answer with an error.
func probeAnswer(t *testing.T, send func(context.Context, *net.UDPAddr, dhtrpc.Request) (*dhtrpc.Response, error), info *HolepunchInfo, host [32]byte, secret [32]byte, p HolepunchPayload) (HolepunchPayload, bool) {
	t.Helper()
	if info == nil || len(info.Relays) == 0 {
		t.Fatal("the handshake reply names no holepunch relays, so there is nothing to probe")
	}
	sp := newSecurePayload(secret)
	relay := info.Relays[0]
	target := hashKey(host)
	enc, err := sp.encrypt(p)
	must(t, err)
	msg, err := EncodeHolepunch(Holepunch{Mode: holepunchFromClient, ID: info.ID, Payload: enc, PeerAddress: &relay.PeerAddress})
	must(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := send(ctx, udpAddrOf(relay.RelayAddress), dhtrpc.Request{Command: cmdPeerHolepunch, Target: target[:], Value: msg})
	if err != nil || resp.Error != 0 {
		return HolepunchPayload{}, false
	}
	hp, err := DecodeHolepunch(resp.Value)
	if err != nil || hp.Mode != holepunchReply {
		return HolepunchPayload{}, false
	}
	reply, ok := sp.decrypt(hp.Payload)
	return reply, ok
}

// Test case 1: the DHT's randomized-punch gate allows one randomized punch at a time (dht._randomPunchLimit 1), and
// after one ends it waits randomPunchInterval (20 s) before the next (dht._lastRandomPunch). A punch that has not
// begun does not count.
func TestRandomPunchGateLimitsAndSpacesRandomizedPunches(t *testing.T) {
	g := &randomGate{limit: 1, interval: 20 * time.Second}
	now := time.Now()
	var first, second bool
	callStub(t, "randomGate.begin", func() { first = g.begin(now) })
	if !first {
		t.Fatal("the first randomized punch was refused with no punch running")
	}
	callStub(t, "randomGate.begin", func() { second = g.begin(now) })
	if second {
		t.Error("a second randomized punch began while the limit of one is in use")
	}
	callStub(t, "randomGate.end", func() { g.end(now) })
	var soon, later bool
	callStub(t, "randomGate.begin", func() { soon = g.begin(now.Add(5 * time.Second)) })
	if soon {
		t.Error("a randomized punch began 5 s after the last one ended, want the 20 s interval")
	}
	callStub(t, "randomGate.begin", func() { later = g.begin(now.Add(21 * time.Second)) })
	if !later {
		t.Error("a randomized punch was refused 21 s after the last one ended")
	}
}

// Test case 2: while randomized punches run on the DHT, a server answers a randomized client's punching probe with
// TRY_LATER instead of punching (upstream _onpeerholepunch). With a relay on the handshake, the answer keeps the
// puncher and echoes the client's token; the same probe during the interval after a punch gets TRY_LATER too. No
// punch is sent to the client. A handshake with no relay gets the same error as an abort (upstream _abort).
func TestServerAnswersTryLaterWhileRandomPunchesRun(t *testing.T) {
	tn := startTestnet(t, 10)
	host := testKeyPair(3)
	relayPub := testKeyPair(9).Public
	srv := newServer(t, tn.Nodes[0], ServerOptions{RelayThrough: always(relayPub)})
	listenOn(t, srv, host)
	relay := holderOtherThanServer(t, tn, host.Public, nodeAddr(t, tn.Nodes[0]))
	ans, secret, ok := punchHandshake(t, relay, host.Public, testKeyPair(5))
	if !ok {
		t.Fatal("the handshake through a holder got no answer")
	}
	client := punchListener(t)
	echo := bytes.Repeat([]byte{8}, 32)
	probe := HolepunchPayload{Firewall: firewallRandom, Punching: true, Token: echo, Addresses: []Address{addressOf(client.LocalAddr().(*net.UDPAddr))}}

	var gate *randomGate
	callStub(t, "gate", func() { gate = tn.Nodes[0].gate() })
	callStub(t, "gate.begin", func() { gate.begin(time.Now()) })
	sendProbe := func(what string, p HolepunchPayload) HolepunchPayload {
		reply, ok := probeAnswer(t, newClient(t).Request, ans.payload.Holepunch, host.Public, secret, p)
		if !ok {
			t.Fatalf("%s: the probe got no answer that decrypts", what)
		}
		return reply
	}
	reply := sendProbe("while a randomized punch runs", probe)
	if reply.Error != holepunchTryLater {
		t.Errorf("answer while a randomized punch runs has error %d, want TRY_LATER (%d)", reply.Error, holepunchTryLater)
	}
	if !bytes.Equal(reply.RemoteToken, echo) {
		t.Errorf("answer does not echo the client's token")
	}
	if n := countPunches(client, 300*time.Millisecond); n != 0 {
		t.Errorf("the server punched %d times while answering TRY_LATER, want none", n)
	}

	callStub(t, "gate.end", func() { gate.end(time.Now()) })
	reply = sendProbe("during the interval after a punch", probe)
	if reply.Error != holepunchTryLater {
		t.Errorf("answer during the 20 s interval has error %d, want TRY_LATER (%d)", reply.Error, holepunchTryLater)
	}

	noRelay := newServer(t, tn.Nodes[1], ServerOptions{})
	other := testKeyPair(4)
	listenOn(t, noRelay, other)
	relay2 := holderOtherThanServer(t, tn, other.Public, nodeAddr(t, tn.Nodes[1]))
	ans2, secret2, ok := punchHandshake(t, relay2, other.Public, testKeyPair(6))
	if !ok {
		t.Fatal("the second handshake through a holder got no answer")
	}
	callStub(t, "gate", func() { tn.Nodes[1].gate().begin(time.Now()) })
	reply2, ok := probeAnswer(t, newClient(t).Request, ans2.payload.Holepunch, other.Public, secret2, probe)
	if !ok {
		t.Fatal("the probe to the handshake without a relay got no answer that decrypts")
	}
	if reply2.Error != holepunchTryLater {
		t.Errorf("answer to a handshake with no relay has error %d, want TRY_LATER as an abort (%d)", reply2.Error, holepunchTryLater)
	}
}

// Test case 3: fast open. A consistent server whose probe names the server's own NAT address (the client's session
// was opened toward it, RemoteAddress) punches back to the client at once, without waiting for the client's punch
// (upstream _onpeerholepunch: "Fast mode"). The client here is a DHT node, so its punch handler sees the datagram.
func TestFastOpenPunchesBackOnProbeNamingOurAddress(t *testing.T) {
	tn := startTestnet(t, 10)
	host := testKeyPair(3)
	srv := newServer(t, tn.Nodes[0], ServerOptions{})
	listenOn(t, srv, host)
	relay := holderOtherThanServer(t, tn, host.Public, nodeAddr(t, tn.Nodes[0]))
	ans, secret, ok := punchHandshake(t, relay, host.Public, testKeyPair(5))
	if !ok {
		t.Fatal("the handshake through a holder got no answer")
	}
	client := tn.Nodes[9]
	got := make(chan *net.UDPAddr, 4)
	callStub(t, "OnPunch", func() { punchSocketOf(t, client).OnPunch(func(from *net.UDPAddr) { got <- from }) })

	serverNode := nodeAddr(t, tn.Nodes[0])
	reply, ok := probeAnswer(t, client.node.Request, ans.payload.Holepunch, host.Public, secret, HolepunchPayload{
		Firewall:      firewallConsistent,
		Punching:      false,
		RemoteAddress: ptr(addressOf(serverNode)),
		Addresses:     []Address{addressOf(nodeAddr(t, client))},
	})
	if !ok || reply.Error != 0 {
		t.Fatalf("the probe got no good answer (ok %v, error %d)", ok, reply.Error)
	}
	select {
	case from := <-got:
		if addressOf(from) != addressOf(serverNode) {
			t.Errorf("fast-open punch came from %v, want the server's node %v", from, serverNode)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the server did not punch back at once to a probe that named its own address")
	}
}

// Test case 4: the NAT sample of a relayed probe. The probe reaches the server through a relay, which sees the server
// at the address the probe names (req.to) and forwards it from itself (req.from): that is a NAT sample of the server's
// puncher from the relay (upstream _onpeerholepunch: p.nat.add(req.to, req.from)). The sample counts once per relay.
func TestRelayedProbeAddsNATSample(t *testing.T) {
	tn := startTestnet(t, 10)
	host := testKeyPair(3)
	srv := newServer(t, tn.Nodes[0], ServerOptions{})
	listenOn(t, srv, host)
	relay := holderOtherThanServer(t, tn, host.Public, nodeAddr(t, tn.Nodes[0]))
	ans, _, ok := punchHandshake(t, relay, host.Public, testKeyPair(5))
	if !ok {
		t.Fatal("the handshake through a holder got no answer")
	}
	seen := addr("198.51.100.44", 40444)
	observer := addr("203.0.113.44", 6881)
	callStub(t, "observeProbe", func() { srv.observeProbe(ans.payload.Holepunch.ID, seen, observer) })
	srv.mu.Lock()
	slot := srv.holepunches[ans.payload.Holepunch.ID]
	srv.mu.Unlock()
	var sampled bool
	if slot != nil && slot.p != nil {
		callStub(t, "sampledFrom", func() { sampled = slot.p.sampledFrom(observer) })
	}
	if !sampled {
		t.Error("the puncher did not take a NAT sample from the relay that forwarded the probe")
	}
}

// Test case 5: a client that meets TRY_LATER waits and punches again. The client's probe round is done; its round
// punch gets TRY_LATER while the server's randomized punch gate is busy, so the client pauses and sends the punch
// again, and after the gate frees, the punch starts (upstream roundPunch: tryLater, then roundPunch again).
func TestClientWaitsOutTryLaterThenPunches(t *testing.T) {
	tn := startTestnet(t, 10)
	host := testKeyPair(3)
	relayPub := testKeyPair(9).Public
	srv := newServer(t, tn.Nodes[0], ServerOptions{RelayThrough: always(relayPub)})
	listenOn(t, srv, host)
	relay := holderOtherThanServer(t, tn, host.Public, nodeAddr(t, tn.Nodes[0]))
	ans, secret, ok := punchHandshake(t, relay, host.Public, testKeyPair(5))
	if !ok {
		t.Fatal("the handshake through a holder got no answer")
	}
	var gate *randomGate
	callStub(t, "gate", func() { gate = tn.Nodes[0].gate() })
	callStub(t, "gate.begin", func() { gate.begin(time.Now()) })

	saved := tryLaterBase
	tryLaterBase = 50 * time.Millisecond
	tryLaterSpread = 0
	t.Cleanup(func() { tryLaterBase, tryLaterSpread = saved, 10*time.Second })

	client := tn.Nodes[9]
	cp := &connectPunch{
		a:      &attempt{d: client, ctx: context.Background(), offer: &relayOffer{key: relayPub}},
		pool:   client.punchPool(),
		sp:     newSecurePayload(secret),
		id:     ans.payload.Holepunch.ID,
		target: keyTarget(host.Public),
	}
	cp.p = newPuncher(t, punchConfig{
		Pool:           cp.pool,
		Initiator:      true,
		RemoteFirewall: firewallConsistent,
		Timing:         punchTiming{BirthdaySockets: 4},
	})
	// Three observers see the client at three ports of its one address, the address the relay sees: the NAT is
	// randomized, and the server verifies that address by the token it echoes.
	for i, o := range []Address{addr("203.0.113.11", 6881), addr("203.0.113.12", 6881), addr("203.0.113.13", 6881)} {
		cp.p.observe(Address{Host: netip.MustParseAddr("127.0.0.1"), Port: uint16(40000 + i)}, o)
	}
	serverAddr := addressOf(nodeAddr(t, tn.Nodes[0]))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cp.a.ctx = ctx
	token, peer, err := cp.probe(ctx, &serverAddr, ans.payload.Holepunch.Relays[0], true)
	if err != nil {
		t.Fatalf("probe round: %v", err)
	}
	// Free the gate after the first TRY_LATER has been answered.
	go func() {
		time.Sleep(300 * time.Millisecond)
		gate.end(time.Now().Add(-time.Hour))
	}()
	var punchErr error
	callStub(t, "punch", func() { punchErr = cp.punch(ctx, peer, token, ans.payload.Holepunch.Relays[0].RelayAddress) })
	if punchErr != nil {
		t.Fatalf("round punch after TRY_LATER: %v, want the punch to go on once the gate frees", punchErr)
	}
	if !cp.p.isPunching() && !cp.p.connected() {
		t.Error("the client's puncher is not punching after the wait")
	}
	cp.p.destroy()
}
