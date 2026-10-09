package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/andrewloable/HoleBridge/internal/keys"
)

// writeAppKeyFile writes body as app.key in dir, then sets its mode exactly (the umask would
// otherwise change a 0644 request).
func writeAppKeyFile(t *testing.T, dir, body string, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(dir, "app.key")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// Case 1: CreateAppKey writes 64 lowercase hex digits and a newline with mode 0600, and refuses
// to overwrite an existing app.key. The key itself is never printed.
func TestCreateAppKeyWritesHexFileAndRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	k, err := CreateAppKey(dir)
	if err != nil {
		t.Fatalf("CreateAppKey: want no error, got %s", report(err))
	}

	raw, err := os.ReadFile(filepath.Join(dir, "app.key"))
	if err != nil {
		t.Fatal(err)
	}
	if want := keys.FormatAppKey(k) + "\n"; string(raw) != want {
		t.Error("app.key: want 64 lowercase hex digits and a newline, and the key the call returned")
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dir, "app.key"))
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("app.key mode: got %o, want 600", mode)
		}
	}

	if _, err := CreateAppKey(dir); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second CreateAppKey: want an error wrapping fs.ErrExist, got %s", report(err))
	}
	after, err := os.ReadFile(filepath.Join(dir, "app.key"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(raw) {
		t.Error("second CreateAppKey changed the existing app.key")
	}
}

// Case 2: LoadAppKey on a missing file returns HB-APPKEY-MISSING.
func TestLoadAppKeyMissingFile(t *testing.T) {
	_, err := LoadAppKey(t.TempDir())
	if code := errCode(err); code != "HB-APPKEY-MISSING" {
		t.Fatalf("LoadAppKey: want HB-APPKEY-MISSING, got %s", report(err))
	}
}

// Case 2: an app.key that group or others can read returns HB-APPKEY-PERMS on POSIX, even when
// its content is a valid key, and the error text does not carry the key.
func TestLoadAppKeyRejectsGroupOrOtherReadable(t *testing.T) {
	skipOnWindows(t)
	body := keys.FormatAppKey(keys.NewAppKey()) + "\n"
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604} {
		t.Run(fmt.Sprintf("mode%o", mode), func(t *testing.T) {
			dir := t.TempDir()
			writeAppKeyFile(t, dir, body, mode)
			_, err := LoadAppKey(dir)
			if code := errCode(err); code != "HB-APPKEY-PERMS" {
				t.Fatalf("LoadAppKey: want HB-APPKEY-PERMS, got %s", report(err))
			}
			if strings.Contains(err.Error(), strings.TrimSpace(body)) {
				t.Error("the error text contains the application key")
			}
		})
	}
}

// Case 2: malformed app.key content returns HB-APPKEY-INVALID, and the error text does not carry
// the content.
func TestLoadAppKeyRejectsBadHex(t *testing.T) {
	good := keys.FormatAppKey(keys.NewAppKey())
	cases := []struct{ name, body string }{
		{"empty file", ""},
		{"63 digits", good[:63] + "\n"},
		{"65 digits", good + "0\n"},
		{"non-hex character", "g" + good[1:] + "\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeAppKeyFile(t, dir, c.body, 0o600)
			_, err := LoadAppKey(dir)
			if code := errCode(err); code != "HB-APPKEY-INVALID" {
				t.Fatalf("LoadAppKey: want HB-APPKEY-INVALID, got %s", report(err))
			}
			if s := strings.TrimSpace(c.body); s != "" && strings.Contains(err.Error(), s) {
				t.Error("the error text contains the file content")
			}
		})
	}
}

// Case 3: SaveAppKey then LoadAppKey round-trips. The file is 64 lowercase hex digits and a
// newline, mode 0600, with no temp file left behind. A second SaveAppKey replaces the key, as
// app-key --set and --rotate need.
func TestSaveAppKeyThenLoadAppKeyRoundTrips(t *testing.T) {
	dir := t.TempDir()
	in := keys.NewAppKey()
	if err := SaveAppKey(dir, in); err != nil {
		t.Fatalf("SaveAppKey: want no error, got %s", report(err))
	}

	raw, err := os.ReadFile(filepath.Join(dir, "app.key"))
	if err != nil {
		t.Fatal(err)
	}
	if want := keys.FormatAppKey(in) + "\n"; string(raw) != want {
		t.Error("app.key: want 64 lowercase hex digits and a newline")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dir, "app.key"))
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("app.key mode: got %o, want 600", mode)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "app.key" {
		t.Errorf("directory after SaveAppKey holds %d entries, want only app.key (no temp file left)", len(entries))
	}

	out, err := LoadAppKey(dir)
	if err != nil {
		t.Fatalf("LoadAppKey: want no error, got %s", report(err))
	}
	if out != in {
		t.Error("LoadAppKey after SaveAppKey: the key changed (values not printed)")
	}

	next := keys.NewAppKey()
	if err := SaveAppKey(dir, next); err != nil {
		t.Fatalf("second SaveAppKey: want no error, got %s", report(err))
	}
	out, err = LoadAppKey(dir)
	if err != nil {
		t.Fatalf("LoadAppKey after second SaveAppKey: want no error, got %s", report(err))
	}
	if out != next {
		t.Error("LoadAppKey after a second SaveAppKey: want the new key (values not printed)")
	}
}

// Case 4: the state file round-trips the detected kinds, and sits in the config directory as
// kinds.json.
func TestStateRoundTripsDetectedKinds(t *testing.T) {
	dir := t.TempDir()
	in := State{Kinds: map[string]string{"web": "https", "ssh": "tcp", "dns": "udp"}}
	if err := SaveState(dir, in); err != nil {
		t.Fatalf("SaveState: want no error, got %s", report(err))
	}
	if _, err := os.Stat(filepath.Join(dir, "kinds.json")); err != nil {
		t.Errorf("SaveState: want kinds.json in the config directory, got %v", err)
	}
	out, err := LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState: want no error, got %s", report(err))
	}
	if !reflect.DeepEqual(out, in) {
		t.Error("LoadState after SaveState: the detected kinds changed")
	}
}

// Case 4: a missing kinds.json loads as an empty State, with no error.
func TestLoadStateMissingFileIsEmpty(t *testing.T) {
	s, err := LoadState(t.TempDir())
	if err != nil {
		t.Fatalf("LoadState: want no error for a missing kinds.json, got %s", report(err))
	}
	if len(s.Kinds) != 0 {
		t.Errorf("LoadState on a missing kinds.json: want no kinds, got %d", len(s.Kinds))
	}
}

// Edge case: LoadAppKey takes the key with or without its trailing newline, and rejects capital
// letters, as the HB-APPKEY-INVALID cause in spec/errors.json says.
func TestLoadAppKeyNewlineOptionalLowercaseOnly(t *testing.T) {
	k := keys.NewAppKey()
	s := keys.FormatAppKey(k)
	dir := t.TempDir()

	writeAppKeyFile(t, dir, s, 0o600)
	got, err := LoadAppKey(dir)
	if err != nil {
		t.Fatalf("key without a trailing newline: want no error, got %s", report(err))
	}
	if got != k {
		t.Error("key without a trailing newline: the key changed (values not printed)")
	}

	writeAppKeyFile(t, dir, strings.ToUpper(s)+"\n", 0o600)
	_, err = LoadAppKey(dir)
	if code := errCode(err); code != "HB-APPKEY-INVALID" {
		t.Fatalf("upper-case hex: want HB-APPKEY-INVALID, got %s", report(err))
	}
}

// Edge case: a kinds.json that is not JSON returns HB-CONFIG-INVALID.
func TestLoadStateRejectsNonJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kinds.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(dir); errCode(err) != "HB-CONFIG-INVALID" {
		t.Fatalf("LoadState: want HB-CONFIG-INVALID, got %s", report(err))
	}
}
