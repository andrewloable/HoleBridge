// Package lan is the host's LAN route: the signed UDP probe that finds a host on the same network,
// and the responder that answers it (docs/architecture.md, LAN route).
package lan

import (
	"crypto/subtle"
	"encoding/binary"
	"time"

	"golang.org/x/crypto/blake2b"
)

const (
	probeSize   = 256
	replySize   = 64
	probeMagic  = "HBLANQ1"
	replyMagic  = "HBLANR1"
	probeMaxAge = 24 * time.Hour // a probe's timestamp may be this far from the host's clock, either way
)

// lanMAC is the BLAKE2b-256 MAC of msg under lanKey.
func lanMAC(lanKey [32]byte, msg []byte) []byte {
	h, _ := blake2b.New256(lanKey[:]) // a 32-byte key is a valid size
	h.Write(msg)
	return h.Sum(nil)
}

// EncodeProbe returns the 256-byte probe: magic HBLANQ1, format version 1, the nonce, the sender's
// wall clock in milliseconds (uint64, little-endian), the MAC keyed with lanKey over bytes 0 to 31,
// then zero padding.
func EncodeProbe(lanKey [32]byte, nonce [16]byte, unixMs uint64) [256]byte {
	var b [probeSize]byte
	copy(b[0:7], probeMagic)
	b[7] = 1
	copy(b[8:24], nonce[:])
	binary.LittleEndian.PutUint64(b[24:32], unixMs)
	copy(b[32:64], lanMAC(lanKey, b[:32]))
	return b
}

// VerifyProbe checks b as a probe under lanKey at time now: size, magic, version, the MAC (compared
// in constant time), and a timestamp within 24 h of now. It returns the probe's nonce when ok.
func VerifyProbe(lanKey [32]byte, b []byte, now time.Time) (nonce [16]byte, ok bool) {
	if len(b) != probeSize || string(b[:7]) != probeMagic || b[7] != 1 {
		return nonce, false
	}
	if subtle.ConstantTimeCompare(lanMAC(lanKey, b[:32]), b[32:64]) != 1 {
		return nonce, false
	}
	ts := time.UnixMilli(int64(binary.LittleEndian.Uint64(b[24:32])))
	if age := now.Sub(ts); age > probeMaxAge || age < -probeMaxAge {
		return nonce, false
	}
	copy(nonce[:], b[8:24])
	return nonce, true
}

// EncodeReply returns the 64-byte reply to the probe with nonce: magic HBLANR1, version 1, the nonce,
// the host's LAN TCP port (uint16, little-endian) at bytes 24 to 25, zeros at 26 to 31, and the MAC
// keyed with lanKey over bytes 0 to 31.
func EncodeReply(lanKey [32]byte, nonce [16]byte, port uint16) [64]byte {
	var b [replySize]byte
	copy(b[0:7], replyMagic)
	b[7] = 1
	copy(b[8:24], nonce[:])
	binary.LittleEndian.PutUint16(b[24:26], port)
	copy(b[32:64], lanMAC(lanKey, b[:32]))
	return b
}

// VerifyReply checks b as the reply to the probe with nonce under lanKey. It returns the host's LAN
// TCP port when ok.
func VerifyReply(lanKey [32]byte, b []byte, nonce [16]byte) (port uint16, ok bool) {
	if len(b) != replySize || string(b[:7]) != replyMagic || b[7] != 1 {
		return 0, false
	}
	if subtle.ConstantTimeCompare(b[8:24], nonce[:]) != 1 {
		return 0, false
	}
	if subtle.ConstantTimeCompare(lanMAC(lanKey, b[:32]), b[32:64]) != 1 {
		return 0, false
	}
	return binary.LittleEndian.Uint16(b[24:26]), true
}
