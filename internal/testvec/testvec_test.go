package testvec

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// recordingTB stands in for a *testing.T so a failing Load can be observed without failing the
// test run. Only the methods Load and Hex call are overridden.
type recordingTB struct {
	testing.TB
	msgs []string
}

func (r *recordingTB) Helper() {}

func (r *recordingTB) Fatalf(format string, args ...any) {
	r.msgs = append(r.msgs, fmt.Sprintf(format, args...))
}

func TestLoadExampleVector(t *testing.T) {
	var v struct {
		Hello string `json:"hello"`
		Bytes string `json:"bytes"`
	}
	Load(t, "example.json", &v)
	if v.Hello != "world" {
		t.Errorf("hello = %q, want %q", v.Hello, "world")
	}
	if v.Bytes != "00ff" {
		t.Errorf("bytes = %q, want %q", v.Bytes, "00ff")
	}
	if got, want := Hex(t, v.Bytes), []byte{0x00, 0xff}; !bytes.Equal(got, want) {
		t.Errorf("Hex(bytes) = %x, want %x", got, want)
	}
}

func TestLoadMissingFileFailsWithClearMessage(t *testing.T) {
	var fake recordingTB
	var v any
	Load(&fake, "no-such-vector.json", &v)
	if len(fake.msgs) != 1 {
		t.Fatalf("got %d failures, want 1: %q", len(fake.msgs), fake.msgs)
	}
	if !strings.Contains(fake.msgs[0], "missing vector file") || !strings.Contains(fake.msgs[0], "no-such-vector.json") {
		t.Errorf("message %q does not name the missing file", fake.msgs[0])
	}
}

func TestHexRejectsBadInput(t *testing.T) {
	for _, s := range []string{"0A", "abc", "zz"} {
		var fake recordingTB
		if got := Hex(&fake, s); got != nil || len(fake.msgs) != 1 {
			t.Errorf("Hex(%q) = %x with %d failures, want nil and 1 failure", s, got, len(fake.msgs))
		}
	}
}
