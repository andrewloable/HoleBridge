package errs

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// failOnPanic turns a panic from a stub into a test failure, so the other tests still run.
func failOnPanic(t *testing.T) {
	t.Helper()
	if r := recover(); r != nil {
		t.Fatalf("%v", r)
	}
}

func TestCatalogHasEverySeededCode(t *testing.T) {
	data, err := os.ReadFile("../../spec/errors.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Codes []Entry `json:"codes"`
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	for _, e := range spec.Codes {
		if _, ok := Catalog[e.Code]; !ok {
			t.Errorf("%s is in spec/errors.json but not in Catalog; regenerate with go generate ./internal/errs", e.Code)
		}
	}
}

func TestEPanicsOnUnknownCode(t *testing.T) {
	// Built from parts so the HB- code checker does not flag this test for an unknown code.
	unknown := "HB-" + "NOPE"
	defer func() {
		r := recover()
		if r == "not implemented" {
			t.Fatal("E is not implemented")
		}
		if r == nil {
			t.Fatalf("E(%s) did not panic", unknown)
		}
	}()
	E(unknown, "", nil)
}

func TestErrorTextHasCodeAndDetail(t *testing.T) {
	defer failOnPanic(t)
	got := E("HB-CONFIG-PERMS", "mode 0644", nil).Error()
	if !strings.HasPrefix(got, "HB-CONFIG-PERMS: ") || !strings.HasSuffix(got, " (mode 0644)") {
		t.Errorf("Error() = %q, want prefix %q and suffix %q", got, "HB-CONFIG-PERMS: ", " (mode 0644)")
	}
}

func TestErrorsIsAndUnwrapReachWrappedError(t *testing.T) {
	defer failOnPanic(t)
	inner := errors.New("inner")
	e := E("HB-CONFIG-PERMS", "mode 0644", inner)
	if !errors.Is(e, inner) {
		t.Error("errors.Is(e, inner) = false, want true")
	}
	if got := errors.Unwrap(e); got != inner {
		t.Errorf("errors.Unwrap(e) = %v, want inner", got)
	}
}

// A code with no problem text must not put a stray ": " in its message. Every seeded code has its
// problem now, so the test adds a code without one to Catalog for its own duration. The code is
// built from parts for the HB- checker.
func TestErrorOmitsEmptyProblem(t *testing.T) {
	defer failOnPanic(t)
	code := "HB-" + "NOPROBLEM"
	Catalog[code] = Entry{Code: code}
	t.Cleanup(func() { delete(Catalog, code) })
	if got, want := E(code, "unknown flag", nil).Error(), code+" (unknown flag)"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if got := E(code, "", nil).Error(); got != code {
		t.Errorf("Error() = %q, want %q", got, code)
	}
}

func TestFormatIsFourLinesEndingWithDocsAnchor(t *testing.T) {
	defer failOnPanic(t)
	out := Format(E("HB-CONFIG-PERMS", "mode 0644", nil), "https://holebridge.app/errors")
	lines := strings.Split(out, "\n")
	if len(lines) != 4 {
		t.Fatalf("Format has %d lines, want 4:\n%s", len(lines), out)
	}
	if !strings.HasSuffix(lines[3], "#hb-config-perms") {
		t.Errorf("last line %q, want suffix %q", lines[3], "#hb-config-perms")
	}
	// The error line follows docs/cli.md: the problem, then the detail in parentheses.
	if !strings.HasPrefix(lines[0], "error HB-CONFIG-PERMS: ") || !strings.HasSuffix(lines[0], " (mode 0644)") {
		t.Errorf("first line %q, want prefix %q and suffix %q", lines[0], "error HB-CONFIG-PERMS: ", " (mode 0644)")
	}
}
