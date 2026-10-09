package status

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"
)

// readToEnd reads conn until the server closes it. It fails the test when the end does not come within wait. The
// read runs on its own goroutine, so the wait holds on a named pipe too, whose reads take no deadline.
func readToEnd(t *testing.T, conn io.Reader, wait time.Duration) ([]byte, error) {
	t.Helper()
	type result struct {
		b   []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		b, err := io.ReadAll(conn)
		ch <- result{b, err}
	}()
	select {
	case r := <-ch:
		return r.b, r.err
	case <-time.After(wait):
		t.Fatalf("the connection was still open after %v", wait)
		return nil, nil
	}
}

// serveWith runs serve in the background with the deadlines d. It returns the result channel and the function that
// stops the server.
func serveWith(t *testing.T, dir string, h Handler, d deadlines) (<-chan error, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- serve(ctx, dir, h, d) }()
	return done, cancel
}

// awaitStopped waits for Serve to return after its context was cancelled. It fails the test when Serve returns an
// error or does not return within serveWait.
func awaitStopped(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v after cancel, want nil", err)
		}
	case <-time.After(serveWait):
		t.Fatal("Serve did not return within 5 s of cancel")
	}
}

// A client that connects and sends nothing is dropped once the request deadline passes, so an idle client holds no
// goroutine for ever.
func TestIdleClientIsDroppedAfterRequestDeadline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("named pipes take no deadline; see deadliner")
	}
	dir := socketDir(t)
	done, _ := serveWith(t, dir, &fakeHandler{}, deadlines{request: 200 * time.Millisecond, answer: 5 * time.Second})
	awaitServing(t, dir, done)

	conn, err := dial(dir)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := readToEnd(t, conn, 3*time.Second); err != nil {
		t.Fatalf("read after the request deadline: %v", err)
	}
}

// A client that stops taking its answer is dropped once the answer deadline passes. The answer is larger than the
// socket buffers, and the client waits longer than the deadline before it reads. The server must then have given up:
// the client gets less than the whole answer.
func TestAnswerWriteIsBounded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("named pipes take no deadline; see deadliner")
	}
	dir := socketDir(t)
	snap := Status{}
	for i := 0; i < 25000; i++ {
		snap.Services = append(snap.Services, Service{Name: "svc", Kind: "tcp"})
	}
	full, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	done, _ := serveWith(t, dir, &fakeHandler{snapshot: snap}, deadlines{request: 5 * time.Second, answer: 200 * time.Millisecond})
	awaitServing(t, dir, done)

	conn, err := dial(dir)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(`{"cmd":"status"}` + "\n")); err != nil {
		t.Fatalf("write request: %v", err)
	}
	time.Sleep(600 * time.Millisecond)
	got, err := readToEnd(t, conn, 5*time.Second)
	if err != nil {
		t.Fatalf("read answer: %v", err)
	}
	if len(got) >= len(full) {
		t.Fatalf("the client received %d bytes, the whole answer, after the answer deadline passed", len(got))
	}
}

// Serve closes the connections it accepted when it stops, so a client that was idle before the stop sees the end
// of its connection, and Serve returns nil.
func TestServeClosesLiveConnectionsWhenItStops(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("named pipes take no deadline; see deadliner")
	}
	dir := socketDir(t)
	done, cancel := serveWith(t, dir, &fakeHandler{}, deadlines{request: 10 * time.Second, answer: 5 * time.Second})
	awaitServing(t, dir, done)

	conn, err := dial(dir)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	// The server accepts in order, so this answer means the idle connection was accepted and tracked.
	if _, err := Query(dir, "status"); err != nil {
		t.Fatalf("Query: %v", err)
	}
	cancel()
	awaitStopped(t, done)
	if _, err := readToEnd(t, conn, 3*time.Second); err != nil {
		t.Fatalf("read after Serve returned: %v", err)
	}
}

// A request on a connection that was open when Serve stopped is not run: the handler's Reload is not called after
// Serve has returned, and the client gets no ok answer.
func TestRequestAfterStopIsNotRun(t *testing.T) {
	dir := socketDir(t)
	h := &fakeHandler{}
	done, cancel := serveWith(t, dir, h, deadlines{request: 10 * time.Second, answer: 5 * time.Second})
	awaitServing(t, dir, done)

	conn, err := dial(dir)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := Query(dir, "status"); err != nil {
		t.Fatalf("Query: %v", err)
	}
	cancel()
	awaitStopped(t, done)

	_, _ = conn.Write([]byte(`{"cmd":"reload"}` + "\n"))
	// The read may fail with a reset, which is also an answer that is not ok. Only an ok answer is a failure.
	got, _ := readToEnd(t, conn, 3*time.Second)
	if strings.Contains(string(got), `"ok":true`) {
		t.Errorf("a reload on a connection open at the stop got an ok answer")
	}
	if n := h.reloadCount(); n != 0 {
		t.Fatalf("the handler's Reload ran %d times after Serve returned, want 0", n)
	}
}

// Query gives up when the host accepts the request but never answers, and returns ErrTimeout.
func TestQueryTimesOutOnSilentHost(t *testing.T) {
	dir := socketDir(t)
	ln, err := listen(dir)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			if _, err := ln.Accept(); err != nil {
				return
			}
		}
	}()

	start := time.Now()
	_, err = query(dir, "status", 200*time.Millisecond)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("query error = %v, want ErrTimeout", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("query took %v, want about 200 ms", d)
	}
}

// A request read just before the stop, and answered after it, runs nothing: the gate in answer refuses it once
// shutdown has begun. Closing the connection hides this race from a client, so the gate is tested directly.
func TestAnswerAfterShutdownRunsNothing(t *testing.T) {
	h := &fakeHandler{}
	s := &server{h: h, conns: map[io.ReadWriteCloser]struct{}{}}
	s.shutdown()
	for _, cmd := range []string{"reload", "status"} {
		if _, ok := s.answer([]byte(`{"cmd":"` + cmd + `"}` + "\n")); ok {
			t.Errorf("a %s request ran after shutdown", cmd)
		}
	}
	if n := h.reloadCount(); n != 0 {
		t.Fatalf("the handler's Reload ran %d times after shutdown, want 0", n)
	}
}
