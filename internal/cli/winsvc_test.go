package cli

import (
	"context"
	"errors"
	"testing"
	"time"
)

// serviceTimeout bounds each wait in the service tests, so a stub that never returns fails the test instead of
// hanging the run.
const serviceTimeout = 5 * time.Second

// failOnPanic is deferred by a test that calls a stub. It fails the test with the panic value, instead of ending the
// whole test binary.
func failOnPanic(t *testing.T) {
	if p := recover(); p != nil {
		t.Fatalf("%v", p)
	}
}

// TestServiceHandlerStopsHostOnStop drives the handler core with a fake change request channel. The Service Control
// Manager asks for a stop: the host's context must be cancelled, and serveHost must return a clean exit once the host
// has stopped.
func TestServiceHandlerStopsHostOnStop(t *testing.T) {
	hostStopped := make(chan struct{})
	run := func(ctx context.Context) error {
		<-ctx.Done()
		close(hostStopped)
		return nil
	}

	// The fake Service Control Manager. requests is buffered so that the send below does not wait for serveHost.
	requests := make(chan serviceRequest, 1)
	report := func(serviceState) {}
	returned := make(chan struct{})
	var svcSpecificEC bool
	var exitCode uint32
	go func() {
		defer close(returned)
		defer func() {
			if p := recover(); p != nil {
				t.Errorf("serveHost panicked: %v", p)
			}
		}()
		svcSpecificEC, exitCode = serveHost(run, requests, report)
	}()

	requests <- requestStop
	select {
	case <-hostStopped:
	case <-time.After(serviceTimeout):
		t.Fatal("the host was not stopped after a Stop request")
	}
	select {
	case <-returned:
	case <-time.After(serviceTimeout):
		t.Fatal("serveHost did not return after the host stopped")
	}
	if svcSpecificEC || exitCode != 0 {
		t.Errorf("serveHost returned svcSpecificEC=%v, exitCode=%d; want false and 0", svcSpecificEC, exitCode)
	}
}

// serveResult is what serveHost returned, or the value it panicked with.
type serveResult struct {
	svcSpecificEC bool
	exitCode      uint32
	panicked      any
}

// serveRun is one serveHost call under test, with the fake Service Control Manager around it.
type serveRun struct {
	t        *testing.T
	requests chan serviceRequest // buffered, so a send never waits for serveHost
	reports  chan serviceState   // buffered, so serveHost's report calls never wait for the test
	done     chan struct{}       // closed when serveHost has returned or panicked
	result   serveResult         // valid once done is closed
}

// startServe runs serveHost(run, ...) in a goroutine.
func startServe(t *testing.T, run func(ctx context.Context) error) *serveRun {
	s := &serveRun{
		t:        t,
		requests: make(chan serviceRequest, 4),
		reports:  make(chan serviceState, 16),
		done:     make(chan struct{}),
	}
	go func() {
		defer close(s.done)
		defer func() {
			if p := recover(); p != nil {
				s.result = serveResult{panicked: p}
			}
		}()
		s.result.svcSpecificEC, s.result.exitCode = serveHost(run, s.requests, func(st serviceState) { s.reports <- st })
	}()
	return s
}

// state returns the next state serveHost reported. It fails the test when serveHost panicked, returned without
// reporting one, or reported none within serviceTimeout.
func (s *serveRun) state() serviceState {
	s.t.Helper()
	select {
	case st := <-s.reports:
		return st
	case <-s.done:
		select {
		case st := <-s.reports: // reported just before it returned
			return st
		default:
		}
		if s.result.panicked != nil {
			s.t.Fatalf("serveHost panicked: %v", s.result.panicked)
		}
		s.t.Fatal("serveHost returned before it reported another state")
	case <-time.After(serviceTimeout):
		s.t.Fatal("serveHost reported no state within the timeout")
	}
	return 0
}

// wait returns what serveHost returned. It fails the test when serveHost panicked or does not return within
// serviceTimeout.
func (s *serveRun) wait() serveResult {
	s.t.Helper()
	select {
	case <-s.done:
	case <-time.After(serviceTimeout):
		s.t.Fatal("serveHost did not return within the timeout")
	}
	if s.result.panicked != nil {
		s.t.Fatalf("serveHost panicked: %v", s.result.panicked)
	}
	return s.result
}

// blockingHost is a host that runs until its context ends, then stops cleanly.
func blockingHost(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// TestServiceReportsStatesAroundStop checks the states serveHost reports to the Service Control Manager: StartPending,
// then Running, then StopPending once a Stop or a Shutdown request arrives, and nothing after StopPending. Shutdown
// stops the host like Stop does.
func TestServiceReportsStatesAroundStop(t *testing.T) {
	for _, req := range []serviceRequest{requestStop, requestShutdown} {
		t.Run(map[serviceRequest]string{requestStop: "stop", requestShutdown: "shutdown"}[req], func(t *testing.T) {
			s := startServe(t, blockingHost)
			if st := s.state(); st != stateStartPending {
				t.Fatalf("first state = %v; want stateStartPending (%v)", st, stateStartPending)
			}
			if st := s.state(); st != stateRunning {
				t.Fatalf("second state = %v; want stateRunning (%v)", st, stateRunning)
			}
			s.requests <- req
			if st := s.state(); st != stateStopPending {
				t.Fatalf("state after the request = %v; want stateStopPending (%v)", st, stateStopPending)
			}
			if r := s.wait(); r.svcSpecificEC || r.exitCode != 0 {
				t.Errorf("serveHost returned svcSpecificEC=%v, exitCode=%d; want false and 0", r.svcSpecificEC, r.exitCode)
			}
			select {
			case st := <-s.reports:
				t.Errorf("serveHost reported %v after stateStopPending; want nothing", st)
			default:
			}
		})
	}
}

// TestServiceInterrogateReportsCurrentState checks that an Interrogate request is answered with the current state,
// here stateRunning, and does not stop the host.
func TestServiceInterrogateReportsCurrentState(t *testing.T) {
	s := startServe(t, blockingHost)
	if st := s.state(); st != stateStartPending {
		t.Fatalf("first state = %v; want stateStartPending (%v)", st, stateStartPending)
	}
	if st := s.state(); st != stateRunning {
		t.Fatalf("second state = %v; want stateRunning (%v)", st, stateRunning)
	}
	s.requests <- requestInterrogate
	if st := s.state(); st != stateRunning {
		t.Fatalf("state after Interrogate = %v; want stateRunning (%v)", st, stateRunning)
	}
	select {
	case <-s.done:
		t.Fatal("serveHost returned after an Interrogate request; want it to keep the host running")
	case <-time.After(100 * time.Millisecond):
	}
	s.requests <- requestStop
	if st := s.state(); st != stateStopPending {
		t.Fatalf("state after Stop = %v; want stateStopPending (%v)", st, stateStopPending)
	}
	s.wait()
}

// TestServiceHostFailsOnItsOwn checks the exit codes for a host that ends without a request: one that fails to start
// (a bad host.json, a port in use) must end the service with a failure code, and not leave it running with no host.
// A failure code is service specific (svcSpecificEC true), since it is not a Win32 error code.
func TestServiceHostFailsOnItsOwn(t *testing.T) {
	s := startServe(t, func(ctx context.Context) error { return errors.New("listen failed") })
	r := s.wait() // no request is sent: serveHost must return once the host has
	if !r.svcSpecificEC || r.exitCode == 0 {
		t.Errorf("serveHost returned svcSpecificEC=%v, exitCode=%d; want true and a non-zero code", r.svcSpecificEC, r.exitCode)
	}
}

// TestServiceHostEndsOnItsOwn checks that a host that returns nil without a request still ends the service, with a
// clean exit.
func TestServiceHostEndsOnItsOwn(t *testing.T) {
	s := startServe(t, func(ctx context.Context) error { return nil })
	r := s.wait()
	if r.svcSpecificEC || r.exitCode != 0 {
		t.Errorf("serveHost returned svcSpecificEC=%v, exitCode=%d; want false and 0", r.svcSpecificEC, r.exitCode)
	}
}

// TestServiceHostFailsWhileStopping checks that a host that fails while stopping makes the service end with a failure
// code, like a host that fails on its own.
func TestServiceHostFailsWhileStopping(t *testing.T) {
	s := startServe(t, func(ctx context.Context) error {
		<-ctx.Done()
		return errors.New("close failed")
	})
	s.state() // StartPending
	s.state() // Running
	s.requests <- requestStop
	r := s.wait()
	if !r.svcSpecificEC || r.exitCode == 0 {
		t.Errorf("serveHost returned svcSpecificEC=%v, exitCode=%d; want true and a non-zero code", r.svcSpecificEC, r.exitCode)
	}
}

// TestServiceInstallCommandLine checks the command line that service-install registers: the executable, then
// --config, the config directory and host. A path with a space is quoted, so the Service Control Manager does not
// split it.
func TestServiceInstallCommandLine(t *testing.T) {
	cases := []struct {
		name, exe, configDir, want string
	}{
		{
			name:      "paths without spaces",
			exe:       `C:\holebridge\holebridge.exe`,
			configDir: `C:\holebridge\config`,
			want:      `C:\holebridge\holebridge.exe --config C:\holebridge\config host`,
		},
		{
			name:      "paths with spaces are quoted",
			exe:       `C:\Program Files\HoleBridge\holebridge.exe`,
			configDir: `C:\Users\Andrew Loable\AppData\Roaming\holebridge`,
			want:      `"C:\Program Files\HoleBridge\holebridge.exe" --config "C:\Users\Andrew Loable\AppData\Roaming\holebridge" host`,
		},
		{
			name:      "only the executable has a space",
			exe:       `C:\Program Files\HoleBridge\holebridge.exe`,
			configDir: `C:\holebridge\config`,
			want:      `"C:\Program Files\HoleBridge\holebridge.exe" --config C:\holebridge\config host`,
		},
		{
			name:      "only the config directory has a space",
			exe:       `C:\holebridge\holebridge.exe`,
			configDir: `C:\Users\Andrew Loable\AppData\Roaming\holebridge`,
			want:      `C:\holebridge\holebridge.exe --config "C:\Users\Andrew Loable\AppData\Roaming\holebridge" host`,
		},
		{
			// A backslash before the closing quote would escape it, so the backslash is doubled.
			name:      "trailing backslash with a space",
			exe:       `C:\holebridge\holebridge.exe`,
			configDir: `C:\Program Data\hb\`,
			want:      `C:\holebridge\holebridge.exe --config "C:\Program Data\hb\\" host`,
		},
		{
			// Without a quote after it, a backslash is literal, and without a space no quotes are needed.
			name:      "trailing backslash without a space",
			exe:       `C:\holebridge\holebridge.exe`,
			configDir: `C:\holebridge\config\`,
			want:      `C:\holebridge\holebridge.exe --config C:\holebridge\config\ host`,
		},
		{
			name:      "quotes in the path",
			exe:       `C:\holebridge\holebridge.exe`,
			configDir: `C:\a "b"\c`,
			want:      `C:\holebridge\holebridge.exe --config "C:\a \"b\"\c" host`,
		},
		{
			// The backslash before the quote is doubled, and the quote is escaped.
			name:      "backslash before a quote",
			exe:       `C:\holebridge\holebridge.exe`,
			configDir: `C:\My Dir\"x`,
			want:      `C:\holebridge\holebridge.exe --config "C:\My Dir\\\"x" host`,
		},
		{
			name:      "empty config directory",
			exe:       `C:\holebridge\holebridge.exe`,
			configDir: ``,
			want:      `C:\holebridge\holebridge.exe --config "" host`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer failOnPanic(t)
			got := serviceCommandLine(c.exe, c.configDir)
			if got != c.want {
				t.Errorf("serviceCommandLine(%q, %q) = %q; want %q", c.exe, c.configDir, got, c.want)
			}
		})
	}
}
