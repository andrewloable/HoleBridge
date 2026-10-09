package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/internal/config"
	"github.com/andrewloable/HoleBridge/internal/errs"
	"github.com/andrewloable/HoleBridge/internal/host/hosttest"
	"github.com/andrewloable/HoleBridge/internal/keys"
	"github.com/andrewloable/HoleBridge/internal/lan"
	"github.com/andrewloable/HoleBridge/internal/links"
	"github.com/andrewloable/HoleBridge/internal/status"
	"github.com/andrewloable/HoleBridge/pears/dhtrpc"
	"github.com/andrewloable/HoleBridge/pears/hyperdht"
	"github.com/andrewloable/HoleBridge/pears/noise"
	"github.com/andrewloable/HoleBridge/pears/secretstream"
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
	// The banner prints the key block (docs/cli.md, hosting), so only the lines outside it must hold no key.
	assertNoSecret(t, "host", outsideKeyBlock(d.stdout)+d.stderr, cfg.Key, keys.Format(cfg.Key), hex.EncodeToString(appKey[:]))
}

// outsideKeyBlock returns out without the lines of the banner's key block, the Key line and the key link, which
// docs/cli.md says a host prints. Every other line of the output must hold no key.
func outsideKeyBlock(out string) string {
	var keep []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "Key: ") || strings.Contains(line, "/k#") {
			continue
		}
		keep = append(keep, line)
	}
	return strings.Join(keep, "\n")
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

// A live session shows in holebridge status. The host the command runs serves an app-role client that dials it
// over the DHT, opens a stream on its echo service and moves bytes. The status answer and the printed table then
// list that session on the direct route with the stream and the bytes, and the answer names no key and no target
// address.
func TestStatusListsLiveSession(t *testing.T) {
	tn := hyperdht.NewTestnet(t, testnetSize)
	useTestnet(t, tn)
	dir := shortDir(t)
	target := echoListener(t)
	// host.json names the key and the service only. Defaults (the limits in particular) come from config.Load, as
	// they do for a host the command starts; a config built as a literal would carry zero limits and refuse every
	// session.
	doc, err := json.Marshal(map[string]any{
		"key":      keys.Format(keys.Generate()),
		"services": map[string]config.Service{"echo": {Target: target, Kind: "tcp"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "host.json"), doc, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}

	done, _ := startHost(t, dir)
	waitForControl(t, dir, done)

	appKey, err := config.LoadAppKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	d, err := keys.Derive(cfg.Key, appKey)
	if err != nil {
		t.Fatal(err)
	}
	node, err := hyperdht.New(hyperdht.Config{Bootstrap: tn.Bootstrap})
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()
	hostPub := [32]byte(d.Host.Public().(ed25519.PublicKey))

	// The host starts announcing as Run starts, which may be after the control socket answers, so a dial that finds
	// nothing yet is retried.
	var c *hosttest.Client
	deadline := time.Now().Add(60 * time.Second)
	for {
		c, err = hosttest.Connect(node, hostPub, d.Client)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Connect: %v", err)
		}
		time.Sleep(250 * time.Millisecond)
	}
	defer c.Close()

	st, err := c.Open("echo")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	payload := []byte("status probe")
	if _, err := st.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(st, got); err != nil {
		t.Fatalf("read the echo: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("the echo differs from the bytes sent")
	}

	n := len(payload)
	var raw []byte
	deadline = time.Now().Add(15 * time.Second)
	for {
		raw, err = status.Query(dir, "status")
		var answer status.Status
		if err == nil && json.Unmarshal(raw, &answer) == nil && len(answer.Sessions) == 1 {
			s := answer.Sessions[0]
			if s.Route == "direct" && s.Streams == 1 && s.BytesIn == uint64(n) && s.BytesOut == uint64(n) {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("status did not list the live session with its stream and bytes: %s (error %v)", raw, err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	code, stdout, stderr := run("--config", dir, "status")
	if code != 0 {
		t.Fatalf("status exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if !lineHas(stdout, "direct", strconv.Itoa(n), strconv.Itoa(n)) {
		t.Errorf("no line of the status table lists the session on route direct with its bytes; stdout:\n%s", stdout)
	}
	assertNoSecret(t, "status", string(raw), cfg.Key, keys.Format(cfg.Key), hex.EncodeToString(appKey[:]), target)
}

// echoListener starts a TCP echo server on 127.0.0.1 and returns its address. It stops when the test ends.
func echoListener(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				io.Copy(c, c)
				c.Close()
			}()
		}
	}()
	return l.Addr().String()
}

// cliExampleHostJSON is the host.json of the example in docs/cli.md#hosting: four services, two of them without a
// kind, and the relay of docs/cli.md#configuration.
const cliExampleHostJSON = `{
  "key": "7KQ-M4X-9TR",
  "services": {
    "web": { "target": "127.0.0.1:8080" },
    "jellyfin": { "target": "127.0.0.1:8096", "origins": ["http://jellyfin.example:8096"] },
    "ssh": { "target": "127.0.0.1:22", "kind": "tcp", "idle": "8h" },
    "dns": { "target": "127.0.0.1:53", "kind": "udp" }
  },
  "lan": { "enabled": true },
  "relay": "R4N-W8P-2KD",
  "limits": {}
}`

// fakeRunningHost is the runningHost of fakeHostRunner. It reports the NAT state and LAN address it was given, and
// it stops when its context ends.
type fakeRunningHost struct {
	ctx context.Context
	nat dhtrpc.NATInfo
	lan string
}

func (h *fakeRunningHost) NAT() dhtrpc.NATInfo { return h.nat }
func (h *fakeRunningHost) LANAddr() string     { return h.lan }
func (h *fakeRunningHost) Wait() error         { <-h.ctx.Done(); return nil }

// fakeHostRunner is the hostRunner of the host banner, LAN and share cases. It records each request and starts no
// network node.
type fakeHostRunner struct {
	nat dhtrpc.NATInfo
	lan string

	mu   sync.Mutex
	reqs []hostRequest
}

var _ hostRunner = (*fakeHostRunner)(nil)

func (f *fakeHostRunner) Start(ctx context.Context, req hostRequest) (runningHost, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	return &fakeRunningHost{ctx: ctx, nat: f.nat, lan: f.lan}, nil
}

// requests returns the requests the runner was started with, in order.
func (f *fakeHostRunner) requests() []hostRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]hostRequest(nil), f.reqs...)
}

// interruptedHost replaces hostContext with a context that is already done, so that a host or share command run
// through a fake runner prints its output and stops at once. The default is restored when the test ends.
func interruptedHost(t *testing.T) {
	t.Helper()
	prev := hostContext
	hostContext = func() (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx, cancel
	}
	t.Cleanup(func() { hostContext = prev })
}

// callCommand runs cmd on configDir with args, with buffers for the two streams. It returns what the command printed
// and its error.
func callCommand(cmd Command, configDir string, args ...string) (stdout, stderr string, err error) {
	var out, errOut bytes.Buffer
	env := Env{
		Stdout: &out,
		Stderr: &errOut,
		Getenv: func(string) string { return "" },
		Now:    time.Now,
	}
	err = cmd(args, env, configDir)
	return out.String(), errOut.String(), err
}

// hasLine reports whether one line of out is exactly line.
func hasLine(out, line string) bool {
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimRight(l, "\r") == line {
			return true
		}
	}
	return false
}

// Case 1: host with the example config of docs/cli.md#hosting prints "Hosting 4 services" with each name and kind.
// web and jellyfin name no kind in host.json, so they take the kinds that kinds.json holds.
// The services are listed sorted by name: config.Services is a map, so the key order of host.json is not kept.
func TestHostPrintsHostingLineWithKinds(t *testing.T) {
	interruptedHost(t)
	dir := shortDir(t)
	writeHostJSON(t, dir, cliExampleHostJSON)
	if err := config.SaveState(dir, config.State{Kinds: map[string]string{"web": "https", "jellyfin": "http"}}); err != nil {
		t.Fatal(err)
	}
	_, appKey, _ := vectors(t)
	writeAppKey(t, dir, appKey)

	stdout, stderr, err := callCommand(hostWith(&fakeHostRunner{lan: "192.0.2.10"}), dir)
	if err != nil {
		t.Fatalf("%v (stderr %q)", err, stderr)
	}
	const want = "Hosting 4 services: dns (udp), jellyfin (http), ssh (tcp), web (https)"
	if !hasLine(stdout, want) {
		t.Errorf("stdout has no line %q: %q", want, stdout)
	}
}

// Case 2: the banner of host carries the host key with its dashes, a QR block, and a key link whose two halves are
// the 9 key symbols after the # and the application key after the dot. The link is the one links.KeyLink builds.
func TestHostBannerHasKeyQRAndLinkWithBothHalves(t *testing.T) {
	interruptedHost(t)
	dir := shortDir(t)
	writeHostJSON(t, dir, keyHostJSON)
	_, appKey, _ := vectors(t)
	writeAppKey(t, dir, appKey)

	stdout, stderr, err := callCommand(hostWith(&fakeHostRunner{lan: "192.0.2.10"}), dir)
	if err != nil {
		t.Fatalf("%v (stderr %q)", err, stderr)
	}
	if got := keyLine(stdout); got != "7KQ-M4X-9TR" {
		t.Errorf("the Key line does not hold the host key with its dashes")
	}
	if qrBlockLines(stdout) == 0 {
		t.Error("the banner has no QR block")
	}
	want := links.KeyLink(links.DefaultBase, "7KQM4X9TR", appKey)
	if got := keyLinkIn(stdout); got != want {
		t.Errorf("the banner's link is not the key link with both halves")
	}
}

// Case 3: host.lock exists while the host command runs, and is removed once it has stopped. The command runs through
// a fake runner, so that the test covers the lock handling of hostWith and starts no network node.
func TestHostLockExistsWhileRunningAndIsRemovedAfterStop(t *testing.T) {
	dir := shortDir(t)
	writeHostJSON(t, dir, keyHostJSON)
	_, appKey, _ := vectors(t)
	writeAppKey(t, dir, appKey)
	ctx, cancel := context.WithCancel(context.Background())
	prev := hostContext
	hostContext = func() (context.Context, context.CancelFunc) { return ctx, cancel }
	t.Cleanup(func() {
		cancel()
		hostContext = prev
	})

	done := make(chan error, 1)
	go func() {
		_, _, err := callCommand(hostWith(&fakeHostRunner{lan: "192.0.2.10"}), dir)
		done <- err
	}()
	lock := filepath.Join(dir, "host.lock")
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case err := <-done:
			t.Fatalf("the host stopped before host.lock existed: %v", err)
		default:
		}
		if _, err := os.Stat(lock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("host.lock does not exist while the host runs")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("the host returned an error after it was stopped: %v", err)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Errorf("host.lock is still there after the host stopped (stat error %v)", err)
	}
}

// Case 4: with lan.enabled false the LAN line says off, and the runner is asked to start no LAN responder or listener.
func TestHostWithLANDisabledSaysOffAndStartsNoLAN(t *testing.T) {
	interruptedHost(t)
	dir := shortDir(t)
	writeHostJSON(t, dir, `{"key":"7KQ-M4X-9TR","lan":{"enabled":false}}`)
	_, appKey, _ := vectors(t)
	writeAppKey(t, dir, appKey)

	fake := &fakeHostRunner{}
	stdout, stderr, err := callCommand(hostWith(fake), dir)
	if err != nil {
		t.Fatalf("%v (stderr %q)", err, stderr)
	}
	reqs := fake.requests()
	if len(reqs) != 1 {
		t.Fatalf("the runner was started %d times, want 1", len(reqs))
	}
	if reqs[0].LAN {
		t.Error("the runner was asked to start the LAN responder and listener with lan.enabled false")
	}
	if !lineHas(stdout, "LAN:", "off") {
		t.Errorf("no LAN line says off: %q", stdout)
	}
}

// Case 5: share 8080 prints 'Sharing 127.0.0.1:8080 as "8080" (temporary key)' and leaves host.json absent. The one
// service is named after the port.
func TestShareOnPortPrintsSharingLineAndWritesNoHostJSON(t *testing.T) {
	interruptedHost(t)
	dir := shortDir(t)
	_, appKey, _ := vectors(t)
	writeAppKey(t, dir, appKey)

	fake := &fakeHostRunner{}
	stdout, stderr, err := callCommand(shareWith(fake), dir, "8080")
	if err != nil {
		t.Fatalf("%v (stderr %q)", err, stderr)
	}
	const want = `Sharing 127.0.0.1:8080 as "8080" (temporary key)`
	if !hasLine(stdout, want) {
		t.Errorf("stdout has no line %q: %q", want, stdout)
	}
	if _, err := os.Stat(filepath.Join(dir, "host.json")); !os.IsNotExist(err) {
		t.Errorf("share wrote host.json (stat error %v)", err)
	}
	reqs := fake.requests()
	if len(reqs) != 1 || reqs[0].Config == nil {
		t.Fatalf("the runner was started %d times, want 1 with a config", len(reqs))
	}
	if svc, ok := reqs[0].Config.Services["8080"]; !ok || svc.Target != "127.0.0.1:8080" {
		t.Errorf("the shared service is not 8080 at 127.0.0.1:8080: %+v", reqs[0].Config.Services)
	}
}

// Case 6: share --name web --kind https 8080 names its service web and gives it the kind https.
func TestShareNameAndKindFlagsSetServiceNameAndKind(t *testing.T) {
	interruptedHost(t)
	dir := shortDir(t)
	_, appKey, _ := vectors(t)
	writeAppKey(t, dir, appKey)

	fake := &fakeHostRunner{}
	stdout, stderr, err := callCommand(shareWith(fake), dir, "--name", "web", "--kind", "https", "8080")
	if err != nil {
		t.Fatalf("%v (stderr %q)", err, stderr)
	}
	const want = `Sharing 127.0.0.1:8080 as "web" (temporary key)`
	if !hasLine(stdout, want) {
		t.Errorf("stdout has no line %q: %q", want, stdout)
	}
	reqs := fake.requests()
	if len(reqs) != 1 || reqs[0].Config == nil {
		t.Fatalf("the runner was started %d times, want 1 with a config", len(reqs))
	}
	services := reqs[0].Config.Services
	if svc, ok := services["web"]; !ok || svc.Kind != "https" || svc.Target != "127.0.0.1:8080" {
		t.Errorf("the shared service is not web at 127.0.0.1:8080 with kind https: %+v", services)
	}
	if _, ok := services["8080"]; ok {
		t.Error("the service is named after the port although --name web was given")
	}
}

// Case 7: two runs of share print different keys, because each run makes a new temporary key.
func TestTwoShareRunsPrintDifferentKeys(t *testing.T) {
	interruptedHost(t)
	dir := shortDir(t)
	_, appKey, _ := vectors(t)
	writeAppKey(t, dir, appKey)

	var printed []string
	for i := 0; i < 2; i++ {
		stdout, stderr, err := callCommand(shareWith(&fakeHostRunner{}), dir, "8080")
		if err != nil {
			t.Fatalf("share run %d: %v (stderr %q)", i+1, err, stderr)
		}
		k := keyLine(stdout)
		if _, err := keys.Normalize(k); err != nil {
			t.Fatalf("share run %d printed no valid Key line", i+1)
		}
		printed = append(printed, k)
	}
	if printed[0] == printed[1] {
		t.Error("two share runs printed the same key")
	}
}

// Case 8: a LAN port that another program holds makes host exit 1 with HB-LAN-PORT-IN-USE and its fix, and the host
// does not keep running with the port taken. The command runs through the default runner, which binds the LAN port.
// Against the stub the command returns "not implemented" at once; a host that ignores the taken port blocks until
// the deadline, and the test then ends it and fails.
func TestHostExitsOneWhenLANPortIsInUse(t *testing.T) {
	tn := hyperdht.NewTestnet(t, testnetSize)
	useTestnet(t, tn)
	occupant, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { occupant.Close() })
	port := occupant.Addr().(*net.TCPAddr).Port
	dir := shortDir(t)
	writeHostJSON(t, dir, `{"key":"7KQ-M4X-9TR","lan":{"port":`+strconv.Itoa(port)+`}}`)
	_, appKey, _ := vectors(t)
	writeAppKey(t, dir, appKey)
	ctx, cancel := context.WithCancel(context.Background())
	prev := hostContext
	hostContext = func() (context.Context, context.CancelFunc) { return ctx, cancel }
	t.Cleanup(func() {
		cancel()
		hostContext = prev
	})

	type result struct {
		code   int
		stderr string
	}
	done := make(chan result, 1)
	go func() {
		var out, errOut bytes.Buffer
		env := Env{
			Stdout: &out,
			Stderr: &errOut,
			Getenv: func(string) string { return "" },
			Now:    time.Now,
		}
		code := 0
		if err := hostWith(defaultHostRunner{})(nil, env, dir); err != nil {
			code = fail(env, err) // the same step Run takes: print the error, take the exit code
		}
		done <- result{code, errOut.String()}
	}()
	select {
	case d := <-done:
		if d.code != 1 {
			t.Errorf("exit code = %d, want 1 (stderr %q)", d.code, d.stderr)
		}
		if !strings.Contains(d.stderr, "HB-LAN-PORT-IN-USE") {
			t.Errorf("stderr does not name HB-LAN-PORT-IN-USE: %q", d.stderr)
		}
		if !lineHas(d.stderr, "Fix:", "lan.port") {
			t.Errorf("stderr has no fix line naming lan.port: %q", d.stderr)
		}
	case <-time.After(15 * time.Second):
		cancel()
		<-done
		t.Fatal("host kept running with its LAN port in use, want exit 1")
	}
}

// The cases below are the banner, bootstrap and LAN cases that the TEST task did not write. They run the same
// command paths as the cases above.

// failingRunner is a hostRunner whose Start fails with err.
type failingRunner struct{ err error }

func (f failingRunner) Start(ctx context.Context, req hostRequest) (runningHost, error) {
	return nil, f.err
}

// callCommandEnv is callCommand with the environment variables env, which the command reads through Getenv.
func callCommandEnv(cmd Command, configDir string, env map[string]string, args ...string) (stdout, stderr string, err error) {
	var out, errOut bytes.Buffer
	e := Env{
		Stdout: &out,
		Stderr: &errOut,
		Getenv: func(k string) string { return env[k] },
		Now:    time.Now,
	}
	err = cmd(args, e, configDir)
	return out.String(), errOut.String(), err
}

// The Hosting line names no service when host.json names none.
func TestHostBannerNamesNoServicesWhenThereAreNone(t *testing.T) {
	interruptedHost(t)
	dir := shortDir(t)
	writeHostJSON(t, dir, keyHostJSON)
	_, appKey, _ := vectors(t)
	writeAppKey(t, dir, appKey)

	stdout, stderr, err := callCommand(hostWith(&fakeHostRunner{lan: "192.0.2.10"}), dir)
	if err != nil {
		t.Fatalf("%v (stderr %q)", err, stderr)
	}
	const want = "Hosting no services, add one with holebridge service add"
	if !hasLine(stdout, want) {
		t.Errorf("stdout has no line %q: %q", want, stdout)
	}
}

// The Internet line states the NAT state of the node, worded as holebridge status words it, and names no address.
func TestHostBannerInternetLineStatesTheNATState(t *testing.T) {
	cases := []struct {
		name string
		nat  dhtrpc.NATInfo
		want string
	}{
		{"no peer has reported the address", dhtrpc.NATInfo{}, "Internet: unknown (no peer has reported our address yet)"},
		{"no ping from outside yet", dhtrpc.NATInfo{Host: "192.0.2.20", Firewalled: true}, "Internet: not reachable (no ping has reached us from outside yet)"},
		{"randomized ports", dhtrpc.NATInfo{Host: "192.0.2.20", Firewalled: true, Randomized: true}, "Internet: not reachable (NAT: random, the ports change, so a relay may be needed)"},
		{"consistent", dhtrpc.NATInfo{Host: "192.0.2.20", Port: 27421}, "Internet: reachable (NAT: consistent)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			interruptedHost(t)
			dir := shortDir(t)
			writeHostJSON(t, dir, keyHostJSON)
			_, appKey, _ := vectors(t)
			writeAppKey(t, dir, appKey)

			stdout, stderr, err := callCommand(hostWith(&fakeHostRunner{nat: c.nat, lan: "192.0.2.10"}), dir)
			if err != nil {
				t.Fatalf("%v (stderr %q)", err, stderr)
			}
			if !hasLine(stdout, c.want) {
				t.Errorf("stdout has no line %q: %q", c.want, stdout)
			}
			if strings.Contains(stdout, "192.0.2.20") {
				t.Error("the banner prints the node's address")
			}
		})
	}
}

// The Relay line says whether a relay is set, and never prints the relay key.
func TestHostBannerRelayLineSaysWhetherARelayIsSet(t *testing.T) {
	cases := []struct {
		name, hostJSON, want string
	}{
		{"none", keyHostJSON, "Relay: none set"},
		{"set", cliExampleHostJSON, "Relay: set"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			interruptedHost(t)
			dir := shortDir(t)
			writeHostJSON(t, dir, c.hostJSON)
			_, appKey, _ := vectors(t)
			writeAppKey(t, dir, appKey)

			stdout, stderr, err := callCommand(hostWith(&fakeHostRunner{lan: "192.0.2.10"}), dir)
			if err != nil {
				t.Fatalf("%v (stderr %q)", err, stderr)
			}
			if !hasLine(stdout, c.want) {
				t.Errorf("stdout has no line %q: %q", c.want, stdout)
			}
			assertNoSecret(t, "host", stdout, "R4N-W8P-2KD", "R4NW8P2KD")
		})
	}
}

// The LAN line names the LAN addresses that the runner reports.
func TestHostBannerLANLineNamesTheLANAddresses(t *testing.T) {
	interruptedHost(t)
	dir := shortDir(t)
	writeHostJSON(t, dir, keyHostJSON)
	_, appKey, _ := vectors(t)
	writeAppKey(t, dir, appKey)

	stdout, stderr, err := callCommand(hostWith(&fakeHostRunner{lan: "192.0.2.10"}), dir)
	if err != nil {
		t.Fatalf("%v (stderr %q)", err, stderr)
	}
	if !hasLine(stdout, "LAN: listening on 192.0.2.10") {
		t.Errorf("stdout has no LAN line with the address: %q", stdout)
	}
}

// HOLEBRIDGE_BOOTSTRAP replaces the bootstrap nodes of the DHT node that a host starts, for testing (docs/cli.md,
// configuration).
func TestHostBootstrapEnvReplacesTheDefaultNodes(t *testing.T) {
	interruptedHost(t)
	dir := shortDir(t)
	writeHostJSON(t, dir, keyHostJSON)
	_, appKey, _ := vectors(t)
	writeAppKey(t, dir, appKey)

	fake := &fakeHostRunner{lan: "192.0.2.10"}
	_, stderr, err := callCommandEnv(hostWith(fake), dir, map[string]string{"HOLEBRIDGE_BOOTSTRAP": "192.0.2.1:49737,relay.example:5000"})
	if err != nil {
		t.Fatalf("%v (stderr %q)", err, stderr)
	}
	reqs := fake.requests()
	want := []string{"192.0.2.1:49737", "relay.example:5000"}
	if len(reqs) != 1 || !slices.Equal(reqs[0].Bootstrap, want) {
		t.Errorf("bootstrap nodes = %v, want %v", reqs, want)
	}
}

// A HOLEBRIDGE_BOOTSTRAP that is not a list of host:port nodes is a usage error, and the host does not start.
func TestHostBootstrapEnvMustBeHostPortNodes(t *testing.T) {
	interruptedHost(t)
	dir := shortDir(t)
	writeHostJSON(t, dir, keyHostJSON)
	_, appKey, _ := vectors(t)
	writeAppKey(t, dir, appKey)

	fake := &fakeHostRunner{}
	_, _, err := callCommandEnv(hostWith(fake), dir, map[string]string{"HOLEBRIDGE_BOOTSTRAP": "no-port-here"})
	var e *errs.Error
	if !errors.As(err, &e) || e.Code != "HB-USAGE" {
		t.Errorf("error = %v, want HB-USAGE", err)
	}
	if n := len(fake.requests()); n != 0 {
		t.Errorf("the runner was started %d times, want 0", n)
	}
}

// A host whose runner cannot start leaves no host.lock behind, and the command returns the runner's error.
func TestHostRemovesLockWhenTheRunnerFailsToStart(t *testing.T) {
	interruptedHost(t)
	dir := shortDir(t)
	writeHostJSON(t, dir, keyHostJSON)
	_, appKey, _ := vectors(t)
	writeAppKey(t, dir, appKey)

	boom := errors.New("runner refused to start")
	_, _, err := callCommand(hostWith(failingRunner{err: boom}), dir)
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want the runner's error", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "host.lock")); !os.IsNotExist(statErr) {
		t.Errorf("host.lock is still there after the runner failed (stat error %v)", statErr)
	}
}

// freeTCPPort returns a TCP port on 127.0.0.1 that nothing holds at the time of the call.
func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// freeUDPPort returns a UDP port on 127.0.0.1 that nothing holds at the time of the call.
func freeUDPPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

// probeLAN sends one signed probe under lanKey to the discovery port and returns the TCP port the reply names, and
// whether a reply came in time.
func probeLAN(t *testing.T, udpPort int, lanKey [32]byte) (uint16, bool) {
	t.Helper()
	var nonce [16]byte
	rand.Read(nonce[:])
	probe := lan.EncodeProbe(lanKey, nonce, uint64(time.Now().UnixMilli()))
	conn, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: udpPort})
	if err != nil {
		t.Fatalf("dial the discovery port: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write(probe[:]); err != nil {
		t.Fatalf("write probe: %v", err)
	}
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		return 0, false
	}
	port, ok := lan.VerifyReply(lanKey, buf[:n], nonce)
	return port, ok
}

// The LAN route of a running host answers a signed probe under its LAN key with its LAN TCP port, and that port
// accepts connections. A probe under another key gets no answer.
func TestHostLANRouteAnswersProbeAndListens(t *testing.T) {
	tn := hyperdht.NewTestnet(t, testnetSize)
	useTestnet(t, tn)
	udpPort, tcpPort := freeUDPPort(t), freeTCPPort(t)
	dir := shortDir(t)
	writeHostJSON(t, dir, `{"key":"7KQ-M4X-9TR","lan":{"discoveryPort":`+strconv.Itoa(udpPort)+`,"port":`+strconv.Itoa(tcpPort)+`}}`)
	_, appKey, _ := vectors(t)
	writeAppKey(t, dir, appKey)
	d, err := keys.Derive("7KQM4X9TR", appKey)
	if err != nil {
		t.Fatal(err)
	}

	done, stop := startHost(t, dir)
	waitForControl(t, dir, done)
	defer stop()

	port, ok := probeLAN(t, udpPort, d.LAN.Reveal())
	if !ok || int(port) != tcpPort {
		t.Errorf("probe under the host's LAN key: reply port %d, answered %v; want port %d", port, ok, tcpPort)
	}
	c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(tcpPort)))
	if err != nil {
		t.Errorf("the LAN TCP port does not accept connections: %v", err)
	} else {
		c.Close()
	}
	var other [32]byte
	rand.Read(other[:])
	if _, ok := probeLAN(t, udpPort, other); ok {
		t.Error("a probe under another key was answered")
	}
}

// When host.json changes the host key and the host reloads, the LAN route listens under the new key: a probe under the
// new key is answered, and one under the old key is not.
func TestHostLANFollowsAChangedKeyOnReload(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGHUP is not delivered to a process on Windows")
	}
	tn := hyperdht.NewTestnet(t, testnetSize)
	useTestnet(t, tn)
	udpPort, tcpPort := freeUDPPort(t), freeTCPPort(t)
	dir := shortDir(t)
	writeHostJSON(t, dir, `{"key":"7KQ-M4X-9TR","lan":{"discoveryPort":`+strconv.Itoa(udpPort)+`,"port":`+strconv.Itoa(tcpPort)+`}}`)
	_, appKey, _ := vectors(t)
	writeAppKey(t, dir, appKey)
	oldDerived, err := keys.Derive("7KQM4X9TR", appKey)
	if err != nil {
		t.Fatal(err)
	}
	oldLANKey := oldDerived.LAN.Reveal()

	done, stop := startHost(t, dir)
	waitForControl(t, dir, done)
	defer stop()
	if _, ok := probeLAN(t, udpPort, oldLANKey); !ok {
		t.Fatal("the LAN route does not answer under the host's first key")
	}

	newHostKey := keys.Generate()
	newDerived, err := keys.Derive(newHostKey, appKey)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Key = newHostKey
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

	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, ok := probeLAN(t, udpPort, newDerived.LAN.Reveal()); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the LAN route does not answer under the changed key after the reload")
		}
	}
	if _, ok := probeLAN(t, udpPort, oldLANKey); ok {
		t.Error("the LAN route still answers under the old key after the reload")
	}
}

// A reload that does not change the host key leaves the LAN route and its sessions alone: an app holding a LAN session
// keeps it through "service add", and the route still answers under the same key. The first reload after start used to
// tear the route down, because the key the route listens under was never recorded.
func TestHostReloadKeepsLANSessionWhenKeyIsUnchanged(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGHUP is not delivered to a process on Windows")
	}
	tn := hyperdht.NewTestnet(t, testnetSize)
	useTestnet(t, tn)
	udpPort, tcpPort := freeUDPPort(t), freeTCPPort(t)
	dir := shortDir(t)
	writeHostJSON(t, dir, `{"key":"7KQ-M4X-9TR","services":{"echo":{"target":"127.0.0.1:9","kind":"tcp"}},"lan":{"discoveryPort":`+strconv.Itoa(udpPort)+`,"port":`+strconv.Itoa(tcpPort)+`}}`)
	_, appKey, _ := vectors(t)
	writeAppKey(t, dir, appKey)
	d, err := keys.Derive("7KQM4X9TR", appKey)
	if err != nil {
		t.Fatal(err)
	}

	done, stop := startHost(t, dir)
	waitForControl(t, dir, done)
	defer stop()

	// An app attaches over the LAN route, as the LAN client of the tests does.
	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(tcpPort)))
	if err != nil {
		t.Fatalf("dial the LAN port: %v", err)
	}
	defer conn.Close()
	var kp noise.KeyPair
	copy(kp.Public[:], d.Client.Public().(ed25519.PublicKey))
	copy(kp.Secret[:], d.Client)
	hostPub := [32]byte(d.Host.Public().(ed25519.PublicKey))
	st := secretstream.New(conn, true, secretstream.Options{KeyPair: kp, RemotePublicKey: &hostPub})
	hctx, hcancel := context.WithTimeout(context.Background(), 5*time.Second)
	err = st.Handshake(hctx)
	hcancel()
	if err != nil {
		t.Fatalf("LAN handshake: %v", err)
	}
	c, err := hosttest.Attach(st)
	if err != nil {
		t.Fatalf("attach the LAN session: %v", err)
	}
	defer c.Close()

	code, out, errOut := run("--config", dir, "service", "add", "web", "9998", "--kind", "tcp")
	if code != 0 || !strings.Contains(out, "change applied") {
		t.Fatalf("service add: exit %d, stdout %q, stderr %q; want exit 0 and the change applied", code, out, errOut)
	}
	wctx, wcancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer wcancel()
	if c.WaitClosed(wctx) == nil {
		t.Fatal("the LAN session closed on a reload that did not change the host key")
	}
	if port, ok := probeLAN(t, udpPort, d.LAN.Reveal()); !ok || int(port) != tcpPort {
		t.Errorf("the LAN route after the reload: reply port %d, answered %v; want port %d", port, ok, tcpPort)
	}
}

// The host logs at the global --log-level: the logger the runner receives is enabled for info only when the level is
// info or lower. With the zero Env, which is what a caller that sets no level passes, it logs at info.
func TestHostLogsAtTheGlobalLogLevel(t *testing.T) {
	interruptedHost(t)
	cases := []struct {
		name      string
		level     slog.Level
		wantInfo  bool
		wantError bool
	}{
		{"error", slog.LevelError, false, true},
		{"zero Env", 0, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := shortDir(t)
			writeHostJSON(t, dir, keyHostJSON)
			_, appKey, _ := vectors(t)
			writeAppKey(t, dir, appKey)
			fake := &fakeHostRunner{}
			var stdout, stderr bytes.Buffer
			env := Env{
				Stdout:   &stdout,
				Stderr:   &stderr,
				Getenv:   func(string) string { return "" },
				Now:      time.Now,
				LogLevel: tc.level,
			}
			if err := hostWith(fake)(nil, env, dir); err != nil {
				t.Fatalf("host: %v (stderr %q)", err, stderr.String())
			}
			reqs := fake.requests()
			if len(reqs) != 1 {
				t.Fatalf("the runner was started %d times, want 1", len(reqs))
			}
			lg := reqs[0].Logger
			if got := lg.Enabled(context.Background(), slog.LevelInfo); got != tc.wantInfo {
				t.Errorf("logger enabled for info = %v, want %v", got, tc.wantInfo)
			}
			if got := lg.Enabled(context.Background(), slog.LevelError); got != tc.wantError {
				t.Errorf("logger enabled for error = %v, want %v", got, tc.wantError)
			}
		})
	}
}

// A host whose control socket cannot be served prints no banner: here the config directory is not owner-only, so
// Serve refuses, and the command returns that error with nothing on stdout.
func TestHostPrintsNoBannerWhenTheControlSocketCannotServe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the control socket's directory check is POSIX")
	}
	tn := hyperdht.NewTestnet(t, testnetSize)
	useTestnet(t, tn)
	dir := shortDir(t)
	writeHostJSON(t, dir, keyHostJSON)
	_, appKey, _ := vectors(t)
	writeAppKey(t, dir, appKey)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	controllableHost(t)

	stdout, stderr, err := callCommand(hostWith(defaultHostRunner{}), dir)
	var e *errs.Error
	if !errors.As(err, &e) || e.Code != "HB-CONFIG-DIR-PERMS" {
		t.Errorf("error = %v (stderr %q), want HB-CONFIG-DIR-PERMS", err, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want no banner for a host that does not serve", stdout)
	}
}

// An explicit --bootstrap beats HOLEBRIDGE_BOOTSTRAP, and the variable beats the default list (relay and relay check
// read the same two settings through bootstrapFlag).
func TestBootstrapFlagBeatsEnvironmentWhichBeatsDefault(t *testing.T) {
	fromEnv := Env{Getenv: func(k string) string {
		if k == "HOLEBRIDGE_BOOTSTRAP" {
			return "192.0.2.1:49737"
		}
		return ""
	}}
	got, err := bootstrapFlag(map[string]string{"bootstrap": "192.0.2.9:49737"}, fromEnv)
	if err != nil || !slices.Equal(got, []string{"192.0.2.9:49737"}) {
		t.Errorf("--bootstrap given: nodes %v, %v; want the flag's node", got, err)
	}
	got, err = bootstrapFlag(map[string]string{}, fromEnv)
	if err != nil || !slices.Equal(got, []string{"192.0.2.1:49737"}) {
		t.Errorf("no flag, env set: nodes %v, %v; want the env node", got, err)
	}
	got, err = bootstrapFlag(map[string]string{}, Env{Getenv: func(string) string { return "" }})
	if err != nil || !slices.Equal(got, bootstrap) {
		t.Errorf("no flag, no env: nodes %v, %v; want the default list", got, err)
	}
}
