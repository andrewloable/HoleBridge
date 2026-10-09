package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/internal/config"
	"github.com/andrewloable/HoleBridge/internal/status"
)

// restartNote is printed by service add and rm while host.lock names a running host.
const restartNote = "restart holebridge host to apply"

// existingHostJSON is a host.json with one service, web. It has no key, so no test prints one.
const existingHostJSON = `{"services":{"web":{"target":"127.0.0.1:8080"}}}`

// writeHostJSON writes body as host.json in dir, with mode 0600 as config.Save writes it.
func writeHostJSON(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "host.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// readHostJSON returns the bytes of host.json in dir.
func readHostJSON(t *testing.T, dir string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "host.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// loadHostJSON loads the config in dir with config.Load, which also validates it.
func loadHostJSON(t *testing.T, dir string) *config.Config {
	t.Helper()
	c, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return c
}

// mustService returns the named service from host.json in dir, failing the test if it is absent.
func mustService(t *testing.T, dir, name string) config.Service {
	t.Helper()
	s, ok := loadHostJSON(t, dir).Services[name]
	if !ok {
		t.Fatalf("host.json has no service %q", name)
	}
	return s
}

// writeHostLock writes host.lock in dir naming this test process, which is live. The file holds
// the process ID in decimal and a newline.
func writeHostLock(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "host.lock"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// appliedLine is printed by service add and rm when the running host reloads host.json.
const appliedLine = "change applied to the running host"

// reloadHandler is the control socket of a running host in the tests: its reload succeeds, and it counts the
// reload requests.
type reloadHandler struct {
	reloads atomic.Int32
}

func (h *reloadHandler) Status() status.Status { return status.Status{} }

func (h *reloadHandler) Reload() error {
	h.reloads.Add(1)
	return nil
}

// controlDir returns a new directory for a config whose control socket is served there. A Unix socket path is
// limited to about 104 bytes on macOS, which a t.TempDir path can exceed.
func controlDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "hb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// serveControl serves the control socket in dir with h until the test ends, and returns once it answers a
// status request.
func serveControl(t *testing.T, dir string, h status.Handler) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- status.Serve(ctx, dir, h) }()
	t.Cleanup(func() {
		cancel()
		<-served
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := status.Query(dir, "status"); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the control socket did not answer a status request")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Case 1: service add web 127.0.0.1:8080 creates host.json with that service. Without --kind the
// host.json entry has no kind: the host detects it (docs/cli.md, hostjson).
func TestServiceAddCreatesHostJSON(t *testing.T) {
	dir := t.TempDir()
	code, _, stderr := run("--config", dir, "service", "add", "web", "127.0.0.1:8080")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dir, "host.json"))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("host.json mode = %o, want 600", got)
		}
	}
	s := mustService(t, dir, "web")
	if s.Target != "127.0.0.1:8080" {
		t.Errorf("target = %q, want %q", s.Target, "127.0.0.1:8080")
	}
	if s.Kind != "" {
		t.Errorf("kind = %q, want none (detected)", s.Kind)
	}
}

// Case 2: service add jellyfin 8096 stores the target 127.0.0.1:8096 (docs/cli.md, targets).
func TestServiceAddBarePortStoresLocalhostTarget(t *testing.T) {
	dir := t.TempDir()
	code, _, stderr := run("--config", dir, "service", "add", "jellyfin", "8096")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if got := mustService(t, dir, "jellyfin").Target; got != "127.0.0.1:8096" {
		t.Errorf("target = %q, want %q", got, "127.0.0.1:8096")
	}
}

// Case 3: service add dns 53 --kind udp stores kind udp.
func TestServiceAddKindUDPStoresKind(t *testing.T) {
	dir := t.TempDir()
	code, _, stderr := run("--config", dir, "service", "add", "dns", "53", "--kind", "udp")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	s := mustService(t, dir, "dns")
	if s.Kind != "udp" {
		t.Errorf("kind = %q, want %q", s.Kind, "udp")
	}
	if s.Target != "127.0.0.1:53" {
		t.Errorf("target = %q, want %q", s.Target, "127.0.0.1:53")
	}
}

// Case 4: add with an invalid name or target exits 2 with HB-USAGE and leaves host.json untouched.
// The file already holds a service, so "untouched" is checked on its bytes.
func TestServiceAddInvalidNameOrTargetExitsTwoAndLeavesHostJSON(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"name with a capital", []string{"Web", "127.0.0.1:8080"}},
		{"name starting with a dash", []string{"-a", "127.0.0.1:8080"}},
		{"empty name", []string{"", "127.0.0.1:8080"}},
		{"name of 33 characters", []string{strings.Repeat("a", 33), "127.0.0.1:8080"}},
		{"target that is not a port", []string{"api", "x:y"}},
		{"target port out of range", []string{"api", "70000"}},
		{"empty target", []string{"api", ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeHostJSON(t, dir, existingHostJSON)
			before := readHostJSON(t, dir)

			args := append([]string{"--config", dir, "service", "add"}, tc.args...)
			code, _, stderr := run(args...)
			if code != 2 {
				t.Errorf("exit code = %d, want 2 (stderr %q)", code, stderr)
			}
			if !strings.Contains(stderr, "HB-USAGE") {
				t.Errorf("stderr does not contain HB-USAGE: %q", stderr)
			}
			if after := readHostJSON(t, dir); !bytes.Equal(after, before) {
				t.Errorf("host.json changed after a rejected add")
			}
		})
	}
}

// Case 5: add of an existing name fails with a message naming it, and host.json keeps the old
// service. No catalog code is checked here: spec/errors.json has none for a duplicate name.
func TestServiceAddExistingNameFailsNamingIt(t *testing.T) {
	dir := t.TempDir()
	writeHostJSON(t, dir, existingHostJSON)
	before := readHostJSON(t, dir)

	code, _, stderr := run("--config", dir, "service", "add", "web", "127.0.0.1:9090")
	if code != 1 {
		t.Errorf("exit code = %d, want 1 (stderr %q)", code, stderr)
	}
	if !strings.Contains(stderr, "web") {
		t.Errorf("stderr does not name the service web: %q", stderr)
	}
	if after := readHostJSON(t, dir); !bytes.Equal(after, before) {
		t.Errorf("host.json changed after a failed add")
	}
}

// Case 6: rm removes a service. rm of a missing name exits 1 with HB-UNKNOWN-SERVICE, naming it.
func TestServiceRmRemovesAndFailsForMissingName(t *testing.T) {
	dir := t.TempDir()
	writeHostJSON(t, dir, `{"services":{"web":{"target":"127.0.0.1:8080"},"jellyfin":{"target":"127.0.0.1:8096"}}}`)

	code, _, stderr := run("--config", dir, "service", "rm", "web")
	if code != 0 {
		t.Fatalf("rm web: exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	services := loadHostJSON(t, dir).Services
	if _, ok := services["web"]; ok {
		t.Errorf("web is still in host.json after rm")
	}
	if _, ok := services["jellyfin"]; !ok {
		t.Errorf("rm web also removed jellyfin")
	}

	code, _, stderr = run("--config", dir, "service", "rm", "web")
	if code != 1 {
		t.Errorf("rm of a missing name: exit code = %d, want 1 (stderr %q)", code, stderr)
	}
	if !strings.Contains(stderr, "HB-UNKNOWN-SERVICE") {
		t.Errorf("stderr does not contain HB-UNKNOWN-SERVICE: %q", stderr)
	}
	// web is name-shaped, so the error names it (nameOf). A word that is not name-shaped is not printed, so a
	// key or an application key typed in the name slot never reaches stderr.
	if !strings.Contains(stderr, "web") {
		t.Errorf("stderr does not name the service web: %q", stderr)
	}
	for _, secret := range []string{"7KQM4X9TR", strings.Repeat("0123456789abcdef", 4)} {
		code, _, stderr = run("--config", dir, "service", "rm", secret)
		if code != 1 {
			t.Errorf("rm of a missing key-shaped name: exit code = %d, want 1 (stderr %q)", code, stderr)
		}
		if !strings.Contains(stderr, "HB-UNKNOWN-SERVICE") {
			t.Errorf("stderr does not contain HB-UNKNOWN-SERVICE: %q", stderr)
		}
		if strings.Contains(stderr, secret) {
			t.Errorf("stderr echoes the word typed as the service name: %q", stderr)
		}
	}
}

// Case 7: ls prints every service as name, kind and target, with detecting for a service whose
// kind is unknown: none in host.json and none in the kinds state file. A kind detected into the
// state file is shown. Each service is checked as one line of exactly those three fields, so the
// column layout and the order are left to the implementation.
func TestServiceLsPrintsKindOrDetecting(t *testing.T) {
	dir := t.TempDir()
	writeHostJSON(t, dir, `{"services":{`+
		`"web":{"target":"127.0.0.1:8080"},`+
		`"jellyfin":{"target":"127.0.0.1:8096"},`+
		`"ssh":{"target":"127.0.0.1:22","kind":"tcp"},`+
		`"dns":{"target":"127.0.0.1:53","kind":"udp"}}}`)
	if err := config.SaveState(dir, config.State{Kinds: map[string]string{"jellyfin": "https"}}); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := run("--config", dir, "service", "ls")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	want := [][]string{
		{"web", "detecting", "127.0.0.1:8080"},
		{"jellyfin", "https", "127.0.0.1:8096"},
		{"ssh", "tcp", "127.0.0.1:22"},
		{"dns", "udp", "127.0.0.1:53"},
	}
	lines := strings.Split(stdout, "\n")
	for _, w := range want {
		found := false
		for _, line := range lines {
			if reflect.DeepEqual(strings.Fields(line), w) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("ls has no line with fields %q; stdout:\n%s", w, stdout)
		}
	}
}

// Case 8: while host.lock in the config directory names a live process, add and rm ask the host to reload
// over the control socket. When the host reloads, they print that the change was applied and no restart
// note. When the reload cannot be asked, they print the restart note. Without host.lock, add prints no note.
// host.lock holds the process ID in decimal (the format is chosen by HoleBridge-trk.3; the host command must
// write the same).
func TestServiceAddAndRmPrintRestartNoteWhileHostRuns(t *testing.T) {
	t.Run("add reloads the running host", func(t *testing.T) {
		dir := controlDir(t)
		writeHostJSON(t, dir, existingHostJSON)
		writeHostLock(t, dir)
		handler := &reloadHandler{}
		serveControl(t, dir, handler)

		code, stdout, stderr := run("--config", dir, "service", "add", "jellyfin", "8096")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
		}
		if n := handler.reloads.Load(); n != 1 {
			t.Errorf("the control socket got %d reload requests, want 1", n)
		}
		if !strings.Contains(stdout, appliedLine) {
			t.Errorf("stdout does not contain %q: %q", appliedLine, stdout)
		}
		if strings.Contains(stdout, restartNote) {
			t.Errorf("stdout contains the restart note after a reload: %q", stdout)
		}
	})

	t.Run("rm reloads the running host", func(t *testing.T) {
		dir := controlDir(t)
		writeHostJSON(t, dir, existingHostJSON)
		writeHostLock(t, dir)
		handler := &reloadHandler{}
		serveControl(t, dir, handler)

		code, stdout, stderr := run("--config", dir, "service", "rm", "web")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
		}
		if n := handler.reloads.Load(); n != 1 {
			t.Errorf("the control socket got %d reload requests, want 1", n)
		}
		if !strings.Contains(stdout, appliedLine) {
			t.Errorf("stdout does not contain %q: %q", appliedLine, stdout)
		}
		if strings.Contains(stdout, restartNote) {
			t.Errorf("stdout contains the restart note after a reload: %q", stdout)
		}
	})

	t.Run("add prints the note when the reload cannot be asked", func(t *testing.T) {
		dir := t.TempDir()
		writeHostJSON(t, dir, existingHostJSON)
		writeHostLock(t, dir)

		code, stdout, stderr := run("--config", dir, "service", "add", "jellyfin", "8096")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
		}
		if !strings.Contains(stdout, restartNote) {
			t.Errorf("stdout does not contain %q: %q", restartNote, stdout)
		}
	})

	t.Run("rm prints the note when the reload cannot be asked", func(t *testing.T) {
		dir := t.TempDir()
		writeHostJSON(t, dir, existingHostJSON)
		writeHostLock(t, dir)

		code, stdout, stderr := run("--config", dir, "service", "rm", "web")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
		}
		if !strings.Contains(stdout, restartNote) {
			t.Errorf("stdout does not contain %q: %q", restartNote, stdout)
		}
	})

	t.Run("no host.lock, no note", func(t *testing.T) {
		dir := t.TempDir()
		writeHostJSON(t, dir, existingHostJSON)

		code, stdout, stderr := run("--config", dir, "service", "add", "jellyfin", "8096")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
		}
		if strings.Contains(stdout, restartNote) {
			t.Errorf("stdout contains the restart note without host.lock: %q", stdout)
		}
	})
}
