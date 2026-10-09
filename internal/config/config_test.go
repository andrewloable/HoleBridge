package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/internal/errs"
	"github.com/andrewloable/HoleBridge/internal/keys"
)

// exampleHostJSON is the host.json of docs/cli.md, Configuration.
const exampleHostJSON = `{
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

// exampleKey and exampleRelay are the canonical forms of the two keys in exampleHostJSON.
const (
	exampleKey   = "7KQM4X9TR"
	exampleRelay = "R4NW8P2KD"
)

// wantLimits are the defaults of docs/architecture.md#limits.
func wantLimits() Limits {
	return Limits{
		SessionsPerKey:         32,
		StreamsPerSession:      128,
		StreamsTotal:           1024,
		TargetConnectTimeout:   Duration(10 * time.Second),
		LANHandshakeDeadline:   Duration(5 * time.Second),
		UnauthLANTotal:         32,
		UnauthLANPerIP:         4,
		ReceiveWindowPerStream: 2 << 20,
		UDPFlowsPerSession:     256,
		UDPFlowsTotal:          4096,
		UDPFlowIdle:            Duration(60 * time.Second),
		MaxDatagram:            1144, // 1156-byte unordered message less the 12-byte frame header: docs/spike-m1.md, "Unordered datagrams"
		OrderedDatagramQueue:   256 << 10,
		ReceiveBudget:          256 << 20,
	}
}

// writeHostJSON writes body as host.json in dir, then sets its mode exactly (the umask would
// otherwise change a 0644 request).
func writeHostJSON(t *testing.T, dir, body string, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(dir, "host.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// errCode returns the catalog code of err, or "" when err is not an errs.Error.
func errCode(err error) string {
	var e *errs.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// report describes err without its text, which could carry key material from a broken
// implementation. It shows the catalog code, or "not implemented" for the stubs.
func report(err error) string {
	switch {
	case err == nil:
		return "no error"
	case errCode(err) != "":
		return errCode(err)
	case errors.Is(err, errors.ErrUnsupported):
		return "not implemented"
	default:
		return "an error without a catalog code"
	}
}

// skipOnWindows skips the POSIX file-mode checks.
func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes")
	}
}

// Case 1: the cli.md example comes back with the same values.
func TestLoadExampleHostJSON(t *testing.T) {
	dir := t.TempDir()
	writeHostJSON(t, dir, exampleHostJSON, 0o600)

	c, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: want no error, got %s", report(err))
	}
	// Keys are compared without printing them.
	if c.Key != exampleKey {
		t.Error("Key: want the canonical form of the example key")
	}
	if c.Relay != exampleRelay {
		t.Error("Relay: want the canonical form of the example relay key")
	}
	wantServices := map[string]Service{
		"web":      {Target: "127.0.0.1:8080"},
		"jellyfin": {Target: "127.0.0.1:8096", Origins: []string{"http://jellyfin.example:8096"}},
		"ssh":      {Target: "127.0.0.1:22", Kind: "tcp", Idle: Duration(8 * time.Hour)},
		"dns":      {Target: "127.0.0.1:53", Kind: "udp"},
	}
	if !reflect.DeepEqual(c.Services, wantServices) {
		t.Errorf("Services: got %+v, want %+v", c.Services, wantServices)
	}
	if c.LAN.Enabled == nil || !*c.LAN.Enabled {
		t.Error("LAN.Enabled: want true from the example")
	}
	if c.Limits != wantLimits() {
		t.Error("Limits: want the defaults for an empty limits object")
	}
}

// Case 2: Save then Load round-trips, and the file is mode 0600 (not checked on Windows).
func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	enabled := true
	in := &Config{
		Key: keys.Generate(),
		Services: map[string]Service{
			"web": {Target: "127.0.0.1:8080", Kind: "https", Origins: []string{"https://web.example.com"}},
			"ssh": {Target: "127.0.0.1:22", Kind: "tcp", Idle: Duration(8 * time.Hour)},
		},
		LAN:    LANConfig{Enabled: &enabled, DiscoveryPort: 47001, Port: 47002},
		Relay:  exampleRelay,
		Limits: wantLimits(),
	}
	if err := Save(dir, in); err != nil {
		t.Fatalf("Save: want no error, got %s", report(err))
	}

	raw, err := os.ReadFile(filepath.Join(dir, "host.json"))
	if err != nil {
		t.Fatal(err)
	}
	// cli.md shows the key in three groups and the file indented by two spaces.
	if !strings.Contains(string(raw), `"key": "`+keys.Format(in.Key)+`"`) {
		t.Error("host.json: want the key written as XXX-XXX-XXX")
	}
	if !strings.Contains(string(raw), "\n  \"key\"") {
		t.Error("host.json: want two-space indentation")
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dir, "host.json"))
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("host.json mode: got %o, want 600", mode)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "host.json" {
		t.Errorf("directory after Save holds %d entries, want only host.json (no temp file left)", len(entries))
	}

	out, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: want no error, got %s", report(err))
	}
	if !reflect.DeepEqual(out, in) {
		t.Error("Load after Save: the config changed (values not printed, they include the key)")
	}
}

// Case 3: a host.json that group or others can read fails with HB-CONFIG-PERMS on POSIX.
func TestLoadRejectsGroupOrOtherReadable(t *testing.T) {
	skipOnWindows(t)
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604} {
		t.Run(fmt.Sprintf("mode%o", mode), func(t *testing.T) {
			dir := t.TempDir()
			writeHostJSON(t, dir, exampleHostJSON, mode)
			_, err := Load(dir)
			if code := errCode(err); code != "HB-CONFIG-PERMS" {
				t.Fatalf("Load: want HB-CONFIG-PERMS, got %s", report(err))
			}
			for _, secret := range []string{"7KQ-M4X-9TR", exampleKey} {
				if strings.Contains(err.Error(), secret) {
					t.Error("the error text contains the host key")
				}
			}
		})
	}
}

// Case 3, the other side: an owner-only file loads.
func TestLoadAcceptsOwnerOnlyMode(t *testing.T) {
	skipOnWindows(t)
	dir := t.TempDir()
	writeHostJSON(t, dir, exampleHostJSON, 0o600)
	if _, err := Load(dir); err != nil {
		t.Fatalf("Load: want no error for mode 0600, got %s", report(err))
	}
}

// Case 4: ParseTarget accepts the forms of docs/cli.md. The LAN and host examples use RFC 5737
// and RFC 2606 names instead of private addresses.
func TestParseTargetAccepts(t *testing.T) {
	for _, tc := range []struct {
		in   string
		host string
		port int
	}{
		{"8080", "127.0.0.1", 8080},
		{"203.0.113.20:445", "203.0.113.20", 445},
		{"nas.example.com:445", "nas.example.com", 445},
		{"[::1]:8080", "::1", 8080},
	} {
		t.Run(tc.in, func(t *testing.T) {
			host, port, err := ParseTarget(tc.in)
			if err != nil {
				t.Fatalf("ParseTarget(%q): want no error, got %s", tc.in, report(err))
			}
			if host != tc.host || port != tc.port {
				t.Errorf("ParseTarget(%q) = %q, %d; want %q, %d", tc.in, host, port, tc.host, tc.port)
			}
		})
	}
}

// Case 4: ParseTarget rejects an empty target, a non-numeric port and a port above 65535.
func TestParseTargetRejects(t *testing.T) {
	for _, in := range []string{"", "x:y", "70000"} {
		t.Run(fmt.Sprintf("%q", in), func(t *testing.T) {
			if _, _, err := ParseTarget(in); err == nil {
				t.Errorf("ParseTarget(%q): want an error", in)
			}
		})
	}
}

// Case 5: service names follow docs/cli.md, Service names.
func TestValidServiceName(t *testing.T) {
	for _, tc := range []struct {
		name string
		ok   bool
	}{
		{"web", true},
		{"jellyfin", true},
		{"a-1", true},
		{"9lives", true},
		{"a", true},
		{strings.Repeat("a", 32), true},
		{"", false},
		{"-a", false},
		{"Web", false},
		{strings.Repeat("a", 33), false},
	} {
		if got := ValidServiceName(tc.name); got != tc.ok {
			t.Errorf("ValidServiceName(%.40q) = %v, want %v", tc.name, got, tc.ok)
		}
	}
}

// Case 6: origins on a tcp or udp service are HB-CONFIG-INVALID; on a web service they are kept.
func TestOriginsOnlyOnWebKinds(t *testing.T) {
	for _, tc := range []struct {
		kind    string
		wantErr bool
	}{
		{"tcp", true},
		{"udp", true},
		{"https", false},
		{"http", false},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			dir := t.TempDir()
			body := `{"services":{"svc":{"target":"127.0.0.1:8080","kind":"` + tc.kind +
				`","origins":["https://svc.example.com"]}}}`
			writeHostJSON(t, dir, body, 0o600)

			c, err := Load(dir)
			if tc.wantErr {
				if code := errCode(err); code != "HB-CONFIG-INVALID" {
					t.Fatalf("Load: want HB-CONFIG-INVALID, got %s", report(err))
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: want no error, got %s", report(err))
			}
			want := []string{"https://svc.example.com"}
			if !reflect.DeepEqual(c.Services["svc"].Origins, want) {
				t.Errorf("origins on a %s service: want them kept", tc.kind)
			}
		})
	}
}

// Case 7: Dir takes the flag, then HOLEBRIDGE_CONFIG, then XDG_CONFIG_HOME, then the default.
func TestDirPrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	flagDir := filepath.Join(t.TempDir(), "flag")
	envDir := filepath.Join(t.TempDir(), "env")
	xdg := filepath.Join(t.TempDir(), "xdg")

	t.Run("flag beats HOLEBRIDGE_CONFIG", func(t *testing.T) {
		got, err := Dir(flagDir, env(map[string]string{
			"HOLEBRIDGE_CONFIG": envDir, "XDG_CONFIG_HOME": xdg, "HOME": home,
		}))
		if err != nil || got != flagDir {
			t.Errorf("Dir with a flag: got %q (%s), want %q", got, report(err), flagDir)
		}
	})

	t.Run("HOLEBRIDGE_CONFIG beats XDG_CONFIG_HOME", func(t *testing.T) {
		got, err := Dir("", env(map[string]string{
			"HOLEBRIDGE_CONFIG": envDir, "XDG_CONFIG_HOME": xdg, "HOME": home,
		}))
		if err != nil || got != envDir {
			t.Errorf("Dir with HOLEBRIDGE_CONFIG: got %q (%s), want %q", got, report(err), envDir)
		}
	})

	t.Run("XDG_CONFIG_HOME beats the default", func(t *testing.T) {
		skipOnWindows(t)
		got, err := Dir("", env(map[string]string{"XDG_CONFIG_HOME": xdg, "HOME": home}))
		want := filepath.Join(xdg, "holebridge")
		if err != nil || got != want {
			t.Errorf("Dir with XDG_CONFIG_HOME: got %q (%s), want %q", got, report(err), want)
		}
	})

	t.Run("default is ~/.config/holebridge", func(t *testing.T) {
		skipOnWindows(t)
		got, err := Dir("", env(map[string]string{"HOME": home}))
		want := filepath.Join(home, ".config", "holebridge")
		if err != nil || got != want {
			t.Errorf("Dir with no settings: got %q (%s), want %q", got, report(err), want)
		}
	})

	if runtime.GOOS == "windows" {
		t.Run("default is %APPDATA%\\holebridge", func(t *testing.T) {
			appdata := filepath.Join(t.TempDir(), "AppData")
			got, err := Dir("", env(map[string]string{"APPDATA": appdata}))
			want := filepath.Join(appdata, "holebridge")
			if err != nil || got != want {
				t.Errorf("Dir with APPDATA: got %q (%s), want %q", got, report(err), want)
			}
		})
	}
}

// checkLANDefaults checks the LAN defaults: enabled, and two distinct unprivileged ports. The
// exact ports are chosen by the LAN spike (HoleBridge-dxe); pin them here when they are chosen.
func checkLANDefaults(t *testing.T, lan LANConfig) {
	t.Helper()
	if lan.Enabled == nil || !*lan.Enabled {
		t.Error("LAN.Enabled: want true by default")
	}
	for _, p := range []struct {
		name string
		port int
	}{{"discoveryPort", lan.DiscoveryPort}, {"port", lan.Port}} {
		if p.port < 1024 || p.port > 65535 {
			t.Errorf("LAN %s = %d: want a default port in 1024-65535", p.name, p.port)
		}
	}
	if lan.DiscoveryPort == lan.Port {
		t.Error("LAN discoveryPort and port: want two different defaults")
	}
}

// Case 8: a limits override replaces only that limit, and LAN defaults apply when lan is absent.
func TestLimitsOverrideAndLANDefaults(t *testing.T) {
	t.Run("one limit overridden, the rest default", func(t *testing.T) {
		dir := t.TempDir()
		writeHostJSON(t, dir, `{"services":{},"limits":{"sessionsPerKey":8}}`, 0o600)

		c, err := Load(dir)
		if err != nil {
			t.Fatalf("Load: want no error, got %s", report(err))
		}
		want := wantLimits()
		want.SessionsPerKey = 8
		if c.Limits != want {
			t.Error("Limits: want sessionsPerKey 8 and every other limit at its default")
		}
	})

	t.Run("lan absent gives the LAN defaults", func(t *testing.T) {
		dir := t.TempDir()
		writeHostJSON(t, dir, `{"services":{}}`, 0o600)

		c, err := Load(dir)
		if err != nil {
			t.Fatalf("Load: want no error, got %s", report(err))
		}
		checkLANDefaults(t, c.LAN)
	})

	t.Run("lan.enabled false is kept", func(t *testing.T) {
		dir := t.TempDir()
		writeHostJSON(t, dir, `{"services":{},"lan":{"enabled":false}}`, 0o600)

		c, err := Load(dir)
		if err != nil {
			t.Fatalf("Load: want no error, got %s", report(err))
		}
		if c.LAN.Enabled == nil || *c.LAN.Enabled {
			t.Error("LAN.Enabled: want false, as written")
		}
	})
}

// Invalid files return HB-CONFIG-INVALID. Each body is a whole host.json, written with mode 0600.
func TestLoadRejectsInvalidFiles(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"not JSON", `{"services":`},
		{"bad service name", `{"services":{"Web":{"target":"127.0.0.1:8080"}}}`},
		{"missing target", `{"services":{"web":{}}}`},
		{"bad target", `{"services":{"web":{"target":"x:y"}}}`},
		{"unknown kind", `{"services":{"web":{"target":"127.0.0.1:8080","kind":"ftp"}}}`},
		{"bad key", `{"key":"NOPE","services":{}}`},
		{"bad relay key", `{"relay":"NOPE","services":{}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeHostJSON(t, dir, tc.body, 0o600)
			if _, err := Load(dir); errCode(err) != "HB-CONFIG-INVALID" {
				t.Errorf("Load: want HB-CONFIG-INVALID, got %s", report(err))
			}
		})
	}
}

// Edge cases beyond the eight: a misspelt limit or trailing data would otherwise be ignored
// silently, and a negative idle or an out-of-range LAN port is a bad value.
func TestLoadRejectsMoreInvalidFiles(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"misspelt limit name", `{"services":{},"limits":{"sessionPerKey":8}}`},
		{"data after the object", `{"services":{}} {}`},
		{"negative idle", `{"services":{"ssh":{"target":"127.0.0.1:22","idle":"-1s"}}}`},
		{"LAN port above 65535", `{"services":{},"lan":{"port":70000}}`},
		{"LAN discovery port zero", `{"services":{},"lan":{"discoveryPort":0}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeHostJSON(t, dir, tc.body, 0o600)
			if _, err := Load(dir); errCode(err) != "HB-CONFIG-INVALID" {
				t.Errorf("Load: want HB-CONFIG-INVALID, got %s", report(err))
			}
		})
	}
}

// Load of a missing host.json returns the file error, so callers can test for fs.ErrNotExist.
func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(t.TempDir()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Load of a missing host.json: want fs.ErrNotExist, got %s", report(err))
	}
}

// Save refuses a config that Load would reject, and writes nothing.
func TestSaveRejectsInvalidConfig(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new")
	c := &Config{
		Services: map[string]Service{"Web": {Target: "127.0.0.1:8080"}},
		LAN:      LANConfig{DiscoveryPort: 27420, Port: 27421},
	}
	if err := Save(dir, c); errCode(err) != "HB-CONFIG-INVALID" {
		t.Fatalf("Save: want HB-CONFIG-INVALID, got %s", report(err))
	}
	if _, err := os.Stat(filepath.Join(dir, "host.json")); !os.IsNotExist(err) {
		t.Error("Save wrote host.json for an invalid config")
	}
}

// Duration writes the short form that cli.md shows: "8h", not "8h0m0s".
func TestDurationJSONShortForm(t *testing.T) {
	for _, tc := range []struct {
		d    Duration
		want string
	}{
		{Duration(8 * time.Hour), `"8h"`},
		{Duration(90 * time.Minute), `"1h30m"`},
		{Duration(60 * time.Second), `"1m"`},
		{Duration(10 * time.Second), `"10s"`},
		{0, `"0s"`},
	} {
		b, err := json.Marshal(tc.d)
		if err != nil || string(b) != tc.want {
			t.Errorf("Marshal(%v) = %s, %v; want %s", time.Duration(tc.d), b, err, tc.want)
		}
	}
}

// More targets that ParseTarget rejects: no port, unbracketed IPv6, an empty host, port zero.
func TestParseTargetMoreRejects(t *testing.T) {
	for _, in := range []string{"[::1]", "::1", ":8080", "0", "nas:"} {
		t.Run(fmt.Sprintf("%q", in), func(t *testing.T) {
			if _, _, err := ParseTarget(in); errCode(err) != "HB-CONFIG-INVALID" {
				t.Errorf("ParseTarget(%q): want HB-CONFIG-INVALID, got %s", in, report(err))
			}
		})
	}
}

// With no setting at all, Dir has no directory to return and says so.
func TestDirWithoutHome(t *testing.T) {
	if _, err := Dir("", func(string) string { return "" }); errCode(err) != "HB-CONFIG-INVALID" {
		t.Errorf("Dir with no settings: want HB-CONFIG-INVALID, got %s", report(err))
	}
}

// savedObjects holds the lan and limits objects of a host.json, field by field.
type savedObjects struct {
	LAN    map[string]json.RawMessage `json:"lan"`
	Limits map[string]json.RawMessage `json:"limits"`
}

// readSavedObjects returns the lan and limits objects of dir/host.json. The fields hold no key.
func readSavedObjects(t *testing.T, dir string) savedObjects {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "host.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f savedObjects
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("host.json is not JSON: %v", err)
	}
	return f
}

// Load then Save of the docs/cli.md example keeps limits as an empty object and lan without the
// provisional ports: a later release that changes a default reaches this host (HoleBridge-d89.12).
// The enabled flag stays, because the file holds it.
func TestSaveOfExampleKeepsDefaultsOut(t *testing.T) {
	dir := t.TempDir()
	writeHostJSON(t, dir, exampleHostJSON, 0o600)
	c, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: want no error, got %s", report(err))
	}
	if err := Save(dir, c); err != nil {
		t.Fatalf("Save: want no error, got %s", report(err))
	}

	f := readSavedObjects(t, dir)
	if len(f.Limits) != 0 {
		t.Errorf("limits: want an empty object, got %d fields", len(f.Limits))
	}
	for _, name := range []string{"discoveryPort", "port"} {
		if _, ok := f.LAN[name]; ok {
			t.Errorf("lan.%s written: a default is not an override", name)
		}
	}
	if string(f.LAN["enabled"]) != "true" {
		t.Errorf("lan.enabled: want true, as the file set it")
	}
}

// A config without LAN ports saves: the ports are left out, and Load fills in the defaults.
func TestSaveWithoutLANPortsOmitsThem(t *testing.T) {
	dir := t.TempDir()
	c := &Config{
		Key:      keys.Generate(),
		Services: map[string]Service{"web": {Target: "127.0.0.1:8080"}},
		Limits:   wantLimits(),
	}
	if err := Save(dir, c); err != nil {
		t.Fatalf("Save: want no error for a config without LAN ports, got %s", report(err))
	}

	f := readSavedObjects(t, dir)
	if len(f.LAN) != 0 || len(f.Limits) != 0 {
		t.Errorf("lan and limits: want empty objects, got %d and %d fields", len(f.LAN), len(f.Limits))
	}
	out, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: want no error, got %s", report(err))
	}
	checkLANDefaults(t, out.LAN)
	if out.Limits != wantLimits() {
		t.Error("Limits: want every limit at its default after Load")
	}
}

// A field the file sets to its default stays in the file: the owner wrote it, so a later release
// that changes the default must not move it. A port the file did not set stays out.
func TestSaveKeepsDefaultValuesTheFileSet(t *testing.T) {
	dir := t.TempDir()
	body := fmt.Sprintf(`{"services":{},"lan":{"discoveryPort":%d},"limits":{"sessionsPerKey":%d}}`,
		defaultDiscoveryPort, wantLimits().SessionsPerKey)
	writeHostJSON(t, dir, body, 0o600)
	c, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: want no error, got %s", report(err))
	}
	if err := Save(dir, c); err != nil {
		t.Fatalf("Save: want no error, got %s", report(err))
	}

	f := readSavedObjects(t, dir)
	if _, ok := f.LAN["discoveryPort"]; !ok {
		t.Error("lan.discoveryPort: want the value the file set kept")
	}
	if _, ok := f.LAN["port"]; ok {
		t.Error("lan.port written: the file did not set it")
	}
	if _, ok := f.Limits["sessionsPerKey"]; !ok {
		t.Error("limits.sessionsPerKey: want the value the file set kept")
	}
	if len(f.Limits) != 1 {
		t.Errorf("limits: want only sessionsPerKey, got %d fields", len(f.Limits))
	}
}

// A limit the caller changes is written, and the other limits stay out of the file.
func TestSaveWritesOnlyChangedLimit(t *testing.T) {
	dir := t.TempDir()
	writeHostJSON(t, dir, exampleHostJSON, 0o600)
	c, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: want no error, got %s", report(err))
	}
	c.Limits.SessionsPerKey = 8
	if err := Save(dir, c); err != nil {
		t.Fatalf("Save: want no error, got %s", report(err))
	}

	f := readSavedObjects(t, dir)
	if len(f.Limits) != 1 || string(f.Limits["sessionsPerKey"]) != "8" {
		t.Errorf("limits: want only sessionsPerKey 8, got %d fields", len(f.Limits))
	}
}
