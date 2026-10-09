package relay

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/internal/keys"
	"github.com/andrewloable/HoleBridge/internal/log"
	"github.com/andrewloable/HoleBridge/internal/testvec"
	"github.com/andrewloable/HoleBridge/pears/hyperdht"
	"github.com/andrewloable/HoleBridge/pears/noise"
)

const (
	testnetSize = 10
	connectWait = 30 * time.Second // one Connect, Listen or Run on the testnet
	readWait    = 15 * time.Second // one datagram-sized exchange on the connection
	pairWait    = 20 * time.Second // the relay's pairing count reaching the expected value
)

// relayVectors is the part of spec/vectors/key-derivation.json these tests read. Every byte value is lowercase
// hex; normalized is the hex of the ASCII relay key. The vector values are test values only.
type relayVectors struct {
	RelayKeys []relayVector `json:"relayKeys"`
}

type relayVector struct {
	AppKey     string            `json:"appKey"`
	Normalized string            `json:"normalized"`
	PublicKeys map[string]string `json:"publicKeys"`
}

func loadRelayVectors(t *testing.T) relayVectors {
	t.Helper()
	var v relayVectors
	testvec.Load(t, "key-derivation.json", &v)
	if len(v.RelayKeys) < 2 {
		t.Fatal("key-derivation.json needs two relayKeys entries")
	}
	return v
}

// vectorAppKey decodes a vector application key, which must be 32 bytes.
func vectorAppKey(t *testing.T, s string) [32]byte {
	t.Helper()
	b := testvec.Hex(t, s)
	if len(b) != 32 {
		t.Fatalf("vector application key is %d bytes, want 32", len(b))
	}
	var k [32]byte
	copy(k[:], b)
	return k
}

// relayFixture returns the first relay vector: its normalized relay key, its application key, and the member
// key pair DeriveRelay gives them. Hosts and apps set the member key pair as their DHT default key pair.
func relayFixture(t *testing.T) (string, [32]byte, noise.KeyPair) {
	t.Helper()
	v := loadRelayVectors(t)
	relayKey := string(testvec.Hex(t, v.RelayKeys[0].Normalized))
	appKey := vectorAppKey(t, v.RelayKeys[0].AppKey)
	d, err := keys.DeriveRelay(relayKey, appKey)
	if err != nil {
		t.Fatalf("DeriveRelay: %v", err)
	}
	return relayKey, appKey, toKeyPair(d.Member)
}

// toKeyPair returns the noise key pair of an ed25519 private key.
func toKeyPair(priv ed25519.PrivateKey) noise.KeyPair {
	var kp noise.KeyPair
	copy(kp.Public[:], priv.Public().(ed25519.PublicKey))
	copy(kp.Secret[:], priv)
	return kp
}

// randomKeyPair returns a new random key pair, for a host or a peer that is not a member.
func randomKeyPair(t *testing.T) noise.KeyPair {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	var kp noise.KeyPair
	copy(kp.Public[:], pub)
	copy(kp.Secret[:], priv)
	return kp
}

// newDHT starts a DHT node on tn whose default key pair is kp, or a random one when kp is nil. The node is
// closed when the test ends. A node made before the relay is closed after it, so the relay unannounces first.
func newDHT(t *testing.T, tn *hyperdht.Testnet, kp *noise.KeyPair) *hyperdht.DHT {
	t.Helper()
	d, err := hyperdht.New(hyperdht.Config{Bootstrap: tn.Bootstrap, DefaultKeyPair: kp})
	if err != nil {
		t.Fatalf("hyperdht.New: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// always returns a relay policy that offers the relay with public key pk on every call.
func always(pk [32]byte) func(bool) *[32]byte {
	return func(bool) *[32]byte { return &pk }
}

// syncBuf is the relay's log sink. The relay writes from its own goroutines, so reads and writes are guarded.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fakeClock replaces the relay's report ticker. The test fires a tick by sending on ticks, and period is the
// duration the relay asked for.
type fakeClock struct {
	ticks  chan time.Time
	mu     sync.Mutex
	period time.Duration
}

// installFakeClock makes every relay started after it use a fake clock. The real ticker is restored when the
// test ends. Tests that install a fake clock do not run in parallel.
func installFakeClock(t *testing.T) *fakeClock {
	t.Helper()
	fc := &fakeClock{ticks: make(chan time.Time)}
	prev := newTicker
	newTicker = func(d time.Duration) (<-chan time.Time, func()) {
		fc.mu.Lock()
		fc.period = d
		fc.mu.Unlock()
		return fc.ticks, func() {}
	}
	t.Cleanup(func() { newTicker = prev })
	return fc
}

// tick fires one report tick, which stands for 10 minutes of the fake clock.
func (fc *fakeClock) tick(t *testing.T) {
	t.Helper()
	select {
	case fc.ticks <- time.Now():
	case <-time.After(readWait):
		t.Fatal("the relay did not read its report ticker")
	}
}

func (fc *fakeClock) periodOf() time.Duration {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.period
}

// startRelay runs a relay on a new DHT node of tn with relayKey and appKey, logging to logs. The relay stops when
// the test ends. The DHT is created first, so it is closed after the relay has stopped.
func startRelay(t *testing.T, tn *hyperdht.Testnet, relayKey string, appKey [32]byte, logs *syncBuf) *Relay {
	t.Helper()
	d := newDHT(t, tn, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r, err := Run(ctx, relayKey, appKey, d, log.New(logs, slog.LevelInfo))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return r
}

// acceptAsync accepts one connection on srv in the background. The result arrives on the channel.
func acceptAsync(srv *hyperdht.Server) <-chan *hyperdht.Conn {
	ch := make(chan *hyperdht.Conn, 1)
	go func() {
		c, err := srv.Accept()
		if err == nil {
			ch <- c
		}
	}()
	return ch
}

// awaitAccept waits for the connection that acceptAsync accepts and fails the test when none arrives.
func awaitAccept(t *testing.T, ch <-chan *hyperdht.Conn) *hyperdht.Conn {
	t.Helper()
	select {
	case c := <-ch:
		return c
	case <-time.After(connectWait):
		t.Fatal("the server accepted no connection")
		return nil
	}
}

// send writes msg on from and checks that the same bytes arrive on to.
func send(t *testing.T, from io.Writer, to io.Reader, msg string) {
	t.Helper()
	if _, err := from.Write([]byte(msg)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := make([]byte, len(msg))
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(to, got)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
	case <-time.After(readWait):
		t.Fatal("the data did not arrive within 15 s")
	}
	if string(got) != msg {
		t.Fatalf("the data that arrived does not match what was sent")
	}
}

// exchange sends data both ways over a connection that the two peers hold.
func exchange(t *testing.T, client, server *hyperdht.Conn) {
	t.Helper()
	send(t, client, server, "ping")
	send(t, server, client, "pong")
}

// pairThroughRelay makes a host and a client, both with member as their DHT default key pair. Both offer the
// relay with relayPub, the client dials the host, and data goes both ways. The relay pairs the two sides on its
// blind relay, so its pairing count goes up.
func pairThroughRelay(t *testing.T, tn *hyperdht.Testnet, relayPub [32]byte, member noise.KeyPair) {
	t.Helper()
	hostDHT := newDHT(t, tn, &member)
	clientDHT := newDHT(t, tn, &member)
	hostKP := randomKeyPair(t)
	srv := hostDHT.CreateServer(hyperdht.ServerOptions{RelayThrough: always(relayPub)})
	t.Cleanup(func() { srv.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), connectWait)
	defer cancel()
	if err := srv.Listen(ctx, hostKP); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	accepted := acceptAsync(srv)
	c, err := clientDHT.Connect(ctx, hostKP.Public, hyperdht.ConnectOptions{RelayThrough: always(relayPub)})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	exchange(t, c, awaitAccept(t, accepted))
}

// pairingLine finds the pairing count in the log: the N of the last "pairings matched: N" line, and whether
// there is one.
var pairingLine = regexp.MustCompile(`pairings matched: (\d+)`)

func lastPairings(logs *syncBuf) (int, bool) {
	all := pairingLine.FindAllStringSubmatch(logs.String(), -1)
	if len(all) == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(all[len(all)-1][1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// awaitPairings fires report ticks until the last pairing line counts at least min pairings. It fails the test
// when that does not happen within pairWait.
func awaitPairings(t *testing.T, fc *fakeClock, logs *syncBuf, min int) {
	t.Helper()
	deadline := time.Now().Add(pairWait)
	for {
		fc.tick(t)
		time.Sleep(200 * time.Millisecond)
		if n, ok := lastPairings(logs); ok && n >= min {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the relay never logged a pairing count of %d or more", min)
		}
	}
}

// Test case 1: two peers whose DHT default key pair is the member key pair pair through the relay and exchange
// data. The relay's pairing count, logged on a report tick, shows that the pair went through the relay.
func TestPeersPairThroughRelayAndExchangeData(t *testing.T) {
	tn := hyperdht.NewTestnet(t, testnetSize)
	relayKey, appKey, member := relayFixture(t)
	fc := installFakeClock(t)
	logs := &syncBuf{}
	r := startRelay(t, tn, relayKey, appKey, logs)

	pairThroughRelay(t, tn, r.PublicKey(), member)
	awaitPairings(t, fc, logs, 1)
}

// Test case 2: a peer with another DefaultKeyPair is refused. The relay admits only the member public key, so a
// dial to the relay from the other key pair gets no answer and Connect fails with the refusal, while the member's
// dial to the same relay succeeds. The control is what shows the failure comes from the firewall.
func TestPeerWithOtherDefaultKeyPairIsRefused(t *testing.T) {
	tn := hyperdht.NewTestnet(t, testnetSize)
	relayKey, appKey, member := relayFixture(t)
	logs := &syncBuf{}
	r := startRelay(t, tn, relayKey, appKey, logs)
	relayPub := r.PublicKey()

	memberDHT := newDHT(t, tn, &member)
	other := randomKeyPair(t)
	otherDHT := newDHT(t, tn, &other)

	ctx, cancel := context.WithTimeout(context.Background(), connectWait)
	defer cancel()
	mc, err := memberDHT.Connect(ctx, relayPub, hyperdht.ConnectOptions{})
	if err != nil {
		t.Fatalf("the member's dial to the relay failed: %v", err)
	}
	t.Cleanup(func() { mc.Close() })

	_, err = otherDHT.Connect(ctx, relayPub, hyperdht.ConnectOptions{})
	if !errors.Is(err, hyperdht.ErrPeerConnectionFailed) {
		t.Fatalf("the other key pair's dial error = %v, want the refusal ErrPeerConnectionFailed", err)
	}
}

// Test case 3: the pairing log line appears only after 10 minutes of the fake clock, with the count, and the log
// holds no key: not the relay key, not the application key, not a derived secret.
func TestPairingLogLineAfterTenMinutesHasCountAndNoKey(t *testing.T) {
	tn := hyperdht.NewTestnet(t, testnetSize)
	relayKey, appKey, member := relayFixture(t)
	fc := installFakeClock(t)
	logs := &syncBuf{}
	r := startRelay(t, tn, relayKey, appKey, logs)

	if got := fc.periodOf(); got != 10*time.Minute {
		t.Fatalf("the relay's report period = %v, want 10m0s", got)
	}
	pairThroughRelay(t, tn, r.PublicKey(), member)
	if _, ok := lastPairings(logs); ok {
		t.Fatal("a pairing line was logged before the first 10 minutes")
	}

	awaitPairings(t, fc, logs, 1)

	// The derived secrets are the seeds of the server and member key pairs; the seed is a prefix of the
	// private key, so a leaked private key contains its seed too. The failure messages print no value.
	d, err := keys.DeriveRelay(relayKey, appKey)
	if err != nil {
		t.Fatalf("DeriveRelay: %v", err)
	}
	dash := relayKey[0:3] + "-" + relayKey[3:6] + "-" + relayKey[6:9]
	secrets := map[string]string{
		"relay key":             relayKey,
		"relay key with dashes": dash,
		"application key":       hex.EncodeToString(appKey[:]),
		"server seed":           hex.EncodeToString(d.Server.Seed()),
		"member seed":           hex.EncodeToString(d.Member.Seed()),
	}
	out := logs.String()
	for name, s := range secrets {
		if strings.Contains(out, s) {
			t.Errorf("the relay log contains the %s", name)
		}
	}
}

// Test case 4: relays from two different application keys with the same relay key have different public keys.
// The vectors give both public keys, so the test also checks the server key pair against them.
func TestAppKeysGiveDifferentRelayPublicKeys(t *testing.T) {
	tn := hyperdht.NewTestnet(t, testnetSize)
	v := loadRelayVectors(t)
	relayKey := string(testvec.Hex(t, v.RelayKeys[0].Normalized))
	appA := vectorAppKey(t, v.RelayKeys[0].AppKey)
	appB := vectorAppKey(t, v.RelayKeys[1].AppKey)

	ra := startRelay(t, tn, relayKey, appA, &syncBuf{})
	rb := startRelay(t, tn, relayKey, appB, &syncBuf{})
	pa, pb := ra.PublicKey(), rb.PublicKey()

	if pa == pb {
		t.Fatal("two relays with different application keys have the same public key")
	}
	if got, want := hex.EncodeToString(pa[:]), v.RelayKeys[0].PublicKeys["server"]; got != want {
		t.Errorf("the first relay's public key does not match the vector")
	}
	if got, want := hex.EncodeToString(pb[:]), v.RelayKeys[1].PublicKeys["server"]; got != want {
		t.Errorf("the second relay's public key does not match the vector")
	}
}
