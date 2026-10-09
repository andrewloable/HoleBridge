package hyperdht

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/pears/dhtrpc"
	"github.com/andrewloable/HoleBridge/pears/noise"
)

// testKeyPair returns the key pair of the Ed25519 seed made of b repeated 32 times, in the layout of
// noise.KeyPair.
func testKeyPair(b byte) noise.KeyPair {
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{b}, 32))
	var kp noise.KeyPair
	copy(kp.Secret[:], priv)
	copy(kp.Public[:], priv[32:])
	return kp
}

// testTarget returns the 32-byte target of topic, its SHA-256. No test key hashes to it, so an announce
// for it is a topic record, not a record of the key's own address.
func testTarget(topic string) [32]byte {
	return sha256.Sum256([]byte(topic))
}

// testRelays returns two relay addresses from the RFC 5737 documentation ranges.
func testRelays() []PeerAddr {
	return []PeerAddr{
		{Host: netip.MustParseAddr("203.0.113.7"), Port: 49737},
		{Host: netip.MustParseAddr("198.51.100.9"), Port: 49738},
	}
}

// startTestnet starts a testnet of size nodes. The stub returns nil, which fails the test.
func startTestnet(t *testing.T, size int) *Testnet {
	t.Helper()
	tn := NewTestnet(t, size)
	if tn == nil {
		t.Fatal("NewTestnet: not implemented")
	}
	return tn
}

// nodeAddr returns the UDP address that d listens on.
func nodeAddr(t *testing.T, d *DHT) *net.UDPAddr {
	t.Helper()
	addr, err := d.addr()
	must(t, err)
	return addr
}

// announceFrom has d announce kp for target, with relays, and fails the test on error.
func announceFrom(t *testing.T, d *DHT, target [32]byte, kp noise.KeyPair, relays []PeerAddr) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := d.Announce(ctx, target, kp, relays); err != nil {
		failIfStub(t, err)
		t.Fatalf("Announce: %v", err)
	}
}

// unannounceFrom has d unannounce kp for target and fails the test on error.
func unannounceFrom(t *testing.T, d *DHT, target [32]byte, kp noise.KeyPair) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := d.Unannounce(ctx, target, kp); err != nil {
		failIfStub(t, err)
		t.Fatalf("Unannounce: %v", err)
	}
}

// startLookup calls d.Lookup. The stub panics, so the panic becomes a test failure here, which lets the
// other tests run.
func startLookup(t *testing.T, d *DHT, ctx context.Context, target [32]byte) <-chan LookupResult {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Lookup: %v", r)
		}
	}()
	return d.Lookup(ctx, target)
}

// lookupAll runs a lookup of target on d and collects the answers until the channel closes. The walk
// must finish within 30 seconds.
func lookupAll(t *testing.T, d *DHT, target [32]byte) []LookupResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	results := startLookup(t, d, ctx, target)
	var got []LookupResult
	for {
		select {
		case r, ok := <-results:
			if !ok {
				return got
			}
			got = append(got, r)
		case <-ctx.Done():
			t.Fatal("Lookup did not finish within 30 s")
		}
	}
}

// peerOf returns the record of the key pk among the lookup answers, and whether one was found.
func peerOf(results []LookupResult, pk [32]byte) (Peer, bool) {
	for _, r := range results {
		for _, p := range r.Peers {
			if bytes.Equal(p.PublicKey, pk[:]) {
				return p, true
			}
		}
	}
	return Peer{}, false
}

// wrongSignature returns a valid Ed25519 signature by kp over other bytes. It does not verify for an
// announce of kp.
func wrongSignature(kp noise.KeyPair) []byte {
	return ed25519.Sign(ed25519.PrivateKey(kp.Secret[:]), []byte("not the announce"))
}

// newClient returns a dhtrpc IO on a loopback UDP socket, which the test closes when it ends. It answers
// no request.
func newClient(t *testing.T) *dhtrpc.IO {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	must(t, err)
	client := dhtrpc.NewIO(conn, func(*dhtrpc.Request, *net.UDPAddr) *dhtrpc.Response { return nil })
	t.Cleanup(func() { client.Close() })
	return client
}

// sendBadAnnounce sends every node of tn an ANNOUNCE for kp, with the signature wrongSignature gives.
// Each node gets the token it gave in a LOOKUP for target first, as an announcer does; a node that does
// not answer the LOOKUP gets nothing. The sends run at once, since a node that rejects an ANNOUNCE says
// nothing. It returns how many nodes gave a token, and the indexes of the nodes that acknowledged the
// ANNOUNCE with no error code.
func sendBadAnnounce(t *testing.T, tn *Testnet, target [32]byte, kp noise.KeyPair, relays []PeerAddr) (int, []int) {
	t.Helper()
	client := newClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	value, err := EncodeAnnounce(Announce{
		Peer:      &Peer{PublicKey: kp.Public[:], RelayAddresses: relays},
		Signature: wrongSignature(kp),
	})
	must(t, err)
	tokened := make([]bool, len(tn.Nodes))
	acked := make([]bool, len(tn.Nodes))
	var wg sync.WaitGroup
	for i, n := range tn.Nodes {
		addr := nodeAddr(t, n)
		wg.Add(1)
		go func() {
			defer wg.Done()
			lk, err := client.Request(ctx, addr, dhtrpc.Request{Command: cmdLookup, Target: target[:]})
			if err != nil {
				return
			}
			tokened[i] = true
			resp, err := client.Request(ctx, addr, dhtrpc.Request{
				Command: cmdAnnounce,
				Target:  target[:],
				Token:   lk.Token,
				Value:   value,
			})
			acked[i] = err == nil && resp.Error == 0
		}()
	}
	wg.Wait()
	var sent int
	var acks []int
	for i := range tn.Nodes {
		if tokened[i] {
			sent++
		}
		if acked[i] {
			acks = append(acks, i)
		}
	}
	return sent, acks
}

// Test case 1: on a 10-node testnet, the record that node 0 announces is found by a Lookup from node 9,
// with the relay addresses it was announced with.
func TestAnnounceFoundByLookup(t *testing.T) {
	tn := startTestnet(t, 10)
	kp := testKeyPair(1)
	target := testTarget("holebridge announce test")
	relays := testRelays()
	announceFrom(t, tn.Nodes[0], target, kp, relays)
	peer, ok := peerOf(lookupAll(t, tn.Nodes[9], target), kp.Public)
	if !ok {
		t.Fatal("Lookup from node 9 found no record for the announced key")
	}
	if !slices.Equal(peer.RelayAddresses, relays) {
		t.Errorf("relay addresses = %v, want %v", peer.RelayAddresses, relays)
	}
}

// Test case 2: Unannounce removes the record. The Lookup before it finds the record, so the test can tell
// that the record was gone afterwards; the Lookup after it does not return it.
func TestUnannounceRemovesRecord(t *testing.T) {
	tn := startTestnet(t, 10)
	kp := testKeyPair(1)
	target := testTarget("holebridge announce test")
	announceFrom(t, tn.Nodes[0], target, kp, testRelays())
	if _, ok := peerOf(lookupAll(t, tn.Nodes[9], target), kp.Public); !ok {
		t.Fatal("before Unannounce: Lookup found no record, so removal cannot be tested")
	}
	unannounceFrom(t, tn.Nodes[0], target, kp)
	if p, ok := peerOf(lookupAll(t, tn.Nodes[9], target), kp.Public); ok {
		t.Errorf("after Unannounce: Lookup still returns the record %v", p)
	}
}

// Test case 3: a record whose signature does not verify is rejected by the storing nodes. The test sends
// an ANNOUNCE for a key, signed by that key over other bytes, to every node of the testnet, each with the
// token it gave. No node may acknowledge it, and a Lookup must not return it. A record announced with a
// good signature on the same target is found first, so the Lookup itself is shown to work.
func TestBadSignatureRejected(t *testing.T) {
	tn := startTestnet(t, 10)
	target := testTarget("holebridge announce test")
	good := testKeyPair(2)
	announceFrom(t, tn.Nodes[0], target, good, testRelays())
	if _, ok := peerOf(lookupAll(t, tn.Nodes[9], target), good.Public); !ok {
		t.Fatal("control: Lookup found no record for the announce with a good signature")
	}

	bad := testKeyPair(1)
	sent, acks := sendBadAnnounce(t, tn, target, bad, testRelays())
	if sent == 0 {
		t.Fatal("no node gave a LOOKUP token, so no ANNOUNCE was sent")
	}
	for _, i := range acks {
		t.Errorf("node %d acknowledged an ANNOUNCE with a bad signature", i)
	}
	if p, ok := peerOf(lookupAll(t, tn.Nodes[9], target), bad.Public); ok {
		t.Errorf("Lookup returns the record with a bad signature: %v", p)
	}
}

// Test case 4: a Lookup on a target that nobody announced finishes with no results.
func TestLookupOfUnannouncedTargetIsEmpty(t *testing.T) {
	tn := startTestnet(t, 10)
	target := testTarget("holebridge nobody announced")
	if got := lookupAll(t, tn.Nodes[9], target); len(got) != 0 {
		t.Errorf("Lookup returned %d results for a target nobody announced, want none", len(got))
	}
}

// Upstream vectors. hyperdht 6.34.1 signs with Persistent.signAnnounce and Persistent.signUnannounce, and
// encodes the message with c.encode(m.announce). The inputs are the test key pair (seed 0x01 repeated), the
// target of the topic "holebridge announce test", a token of 0x22 bytes and a node id of 0x33 bytes. The
// announce carries testRelays, and the unannounce carries no relay address.
const (
	upstreamAnnounceSig     = "b25dadd5fb28537bb57b00f7d327a9cc8253c31f460aecd08b3375f3e3920625c6f4c602795a892935fd4457e48816ef8bf50c968baee2a122cae5be7f95b305"
	upstreamAnnounceValue   = "058a88e3dd7409f195fd52db2d3cba5d72ca6709bf1d94121bf3748801b40f6f5c02cb00710749c2c63364094ac2b25dadd5fb28537bb57b00f7d327a9cc8253c31f460aecd08b3375f3e3920625c6f4c602795a892935fd4457e48816ef8bf50c968baee2a122cae5be7f95b305"
	upstreamUnannounceSig   = "272d95839a26783e73cec737f35a3b446ad119a5a0b9fbbb3e7334d15822352f645183c7c67fd437b1af6db5a87a793e0e2a717ff6aa49bf25ef1ce7a6c3f405"
	upstreamUnannounceValue = "058a88e3dd7409f195fd52db2d3cba5d72ca6709bf1d94121bf3748801b40f6f5c00272d95839a26783e73cec737f35a3b446ad119a5a0b9fbbb3e7334d15822352f645183c7c67fd437b1af6db5a87a793e0e2a717ff6aa49bf25ef1ce7a6c3f405"
)

// upstreamBytes decodes a hex vector from the constants above.
func upstreamBytes(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	must(t, err)
	return b
}

// TestAnnounceMatchesUpstream checks the signed bytes and the encoding of ANNOUNCE and UNANNOUNCE against
// hyperdht 6.34.1. An upstream signature verifies over the signable bytes this code builds, and this code
// signs the same bytes to the same signature (Ed25519 is deterministic) and encodes the same message.
func TestAnnounceMatchesUpstream(t *testing.T) {
	kp := testKeyPair(1)
	target := testTarget("holebridge announce test")
	token := bytes.Repeat([]byte{0x22}, 32)
	id := bytes.Repeat([]byte{0x33}, 32)
	secret := ed25519.PrivateKey(kp.Secret[:])

	cases := []struct {
		name      string
		ns        [32]byte
		peer      Peer
		signature string
		value     string
	}{
		{"announce", nsAnnounce, Peer{PublicKey: kp.Public[:], RelayAddresses: testRelays()}, upstreamAnnounceSig, upstreamAnnounceValue},
		{"unannounce", nsUnannounce, Peer{PublicKey: kp.Public[:], RelayAddresses: []Address{}}, upstreamUnannounceSig, upstreamUnannounceValue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := upstreamBytes(t, tc.signature)
			signable, err := annSignable(target[:], token, id, tc.peer, nil, tc.ns)
			must(t, err)
			if !ed25519.Verify(kp.Public[:], signable, want) {
				t.Fatal("the upstream signature does not verify over the signable bytes of this code")
			}
			if got := ed25519.Sign(secret, signable); !bytes.Equal(got, want) {
				t.Errorf("signature = %x, want %x", got, want)
			}
			peer := tc.peer
			value, err := EncodeAnnounce(Announce{Peer: &peer, Signature: want})
			must(t, err)
			if got := hex.EncodeToString(value); got != tc.value {
				t.Errorf("encoding = %s, want %s", got, tc.value)
			}
		})
	}
}
