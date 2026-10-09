package hyperdht

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/blake2b"

	"github.com/andrewloable/HoleBridge/pears/dhtrpc"
	"github.com/andrewloable/HoleBridge/pears/noise"
	"github.com/andrewloable/HoleBridge/pears/udx"
)

// upstreamNSPeerHandshake is NS.PEER_HANDSHAKE of hyperdht 6.34.1: the namespace of hyperswarm/dht and
// command 0, which is the Noise prologue of every handshake. The value is printed by lib/constants.js.
const upstreamNSPeerHandshake = "14d6d4b49214ab1033ed204976caa258bae9e1e8543b9ad1fd996a910b0c4e3a"

// handshakeWait bounds the wait for the answer to a raw handshake. The DHT request gives up at its own
// deadline, about 4 s, so an unanswered handshake returns sooner. Upstream's dial reports its refusal after
// about 8 s (docs/spike-m1.md). A raw client needs only the absence of an answer.
const handshakeWait = 8 * time.Second

// hashKey returns the target that a server announces its key pair on: the BLAKE2b-256 of the public key,
// as upstream's unslabbedHash gives it.
func hashKey(pk [32]byte) [32]byte {
	return blake2b.Sum256(pk[:])
}

// callCreateServer calls d.CreateServer. The stub panics, so the panic becomes a test failure here, which
// lets the other tests run.
func callCreateServer(t *testing.T, d *DHT, opts ServerOptions) *Server {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("CreateServer: %v", r)
		}
	}()
	return d.CreateServer(opts)
}

// newServer returns a server on d, and closes it when the test ends.
func newServer(t *testing.T, d *DHT, opts ServerOptions) *Server {
	t.Helper()
	srv := callCreateServer(t, d, opts)
	t.Cleanup(func() { srv.Close() })
	return srv
}

// listenOn starts srv on kp with a 30 s deadline, and fails the test on error.
func listenOn(t *testing.T, srv *Server, kp noise.KeyPair) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Listen(ctx, kp); err != nil {
		failIfStub(t, err)
		t.Fatalf("Listen: %v", err)
	}
}

// closeServer closes srv, and fails the test on error.
func closeServer(t *testing.T, srv *Server) {
	t.Helper()
	if err := srv.Close(); err != nil {
		failIfStub(t, err)
		t.Fatalf("Close: %v", err)
	}
}

// acceptAll calls srv.Accept until it fails. It passes each connection on the returned channel, and closes
// the channel when Accept fails.
func acceptAll(srv *Server) <-chan *AcceptedConn {
	conns := make(chan *AcceptedConn)
	go func() {
		defer close(conns)
		for {
			c, err := srv.Accept()
			if err != nil {
				return
			}
			conns <- c
		}
	}()
	return conns
}

// expectNoAccepts fails the test for each connection that conns passes on. It also fails when conns is not
// closed within 10 s, which means Accept did not return an error after Close.
func expectNoAccepts(t *testing.T, conns <-chan *AcceptedConn) {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case _, ok := <-conns:
			if !ok {
				return
			}
			t.Error("Accept returned a connection")
		case <-timeout:
			t.Fatal("Accept did not return an error within 10 s of Close")
		}
	}
}

// waitUntil polls cond every 10 ms until it holds, and fails the test when it does not hold within d.
func waitUntil(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within %v", what, d)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// mustReply sends req to addr with client and returns the reply. It fails the test when the request gets
// no reply, and what names the request in the message.
func mustReply(t *testing.T, client *dhtrpc.IO, addr *net.UDPAddr, req dhtrpc.Request, what string) *dhtrpc.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := client.Request(ctx, addr, req)
	if err != nil {
		failIfStub(t, err)
		t.Fatalf("%s got no reply: %v", what, err)
	}
	return resp
}

// lookupAt sends a LOOKUP for target to addr. The reply carries the token that an ANNOUNCE or UNANNOUNCE to
// that node needs, and the node's id, which the signature covers.
func lookupAt(t *testing.T, client *dhtrpc.IO, addr *net.UDPAddr, target [32]byte) *dhtrpc.Response {
	t.Helper()
	return mustReply(t, client, addr, dhtrpc.Request{Command: cmdLookup, Target: target[:]}, "LOOKUP")
}

// encodeAnnounce returns the encoding of m, and fails the test on error.
func encodeAnnounce(t *testing.T, m Announce) []byte {
	t.Helper()
	value, err := EncodeAnnounce(m)
	must(t, err)
	return value
}

// signedValue returns the value of an ANNOUNCE (ns nsAnnounce) or an UNANNOUNCE (ns nsUnannounce) that kp
// sends for target to a node which answered a LOOKUP with token and id. The signature covers the peer, the
// refresh (nil when absent) and the node's id, as upstream's signAnnounce does.
func signedValue(t *testing.T, kp noise.KeyPair, target [32]byte, token, id []byte, peer Peer, refresh []byte, ns [32]byte) []byte {
	t.Helper()
	signable, err := annSignable(target[:], token, id, peer, refresh, ns)
	must(t, err)
	sig := ed25519.Sign(ed25519.PrivateKey(kp.Secret[:]), signable)
	return encodeAnnounce(t, Announce{Peer: &peer, Refresh: refresh, Signature: sig})
}

// storedAt reports whether the node at addr returns a record of the key pk for target in its LOOKUP reply.
func storedAt(t *testing.T, client *dhtrpc.IO, addr *net.UDPAddr, target [32]byte, pk [32]byte) bool {
	t.Helper()
	resp := lookupAt(t, client, addr, target)
	if len(resp.Value) == 0 {
		return false
	}
	raw, err := DecodeLookupRawReply(resp.Value)
	must(t, err)
	for _, p := range raw.Peers {
		if bytes.Equal(p.PublicKey, pk[:]) {
			return true
		}
	}
	return false
}

// expectBareReply fails the test unless resp has no error code, no token and no closer nodes. That is what
// upstream's ANNOUNCE and UNANNOUNCE replies carry (token false, closerNodes false).
func expectBareReply(t *testing.T, what string, resp *dhtrpc.Response) {
	t.Helper()
	if resp.Error != 0 {
		t.Errorf("%s reply: error %d, want 0", what, resp.Error)
	}
	if resp.Token != nil {
		t.Errorf("%s reply carries a token, want none", what)
	}
	if len(resp.CloserNodes) != 0 {
		t.Errorf("%s reply carries %d closer nodes, want none", what, len(resp.CloserNodes))
	}
}

// peerReply is a FIND_PEER answer: the node that sent it, and the record it returned.
type peerReply struct {
	from *net.UDPAddr
	peer Peer
}

// findPeerAll sends a FIND_PEER for target to every node of tn at once, and returns the answers that carry a
// record. A node with no record may stay silent, so such a request ends at its deadline.
func findPeerAll(t *testing.T, tn *Testnet, target [32]byte) []peerReply {
	t.Helper()
	client := newClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	answers := make([]*peerReply, len(tn.Nodes))
	var wg sync.WaitGroup
	for i, n := range tn.Nodes {
		addr := nodeAddr(t, n)
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := client.Request(ctx, addr, dhtrpc.Request{Command: cmdFindPeer, Target: target[:]})
			if err != nil || len(resp.Value) == 0 {
				return
			}
			if p, err := DecodePeer(resp.Value); err == nil {
				answers[i] = &peerReply{from: addr, peer: p}
			}
		}()
	}
	wg.Wait()
	var out []peerReply
	for _, a := range answers {
		if a != nil {
			out = append(out, *a)
		}
	}
	return out
}

// rawAnswer is the answer to a raw handshake: the server's payload, and the static key that the answer
// proves.
type rawAnswer struct {
	payload   HandshakePayload
	serverKey [32]byte
}

// sendHandshake sends a raw Noise IK handshake from kp to the server whose public key is host, the way a
// client of the DHT does. It finds the node that holds the server's record with FIND_PEER on the hash of
// host, sends the PEER_HANDSHAKE request to that node, and waits for the answer. It returns false when no
// node holds the record, or when no answer arrives that decodes as a handshake reply.
func sendHandshake(t *testing.T, tn *Testnet, host [32]byte, kp noise.KeyPair) (rawAnswer, bool) {
	t.Helper()
	prologue := dhtNamespace(cmdPeerHandshake)
	if got := hex.EncodeToString(prologue[:]); got != upstreamNSPeerHandshake {
		t.Fatalf("handshake prologue is %s, want the upstream NS.PEER_HANDSHAKE", got)
	}
	target := hashKey(host)
	var relay *net.UDPAddr
	for _, a := range findPeerAll(t, tn, target) {
		if bytes.Equal(a.peer.PublicKey, host[:]) {
			relay = a.from
			break
		}
	}
	if relay == nil {
		return rawAnswer{}, false
	}
	return handshakeAt(t, relay, host, kp)
}

// handshakeAt sends the raw handshake of sendHandshake to the node at relay, which holds the record of the
// server whose public key is host. It returns false when no answer arrives that decodes as a handshake reply.
func handshakeAt(t *testing.T, relay *net.UDPAddr, host [32]byte, kp noise.KeyPair) (rawAnswer, bool) {
	t.Helper()
	prologue := dhtNamespace(cmdPeerHandshake)
	target := hashKey(host)

	payload, err := EncodeNoisePayload(NoisePayload{
		UDX:          &UDXInfo{Version: 1, ID: 1},
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
	resp, err := newClient(t).Request(ctx, relay, dhtrpc.Request{Command: cmdPeerHandshake, Target: target[:], Value: value})
	if err != nil || resp.Error != 0 || len(resp.Value) == 0 {
		return rawAnswer{}, false
	}
	ans, err := DecodeHandshake(resp.Value)
	if err != nil || ans.Mode != handshakeReply || len(ans.Noise) == 0 {
		return rawAnswer{}, false
	}
	body, err := hs.Recv(ans.Noise)
	if err != nil || !hs.Complete() {
		return rawAnswer{}, false
	}
	p, err := DecodeNoisePayload(body)
	if err != nil {
		return rawAnswer{}, false
	}
	_, _, _, serverKey := hs.Result()
	return rawAnswer{payload: p, serverKey: serverKey}, true
}

// Test case 1: Listen announces the key pair. A Lookup of the hash of its public key, from another node,
// finds the server's record.
func TestListenAnnouncesKeyPair(t *testing.T) {
	tn := startTestnet(t, 10)
	host := testKeyPair(3)
	srv := newServer(t, tn.Nodes[0], ServerOptions{})
	listenOn(t, srv, host)
	if _, ok := peerOf(lookupAll(t, tn.Nodes[9], hashKey(host.Public)), host.Public); !ok {
		t.Fatal("Lookup from node 9 found no record for the key pair that Listen announced")
	}
}

// The announce of a key on the hash of that key is the router's record of the key (upstream persistent.js
// onannounce, announceSelf), not an ordinary record. FIND_PEER answers from the router with that record and
// the relay addresses it was announced with. Listen depends on this route.
func TestKeyHashAnnounceAnswersFindPeer(t *testing.T) {
	tn := startTestnet(t, 10)
	kp := testKeyPair(4)
	relays := testRelays()
	announceFrom(t, tn.Nodes[0], hashKey(kp.Public), kp, relays)
	for _, a := range findPeerAll(t, tn, hashKey(kp.Public)) {
		if bytes.Equal(a.peer.PublicKey, kp.Public[:]) && slices.Equal(a.peer.RelayAddresses, relays) {
			return
		}
	}
	t.Fatal("FIND_PEER on the hash of the key found no record with the relay addresses it was announced with")
}

// Handshake routing: a client whose key the firewall admits gets the server's answer to its handshake. The
// handshake reaches the server through the node that holds the server's record, and the answer proves the
// server's key. This is the control for TestFirewallRefusesClient: a refused client gets no answer from the
// firewall, not from a broken route.
func TestHandshakeRoutedToAdmittedClient(t *testing.T) {
	tn := startTestnet(t, 10)
	host := testKeyPair(3)
	srv := newServer(t, tn.Nodes[0], ServerOptions{Firewall: func([32]byte, HandshakePayload) bool { return false }})
	listenOn(t, srv, host)
	ans, ok := sendHandshake(t, tn, host.Public, testKeyPair(5))
	if !ok {
		t.Fatal("the admitted client got no answer to its handshake")
	}
	if ans.serverKey != host.Public {
		t.Error("the answer does not prove the server's key")
	}
	if ans.payload.Error != 0 {
		t.Errorf("answer error = %d, want 0", ans.payload.Error)
	}
}

// Test case 2: a client whose key the firewall refuses never gets a connection. Its handshake gets no answer,
// the firewall is called once with its key, and Accept never returns a connection.
func TestFirewallRefusesClient(t *testing.T) {
	tn := startTestnet(t, 10)
	host := testKeyPair(3)
	refused := testKeyPair(5)
	var mu sync.Mutex
	var calls [][32]byte // the keys that the firewall was called with
	srv := newServer(t, tn.Nodes[0], ServerOptions{Firewall: func(pk [32]byte, _ HandshakePayload) bool {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, pk)
		return pk == refused.Public
	}})
	listenOn(t, srv, host)
	conns := acceptAll(srv)

	if _, ok := sendHandshake(t, tn, host.Public, refused); ok {
		t.Error("the refused client got an answer to its handshake")
	}
	waitUntil(t, 5*time.Second, "the firewall call for the refused handshake", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(calls) > 0
	})
	mu.Lock()
	got := slices.Clone(calls)
	mu.Unlock()
	if len(got) != 1 || got[0] != refused.Public {
		t.Errorf("firewall was called %d times, want once with the refused client's key", len(got))
	}
	closeServer(t, srv)
	expectNoAccepts(t, conns)
}

// Test case 3: Close unannounces the key pair and stops Accept. A Lookup from another node no longer finds
// the server's record, a handshake gets no answer, and Accept returns an error with no connection.
func TestCloseUnannouncesAndStopsAccepting(t *testing.T) {
	tn := startTestnet(t, 10)
	host := testKeyPair(3)
	srv := newServer(t, tn.Nodes[0], ServerOptions{})
	listenOn(t, srv, host)
	target := hashKey(host.Public)
	if _, ok := peerOf(lookupAll(t, tn.Nodes[9], target), host.Public); !ok {
		t.Fatal("before Close: Lookup found no record, so removal cannot be tested")
	}
	conns := acceptAll(srv)
	closeServer(t, srv)
	if _, ok := peerOf(lookupAll(t, tn.Nodes[9], target), host.Public); ok {
		t.Error("after Close: Lookup still returns the record of the closed server")
	}
	if _, ok := sendHandshake(t, tn, host.Public, testKeyPair(5)); ok {
		t.Error("after Close: the server still answers handshakes")
	}
	expectNoAccepts(t, conns)
}

// Refresh-only ANNOUNCE (upstream persistent.js _onrefresh). An ANNOUNCE may carry the hash of a refresh
// token. A later ANNOUNCE with no peer that carries the token itself renews the record it belongs to. The
// test removes the record with an UNANNOUNCE first, so the renewal shows: the record is back in a LOOKUP
// after the refresh-only ANNOUNCE. A token the node never saw gets no reply.
func TestRefreshOnlyAnnounceRestoresRecord(t *testing.T) {
	tn := startTestnet(t, 10)
	client := newClient(t)
	addr := nodeAddr(t, tn.Nodes[1])
	target := testTarget("holebridge refresh test")
	kp := testKeyPair(6)
	refreshToken := bytes.Repeat([]byte{0x31}, 32)
	refreshHash := blake2b.Sum256(refreshToken)

	lk := lookupAt(t, client, addr, target)
	value := signedValue(t, kp, target, lk.Token, lk.ID, Peer{PublicKey: kp.Public[:], RelayAddresses: testRelays()}, refreshHash[:], nsAnnounce)
	resp := mustReply(t, client, addr, dhtrpc.Request{Command: cmdAnnounce, Target: target[:], Token: lk.Token, Value: value}, "ANNOUNCE with a refresh")
	if resp.Error != 0 {
		t.Fatalf("ANNOUNCE with a refresh: error %d, want 0", resp.Error)
	}
	if !storedAt(t, client, addr, target, kp.Public) {
		t.Fatal("ANNOUNCE with a refresh: the record is not stored")
	}

	lk = lookupAt(t, client, addr, target)
	value = signedValue(t, kp, target, lk.Token, lk.ID, Peer{PublicKey: kp.Public[:]}, nil, nsUnannounce)
	resp = mustReply(t, client, addr, dhtrpc.Request{Command: cmdUnannounce, Target: target[:], Token: lk.Token, Value: value}, "UNANNOUNCE")
	if resp.Error != 0 {
		t.Fatalf("UNANNOUNCE: error %d, want 0", resp.Error)
	}
	if storedAt(t, client, addr, target, kp.Public) {
		t.Fatal("UNANNOUNCE did not remove the record, so the refresh cannot be seen")
	}

	lk = lookupAt(t, client, addr, target)
	value = encodeAnnounce(t, Announce{Refresh: refreshToken})
	resp = mustReply(t, client, addr, dhtrpc.Request{Command: cmdAnnounce, Target: target[:], Token: lk.Token, Value: value}, "refresh-only ANNOUNCE")
	if resp.Error != 0 {
		t.Fatalf("refresh-only ANNOUNCE: error %d, want 0", resp.Error)
	}
	if !storedAt(t, client, addr, target, kp.Public) {
		t.Error("after the refresh-only ANNOUNCE the record is not stored again")
	}

	lk = lookupAt(t, client, addr, target)
	value = encodeAnnounce(t, Announce{Refresh: bytes.Repeat([]byte{0x32}, 32)})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	_, err := client.Request(ctx, addr, dhtrpc.Request{Command: cmdAnnounce, Target: target[:], Token: lk.Token, Value: value})
	cancel()
	if err == nil {
		t.Error("refresh-only ANNOUNCE with a token the node never saw got a reply")
	}
}

// Upstream answers ANNOUNCE and UNANNOUNCE with token false and closerNodes false (persistent.js), so the
// reply carries neither a token nor closer nodes. This node's IO adds a token to every reply that has none,
// so a handler that answers with no token must opt out of it.
func TestAnnounceRepliesCarryNoToken(t *testing.T) {
	tn := startTestnet(t, 10)
	client := newClient(t)
	addr := nodeAddr(t, tn.Nodes[2])
	target := testTarget("holebridge reply test")
	kp := testKeyPair(7)

	lk := lookupAt(t, client, addr, target)
	value := signedValue(t, kp, target, lk.Token, lk.ID, Peer{PublicKey: kp.Public[:], RelayAddresses: testRelays()}, nil, nsAnnounce)
	expectBareReply(t, "ANNOUNCE", mustReply(t, client, addr, dhtrpc.Request{Command: cmdAnnounce, Target: target[:], Token: lk.Token, Value: value}, "ANNOUNCE"))

	lk = lookupAt(t, client, addr, target)
	value = signedValue(t, kp, target, lk.Token, lk.ID, Peer{PublicKey: kp.Public[:]}, nil, nsUnannounce)
	expectBareReply(t, "UNANNOUNCE", mustReply(t, client, addr, dhtrpc.Request{Command: cmdUnannounce, Target: target[:], Token: lk.Token, Value: value}, "UNANNOUNCE"))
}

// TestSlowFirewallDoesNotStallTheNode blocks the server's firewall, then checks that the server's node still
// does its other work: a Lookup from that node finds the server's record while the firewall is still blocked.
// The firewall runs off the DHT read loop, so the replies the Lookup waits for are read.
func TestSlowFirewallDoesNotStallTheNode(t *testing.T) {
	tn := startTestnet(t, 10)
	host := testKeyPair(3)
	client := testKeyPair(5)
	release := make(chan struct{})
	asked := make(chan struct{}, 1)
	srv := newServer(t, tn.Nodes[0], ServerOptions{Firewall: func([32]byte, HandshakePayload) bool {
		select {
		case asked <- struct{}{}:
		default:
		}
		<-release
		return false
	}})
	t.Cleanup(func() { close(release) }) // runs before srv.Close, so no firewall call is left blocked
	listenOn(t, srv, host)

	ctx, cancel := context.WithTimeout(context.Background(), readWait)
	t.Cleanup(cancel)
	go tn.Nodes[9].Connect(ctx, host.Public, ConnectOptions{KeyPair: &client})
	select {
	case <-asked:
	case <-time.After(readWait):
		t.Fatal("the firewall was never asked about the client's handshake")
	}

	lookupCtx, lookupCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer lookupCancel()
	var got []LookupResult
	for r := range startLookup(t, tn.Nodes[0], lookupCtx, hashKey(host.Public)) {
		got = append(got, r)
	}
	if _, ok := peerOf(got, host.Public); !ok {
		t.Error("a Lookup from the server's node found no record within 3 s while the firewall was blocked")
	}
}

// admitRawStream sends a raw handshake from the DHT node d to the server whose public key is host, naming st as the
// client's UDX stream. It returns the address of the node that admitted the handshake and the server's UDX stream id
// from the reply. When direct is not nil the handshake goes straight to that node, so it comes direct; otherwise it goes
// to the nodes that hold the server's record, and a holder's handshake is relayed. It fails the test when no node admits
// it.
func admitRawStream(t *testing.T, d *DHT, host [32]byte, st *udx.Stream, direct *net.UDPAddr) (*net.UDPAddr, uint32) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	target := keyTarget(host)
	hs := noise.NewInitiator(d.keyPair, host, nsPeerHandshake[:])
	payload, err := EncodeNoisePayload(NoisePayload{
		UDX:          &UDXInfo{Version: 1, ID: uint64(st.ID())},
		SecretStream: &SecretStreamInfo{Version: 1},
	})
	must(t, err)
	msg1, err := hs.Send(payload)
	must(t, err)
	value, err := EncodeHandshake(Handshake{Mode: handshakeFromClient, Noise: msg1})
	must(t, err)
	var nodes <-chan *net.UDPAddr
	if direct != nil {
		one := make(chan *net.UDPAddr, 1)
		one <- direct
		close(one)
		nodes = one
	} else {
		nodes = d.holders(ctx, target)
	}
	for addr := range nodes {
		resp, err := d.node.Request(ctx, addr, dhtrpc.Request{Command: cmdPeerHandshake, Target: target[:], Value: value})
		if err != nil || resp.Error != 0 {
			continue
		}
		h, err := DecodeHandshake(resp.Value)
		if err != nil || h.Mode != handshakeReply {
			continue
		}
		done := *hs
		body, err := done.Recv(h.Noise)
		if err != nil {
			continue
		}
		p, err := DecodeNoisePayload(body)
		if err != nil || p.Error != 0 || p.UDX == nil {
			continue
		}
		return addr, uint32(p.UDX.ID)
	}
	t.Fatal("no node admitted the raw handshake")
	return nil, 0
}

// Test for HoleBridge-85m.5.11: an admitted client that never sends its secret stream header does not hold its
// stream. The server's header exchange has headerExchangeWait; when it expires the server closes the stream, so the
// client's read ends, and Accept returns no connection. The handshake goes straight to the server's node, so it comes
// direct and the server claims the stream at admission, as upstream does. A relayed handshake's stream is not claimed
// at admission (its puncher or its relay pairing claims it), so nothing here would connect it to the client.
func TestAdmittedStreamWithoutHeaderIsDropped(t *testing.T) {
	tn := startTestnet(t, 10)
	host := testKeyPair(3)
	srv := newServer(t, tn.Nodes[0], ServerOptions{})
	listenOn(t, srv, host)
	accepted := acceptNext(srv)

	d := tn.Nodes[9]
	st := d.newStream()
	t.Cleanup(func() { st.Destroy() })
	addr, id := admitRawStream(t, d, host.Public, st, nodeAddr(t, tn.Nodes[0]))
	must(t, st.Connect(id, addr))

	// The client sends nothing on the stream. The read ends once the server drops the stream.
	ended := make(chan struct{})
	go func() {
		st.Read(make([]byte, 1))
		close(ended)
	}()
	select {
	case <-ended:
	case <-time.After(headerExchangeWait + 5*time.Second):
		t.Fatalf("the server still held the stream %v after the admitted handshake", headerExchangeWait+5*time.Second)
	}
	select {
	case r := <-accepted:
		t.Errorf("Accept returned a connection (err %v) for a client that never sent its header", r.err)
	default:
	}
}

// Test for HoleBridge-85m.5.26: a stream that no path claims is given up when its admitted handshake clears, as upstream
// drops a relayed stream that nothing took. A stream that a path has claimed is left up when the handshake clears.
func TestUnclaimedStreamIsGivenUpWhenHandshakeClears(t *testing.T) {
	tn := startTestnet(t, 2)
	d := tn.Nodes[0]
	srv := newServer(t, d, ServerOptions{})
	unclaimed, claimed := d.newStream(), d.newStream()
	t.Cleanup(func() {
		unclaimed.Destroy()
		claimed.Destroy()
	})
	var unclaimedClaim, claimedClaim streamClaim
	claimedClaim.take()
	srv.keepHolepunchWith(srv.reserveHolepunch(), [32]byte{1}, false, unclaimed, &unclaimedClaim)
	srv.keepHolepunchWith(srv.reserveHolepunch(), [32]byte{2}, false, claimed, &claimedClaim)

	ended := make(chan struct{})
	go func() {
		unclaimed.Read(make([]byte, 1))
		close(ended)
	}()
	select {
	case <-ended:
	case <-time.After(handshakeClearWait + 5*time.Second):
		t.Fatalf("the unclaimed stream was still up %v after its handshake cleared", handshakeClearWait+5*time.Second)
	}

	up := make(chan struct{})
	go func() {
		claimed.Read(make([]byte, 1))
		close(up)
	}()
	select {
	case <-up:
		t.Error("the claimed stream was given up when its handshake cleared")
	case <-time.After(500 * time.Millisecond):
	}
}

// Test for HoleBridge-85m.5.13: a second server on a key pair that a server on the same DHT already listens on is
// refused with ErrKeyPairAlreadyUsed, and the first server keeps its record. Once the first server closes, the key
// pair is free to listen again.
func TestSecondServerOnKeyPairRefused(t *testing.T) {
	tn := startTestnet(t, 10)
	host := testKeyPair(3)
	first := newServer(t, tn.Nodes[0], ServerOptions{})
	listenOn(t, first, host)
	second := newServer(t, tn.Nodes[0], ServerOptions{})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := second.Listen(ctx, host)
	if !errors.Is(err, ErrKeyPairAlreadyUsed) {
		t.Fatalf("second Listen returned %v, want ErrKeyPairAlreadyUsed", err)
	}
	if _, ok := peerOf(lookupAll(t, tn.Nodes[9], hashKey(host.Public)), host.Public); !ok {
		t.Error("the first server lost its record after the second Listen was refused")
	}

	closeServer(t, first)
	listenOn(t, second, host)
}
