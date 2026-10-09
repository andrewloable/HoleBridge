package lan

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/internal/testvec"
)

// lanVectors is spec/vectors/lan-probe.json, written by spec/gen/lan-probe.js. Every byte value is
// lowercase hex. lanKey is the LAN probe key the vector is MACed with. No failure message prints a
// vector key, nonce, probe or reply, so a failing test never leaks key material.
type lanVectors struct {
	Probes  []lanProbeVector   `json:"probes"`
	Replies []lanReplyVector   `json:"replies"`
	Invalid []lanInvalidVector `json:"invalid"`
}

type lanProbeVector struct {
	LanKey      string `json:"lanKey"`
	Nonce       string `json:"nonce"`
	TimestampMs uint64 `json:"timestampMs"`
	Hex         string `json:"hex"`
}

type lanReplyVector struct {
	LanKey string `json:"lanKey"`
	Nonce  string `json:"nonce"`
	Port   uint16 `json:"port"`
	Hex    string `json:"hex"`
}

type lanInvalidVector struct {
	LanKey string `json:"lanKey"`
	Hex    string `json:"hex"`
	Reason string `json:"reason"`
}

// loadLANVectors reads the vector file and fails when it is too small to prove anything: the
// acceptance of HoleBridge-hb5.18.1 is at least 3 probes, 3 replies and 6 invalid cases.
func loadLANVectors(t *testing.T) lanVectors {
	t.Helper()
	var v lanVectors
	testvec.Load(t, "lan-probe.json", &v)
	if len(v.Probes) < 3 || len(v.Replies) < 3 || len(v.Invalid) < 6 {
		t.Fatalf("lan-probe.json holds %d probes, %d replies and %d invalid cases; want at least 3, 3 and 6",
			len(v.Probes), len(v.Replies), len(v.Invalid))
	}
	return v
}

// vectorBytes decodes a vector value that must be exactly n bytes.
func vectorBytes(t *testing.T, s string, n int) []byte {
	t.Helper()
	b := testvec.Hex(t, s)
	if len(b) != n {
		t.Fatalf("vector value is %d bytes, want %d", len(b), n)
	}
	return b
}

func vectorKey(t *testing.T, s string) [32]byte {
	t.Helper()
	var k [32]byte
	copy(k[:], vectorBytes(t, s, len(k)))
	return k
}

func vectorNonce(t *testing.T, s string) [16]byte {
	t.Helper()
	var n [16]byte
	copy(n[:], vectorBytes(t, s, len(n)))
	return n
}

// failOnPanic turns the panic of a stub into a failure of the test that defers it, so the other
// tests still run. Defer it first in every test that calls a stub which panics.
func failOnPanic(t *testing.T) {
	t.Helper()
	if r := recover(); r != nil {
		t.Fatalf("%v", r)
	}
}

// Case 1: EncodeProbe and EncodeReply give the vector bytes for every vector.
func TestEncodeProbeAndReplyMatchVectors(t *testing.T) {
	defer failOnPanic(t)
	v := loadLANVectors(t)
	for i, p := range v.Probes {
		got := EncodeProbe(vectorKey(t, p.LanKey), vectorNonce(t, p.Nonce), p.TimestampMs)
		if !bytes.Equal(got[:], testvec.Hex(t, p.Hex)) {
			t.Errorf("probe %d: EncodeProbe is not the vector bytes", i)
		}
	}
	for i, r := range v.Replies {
		got := EncodeReply(vectorKey(t, r.LanKey), vectorNonce(t, r.Nonce), r.Port)
		if !bytes.Equal(got[:], testvec.Hex(t, r.Hex)) {
			t.Errorf("reply %d: EncodeReply is not the vector bytes", i)
		}
	}
}

// Case 2: VerifyProbe accepts each vector probe at its timestamp and rejects every invalid entry.
func TestVerifyProbeVectors(t *testing.T) {
	defer failOnPanic(t)
	v := loadLANVectors(t)
	for i, p := range v.Probes {
		now := time.UnixMilli(int64(p.TimestampMs))
		nonce, ok := VerifyProbe(vectorKey(t, p.LanKey), testvec.Hex(t, p.Hex), now)
		if !ok {
			t.Errorf("probe %d: VerifyProbe rejects a vector probe at its timestamp", i)
			continue
		}
		if nonce != vectorNonce(t, p.Nonce) {
			t.Errorf("probe %d: VerifyProbe returns a different nonce", i)
		}
	}
	for i, bad := range v.Invalid {
		raw := testvec.Hex(t, bad.Hex)
		if len(raw) < 32 {
			t.Fatalf("invalid case %d is %d bytes, too short to hold a timestamp", i, len(raw))
		}
		// Each invalid entry carries the timestamp of the valid probe it was built from, so the
		// case is judged at that time and only its own defect can make it fail.
		ts := binary.LittleEndian.Uint64(raw[24:32])
		if _, ok := VerifyProbe(vectorKey(t, bad.LanKey), raw, time.UnixMilli(int64(ts))); ok {
			t.Errorf("invalid case %d (%s): VerifyProbe accepts it", i, bad.Reason)
		}
	}
}

// Case 3: a probe 25 h old or 25 h in the future is rejected; 23 h either way is accepted.
func TestProbeFreshnessWindow(t *testing.T) {
	defer failOnPanic(t)
	v := loadLANVectors(t)
	key := vectorKey(t, v.Probes[0].LanKey)
	nonce := vectorNonce(t, v.Probes[0].Nonce)
	now := time.UnixMilli(int64(v.Probes[0].TimestampMs))
	cases := []struct {
		name string
		off  time.Duration
		want bool
	}{
		{"25 h old", -25 * time.Hour, false},
		{"25 h in the future", 25 * time.Hour, false},
		{"23 h old", -23 * time.Hour, true},
		{"23 h in the future", 23 * time.Hour, true},
	}
	for _, c := range cases {
		probe := EncodeProbe(key, nonce, uint64(now.Add(c.off).UnixMilli()))
		if _, ok := VerifyProbe(key, probe[:], now); ok != c.want {
			t.Errorf("probe %s: VerifyProbe ok = %v, want %v", c.name, ok, c.want)
		}
	}
}

// VerifyReply accepts each vector reply under its nonce and rejects it under another nonce.
func TestVerifyReplyVectors(t *testing.T) {
	defer failOnPanic(t)
	v := loadLANVectors(t)
	for i, r := range v.Replies {
		key := vectorKey(t, r.LanKey)
		nonce := vectorNonce(t, r.Nonce)
		port, ok := VerifyReply(key, testvec.Hex(t, r.Hex), nonce)
		if !ok {
			t.Errorf("reply %d: VerifyReply rejects a vector reply under its own nonce", i)
			continue
		}
		if port != r.Port {
			t.Errorf("reply %d: VerifyReply port = %d, want %d", i, port, r.Port)
		}
		other := nonce
		other[0] ^= 0xff
		if _, ok := VerifyReply(key, testvec.Hex(t, r.Hex), other); ok {
			t.Errorf("reply %d: VerifyReply accepts it under another nonce", i)
		}
	}
}
