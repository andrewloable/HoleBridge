// Package dhtrpc is a throwaway port of the dht-rpc 6.27.0 wire format (io.js: Request and
// Request.decode, _encodeRequest, _sendReply, decodeReply; peer.js: id; commands.js) for the
// spike. Upstream: github.com/holepunchto/dht-rpc, Apache-2.0.
package dhtrpc

import (
	"bytes"
	"errors"
	"fmt"
	"net/netip"

	"golang.org/x/crypto/blake2b"

	"holebridge-spike-pears-go/internal/compact"
)

// Commands (dht-rpc lib/commands.js).
const (
	CmdPing     = 0
	CmdPingNat  = 1
	CmdFindNode = 2
)

// Byte 0 of every message: high nibble type (0 request, 1 response), low nibble version 3
// (dht-rpc io.js: VERSION, REQUEST_ID, RESPONSE_ID).
const (
	versionNibble = 0b11
	requestID     = 0b0000<<4 | versionNibble // 0x03
	responseID    = 0b0001<<4 | versionNibble // 0x13
)

// Request flags (io.js _encodeRequest / Request.decode).
const (
	reqFlagID       = 1
	reqFlagToken    = 2
	reqFlagInternal = 4
	reqFlagTarget   = 8
	reqFlagValue    = 16
)

// Response flags (io.js _sendReply / decodeReply).
const (
	resFlagID          = 1
	resFlagToken       = 2
	resFlagCloserNodes = 4
	resFlagError       = 8
	resFlagValue       = 16
)

// PeerID is dht-rpc peer.id: BLAKE2b-256 (unkeyed, 32 bytes) over the 6-byte IPv4 address
// encoding (peer.js id: ipv4.encode then crypto_generichash with the default 32-byte output).
func PeerID(a netip.AddrPort) [32]byte {
	var buf [6]byte
	ip := a.Addr().As4()
	copy(buf[:4], ip[:])
	buf[4] = byte(a.Port())
	buf[5] = byte(a.Port() >> 8)
	return blake2b.Sum256(buf[:])
}

// EncodeRequest mirrors io.js _encodeRequest for a request sent from an ephemeral client socket,
// so no sender id and no token. internal sets the internal flag used by findNode and ping.
func EncodeRequest(tid uint16, to netip.AddrPort, command uint64, target, value []byte, internal bool) []byte {
	var flags byte
	if internal {
		flags |= reqFlagInternal
	}
	if target != nil {
		flags |= reqFlagTarget
	}
	if value != nil {
		flags |= reqFlagValue
	}
	b := []byte{requestID, flags}
	b = compact.AppendUint16(b, tid)
	b = compact.AppendIPv4(b, to)
	b = compact.AppendUint(b, command)
	if target != nil {
		b = append(b, target...)
	}
	if value != nil {
		b = compact.AppendUint(b, uint64(len(value)))
		b = append(b, value...)
	}
	return b
}

// Response is a decoded dht-rpc reply (io.js decodeReply).
type Response struct {
	TID        uint16
	To         netip.AddrPort // the replier's view of our address
	ID         []byte         // present when the replier is non-ephemeral and on its server socket
	Token      []byte
	CloserNode []netip.AddrPort
	Error      uint64
	Value      []byte
}

// DecodeResponse decodes a reply datagram that arrived from "from".
func DecodeResponse(buf []byte) (*Response, error) {
	if len(buf) < 2 {
		return nil, errors.New("dhtrpc: datagram too short")
	}
	if buf[0] != responseID {
		return nil, fmt.Errorf("dhtrpc: first byte %#x is not a response", buf[0])
	}
	r := &compact.Reader{B: buf, P: 1}
	flags, err := r.Uint()
	if err != nil {
		return nil, err
	}
	res := &Response{}
	if res.TID, err = r.Uint16(); err != nil {
		return nil, err
	}
	if res.To, err = r.IPv4(); err != nil {
		return nil, err
	}
	if flags&resFlagID != 0 {
		if res.ID, err = r.Fixed(32); err != nil {
			return nil, err
		}
	}
	if flags&resFlagToken != 0 {
		if res.Token, err = r.Fixed(32); err != nil {
			return nil, err
		}
	}
	if flags&resFlagCloserNodes != 0 {
		if res.CloserNode, err = r.IPv4Array(); err != nil {
			return nil, err
		}
	}
	if flags&resFlagError != 0 {
		if res.Error, err = r.Uint(); err != nil {
			return nil, err
		}
	}
	if flags&resFlagValue != 0 {
		if res.Value, err = r.Buffer(); err != nil {
			return nil, err
		}
	}
	if r.P != len(buf) {
		return nil, fmt.Errorf("dhtrpc: %d trailing bytes", len(buf)-r.P)
	}
	return res, nil
}

// CheckID reports whether a reply's sender id, when present, matches PeerID of the address the
// datagram came from (io.js validateId).
func CheckID(res *Response, from netip.AddrPort) bool {
	if res.ID == nil {
		return false
	}
	want := PeerID(from)
	return bytes.Equal(want[:], res.ID)
}
