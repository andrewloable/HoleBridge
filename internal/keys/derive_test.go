package keys

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
	"testing"

	"github.com/andrewloable/HoleBridge/internal/testvec"
)

// deriveVectors is the part of spec/vectors/key-derivation.json the derivation tests read. Every
// byte value is lowercase hex, and normalized is the hex of the ASCII key. No failure message
// prints a vector value or a derived value, so a failing test never leaks key material.
type deriveVectors struct {
	Keys      []deriveVector `json:"keys"`
	RelayKeys []deriveVector `json:"relayKeys"`
}

type deriveVector struct {
	AppKey      string            `json:"appKey"`
	Normalized  string            `json:"normalized"`
	Salt        string            `json:"salt"`
	Root        string            `json:"root"`
	Seeds       map[string]string `json:"seeds"`
	PublicKeys  map[string]string `json:"publicKeys"`
	LanProbeKey string            `json:"lanProbeKey"`
}

func loadDerivation(t *testing.T) deriveVectors {
	t.Helper()
	var v deriveVectors
	testvec.Load(t, "key-derivation.json", &v)
	if len(v.Keys) == 0 || len(v.RelayKeys) == 0 {
		t.Fatal("key-derivation.json needs both keys and relayKeys")
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

func vectorAppKey(t *testing.T, s string) [32]byte {
	t.Helper()
	var k [32]byte
	copy(k[:], vectorBytes(t, s, 32))
	return k
}

// checkStretch fails unless the salt and the root of the entry's key are the vector values.
func checkStretch(t *testing.T, saltLabel string, c deriveVector) {
	t.Helper()
	appKey := vectorAppKey(t, c.AppKey)
	salt, root := stretch(saltLabel, string(testvec.Hex(t, c.Normalized)), appKey)
	if !bytes.Equal(salt[:], vectorBytes(t, c.Salt, 16)) {
		t.Error("salt does not match the vector")
	}
	if !bytes.Equal(root[:], vectorBytes(t, c.Root, 32)) {
		t.Error("root does not match the vector")
	}
}

// checkPrivate fails unless priv has the vector seed and public key for role.
func checkPrivate(t *testing.T, role string, priv ed25519.PrivateKey, seed, pub string) {
	t.Helper()
	if len(priv) != ed25519.PrivateKeySize {
		t.Errorf("%s key is %d bytes, want %d", role, len(priv), ed25519.PrivateKeySize)
		return
	}
	if !bytes.Equal(priv.Seed(), vectorBytes(t, seed, 32)) {
		t.Errorf("%s seed does not match the vector", role)
	}
	if !bytes.Equal(priv.Public().(ed25519.PublicKey), vectorBytes(t, pub, 32)) {
		t.Errorf("%s public key does not match the vector", role)
	}
}

// checkLAN fails unless the LAN key is the vector seed and the Ed25519 public key made from it is
// the vector public key.
func checkLAN(t *testing.T, lan [32]byte, seed, pub string) {
	t.Helper()
	if !bytes.Equal(lan[:], vectorBytes(t, seed, 32)) {
		t.Error("lan key does not match the vector")
	}
	if !bytes.Equal(ed25519.NewKeyFromSeed(lan[:]).Public().(ed25519.PublicKey), vectorBytes(t, pub, 32)) {
		t.Error("lan public key does not match the vector")
	}
}

func TestDeriveMatchesVectors(t *testing.T) {
	v := loadDerivation(t)
	for i, c := range v.Keys {
		t.Run(fmt.Sprintf("key %d", i), func(t *testing.T) {
			defer failOnPanic(t)
			checkStretch(t, keySaltLabel, c)
			appKey := vectorAppKey(t, c.AppKey)
			d, err := Derive(string(testvec.Hex(t, c.Normalized)), appKey)
			if err != nil {
				t.Fatalf("Derive error = %v, want nil", err)
			}
			checkPrivate(t, "host", d.Host, c.Seeds["host"], c.PublicKeys["host"])
			checkPrivate(t, "client", d.Client, c.Seeds["client"], c.PublicKeys["client"])
			checkLAN(t, d.LAN.Reveal(), c.Seeds["lan"], c.PublicKeys["lan"])
			// The LAN probe key is the lan seed, the same value the engine checks (lanProbeKey).
			if lan := d.LAN.Reveal(); !bytes.Equal(lan[:], vectorBytes(t, c.LanProbeKey, 32)) {
				t.Error("LAN probe key does not match the vector lanProbeKey")
			}
		})
	}
}

func TestDeriveRelayMatchesVectors(t *testing.T) {
	v := loadDerivation(t)
	for i, c := range v.RelayKeys {
		t.Run(fmt.Sprintf("relay key %d", i), func(t *testing.T) {
			defer failOnPanic(t)
			checkStretch(t, relaySaltLabel, c)
			appKey := vectorAppKey(t, c.AppKey)
			d, err := DeriveRelay(string(testvec.Hex(t, c.Normalized)), appKey)
			if err != nil {
				t.Fatalf("DeriveRelay error = %v, want nil", err)
			}
			checkPrivate(t, "server", d.Server, c.Seeds["server"], c.PublicKeys["server"])
			checkPrivate(t, "member", d.Member, c.Seeds["member"], c.PublicKeys["member"])
		})
	}
}

func TestDifferentAppKeysGiveDifferentHostKeys(t *testing.T) {
	defer failOnPanic(t)
	const key = "7KQM4X9TR"
	var a, b [32]byte
	for i := range a {
		a[i] = byte(i)
		b[i] = 0xff
	}
	da, err := Derive(key, a)
	if err != nil {
		t.Fatalf("Derive with application key 0x00..0x1f error = %v, want nil", err)
	}
	db, err := Derive(key, b)
	if err != nil {
		t.Fatalf("Derive with application key 0xff..0xff error = %v, want nil", err)
	}
	if bytes.Equal(da.Host.Public().(ed25519.PublicKey), db.Host.Public().(ed25519.PublicKey)) {
		t.Error("two application keys give the same host public key for the same key")
	}
}

func TestDeriveIsDeterministic(t *testing.T) {
	defer failOnPanic(t)
	const key = "HJKMNPQRS"
	var appKey [32]byte
	for i := range appKey {
		appKey[i] = byte(i)
	}
	d1, err := Derive(key, appKey)
	if err != nil {
		t.Fatalf("Derive error = %v, want nil", err)
	}
	d2, err := Derive(key, appKey)
	if err != nil {
		t.Fatalf("second Derive error = %v, want nil", err)
	}
	if !bytes.Equal(d1.Host, d2.Host) || !bytes.Equal(d1.Client, d2.Client) || d1.LAN.Reveal() != d2.LAN.Reveal() {
		t.Error("two Derive calls with the same input give different keys")
	}
	r1, err := DeriveRelay(key, appKey)
	if err != nil {
		t.Fatalf("DeriveRelay error = %v, want nil", err)
	}
	r2, err := DeriveRelay(key, appKey)
	if err != nil {
		t.Fatalf("second DeriveRelay error = %v, want nil", err)
	}
	if !bytes.Equal(r1.Server, r2.Server) || !bytes.Equal(r1.Member, r2.Member) {
		t.Error("two DeriveRelay calls with the same input give different keys")
	}
}

func TestDeriveNormalizesItsInput(t *testing.T) {
	defer failOnPanic(t)
	var appKey [32]byte
	canonical, err := Derive("7KQM4X9TR", appKey)
	if err != nil {
		t.Fatalf("Derive error = %v, want nil", err)
	}
	spelled, err := Derive("7kqm-4x9tr", appKey)
	if err != nil {
		t.Fatalf("Derive of a lowercase key with a dash error = %v, want nil", err)
	}
	if !bytes.Equal(canonical.Host, spelled.Host) || !bytes.Equal(canonical.Client, spelled.Client) {
		t.Error("a lowercase key with a dash gives different keys from the canonical key")
	}
}

func TestDeriveRejectsInvalidKeys(t *testing.T) {
	defer failOnPanic(t)
	var appKey [32]byte
	// Too short, and U, which is not in the alphabet.
	for _, key := range []string{"7KQM4X9", "7KQM4X9TU"} {
		if _, err := Derive(key, appKey); err == nil {
			t.Errorf("Derive accepted an invalid key of %d symbols", len(key))
		}
		if _, err := DeriveRelay(key, appKey); err == nil {
			t.Errorf("DeriveRelay accepted an invalid key of %d symbols", len(key))
		}
	}
}
