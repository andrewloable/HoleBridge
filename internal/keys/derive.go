package keys

import (
	"crypto/ed25519"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/blake2b"

	"github.com/andrewloable/HoleBridge/internal/log"
)

// OPS and MEM are the Argon2id cost of the derivation (docs/security.md, Derivation): time OPS and
// memory MEM. They are numbers in code, never a library default, so a library change cannot move
// the derivation. MEM is in bytes, as in security.md and spec/vectors/key-derivation.json; the
// argon2 package takes KiB, so the implementation passes MEM / 1024.
const (
	OPS = 3
	MEM = 64 << 20 // 64 MiB: 67108864 bytes
)

// Salt labels of the two derivations (docs/security.md, Derivation and Relay keys).
const (
	keySaltLabel   = "holebridge key v1 salt"
	relaySaltLabel = "holebridge relay v1 salt"
)

// Derived is the key material of one host key: the host and client key pairs and the LAN probe
// key. The LAN key is a 32-byte secret, not a key pair; its public key is derived from it.
type Derived struct {
	Host, Client ed25519.PrivateKey
	LAN          log.Secret[[32]byte]
}

// RelayDerived is the key material of one relay key: the server and member key pairs.
type RelayDerived struct {
	Server, Member ed25519.PrivateKey
}

// Derive turns a normalized 9-symbol key and the application key into the host key material.
// The derivation is docs/security.md, Derivation. The key is passed through Normalize first, so an
// invalid key is an error and never a different derivation; a normalized key is unchanged by it.
func Derive(normalized string, appKey [32]byte) (Derived, error) {
	key, err := Normalize(normalized)
	if err != nil {
		return Derived{}, err
	}
	_, root := stretch(keySaltLabel, key, appKey)
	return Derived{
		Host:   ed25519.NewKeyFromSeed(seed("holebridge key v1 host", root, appKey)),
		Client: ed25519.NewKeyFromSeed(seed("holebridge key v1 client", root, appKey)),
		LAN:    log.NewSecret([32]byte(seed("holebridge key v1 lan", root, appKey))),
	}, nil
}

// DeriveRelay turns a normalized 9-symbol relay key and the application key into the relay key
// material. The derivation is docs/security.md, Relay keys. Like Derive, it normalizes its input
// first and returns an error for an invalid key.
func DeriveRelay(normalized string, appKey [32]byte) (RelayDerived, error) {
	key, err := Normalize(normalized)
	if err != nil {
		return RelayDerived{}, err
	}
	_, root := stretch(relaySaltLabel, key, appKey)
	return RelayDerived{
		Server: ed25519.NewKeyFromSeed(seed("holebridge relay v1 server", root, appKey)),
		Member: ed25519.NewKeyFromSeed(seed("holebridge relay v1 member", root, appKey)),
	}, nil
}

// stretch returns the salt and the Argon2id root of a derivation. saltLabel is keySaltLabel for
// host keys and relaySaltLabel for relay keys. The salt is BLAKE2b-128 of the label and appKey;
// the root is Argon2id13 of normalized with that salt, OPS and MEM, 32 bytes.
func stretch(saltLabel, normalized string, appKey [32]byte) (salt [16]byte, root [32]byte) {
	h, _ := blake2b.New(len(salt), nil) // unkeyed; 16 bytes is a valid size
	h.Write([]byte(saltLabel))
	h.Write(appKey[:])
	copy(salt[:], h.Sum(nil))
	root = [32]byte(argon2.IDKey([]byte(normalized), salt[:], OPS, MEM/1024, 1, 32))
	return salt, root
}

// seed returns the 32-byte seed of one role: BLAKE2b-256 keyed with appKey over the role label
// (for example "holebridge key v1 host") followed by root.
func seed(label string, root, appKey [32]byte) []byte {
	h, _ := blake2b.New256(appKey[:]) // a 32-byte key is a valid size
	h.Write([]byte(label))
	h.Write(root[:])
	return h.Sum(nil)
}
