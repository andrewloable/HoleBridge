// Package testvec loads the shared test vectors in spec/vectors for Go tests.
package testvec

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Load reads spec/vectors/<name> from the repo root and unmarshals it into v.
// It fails the test if the file is missing or does not decode into v.
func Load(t testing.TB, name string, v any) {
	t.Helper()
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("testvec: %v", err)
		return
	}
	path := filepath.Join(root, "spec", "vectors", name)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("testvec: missing vector file %s: generate it or check the name", path)
		return
	}
	if err != nil {
		t.Fatalf("testvec: %v", err)
		return
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("testvec: %s does not decode: %v", name, err)
	}
}

// Hex decodes s, which must be lowercase hex with an even length. It fails the test otherwise.
func Hex(t testing.TB, s string) []byte {
	t.Helper()
	if strings.ToLower(s) != s {
		t.Fatalf("testvec: hex must be lowercase")
		return nil
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("testvec: invalid hex: %v", err)
		return nil
	}
	return b
}

// repoRoot walks up from the working directory to the directory holding go.mod.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no go.mod found above the working directory")
		}
		dir = parent
	}
}
