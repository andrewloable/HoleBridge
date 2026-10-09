// Package compact is the pears-go port of compact-encoding, the binary encoding the Holepunch
// libraries use on the wire: unsigned and signed integers, floats, buffers, strings and fixed-size
// fields.
//
// Ported from compact-encoding 3.5.2 (index.js), Apache License 2.0, Holepunch Inc. The npm package
// has no copyright line of its own; its repository is holepunchto/compact-encoding.
//
// The bytes written match the reference. Two decode and encode limits differ: Uint, Int and Uint64
// take the full Go range where the reference rejects values above 2^53-1 (signed ints outside
// -2^52 to 2^52-1), and String decodes invalid UTF-8 as its raw bytes where the reference replaces
// them with U+FFFD.
package compact

import (
	"encoding/binary"
	"errors"
	"math"
)

var (
	ErrOutOfBounds = errors.New("compact: out of bounds")
	ErrTrailing    = errors.New("compact: trailing bytes")
	ErrTooLong     = errors.New("compact: length exceeds max")
)

// Encoder appends compact-encoding values to a buffer. The zero value is ready to use.
type Encoder struct {
	buf []byte
}

// Bytes returns the encoded bytes.
func (e *Encoder) Bytes() []byte { return e.buf }

// Uint writes a uint: one byte up to 0xfc, else 0xfd, 0xfe or 0xff and a uint16, uint32 or uint64.
func (e *Encoder) Uint(v uint64) {
	switch {
	case v <= 0xfc:
		e.buf = append(e.buf, byte(v))
	case v <= 0xffff:
		e.buf = append(e.buf, 0xfd)
		e.Uint16(uint16(v))
	case v <= 0xffffffff:
		e.buf = append(e.buf, 0xfe)
		e.Uint32(uint32(v))
	default:
		e.buf = append(e.buf, 0xff)
		e.Uint64(v)
	}
}

// Uint8 writes one byte.
func (e *Encoder) Uint8(v uint8) { e.buf = append(e.buf, v) }

// Uint16 writes a little-endian uint16.
func (e *Encoder) Uint16(v uint16) { e.buf = binary.LittleEndian.AppendUint16(e.buf, v) }

// Uint24 writes a little-endian 24-bit unsigned integer.
func (e *Encoder) Uint24(v uint32) { e.buf = append(e.buf, byte(v), byte(v>>8), byte(v>>16)) }

// Uint32 writes a little-endian uint32.
func (e *Encoder) Uint32(v uint32) { e.buf = binary.LittleEndian.AppendUint32(e.buf, v) }

// Uint64 writes a little-endian uint64.
func (e *Encoder) Uint64(v uint64) { e.buf = binary.LittleEndian.AppendUint64(e.buf, v) }

// Int writes a signed integer, zig-zag encoded as a uint.
func (e *Encoder) Int(v int64) { e.Uint(uint64(v<<1) ^ uint64(v>>63)) }

// Bool writes one byte: 1 for true, 0 for false.
func (e *Encoder) Bool(v bool) {
	if v {
		e.buf = append(e.buf, 1)
	} else {
		e.buf = append(e.buf, 0)
	}
}

// Float64 writes a little-endian IEEE 754 float64.
func (e *Encoder) Float64(v float64) { e.Uint64(math.Float64bits(v)) }

// Buffer writes a length prefix (uint) and the bytes. A nil and an empty buffer both write length 0.
func (e *Encoder) Buffer(b []byte) {
	e.Len(len(b))
	e.buf = append(e.buf, b...)
}

// Raw writes the bytes with no length prefix.
func (e *Encoder) Raw(b []byte) { e.buf = append(e.buf, b...) }

// Fixed writes the bytes with no length prefix, for a field whose size the schema fixes.
func (e *Encoder) Fixed(b []byte) { e.Raw(b) }

// String writes a length prefix (uint) and the UTF-8 bytes of s.
func (e *Encoder) String(s string) {
	e.Len(len(s))
	e.buf = append(e.buf, s...)
}

// Len writes n as a uint, the length prefix that Buffer and String write.
func (e *Encoder) Len(n int) { e.Uint(uint64(n)) }

// Decoder reads compact-encoding values from a buffer.
type Decoder struct {
	b []byte
}

// NewDecoder returns a decoder that reads b from the start.
func NewDecoder(b []byte) *Decoder { return &Decoder{b: b} }

// Done returns ErrTrailing when bytes remain unread, and nil otherwise.
func (d *Decoder) Done() error {
	if len(d.b) > 0 {
		return ErrTrailing
	}
	return nil
}

// take returns the next n bytes and moves past them. It returns ErrOutOfBounds, and does not move,
// when fewer than n bytes remain.
func (d *Decoder) take(n int) ([]byte, error) {
	if n < 0 || n > len(d.b) {
		return nil, ErrOutOfBounds
	}
	b := d.b[:n:n]
	d.b = d.b[n:]
	return b, nil
}

// Uint reads a uint. It returns ErrOutOfBounds when the input is truncated.
func (d *Decoder) Uint() (uint64, error) {
	a, err := d.Uint8()
	if err != nil {
		return 0, err
	}
	switch a {
	case 0xfd:
		v, err := d.Uint16()
		return uint64(v), err
	case 0xfe:
		v, err := d.Uint32()
		return uint64(v), err
	case 0xff:
		return d.Uint64()
	}
	return uint64(a), nil
}

// Uint8 reads one byte.
func (d *Decoder) Uint8() (uint8, error) {
	b, err := d.take(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

// Uint16 reads a little-endian uint16.
func (d *Decoder) Uint16() (uint16, error) {
	b, err := d.take(2)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(b), nil
}

// Uint24 reads a little-endian 24-bit unsigned integer.
func (d *Decoder) Uint24() (uint32, error) {
	b, err := d.take(3)
	if err != nil {
		return 0, err
	}
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16, nil
}

// Uint32 reads a little-endian uint32.
func (d *Decoder) Uint32() (uint32, error) {
	b, err := d.take(4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b), nil
}

// Uint64 reads a little-endian uint64.
func (d *Decoder) Uint64() (uint64, error) {
	b, err := d.take(8)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(b), nil
}

// Int reads a zig-zag encoded signed integer.
func (d *Decoder) Int() (int64, error) {
	u, err := d.Uint()
	if err != nil {
		return 0, err
	}
	return int64(u>>1) ^ -int64(u&1), nil
}

// Bool reads one byte as a bool. Only the byte 1 is true, as in the reference.
func (d *Decoder) Bool() (bool, error) {
	b, err := d.take(1)
	if err != nil {
		return false, err
	}
	return b[0] == 1, nil
}

// Float64 reads a little-endian float64.
func (d *Decoder) Float64() (float64, error) {
	u, err := d.Uint64()
	if err != nil {
		return 0, err
	}
	return math.Float64frombits(u), nil
}

// Buffer reads a length-prefixed buffer. The result shares memory with the input.
func (d *Decoder) Buffer() ([]byte, error) {
	n, err := d.Uint()
	if err != nil {
		return nil, err
	}
	if n > uint64(len(d.b)) {
		return nil, ErrOutOfBounds
	}
	return d.take(int(n))
}

// String reads a length-prefixed UTF-8 string.
func (d *Decoder) String() (string, error) {
	b, err := d.Buffer()
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Fixed reads n bytes with no length prefix. The result shares memory with the input.
func (d *Decoder) Fixed(n int) ([]byte, error) { return d.take(n) }

// Len reads a length prefix (uint). It returns ErrTooLong when the length is over max.
func (d *Decoder) Len(max int) (int, error) {
	n, err := d.Uint()
	if err != nil {
		return 0, err
	}
	if max < 0 || n > uint64(max) {
		return 0, ErrTooLong
	}
	return int(n), nil
}
