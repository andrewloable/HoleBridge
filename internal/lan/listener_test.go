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
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/pears/noise"
	"github.com/andrewloable/HoleBridge/pears/secretstream"
	"golang.org/x/crypto/blake2b"
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
	// the client must send one message before the host hands the stream out (proof, HoleBridge-hb5.18.7)
	sendProof(t, client)
	server := nextStream(t, results)
	readProof(t, server)
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
	// the client must send one message before the host hands the stream out (proof, HoleBridge-hb5.18.7)
	sendProof(t, client)
	server := nextStream(t, results)
	readProof(t, server)
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

// Extra: a peer that opens with a length prefix naming 16 MiB is closed on that prefix, before any
// of its bytes is read, so the connection does not wait for the handshake deadline.
func TestListenerRefusesHugePreAuthFrameAtOnce(t *testing.T) {
	defer failOnPanic(t)
	ln := listen(t)
	hostKP := newKeyPair(t)
	l := NewListener(ln, hostKP, func([32]byte) bool { return true }, testConfig())
	startAccepting(l)

	c := dial(t, ln)
	if _, err := c.Write([]byte{0xff, 0xff, 0xff}); err != nil {
		t.Fatalf("write: %v", err)
	}
	expectClosed(t, c, atOnce, "peer that sends a 16 MiB frame before it is authenticated")
}

// Extra: a handshake that ends without admitting its client, by refusal or by failure, gives its
// slot back. Eight such handshakes in turn, twice the per-IP cap, must not refuse the connections
// that follow.
func TestListenerFreesSlotsWhenHandshakesEnd(t *testing.T) {
	defer failOnPanic(t)
	ln := listen(t)
	hostKP, otherKP := newKeyPair(t), newKeyPair(t)
	l := NewListener(ln, hostKP, func([32]byte) bool { return false }, testConfig())
	startAccepting(l)

	for i := 0; i < 8; i++ {
		if _, err := clientHandshake(t, dial(t, ln), otherKP, hostKP.Public); err == nil {
			t.Fatal("a client whose key is not admitted completed the handshake")
		}
	}
	var held []net.Conn
	for i := 0; i < 4; i++ {
		held = append(held, dial(t, ln))
	}
	expectOpen(t, held)
	waitRejected(t, l, 0)
}

// countingConn counts the bytes its Read returns, keepalive frames included.
type countingConn struct {
	net.Conn
	read atomic.Int64
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.read.Add(int64(n))
	return n, err
}

// Extra: an accepted stream sends keepalive frames while it is idle, at the interval the listener is
// configured with (50 ms here, so the test is quick). Read skips keepalives, so the client counts the
// bytes its connection reads: an idle stream must grow the count by two keepalive frames.
func TestListenerStreamsSendKeepalive(t *testing.T) {
	defer failOnPanic(t)
	ln := listen(t)
	hostKP, clientKP := newKeyPair(t), newKeyPair(t)
	admit := func(remote [32]byte) bool { return remote == clientKP.Public }
	const keepalive = 50 * time.Millisecond
	cfg := testConfig()
	cfg.Keepalive = keepalive
	l := NewListener(ln, hostKP, admit, cfg)
	results := startAccepting(l)

	c := &countingConn{Conn: dial(t, ln)}
	client := secretstream.New(c, true, secretstream.Options{KeyPair: clientKP, RemotePublicKey: &hostKP.Public})
	ctx, cancel := context.WithTimeout(context.Background(), handshakeWait)
	defer cancel()
	if err := client.Handshake(ctx); err != nil {
		t.Fatalf("client Handshake: %v", err)
	}
	// the client must send one message before the host hands the stream out (proof, HoleBridge-hb5.18.7)
	sendProof(t, client)
	server := nextStream(t, results)
	readProof(t, server)
	if got := server.Keepalive(); got != keepalive {
		t.Fatalf("accepted stream Keepalive() = %v, want %v", got, keepalive)
	}

	// Read blocks on the idle stream, because only keepalive frames arrive. It ends when the client is destroyed.
	t.Cleanup(func() { client.Destroy() })
	go func() {
		client.Read(make([]byte, 1))
	}()

	base := c.read.Load()
	deadline := time.Now().Add(2 * time.Second)
	for c.read.Load() < base+40 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := c.read.Load() - base; got < 40 {
		t.Fatalf("idle accepted stream sent %d bytes in 2 s, want at least 40 (two keepalive frames)", got)
	}
}

// recordingConn keeps a copy of every write its owner makes and of every byte it reads, so a test can
// replay what a real client sent and check its own transcript against the implementation's.
type recordingConn struct {
	net.Conn
	mu     sync.Mutex
	writes [][]byte
	reads  []byte
}

func (c *recordingConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.writes = append(c.writes, append([]byte(nil), p...))
	c.mu.Unlock()
	return c.Conn.Write(p)
}

func (c *recordingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.mu.Lock()
	c.reads = append(c.reads, p[:n]...)
	c.mu.Unlock()
	return n, err
}

// written returns a copy of each write so far, in order.
func (c *recordingConn) written() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]byte, len(c.writes))
	copy(out, c.writes)
	return out
}

// readBytes returns a copy of every byte read so far.
func (c *recordingConn) readBytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.reads...)
}

// handshakeHash is the Noise handshake hash after message 1 and message 2 of an IK handshake to hostPub.
// It is computed from the public bytes alone: each token of the two messages is mixed in, in order.
// An attacker that holds the two messages computes the same value, so it can compute the stream id a
// replayed message 1 needs.
func handshakeHash(hostPub [32]byte, msg1, msg2 []byte) [64]byte {
	mix := func(h [64]byte, data []byte) [64]byte {
		return blake2b.Sum512(append(h[:], data...))
	}
	var h [64]byte
	copy(h[:], "Noise_IK_Ed25519_ChaChaPoly_BLAKE2b")
	h = mix(h, nil)          // the empty prologue
	h = mix(h, hostPub[:])   // the responder's static key, known to the initiator before message 1
	h = mix(h, msg1[:32])    // message 1: the initiator's ephemeral key
	h = mix(h, msg1[32:80])  // message 1: the initiator's static key, encrypted
	h = mix(h, msg1[80:])    // message 1: the empty payload, encrypted
	h = mix(h, msg2[:32])    // message 2: the responder's ephemeral key
	return mix(h, msg2[32:]) // message 2: the empty payload, encrypted
}

// initiatorStreamID is the stream id that secretstream expects in the initiator's header frame: BLAKE2b-256
// keyed with the handshake hash, over the initiator's namespace, which is derived as secretstream derives it.
func initiatorStreamID(hash [64]byte) [32]byte {
	base := blake2b.Sum256([]byte("hyperswarm/secret-stream"))
	ns := blake2b.Sum256(append(base[:], 0))
	h, _ := blake2b.New256(hash[:])
	h.Write(ns[:])
	var id [32]byte
	h.Sum(id[:0])
	return id
}

// replayDeadline is the handshake deadline of the proof tests. It is long enough to look at a
// connection before the deadline, and short enough that the tests stay quick.
const replayDeadline = 1500 * time.Millisecond

// Proof: a replayed message 1 completes the header exchange but sends no authenticated message. It
// keeps its pre-auth slot until the handshake deadline, is closed then and counted as a refusal, and
// never comes out of Accept.
func TestListenerRefusesReplayedMessageOne(t *testing.T) {
	defer failOnPanic(t)
	ln := listen(t)
	hostKP, clientKP := newKeyPair(t), newKeyPair(t)
	admit := func(remote [32]byte) bool { return remote == clientKP.Public }
	cfg := testConfig()
	cfg.HandshakeDeadline = replayDeadline
	l := NewListener(ln, hostKP, admit, cfg)
	results := startAccepting(l)

	// A real client handshakes once. The connection records what it writes (message 1 and its stream
	// header) and what it reads (the host's message 2). The client then leaves without a message of its own.
	captured := &recordingConn{Conn: dial(t, ln)}
	real, err := clientHandshake(t, captured, clientKP, hostKP.Public)
	if err != nil {
		t.Fatalf("real client Handshake: %v", err)
	}
	real.Destroy()
	writes := captured.written()
	if len(writes) != 2 || len(writes[0]) != 3+96 || len(writes[1]) != 3+32+24 {
		t.Fatalf("the real handshake wrote %d messages, want message 1 (96 bytes) and the header (56 bytes)", len(writes))
	}
	msg1 := writes[0][3:]
	msg2, err := readFrame(bytes.NewReader(captured.readBytes()), maxNoiseMessage)
	if err != nil || len(msg2) != 48 {
		t.Fatalf("the real handshake's message 2: %d bytes, %v", len(msg2), err)
	}
	// The helpers must agree with the implementation on the real session, or the replay below proves nothing.
	if handshakeHash(hostKP.Public, msg1, msg2) != real.HandshakeHash() {
		t.Fatal("the test's handshake hash differs from the implementation's on the real session")
	}
	wantID := initiatorStreamID(real.HandshakeHash())
	if !bytes.Equal(writes[1][3:35], wantID[:]) {
		t.Fatal("the test's stream id differs from the implementation's on the real session")
	}
	// Give the host time to see that connection end and return its slot before the test fills the cap.
	time.Sleep(silenceWait)

	// Three silent connections from this IP take three of the four per-IP slots. The replay takes the fourth.
	var held []net.Conn
	for i := 0; i < 3; i++ {
		held = append(held, dial(t, ln))
	}
	expectOpen(t, held)

	// The replay: message 1 as captured. The attacker reads the host's new message 2, computes the stream
	// id for that handshake from the public transcript, and sends the captured header under it. Nothing
	// authenticated follows.
	replay := dial(t, ln)
	start := time.Now()
	if _, err := replay.Write(writes[0]); err != nil {
		t.Fatalf("replay write of message 1: %v", err)
	}
	if err := replay.SetReadDeadline(time.Now().Add(replyWait)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	fresh, err := readFrame(replay, maxNoiseMessage)
	if err != nil {
		t.Fatalf("host did not answer the replayed message 1: %v", err)
	}
	if len(fresh) != 48 {
		t.Fatalf("host message 2 is %d bytes, want 48", len(fresh))
	}
	id := initiatorStreamID(handshakeHash(hostKP.Public, msg1, fresh))
	if err := writeFrame(replay, append(id[:], writes[1][3+32:]...)); err != nil {
		t.Fatalf("replay write of the header: %v", err)
	}
	if _, err := readFrame(replay, maxNoiseMessage); err != nil {
		t.Fatalf("host did not send its header to the replay: %v", err)
	}

	select {
	case <-results:
		t.Fatal("Accept returned a stream for a replayed message 1 that sent no authenticated message")
	case <-time.After(silenceWait):
	}

	// The replay still holds its slot, so a fifth connection from this IP is closed at once, and the
	// replay itself is still open before the deadline.
	expectClosed(t, dial(t, ln), atOnce, "fifth connection from one IP while a replay holds a slot")
	if !idle(t, replay, 10*time.Millisecond) {
		t.Fatal("replayed connection closed before the handshake deadline")
	}

	// The handshake deadline closes the replay, and not before it.
	expectClosed(t, replay, replayDeadline+atOnce, "replayed connection after the handshake deadline")
	if elapsed := time.Since(start); elapsed < replayDeadline-100*time.Millisecond {
		t.Fatalf("replayed connection closed after %v, before the handshake deadline of %v", elapsed, replayDeadline)
	}

	select {
	case <-results:
		t.Fatal("Accept returned a stream for a replayed message 1 that sent no authenticated message")
	default:
	}
	// Refusals: the fifth connection at accept, then the replay and the three held connections at their deadline.
	waitRejected(t, l, 5)
}

// A real client that sends messages right after connecting: Accept returns its stream once the first
// message has arrived, and the consumer reads every message in order, starting with the first.
func TestListenerDeliversFirstMessagesInOrder(t *testing.T) {
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
	// The app's first write is a Protomux open. Two messages, written straight after the handshake,
	// show that neither is lost or reordered before the consumer reads it.
	first, second := []byte("first message, sent right after connecting"), []byte("second message")
	if _, err := client.Write(first); err != nil {
		t.Fatalf("client Write: %v", err)
	}
	if _, err := client.Write(second); err != nil {
		t.Fatalf("client Write: %v", err)
	}
	server := nextStream(t, results)

	want := append(append([]byte(nil), first...), second...)
	got := make([]byte, len(want))
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
		t.Fatal("the accepted stream read nothing of the client's first message")
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the accepted stream read %q first, want the client's messages %q", got, want)
	}
	expectEcho(t, client, server, "ping")
}

// Keepalive frames decrypt, but they are not proof: a client that sends only keepalives keeps its
// connection until the handshake deadline, and Accept never returns its stream.
func TestListenerKeepaliveIsNotProof(t *testing.T) {
	defer failOnPanic(t)
	ln := listen(t)
	hostKP, clientKP := newKeyPair(t), newKeyPair(t)
	admit := func(remote [32]byte) bool { return remote == clientKP.Public }
	cfg := testConfig()
	cfg.HandshakeDeadline = replayDeadline
	l := NewListener(ln, hostKP, admit, cfg)
	results := startAccepting(l)

	conn := &recordingConn{Conn: dial(t, ln)}
	client := secretstream.New(conn, true, secretstream.Options{KeyPair: clientKP, RemotePublicKey: &hostKP.Public, Keepalive: 20 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), handshakeWait)
	defer cancel()
	if err := client.Handshake(ctx); err != nil {
		t.Fatalf("client Handshake: %v", err)
	}
	t.Cleanup(func() { client.Destroy() })

	// Half the deadline passes with keepalives only. The client's keepalives must really go out, so
	// the test covers frames the host decrypts and skips.
	time.Sleep(replayDeadline / 2)
	if n := len(conn.written()); n <= 2 {
		t.Fatalf("the client wrote %d messages, want keepalives after its two handshake messages", n)
	}
	select {
	case <-results:
		t.Fatal("Accept returned a stream for a client that sent only keepalives")
	default:
	}
	if !idle(t, conn, 10*time.Millisecond) {
		t.Fatal("the connection closed before the handshake deadline")
	}
	expectClosed(t, conn, replayDeadline, "a connection that sent only keepalives")
	waitRejected(t, l, 1)
	select {
	case <-results:
		t.Fatal("Accept returned a stream for a client that sent only keepalives")
	default:
	}
}

// proofMessage is the message a test client sends to prove its key (HoleBridge-hb5.18.7).
const proofMessage = "proof"

// sendProof writes the proof message from the client, the one message it must send before the host
// hands the stream out.
func sendProof(t *testing.T, client *secretstream.Stream) {
	t.Helper()
	if _, err := client.Write([]byte(proofMessage)); err != nil {
		t.Fatalf("client Write of the proof message: %v", err)
	}
}

// readProof reads the proof message from the accepted stream, so the stream is idle after it.
func readProof(t *testing.T, server *secretstream.Stream) {
	t.Helper()
	got := make([]byte, len(proofMessage))
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(server, got)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("read of the client's proof message: %v", err)
		}
	case <-time.After(replyWait):
		t.Fatal("the accepted stream read nothing of the client's proof message")
	}
}

// Cap: before its first message decrypts, a peer may send no frame that names more than 65535 bytes.
// A longer length prefix closes the connection at once, before any of its payload is read, and counts
// as a refusal.
func TestListenerClosesOversizedFrameBeforeProof(t *testing.T) {
	defer failOnPanic(t)
	ln := listen(t)
	hostKP, clientKP := newKeyPair(t), newKeyPair(t)
	admit := func(remote [32]byte) bool { return remote == clientKP.Public }
	l := NewListener(ln, hostKP, admit, testConfig())
	results := startAccepting(l)

	// A real client completes the header exchange, then writes a length prefix that names 65536 bytes.
	raw := dial(t, ln)
	client := secretstream.New(raw, true, secretstream.Options{KeyPair: clientKP, RemotePublicKey: &hostKP.Public})
	ctx, cancel := context.WithTimeout(context.Background(), handshakeWait)
	defer cancel()
	if err := client.Handshake(ctx); err != nil {
		t.Fatalf("client Handshake: %v", err)
	}
	if _, err := raw.Write([]byte{0x00, 0x00, 0x01}); err != nil {
		t.Fatalf("write of the length prefix: %v", err)
	}
	expectClosed(t, raw, atOnce, "frame of 65536 bytes before the peer is proven")
	waitRejected(t, l, 1)
	select {
	case <-results:
		t.Fatal("Accept returned a stream for a peer that sent no proof")
	default:
	}
}

// The cap is exact: a first message whose sealed frame names exactly 65535 bytes is accepted, and the
// consumer reads it whole.
func TestListenerAcceptsFirstFrameAtCap(t *testing.T) {
	defer failOnPanic(t)
	ln := listen(t)
	hostKP, clientKP := newKeyPair(t), newKeyPair(t)
	admit := func(remote [32]byte) bool { return remote == clientKP.Public }
	l := NewListener(ln, hostKP, admit, testConfig())
	results := startAccepting(l)

	raw := &recordingConn{Conn: dial(t, ln)}
	client, err := clientHandshake(t, raw, clientKP, hostKP.Public)
	if err != nil {
		t.Fatalf("client Handshake: %v", err)
	}
	msg := bytes.Repeat([]byte{0x5a}, 65535-secretstream.ABytes)
	if _, err := client.Write(msg); err != nil {
		t.Fatalf("client Write: %v", err)
	}
	if writes := raw.written(); len(writes) != 3 || len(writes[2]) != 3+65535 {
		t.Fatalf("the first message was sent as a frame of %d bytes, want 65535", len(writes[len(writes)-1])-3)
	}
	server := nextStream(t, results)
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
		t.Fatal("the accepted stream read nothing of the client's first message")
	}
	if !bytes.Equal(got, msg) {
		t.Fatal("the accepted stream read bytes other than the client wrote")
	}
}

// After the proof the frame cap is gone: large frames written right behind the proof message, and one
// after a pause, all arrive whole and in order.
func TestListenerLiftsFrameCapAfterProof(t *testing.T) {
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
	msgs := [][]byte{
		[]byte(proofMessage),
		bytes.Repeat([]byte{1}, 70000),
		bytes.Repeat([]byte{2}, 1<<20),
		bytes.Repeat([]byte{3}, 4<<20),
	}
	go func() {
		for _, m := range msgs {
			if _, err := client.Write(m); err != nil {
				return
			}
		}
	}()
	server := nextStream(t, results)
	for i, m := range msgs {
		got, err := server.ReadFrame()
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		if !bytes.Equal(got, m) {
			t.Fatalf("message %d: read %d bytes, want %d", i, len(got), len(m))
		}
	}
	time.Sleep(200 * time.Millisecond)
	big := bytes.Repeat([]byte{4}, 8<<20)
	go client.Write(big)
	got, err := server.ReadFrame()
	if err != nil || !bytes.Equal(got, big) {
		t.Fatalf("large frame after a pause: %d bytes, %v", len(got), err)
	}
}

// A proven connection is no longer under the handshake deadline: idle for three times the deadline, it
// still carries messages both ways.
func TestListenerProvenConnectionOutlivesDeadline(t *testing.T) {
	defer failOnPanic(t)
	ln := listen(t)
	hostKP, clientKP := newKeyPair(t), newKeyPair(t)
	admit := func(remote [32]byte) bool { return remote == clientKP.Public }
	cfg := testConfig()
	cfg.HandshakeDeadline = 400 * time.Millisecond
	cfg.Keepalive = 50 * time.Millisecond
	l := NewListener(ln, hostKP, admit, cfg)
	results := startAccepting(l)
	client, err := clientHandshake(t, dial(t, ln), clientKP, hostKP.Public)
	if err != nil {
		t.Fatalf("client Handshake: %v", err)
	}
	sendProof(t, client)
	server := nextStream(t, results)
	readProof(t, server)
	time.Sleep(3 * cfg.HandshakeDeadline)
	expectEcho(t, client, server, "late")
	if _, err := server.Write([]byte("back")); err != nil {
		t.Fatalf("host write after the deadline: %v", err)
	}
	back := make([]byte, 4)
	if _, err := io.ReadFull(client, back); err != nil || string(back) != "back" {
		t.Fatalf("client read %q, %v", back, err)
	}
}
