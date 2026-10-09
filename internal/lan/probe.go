// Package lan is the host's LAN route: the signed UDP probe that finds a host on the same network,
// and the responder that answers it (docs/architecture.md, LAN route).
package lan

import "time"

// EncodeProbe returns the 256-byte probe: magic HBLANQ1, format version 1, the nonce, the sender's
// wall clock in milliseconds (uint64, little-endian), the MAC keyed with lanKey over bytes 0 to 31,
// then zero padding.
func EncodeProbe(lanKey [32]byte, nonce [16]byte, unixMs uint64) [256]byte {
	panic("not implemented")
}

// VerifyProbe checks b as a probe under lanKey at time now: size, magic, version, the MAC (compared
// in constant time), and a timestamp within 24 h of now. It returns the probe's nonce when ok.
func VerifyProbe(lanKey [32]byte, b []byte, now time.Time) (nonce [16]byte, ok bool) {
	panic("not implemented")
}

// EncodeReply returns the 64-byte reply to the probe with nonce: magic HBLANR1, version 1, the nonce,
// the host's LAN TCP port (uint16, little-endian) at bytes 24 to 25, zeros at 26 to 31, and the MAC
// keyed with lanKey over bytes 0 to 31.
func EncodeReply(lanKey [32]byte, nonce [16]byte, port uint16) [64]byte {
	panic("not implemented")
}

// VerifyReply checks b as the reply to the probe with nonce under lanKey. It returns the host's LAN
// TCP port when ok.
func VerifyReply(lanKey [32]byte, b []byte, nonce [16]byte) (port uint16, ok bool) {
	panic("not implemented")
}
