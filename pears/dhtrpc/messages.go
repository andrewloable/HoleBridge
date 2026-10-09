// Package dhtrpc is the wire format of pears-go's dht-rpc: the request and response packets that
// dht-rpc 6.27.0 sends over UDP, as its encoder lays them out (lib/io.js).
//
// Ported from dht-rpc 6.27.0 lib/io.js and lib/peer.js, MIT License, Copyright (c) 2021 Mathias
// Buus.
//
// Byte 0 is (type << 4) | 3: 0x03 for a request, 0x13 for a response. Then come the flags byte, the
// transaction id (uint16 LE), the address the packet is for (IPv4 host, then port as uint16 LE),
// and the optional fields that the flags name, in flag order.
package dhtrpc

import (
	"bytes"
	"errors"
	"net"
	"net/netip"

	"github.com/andrewloable/HoleBridge/pears/compact"
)

// ErrUnknownType is returned by Decode for a packet whose type byte is neither a request nor a
// response.
var ErrUnknownType = errors.New("dhtrpc: unknown packet type")

// Byte 0 of each packet kind, (type << 4) | 3.
const (
	requestID  = 0x03
	responseID = 0x13
)

// Flag bits. A request and a reply share the bits for id, token and value. The other two bits are
// named for the kind that uses them.
const (
	flagID       = 1
	flagToken    = 2
	flagInternal = 4 // request
	flagCloser   = 4 // reply: closer nodes follow
	flagTarget   = 8 // request
	flagError    = 8 // reply
	flagValue    = 16
)

// Addr is an IPv4 peer: its host and UDP port.
type Addr struct {
	Host netip.Addr
	Port uint16
}

// Request is a dht-rpc request. ID (the sender id), Token, Target and Value are nil when the packet
// does not carry them. Command is a compact uint. From is the address a received request came from;
// the IO sets it before the handler runs, and it is not encoded.
type Request struct {
	Tid      uint16
	To       Addr
	ID       []byte
	Token    []byte
	Internal bool
	Command  uint64
	Target   []byte
	Value    []byte
	From     *net.UDPAddr
}

// Response is a dht-rpc reply. ID, Token, CloserNodes and Value are nil when the packet does not
// carry them. Error is 0 when the reply has no error code. NoToken, which is not encoded, keeps the IO
// from adding a token to a reply that has none, as upstream does when a handler replies token false.
// From is the address a received reply came from, as upstream's res.from; the IO sets it on receipt and
// it is not encoded. A caller that sent a request checks it against the node it asked.
type Response struct {
	Tid         uint16
	To          Addr
	ID          []byte
	Token       []byte
	CloserNodes []Addr
	Error       uint64
	Value       []byte
	NoToken     bool
	From        *net.UDPAddr
}

// EncodeRequest returns the packet bytes of r. It panics when ID, Token or Target is not 32 bytes,
// or when To.Host is not IPv4: the packet layout cannot carry them.
func EncodeRequest(r Request) []byte {
	var flags uint8
	if r.ID != nil {
		flags |= flagID
	}
	if r.Token != nil {
		flags |= flagToken
	}
	if r.Internal {
		flags |= flagInternal
	}
	if r.Target != nil {
		flags |= flagTarget
	}
	if r.Value != nil {
		flags |= flagValue
	}
	var e compact.Encoder
	e.Uint8(requestID)
	e.Uint8(flags)
	e.Uint16(r.Tid)
	putAddr(&e, r.To)
	if r.ID != nil {
		e.Fixed(fixed32(r.ID))
	}
	if r.Token != nil {
		e.Fixed(fixed32(r.Token))
	}
	e.Uint(r.Command)
	if r.Target != nil {
		e.Fixed(fixed32(r.Target))
	}
	if r.Value != nil {
		e.Buffer(r.Value)
	}
	return e.Bytes()
}

// EncodeResponse returns the packet bytes of r. It panics on the same inputs as EncodeRequest.
func EncodeResponse(r Response) []byte {
	var flags uint8
	if r.ID != nil {
		flags |= flagID
	}
	if r.Token != nil {
		flags |= flagToken
	}
	if len(r.CloserNodes) > 0 {
		flags |= flagCloser
	}
	if r.Error != 0 {
		flags |= flagError
	}
	if r.Value != nil {
		flags |= flagValue
	}
	var e compact.Encoder
	e.Uint8(responseID)
	e.Uint8(flags)
	e.Uint16(r.Tid)
	putAddr(&e, r.To)
	if r.ID != nil {
		e.Fixed(fixed32(r.ID))
	}
	if r.Token != nil {
		e.Fixed(fixed32(r.Token))
	}
	if len(r.CloserNodes) > 0 {
		e.Len(len(r.CloserNodes))
		for _, a := range r.CloserNodes {
			putAddr(&e, a)
		}
	}
	if r.Error != 0 {
		e.Uint(r.Error)
	}
	if r.Value != nil {
		e.Buffer(r.Value)
	}
	return e.Bytes()
}

// Decode parses one packet and returns a *Request or a *Response. It returns ErrUnknownType for a
// packet with an unknown type byte, and compact.ErrOutOfBounds for a truncated packet. Like dht-rpc,
// it ignores bytes after the fields the flags name. The result does not share memory with b.
func Decode(b []byte) (any, error) {
	d := compact.NewDecoder(bytes.Clone(b))
	kind, err := d.Uint8()
	if err != nil {
		return nil, err
	}
	switch kind {
	case requestID:
		r, err := decodeRequest(d)
		if err != nil {
			return nil, err
		}
		return r, nil
	case responseID:
		r, err := decodeResponse(d)
		if err != nil {
			return nil, err
		}
		return r, nil
	}
	return nil, ErrUnknownType
}

// fixed32 returns b, which must be 32 bytes: the id, token and target fields have that size.
func fixed32(b []byte) []byte {
	if len(b) != 32 {
		panic("dhtrpc: id, token and target must be 32 bytes")
	}
	return b
}

// putAddr writes a as its IPv4 host, then its port as uint16 LE.
func putAddr(e *compact.Encoder, a Addr) {
	if !a.Host.Is4() {
		panic("dhtrpc: address host must be IPv4")
	}
	ip := a.Host.As4()
	e.Fixed(ip[:])
	e.Uint16(a.Port)
}

// getAddr reads an address that putAddr wrote.
func getAddr(d *compact.Decoder) (Addr, error) {
	ip, err := d.Fixed(4)
	if err != nil {
		return Addr{}, err
	}
	port, err := d.Uint16()
	if err != nil {
		return Addr{}, err
	}
	return Addr{Host: netip.AddrFrom4([4]byte(ip)), Port: port}, nil
}

// fixedIf reads a 32-byte field when on is set, and returns nil when it is not.
func fixedIf(d *compact.Decoder, on bool) ([]byte, error) {
	if !on {
		return nil, nil
	}
	return d.Fixed(32)
}

// decodeRequest reads a request after its type byte, as Request.decode does in lib/io.js.
func decodeRequest(d *compact.Decoder) (*Request, error) {
	flags, err := d.Uint()
	if err != nil {
		return nil, err
	}
	var r Request
	if r.Tid, err = d.Uint16(); err != nil {
		return nil, err
	}
	if r.To, err = getAddr(d); err != nil {
		return nil, err
	}
	if r.ID, err = fixedIf(d, flags&flagID != 0); err != nil {
		return nil, err
	}
	if r.Token, err = fixedIf(d, flags&flagToken != 0); err != nil {
		return nil, err
	}
	r.Internal = flags&flagInternal != 0
	if r.Command, err = d.Uint(); err != nil {
		return nil, err
	}
	if r.Target, err = fixedIf(d, flags&flagTarget != 0); err != nil {
		return nil, err
	}
	if flags&flagValue != 0 {
		if r.Value, err = d.Buffer(); err != nil {
			return nil, err
		}
	}
	return &r, nil
}

// decodeResponse reads a reply after its type byte, as decodeReply does in lib/io.js.
func decodeResponse(d *compact.Decoder) (*Response, error) {
	flags, err := d.Uint()
	if err != nil {
		return nil, err
	}
	var r Response
	if r.Tid, err = d.Uint16(); err != nil {
		return nil, err
	}
	if r.To, err = getAddr(d); err != nil {
		return nil, err
	}
	if r.ID, err = fixedIf(d, flags&flagID != 0); err != nil {
		return nil, err
	}
	if r.Token, err = fixedIf(d, flags&flagToken != 0); err != nil {
		return nil, err
	}
	if flags&flagCloser != 0 {
		// The count is not checked against the packet size: each address takes 6 bytes, so the
		// loop stops at the end of the packet.
		n, err := d.Uint()
		if err != nil {
			return nil, err
		}
		for i := uint64(0); i < n; i++ {
			a, err := getAddr(d)
			if err != nil {
				return nil, err
			}
			r.CloserNodes = append(r.CloserNodes, a)
		}
	}
	if flags&flagError != 0 {
		if r.Error, err = d.Uint(); err != nil {
			return nil, err
		}
	}
	if flags&flagValue != 0 {
		if r.Value, err = d.Buffer(); err != nil {
			return nil, err
		}
	}
	return &r, nil
}
