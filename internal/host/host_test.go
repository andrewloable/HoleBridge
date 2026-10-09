package host

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/internal/config"
	"github.com/andrewloable/HoleBridge/internal/host/hosttest"
	"github.com/andrewloable/HoleBridge/internal/keys"
	"github.com/andrewloable/HoleBridge/internal/log"
	"github.com/andrewloable/HoleBridge/internal/mux"
	"github.com/andrewloable/HoleBridge/internal/protocol"
	"github.com/andrewloable/HoleBridge/pears/hyperdht"
	"github.com/andrewloable/HoleBridge/pears/noise"
	"github.com/andrewloable/HoleBridge/pears/protomux"
)

// readWait bounds one wait in these tests: a connect retry, an echo, a log line, a reject.
const readWait = 15 * time.Second

// rig is one test's network and identity. The host runs on the first node of a 10-node testnet on
// 127.0.0.1, and the clients dial from the second. The key and the application key are random per test;
// no test prints them.
type rig struct {
	tn        *hyperdht.Testnet
	key       string
	appKey    [32]byte
	hostPub   [32]byte
	clientKey ed25519.PrivateKey // the derived client key pair
	logs      *syncBuffer
}

// newRig starts a testnet and derives the host and client key pairs from a random key and application key.
func newRig(t *testing.T) *rig {
	t.Helper()
	tn := hyperdht.NewTestnet(t, 10)
	appKey := keys.NewAppKey()
	key := keys.Generate()
	d, err := keys.Derive(key, appKey)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	return &rig{
		tn:        tn,
		key:       key,
		appKey:    appKey,
		hostPub:   [32]byte(d.Host.Public().(ed25519.PublicKey)),
		clientKey: d.Client,
		logs:      &syncBuffer{},
	}
}

// hostConfig writes host.json for the rig's key with the given services and limit overrides, and loads it
// with config.Load. The LAN route is off, so no test binds the LAN ports.
func (r *rig) hostConfig(t *testing.T, services map[string]config.Service, limits map[string]any) *config.Config {
	t.Helper()
	doc := map[string]any{
		"key":      keys.Format(r.key),
		"services": services,
		"lan":      map[string]any{"enabled": false},
	}
	if limits != nil {
		doc["limits"] = limits
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal host.json: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "host.json"), b, 0o600); err != nil {
		t.Fatalf("write host.json: %v", err)
	}
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// startHost runs New for cfg, then Run in the background, and stops Run when the test ends. dial is the
// target dialer, or nil for the default one. Logs go to r.logs at debug level.
func (r *rig) startHost(t *testing.T, cfg *config.Config, dial func(context.Context, string, string) (net.Conn, error)) {
	t.Helper()
	h, err := New(cfg, r.appKey, Options{
		DHT:   r.tn.Nodes[0],
		Dial:  dial,
		Clock: time.Now,
		Log:   log.New(r.logs, slog.LevelDebug),
	})
	failIfStub(t, err)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(readWait):
			t.Errorf("Run did not return within %v of its context ending", readWait)
		}
	})
}

// connect dials the host with clientKey and returns the client once the host's handshake has arrived. The
// host starts announcing as Run starts, so a dial that finds nothing yet is retried for readWait.
func (r *rig) connect(t *testing.T, clientKey ed25519.PrivateKey) *hosttest.Client {
	t.Helper()
	deadline := time.Now().Add(readWait)
	for {
		c, err := hosttest.Connect(r.tn.Nodes[1], r.hostPub, clientKey)
		if err == nil {
			t.Cleanup(func() { c.Close() })
			return c
		}
		failIfStub(t, err)
		if time.Now().After(deadline) {
			t.Fatalf("Connect: %v", err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// rawHandshake dials the host with the rig's client key pair, opens the holebridge channel with a handshake
// of the given version, and returns a channel that is closed when the host closes that channel.
func (r *rig) rawHandshake(t *testing.T, version uint64) <-chan struct{} {
	t.Helper()
	var kp noise.KeyPair
	copy(kp.Public[:], r.clientKey.Public().(ed25519.PublicKey))
	copy(kp.Secret[:], r.clientKey)
	ctx, cancel := context.WithTimeout(context.Background(), readWait)
	defer cancel()
	conn, err := r.tn.Nodes[1].Connect(ctx, r.hostPub, hyperdht.ConnectOptions{KeyPair: &kp})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	closed := make(chan struct{})
	var once sync.Once
	m := protomux.New(conn)
	ch := m.CreateChannel(protomux.ChannelOptions{
		Protocol: "holebridge",
		OnClose:  func(bool) { once.Do(func() { close(closed) }) },
	})
	if ch == nil {
		t.Fatal("CreateChannel returned nil")
	}
	hs := protocol.EncodeHandshake(protocol.Handshake{Version: version, Flags: protocol.FlagResume | protocol.FlagDatagrams})
	if err := ch.Open(hs); err != nil {
		t.Fatalf("Open: %v", err)
	}
	return closed
}

// waitLog fails the test unless the host logs want within readWait.
func (r *rig) waitLog(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(readWait)
	for !bytes.Contains(r.logs.Bytes(), []byte(want)) {
		if time.Now().After(deadline) {
			t.Fatalf("the host did not log %s", want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// syncBuffer is a log sink that the host's goroutines write to while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// Bytes returns a copy of everything written so far.
func (b *syncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

// echoServices is the one service of most tests: "echo", a TCP service whose target echoes.
func echoServices(t *testing.T) map[string]config.Service {
	t.Helper()
	return map[string]config.Service{"echo": {Target: echoTarget(t), Kind: "tcp"}}
}

// echoTarget starts a TCP echo server on 127.0.0.1 and returns its address. It stops when the test ends.
func echoTarget(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				io.Copy(c, c)
				c.Close()
			}()
		}
	}()
	return l.Addr().String()
}

// closedAddr returns a 127.0.0.1 address with nothing listening on it.
func closedAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// roundTrip writes data to st and reads back as many bytes, failing the test if the echo does not arrive
// within readWait. It returns the bytes read.
func roundTrip(t *testing.T, st *mux.Stream, data []byte) []byte {
	t.Helper()
	got := make([]byte, len(data))
	err := bounded(t, "the echo", func() error {
		werr := make(chan error, 1)
		go func() {
			_, err := st.Write(data)
			werr <- err
		}()
		if _, err := io.ReadFull(st, got); err != nil {
			return err
		}
		return <-werr
	})
	if err != nil {
		t.Fatalf("echo: %v", err)
	}
	return got
}

// bounded runs f and fails the test if it does not return within readWait. It returns f's error.
func bounded(t *testing.T, what string, f func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- f() }()
	select {
	case err := <-done:
		return err
	case <-time.After(readWait):
		t.Fatalf("%s did not finish within %v", what, readWait)
		return nil
	}
}

// rejectOf returns the reject that an Open error carries, and fails the test if the error is not a reject.
func rejectOf(t *testing.T, err error) *mux.RejectError {
	t.Helper()
	failIfStub(t, err)
	var re *mux.RejectError
	if !errors.As(err, &re) {
		t.Fatalf("Open error = %v, want a reject", err)
	}
	return re
}

// failIfStub reports a stub's error as "not implemented", so a test that fails only on a stub says so.
func failIfStub(t *testing.T, err error) {
	t.Helper()
	if errors.Is(err, errors.ErrUnsupported) {
		t.Fatal("not implemented")
	}
}

// Case 1: a client with the derived client key pair connects and receives the handshake, which lists the
// configured services with their kinds and port hints, and carries no target address.
func TestHandshakeListsServices(t *testing.T) {
	r := newRig(t)
	r.startHost(t, r.hostConfig(t, map[string]config.Service{
		"web": {Target: "127.0.0.1:8080", Kind: "http"},
		"ssh": {Target: "127.0.0.1:22", Kind: "tcp"},
	}, nil), nil)

	hs := r.connect(t, r.clientKey).Handshake()
	if hs.Version != 1 {
		t.Errorf("version = %d, want 1", hs.Version)
	}
	if hs.Flags&protocol.FlagResume == 0 || hs.Flags&protocol.FlagDatagrams == 0 {
		t.Errorf("flags = %d, want resume and unordered datagrams set", hs.Flags)
	}
	if hs.Flags&protocol.FlagLAN != 0 || hs.LAN != nil {
		t.Error("LAN fields sent with the LAN route off")
	}

	// The port hint is the target's own port, and the kind is the configured one.
	want := map[string]protocol.Service{
		"web": {Name: "web", Kind: protocol.KindHTTP, Port: 8080},
		"ssh": {Name: "ssh", Kind: protocol.KindTCP, Port: 22},
	}
	if len(hs.Services) != len(want) {
		t.Fatalf("services = %d, want %d", len(hs.Services), len(want))
	}
	for _, s := range hs.Services {
		w, ok := want[s.Name]
		if !ok {
			t.Errorf("unexpected service %q", s.Name)
			continue
		}
		if s.Kind != w.Kind || s.Port != w.Port || len(s.Origins) != 0 {
			t.Errorf("service %q: kind %d port %d origins %v, want kind %d port %d and no origins",
				s.Name, s.Kind, s.Port, s.Origins, w.Kind, w.Port)
		}
	}

	// No target address reaches the app: the handshake, re-encoded, names no 127.0.0.1.
	if frame := protocol.EncodeHandshake(hs); bytes.Contains(frame, []byte("127.0.0.1")) {
		t.Error("the handshake carries a target address")
	}
}

// Case 2: the host's firewall admits only the client key pair. A client with another key pair gets no
// handshake and is refused as a peer, while the derived client key still connects.
func TestFirewallRefusesOtherKey(t *testing.T) {
	r := newRig(t)
	r.startHost(t, r.hostConfig(t, echoServices(t), nil), nil)
	r.connect(t, r.clientKey)

	_, other, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	c, err := hosttest.Connect(r.tn.Nodes[1], r.hostPub, other)
	if err == nil {
		c.Close()
		t.Fatal("a client with another key pair connected")
	}
	failIfStub(t, err)
	if !errors.Is(err, hyperdht.ErrPeerConnectionFailed) {
		t.Errorf("Connect error = %v, want the firewall refusal (hyperdht.ErrPeerConnectionFailed)", err)
	}
}

// Case 3: a stream to a TCP service whose target echoes returns 1 MB intact.
func TestOpenEchoesMegabyte(t *testing.T) {
	r := newRig(t)
	r.startHost(t, r.hostConfig(t, echoServices(t), nil), nil)
	c := r.connect(t, r.clientKey)

	st, err := c.Open("echo")
	failIfStub(t, err)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	want := make([]byte, 1<<20)
	if _, err := rand.Read(want); err != nil {
		t.Fatalf("rand: %v", err)
	}
	if got := roundTrip(t, st, want); !bytes.Equal(got, want) {
		t.Fatal("the echo differs from the 1 MB sent")
	}
}

// Case 4: a service the configuration does not name is refused with reject code 1.
func TestUnknownServiceRejectedWithCode1(t *testing.T) {
	r := newRig(t)
	r.startHost(t, r.hostConfig(t, echoServices(t), nil), nil)
	c := r.connect(t, r.clientKey)

	_, err := c.Open("missing")
	if re := rejectOf(t, err); re.Code != 1 {
		t.Errorf("reject code = %d, want 1 (unknown service)", re.Code)
	}
}

// Case 5: a target that refuses the connection rejects the stream with code 3, and the reason names the
// refusal.
func TestRefusedTargetRejectedWithCode3(t *testing.T) {
	r := newRig(t)
	r.startHost(t, r.hostConfig(t, map[string]config.Service{
		"down": {Target: closedAddr(t), Kind: "tcp"},
	}, nil), nil)
	c := r.connect(t, r.clientKey)

	_, err := c.Open("down")
	re := rejectOf(t, err)
	if re.Code != 3 {
		t.Errorf("reject code = %d, want 3 (target refused)", re.Code)
	}
	if !strings.Contains(re.Reason, "refused") {
		t.Errorf("reject reason %q does not name the refused connection", re.Reason)
	}
}

// Case 6: a target whose dial never returns rejects the stream with code 4 once the target connect timeout
// has passed. The test sets that timeout to 300 ms.
func TestSilentTargetRejectedWithCode4(t *testing.T) {
	r := newRig(t)
	cfg := r.hostConfig(t, map[string]config.Service{
		"slow": {Target: "127.0.0.1:9", Kind: "tcp"},
	}, map[string]any{"targetConnectTimeout": "300ms"})
	silent := func(ctx context.Context, network, addr string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	r.startHost(t, cfg, silent)
	c := r.connect(t, r.clientKey)

	start := time.Now()
	_, err := c.Open("slow")
	elapsed := time.Since(start)
	re := rejectOf(t, err)
	if re.Code != 4 {
		t.Errorf("reject code = %d, want 4 (target timed out)", re.Code)
	}
	if elapsed < 250*time.Millisecond {
		t.Errorf("rejected after %v, before the 300 ms timeout", elapsed)
	}
}

// Case 7: 32 sessions per key are admitted; the 33rd session for the same key is refused.
func TestSessionsPerKeyCapped(t *testing.T) {
	r := newRig(t)
	r.startHost(t, r.hostConfig(t, echoServices(t), nil), nil)
	r.connect(t, r.clientKey)
	for i := 2; i <= 32; i++ {
		c, err := hosttest.Connect(r.tn.Nodes[1], r.hostPub, r.clientKey)
		failIfStub(t, err)
		if err != nil {
			t.Fatalf("session %d of 32: Connect: %v", i, err)
		}
		t.Cleanup(func() { c.Close() })
	}

	c, err := hosttest.Connect(r.tn.Nodes[1], r.hostPub, r.clientKey)
	if err == nil {
		c.Close()
		t.Fatal("the 33rd session for the key was admitted, want it refused")
	}
	failIfStub(t, err)
}

// Case 8: a client handshake with version 2 closes the channel, and the host logs HB-VERSION-MISMATCH.
func TestVersionMismatchClosesChannel(t *testing.T) {
	r := newRig(t)
	r.startHost(t, r.hostConfig(t, echoServices(t), nil), nil)
	r.connect(t, r.clientKey)

	closed := r.rawHandshake(t, 2)
	select {
	case <-closed:
	case <-time.After(readWait):
		t.Fatal("the host kept the channel open after a version 2 handshake")
	}
	r.waitLog(t, "HB-VERSION-MISMATCH")
}

// Case 9: the host's log never carries the key, the application key or payload bytes, across a whole session
// that moves 4 KB.
func TestLogsOmitKeyAppKeyAndPayload(t *testing.T) {
	r := newRig(t)
	r.startHost(t, r.hostConfig(t, echoServices(t), nil), nil)
	c := r.connect(t, r.clientKey)

	st, err := c.Open("echo")
	failIfStub(t, err)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	payload := make([]byte, 4096)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand: %v", err)
	}
	if got := roundTrip(t, st, payload); !bytes.Equal(got, payload) {
		t.Fatal("the echo differs from the payload sent")
	}

	logs := r.logs.Bytes()
	secrets := []struct {
		name  string
		value []byte
	}{
		{"key", []byte(r.key)},
		{"key in groups", []byte(keys.Format(r.key))},
		{"application key", r.appKey[:]},
		{"application key in hex", []byte(keys.FormatAppKey(r.appKey))},
		{"payload", payload[:32]},
		{"payload in hex", []byte(hex.EncodeToString(payload[:32]))},
	}
	for _, s := range secrets {
		if bytes.Contains(logs, s.value) {
			t.Errorf("the host log contains the %s", s.name)
		}
	}
}
