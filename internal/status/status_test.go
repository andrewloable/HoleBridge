package status

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/pears/dhtrpc"
)

// serveWait bounds how long a test waits for the control socket to answer, or for Serve to return.
const serveWait = 5 * time.Second

// fakeHandler is a test Handler. snapshot is what Status returns. key and target stand for state a host holds
// (its key and a service's target address); the handler keeps them, but Status never returns them, and the
// tests check that no answer carries them.
type fakeHandler struct {
	mu       sync.Mutex
	snapshot Status
	key      string
	target   string
	reloads  int
}

func (h *fakeHandler) Status() Status {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.snapshot
}

func (h *fakeHandler) Reload() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reloads++
	return nil
}

func (h *fakeHandler) reloadCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reloads
}

// socketDir returns an empty directory for a control socket. The path is kept short on purpose: a Unix socket
// path is limited to about 104 bytes on macOS, which a long t.TempDir path can exceed.
func socketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "hbs")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// serveAsync runs Serve on dir in the background and returns its result channel. The server stops when the test
// ends.
func serveAsync(t *testing.T, dir string, h Handler) <-chan error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, dir, h) }()
	return done
}

// awaitServing waits until the control socket in dir answers a status request. It fails the test when Serve
// returns first, or when the socket does not answer within serveWait.
func awaitServing(t *testing.T, dir string, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(serveWait)
	for {
		select {
		case err := <-done:
			t.Fatalf("Serve returned before the socket answered: %v", err)
		default:
		}
		if _, err := Query(dir, "status"); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the control socket did not answer within 5 s")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Test case 1: a status query against a test Handler returns the handler's snapshot, with the JSON names of
// docs/architecture.md (services, sessions, nat, relay).
func TestStatusQueryReturnsHandlerSnapshot(t *testing.T) {
	dir := socketDir(t)
	want := Status{
		Services: []Service{{Name: "nas", Kind: "https"}, {Name: "ssh", Kind: "tcp"}},
		Sessions: []Session{{Route: "direct", Streams: 2, Flows: 1, BytesIn: 1234, BytesOut: 5678}},
		NAT:      dhtrpc.NATInfo{Host: "198.51.100.7", Port: 49737},
		Relay:    true,
	}
	h := &fakeHandler{snapshot: want}
	awaitServing(t, dir, serveAsync(t, dir, h))

	raw, err := Query(dir, "status")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("status answer is not a JSON object: %v", err)
	}
	for _, name := range []string{"services", "sessions", "nat", "relay"} {
		if _, ok := fields[name]; !ok {
			t.Errorf("status answer has no %q field", name)
		}
	}
	var got Status
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("status answer does not decode: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("status = %+v, want the handler snapshot %+v", got, want)
	}
}

// Test case 2: the socket file has mode 0600 (POSIX).
func TestSocketFileIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes only; Windows uses a named pipe")
	}
	dir := socketDir(t)
	awaitServing(t, dir, serveAsync(t, dir, &fakeHandler{}))

	fi, err := os.Stat(filepath.Join(dir, socketName))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("socket mode = %o, want 600", got)
	}
}

// Test case 3: a second Serve on the same directory fails while the first one runs, and the first keeps answering.
func TestSecondServeOnSameDirFails(t *testing.T) {
	dir := socketDir(t)
	h := &fakeHandler{}
	awaitServing(t, dir, serveAsync(t, dir, h))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	second := make(chan error, 1)
	go func() { second <- Serve(ctx, dir, h) }()
	select {
	case err := <-second:
		if err == nil {
			t.Fatal("a second Serve on the same directory returned nil, want an error while the first runs")
		}
	case <-time.After(serveWait):
		t.Fatal("a second Serve on the same directory did not fail within 5 s")
	}
	if _, err := Query(dir, "status"); err != nil {
		t.Fatalf("the first server stopped answering: %v", err)
	}
}

// Test case 4: a query with no server in the directory returns the not running error.
func TestQueryWithNoServerIsNotRunning(t *testing.T) {
	dir := socketDir(t)
	_, err := Query(dir, "status")
	if !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Query error = %v, want ErrNotRunning", err)
	}
}

// Test case 5: the JSON never contains the key or a target address. The handler holds both in its state, as a
// host does, and the status and reload answers are checked. The values are test values: a 9-symbol key and its
// application key from spec/vectors, and a loopback target.
func TestAnswersNeverContainKeyOrTargetAddress(t *testing.T) {
	dir := socketDir(t)
	h := &fakeHandler{
		snapshot: Status{
			Services: []Service{{Name: "nas", Kind: "https"}},
			Sessions: []Session{{Route: "direct", Streams: 1}},
		},
		key:    "7KQ-M4X-9TR",
		target: "127.0.0.1:8096",
	}
	secrets := []string{
		h.key,
		"7KQM4X9TR",
		"000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f",
		h.target,
	}
	awaitServing(t, dir, serveAsync(t, dir, h))

	for _, cmd := range []string{"status", "reload"} {
		raw, err := Query(dir, cmd)
		if err != nil {
			t.Fatalf("Query %s: %v", cmd, err)
		}
		for _, s := range secrets {
			if strings.Contains(string(raw), s) {
				// The value is not printed: a failing test must not leak key material.
				t.Errorf("the %s answer contains a key or a target address", cmd)
			}
		}
	}
}

// Not one of the listed cases: the reload request runs the handler's Reload once and answers ok. The design gives
// reload its own answer, so it has a test beside the status cases.
func TestReloadRunsHandlerAndAnswersOK(t *testing.T) {
	dir := socketDir(t)
	h := &fakeHandler{}
	awaitServing(t, dir, serveAsync(t, dir, h))

	raw, err := Query(dir, "reload")
	if err != nil {
		t.Fatalf("Query reload: %v", err)
	}
	var ans struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(raw, &ans); err != nil || !ans.OK {
		t.Fatalf("reload answer = %s, want an ok answer", raw)
	}
	if n := h.reloadCount(); n != 1 {
		t.Fatalf("the handler's Reload ran %d times, want 1", n)
	}
}

// Edge case (not a listed case): a socket file left by a server that died, with no listener behind it, is
// replaced, so Serve starts and answers.
func TestServeReplacesStaleSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a named pipe leaves no file behind")
	}
	dir := socketDir(t)
	ln, err := net.Listen("unix", filepath.Join(dir, socketName))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	awaitServing(t, dir, serveAsync(t, dir, &fakeHandler{}))
}

// Edge case (not a listed case): a request the server does not know gets an error answer, and the server keeps
// answering.
func TestUnknownCommandAnswersError(t *testing.T) {
	dir := socketDir(t)
	awaitServing(t, dir, serveAsync(t, dir, &fakeHandler{}))

	raw, err := Query(dir, "bogus")
	if err != nil {
		t.Fatalf("Query bogus: %v", err)
	}
	var ans struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &ans); err != nil || ans.Error == "" {
		t.Fatalf("bogus answer = %s, want an error answer", raw)
	}
	if _, err := Query(dir, "status"); err != nil {
		t.Fatalf("the server stopped answering after an unknown command: %v", err)
	}
}
