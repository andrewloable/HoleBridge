// Package noise is a throwaway port of the Noise IK handshake that hyperdht uses for peer
// connections: Noise_IK_Ed25519_ChaChaPoly_BLAKE2b, with the prologue hyperdht passes
// (NS.PEER_HANDSHAKE). Ported from noise-handshake 4.2.0 (noise.js, symmetric-state.js,
// cipher.js, hkdf.js, hmac.js), noise-curve-ed 2.1.0 (index.js) and hyperdht 6.34.1
// (lib/noise-wrap.js, lib/constants.js) plus hypercore-crypto 3.7.0 (namespace). All are
// Apache-2.0 or MIT upstream. Only the handshake pattern and what the spike needs are here.
package noise

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"encoding/binary"
	"errors"

	"filippo.io/edwards25519"
	"golang.org/x/crypto/blake2b"
	"golang.org/x/crypto/chacha20poly1305"
)

const protocolName = "Noise_IK_Ed25519_ChaChaPoly_BLAKE2b"

// hyperdht prologue: hypercore-crypto namespace('hyperswarm/dht') id 0 (PEER_HANDSHAKE).
// nsRoot is BLAKE2b-256("hyperswarm/dht"); the prologue is BLAKE2b-256(nsRoot || 0x00).
var nsRoot = blake2b.Sum256([]byte("hyperswarm/dht"))

// Prologue returns the handshake prologue hyperdht uses.
func Prologue() [32]byte {
	return blake2b.Sum256(append(nsRoot[:], 0))
}

// Keypair is an Ed25519 key pair as noise-curve-ed uses it: the public key is the compressed
// point, and DH uses the scalar derived from the seed (SHA-512, clamped).
type Keypair struct {
	Public [32]byte
	scalar [32]byte
}

// KeypairFromSeed is noise-curve-ed generateKeyPair(seed) (crypto_sign_seed_keypair).
func KeypairFromSeed(seed [32]byte) Keypair {
	priv := ed25519.NewKeyFromSeed(seed[:])
	var kp Keypair
	copy(kp.Public[:], priv[32:])
	h := sha512.Sum512(seed[:])
	h[0] &= 248
	h[31] &= 127
	h[31] |= 64
	copy(kp.scalar[:], h[:32])
	return kp
}

// GenerateKeypair makes a random key pair (ephemeral keys).
func GenerateKeypair() (Keypair, error) {
	var seed [32]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return Keypair{}, err
	}
	return KeypairFromSeed(seed), nil
}

// dh is noise-curve-ed dh(publicKey, keypair): scalar times the point, compressed. libsodium
// runs crypto_scalarmult_ed25519_noclamp on the clamped scalar. Here the scalar is reduced mod
// l first. That gives the same point for any key of prime order, which every honest key is.
func dh(pub [32]byte, kp Keypair) ([32]byte, error) {
	var out [32]byte
	p, err := new(edwards25519.Point).SetBytes(pub[:])
	if err != nil {
		return out, err
	}
	var wide [64]byte
	copy(wide[:], kp.scalar[:])
	s, err := new(edwards25519.Scalar).SetUniformBytes(wide[:])
	if err != nil {
		return out, err
	}
	copy(out[:], new(edwards25519.Point).ScalarMult(s, p).Bytes())
	return out, nil
}

// hmac512 is noise-handshake hmac.js: HMAC over BLAKE2b-512 with a 128-byte block.
func hmac512(key []byte, parts ...[]byte) [64]byte {
	var k [128]byte
	if len(key) > 128 {
		h := blake2b.Sum512(key)
		copy(k[:], h[:])
	} else {
		copy(k[:], key)
	}
	var ipad, opad [128]byte
	for i := range k {
		ipad[i] = 0x36 ^ k[i]
		opad[i] = 0x5c ^ k[i]
	}
	inner, _ := blake2b.New512(nil)
	inner.Write(ipad[:])
	for _, p := range parts {
		inner.Write(p)
	}
	ih := inner.Sum(nil)
	outer, _ := blake2b.New512(nil)
	outer.Write(opad[:])
	outer.Write(ih)
	var out [64]byte
	copy(out[:], outer.Sum(nil))
	return out
}

// hkdf2 is noise-handshake hkdf(salt, ikm) with the default empty info and length 128: two
// 64-byte outputs. hkdfExpand block i is HMAC(prk, prev || info || i+1).
func hkdf2(salt, ikm []byte) ([64]byte, [64]byte) {
	prk := hmac512(salt, ikm)
	t1 := hmac512(prk[:], []byte{0x01})
	t2 := hmac512(prk[:], t1[:], []byte{0x02})
	return t1, t2
}

// nonce12 is the IETF ChaCha20-Poly1305 nonce noise-handshake builds: a uint32 LE counter at
// byte offset 4, zeros elsewhere.
func nonce12(n uint64) []byte {
	b := make([]byte, chacha20poly1305.NonceSize)
	binary.LittleEndian.PutUint32(b[4:], uint32(n))
	return b
}

// symmetricState is noise-handshake SymmetricState plus CipherState.
type symmetricState struct {
	digest [64]byte
	ck     [64]byte
	key    [32]byte
	hasKey bool
	nonce  uint64
}

func (s *symmetricState) mixHash(data []byte) {
	h, _ := blake2b.New512(nil)
	h.Write(s.digest[:])
	h.Write(data)
	copy(s.digest[:], h.Sum(nil))
}

func (s *symmetricState) mixKey(remote [32]byte, local Keypair) error {
	d, err := dh(remote, local)
	if err != nil {
		return err
	}
	ck, k := hkdf2(s.ck[:], d[:])
	s.ck = ck
	copy(s.key[:], k[:32])
	s.hasKey = true
	s.nonce = 0
	return nil
}

func (s *symmetricState) encryptAndHash(pt []byte) ([]byte, error) {
	aead, err := chacha20poly1305.New(s.key[:])
	if err != nil {
		return nil, err
	}
	ct := aead.Seal(nil, nonce12(s.nonce), pt, s.digest[:])
	s.nonce++
	s.mixHash(ct)
	return ct, nil
}

func (s *symmetricState) decryptAndHash(ct []byte) ([]byte, error) {
	aead, err := chacha20poly1305.New(s.key[:])
	if err != nil {
		return nil, err
	}
	pt, err := aead.Open(nil, nonce12(s.nonce), ct, s.digest[:])
	if err != nil {
		return nil, errors.New("noise: decrypt failed (handshake MAC mismatch)")
	}
	s.nonce++
	s.mixHash(ct)
	return pt, nil
}

// Handshake runs the two-message IK pattern. The initiator knows the responder's static key
// up front. The responder learns the initiator's static key from message 1.
type Handshake struct {
	initiator bool
	s         Keypair
	rs        [32]byte
	e         Keypair
	re        [32]byte
	st        symmetricState
	Tx, Rx    [32]byte
	Hash      [64]byte
	Complete  bool
}

// NewHandshake sets up the pattern (noise-handshake initialise). remote is the responder's
// static key for the initiator, and it is ignored by the responder.
func NewHandshake(initiator bool, static Keypair, remote [32]byte) *Handshake {
	h := &Handshake{initiator: initiator, s: static}
	copy(h.st.digest[:], protocolName) // 35 bytes, at most 64, so the digest is the name padded
	h.st.ck = h.st.digest
	pro := Prologue()
	h.st.mixHash(pro[:])
	// IK pre-message: the responder's static key. The initiator mixes the remote key, the
	// responder mixes its own.
	if initiator {
		h.rs = remote
		h.st.mixHash(remote[:])
	} else {
		h.st.mixHash(static.Public[:])
	}
	return h
}

// WriteMessage1 is the initiator's message: e, es, s, ss, then the payload.
func (h *Handshake) WriteMessage1(payload []byte) ([]byte, error) {
	e, err := GenerateKeypair()
	if err != nil {
		return nil, err
	}
	h.e = e
	out := append([]byte{}, e.Public[:]...)
	h.st.mixHash(e.Public[:])
	if err := h.st.mixKey(h.rs, h.e); err != nil { // es
		return nil, err
	}
	ct, err := h.st.encryptAndHash(h.s.Public[:])
	if err != nil {
		return nil, err
	}
	out = append(out, ct...)
	if err := h.st.mixKey(h.rs, h.s); err != nil { // ss
		return nil, err
	}
	ct, err = h.st.encryptAndHash(payload)
	if err != nil {
		return nil, err
	}
	return append(out, ct...), nil
}

// ReadMessage1 is the responder's side of message 1. It returns the initiator's payload.
func (h *Handshake) ReadMessage1(msg []byte) ([]byte, error) {
	if len(msg) < 32+48 {
		return nil, errors.New("noise: message 1 too short")
	}
	copy(h.re[:], msg[:32])
	h.st.mixHash(h.re[:])
	if err := h.st.mixKey(h.re, h.s); err != nil { // es, responder side
		return nil, err
	}
	rs, err := h.st.decryptAndHash(msg[32 : 32+48])
	if err != nil {
		return nil, err
	}
	copy(h.rs[:], rs)
	if err := h.st.mixKey(h.rs, h.s); err != nil { // ss
		return nil, err
	}
	return h.st.decryptAndHash(msg[32+48:])
}

// WriteMessage2 is the responder's message: e, ee, se, then the payload. It completes the
// handshake on the responder side.
func (h *Handshake) WriteMessage2(payload []byte) ([]byte, error) {
	e, err := GenerateKeypair()
	if err != nil {
		return nil, err
	}
	h.e = e
	out := append([]byte{}, e.Public[:]...)
	h.st.mixHash(e.Public[:])
	if err := h.st.mixKey(h.re, h.e); err != nil { // ee
		return nil, err
	}
	if err := h.st.mixKey(h.rs, h.e); err != nil { // se, responder side
		return nil, err
	}
	ct, err := h.st.encryptAndHash(payload)
	if err != nil {
		return nil, err
	}
	h.final()
	return append(out, ct...), nil
}

// ReadMessage2 is the initiator's side of message 2. It completes the handshake.
func (h *Handshake) ReadMessage2(msg []byte) ([]byte, error) {
	if len(msg) < 32 {
		return nil, errors.New("noise: message 2 too short")
	}
	copy(h.re[:], msg[:32])
	h.st.mixHash(h.re[:])
	if err := h.st.mixKey(h.re, h.e); err != nil { // ee
		return nil, err
	}
	if err := h.st.mixKey(h.re, h.s); err != nil { // se, initiator side
		return nil, err
	}
	pt, err := h.st.decryptAndHash(msg[32:])
	if err != nil {
		return nil, err
	}
	h.final()
	return pt, nil
}

// final is noise-handshake final(): split the chaining key, initiator sends with the first key.
func (h *Handshake) final() {
	k1, k2 := hkdf2(h.st.ck[:], []byte{})
	if h.initiator {
		copy(h.Tx[:], k1[:32])
		copy(h.Rx[:], k2[:32])
	} else {
		copy(h.Tx[:], k2[:32])
		copy(h.Rx[:], k1[:32])
	}
	h.Hash = h.st.digest
	h.Complete = true
}

// Seal and Open are one ChaCha20-Poly1305 record with nonce 0 and no associated data. The
// spike uses them to test that both sides hold matching transport keys.
func Seal(key [32]byte, pt []byte) ([]byte, error) {
	aead, err := chacha20poly1305.New(key[:])
	if err != nil {
		return nil, err
	}
	return aead.Seal(nil, nonce12(0), pt, nil), nil
}

func Open(key [32]byte, ct []byte) ([]byte, error) {
	aead, err := chacha20poly1305.New(key[:])
	if err != nil {
		return nil, err
	}
	return aead.Open(nil, nonce12(0), ct, nil)
}
