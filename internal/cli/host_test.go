package cli

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/internal/config"
	"github.com/andrewloable/HoleBridge/internal/keys"
	"github.com/andrewloable/HoleBridge/internal/status"
	"github.com/andrewloable/HoleBridge/pears/hyperdht"
)

// hostDone is the result of a holebridge host run that the test started in the background.
type hostDone struct {
	code           int
	stdout, stderr string
}

// startHost runs holebridge host on the config directory dir in a goroutine. It returns the channel that receives
// the result when the host has exited. The host ends when the returned cancel is called, or when the test ends.
func startHost(t *testing.T, dir string) (<-chan hostDone, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	prev := hostContext
	hostContext = func() (context.Context, context.CancelFunc) { return ctx, cancel }
	t.Cleanup(func() {
		cancel()
		hostContext = prev
	})
	done := make(chan hostDone, 1)
	go func() {
		code, stdout, stderr := run("--config", dir, "host")
		done <- hostDone{code, stdout, stderr}
	}()
	return done, cancel
}

// waitForControl polls the control socket of dir until it answers a status request, and returns the answer. It fails
// the test when the host exits first or when no answer comes in time.
func waitForControl(t *testing.T, dir string, done <-chan hostDone) status.Status {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		select {
		case d := <-done:
			t.Fatalf("host exited before it answered: exit code %d, stderr %q", d.code, d.stderr)
		default:
		}
		raw, err := status.Query(dir, "status")
		if err == nil {
			var st status.Status
			if err := json.Unmarshal(raw, &st); err != nil {
				t.Fatalf("status answer is not JSON: %v", err)
			}
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("no status answer from the control socket: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// hasService reports whether st lists a service with this name and kind.
func hasService(st status.Status, name, kind string) bool {
	for _, s := range st.Services {
		if s.Name == name && s.Kind == kind {
			return true
		}
	}
	return false
}

// Case 1: a host started by the command serves the control socket, holds host.lock while it runs, and reloads when
// service add asks it over the socket: the add prints that the change was applied, and the next status answer lists
// the new service. When the host is stopped it removes host.lock and the socket, and prints no key.
func TestHostServesControlSocketAndReloadsOnServiceAdd(t *testing.T) {
	tn := hyperdht.NewTestnet(t, testnetSize)
	useTestnet(t, tn)
	dir := shortDir(t)

	done, stop := startHost(t, dir)
	st := waitForControl(t, dir, done)
	if len(st.Services) != 0 {
		t.Errorf("a host with no services lists %d services, want 0", len(st.Services))
	}
	if _, err := os.Stat(filepath.Join(dir, "host.lock")); err != nil {
		t.Errorf("host.lock does not exist while the host runs: %v", err)
	}

	code, stdout, stderr := run("--config", dir, "service", "add", "web", "127.0.0.1:8080")
	if code != 0 {
		t.Fatalf("service add exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if !strings.Contains(stdout, "change applied to the running host") {
		t.Errorf("service add did not report the change applied: %q", stdout)
	}
	if strings.Contains(stdout, "restart holebridge host") {
		t.Errorf("service add printed the restart note while the host reloads: %q", stdout)
	}
	st, err := queryStatus(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !hasService(st, "web", "detecting") {
		t.Errorf("after the reload the status does not list web: %+v", st.Services)
	}

	stop()
	d := <-done
	if d.code != 0 {
		t.Errorf("host exit code = %d, want 0 (stderr %q)", d.code, d.stderr)
	}
	for _, name := range []string{"host.lock", "control.sock"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s is still there after the host stopped", name)
		}
	}
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	appKey, err := config.LoadAppKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecret(t, "host", d.stdout+d.stderr, cfg.Key, keys.Format(cfg.Key), hex.EncodeToString(appKey[:]))
}

// Case 2: SIGHUP reloads host.json in a running host. A service added to the file by hand appears in the status
// answer after the signal, without any request over the socket.
func TestHostReloadsOnSIGHUP(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGHUP is not delivered to a process on Windows")
	}
	tn := hyperdht.NewTestnet(t, testnetSize)
	useTestnet(t, tn)
	dir := shortDir(t)

	done, stop := startHost(t, dir)
	waitForControl(t, dir, done)

	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Services = map[string]config.Service{"ssh": {Target: "127.0.0.1:22"}}
	if err := config.Save(dir, cfg); err != nil {
		t.Fatal(err)
	}
	p, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Signal(syscall.SIGHUP); err != nil {
		t.Fatalf("signal SIGHUP: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		st, err := queryStatus(dir)
		if err == nil && hasService(st, "ssh", "detecting") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("SIGHUP did not reload the service ssh into the status answer (last error %v)", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	stop()
	if d := <-done; d.code != 0 {
		t.Errorf("host exit code = %d, want 0 (stderr %q)", d.code, d.stderr)
	}
}

// Edge case: a host does not start while another one holds host.lock, and it leaves that lock in place.
func TestHostRefusesWhenAnotherHostHoldsTheLock(t *testing.T) {
	dir := shortDir(t)
	lock := filepath.Join(dir, "host.lock")
	if err := os.WriteFile(lock, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	controllableHost(t)

	code, stdout, stderr := run("--config", dir, "host")
	if code != 1 {
		t.Errorf("exit code = %d, want 1 (stderr %q)", code, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Errorf("host.lock was removed by the refused host: %v", err)
	}
}

// queryStatus asks the control socket of dir for its status.
func queryStatus(dir string) (status.Status, error) {
	var st status.Status
	raw, err := status.Query(dir, "status")
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(raw, &st)
}

// controllableHost replaces hostContext with a context the test cancels, for a host that is not started in the
// background. The default is restored when the test ends.
func controllableHost(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	prev := hostContext
	hostContext = func() (context.Context, context.CancelFunc) { return ctx, cancel }
	t.Cleanup(func() {
		cancel()
		hostContext = prev
	})
}

// reloadCounter is the control socket's handler in TestServiceAddSendsReloadOverControlSocket. It counts reload
// requests and reports no status.
type reloadCounter struct {
	mu      sync.Mutex
	reloads int
}

func (c *reloadCounter) Status() status.Status { return status.Status{} }

func (c *reloadCounter) Reload() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reloads++
	return nil
}

func (c *reloadCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reloads
}

// Case 4: holebridge service add on a running host sends a reload request over the control socket, and prints no
// restart note. The host is running when host.lock names a live process; the test writes its own process ID and
// serves the control socket with a handler that counts reload requests. The case lives here, not in internal/host,
// because internal/cli imports internal/host.
func TestServiceAddSendsReloadOverControlSocket(t *testing.T) {
	dir := shortDir(t)
	cfg := &config.Config{Key: keys.Generate(), Services: map[string]config.Service{"echo": {Target: "127.0.0.1:7", Kind: "tcp"}}}
	if err := config.Save(dir, cfg); err != nil {
		t.Fatalf("config.Save: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "host.lock"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatalf("write host.lock: %v", err)
	}

	handler := &reloadCounter{}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- status.Serve(ctx, dir, handler) }()
	t.Cleanup(func() {
		cancel()
		<-served
	})
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := status.Query(dir, "status"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the control socket did not answer a status request")
		}
		time.Sleep(50 * time.Millisecond)
	}

	code, stdout, stderr := run("--config", dir, "service", "add", "jellyfin", "8096")
	if code != 0 {
		t.Fatalf("service add exit code = %d, stderr: %s", code, stderr)
	}
	if n := handler.count(); n != 1 {
		t.Errorf("the control socket got %d reload requests from service add, want 1", n)
	}
	if strings.Contains(stdout, "restart holebridge host") {
		t.Error("service add printed the restart note on a running host, want a reload instead")
	}
}
