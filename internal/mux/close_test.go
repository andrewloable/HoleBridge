package mux

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/internal/protocol"
)

// These tests cover the close handshake, rejects, limits and malformed input (docs/architecture.md,
// "Messages", "Limits" and "Malformed input"). They fail until HoleBridge-hb5.16.10 is implemented:
// Stream.Close, Stream.CloseWrite and Session.Close are stubs, and the limits and the cancelled-open
// cleanup are not enforced yet. The helpers pair, wire, pattern, hostTarget and acceptHook are in
// flow_test.go.

// settle bounds how long a test waits for the other side of an exchange.
const settle = 5 * time.Second

// acceptEverything is a host accept callback that accepts every service without a target.
func acceptEverything(string) AcceptResult {
	return AcceptResult{}
}

// linkPair connects an app session and a host session with the given configs, each with its own
// budget as in pair. The host accepts services with accept.
func linkPair(t *testing.T, accept func(string) AcceptResult, appCfg, hostCfg Config) (app, host *Session) {
	t.Helper()
	up, down := newWire(), newWire()
	app = NewSession(RoleApp, up, appCfg, NewBudget(64*mib), nil)
	host = NewSession(RoleHost, down, hostCfg, NewBudget(256*mib), accept)
	up.deliver(t, host)
	down.deliver(t, app)
	return app, host
}

// openStream opens a stream to service on app, giving up after settle.
func openStream(app *Session, service string) (*Stream, error) {
	ctx, cancel := context.WithTimeout(context.Background(), settle)
	defer cancel()
	return app.Open(ctx, service)
}

// returnsWithin runs f and waits up to settle for its error. It fails the test if f is still running.
func returnsWithin(t *testing.T, what string, f func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- f() }()
	select {
	case err := <-done:
		return err
	case <-time.After(settle):
		t.Fatalf("%s did not return within %v", what, settle)
		return nil
	}
}

// awaitSent waits until w has sent a message that match accepts. It fails the test if none is sent
// within settle. what names the message in the failure.
func awaitSent(t *testing.T, w *wire, what string, match func(any) bool) {
	t.Helper()
	deadline := time.Now().Add(settle)
	for {
		for _, m := range w.messages() {
			if match(m) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s was not sent within %v", what, settle)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// isOpenFor matches the open message for stream id.
func isOpenFor(id uint64) func(any) bool {
	return func(m any) bool {
		o, ok := m.(protocol.Open)
		return ok && o.Stream == id
	}
}

// isCloseFor matches the close message for stream id.
func isCloseFor(id uint64) func(any) bool {
	return func(m any) bool {
		c, ok := m.(protocol.Close)
		return ok && c.Stream == id
	}
}

// inUse returns how many stream slots c holds.
func inUse(c *Counter) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used
}

// awaitInUse waits until c holds want stream slots. It fails the test if it does not within settle.
func awaitInUse(t *testing.T, c *Counter, want int, when string) {
	t.Helper()
	deadline := time.Now().Add(settle)
	for inUse(c) != want {
		if time.Now().After(deadline) {
			t.Fatalf("%s: the counter holds %d stream slots, want %d", when, inUse(c), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// testClock is a Config.Clock that the test moves by hand.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Case 1: the app writes 10 KiB and calls CloseWrite. The host reads all of it and then io.EOF with no
// error. The host can still write back, and the app reads it (half-close).
func TestCloseWriteHalfCloses(t *testing.T) {
	app, host := linkPair(t, acceptEverything, testConfig(), testConfig())
	st, err := openStream(app, "web")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	target := hostTarget(t, host, st.ID())
	want := pattern(10 << 10)
	if _, err := st.Write(want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := st.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	var got []byte
	err = returnsWithin(t, "the host reading to io.EOF", func() error {
		var rerr error
		got, rerr = io.ReadAll(target)
		return rerr
	})
	if err != nil {
		t.Fatalf("host read after the app's CloseWrite ended with %v, want a clean io.EOF", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("host read %d bytes before EOF, want the %d bytes written before CloseWrite", len(got), len(want))
	}
	reply := []byte("pong")
	if _, err := target.Write(reply); err != nil {
		t.Fatalf("host Write after the app's CloseWrite: %v, want the half-closed stream to still take writes", err)
	}
	back := make([]byte, len(reply))
	err = returnsWithin(t, "the app reading the host's reply", func() error {
		_, rerr := io.ReadFull(st, back)
		return rerr
	})
	if err != nil || !bytes.Equal(back, reply) {
		t.Fatalf("app read %q (error %v) after the host wrote %q", back, err, reply)
	}
}

// Case 2: both sides close the stream. Both Close calls return nil, and the stream count of each end
// goes back to 0. Each session has its own Counter, so each end's count is checked on its own.
func TestBothSidesCloseFreesTheStream(t *testing.T) {
	appLimits, hostLimits := NewCounter(1024), NewCounter(1024)
	appCfg, hostCfg := testConfig(), testConfig()
	appCfg.Limits, hostCfg.Limits = appLimits, hostLimits
	app, host := linkPair(t, acceptEverything, appCfg, hostCfg)
	st, err := openStream(app, "web")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	target := hostTarget(t, host, st.ID())
	if a, h := inUse(appLimits), inUse(hostLimits); a != 1 || h != 1 {
		t.Errorf("after Open the counters hold %d (app) and %d (host) stream slots, want 1 and 1: the stream count is not kept yet (not implemented)", a, h)
	}
	closed := make(chan error, 2)
	go func() { closed <- st.Close() }()
	go func() { closed <- target.Close() }()
	for i := 0; i < 2; i++ {
		select {
		case err := <-closed:
			if err != nil {
				t.Fatalf("Close: %v", err)
			}
		case <-time.After(settle):
			t.Fatalf("Close did not return within %v", settle)
		}
	}
	awaitInUse(t, appLimits, 0, "app side after both sides closed")
	awaitInUse(t, hostLimits, 0, "host side after both sides closed")
}

// Case 3: the host rejects the service with code 1 (unknown service). Open returns a *RejectError with
// that code and the reason, and no stream.
func TestRejectWithCodeOneReturnsRejectError(t *testing.T) {
	reject := func(string) AcceptResult {
		return AcceptResult{Code: 1, Reason: "unknown service"}
	}
	app, _ := linkPair(t, reject, testConfig(), testConfig())
	st, err := openStream(app, "nope")
	if st != nil {
		t.Errorf("Open returned a stream for a rejected service")
	}
	var rej *RejectError
	if !errors.As(err, &rej) {
		t.Fatalf("Open error = %v, want a *RejectError", err)
	}
	if rej.Code != 1 || rej.Reason != "unknown service" {
		t.Fatalf("RejectError = {Code %d, Reason %q}, want {Code 1, Reason %q}", rej.Code, rej.Reason, "unknown service")
	}
}

// Case 4: a session admits 128 streams (Config.MaxStreams). The 129th open is rejected with code 2, limit
// reached.
func TestStreamLimitPerSessionRejectsWithCodeTwo(t *testing.T) {
	app, _ := linkPair(t, acceptEverything, testConfig(), testConfig())
	for i := 1; i <= 128; i++ {
		if _, err := openStream(app, "web"); err != nil {
			t.Fatalf("stream %d: Open: %v, want the first 128 streams to open", i, err)
		}
	}
	_, err := openStream(app, "web")
	var rej *RejectError
	if !errors.As(err, &rej) || rej.Code != 2 {
		t.Fatalf("stream 129: Open error = %v, want a RejectError with code 2 (limit reached); the per-session limit of 128 is not enforced yet (not implemented)", err)
	}
}

// Case 5: two app sessions on one host share a Counter of 3 streams. Three streams open across the two
// sessions. A fourth, on the second session, is rejected with code 2.
func TestSharedCounterRejectsFourthStreamWithCodeTwo(t *testing.T) {
	shared := NewCounter(3)
	hostCfg := testConfig()
	hostCfg.Limits = shared
	app1, _ := linkPair(t, acceptEverything, testConfig(), hostCfg)
	app2, _ := linkPair(t, acceptEverything, testConfig(), hostCfg)
	for _, app := range []*Session{app1, app1, app2} {
		if _, err := openStream(app, "web"); err != nil {
			t.Fatalf("Open: %v, want the first 3 streams across the two sessions to open", err)
		}
	}
	_, err := openStream(app2, "web")
	var rej *RejectError
	if !errors.As(err, &rej) || rej.Code != 2 {
		t.Fatalf("fourth stream: Open error = %v, want a RejectError with code 2 (limit reached); the shared Counter is not enforced yet (not implemented)", err)
	}
}

// Case 6: the host sends data on a stream the app opened, before opened. That resets this stream only:
// its Open fails, the session stays up, and a second stream opens and carries on.
func TestDataBeforeOpenedResetsOnlyThatStream(t *testing.T) {
	sent := newWire()
	app := NewSession(RoleApp, sent, testConfig(), NewBudget(64*mib), nil)
	first := make(chan error, 1)
	go func() {
		_, err := openStream(app, "web")
		first <- err
	}()
	awaitSent(t, sent, "open for stream 1", isOpenFor(1))
	if err := app.Receive(protocol.Data{Stream: 1, Payload: []byte("early")}); err != nil {
		t.Fatalf("Receive(data before opened) = %v, want nil: the stream is reset and the session stays up", err)
	}
	err := <-first
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Open of stream 1 still waiting after %v: data before opened did not reset it (not implemented)", settle)
	}
	if err == nil {
		t.Fatal("Open of stream 1 succeeded after data before opened, want its reset error")
	}
	second := make(chan error, 1)
	var st2 *Stream
	go func() {
		var oerr error
		st2, oerr = openStream(app, "web")
		second <- oerr
	}()
	awaitSent(t, sent, "open for stream 2", isOpenFor(2))
	if err := app.Receive(protocol.Opened{Stream: 2, Window: 2 * mib}); err != nil {
		t.Fatalf("Receive(opened for stream 2): %v", err)
	}
	if err := <-second; err != nil || st2 == nil || st2.ID() != 2 {
		t.Fatalf("stream 2 after the reset: Open returned stream %v and error %v, want stream 2 open", st2, err)
	}
}

// Case 7: data for a stream id the host never opened is an error from Receive, and that error closes the
// session.
func TestDataForUnknownStreamErrorsTheSession(t *testing.T) {
	host := NewSession(RoleHost, newWire(), testConfig(), NewBudget(256*mib), acceptEverything)
	if err := host.Receive(protocol.Data{Stream: 9, Payload: []byte("x")}); err == nil {
		t.Fatal("Receive(data for stream 9, never opened) = nil, want an error that closes the session")
	}
}

// Case 8: the app's close ends the host's read side of stream 1. The host's own close answers it only when the
// host closes its side, and then the stream is finished. Within 60 s of that, a late window and a late close
// for the stream are ignored: Receive returns nil.
func TestLateWindowAndCloseIgnoredWithin60Seconds(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)}
	cfg := testConfig()
	cfg.Clock = clock.Now
	out := newWire()
	host := NewSession(RoleHost, out, cfg, NewBudget(256*mib), acceptEverything)
	if err := host.Receive(protocol.Open{Stream: 1, Service: "web", Window: 2 * mib}); err != nil {
		t.Fatalf("Receive(open): %v", err)
	}
	if err := host.Receive(protocol.Close{Stream: 1}); err != nil {
		t.Fatalf("Receive(close) = %v, want nil: close is a handshake", err)
	}
	for _, m := range out.messages() {
		if isCloseFor(1)(m) {
			t.Fatalf("the host answered the close before its own side closed")
		}
	}
	if err := hostTarget(t, host, 1).CloseWrite(); err != nil {
		t.Fatalf("host CloseWrite: %v", err)
	}
	awaitSent(t, out, "the host's close once its side closes", isCloseFor(1))
	clock.Advance(59 * time.Second)
	if err := host.Receive(protocol.Window{Stream: 1, Credit: 64 << 10, Received: 0}); err != nil {
		t.Fatalf("late window 59 s after the close = %v, want it ignored", err)
	}
	if err := host.Receive(protocol.Close{Stream: 1}); err != nil {
		t.Fatalf("late close 59 s after the close = %v, want it ignored", err)
	}
}

// Case 9: closing the session fails every open stream's Read and Write. One stream sits idle in Read, and
// another is blocked in Write at its 2 MiB window because the host does not read.
func TestSessionCloseFailsReadsAndWrites(t *testing.T) {
	app, _, up, _ := pair(t, acceptEverything)
	idle, err := openStream(app, "web")
	if err != nil {
		t.Fatalf("Open idle stream: %v", err)
	}
	blocked, err := openStream(app, "web")
	if err != nil {
		t.Fatalf("Open blocked stream: %v", err)
	}
	reading := make(chan error, 1)
	go func() {
		_, rerr := idle.Read(make([]byte, 16))
		reading <- rerr
	}()
	writing := make(chan error, 1)
	go func() {
		_, werr := blocked.Write(pattern(4 * mib))
		writing <- werr
	}()
	deadline := time.Now().Add(settle)
	for up.dataBytes() < 2*mib && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := up.dataBytes(); got != 2*mib {
		t.Fatalf("the write sent %d bytes before blocking, want the granted 2 MiB", got)
	}
	if err := app.Close(); err != nil {
		t.Fatalf("closing the session: %v", err)
	}
	select {
	case rerr := <-reading:
		if rerr == nil {
			t.Error("Read on an open stream returned nil after Session.Close, want an error")
		}
	case <-time.After(settle):
		t.Fatalf("Read still blocked %v after Session.Close", settle)
	}
	select {
	case werr := <-writing:
		if werr == nil {
			t.Error("Write on an open stream returned nil after Session.Close, want an error")
		}
	case <-time.After(settle):
		t.Fatalf("Write still blocked %v after Session.Close", settle)
	}
}

// Cancelled open (not one of the nine cases): an Open whose context is cancelled while the host has not
// answered returns the cancellation. It gives back its stream slot, sends close for its stream so the
// host frees its side, and a late opened for that stream is ignored instead of closing the session.
// The docs do not fix this cleanup; the test records the shape the task notes choose.
func TestCancelledOpenIsCleanedUp(t *testing.T) {
	limits := NewCounter(1024)
	cfg := testConfig()
	cfg.Limits = limits
	sent := newWire()
	app := NewSession(RoleApp, sent, cfg, NewBudget(64*mib), nil)
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() {
		_, err := app.Open(ctx, "web")
		res <- err
	}()
	awaitSent(t, sent, "open for stream 1", isOpenFor(1))
	cancel()
	select {
	case err := <-res:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled Open returned %v, want context.Canceled", err)
		}
	case <-time.After(settle):
		t.Fatalf("cancelled Open did not return within %v", settle)
	}
	awaitInUse(t, limits, 0, "after the cancelled open")
	awaitSent(t, sent, "close for cancelled stream 1 (cancelled-open cleanup is not implemented)", isCloseFor(1))
	if err := app.Receive(protocol.Opened{Stream: 1, Window: 2 * mib}); err != nil {
		t.Fatalf("Receive(late opened for the cancelled stream) = %v, want nil: the session stays up", err)
	}
}

// awaitReading waits until a Read is blocked waiting for bytes on st. It fails the test if none is within
// settle. A Read holds the session lock until it waits, so a count seen under the lock means it waits.
func awaitReading(t *testing.T, st *Stream) {
	t.Helper()
	deadline := time.Now().Add(settle)
	for {
		s := st.lock()
		waiting := st.readers > 0
		s.mu.Unlock()
		if waiting {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no Read was blocked on the stream within %v", settle)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Half-close with a blocked read (HoleBridge-hb5.16.11): the host's target is blocked in Read, waiting for
// bytes, when the app's CloseWrite arrives. The host does not answer while the read waits, so the read
// returns io.EOF, the host writes back, and the app reads the reply. Only the host's own CloseWrite sends
// its close, and the app's stream then completes.
func TestCloseWithBlockedReadWaitsForTheLocalSide(t *testing.T) {
	app, host, _, down := pair(t, acceptEverything)
	st, err := openStream(app, "web")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	target := hostTarget(t, host, st.ID())
	reading := make(chan error, 1)
	go func() {
		_, rerr := target.Read(make([]byte, 16))
		reading <- rerr
	}()
	awaitReading(t, target)
	if err := st.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	select {
	case rerr := <-reading:
		if rerr != io.EOF {
			t.Fatalf("host's blocked Read returned %v, want io.EOF", rerr)
		}
	case <-time.After(settle):
		t.Fatalf("host's blocked Read did not return within %v after the app's close", settle)
	}
	reply := []byte("pong")
	if _, err := target.Write(reply); err != nil {
		t.Fatalf("host Write after the app's close: %v, want the half-closed stream to take the reply", err)
	}
	back := make([]byte, len(reply))
	err = returnsWithin(t, "the app reading the host's reply", func() error {
		_, rerr := io.ReadFull(st, back)
		return rerr
	})
	if err != nil || !bytes.Equal(back, reply) {
		t.Fatalf("app read %q (error %v) after the host wrote %q", back, err, reply)
	}
	for _, m := range down.messages() {
		if c, ok := m.(protocol.Close); ok && c.Stream == st.ID() {
			t.Fatalf("host sent close before its own CloseWrite: the answer must wait while a read waits")
		}
	}
	if err := target.CloseWrite(); err != nil {
		t.Fatalf("host CloseWrite: %v", err)
	}
	awaitSent(t, down, "the host's close once its side ends", isCloseFor(st.ID()))
}

// awaitPeerClosed waits until the peer's close has come for st. It fails the test if it does not within settle.
func awaitPeerClosed(t *testing.T, st *Stream) {
	t.Helper()
	deadline := time.Now().Add(settle)
	for {
		s := st.lock()
		done := st.rclosed
		s.mu.Unlock()
		if done {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the peer's close did not arrive within %v", settle)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Half-close with a close between two reads (HoleBridge-hb5.16.13): the app writes GET and CloseWrite, and the
// close lands while the host has read GET and is not reading, so no Read waits. The host can still write the
// reply, and the app reads it. A close answered at once would make that write fail with stream closed.
func TestCloseBetweenReadsLeavesTheHostAbleToWrite(t *testing.T) {
	app, host := linkPair(t, acceptEverything, testConfig(), testConfig())
	st, err := openStream(app, "web")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	target := hostTarget(t, host, st.ID())
	if _, err := st.Write([]byte("GET")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := io.ReadFull(target, make([]byte, 3)); err != nil {
		t.Fatalf("host read the request: %v", err)
	}
	if err := st.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	awaitPeerClosed(t, target)
	reply := []byte("resp")
	if _, err := target.Write(reply); err != nil {
		t.Fatalf("host Write after the app's CloseWrite: %v, want the reply to go out", err)
	}
	back := make([]byte, len(reply))
	err = returnsWithin(t, "the app reading the reply", func() error {
		_, rerr := io.ReadFull(st, back)
		return rerr
	})
	if err != nil || !bytes.Equal(back, reply) {
		t.Fatalf("app read %q (error %v) after the host wrote %q", back, err, reply)
	}
}
