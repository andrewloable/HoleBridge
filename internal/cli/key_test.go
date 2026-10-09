package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/andrewloable/HoleBridge/internal/config"
	"github.com/andrewloable/HoleBridge/internal/keys"
	"github.com/andrewloable/HoleBridge/internal/links"
)

// keyHostJSON is a host.json holding a host key and no services. The key is the example from
// docs/security.md.
const keyHostJSON = `{"key":"7KQ-M4X-9TR"}`

// appKeyPattern is an application key as app.key holds it, without the newline: 64 lowercase hex
// digits.
var appKeyPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// mustHostKey returns the host key in dir/host.json in its normalized form, 9 symbols with no
// dashes. It fails the test unless host.json holds a valid key. Failure messages never show a key.
func mustHostKey(t *testing.T, dir string) string {
	t.Helper()
	k := loadHostJSON(t, dir).Key
	if n, err := keys.Normalize(k); err != nil || n != k {
		t.Fatalf("host.json has no valid host key")
	}
	return k
}

// storedKey returns the key field of host.json as it is written on disk, with its dashes.
func storedKey(t *testing.T, dir string) string {
	t.Helper()
	var f struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(readHostJSON(t, dir), &f); err != nil {
		t.Fatalf("host.json is not JSON: %v", err)
	}
	return f.Key
}

// readAppKeyFile returns the application key in dir/app.key without its newline. It fails the test
// unless the file holds 64 lowercase hex digits and a newline.
func readAppKeyFile(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "app.key"))
	if err != nil {
		t.Fatalf("app.key: %v", err)
	}
	s := strings.TrimSuffix(string(b), "\n")
	if !strings.HasSuffix(string(b), "\n") || !appKeyPattern.MatchString(s) {
		t.Fatalf("app.key does not hold 64 lowercase hex digits and a newline")
	}
	return s
}

// requireMode0600 fails the test when path is not mode 0600. Windows has no such modes, so the
// check passes there.
func requireMode0600(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("%s mode = %o, want 600", filepath.Base(path), got)
	}
}

// assertNoSecret fails the test if out holds any of the secrets. The message names the command and
// never the secret.
func assertNoSecret(t *testing.T, command, out string, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if s != "" && strings.Contains(out, s) {
			t.Errorf("%s prints a key it should not print", command)
		}
	}
}

// keyLine returns the value after "Key: " on the line of out that starts with it, or "" if none.
func keyLine(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(line, "Key: "); ok {
			return v
		}
	}
	return ""
}

// keyLinkIn returns the word of out that holds the key link, or "" if there is none.
func keyLinkIn(out string) string {
	for _, w := range strings.Fields(out) {
		if strings.Contains(w, "/k#") {
			return w
		}
	}
	return ""
}

// qrBlockLines counts the lines of out drawn with the half-block characters of qr.Terminal.
func qrBlockLines(out string) int {
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.ContainsAny(line, "█▀▄") {
			n++
		}
	}
	return n
}

// Case 1: key on an empty config creates a valid key, prints it with dashes, a QR block and a link
// ending in .<64 hex. The key command may print the host key (docs/cli.md, commands). The link
// carries the application key, so key also creates app.key on first use (docs/security.md, the
// application key), and the link must be the one links.KeyLink builds from both saved keys.
func TestKeyOnEmptyConfigCreatesKeyAndPrintsIt(t *testing.T) {
	dir := t.TempDir()
	code, stdout, stderr := run("--config", dir, "key")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	k := mustHostKey(t, dir)
	if got, want := storedKey(t, dir), keys.Format(k); got != want {
		t.Errorf("host.json key = %q, want the key with dashes", got)
	}
	requireMode0600(t, filepath.Join(dir, "host.json"))

	if !strings.Contains(stdout, "Key: "+keys.Format(k)) {
		t.Errorf("stdout does not print the host key as Key: XXX-XXX-XXX")
	}
	if n := qrBlockLines(stdout); n < 11 {
		t.Errorf("stdout has %d lines of QR block characters, want at least 11", n)
	}

	appKey, err := config.LoadAppKey(dir)
	if err != nil {
		t.Fatalf("key did not create app.key: %v", err)
	}
	requireMode0600(t, filepath.Join(dir, "app.key"))
	if got, want := keyLinkIn(stdout), links.KeyLink(links.DefaultBase, k, appKey); got != want {
		t.Errorf("link is not the one links.KeyLink builds from the saved keys")
	}
}

// Case 2: key twice prints the same key, and the second call leaves the stored key alone.
func TestKeyTwicePrintsSameKey(t *testing.T) {
	dir := t.TempDir()
	code, first, stderr := run("--config", dir, "key")
	if code != 0 {
		t.Fatalf("first key: exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	before := mustHostKey(t, dir)
	if keyLine(first) == "" {
		t.Fatalf("first key prints no Key: line")
	}

	code, second, stderr := run("--config", dir, "key")
	if code != 0 {
		t.Fatalf("second key: exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if keyLine(second) != keyLine(first) {
		t.Errorf("second key prints a different host key")
	}
	if after := mustHostKey(t, dir); after != before {
		t.Errorf("second key changed the stored host key")
	}
}

// Case 3: key --rotate replaces the host key in host.json. The docs do not say rotate prints the new
// key, so its output must not hold the old key or the new one. On a running host it says the old key
// stays active until holebridge host restarts (docs/cli.md, commands).
func TestKeyRotateChangesKeyInHostJSON(t *testing.T) {
	t.Run("stored key changes", func(t *testing.T) {
		dir := t.TempDir()
		writeHostJSON(t, dir, keyHostJSON)
		old := mustHostKey(t, dir)

		code, stdout, stderr := run("--config", dir, "key", "--rotate")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
		}
		now := mustHostKey(t, dir)
		if now == old {
			t.Errorf("key --rotate kept the old host key")
		}
		if got, want := storedKey(t, dir), keys.Format(now); got != want {
			t.Errorf("host.json key is not the new key with dashes")
		}
		assertNoSecret(t, "key --rotate", stdout+stderr, old, keys.Format(old), now, keys.Format(now))
	})

	t.Run("running host says the old key stays active until restart", func(t *testing.T) {
		dir := t.TempDir()
		writeHostJSON(t, dir, keyHostJSON)
		writeHostLock(t, dir)
		old := mustHostKey(t, dir)

		code, stdout, stderr := run("--config", dir, "key", "--rotate")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
		}
		lower := strings.ToLower(stdout)
		if !strings.Contains(lower, "old key") || !strings.Contains(lower, "restart") {
			t.Errorf("stdout does not say the old key stays active until restart")
		}
		assertNoSecret(t, "key --rotate", stdout+stderr, old, keys.Format(old), mustHostKey(t, dir))
	})

	t.Run("no host.lock, no note", func(t *testing.T) {
		dir := t.TempDir()
		writeHostJSON(t, dir, keyHostJSON)

		code, stdout, stderr := run("--config", dir, "key", "--rotate")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
		}
		if strings.Contains(strings.ToLower(stdout), "old key") {
			t.Errorf("stdout talks about the old key although no host is running")
		}
	})
}

// Case 4: key --set normalizes the key it is given and stores it with dashes. A key with a U is
// rejected with HB-KEY-INVALID, and host.json is left alone. The command prints no key.
func TestKeySetNormalizesAndRejectsBadKeys(t *testing.T) {
	t.Run("forgiving input is stored as 7KQ-M4X-9TR", func(t *testing.T) {
		dir := t.TempDir()
		code, stdout, stderr := run("--config", dir, "key", "--set", "7kq-m4x-9tr")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
		}
		if got := storedKey(t, dir); got != "7KQ-M4X-9TR" {
			t.Errorf("host.json key = %q, want %q", got, "7KQ-M4X-9TR")
		}
		assertNoSecret(t, "key --set", stdout+stderr, "7KQ-M4X-9TR", "7KQM4X9TR")
	})

	t.Run("U is rejected and host.json is untouched", func(t *testing.T) {
		dir := t.TempDir()
		writeHostJSON(t, dir, keyHostJSON)
		before := readHostJSON(t, dir)

		code, stdout, stderr := run("--config", dir, "key", "--set", "7KQ-M4X-9TU")
		if code != 1 {
			t.Errorf("exit code = %d, want 1 (stderr %q)", code, stderr)
		}
		if !strings.Contains(stderr, "HB-KEY-INVALID") {
			t.Errorf("stderr does not contain HB-KEY-INVALID: %q", stderr)
		}
		if after := readHostJSON(t, dir); !bytes.Equal(after, before) {
			t.Errorf("host.json changed after a rejected key")
		}
		assertNoSecret(t, "key --set", stdout+stderr, "7KQ-M4X-9TU", "7KQM4X9TU")
	})
}

// Case 5: app-key creates app.key with mode 0600 and prints its 64 hex digits, with a warning that
// it is a secret. A second call prints the same key (docs/cli.md, commands).
func TestAppKeyCreatesFileAndPrintsSameKey(t *testing.T) {
	dir := t.TempDir()
	code, first, stderr := run("--config", dir, "app-key")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	k := readAppKeyFile(t, dir)
	requireMode0600(t, filepath.Join(dir, "app.key"))
	if !strings.Contains(first, k) {
		t.Errorf("app-key does not print the application key in app.key")
	}
	if !strings.Contains(strings.ToLower(first+stderr), "secret") {
		t.Errorf("app-key does not warn that the application key is a secret")
	}

	code, second, stderr := run("--config", dir, "app-key")
	if code != 0 {
		t.Fatalf("second app-key: exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if !strings.Contains(second, k) {
		t.Errorf("second app-key does not print the same application key")
	}
	if got := readAppKeyFile(t, dir); got != k {
		t.Errorf("second app-key changed app.key")
	}
}

// Case 6: app-key --new fails when app.key exists and leaves the file alone. The failure names
// app.key, as service add names the service it refuses. No catalog code fits, so none is checked.
// The second subtest keeps the case from passing on a stub: --new must create the file when none
// exists.
func TestAppKeyNewFailsWhenAppKeyExists(t *testing.T) {
	t.Run("existing app.key is kept", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := config.CreateAppKey(dir); err != nil {
			t.Fatal(err)
		}
		before := readAppKeyFile(t, dir)

		code, stdout, stderr := run("--config", dir, "app-key", "--new")
		if code != 1 {
			t.Errorf("exit code = %d, want 1 (stderr %q)", code, stderr)
		}
		if !strings.Contains(stderr, "app.key") {
			t.Errorf("stderr does not name app.key: %q", stderr)
		}
		if got := readAppKeyFile(t, dir); got != before {
			t.Errorf("app-key --new replaced the existing app.key")
		}
		assertNoSecret(t, "app-key --new", stdout+stderr, before)
	})

	t.Run("creates app.key when none exists", func(t *testing.T) {
		dir := t.TempDir()
		code, _, stderr := run("--config", dir, "app-key", "--new")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
		}
		readAppKeyFile(t, dir)
		requireMode0600(t, filepath.Join(dir, "app.key"))
	})
}

// Case 7: app-key --rotate replaces app.key and prints that every app must be re-provisioned. The
// docs name no output for rotate, so the new key is not printed: apps get it from the link that key
// prints.
func TestAppKeyRotateReplacesKeyAndPrintsReprovisionNote(t *testing.T) {
	dir := t.TempDir()
	if _, err := config.CreateAppKey(dir); err != nil {
		t.Fatal(err)
	}
	old := readAppKeyFile(t, dir)

	code, stdout, stderr := run("--config", dir, "app-key", "--rotate")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	now := readAppKeyFile(t, dir)
	if now == old {
		t.Errorf("app-key --rotate kept the old application key")
	}
	requireMode0600(t, filepath.Join(dir, "app.key"))
	if !strings.Contains(strings.ToLower(stdout), "re-provision") {
		t.Errorf("stdout does not say every app must be re-provisioned")
	}
	assertNoSecret(t, "app-key --rotate", stdout+stderr, old, now)
}

// Case 8: app-key --set with 63 digits exits 1 with HB-APPKEY-INVALID and leaves app.key alone. The
// value is the first 63 digits of the current key, a copy cut short, which is the mistake the
// catalog describes.
func TestAppKeySetRejectsShortKey(t *testing.T) {
	dir := t.TempDir()
	if _, err := config.CreateAppKey(dir); err != nil {
		t.Fatal(err)
	}
	before := readAppKeyFile(t, dir)
	short := before[:63]

	code, stdout, stderr := run("--config", dir, "app-key", "--set", short)
	if code != 1 {
		t.Errorf("exit code = %d, want 1 (stderr %q)", code, stderr)
	}
	if !strings.Contains(stderr, "HB-APPKEY-INVALID") {
		t.Errorf("stderr does not contain HB-APPKEY-INVALID: %q", stderr)
	}
	if got := readAppKeyFile(t, dir); got != before {
		t.Errorf("app-key --set changed app.key after a rejected value")
	}
	assertNoSecret(t, "app-key --set", stdout+stderr, before, short)
}

// Not a listed case: app-key --set with 64 lowercase hex digits saves them, and a later app-key
// prints them (docs/security.md, the application key; design of HoleBridge-trk.5).
func TestAppKeySetSavesValidKey(t *testing.T) {
	dir := t.TempDir()
	want := strings.Repeat("ab", 32)

	code, stdout, stderr := run("--config", dir, "app-key", "--set", want)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if got := readAppKeyFile(t, dir); got != want {
		t.Errorf("app.key does not hold the value given to --set")
	}
	requireMode0600(t, filepath.Join(dir, "app.key"))
	assertNoSecret(t, "app-key --set", stdout+stderr, want)

	code, out, stderr := run("--config", dir, "app-key")
	if code != 0 {
		t.Fatalf("app-key: exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if !strings.Contains(out, want) {
		t.Errorf("app-key does not print the key set with --set")
	}
}
