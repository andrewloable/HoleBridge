package lan

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/pears/noise"
	"github.com/andrewloable/HoleBridge/pears/secretstream"
)

const (
	// handshakeWait is how long a test's client waits for its handshake. It is longer than the
	// listener's deadline in these tests, so a handshake the listener holds open fails by time-out.
	handshakeWait = 3 * time.Second
	// acceptWait is how long a test waits for Accept to return a stream, or for a refusal to count.
	acceptWait = 5 * time.Second
	// atOnce is how soon the listener must close a connection it refuses at accept.
	atOnce = time.Second
	// shortDeadline is the handshake deadline of the silent-client test.
	shortDeadline = 300 * time.Millisecond
)

// newKeyPair returns a random noise key pair. Its secret is the seed followed by the public key.
func newKeyPair(t *testing.T) noise.KeyPair {
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

// listen opens a TCP listener on loopback, closed when the test ends.
func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

// dial connects to ln, closed when the test ends. Every test connection comes from 127.0.0.1.
func dial(t *testing.T, ln net.Listener) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", ln.Addr().String(), handshakeWait)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// idle reports whether c stays open and silent for wait. A connection the listener has closed
// returns at once with an error other than a time-out.
func idle(t *testing.T, c net.Conn, wait time.Duration) bool {
	t.Helper()
	if err := c.SetReadDeadline(time.Now().Add(wait)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	_, err := c.Read(make([]byte, 1))
	return errors.Is(err, os.ErrDeadlineExceeded)
}

// expectOpen fails the test unless every connection in conns is still open and silent. It waits
// silenceWait first, so the listener has time to act on the connections made so far.
func expectOpen(t *testing.T, conns []net.Conn) {
	t.Helper()
	time.Sleep(silenceWait)
	for i, c := range conns {
		if !idle(t, c, 10*time.Millisecond) {
			t.Fatalf("connection %d of %d was closed or sent data, want it held open and silent", i+1, len(conns))
		}
	}
}

// expectClosed fails the test unless the listener closes c within wait.
func expectClosed(t *testing.T, c net.Conn, wait time.Duration, what string) {
	t.Helper()
	if idle(t, c, wait) {
		t.Fatalf("%s: still open after %v, want it closed", what, wait)
	}
}

// clientHandshake runs the initiator side of the handshake over c with kp, expecting hostPub.
func clientHandshake(t *testing.T, c net.Conn, kp noise.KeyPair, hostPub [32]byte) (*secretstream.Stream, error) {
	t.Helper()
	st := secretstream.New(c, true, secretstream.Options{KeyPair: kp, RemotePublicKey: &hostPub})
	ctx, cancel := context.WithTimeout(context.Background(), handshakeWait)
	defer cancel()
	return st, st.Handshake(ctx)
}

// acceptResult is one call's outcome from Accept.
type acceptResult struct {
	s   *secretstream.Stream
	err error
}

// startAccepting calls l.Accept in a loop on its own goroutine, so the listener's accept work runs
// whether or not a test waits for a stream. Each outcome arrives on the returned channel; the loop
// ends at the first error.
func startAccepting(l *Listener) <-chan acceptResult {
	out := make(chan acceptResult, 16)
	go func() {
		for {
			s, err := l.Accept()
			out <- acceptResult{s, err}
			if err != nil {
				return
			}
		}
	}()
	return out
}

// nextStream waits for the next stream from Accept and destroys it when the test ends.
func nextStream(t *testing.T, results <-chan acceptResult) *secretstream.Stream {
	t.Helper()
	select {
	case r := <-results:
		failIfStub(t, r.err)
		if r.err != nil {
			t.Fatalf("Accept: %v", r.err)
		}
		s := r.s
		t.Cleanup(func() { s.Destroy() })
		return s
	case <-time.After(acceptWait):
		t.Fatal("Accept returned no stream for an admitted client")
	}
	return nil
}

// expectEcho checks that the accepted stream reads what the client writes, which shows the stream
// is a working secret stream and not only a handshaken one.
func expectEcho(t *testing.T, client, server *secretstream.Stream, msg string) {
	t.Helper()
	if _, err := client.Write([]byte(msg)); err != nil {
		t.Fatalf("client Write: %v", err)
	}
	got := make([]byte, len(msg))
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(server, got)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("read on the accepted stream: %v", err)
		}
	case <-time.After(replyWait):
		t.Fatal("the accepted stream read nothing from the client")
	}
	if string(got) != msg {
		t.Fatal("the accepted stream read bytes other than the client wrote")
	}
}

// testConfig is the limits the spec sets, with a log that discards.
func testConfig() ListenerConfig {
	return ListenerConfig{
		HandshakeDeadline: 5 * time.Second,
		MaxUnauth:         32,
		MaxUnauthPerIP:    4,
		Log:               slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// manualClock is a clock the test moves by hand. It is safe for the listener's goroutines to read.
type manualClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// syncBuffer is a bytes.Buffer that the listener's goroutines may write while the test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// waitRejected waits until l has counted want refusals, and fails if it counts more.
func waitRejected(t *testing.T, l *Listener, want uint64) {
	t.Helper()
	deadline := time.Now().Add(acceptWait)
	for l.Rejected() < want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := l.Rejected(); got != want {
		t.Fatalf("Rejected = %d, want %d", got, want)
	}
}

// Case 1: a client with the admitted key completes Noise over TCP, and Accept returns its stream.
func TestListenerAcceptsAdmittedClient(t *testing.T) {
	defer failOnPanic(t)
	ln := listen(t)
	hostKP, clientKP := newKeyPair(t), newKeyPair(t)
	admit := func(remote [32]byte) bool { return remote == clientKP.Public }
	l := NewListener(ln, hostKP, admit, testConfig())
	results := startAccepting(l)
	client, err := clientHandshake(t, dial(t, ln), clientKP, hostKP.Public)
	if err != nil {
		t.Fatalf("client Handshake: %v", err)
	}
	server := nextStream(t, results)
	if server.RemotePublicKey() != clientKP.Public {
		t.Fatal("Accept returned a stream whose remote key is not the admitted client's")
	}
	expectEcho(t, client, server, "ping")
}

// Case 2: a client with another key is closed and never returned by Accept.
func TestListenerRefusesOtherKey(t *testing.T) {
	defer failOnPanic(t)
	ln := listen(t)
	hostKP, clientKP, otherKP := newKeyPair(t), newKeyPair(t), newKeyPair(t)
	admit := func(remote [32]byte) bool { return remote == clientKP.Public }
	l := NewListener(ln, hostKP, admit, testConfig())
	results := startAccepting(l)

	start := time.Now()
	if _, err := clientHandshake(t, dial(t, ln), otherKP, hostKP.Public); err == nil {
		t.Fatal("a client with another key completed the handshake")
	}
	if time.Since(start) >= handshakeWait-time.Second {
		t.Fatal("the listener held the connection of another key open until the client gave up")
	}

	client, err := clientHandshake(t, dial(t, ln), clientKP, hostKP.Public)
	if err != nil {
		t.Fatalf("admitted client Handshake: %v", err)
	}
	server := nextStream(t, results)
	if server.RemotePublicKey() != clientKP.Public {
		t.Fatal("Accept returned a stream for a key that was not admitted")
	}
	expectEcho(t, client, server, "ping")
}

// Case 3: a client that sends nothing is closed when the handshake deadline passes.
func TestListenerClosesSilentClient(t *testing.T) {
	defer failOnPanic(t)
	ln := listen(t)
	hostKP := newKeyPair(t)
	cfg := testConfig()
	cfg.HandshakeDeadline = shortDeadline
	l := NewListener(ln, hostKP, func([32]byte) bool { return false }, cfg)
	startAccepting(l)

	start := time.Now()
	c := dial(t, ln)
	if idle(t, c, 3*time.Second) {
		t.Fatal("silent client still open 3 s after a 300 ms handshake deadline")
	}
	if elapsed := time.Since(start); elapsed < shortDeadline/2 {
		t.Fatalf("silent client closed after %v, before the handshake deadline", elapsed)
	}
}

// Case 4: the 5th unauthenticated connection from one IP is closed at once; the first 4 stay.
func TestListenerCapsConnectionsPerIP(t *testing.T) {
	defer failOnPanic(t)
	ln := listen(t)
	hostKP := newKeyPair(t)
	l := NewListener(ln, hostKP, func([32]byte) bool { return false }, testConfig())
	startAccepting(l)

	var held []net.Conn
	for i := 0; i < 4; i++ {
		held = append(held, dial(t, ln))
	}
	expectOpen(t, held)
	expectClosed(t, dial(t, ln), atOnce, "5th unauthenticated connection from one IP")
	expectOpen(t, held)
}

// Case 5: the 33rd unauthenticated connection overall is closed at once.
func TestListenerCapsUnauthenticatedTotal(t *testing.T) {
	defer failOnPanic(t)
	ln := listen(t)
	hostKP := newKeyPair(t)
	cfg := testConfig()
	// Every test connection comes from 127.0.0.1, and the per-IP cap would refuse the 5th of them
	// first. Raising that cap leaves only the total cap in play.
	cfg.MaxUnauthPerIP = 64
	l := NewListener(ln, hostKP, func([32]byte) bool { return false }, cfg)
	startAccepting(l)

	var held []net.Conn
	for i := 0; i < 32; i++ {
		held = append(held, dial(t, ln))
	}
	expectOpen(t, held)
	expectClosed(t, dial(t, ln), atOnce, "33rd unauthenticated connection overall")
	expectOpen(t, held)
}

// Case 6: refusals are counted, and the log line appears at most once a minute on a fake clock.
func TestListenerCountsAndLogsRefusals(t *testing.T) {
	defer failOnPanic(t)
	ln := listen(t)
	hostKP := newKeyPair(t)
	clock := &manualClock{now: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	var logs syncBuffer
	cfg := testConfig()
	cfg.Log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg.now = clock.Now
	l := NewListener(ln, hostKP, func([32]byte) bool { return false }, cfg)
	startAccepting(l)

	// Four connections fill the per-IP cap of 4. Each later dial is one refusal.
	var held []net.Conn
	for i := 0; i < 4; i++ {
		held = append(held, dial(t, ln))
	}
	expectOpen(t, held)
	logLines := func() int { return strings.Count(logs.String(), "\n") }

	for i := 0; i < 3; i++ {
		dial(t, ln)
	}
	waitRejected(t, l, 3)
	if n := logLines(); n != 1 {
		t.Fatalf("3 refusals in one minute logged %d lines, want 1", n)
	}

	clock.Advance(59 * time.Second)
	dial(t, ln)
	waitRejected(t, l, 4)
	if n := logLines(); n != 1 {
		t.Fatalf("a refusal 59 s after the first logged again: %d lines, want 1", n)
	}

	clock.Advance(2 * time.Second)
	dial(t, ln)
	waitRejected(t, l, 5)
	if n := logLines(); n != 2 {
		t.Fatalf("a refusal more than a minute after the first logged %d lines, want 2", n)
	}

	if strings.Contains(logs.String(), hex.EncodeToString(hostKP.Secret[:32])) {
		t.Fatal("the log holds the host key seed")
	}
}
