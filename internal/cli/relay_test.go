package cli

import (
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/andrewloable/HoleBridge/internal/config"
	"github.com/andrewloable/HoleBridge/internal/keys"
	"github.com/andrewloable/HoleBridge/internal/log"
	"github.com/andrewloable/HoleBridge/internal/relay"
	"github.com/andrewloable/HoleBridge/internal/testvec"
	"github.com/andrewloable/HoleBridge/pears/dhtrpc"
	"github.com/andrewloable/HoleBridge/pears/hyperdht"
)

// testnetSize is the number of DHT nodes in a testnet, as in the internal/relay tests.
const testnetSize = 10

// relayVectors is the part of spec/vectors/key-derivation.json these tests read. Every byte value is
// lowercase hex; normalized is the hex of the ASCII relay key. The vector values are test values only.
type relayVectors struct {
	RelayKeys []relayVector `json:"relayKeys"`
}

type relayVector struct {
	AppKey     string `json:"appKey"`
	Normalized string `json:"normalized"`
}

// vectors returns the normalized relay key of the first relay vector, and the application keys of the
// first and second vectors. The relay key is the same in both; the application keys differ.
func vectors(t *testing.T) (relayKey string, appA, appB [32]byte) {
	t.Helper()
	var v relayVectors
	testvec.Load(t, "key-derivation.json", &v)
	if len(v.RelayKeys) < 2 {
		t.Fatal("key-derivation.json needs two relayKeys entries")
	}
	relayKey = string(testvec.Hex(t, v.RelayKeys[0].Normalized))
	if n, err := keys.Normalize(relayKey); err != nil || n != relayKey {
		t.Fatal("the vector relay key is not a normalized 9-symbol key")
	}
	return relayKey, vectorAppKey(t, v.RelayKeys[0].AppKey), vectorAppKey(t, v.RelayKeys[1].AppKey)
}

// vectorAppKey decodes a vector application key, which must be 32 bytes.
func vectorAppKey(t *testing.T, s string) [32]byte {
	t.Helper()
	b := testvec.Hex(t, s)
	if len(b) != 32 {
		t.Fatalf("vector application key is %d bytes, want 32", len(b))
	}
	var k [32]byte
	copy(k[:], b)
	return k
}

// writeRelayKey writes path as a relay key file: the key as XXX-XXX-XXX and a newline, mode 0600.
func writeRelayKey(t *testing.T, path, relayKey string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(keys.Format(relayKey)+"\n"), 0o600); err != nil {
		t.Fatalf("relay key file: %v", err)
	}
}

// writeAppKey stores k as the app.key of dir, through the config package.
func writeAppKey(t *testing.T, dir string, k [32]byte) {
	t.Helper()
	if err := config.SaveAppKey(dir, k); err != nil {
		t.Fatalf("app.key: %v", err)
	}
}

// lineAfter returns the rest of the first line of out that starts with prefix, and whether there is one.
func lineAfter(out, prefix string) (string, bool) {
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			return rest, true
		}
	}
	return "", false
}

// fakeRelay stands in for a running relay. It has the public key and NAT state that the relay command
// prints.
type fakeRelay struct {
	pub [32]byte
	nat dhtrpc.NATInfo
}

func (f fakeRelay) PublicKey() [32]byte { return f.pub }
func (f fakeRelay) NAT() dhtrpc.NATInfo { return f.nat }

// startCall is one call of startRelay: the keys it was given.
type startCall struct {
	relayKey string
	appKey   [32]byte
}

// fakeStart replaces startRelay with a function that records its keys and returns r. The default is
// restored when the test ends. The result lists the calls made.
func fakeStart(t *testing.T, r fakeRelay) *[]startCall {
	t.Helper()
	var calls []startCall
	prev := startRelay
	startRelay = func(ctx context.Context, relayKey string, appKey [32]byte, logger *slog.Logger) (relayStatus, error) {
		calls = append(calls, startCall{relayKey: relayKey, appKey: appKey})
		return r, nil
	}
	t.Cleanup(func() { startRelay = prev })
	return &calls
}

// stopRelayAtOnce replaces relayContext with a context that is already done, so that a foreground
// relay command returns once it has printed. The default is restored when the test ends.
func stopRelayAtOnce(t *testing.T) {
	t.Helper()
	prev := relayContext
	relayContext = func() (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx, cancel
	}
	t.Cleanup(func() { relayContext = prev })
}

// useTestnet points the relay and relay check commands at the nodes of tn. The default is restored
// when the test ends.
func useTestnet(t *testing.T, tn *hyperdht.Testnet) {
	t.Helper()
	prev := bootstrap
	bootstrap = tn.Bootstrap
	t.Cleanup(func() { bootstrap = prev })
}

// startTestRelay runs internal/relay on a new DHT node of tn, under relayKey and appKey. The relay
// stops when the test ends.
func startTestRelay(t *testing.T, tn *hyperdht.Testnet, relayKey string, appKey [32]byte) {
	t.Helper()
	d, err := hyperdht.New(hyperdht.Config{Bootstrap: tn.Bootstrap})
	if err != nil {
		t.Fatalf("hyperdht.New: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if _, err := relay.Run(ctx, relayKey, appKey, d, log.New(io.Discard, slog.LevelInfo)); err != nil {
		t.Fatalf("relay.Run: %v", err)
	}
}

// Case 1: relay --new-key writes relay.key with a valid key, mode 0600, and prints the key once with
// the path it was saved to. A second run fails, leaves the file alone and does not print the key.
func TestRelayNewKeyWritesKeyOnceAndRefusesSecondRun(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.key")

	code, stdout, stderr := run("--config", dir, "relay", "--new-key")
	if code != 0 {
		t.Fatalf("first run exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("relay.key: %v", err)
	}
	if !bytes.HasSuffix(b, []byte("\n")) {
		t.Fatal("relay.key does not end with a newline")
	}
	s := strings.TrimSuffix(string(b), "\n")
	norm, err := keys.Normalize(s)
	if err != nil || keys.Format(norm) != s {
		t.Fatal("relay.key does not hold a valid relay key written as XXX-XXX-XXX")
	}
	requireMode0600(t, path)

	if n := strings.Count(stdout, s); n != 1 {
		t.Errorf("the relay key is printed %d times, want once", n)
	}
	line, ok := lineAfter(stdout, "Relay key: ")
	if !ok {
		t.Fatalf("stdout has no Relay key line: %q", stdout)
	}
	if want := s + " (saved to " + path + ", mode 0600)"; strings.Join(strings.Fields(line), " ") != want {
		t.Errorf("Relay key line = %q, want it to end with %q", line, want)
	}
	assertNoSecret(t, "relay --new-key", stderr, s, norm)

	code, stdout, stderr = run("--config", dir, "relay", "--new-key")
	if code == 0 {
		t.Error("second run exit code = 0, want a failure")
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, b) {
		t.Error("the second run changed relay.key")
	}
	assertNoSecret(t, "relay --new-key (second run)", stdout+stderr, s, norm)
}

// Case 2: relay without relay.key exits 1 with a message that names relay --new-key. The command
// starts no relay and creates no relay.key.
func TestRelayWithoutRelayKeyExitsOneNamingNewKey(t *testing.T) {
	_, appKey, _ := vectors(t)
	dir := t.TempDir()
	writeAppKey(t, dir, appKey)
	calls := fakeStart(t, fakeRelay{})

	code, stdout, stderr := run("--config", dir, "relay")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (stderr %q)", code, stderr)
	}
	if !strings.Contains(stderr, "HB-RELAY-KEY-MISSING") {
		t.Errorf("stderr does not carry HB-RELAY-KEY-MISSING: %q", stderr)
	}
	if !strings.Contains(stderr, "relay --new-key") {
		t.Errorf("stderr does not name relay --new-key: %q", stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if len(*calls) != 0 {
		t.Errorf("the relay was started %d times, want 0", len(*calls))
	}
	if _, err := os.Stat(filepath.Join(dir, "relay.key")); !os.IsNotExist(err) {
		t.Error("relay created relay.key")
	}
	assertNoSecret(t, "relay", stdout+stderr, hex.EncodeToString(appKey[:]))
}

// Edge case: a relay.key that group or others can read is refused, as app.key is, with HB-RELAY-KEY-PERMS.
// Neither relay nor relay check starts anything, and the key is not printed. relay check refuses before it
// starts a DHT node, so no test touches the network.
func TestRelayKeyReadableByOthersIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits")
	}
	relayKey, appKey, _ := vectors(t)
	dir := t.TempDir()
	writeAppKey(t, dir, appKey)
	path := filepath.Join(dir, "relay.key")
	writeRelayKey(t, path, relayKey)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	calls := fakeStart(t, fakeRelay{})

	code, stdout, stderr := run("--config", dir, "relay")
	if code != 1 {
		t.Errorf("relay exit code = %d, want 1 (stderr %q)", code, stderr)
	}
	if !strings.Contains(stderr, "HB-RELAY-KEY-PERMS") {
		t.Errorf("relay stderr does not carry HB-RELAY-KEY-PERMS: %q", stderr)
	}
	if len(*calls) != 0 {
		t.Errorf("the relay was started %d times, want 0", len(*calls))
	}
	assertNoSecret(t, "relay", stdout+stderr, relayKey, keys.Format(relayKey), hex.EncodeToString(appKey[:]))

	code, stdout, stderr = run("--config", dir, "relay", "check", path)
	if code != 1 {
		t.Errorf("relay check exit code = %d, want 1 (stderr %q)", code, stderr)
	}
	if !strings.Contains(stderr, "HB-RELAY-KEY-PERMS") {
		t.Errorf("relay check stderr does not carry HB-RELAY-KEY-PERMS: %q", stderr)
	}
	assertNoSecret(t, "relay check", stdout+stderr, relayKey, keys.Format(relayKey), hex.EncodeToString(appKey[:]))
}

// Case 3: relay runs the relay with the keys in relay.key and app.key, and prints the public key line
// and the NAT line. The fake relay stands in for internal/relay. No relay key or application key is
// printed.
func TestRelayPrintsPublicKeyAndNATLine(t *testing.T) {
	relayKey, appKey, _ := vectors(t)
	dir := t.TempDir()
	writeRelayKey(t, filepath.Join(dir, "relay.key"), relayKey)
	writeAppKey(t, dir, appKey)
	var pub [32]byte
	for i := range pub {
		pub[i] = byte(i*7 + 1)
	}
	calls := fakeStart(t, fakeRelay{
		pub: pub,
		nat: dhtrpc.NATInfo{Host: "203.0.113.7", Port: 49737},
	})
	stopRelayAtOnce(t)

	code, stdout, stderr := run("--config", dir, "relay")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if len(*calls) != 1 {
		t.Fatalf("the relay was started %d times, want 1", len(*calls))
	}
	if got := (*calls)[0]; got.relayKey != relayKey || got.appKey != appKey {
		t.Error("the relay was not started with the key in relay.key and the application key in app.key")
	}
	if got, ok := lineAfter(stdout, "Relay public key "); !ok || got != hex.EncodeToString(pub[:]) {
		t.Errorf("Relay public key line = %q, want %q", got, hex.EncodeToString(pub[:]))
	}
	const wantNAT = "203.0.113.7:49737 firewalled=false randomized=false"
	if got, ok := lineAfter(stdout, "Public UDP "); !ok || got != wantNAT {
		t.Errorf("Public UDP line = %q, want %q", got, wantNAT)
	}
	assertNoSecret(t, "relay", stdout+stderr, relayKey, keys.Format(relayKey), hex.EncodeToString(appKey[:]))
}

// Case 4: relay check against a relay on a Go testnet prints member: admitted and stranger: refused,
// and exits 0. The relay is internal/relay on the testnet, and the check runs with the same app.key.
func TestRelayCheckAdmitsMemberAndRefusesStranger(t *testing.T) {
	tn := hyperdht.NewTestnet(t, testnetSize)
	useTestnet(t, tn)
	relayKey, appKey, _ := vectors(t)
	startTestRelay(t, tn, relayKey, appKey)

	dir := t.TempDir()
	writeAppKey(t, dir, appKey)
	keyFile := filepath.Join(t.TempDir(), "relay.key")
	writeRelayKey(t, keyFile, relayKey)

	code, stdout, stderr := run("--config", dir, "relay", "check", keyFile)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stdout %q, stderr %q)", code, stdout, stderr)
	}
	for _, want := range []string{"member: admitted", "stranger: refused"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout does not contain %q: %q", want, stdout)
		}
	}
	assertNoSecret(t, "relay check", stdout+stderr, relayKey, keys.Format(relayKey), hex.EncodeToString(appKey[:]))
}

// Case 5: relay check against a relay with another app key exits 1. This machine's app.key differs
// from the relay's, so the member is not admitted. Both results are still printed.
func TestRelayCheckWithAnotherAppKeyExitsOne(t *testing.T) {
	tn := hyperdht.NewTestnet(t, testnetSize)
	useTestnet(t, tn)
	relayKey, appA, appB := vectors(t)
	startTestRelay(t, tn, relayKey, appA)

	dir := t.TempDir()
	writeAppKey(t, dir, appB)
	keyFile := filepath.Join(t.TempDir(), "relay.key")
	writeRelayKey(t, keyFile, relayKey)

	code, stdout, stderr := run("--config", dir, "relay", "check", keyFile)
	if code != 1 {
		t.Errorf("exit code = %d, want 1 (stderr %q)", code, stderr)
	}
	if strings.Contains(stdout, "member: admitted") {
		t.Error("the member was admitted by a relay with another application key")
	}
	for _, prefix := range []string{"member: ", "stranger: "} {
		if _, ok := lineAfter(stdout, prefix); !ok {
			t.Errorf("stdout has no %q line: %q", prefix, stdout)
		}
	}
	assertNoSecret(t, "relay check", stdout+stderr,
		relayKey, keys.Format(relayKey), hex.EncodeToString(appA[:]), hex.EncodeToString(appB[:]))
}

// Edge case: --bootstrap takes host:port nodes separated by commas, and refuses anything else with a usage
// error, so a typo never starts a node on the wrong network.
func TestParseBootstrapAcceptsNodesAndRefusesOthers(t *testing.T) {
	got, err := parseBootstrap("192.0.2.1:49737,relay.example:5000")
	if err != nil || len(got) != 2 || got[0] != "192.0.2.1:49737" || got[1] != "relay.example:5000" {
		t.Fatalf("parseBootstrap of two nodes = %q, %v", got, err)
	}
	for _, bad := range []string{"", "192.0.2.1", ":49737", "192.0.2.1:0", "192.0.2.1:70000", "192.0.2.1:49737,,192.0.2.2:1", "192.0.2.1:x"} {
		if _, err := parseBootstrap(bad); err == nil {
			t.Errorf("parseBootstrap(%q) accepted a bad node", bad)
		}
	}
}

// Edge case: relay check with --bootstrap uses the nodes of the flag, not the default list. The default list is
// the public network, which a test must not reach, so this run passes only if the flag is honoured.
func TestRelayCheckBootstrapFlagReplacesDefault(t *testing.T) {
	tn := hyperdht.NewTestnet(t, testnetSize)
	relayKey, appKey, _ := vectors(t)
	startTestRelay(t, tn, relayKey, appKey)

	dir := t.TempDir()
	writeAppKey(t, dir, appKey)
	keyFile := filepath.Join(t.TempDir(), "relay.key")
	writeRelayKey(t, keyFile, relayKey)

	code, stdout, stderr := run("--config", dir, "relay", "check", keyFile, "--bootstrap", strings.Join(tn.Bootstrap, ","))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stdout %q, stderr %q)", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "member: admitted") {
		t.Errorf("stdout does not admit the member: %q", stdout)
	}
}
