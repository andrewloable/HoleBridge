package host

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/internal/cli"
	"github.com/andrewloable/HoleBridge/internal/config"
	"github.com/andrewloable/HoleBridge/internal/host/hosttest"
	"github.com/andrewloable/HoleBridge/internal/keys"
	"github.com/andrewloable/HoleBridge/internal/log"
	"github.com/andrewloable/HoleBridge/internal/protocol"
	"github.com/andrewloable/HoleBridge/internal/status"
	"github.com/andrewloable/HoleBridge/pears/hyperdht"
	"github.com/andrewloable/HoleBridge/pears/noise"
)

// writeHostJSON writes host.json in dir for key and services, with the LAN route off, as hostConfig does.
func writeHostJSON(t *testing.T, dir, key string, services map[string]config.Service) {
	t.Helper()
	doc := map[string]any{
		"key":      keys.Format(key),
		"services": services,
		"lan":      map[string]any{"enabled": false},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal host.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "host.json"), b, 0o600); err != nil {
		t.Fatalf("write host.json: %v", err)
	}
}

// reloadHost starts Run on a host for the config in dir, with Options.Dir set to dir, so that Reload re-reads
// that host.json. It stops Run when the test ends and returns the host, whose Reload the test calls.
func (r *rig) reloadHost(t *testing.T, dir string) *Host {
	t.Helper()
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	h, err := New(cfg, r.appKey, Options{
		DHT:   r.tn.Nodes[0],
		Clock: time.Now,
		Log:   log.New(r.logs, slog.LevelDebug),
		Dir:   dir,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(readWait):
			t.Errorf("Run did not return within %v of its context ending", readWait)
		}
	})
	return h
}

// reload runs Reload and fails the test on its error. A stub's error is reported as not implemented.
func reload(t *testing.T, h *Host) {
	t.Helper()
	err := h.Reload()
	failIfStub(t, err)
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
}

// Case 1: after a service is added to host.json and Reload runs, a connected client receives a services message
// that lists the new service with its kind and port hint, and still lists the service it had.
func TestReloadPushesAddedServiceToConnectedApp(t *testing.T) {
	r := newRig(t)
	dir := t.TempDir()
	echo := echoTarget(t)
	writeHostJSON(t, dir, r.key, map[string]config.Service{"echo": {Target: echo, Kind: "tcp"}})
	h := r.reloadHost(t, dir)
	c := r.connect(t, r.clientKey)

	writeHostJSON(t, dir, r.key, map[string]config.Service{
		"echo": {Target: echo, Kind: "tcp"},
		"web":  {Target: "127.0.0.1:8080", Kind: "http"},
	})
	reload(t, h)

	ctx, cancel := context.WithTimeout(context.Background(), readWait)
	defer cancel()
	list, err := c.NextServices(ctx)
	failIfStub(t, err)
	if err != nil {
		t.Fatalf("NextServices: %v", err)
	}
	found := map[string]protocol.Service{}
	for _, s := range list {
		found[s.Name] = s
	}
	web, ok := found["web"]
	if !ok {
		t.Fatalf("the services message lists %d services and not web", len(list))
	}
	if web.Kind != protocol.KindHTTP || web.Port != 8080 {
		t.Errorf("web: kind %d port %d, want kind %d port 8080", web.Kind, web.Port, protocol.KindHTTP)
	}
	if _, ok := found["echo"]; !ok {
		t.Error("the services message does not list echo, which was not removed")
	}
}

// Case 2: after a service is removed from host.json and Reload runs, a new open of it is rejected with code 1,
// and a stream opened before the reload still carries bytes to its target.
func TestReloadRemovedServiceRejectsNewOpensKeepsOpenStreams(t *testing.T) {
	r := newRig(t)
	dir := t.TempDir()
	echo := echoTarget(t)
	services := map[string]config.Service{
		"echo": {Target: echo, Kind: "tcp"},
		"gone": {Target: echo, Kind: "tcp"},
	}
	writeHostJSON(t, dir, r.key, services)
	h := r.reloadHost(t, dir)
	c := r.connect(t, r.clientKey)

	st, err := c.Open("gone")
	failIfStub(t, err)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if got := roundTrip(t, st, []byte("before the reload")); string(got) != "before the reload" {
		t.Fatal("the echo differs before the reload")
	}

	delete(services, "gone")
	writeHostJSON(t, dir, r.key, services)
	reload(t, h)

	_, err = c.Open("gone")
	if re := rejectOf(t, err); re.Code != 1 {
		t.Errorf("reject code = %d, want 1 (unknown service) for a removed service", re.Code)
	}
	if got := roundTrip(t, st, []byte("after the reload")); !bytes.Equal(got, []byte("after the reload")) {
		t.Error("the stream opened before the reload no longer echoes")
	}
}

// Case 3: after the key in host.json is rotated and Reload runs, the host listens under the new key: a client
// with the new key pair connects, the old client's connection is closed, the old client key is refused, and the
// old host key no longer accepts connections.
func TestReloadRotatedKeyRelistensAndClosesOldSessions(t *testing.T) {
	r := newRig(t)
	dir := t.TempDir()
	services := echoServices(t)
	writeHostJSON(t, dir, r.key, services)
	h := r.reloadHost(t, dir)
	old := r.connect(t, r.clientKey)
	oldHostPub, oldClientKey := r.hostPub, r.clientKey

	newKey := keys.Generate()
	d, err := keys.Derive(newKey, r.appKey)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	writeHostJSON(t, dir, newKey, services)
	reload(t, h)

	ctx, cancel := context.WithTimeout(context.Background(), readWait)
	err = old.WaitClosed(ctx)
	cancel()
	failIfStub(t, err)
	if err != nil {
		t.Fatalf("the old client's connection was not closed after the rotation: %v", err)
	}

	r.key = newKey
	r.hostPub = [32]byte(d.Host.Public().(ed25519.PublicKey))
	r.clientKey = d.Client
	r.connect(t, r.clientKey)

	c, err := hosttest.Connect(r.tn.Nodes[1], r.hostPub, oldClientKey)
	if err == nil {
		c.Close()
		t.Fatal("the old client key was admitted by the host under the new key")
	}
	failIfStub(t, err)

	var kp noise.KeyPair
	copy(kp.Public[:], oldClientKey.Public().(ed25519.PublicKey))
	copy(kp.Secret[:], oldClientKey)
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	conn, err := r.tn.Nodes[1].Connect(dialCtx, oldHostPub, hyperdht.ConnectOptions{KeyPair: &kp})
	dialCancel()
	if err == nil {
		conn.Close()
		t.Fatal("the host still accepts connections under the old host key")
	}
}

// reloadCounter is the control socket's handler in case 4. It counts reload requests and reports no status.
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
// restart note. The host is running when host.lock names a live process; the test writes its own process ID,
// and serves the control socket with a handler that counts reload requests.
func TestServiceAddSendsReloadOverControlSocket(t *testing.T) {
	// A Unix socket path is limited to about 104 bytes on macOS, so the directory is not t.TempDir.
	dir, err := os.MkdirTemp("", "hb")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	writeHostJSON(t, dir, keys.Generate(), map[string]config.Service{"echo": {Target: "127.0.0.1:7", Kind: "tcp"}})
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
	deadline := time.Now().Add(readWait)
	for {
		if _, err := status.Query(dir, "status"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the control socket did not answer a status request")
		}
		time.Sleep(50 * time.Millisecond)
	}

	var stdout, stderr bytes.Buffer
	code := cli.Run([]string{"--config", dir, "service", "add", "jellyfin", "8096"}, cli.Env{
		Stdout: &stdout,
		Stderr: &stderr,
		Getenv: func(string) string { return "" },
		Now:    time.Now,
	})
	if code != 0 {
		t.Fatalf("service add exit code = %d, stderr: %s", code, stderr.String())
	}
	if n := handler.count(); n != 1 {
		t.Errorf("the control socket got %d reload requests from service add, want 1", n)
	}
	if strings.Contains(stdout.String(), "restart holebridge host") {
		t.Error("service add printed the restart note on a running host, want a reload instead")
	}
}
