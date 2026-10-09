// Package noise implements the Noise IK handshake that hyperdht runs between peers, on the Ed25519
// curve: Noise_IK_Ed25519_ChaChaPoly_BLAKE2b with the prologue hyperdht passes (decisions D32). It
// follows noise-handshake 4.2.0 and noise-curve-ed 2.1.0, whose output spec/gen/noise.js records in
// spec/vectors/noise.json.
//
// Ported from noise-handshake 4.2.0 (noise.js, symmetric-state.js, cipher.js, hkdf.js, hmac.js),
// Apache License 2.0, author Holepunch. The npm package has no copyright line of its own; its
// repository is holepunchto/noise-handshake.
package noise

import (
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"hash"
	"io"

	"golang.org/x/crypto/blake2b"
	"golang.org/x/crypto/chacha20poly1305"
)

// KeyPair is an Ed25519 key pair in libsodium's layout: Secret is the 32-byte seed followed by Public.
type KeyPair struct {
	Public [32]byte
	Secret [64]byte
}

// ephemeralRand supplies the 32-byte seed of each ephemeral key pair. Product code uses crypto/rand.
// Tests replace it with the seeds the vector records, so the handshake messages can be reproduced.
var ephemeralRand io.Reader = rand.Reader

// protocolName is the Noise protocol name. It is shorter than the 64-byte digest, so the digest starts
// as the name padded with zeros.
const protocolName = "Noise_IK_Ed25519_ChaChaPoly_BLAKE2b"

var (
	errOutOfOrder = errors.New("noise: handshake message out of order")
	errShort      = errors.New("noise: handshake message too short")
)

// Handshake is one side of the IK handshake. The initiator sends message 1 and reads message 2. The
// responder reads message 1 and sends message 2. After Send or Recv returns an error the handshake
// may be partly updated, so discard it.
type Handshake struct {
	initiator bool
	static    KeyPair
	ephemeral KeyPair
	rs        [32]byte // remote static public key
	re        [32]byte // remote ephemeral public key
	ck        [64]byte // chaining key
	digest    [64]byte // handshake hash
	aead      cipher.AEAD
	nonce     uint64
	step      int // messages exchanged so far: 0, 1 or 2
	tx, rx    [32]byte
}

// NewInitiator starts the handshake for the side that sends message 1. IK needs the responder's
// static public key up front, as remoteStatic.
func NewInitiator(static KeyPair, remoteStatic [32]byte, prologue []byte) *Handshake {
	h := newHandshake(true, static, prologue)
	h.rs = remoteStatic
	h.mixHash(remoteStatic[:])
	return h
}

// NewResponder starts the handshake for the side that reads message 1.
func NewResponder(static KeyPair, prologue []byte) *Handshake {
	h := newHandshake(false, static, prologue)
	h.mixHash(static.Public[:])
	return h
}

// newHandshake starts the symmetric state with the protocol name and the prologue.
func newHandshake(initiator bool, static KeyPair, prologue []byte) *Handshake {
	h := &Handshake{initiator: initiator, static: static}
	copy(h.digest[:], protocolName)
	h.ck = h.digest
	h.mixHash(prologue)
	return h
}

// Send writes the next handshake message, carrying payload, and returns it.
func (h *Handshake) Send(payload []byte) ([]byte, error) {
	if err := h.expect(true); err != nil {
		return nil, err
	}
	e, err := newEphemeral()
	if err != nil {
		return nil, err
	}
	h.ephemeral = e
	h.mixHash(e.Public[:])
	out := append([]byte(nil), e.Public[:]...)
	if h.step == 0 {
		// Message 1 is e, es, s, ss, then the payload.
		if err := h.mixKey(e, h.rs); err != nil {
			return nil, err
		}
		out = append(out, h.encryptAndHash(h.static.Public[:])...)
		if err := h.mixKey(h.static, h.rs); err != nil {
			return nil, err
		}
	} else {
		// Message 2 is e, ee, se, then the payload.
		if err := h.mixKey(e, h.re); err != nil {
			return nil, err
		}
		if err := h.mixKey(e, h.rs); err != nil {
			return nil, err
		}
	}
	out = append(out, h.encryptAndHash(payload)...)
	h.step++
	if h.step == 2 {
		h.split()
	}
	return out, nil
}

// Recv reads the next handshake message and returns the payload it carries.
func (h *Handshake) Recv(msg []byte) ([]byte, error) {
	if err := h.expect(false); err != nil {
		return nil, err
	}
	var payload []byte
	var err error
	if h.step == 0 {
		// Message 1 is e (32 bytes), s (32 bytes and a 16-byte tag), then the payload.
		if len(msg) < 32+48+16 {
			return nil, errShort
		}
		h.re = [32]byte(msg[:32])
		h.mixHash(msg[:32])
		if err = h.mixKey(h.static, h.re); err != nil { // es
			return nil, err
		}
		var rs []byte
		if rs, err = h.decryptAndHash(msg[32:80]); err != nil {
			return nil, err
		}
		h.rs = [32]byte(rs)
		if err = h.mixKey(h.static, h.rs); err != nil { // ss
			return nil, err
		}
		if payload, err = h.decryptAndHash(msg[80:]); err != nil {
			return nil, err
		}
	} else {
		// Message 2 is e, then the payload.
		if len(msg) < 32+16 {
			return nil, errShort
		}
		h.re = [32]byte(msg[:32])
		h.mixHash(msg[:32])
		if err = h.mixKey(h.ephemeral, h.re); err != nil { // ee
			return nil, err
		}
		if err = h.mixKey(h.static, h.re); err != nil { // se
			return nil, err
		}
		if payload, err = h.decryptAndHash(msg[32:]); err != nil {
			return nil, err
		}
	}
	h.step++
	if h.step == 2 {
		h.split()
	}
	return payload, nil
}

// Complete reports whether both messages have been exchanged.
func (h *Handshake) Complete() bool {
	return h.step == 2
}

// Result returns the transport keys for sending (tx) and receiving (rx), the handshake hash, and the
// remote static public key. It is valid once Complete reports true.
func (h *Handshake) Result() (tx, rx [32]byte, hash [64]byte, remoteStatic [32]byte) {
	return h.tx, h.rx, h.digest, h.rs
}

// expect returns an error unless the next message is one this side sends (sending) or reads. The
// initiator sends message 1 and the responder sends message 2.
func (h *Handshake) expect(sending bool) error {
	sendsNext := h.initiator == (h.step == 0)
	if h.step > 1 || sending != sendsNext {
		return errOutOfOrder
	}
	return nil
}

// mixHash sets the handshake hash to BLAKE2b-512 of the hash followed by data.
func (h *Handshake) mixHash(data []byte) {
	h.digest = blake2b.Sum512(append(h.digest[:], data...))
}

// mixKey mixes the Diffie-Hellman output of local and remote into the chaining key, and starts a new
// cipher key with nonce 0.
func (h *Handshake) mixKey(local KeyPair, remote [32]byte) error {
	shared, err := dh(remote, local)
	if err != nil {
		return err
	}
	var k [64]byte
	h.ck, k = hkdf(h.ck[:], shared[:])
	h.aead, err = chacha20poly1305.New(k[:32])
	h.nonce = 0
	return err
}

// encryptAndHash encrypts plaintext with the handshake hash as associated data, then mixes the
// ciphertext into the hash.
func (h *Handshake) encryptAndHash(plaintext []byte) []byte {
	ct := h.aead.Seal(nil, h.nonceBytes(), plaintext, h.digest[:])
	h.nonce++
	h.mixHash(ct)
	return ct
}

// decryptAndHash decrypts ciphertext with the handshake hash as associated data, then mixes the
// ciphertext into the hash.
func (h *Handshake) decryptAndHash(ciphertext []byte) ([]byte, error) {
	pt, err := h.aead.Open(nil, h.nonceBytes(), ciphertext, h.digest[:])
	if err != nil {
		return nil, err
	}
	h.nonce++
	h.mixHash(ciphertext)
	return pt, nil
}

// nonceBytes is the 12-byte AEAD nonce: four zero bytes, then the counter as a little-endian uint64.
func (h *Handshake) nonceBytes() []byte {
	var n [12]byte
	binary.LittleEndian.PutUint64(n[4:], h.nonce)
	return n[:]
}

// split derives the transport keys. The initiator sends with the first key and the responder with
// the second.
func (h *Handshake) split() {
	k1, k2 := hkdf(h.ck[:], nil)
	h.tx, h.rx = [32]byte(k1[:32]), [32]byte(k2[:32])
	if !h.initiator {
		h.tx, h.rx = h.rx, h.tx
	}
}

// hkdf is HKDF over BLAKE2b-512 with empty info, as hkdf.js computes it. It returns the two 64-byte
// outputs.
func hkdf(key, ikm []byte) (out1, out2 [64]byte) {
	prk := hmacBlake2b(key, ikm)
	out1 = hmacBlake2b(prk[:], []byte{1})
	out2 = hmacBlake2b(prk[:], out1[:], []byte{2})
	return out1, out2
}

// hmacBlake2b is HMAC over BLAKE2b-512, with the 128-byte block that hmac.js uses.
func hmacBlake2b(key []byte, parts ...[]byte) [64]byte {
	m := hmac.New(newBlake2b, key)
	for _, p := range parts {
		m.Write(p)
	}
	var out [64]byte
	copy(out[:], m.Sum(nil))
	return out
}

func newBlake2b() hash.Hash {
	h, _ := blake2b.New512(nil) // a nil key never fails
	return h
}

// newEphemeral draws an Ed25519 key pair from a 32-byte seed read from ephemeralRand, as
// crypto_sign_seed_keypair does.
func newEphemeral() (KeyPair, error) {
	var seed [32]byte
	if _, err := io.ReadFull(ephemeralRand, seed[:]); err != nil {
		return KeyPair{}, err
	}
	priv := ed25519.NewKeyFromSeed(seed[:])
	var kp KeyPair
	copy(kp.Secret[:], priv)
	copy(kp.Public[:], priv[32:])
	return kp, nil
}
