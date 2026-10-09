package mux

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/internal/protocol"
)

const mib = 1 << 20

// testConfig is the design's default: a 2 MiB window and 128 streams. Limits and Clock stay unset,
// because these tests use neither the shared counter nor a fake clock.
func testConfig() Config {
	return Config{Window: 2 * mib, MaxStreams: 128}
}

// acceptHook is the host's accept callback. It records the service names it is called with and
// accepts every stream without a target, so the host's side of the stream is read with hostTarget.
func acceptHook(t *testing.T, services *[]string) func(string) AcceptResult {
	return func(service string) AcceptResult {
		*services = append(*services, service)
		return AcceptResult{}
	}
}

// hostTarget returns the host's side of stream id: the bytes the host forwards to the service's
// target, read and written as the service would. It fails the test if host has no such stream.
func hostTarget(t *testing.T, host *Session, id uint64) *Stream {
	t.Helper()
	host.mu.Lock()
	st := host.streams[id]
	host.mu.Unlock()
	if st == nil {
		t.Fatalf("the host has no stream %d", id)
	}
	return st
}

// wire is a Sender for one direction. It records each message, counts the data payload bytes and
// queues the message for deliver.
type wire struct {
	ch    chan any
	mu    sync.Mutex
	msgs  []any
	bytes uint64
}

func newWire() *wire {
	return &wire{ch: make(chan any, 4096)}
}

func (w *wire) Send(msg any) error {
	w.mu.Lock()
	w.msgs = append(w.msgs, msg)
	if d, ok := msg.(protocol.Data); ok {
		w.bytes += uint64(len(d.Payload))
	}
	w.mu.Unlock()
	w.ch <- msg
	return nil
}

// messages returns the messages sent so far, in order.
func (w *wire) messages() []any {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]any(nil), w.msgs...)
}

// dataBytes returns the data payload bytes sent so far.
func (w *wire) dataBytes() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.bytes
}

// deliver hands the messages sent on w to dst, in order, on a goroutine of its own, so that a Send
// never waits for a Receive.
func (w *wire) deliver(t *testing.T, dst *Session) {
	go func() {
		for msg := range w.ch {
			if err := dst.Receive(msg); err != nil {
				t.Errorf("Receive(%T): %v", msg, err)
			}
		}
	}()
}

// pair returns an app session and a host session. What the app sends reaches the host, and what the
// host sends reaches the app, in order; up and down record those messages. The host accepts services
// with accept.
func pair(t *testing.T, accept func(string) AcceptResult) (app, host *Session, up, down *wire) {
	t.Helper()
	up, down = newWire(), newWire()
	app = NewSession(RoleApp, up, testConfig(), NewBudget(64*mib), nil)
	host = NewSession(RoleHost, down, testConfig(), NewBudget(256*mib), accept)
	up.deliver(t, host)
	down.deliver(t, app)
	return app, host, up, down
}

// pattern returns n bytes that differ from their neighbours, so a dropped or repeated byte shows.
func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

// openeds returns the opened messages among msgs.
func openeds(msgs []any) []protocol.Opened {
	var out []protocol.Opened
	for _, m := range msgs {
		if o, ok := m.(protocol.Opened); ok {
			out = append(out, o)
		}
	}
	return out
}

// windows returns the window messages among msgs.
func windows(msgs []any) []protocol.Window {
	var out []protocol.Window
	for _, m := range msgs {
		if w, ok := m.(protocol.Window); ok {
			out = append(out, w)
		}
	}
	return out
}

// Case 1: the app's Open sends open with a 2 MiB window. The host's accept runs with the service name,
// and the host answers opened with a 2 MiB grant.
func TestOpenSendsWindowAndHostAccepts(t *testing.T) {
	var services []string
	app, _, up, down := pair(t, acceptHook(t, &services))
	st, err := app.Open(context.Background(), "web")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	msgs := up.messages()
	if len(msgs) != 1 {
		t.Fatalf("app sent %d messages, want one open", len(msgs))
	}
	got, ok := msgs[0].(protocol.Open)
	if !ok {
		t.Fatalf("app sent %T, want protocol.Open", msgs[0])
	}
	if want := (protocol.Open{Stream: st.ID(), Service: "web", Window: 2 * mib}); got != want {
		t.Errorf("open = %+v, want %+v", got, want)
	}
	if len(services) != 1 || services[0] != "web" {
		t.Errorf("accept ran with %q, want [web]", services)
	}
	opened := openeds(down.messages())
	if len(opened) != 1 || opened[0].Stream != st.ID() || opened[0].Window != 2*mib {
		t.Errorf("host answers = %+v, want one opened for stream %d with window %d", opened, st.ID(), 2*mib)
	}
}

// Case 2: 10 MB written on the app's side arrives intact at the host's side.
func TestTenMegabytesArriveIntact(t *testing.T) {
	var services []string
	app, host, _, _ := pair(t, acceptHook(t, &services))
	st, err := app.Open(context.Background(), "web")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	target := hostTarget(t, host, st.ID())
	want := pattern(10 * mib)
	werr := make(chan error, 1)
	go func() {
		_, err := st.Write(want)
		werr <- err
	}()
	got := make([]byte, len(want))
	if _, err := io.ReadFull(target, got); err != nil {
		t.Fatalf("read at the host: %v", err)
	}
	if err := <-werr; err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("10 MB arrived altered")
	}
}

// Case 3: with the host not reading, the app's Write blocks after exactly the 2 MiB the host granted.
func TestWriterBlocksAfterTheGrantedWindow(t *testing.T) {
	var services []string
	app, _, up, _ := pair(t, acceptHook(t, &services))
	st, err := app.Open(context.Background(), "web")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := st.Write(make([]byte, 4*mib))
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("Write returned with %d bytes sent, want it to block: %v", up.dataBytes(), err)
	default:
	}
	if got := up.dataBytes(); got != 2*mib {
		t.Fatalf("sent %d data bytes before blocking, want the granted 2 MiB", got)
	}
}

// Case 4: the host's reader takes 64 KiB, so the host sends a window message with credit and its
// received total, and the app's Write goes on.
func TestReadingFreesCredit(t *testing.T) {
	var services []string
	app, host, up, down := pair(t, acceptHook(t, &services))
	st, err := app.Open(context.Background(), "web")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	target := hostTarget(t, host, st.ID())
	go func() {
		_, _ = st.Write(make([]byte, 4*mib))
	}()
	time.Sleep(200 * time.Millisecond) // the writer is blocked at 2 MiB
	if _, err := io.ReadFull(target, make([]byte, 64<<10)); err != nil {
		t.Fatalf("read at the host: %v", err)
	}
	ws := windows(down.messages())
	if len(ws) == 0 || ws[0].Stream != st.ID() || ws[0].Credit == 0 || ws[0].Received != 2*mib {
		t.Fatalf("after reading 64 KiB the host sent windows %+v, want one with credit above 0 and received 2 MiB", ws)
	}
	deadline := time.Now().Add(5 * time.Second)
	for up.dataBytes() <= 2*mib && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := up.dataBytes(); got <= 2*mib {
		t.Fatalf("writer did not continue after the grant: %d data bytes sent", got)
	}
}

// Case 5: a peer that sends past its credit has that stream reset with an error. The session stays
// up, and another stream on it carries on.
func TestOverrunResetsOnlyThatStream(t *testing.T) {
	var services []string
	host := NewSession(RoleHost, newWire(), testConfig(), NewBudget(256*mib), acceptHook(t, &services))
	for id := uint64(1); id <= 2; id++ {
		if err := host.Receive(protocol.Open{Stream: id, Service: "web", Window: 2 * mib}); err != nil {
			t.Fatalf("Receive(open %d): %v", id, err)
		}
	}
	// Stream 1 gets exactly its 2 MiB of credit, then one byte more.
	chunk := make([]byte, 64<<10)
	for sent := 0; sent < 2*mib; sent += len(chunk) {
		if err := host.Receive(protocol.Data{Stream: 1, Payload: chunk}); err != nil {
			t.Fatalf("Receive(data on stream 1): %v", err)
		}
	}
	if err := host.Receive(protocol.Data{Stream: 1, Payload: []byte{1}}); err != nil {
		t.Fatalf("an overrun closed the session, want only stream 1 reset: %v", err)
	}
	// The reset reaches stream 1's target as an error, not as a clean end. Bytes received before the
	// overrun may be read first.
	if _, err := io.Copy(io.Discard, hostTarget(t, host, 1)); err == nil {
		t.Fatal("stream 1 ended cleanly, want a reset error")
	}
	// Stream 2 carries on.
	want := pattern(1 << 10)
	if err := host.Receive(protocol.Data{Stream: 2, Payload: want}); err != nil {
		t.Fatalf("Receive(data on stream 2): %v", err)
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(hostTarget(t, host, 2), got); err != nil {
		t.Fatalf("read stream 2 at the host: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("stream 2 arrived altered")
	}
}

// Case 6: with a 4 MiB budget and four open streams, no grant made once the fourth is open exceeds
// the 1 MiB fair share. The grants so far total at most the budget. No window is sent without a read,
// and a read brings a new grant. The docs do not fix when the budget counts as spent, so the test
// checks only what they fix.
func TestBudgetCapsGrantsAndFollowsDraining(t *testing.T) {
	var services []string
	down := newWire()
	host := NewSession(RoleHost, down, testConfig(), NewBudget(4*mib), acceptHook(t, &services))
	for id := uint64(1); id <= 4; id++ {
		if err := host.Receive(protocol.Open{Stream: id, Service: "web", Window: 2 * mib}); err != nil {
			t.Fatalf("Receive(open %d): %v", id, err)
		}
	}
	opened := openeds(down.messages())
	if len(opened) != 4 {
		t.Fatalf("host answered %d opens, want 4", len(opened))
	}
	if opened[3].Window > mib {
		t.Errorf("grant to the fourth stream is %d, want at most the 1 MiB share of 4 MiB", opened[3].Window)
	}
	var granted uint64
	for _, o := range opened {
		granted += o.Window
	}
	if granted > 4*mib {
		t.Errorf("grants total %d, more than the 4 MiB budget", granted)
	}
	if ws := windows(down.messages()); len(ws) != 0 {
		t.Fatalf("host sent %d window messages before any stream was drained", len(ws))
	}
	// Draining stream 1: the app sends 64 KiB, the host's target reads it, and the host grants again.
	if err := host.Receive(protocol.Data{Stream: 1, Payload: make([]byte, 64<<10)}); err != nil {
		t.Fatalf("Receive(data on stream 1): %v", err)
	}
	if _, err := io.ReadFull(hostTarget(t, host, 1), make([]byte, 64<<10)); err != nil {
		t.Fatalf("read at the host: %v", err)
	}
	ws := windows(down.messages())
	if len(ws) == 0 || ws[0].Stream != 1 || ws[0].Credit == 0 || ws[0].Credit > mib {
		t.Errorf("after the read the host sent windows %+v, want one for stream 1 with credit from 1 to 1 MiB", ws)
	}
}

// Case 7: a 200 KiB write is sent as data messages of at most 65536 bytes, which together carry all
// of it. The 2 MiB window needs no read, so the write completes.
func TestWriteIsSplitIntoChunks(t *testing.T) {
	var services []string
	app, _, up, _ := pair(t, acceptHook(t, &services))
	st, err := app.Open(context.Background(), "web")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	payload := pattern(200 << 10)
	if _, err := st.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	var total int
	for _, m := range up.messages() {
		d, ok := m.(protocol.Data)
		if !ok {
			continue
		}
		if n := len(d.Payload); n > 64<<10 {
			t.Errorf("data message of %d bytes, want at most 65536", n)
		}
		total += len(d.Payload)
	}
	if total != len(payload) {
		t.Errorf("data messages carry %d bytes, want %d", total, len(payload))
	}
}

// A stream admitted with no credit is granted once another stream frees the budget (HoleBridge-eon.8). The host
// budget holds 2 MiB: the first stream takes all of it, so the second is admitted with 0. Closing the first on
// both sides frees its credit, and the second is granted its window, so its write goes through.
func TestStarvedStreamIsGrantedWhenCreditFrees(t *testing.T) {
	up, down := newWire(), newWire()
	var services []string
	app := NewSession(RoleApp, up, testConfig(), NewBudget(64*mib), nil)
	host := NewSession(RoleHost, down, testConfig(), NewBudget(2*mib), acceptHook(t, &services))
	up.deliver(t, host)
	down.deliver(t, app)
	first, err := openStream(app, "web")
	if err != nil {
		t.Fatalf("Open first stream: %v", err)
	}
	second, err := openStream(app, "web")
	if err != nil {
		t.Fatalf("Open second stream: %v", err)
	}
	if opened := openeds(down.messages()); len(opened) != 2 || opened[1].Window != 0 {
		t.Fatalf("the second stream was admitted with window %v, want 0 with the budget spent", opened)
	}
	hostFirst := hostTarget(t, host, first.ID())
	target := hostTarget(t, host, second.ID())
	written := make(chan error, 1)
	go func() {
		_, werr := second.Write([]byte("hi"))
		written <- werr
	}()
	if err := first.Close(); err != nil {
		t.Fatalf("app closing the first stream: %v", err)
	}
	if err := hostFirst.Close(); err != nil {
		t.Fatalf("host closing the first stream: %v", err)
	}
	awaitSent(t, down, "the grant to the second stream", func(m any) bool {
		w, ok := m.(protocol.Window)
		return ok && w.Stream == second.ID() && w.Credit > 0
	})
	select {
	case err := <-written:
		if err != nil {
			t.Fatalf("Write on the second stream: %v", err)
		}
	case <-time.After(settle):
		t.Fatalf("Write on the second stream did not finish within %v after the first closed", settle)
	}
	got := make([]byte, 2)
	if err := returnsWithin(t, "the host reading the second stream", func() error {
		_, rerr := io.ReadFull(target, got)
		return rerr
	}); err != nil || string(got) != "hi" {
		t.Fatalf("host read %q (error %v) on the second stream, want %q", got, err, "hi")
	}
}
