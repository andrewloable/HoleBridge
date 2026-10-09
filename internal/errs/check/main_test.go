package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// repoRoot is the repository root, three directories up from this package.
const repoRoot = "../../.."

// unknown is a code that is not in the catalog. It is built from two parts so that this file does
// not trip the checker when it scans the repository.
const unknown = "HB-" + "MADE-UP"

// catalog is the set of codes the tests treat as known.
var catalog = map[string]bool{"HB-CONFIG-PERMS": true, "HB-USAGE": true}

// write creates each file under root, with its parent directories.
func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// exitCode runs the built checker on dir and returns its exit code and combined output.
func exitCode(t *testing.T, bin, dir string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, dir)
	out, err := cmd.CombinedOutput()
	if cmd.ProcessState == nil {
		t.Fatalf("%s did not run: %v", bin, err)
	}
	return cmd.ProcessState.ExitCode(), string(out)
}

func TestFilesWithOnlyCatalogCodesHaveNoFindings(t *testing.T) {
	root := t.TempDir()
	write(t, root, map[string]string{
		"a.go": "package a\n\nvar a = \"HB-CONFIG-PERMS\"\n",
		"b.js": "const b = 'HB-USAGE';\n",
	})
	got, err := Check(root, catalog)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %d findings, want none: %+v", len(got), got)
	}
}

func TestUnknownCodeGivesOneFindingWithFileAndLine(t *testing.T) {
	root := t.TempDir()
	write(t, root, map[string]string{
		"pkg/x.go": "package pkg\n\n// " + unknown + " is not in the catalog.\n",
	})
	got, err := Check(root, catalog)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1: %+v", len(got), got)
	}
	want := Finding{File: "pkg/x.go", Line: 3, Code: unknown}
	if got[0] != want {
		t.Errorf("finding = %+v, want %+v", got[0], want)
	}
}

func TestGeneratedFilesAndNodeModulesAreIgnored(t *testing.T) {
	root := t.TempDir()
	write(t, root, map[string]string{
		"internal/errs/codes_gen.go":           "package errs\n\n// " + unknown + "\n",
		"app/engine/lib/errors.gen.js":         "// " + unknown + "\n",
		"app/lib/src/errors.g.dart":            "// " + unknown + "\n",
		"app/engine/node_modules/dep/index.js": "// " + unknown + "\n",
		"main.go":                              "package main\n\n// " + unknown + "\n",
	})
	got, err := Check(root, catalog)
	if err != nil {
		t.Fatal(err)
	}
	// main.go is not skipped, so the one finding must be in it and nowhere else.
	if len(got) != 1 || got[0].File != "main.go" {
		t.Errorf("got %+v, want one finding in main.go only", got)
	}
}

func TestCommandExitsOneOnFindingsAndZeroOnNone(t *testing.T) {
	name := "check"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bin := filepath.Join(t.TempDir(), name)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-o", bin, "./internal/errs/check")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./internal/errs/check failed: %v\n%s", err, out)
	}

	t.Run("findings", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, map[string]string{"x.go": "package x\n\nvar x = \"" + unknown + "\"\n"})
		code, out := exitCode(t, bin, root)
		if code != 1 {
			t.Errorf("exit code %d, want 1; output:\n%s", code, out)
		}
	})

	t.Run("clean", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, map[string]string{"x.go": "package x\n\nvar x = \"HB-USAGE\"\n"})
		code, out := exitCode(t, bin, root)
		if code != 0 {
			t.Errorf("exit code %d, want 0; output:\n%s", code, out)
		}
	})
}
