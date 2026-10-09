// Package udx is the pears-go port of the UDX transport: the packet header in this file, and the
// socket in socket.go that splits one UDP connection between UDX streams and dht-rpc.
//
// Ported from libudx udx.h (UDX_MAGIC_BYTE, UDX_VERSION, UDX_HEADER_*) and udx.c (init_stream_packet,
// process_packet, mtu_probeify_packet), Apache License 2.0, Copyright (c) 2021 Holepunch Inc.
package udx

import (
	"encoding/binary"
	"errors"
)

// Type flags of the UDX header, libudx udx.h UDX_HEADER_*.
const (
	FlagData    uint8 = 0b00001
	FlagEnd     uint8 = 0b00010
	FlagSack    uint8 = 0b00100
	FlagMessage uint8 = 0b01000
	FlagDestroy uint8 = 0b10000
)

const (
	magic      = 255 // UDX_MAGIC_BYTE
	version    = 1   // UDX_VERSION
	headerSize = 20  // bytes before the data offset padding and the payload
)

// Header is the 20-byte UDX packet header. The integers are little endian on the wire.
type Header struct {
	Type       uint8  // combination of the Flag constants
	DataOffset uint8  // zero padding between the header and the payload (MTU probes)
	RemoteID   uint32 // the stream id on the receiving side
	RecvWindow uint32
	Seq        uint32
	Ack        uint32
}

// EncodeHeader returns the header, then DataOffset zero bytes, then payload.
func EncodeHeader(h Header, payload []byte) []byte {
	start := headerSize + int(h.DataOffset)
	b := make([]byte, start+len(payload))
	b[0] = magic
	b[1] = version
	b[2] = h.Type
	b[3] = h.DataOffset
	binary.LittleEndian.PutUint32(b[4:], h.RemoteID)
	binary.LittleEndian.PutUint32(b[8:], h.RecvWindow)
	binary.LittleEndian.PutUint32(b[12:], h.Seq)
	binary.LittleEndian.PutUint32(b[16:], h.Ack)
	copy(b[start:], payload)
	return b
}

// DecodeHeader parses one UDX packet. It returns the header and the payload after the padding.
func DecodeHeader(b []byte) (Header, []byte, error) {
	if !IsUDX(b) {
		return Header{}, nil, errors.New("udx: not a UDX packet")
	}
	h := Header{
		Type:       b[2],
		DataOffset: b[3],
		RemoteID:   binary.LittleEndian.Uint32(b[4:]),
		RecvWindow: binary.LittleEndian.Uint32(b[8:]),
		Seq:        binary.LittleEndian.Uint32(b[12:]),
		Ack:        binary.LittleEndian.Uint32(b[16:]),
	}
	start := headerSize + int(h.DataOffset)
	if start > len(b) {
		return Header{}, nil, errors.New("udx: data offset runs past the end of the packet")
	}
	return h, b[start:], nil
}

// IsUDX reports whether b is a UDX packet: at least 20 bytes, magic 0xFF and version 1.
func IsUDX(b []byte) bool {
	return len(b) >= headerSize && b[0] == magic && b[1] == version
}
