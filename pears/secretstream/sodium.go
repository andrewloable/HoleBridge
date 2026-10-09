// Package secretstream implements libsodium's crypto_secretstream_xchacha20poly1305 in pure Go, as
// pears-go needs it for the Holepunch secret stream (decisions D32).
//
// It ports src/libsodium/crypto_secretstream/xchacha20poly1305/secretstream_xchacha20poly1305.c from
// libsodium 1.0.20-RELEASE. The vectors, which sodium-native 5.1.0 generates, match it byte for byte.
// The upstream copyright, from libsodium's LICENSE (ISC):
//
//	Copyright (c) 2013-2024 Frank Denis <j at pureftpd dot org>
package secretstream

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"errors"

	"golang.org/x/crypto/chacha20"
	"golang.org/x/crypto/poly1305"
)

// HeaderSize is the length of the stream header, which the sender sends once before its messages.
const HeaderSize = 24

// ABytes is what Seal adds to a message: one encrypted tag byte and a 16-byte Poly1305 tag.
const ABytes = 17

// Tags mark each message, as in libsodium.
const (
	TagMessage = 0
	TagPush    = 1
	TagRekey   = 2
	TagFinal   = 3
)

var errAuth = errors.New("secretstream: message failed authentication")

var zeros [16]byte

// state is libsodium's crypto_secretstream_xchacha20poly1305_state. The nonce is the 4-byte
// little-endian message counter followed by the 8-byte inonce.
type state struct {
	k     [32]byte
	nonce [12]byte
}

// newState derives the state from key and header, as libsodium's init_pull does.
func newState(key [32]byte, header [24]byte) state {
	sub, err := chacha20.HChaCha20(key[:], header[:16])
	if err != nil {
		panic(err) // not reachable: the key and nonce sizes are fixed by the array types
	}
	var s state
	copy(s.k[:], sub)
	copy(s.nonce[4:], header[16:])
	s.nonce[0] = 1 // the counter starts at 1
	return s
}

// xor XORs src with the keystream of the current nonce, starting at block counter, into dst.
func (s *state) xor(dst, src []byte, counter uint32) {
	c, err := chacha20.NewUnauthenticatedCipher(s.k[:], s.nonce[:])
	if err != nil {
		panic(err) // not reachable: the key and nonce sizes are fixed by the array types
	}
	c.SetCounter(counter)
	c.XORKeyStream(dst, src)
}

// authenticate returns the Poly1305 tag over the associated data ad, the 64-byte block that holds
// the encrypted tag and keystream, and the ciphertext c, in libsodium's order. The one-time key is
// the first 32 bytes of keystream block 0. As in libsodium, the ciphertext is padded with len(c)
// mod 16 zero bytes, not up to the next multiple of 16.
func (s *state) authenticate(block *[64]byte, ad, c []byte) []byte {
	var otk [64]byte
	s.xor(otk[:], otk[:], 0)
	mac := poly1305.New((*[32]byte)(otk[:32]))
	mac.Write(ad)
	mac.Write(zeros[:(16-len(ad)%16)%16])
	mac.Write(block[:])
	mac.Write(c)
	mac.Write(zeros[:len(c)%16])
	var n [8]byte
	binary.LittleEndian.PutUint64(n[:], uint64(len(ad)))
	mac.Write(n[:])
	binary.LittleEndian.PutUint64(n[:], uint64(64+len(c)))
	mac.Write(n[:])
	return mac.Sum(nil)
}

// advance moves the stream past a message whose authenticator is mac. The inonce absorbs the first
// 8 bytes of mac, the counter steps, and the stream rekeys after a REKEY tag or when the counter
// wraps to zero.
func (s *state) advance(tag byte, mac []byte) {
	for i := range 8 {
		s.nonce[4+i] ^= mac[i]
	}
	counter := binary.LittleEndian.Uint32(s.nonce[:4]) + 1
	binary.LittleEndian.PutUint32(s.nonce[:4], counter)
	if tag&TagRekey != 0 || counter == 0 {
		s.rekey()
	}
}

// rekey replaces the key and inonce with 40 bytes of keystream XORed onto the old key and inonce,
// then resets the counter to 1.
func (s *state) rekey() {
	var kin [40]byte
	copy(kin[:32], s.k[:])
	copy(kin[32:], s.nonce[4:])
	s.xor(kin[:], kin[:], 0)
	copy(s.k[:], kin[:32])
	copy(s.nonce[4:], kin[32:])
	binary.LittleEndian.PutUint32(s.nonce[:4], 1)
}

// Push seals the messages of one stream.
type Push struct{ s state }

// NewPush starts a stream under key with a fresh random header.
func NewPush(key [32]byte) (*Push, [24]byte, error) {
	var header [24]byte
	if _, err := rand.Read(header[:]); err != nil {
		return nil, [24]byte{}, err
	}
	return newPushWithHeader(key, header), header, nil
}

// newPushWithHeader starts a stream under key and header. It is for tests, which replay vectors.
func newPushWithHeader(key [32]byte, header [24]byte) *Push {
	return &Push{newState(key, header)}
}

// Seal encrypts msg with tag and ad, and advances the stream.
func (p *Push) Seal(msg, ad []byte, tag byte) []byte {
	out := make([]byte, ABytes+len(msg))
	c := out[1 : 1+len(msg)]
	var block [64]byte
	block[0] = tag
	p.s.xor(block[:], block[:], 1)
	out[0] = block[0]
	p.s.xor(c, msg, 2)
	mac := p.s.authenticate(&block, ad, c)
	copy(out[1+len(msg):], mac)
	p.s.advance(tag, mac)
	return out
}

// Pull opens the messages of one stream.
type Pull struct{ s state }

// NewPull starts reading a stream that was started under key and header.
func NewPull(key [32]byte, header [24]byte) *Pull {
	return &Pull{newState(key, header)}
}

// Open decrypts and authenticates the next message, and returns its plaintext and tag. A message
// that fails authentication leaves the stream where it was.
func (p *Pull) Open(ct, ad []byte) (msg []byte, tag byte, err error) {
	if len(ct) < ABytes {
		return nil, 0, errAuth
	}
	n := len(ct) - ABytes
	c := ct[1 : 1+n]
	var block [64]byte
	block[0] = ct[0]
	p.s.xor(block[:], block[:], 1)
	tag = block[0]
	block[0] = ct[0]
	mac := p.s.authenticate(&block, ad, c)
	if subtle.ConstantTimeCompare(mac, ct[1+n:]) != 1 {
		return nil, 0, errAuth
	}
	msg = make([]byte, n)
	p.s.xor(msg, c, 2)
	p.s.advance(tag, mac)
	return msg, tag, nil
}
