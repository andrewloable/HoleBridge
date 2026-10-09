package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot is the repository root, three directories up from this package.
const repoRoot = "../../.."

// outputs are the files the generator writes, relative to the repository root.
var outputs = []string{
	"internal/errs/codes_gen.go",
	"app/engine/lib/errors.gen.js",
	"app/lib/src/errors.g.dart",
	"docs/errors.md",
}

// generate runs go generate ./internal/errs from the repository root, the documented way to regenerate.
func generate(t *testing.T) {
	t.Helper()
	cmd := exec.Command("go", "generate", "./internal/errs")
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go generate ./internal/errs failed: %v\n%s", err, out)
	}
}

// read returns the generated outputs, keyed by their path.
func read(t *testing.T) map[string]string {
	t.Helper()
	got := make(map[string]string, len(outputs))
	for _, p := range outputs {
		data, err := os.ReadFile(filepath.Join(repoRoot, p))
		if err != nil {
			t.Fatal(err)
		}
		got[p] = string(data)
	}
	return got
}

func TestGenerateTwiceGivesIdenticalFiles(t *testing.T) {
	generate(t)
	first := read(t)
	generate(t)
	second := read(t)
	for _, p := range outputs {
		if first[p] != second[p] {
			t.Errorf("%s differs between two runs of the generator", p)
		}
	}
}

func TestGenerateWritesEveryCodeToEachOutput(t *testing.T) {
	generate(t)
	out := read(t)
	data, err := os.ReadFile(filepath.Join(repoRoot, "spec", "errors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Codes []struct {
			Code string `json:"code"`
		} `json:"codes"`
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	js := out["app/engine/lib/errors.gen.js"]
	dart := out["app/lib/src/errors.g.dart"]
	doc := out["docs/errors.md"]
	for _, c := range spec.Codes {
		if !strings.Contains(js, c.Code) {
			t.Errorf("errors.gen.js does not contain %s", c.Code)
		}
		if !strings.Contains(dart, c.Code) {
			t.Errorf("errors.g.dart does not contain %s", c.Code)
		}
		if !strings.Contains(doc, "## "+c.Code) {
			t.Errorf("docs/errors.md has no %q heading", "## "+c.Code)
		}
	}
}
