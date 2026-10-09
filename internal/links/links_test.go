package links

import (
	"strings"
	"testing"

	"github.com/andrewloable/HoleBridge/internal/testvec"
)

// failOnPanic turns a panic from a stub into a test failure, so the other tests still run.
func failOnPanic(t *testing.T) {
	t.Helper()
	if r := recover(); r != nil {
		t.Fatalf("%v", r)
	}
}

// linkVectors is the part of spec/vectors/links.json the key-link tests read. Failure messages name
// an entry by its index only, so no key or application key reaches the test log.
type linkVectors struct {
	Base     string `json:"base"`
	KeyLinks []struct {
		Key    string `json:"key"`
		AppKey string `json:"appKey"`
		Link   string `json:"link"`
	} `json:"keyLinks"`
	Parse []struct {
		Link         string `json:"link"`
		ExpectKey    string `json:"expectKey"`
		ExpectAppKey string `json:"expectAppKey"`
	} `json:"parse"`
	Invalid []struct {
		Link   string `json:"link"`
		Reason string `json:"reason"`
	} `json:"invalid"`
}

// appKeyVector decodes a 64-digit lowercase hex application key from the vector file.
func appKeyVector(t *testing.T, s string) [32]byte {
	t.Helper()
	b := testvec.Hex(t, s)
	if len(b) != 32 {
		t.Fatalf("testvec: application key is %d bytes, want 32", len(b))
	}
	var k [32]byte
	copy(k[:], b)
	return k
}

func TestKeyLinkEqualsEveryKeyLinkVector(t *testing.T) {
	defer failOnPanic(t)
	var v linkVectors
	testvec.Load(t, "links.json", &v)
	if len(v.KeyLinks) == 0 {
		t.Fatal("links.json has no keyLinks")
	}
	if DefaultBase != v.Base {
		t.Errorf("DefaultBase is not the vector base")
	}
	for i, c := range v.KeyLinks {
		got := KeyLink(v.Base, c.Key, appKeyVector(t, c.AppKey))
		if got != c.Link {
			t.Errorf("keyLinks[%d]: KeyLink did not return the vector link", i)
		}
	}
}

func TestParseKeyLinkGivesExpectedValues(t *testing.T) {
	defer failOnPanic(t)
	var v linkVectors
	testvec.Load(t, "links.json", &v)
	if len(v.Parse) == 0 {
		t.Fatal("links.json has no parse entries")
	}
	for i, c := range v.KeyLinks {
		key, appKey, err := ParseKeyLink(c.Link)
		if err != nil {
			t.Errorf("keyLinks[%d]: ParseKeyLink error = %v, want nil", i, err)
			continue
		}
		if key != c.Key {
			t.Errorf("keyLinks[%d]: ParseKeyLink returned a different key", i)
		}
		if appKey != appKeyVector(t, c.AppKey) {
			t.Errorf("keyLinks[%d]: ParseKeyLink returned a different application key", i)
		}
	}
	for i, c := range v.Parse {
		key, appKey, err := ParseKeyLink(c.Link)
		if err != nil {
			t.Errorf("parse[%d]: ParseKeyLink error = %v, want nil", i, err)
			continue
		}
		if key != c.ExpectKey {
			t.Errorf("parse[%d]: ParseKeyLink returned a different key", i)
		}
		if appKey != appKeyVector(t, c.ExpectAppKey) {
			t.Errorf("parse[%d]: ParseKeyLink returned a different application key", i)
		}
	}
}

func TestParseKeyLinkRejectsEveryInvalidVector(t *testing.T) {
	defer failOnPanic(t)
	var v linkVectors
	testvec.Load(t, "links.json", &v)
	if len(v.Invalid) == 0 {
		t.Fatal("links.json has no invalid entries")
	}
	// The list also holds handoff links (/h). They are not key links, so ParseKeyLink must reject
	// them too.
	for i, c := range v.Invalid {
		if _, _, err := ParseKeyLink(c.Link); err == nil {
			t.Errorf("invalid[%d] (%s): ParseKeyLink returned no error", i, c.Reason)
		}
	}
}

// Application keys in links are lowercase hex only (docs/security.md, the application key). keys
// ParseAppKey accepts uppercase, so the link parser must reject it on its own.
func TestParseKeyLinkRejectsUppercaseAppKey(t *testing.T) {
	defer failOnPanic(t)
	link := DefaultBase + "/k#7KQM4X9TR." + strings.Repeat("F", 64)
	if _, _, err := ParseKeyLink(link); err == nil {
		t.Error("ParseKeyLink accepted an uppercase application key, want an error")
	}
}
