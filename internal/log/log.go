// Package log is the HoleBridge logger. Secret wraps the values that must never reach a log line,
// an error message or JSON output: every formatting path prints "[redacted]".
package log

import (
	"fmt"
	"io"
	"log/slog"
)

// Secret wraps a value that must never be printed. Reveal is the only way to read it.
// The value sits behind a pointer: fmt prints unexported fields by reflection and skips their
// methods, so a struct holding a Secret in an unexported field would otherwise print the value.
type Secret[T any] struct{ v *T }

// NewSecret wraps v.
func NewSecret[T any](v T) Secret[T] {
	return Secret[T]{v: &v}
}

// Reveal returns the wrapped value. It is the only way to read the value.
func (s Secret[T]) Reveal() T {
	if s.v == nil {
		var zero T
		return zero
	}
	return *s.v
}

// String returns "[redacted]".
func (s Secret[T]) String() string {
	return "[redacted]"
}

// GoString returns "[redacted]", so %#v does not print the value.
func (s Secret[T]) GoString() string {
	return "[redacted]"
}

// Format writes "[redacted]" for every verb.
func (s Secret[T]) Format(f fmt.State, verb rune) {
	io.WriteString(f, "[redacted]")
}

// LogValue makes slog print "[redacted]" for the value.
func (s Secret[T]) LogValue() slog.Value {
	return slog.StringValue("[redacted]")
}

// MarshalJSON encodes the value as the JSON string "[redacted]".
func (s Secret[T]) MarshalJSON() ([]byte, error) {
	return []byte(`"[redacted]"`), nil
}

// New returns a logger that writes text records at level or above to w.
func New(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
}

// ParseLevel maps "error", "warn", "info" or "debug" to its slog level.
func ParseLevel(s string) (slog.Level, error) {
	switch s {
	case "error":
		return slog.LevelError, nil
	case "warn":
		return slog.LevelWarn, nil
	case "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	}
	return 0, fmt.Errorf("unknown log level %q", s)
}
