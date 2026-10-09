package keys

import (
	"errors"
	"strings"
	"testing"

	"github.com/andrewloable/HoleBridge/internal/errs"
	"github.com/andrewloable/HoleBridge/internal/testvec"
)

// failOnPanic turns a panic from a stub into a test failure, so the other tests still run.
func failOnPanic(t *testing.T) {
	t.Helper()
	if r := recover(); r != nil {
		t.Fatalf("%v", r)
	}
}

// keyVectors is the part of spec/vectors/key.json the normalization tests read.
type keyVectors struct {
	Valid []struct {
		Input      string `json:"input"`
		Normalized string `json:"normalized"`
	} `json:"valid"`
	Invalid []struct {
		Input string `json:"input"`
		Error string `json:"error"`
	} `json:"invalid"`
}

// derivationVectors is the part of spec/vectors/key-derivation.json the key tests read. Its
// normalized field is the hex of the ASCII key.
type derivationVectors struct {
	Keys []struct {
		Normalized string `json:"normalized"`
	} `json:"keys"`
}

func TestNormalizeValidVectors(t *testing.T) {
	var v keyVectors
	testvec.Load(t, "key.json", &v)
	if len(v.Valid) == 0 {
		t.Fatal("key.json has no valid cases")
	}
	for _, c := range v.Valid {
		got, err := Normalize(c.Input)
		if err != nil {
			t.Errorf("Normalize(%q) error = %v, want %q", c.Input, err, c.Normalized)
			continue
		}
		if got != c.Normalized {
			t.Errorf("Normalize(%q) = %q, want %q", c.Input, got, c.Normalized)
		}
	}
}

func TestNormalizeInvalidVectorsReturnReason(t *testing.T) {
	var v keyVectors
	testvec.Load(t, "key.json", &v)
	if len(v.Invalid) == 0 {
		t.Fatal("key.json has no invalid cases")
	}
	for _, c := range v.Invalid {
		got, err := Normalize(c.Input)
		var ke *errs.Error
		if !errors.As(err, &ke) {
			t.Errorf("Normalize(%q) = %q, %v; want an HB-KEY-INVALID error with reason %q", c.Input, got, err, c.Error)
			continue
		}
		if ke.Code != "HB-KEY-INVALID" {
			t.Errorf("Normalize(%q) code = %q, want HB-KEY-INVALID", c.Input, ke.Code)
		}
		if ke.Detail != c.Error {
			t.Errorf("Normalize(%q) reason = %q, want %q", c.Input, ke.Detail, c.Error)
		}
	}
}

// Length is checked before character, and a character outside the BMP is one symbol, as in the
// engine and Dart suites.
func TestNormalizeFaultOrderAndSymbolCount(t *testing.T) {
	cases := []struct {
		name, input, reason string
	}{
		{"both faults: length first", "7KQM4X9TU9", "length"},
		{"one character outside the BMP", "7KQM4X9T\U0001F600", "character"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Normalize(c.input)
			var ke *errs.Error
			if !errors.As(err, &ke) || ke.Detail != c.reason {
				t.Errorf("Normalize(%q) = %q, %v; want reason %q", c.input, got, err, c.reason)
			}
		})
	}
}

func TestDerivationVectorKeysAreCanonical(t *testing.T) {
	var v derivationVectors
	testvec.Load(t, "key-derivation.json", &v)
	if len(v.Keys) == 0 {
		t.Fatal("key-derivation.json has no keys")
	}
	for i, c := range v.Keys {
		k := string(testvec.Hex(t, c.Normalized))
		got, err := Normalize(k)
		if err != nil || got != k {
			t.Errorf("derivation key %d: Normalize(%q) = %q, %v; want the key unchanged", i, k, got, err)
		}
	}
}

func TestFormatGroupsInThrees(t *testing.T) {
	defer failOnPanic(t)
	if got, want := Format("7KQM4X9TR"), "7KQ-M4X-9TR"; got != want {
		t.Errorf("Format(%q) = %q, want %q", "7KQM4X9TR", got, want)
	}
}

func TestGenerateIsNineSymbolsFromAlphabet(t *testing.T) {
	defer failOnPanic(t)
	for i := 0; i < 10000; i++ {
		k := Generate()
		if len(k) != 9 {
			t.Fatalf("Generate() returned %d symbols, want 9", len(k))
		}
		for j := 0; j < len(k); j++ {
			if strings.IndexByte(Alphabet, k[j]) < 0 {
				t.Fatalf("Generate() symbol %d is not in Alphabet", j)
			}
		}
		got, err := Normalize(k)
		if err != nil || got != k {
			t.Fatalf("Normalize(Generate()) did not return the key unchanged: %v", err)
		}
	}
}

func TestGenerateHitsEverySymbol(t *testing.T) {
	defer failOnPanic(t)
	seen := make(map[byte]int)
	for i := 0; i < 10000; i++ {
		k := Generate()
		for j := 0; j < len(k); j++ {
			seen[k[j]]++
		}
	}
	for i := 0; i < len(Alphabet); i++ {
		if seen[Alphabet[i]] == 0 {
			t.Errorf("symbol %q never drawn in 10000 keys", Alphabet[i])
		}
	}
}

func TestAppKeyFormatParseRoundTrip(t *testing.T) {
	defer failOnPanic(t)
	k := NewAppKey()
	s := FormatAppKey(k)
	if len(s) != 64 || strings.ToLower(s) != s {
		t.Fatalf("FormatAppKey() is %d characters, want 64 lowercase hex digits", len(s))
	}
	got, err := ParseAppKey(s)
	if err != nil {
		t.Fatalf("ParseAppKey(FormatAppKey(k)) error = %v, want nil", err)
	}
	if got != k {
		t.Error("ParseAppKey(FormatAppKey(k)) does not return k")
	}
}

func TestParseAppKeyRejectsBadInput(t *testing.T) {
	defer failOnPanic(t)
	cases := []struct {
		name, input string
	}{
		{"63 digits", strings.Repeat("0", 63)},
		{"65 digits", strings.Repeat("0", 65)},
		{"non-hex character", strings.Repeat("0", 31) + "g" + strings.Repeat("0", 32)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer failOnPanic(t)
			_, err := ParseAppKey(c.input)
			var ke *errs.Error
			if !errors.As(err, &ke) || ke.Code != "HB-APPKEY-INVALID" {
				t.Errorf("ParseAppKey(%s) error = %v, want HB-APPKEY-INVALID", c.name, err)
			}
		})
	}
}

func TestParseAppKeyAcceptsUppercaseHex(t *testing.T) {
	defer failOnPanic(t)
	var want [32]byte
	for i := range want {
		want[i] = 0xab
	}
	for _, c := range []struct{ name, input string }{
		{"lowercase", strings.Repeat("ab", 32)},
		{"uppercase", strings.Repeat("AB", 32)},
	} {
		got, err := ParseAppKey(c.input)
		if err != nil {
			t.Errorf("ParseAppKey(%s hex) error = %v, want nil", c.name, err)
			continue
		}
		if got != want {
			t.Errorf("ParseAppKey(%s hex) does not return the 0xab key", c.name)
		}
	}
}
