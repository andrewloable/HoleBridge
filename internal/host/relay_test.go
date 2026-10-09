package host

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/andrewloable/HoleBridge/internal/errs"
	"github.com/andrewloable/HoleBridge/internal/keys"
	"github.com/andrewloable/HoleBridge/internal/relay"
	"github.com/andrewloable/HoleBridge/internal/testvec"
	"github.com/andrewloable/HoleBridge/pears/dhtrpc"
	"github.com/andrewloable/HoleBridge/pears/hyperdht"
)

// relayVector is one entry of the relayKeys part of spec/vectors/key-derivation.json. Normalized is the relay key
// in hex, so the test never prints it; PublicKeys holds the "server" and "member" public keys in hex.
type relayVector struct {
	AppKey     string            `json:"appKey"`
	Normalized string            `json:"normalized"`
	PublicKeys map[string]string `json:"publicKeys"`
}

// relayVectorsOf loads the relay key vectors. It fails the test when there are none.
func relayVectorsOf(t *testing.T) []relayVector {
	t.Helper()
	var v struct {
		RelayKeys []relayVector `json:"relayKeys"`
	}
	testvec.Load(t, "key-derivation.json", &v)
	if len(v.RelayKeys) == 0 {
		t.Fatal("key-derivation.json has no relayKeys")
	}
	return v.RelayKeys
}

// relayKeyOf returns the normalized relay key of a vector. The key is a secret; callers never print it.
func relayKeyOf(t *testing.T, c relayVector) string {
	t.Helper()
	return string(testvec.Hex(t, c.Normalized))
}

// publicKeyOf returns a public key given in hex in a vector.
func publicKeyOf(t *testing.T, c relayVector, role string) [32]byte {
	t.Helper()
	var pub [32]byte
	copy(pub[:], testvec.Hex(t, c.PublicKeys[role]))
	return pub
}

// appKeyOf returns the application key of a vector.
func appKeyOf(t *testing.T, c relayVector) [32]byte {
	t.Helper()
	k, err := keys.ParseAppKey(c.AppKey)
	if err != nil {
		t.Fatalf("vector application key: %v", err)
	}
	return k
}

// consistentNAT is a fake NAT state: this host's address and port are the same to every peer.
func consistentNAT() dhtrpc.NATInfo {
	return dhtrpc.NATInfo{Host: "192.0.2.20", Port: 27421}
}

// randomizedNAT is a fake NAT state: the host is consistent, but the port is not (dhtrpc.NATInfo.Randomized).
func randomizedNAT() dhtrpc.NATInfo {
	return dhtrpc.NATInfo{Host: "192.0.2.20", Firewalled: true, Randomized: true}
}

// policyOf returns RelayThrough(relayServer, nat). The stub panics with "not implemented", and this helper turns that
// panic into a test failure with the same message, so the failure reads as the stub's, not as a crash.
func policyOf(t *testing.T, relayServer *[32]byte, nat func() dhtrpc.NATInfo) func(bool) *[32]byte {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%v", r)
		}
	}()
	return RelayThrough(relayServer, nat)
}

// Case 1: with a relay key, the DHT's default key pair is the member key pair of that relay key. Its public key is the
// member public key of each relay vector in spec/vectors/key-derivation.json. The DHT keeps the key pair it is given
// (its keyPair field is not exported), so the test checks the key pair DefaultKeyPair returns, which is what the host
// passes to the DHT as DefaultKeyPair.
func TestRelayKeyDefaultKeyPairIsMemberPair(t *testing.T) {
	for i, c := range relayVectorsOf(t) {
		t.Run(fmt.Sprintf("relay key %d", i), func(t *testing.T) {
			kp, err := DefaultKeyPair(relayKeyOf(t, c), appKeyOf(t, c))
			failIfStub(t, err)
			if err != nil {
				t.Fatalf("DefaultKeyPair: %v", err)
			}
			if kp == nil {
				t.Fatal("DefaultKeyPair returned no key pair for a relay key")
			}
			if want := publicKeyOf(t, c, "member"); !bytes.Equal(kp.Public[:], want[:]) {
				t.Fatal("the default key pair is not the member key pair of the relay key: public key differs from the vector")
			}
		})
	}
}

// Case 2: RelayThrough(true) returns the relay server public key. RelayThrough(false) returns nil while the host's NAT
// is consistent, and the relay server public key while it is randomized. The policy reads the NAT at each call.
func TestRelayThroughOfferedWhenForcedOrRandomized(t *testing.T) {
	c := relayVectorsOf(t)[0]
	server := publicKeyOf(t, c, "server")
	cases := []struct {
		name    string
		nat     dhtrpc.NATInfo
		force   bool
		offered bool
	}{
		{"forced, consistent NAT", consistentNAT(), true, true},
		{"not forced, consistent NAT", consistentNAT(), false, false},
		{"not forced, randomized NAT", randomizedNAT(), false, true},
		{"forced, randomized NAT", randomizedNAT(), true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nat := tc.nat
			policy := policyOf(t, &server, func() dhtrpc.NATInfo { return nat })
			got := policy(tc.force)
			if !tc.offered {
				if got != nil {
					t.Fatal("the policy offered a relay it should not: want nil")
				}
				return
			}
			if got == nil || *got != server {
				t.Fatal("the policy did not return the relay server public key")
			}
		})
	}

	t.Run("reads the NAT at each call", func(t *testing.T) {
		nat := consistentNAT()
		policy := policyOf(t, &server, func() dhtrpc.NATInfo { return nat })
		if got := policy(false); got != nil {
			t.Fatal("the policy offered a relay on a consistent NAT: want nil")
		}
		nat = randomizedNAT()
		if got := policy(false); got == nil || *got != server {
			t.Fatal("the policy did not offer the relay after the NAT became randomized")
		}
	})
}

// Case 3: without a relay key, the DHT keeps its random default key pair (DefaultKeyPair returns no key pair and no
// error), and the server has no relayThrough policy, even on a randomized NAT.
func TestNoRelayKeyLeavesDefaultKeyPairAndRelayThroughUnset(t *testing.T) {
	appKey := keys.NewAppKey()
	kp, err := DefaultKeyPair("", appKey)
	failIfStub(t, err)
	if err != nil {
		t.Fatalf("DefaultKeyPair without a relay key: %v", err)
	}
	if kp != nil {
		t.Fatal("without a relay key the default key pair must stay as it is: DefaultKeyPair returned a key pair")
	}
	if policy := policyOf(t, nil, consistentNAT); policy != nil {
		t.Fatal("without a relay key RelayThrough must be nil on a consistent NAT")
	}
	if policy := policyOf(t, nil, randomizedNAT); policy != nil {
		t.Fatal("without a relay key RelayThrough must be nil on a randomized NAT")
	}
}

// An invalid relay key is rejected by both derivations with HB-KEY-INVALID, the same as keys.DeriveRelay rejects it.
func TestInvalidRelayKeyIsRejected(t *testing.T) {
	appKey := keys.NewAppKey()
	kp, err := DefaultKeyPair("NOPE", appKey)
	failIfStub(t, err)
	checkInvalidKey(t, "DefaultKeyPair", kp != nil, err)

	pub, err := RelayServerPublicKey("NOPE", appKey)
	failIfStub(t, err)
	checkInvalidKey(t, "RelayServerPublicKey", pub != [32]byte{}, err)
}

// checkInvalidKey fails the test unless err is an HB-KEY-INVALID error and no key came back with it.
func checkInvalidKey(t *testing.T, name string, gotKey bool, err error) {
	t.Helper()
	var e *errs.Error
	if !errors.As(err, &e) || e.Code != "HB-KEY-INVALID" {
		t.Fatalf("%s: want an HB-KEY-INVALID error for an invalid relay key, got %v", name, err)
	}
	if gotKey {
		t.Fatalf("%s: returned a key together with the error", name)
	}
}

// Case 4: on a Go testnet with a holebridge relay under the relay key, a hosttest client with the same relay key
// connects through the relay. Both DHT nodes are forced off the direct path (hyperdht.ForceRelayForTest), which stands
// in for two randomized NATs: the bytes can only cross the relay. The host and the app use the member key pair as their
// DHT default key pair, the one the relay admits.
func TestRelayedClientWithSameRelayKeyConnects(t *testing.T) {
	r := newRig(t)
	relayKey := keys.Generate()

	member, err := DefaultKeyPair(relayKey, r.appKey)
	failIfStub(t, err)
	if err != nil {
		t.Fatalf("DefaultKeyPair: %v", err)
	}
	if member == nil {
		t.Fatal("DefaultKeyPair returned no key pair for a relay key")
	}
	relayPub, err := RelayServerPublicKey(relayKey, r.appKey)
	failIfStub(t, err)
	if err != nil {
		t.Fatalf("RelayServerPublicKey: %v", err)
	}

	relayDHT := newTestDHT(t, r.tn, nil)
	relayCtx, cancelRelay := context.WithCancel(context.Background())
	t.Cleanup(cancelRelay)
	if _, err := relay.Run(relayCtx, relayKey, r.appKey, relayDHT, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("relay.Run: %v", err)
	}
	hostDHT := newTestDHT(t, r.tn, member)
	appDHT := newTestDHT(t, r.tn, member)
	hyperdht.ForceRelayForTest(t, hostDHT)
	hyperdht.ForceRelayForTest(t, appDHT)

	cfg := r.hostConfig(t, echoServices(t), nil)
	cfg.Relay = relayKey
	h := r.newWireHost(t, cfg, func(o *Options) { o.DHT = hostDHT })
	r.runWire(t, h)
	c := r.dialThroughRelay(t, appDHT, relayPub)

	st, err := c.Open("echo")
	failIfStub(t, err)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	want := make([]byte, 1000)
	if _, err := rand.Read(want); err != nil {
		t.Fatalf("rand: %v", err)
	}
	if got := roundTrip(t, st, want); !bytes.Equal(got, want) {
		t.Fatal("the echo differs from the bytes sent")
	}
	awaitSession(t, h, "a relayed session with one stream", func(s Session) bool {
		return s.Route == "relay" && s.Streams == 1
	})
}
