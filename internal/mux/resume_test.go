package mux

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/internal/protocol"
)

// These tests cover stream resume (docs/architecture.md, "Sessions and reconnects" and "Encoding"): the
// token of opened, the kept bytes and their caps, the grace period, reattach and reattached, and the app's
// drop of data before reattached. They fail until HoleBridge-hb5.17.2 is implemented: NewResumeTable,
// ResumeTable.Bind, Session.EnableResume, Session.Detach and Session.Adopt are stubs, and Receive has no case
// for reattach. The helpers pair, wire, pattern, hostTarget, acceptHook, openeds and windows are in
// flow_test.go; settle, acceptEverything, openStream, returnsWithin, awaitSent, isCloseFor and testClock are
// in close_test.go.

// resumeWait bounds a bulk transfer, which is larger than the settle wait of the other tests.
const resumeWait = 30 * time.Second

// resumeCfg is the design's caps: 4 MiB kept per stream, 32 MiB kept in all, and a 60 s grace period.
func resumeCfg() ResumeConfig {
	return ResumeConfig{PerStream: 4 * mib, Total: 32 * mib, Grace: 60 * time.Second}
}

// stubPanic runs f and returns what it panicked with, or "" when f returned. It turns a stub that has no
// error to return into a failure the test can report.
func stubPanic(f func()) (msg string) {
	defer func() {
		if r := recover(); r != nil {
			msg = fmt.Sprint(r)
		}
	}()
	f()
	return ""
}

// newTable returns the host's resume table for cfg and clock, failing the test while NewResumeTable is a stub.
func newTable(t *testing.T, cfg ResumeConfig, clock func() time.Time) *ResumeTable {
	t.Helper()
	var tab *ResumeTable
	if msg := stubPanic(func() { tab = NewResumeTable(cfg, clock) }); msg != "" {
		t.Fatalf("NewResumeTable: %s", msg)
	}
	return tab
}

// bind makes host, a host session, resumable through tab.
func bind(t *testing.T, tab *ResumeTable, host *Session) {
	t.Helper()
	if err := tab.Bind(host); err != nil {
		t.Fatalf("ResumeTable.Bind: %v", err)
	}
}

// enableResume makes app, an app session, keep its streams when its transport drops.
func enableResume(t *testing.T, app *Session) {
	t.Helper()
	if err := app.EnableResume(); err != nil {
		t.Fatalf("Session.EnableResume: %v", err)
	}
}

// detach ends the transport of s, as a drop does, and fails the test while Detach is a stub.
func detach(t *testing.T, s *Session) {
	t.Helper()
	if msg := stubPanic(s.Detach); msg != "" {
		t.Fatalf("Session.Detach: %s", msg)
	}
}

// adopt makes next, a new app session, take over the streams that prev left detached.
func adopt(t *testing.T, next, prev *Session) {
	t.Helper()
	if err := next.Adopt(prev); err != nil {
		t.Fatalf("Session.Adopt: %v", err)
	}
}

// receive hands msg to s and fails the test if s returns an error.
func receive(t *testing.T, s *Session, msg any) {
	t.Helper()
	if err := s.Receive(msg); err != nil {
		t.Fatalf("Receive(%T): %v", msg, err)
	}
}

// doWithin runs f and waits up to resumeWait for its error. It fails the test if f is still running then.
func doWithin(t *testing.T, what string, f func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- f() }()
	select {
	case err := <-done:
		return err
	case <-time.After(resumeWait):
		t.Fatalf("%s did not finish within %v", what, resumeWait)
		return nil
	}
}

// dropLink is a wire whose messages reach dst, in order, until it is cut off. What is sent after the cut,
// or still in flight, is lost, as a dropped transport loses it. Delivery errors are reported when the test
// ends, since a goroutine must not report to t once the test has returned.
type dropLink struct {
	*wire
	mu   sync.Mutex
	dead bool
	err  error // the first error the destination returned
}

// newDropLink returns a link whose messages are recorded and wait until start is called.
func newDropLink(t *testing.T) *dropLink {
	t.Helper()
	l := &dropLink{wire: newWire()}
	t.Cleanup(func() {
		if err := l.failure(); err != nil {
			t.Errorf("delivery: %v", err)
		}
	})
	return l
}

// start delivers the link's messages to dst on a goroutine of its own. A nil dst discards them.
func (l *dropLink) start(dst *Session) {
	go func() {
		for msg := range l.ch {
			l.mu.Lock()
			skip := l.dead || l.err != nil || dst == nil
			l.mu.Unlock()
			if skip {
				continue
			}
			if err := dst.Receive(msg); err != nil {
				l.mu.Lock()
				l.err = fmt.Errorf("Receive(%T): %w", msg, err)
				l.mu.Unlock()
			}
		}
	}()
}

// cutOff drops what is sent from now on, and what is still in flight.
func (l *dropLink) cutOff() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.dead = true
}

// failure returns the first delivery error, or nil.
func (l *dropLink) failure() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

// resumeConn connects an app session and a host session over two links, and starts their delivery. The app
// session keeps its streams when it drops, and the host session is bound to tab.
func resumeConn(t *testing.T, tab *ResumeTable, accept func(string) AcceptResult) (app, host *Session, up, down *dropLink) {
	t.Helper()
	up, down = newDropLink(t), newDropLink(t)
	app = NewSession(RoleApp, up, testConfig(), NewBudget(64*mib), nil)
	host = NewSession(RoleHost, down, testConfig(), NewBudget(256*mib), accept)
	up.start(host)
	down.start(app)
	enableResume(t, app)
	bind(t, tab, host)
	return app, host, up, down
}

// dropConn drops the connection of app and host: both links are cut off and both sessions detach.
func dropConn(t *testing.T, app, host *Session, up, down *dropLink) {
	t.Helper()
	up.cutOff()
	down.cutOff()
	detach(t, app)
	detach(t, host)
}

// issuedToken returns the resume token that the host sent with opened for stream id. It fails the test if
// there is no such opened. The token is never printed.
func issuedToken(t *testing.T, down *dropLink, id uint64) [16]byte {
	t.Helper()
	for _, o := range openeds(down.messages()) {
		if o.Stream == id {
			return o.Token
		}
	}
	t.Fatalf("the host sent no opened for stream %d", id)
	return [16]byte{}
}

// dataFor returns the data payload bytes sent for stream id among msgs.
func dataFor(msgs []any, id uint64) uint64 {
	var n uint64
	for _, m := range msgs {
		if d, ok := m.(protocol.Data); ok && d.Stream == id {
			n += uint64(len(d.Payload))
		}
	}
	return n
}

// isReattachFor matches the reattach message for stream id.
func isReattachFor(id uint64) func(any) bool {
	return func(m any) bool {
		r, ok := m.(protocol.Reattach)
		return ok && r.Stream == id
	}
}

// isReattachedFor matches the reattached message for stream id.
func isReattachedFor(id uint64) func(any) bool {
	return func(m any) bool {
		r, ok := m.(protocol.Reattached)
		return ok && r.Stream == id
	}
}

// Case 1: the app sends 20 MB. The connection drops after 5 MB have reached the host, and the app reattaches
// its stream in a new session. The 20 MB arrives at the host byte-exact, and the stream's token is nonzero.
func TestTransferSurvivesADropAndReattach(t *testing.T) {
	tab := newTable(t, resumeCfg(), nil)
	app1, host1, up1, down1 := resumeConn(t, tab, acceptEverything)
	st, err := openStream(app1, "web")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if issuedToken(t, down1, st.ID()) == ([16]byte{}) {
		t.Fatal("opened carried a zero token with resume on")
	}
	target := hostTarget(t, host1, st.ID())
	want := pattern(20 * mib)
	written := make(chan error, 1)
	go func() {
		_, err := st.Write(want)
		written <- err
	}()

	head := make([]byte, 5*mib)
	if err := doWithin(t, "the host's read before the drop", func() error {
		_, err := io.ReadFull(target, head)
		return err
	}); err != nil {
		t.Fatalf("read at the host before the drop: %v", err)
	}
	dropConn(t, app1, host1, up1, down1)
	app2, _, _, _ := resumeConn(t, tab, acceptEverything)
	adopt(t, app2, app1)

	tail := make([]byte, len(want)-len(head))
	if err := doWithin(t, "the host's read after the reattach", func() error {
		_, err := io.ReadFull(target, tail)
		return err
	}); err != nil {
		t.Fatalf("read at the host after the reattach: %v", err)
	}
	select {
	case err := <-written:
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
	case <-time.After(resumeWait):
		t.Fatal("the app's Write did not finish after the reattach")
	}
	if !bytes.Equal(append(head, tail...), want) {
		t.Fatal("the 20 MB arrived altered across the reattach")
	}
}

// Case 2: after a drop, a reattach with a token that differs from the issued one in one bit is refused. The
// host sends no reattached for the stream, sends close for it, and the session stays up. The comparison must
// be constant time, so the source must call subtle.ConstantTimeCompare; timing itself is not tested.
func TestWrongTokenIsRefused(t *testing.T) {
	src, err := os.ReadFile("resume.go")
	if err != nil {
		t.Fatalf("read resume.go: %v", err)
	}
	if !strings.Contains(string(src), "subtle.ConstantTimeCompare(") {
		t.Error("resume.go does not compare tokens with subtle.ConstantTimeCompare")
	}

	tab := newTable(t, resumeCfg(), nil)
	app1, host1, up1, down1 := resumeConn(t, tab, acceptEverything)
	st, err := openStream(app1, "web")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	token := issuedToken(t, down1, st.ID())
	dropConn(t, app1, host1, up1, down1)

	// The new host session answers into a sink: the test plays the app, so no app session is linked.
	down2 := newDropLink(t)
	down2.start(nil)
	host2 := NewSession(RoleHost, down2, testConfig(), NewBudget(256*mib), acceptEverything)
	bind(t, tab, host2)
	wrong := token
	wrong[15] ^= 0x01
	if err := host2.Receive(protocol.Reattach{Stream: st.ID(), Token: wrong, Received: 0, Limit: 2 * mib}); err != nil {
		t.Fatalf("a reattach with a wrong token ended the session: %v", err)
	}
	for _, m := range down2.messages() {
		if _, ok := m.(protocol.Reattached); ok {
			t.Fatal("the host reattached a stream on a wrong token")
		}
	}
	awaitSent(t, down2.wire, "close for the refused stream", isCloseFor(st.ID()))
}

// Case 3: the grace period is 60 s on a fake clock. A stream reattached 59 s after the drop resumes. A stream
// reattached 61 s after the drop is refused: the host sends close for it, and the app's stream closes as it
// would without resume.
func TestNoReattachWithinGraceClosesTheStream(t *testing.T) {
	t.Run("reattach at 59 s resumes", func(t *testing.T) {
		clock := &testClock{}
		tab := newTable(t, resumeCfg(), clock.Now)
		app1, host1, up1, down1 := resumeConn(t, tab, acceptEverything)
		st, err := openStream(app1, "web")
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		dropConn(t, app1, host1, up1, down1)
		clock.Advance(59 * time.Second)

		app2, _, _, down2 := resumeConn(t, tab, acceptEverything)
		adopt(t, app2, app1)
		awaitSent(t, down2.wire, "reattached for the stream", isReattachedFor(st.ID()))
	})

	t.Run("reattach at 61 s is refused and closes the stream", func(t *testing.T) {
		clock := &testClock{}
		tab := newTable(t, resumeCfg(), clock.Now)
		app1, host1, up1, down1 := resumeConn(t, tab, acceptEverything)
		st, err := openStream(app1, "web")
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		dropConn(t, app1, host1, up1, down1)
		clock.Advance(61 * time.Second)

		app2, _, _, down2 := resumeConn(t, tab, acceptEverything)
		adopt(t, app2, app1)
		awaitSent(t, down2.wire, "close for the expired stream", isCloseFor(st.ID()))
		for _, m := range down2.messages() {
			if r, ok := m.(protocol.Reattached); ok && r.Stream == st.ID() {
				t.Fatal("the host reattached a stream after the grace period")
			}
		}
		if err := returnsWithin(t, "Read on the app's stream", func() error {
			_, err := st.Read(make([]byte, 1))
			return err
		}); err == nil {
			t.Error("the app's stream still reads after the grace period, want it closed")
		}
	})
}

// Case 4: a peer that grants credit and never acknowledges what it is sent lets the host keep more than 4 MiB
// on one stream. That stream is closed: its Write fails, close is sent for it, and no more than 4 MiB of its
// bytes leave the host. Another stream on the same session still carries data.
func TestKeptBytesBeyondPerStreamCapCloseTheStream(t *testing.T) {
	var services []string
	tab := newTable(t, resumeCfg(), nil)
	down := newDropLink(t)
	host := NewSession(RoleHost, down, testConfig(), NewBudget(256*mib), acceptHook(t, &services))
	down.start(nil)
	bind(t, tab, host)

	// The app opens stream 1 with a 2 MiB window and then grants 8 MiB more, acknowledging nothing.
	receive(t, host, protocol.Open{Stream: 1, Service: "web", Window: 2 * mib})
	receive(t, host, protocol.Window{Stream: 1, Credit: 8 * mib, Received: 0})
	target := hostTarget(t, host, 1)
	err := returnsWithin(t, "the host's write of 5 MiB", func() error {
		_, err := target.Write(make([]byte, 5*mib))
		return err
	})
	if err == nil {
		t.Fatal("a write kept 5 MiB unacknowledged on one stream, want the stream closed")
	}
	awaitSent(t, down.wire, "close for stream 1", isCloseFor(1))
	if got := dataFor(down.messages(), 1); got > 4*mib {
		t.Errorf("%d bytes of stream 1 left the host, want at most the 4 MiB kept per stream", got)
	}

	receive(t, host, protocol.Open{Stream: 2, Service: "web", Window: 2 * mib})
	target2 := hostTarget(t, host, 2)
	if err := returnsWithin(t, "the host's write on stream 2", func() error {
		_, err := target2.Write(pattern(1 << 10))
		return err
	}); err != nil {
		t.Fatalf("stream 2 after stream 1 closed: %v", err)
	}
}

// Case 5: ten streams each keep 3.5 MiB unacknowledged, which is 35 MiB in all and more than the 32 MiB total
// cap. At least one stream is closed, and the bytes sent before the closes stay within 32 MiB.
func TestKeptBytesBeyondTotalCapCloseStreams(t *testing.T) {
	var services []string
	tab := newTable(t, resumeCfg(), nil)
	down := newDropLink(t)
	host := NewSession(RoleHost, down, testConfig(), NewBudget(256*mib), acceptHook(t, &services))
	down.start(nil)
	bind(t, tab, host)

	const streams = 10
	const each = 3*mib + mib/2
	results := make(chan error, streams)
	for id := uint64(1); id <= streams; id++ {
		receive(t, host, protocol.Open{Stream: id, Service: "web", Window: 2 * mib})
		receive(t, host, protocol.Window{Stream: id, Credit: 4 * mib, Received: 0})
		target := hostTarget(t, host, id)
		go func() {
			_, err := target.Write(make([]byte, each))
			results <- err
		}()
	}
	closed := 0
	for i := 0; i < streams; i++ {
		select {
		case err := <-results:
			if err != nil {
				closed++
			}
		case <-time.After(resumeWait):
			t.Fatal("a write on the host did not finish")
		}
	}
	if closed == 0 {
		t.Error("35 MiB kept unacknowledged over ten streams and no stream was closed, want one closed at the 32 MiB total")
	}
	if got := down.dataBytes(); got > 32*mib {
		t.Errorf("%d bytes kept unacknowledged in all, want at most the 32 MiB total cap", got)
	}
}

// Case 6: without FlagResume, opened carries a zero token, and Detach closes the streams at once: a Read on
// the host's stream and a Write on the app's stream fail without waiting for a reattach.
func TestWithoutResumeOpenedHasZeroTokenAndDetachCloses(t *testing.T) {
	app, host, _, down := pair(t, acceptEverything)
	st, err := openStream(app, "web")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	opened := openeds(down.messages())
	if len(opened) != 1 {
		t.Fatalf("the host answered %d opens, want 1", len(opened))
	}
	if opened[0].Token != ([16]byte{}) {
		t.Error("opened carried a token without resume, want zeros")
	}

	target := hostTarget(t, host, st.ID())
	detach(t, host)
	if err := returnsWithin(t, "Read on the host's stream after Detach", func() error {
		_, err := target.Read(make([]byte, 1))
		return err
	}); err == nil {
		t.Error("the host's stream still reads after Detach without resume, want it closed")
	}

	detach(t, app)
	if err := returnsWithin(t, "Write on the app's stream after Detach", func() error {
		_, err := st.Write([]byte("x"))
		return err
	}); err == nil {
		t.Error("the app's stream still writes after Detach without resume, want it closed")
	}
}

// Case 7: the app's new session holds the stream but has not been reattached yet. Data and window that arrive
// on it before reattached are dropped, so the reader gets only the data that follows reattached. The reattach
// carries the issued token, the bytes the app received (none) and its free window (2 MiB). After reattached,
// the app sends no more than the reattached limit.
func TestAppDropsDataBeforeReattached(t *testing.T) {
	tab := newTable(t, resumeCfg(), nil)
	app1, host1, up1, down1 := resumeConn(t, tab, acceptEverything)
	st, err := openStream(app1, "web")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	token := issuedToken(t, down1, st.ID())
	dropConn(t, app1, host1, up1, down1)

	// The new app session's link is not connected to a host, so the test plays the host.
	up2 := newDropLink(t)
	up2.start(nil)
	app2 := NewSession(RoleApp, up2, testConfig(), NewBudget(64*mib), nil)
	enableResume(t, app2)
	adopt(t, app2, app1)
	awaitSent(t, up2.wire, "reattach for the stream", isReattachFor(st.ID()))
	for _, m := range up2.messages() {
		r, ok := m.(protocol.Reattach)
		if !ok || r.Stream != st.ID() {
			continue
		}
		if r.Token != token {
			t.Error("reattach does not carry the issued token")
		}
		if r.Received != 0 || r.Limit != 2*mib {
			t.Errorf("reattach has received %d and limit %d, want 0 and 2 MiB", r.Received, r.Limit)
		}
	}

	// Stale data and a stale window arrive from the old session before the host's reattached.
	receive(t, app2, protocol.Data{Stream: st.ID(), Payload: []byte("stale")})
	receive(t, app2, protocol.Window{Stream: st.ID(), Credit: mib, Received: 0})
	receive(t, app2, protocol.Reattached{Stream: st.ID(), Received: 0, Limit: 2 * mib})
	receive(t, app2, protocol.Data{Stream: st.ID(), Payload: []byte("fresh")})

	got := make([]byte, len("fresh"))
	if err := returnsWithin(t, "the app's read after reattached", func() error {
		_, err := io.ReadFull(st, got)
		return err
	}); err != nil {
		t.Fatalf("read after reattached: %v", err)
	}
	if string(got) != "fresh" {
		t.Errorf("the reader got %q, want only the data sent after reattached", got)
	}

	// The stale window granted 1 MiB, but the reattached limit is 2 MiB: a 3 MiB write sends 2 MiB and blocks.
	go func() {
		_, _ = st.Write(make([]byte, 3*mib))
	}()
	deadline := time.Now().Add(settle)
	for up2.dataBytes() < 2*mib && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if got := up2.dataBytes(); got != 2*mib {
		t.Errorf("the app sent %d bytes after reattached, want the reattached limit of 2 MiB", got)
	}
}
