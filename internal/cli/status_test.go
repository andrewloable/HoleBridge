package cli

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/internal/status"
)

// serveWait bounds how long a test waits for the control socket to answer, or for it to stop.
const serveWait = 5 * time.Second

// fakeStatusHandler answers the control socket with a fixed snapshot. Reload does nothing.
type fakeStatusHandler struct {
	snap status.Status
}

func (h fakeStatusHandler) Status() status.Status { return h.snap }
func (h fakeStatusHandler) Reload() error         { return nil }

// shortDir returns a new temp directory with a short path, for the control socket: a Unix socket path
// is limited to about 104 bytes on macOS, which t.TempDir can exceed. It is removed when the test ends.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "hbst")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// serveStatus runs a control socket in dir that answers status requests with snap. It stops when the
// test ends, and returns once the socket answers.
func serveStatus(t *testing.T, dir string, snap status.Status) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- status.Serve(ctx, dir, fakeStatusHandler{snap: snap}) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(serveWait):
			t.Error("the control socket did not stop")
		}
	})
	deadline := time.Now().Add(serveWait)
	for {
		if _, err := status.Query(dir, "status"); err == nil {
			return
		}
		select {
		case err := <-done:
			t.Fatalf("status.Serve returned before the socket answered: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("the control socket did not answer")
		}
	}
}

// lineHas reports whether one line of out holds every word, ignoring case.
func lineHas(out string, words ...string) bool {
	for _, line := range strings.Split(out, "\n") {
		l := strings.ToLower(line)
		ok := true
		for _, w := range words {
			if !strings.Contains(l, strings.ToLower(w)) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// Case 1: against a control socket that answers with two services and two sessions, status lists each
// service with its name and kind, and each session on one line with its route, its streams, its flows
// and its byte counts (as plain decimal numbers). Exit 0.
func TestStatusListsServicesAndSessions(t *testing.T) {
	dir := shortDir(t)
	services := []status.Service{{Name: "web", Kind: "https"}, {Name: "ssh", Kind: "tcp"}}
	sessions := []status.Session{
		{Route: "direct", Streams: 12, Flows: 345, BytesIn: 1234567, BytesOut: 7654321},
		{Route: "relay", Streams: 3, Flows: 4, BytesIn: 890123, BytesOut: 456789},
	}
	serveStatus(t, dir, status.Status{Services: services, Sessions: sessions})

	code, stdout, stderr := run("--config", dir, "status")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	for _, s := range services {
		if !lineHas(stdout, s.Name, s.Kind) {
			t.Errorf("no line lists service %s (%s); stdout:\n%s", s.Name, s.Kind, stdout)
		}
	}
	for _, se := range sessions {
		words := []string{
			se.Route,
			strconv.Itoa(se.Streams),
			strconv.Itoa(se.Flows),
			strconv.FormatUint(se.BytesIn, 10),
			strconv.FormatUint(se.BytesOut, 10),
		}
		if !lineHas(stdout, words...) {
			t.Errorf("no line lists the session on route %s with its streams, flows and bytes; stdout:\n%s", se.Route, stdout)
		}
	}
}

// Case 2: with no control socket answering, status prints HB-NOT-RUNNING and "holebridge is not running" on
// stderr and exits 1. Nothing goes to stdout.
func TestStatusWithNoServerSaysNotRunningAndExitsOne(t *testing.T) {
	dir := shortDir(t)

	code, stdout, stderr := run("--config", dir, "status")
	if code != 1 {
		t.Errorf("exit code = %d, want 1 (stderr %q)", code, stderr)
	}
	if !strings.Contains(stderr, "HB-NOT-RUNNING") {
		t.Errorf("stderr does not carry HB-NOT-RUNNING: %q", stderr)
	}
	if !strings.Contains(stderr, "holebridge is not running") {
		t.Errorf("stderr does not say holebridge is not running: %q", stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
}
