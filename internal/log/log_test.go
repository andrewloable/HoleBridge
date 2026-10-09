package log

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
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

func TestSecretHidesEveryVerb(t *testing.T) {
	defer failOnPanic(t)
	s := NewSecret([]byte{1, 2, 3})
	for _, format := range []string{"%v", "%s", "%+v", "%#v", "%d", "%x", "%q"} {
		if got := fmt.Sprintf(format, s); got != "[redacted]" {
			t.Errorf("Sprintf(%q) = %q, want %q", format, got, "[redacted]")
		}
	}
}

func TestSecretFieldInStructIsRedacted(t *testing.T) {
	defer failOnPanic(t)
	type login struct {
		User string
		Key  Secret[string]
	}
	got := fmt.Sprintf("%+v", login{User: "alice", Key: NewSecret("secret-value")})
	want := "{User:alice Key:[redacted]}"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSlogAttrWithSecretIsRedacted(t *testing.T) {
	defer failOnPanic(t)
	var buf bytes.Buffer
	New(&buf, slog.LevelDebug).Info("connect", "key", NewSecret("secret-value"))
	out := buf.String()
	if !strings.Contains(out, "key=[redacted]") {
		t.Errorf("output %q does not contain key=[redacted]", out)
	}
	if strings.Contains(out, "secret-value") {
		t.Errorf("output %q leaks the secret value", out)
	}
}

func TestMarshalJSONRedactsSecretField(t *testing.T) {
	defer failOnPanic(t)
	type record struct {
		User string         `json:"user"`
		Key  Secret[string] `json:"key"`
	}
	b, err := json.Marshal(record{User: "alice", Key: NewSecret("secret-value")})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"user":"alice","key":"[redacted]"}`
	if string(b) != want {
		t.Errorf("got %s, want %s", b, want)
	}
}

func TestRevealReturnsOriginalValue(t *testing.T) {
	defer failOnPanic(t)
	want := []byte{1, 2, 3}
	got := NewSecret(want).Reveal()
	if !bytes.Equal(got, want) {
		t.Errorf("Reveal() = %v, want %v", got, want)
	}
}

func TestParseLevelAcceptsNamesAndRejectsUnknown(t *testing.T) {
	defer failOnPanic(t)
	for _, tc := range []struct {
		in   string
		want slog.Level
	}{
		{"error", slog.LevelError},
		{"warn", slog.LevelWarn},
		{"info", slog.LevelInfo},
		{"debug", slog.LevelDebug},
	} {
		got, err := ParseLevel(tc.in)
		if err != nil {
			t.Errorf("ParseLevel(%q): %v", tc.in, err)
		} else if got != tc.want {
			t.Errorf("ParseLevel(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	if _, err := ParseLevel("verbose"); err == nil {
		t.Error("ParseLevel(\"verbose\") returned no error, want one")
	}
}

func TestNewDropsRecordsBelowLevel(t *testing.T) {
	defer failOnPanic(t)
	var buf bytes.Buffer
	lg := New(&buf, slog.LevelWarn)
	lg.Info("info-record")
	lg.Warn("warn-record")
	out := buf.String()
	if strings.Contains(out, "info-record") {
		t.Errorf("Info record written at level warn: %q", out)
	}
	if !strings.Contains(out, "warn-record") {
		t.Errorf("Warn record missing: %q", out)
	}
}

func TestSecretNeverLeaksThroughUnexportedFieldOrPointerVerb(t *testing.T) {
	defer failOnPanic(t)
	type holder struct {
		name string
		key  Secret[string]
	}
	const raw = "secret-value"
	h := holder{name: "alice", key: NewSecret(raw)}
	var buf bytes.Buffer
	New(&buf, slog.LevelDebug).Info("cfg", "h", h)
	for _, got := range []string{
		fmt.Sprintf("%v", h),
		fmt.Sprintf("%+v", h),
		fmt.Sprintf("%#v", h),
		fmt.Sprintf("%v", &h),
		fmt.Sprintf("%p", h.key),
		buf.String(),
	} {
		if strings.Contains(got, raw) {
			t.Errorf("output %q leaks the secret value", got)
		}
	}
}

func TestZeroSecretIsSafe(t *testing.T) {
	defer failOnPanic(t)
	var s Secret[string]
	if got := s.Reveal(); got != "" {
		t.Errorf("zero Reveal() = %q, want empty", got)
	}
	if got := fmt.Sprint(s); got != "[redacted]" {
		t.Errorf("zero Sprint = %q, want %q", got, "[redacted]")
	}
}
