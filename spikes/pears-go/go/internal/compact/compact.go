// Package compact is a throwaway port of the subset of compact-encoding (npm 3.5.2, upstream
// file index.js) that the spike needs: varint uint, fixed-width LE uint16, fixed buffers,
// length-prefixed buffers, IPv4 addresses (4 bytes dotted, then uint16 LE port) and arrays.
// Upstream: github.com/holepunchto/compact-encoding, MIT licence.
package compact

import (
	"encoding/binary"
	"errors"
	"net/netip"
)

var ErrShort = errors.New("compact: out of bounds")

// Reader decodes from B starting at P.
type Reader struct {
	B []byte
	P int
}

func (r *Reader) need(n int) error {
	if len(r.B)-r.P < n {
		return ErrShort
	}
	return nil
}

func (r *Reader) Byte() (byte, error) {
	if err := r.need(1); err != nil {
		return 0, err
	}
	v := r.B[r.P]
	r.P++
	return v, nil
}

// Uint decodes compact-encoding uint: one byte up to 0xfc, else 0xfd/0xfe/0xff prefixes for
// LE uint16/uint32/uint64.
func (r *Reader) Uint() (uint64, error) {
	a, err := r.Byte()
	if err != nil {
		return 0, err
	}
	switch {
	case a <= 0xfc:
		return uint64(a), nil
	case a == 0xfd:
		v, err := r.Uint16()
		return uint64(v), err
	case a == 0xfe:
		if err := r.need(4); err != nil {
			return 0, err
		}
		v := binary.LittleEndian.Uint32(r.B[r.P:])
		r.P += 4
		return uint64(v), nil
	default:
		if err := r.need(8); err != nil {
			return 0, err
		}
		v := binary.LittleEndian.Uint64(r.B[r.P:])
		r.P += 8
		return v, nil
	}
}

func (r *Reader) Uint16() (uint16, error) {
	if err := r.need(2); err != nil {
		return 0, err
	}
	v := binary.LittleEndian.Uint16(r.B[r.P:])
	r.P += 2
	return v, nil
}

// Fixed returns the next n bytes, aliasing the input.
func (r *Reader) Fixed(n int) ([]byte, error) {
	if err := r.need(n); err != nil {
		return nil, err
	}
	v := r.B[r.P : r.P+n]
	r.P += n
	return v, nil
}

// Buffer decodes a compact-encoding buffer: uint length, then bytes.
func (r *Reader) Buffer() ([]byte, error) {
	n, err := r.Uint()
	if err != nil {
		return nil, err
	}
	if n > uint64(len(r.B)-r.P) {
		return nil, ErrShort
	}
	return r.Fixed(int(n))
}

// IPv4 decodes an IPv4 address: 4 dotted bytes then a uint16 LE port.
func (r *Reader) IPv4() (netip.AddrPort, error) {
	ip, err := r.Fixed(4)
	if err != nil {
		return netip.AddrPort{}, err
	}
	port, err := r.Uint16()
	if err != nil {
		return netip.AddrPort{}, err
	}
	return netip.AddrPortFrom(netip.AddrFrom4([4]byte(ip)), port), nil
}

// IPv4Array decodes a uint count, then that many IPv4 addresses.
func (r *Reader) IPv4Array() ([]netip.AddrPort, error) {
	n, err := r.Uint()
	if err != nil {
		return nil, err
	}
	if n > 0x100000 {
		return nil, errors.New("compact: array is too big")
	}
	out := make([]netip.AddrPort, 0, n)
	for i := uint64(0); i < n; i++ {
		a, err := r.IPv4()
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// AppendUint appends compact-encoding uint.
func AppendUint(b []byte, n uint64) []byte {
	switch {
	case n <= 0xfc:
		return append(b, byte(n))
	case n <= 0xffff:
		return binary.LittleEndian.AppendUint16(append(b, 0xfd), uint16(n))
	case n <= 0xffffffff:
		return binary.LittleEndian.AppendUint32(append(b, 0xfe), uint32(n))
	default:
		return binary.LittleEndian.AppendUint64(append(b, 0xff), n)
	}
}

func AppendUint16(b []byte, n uint16) []byte { return binary.LittleEndian.AppendUint16(b, n) }

// AppendIPv4 appends the 4 address bytes and the LE port. The address must be IPv4.
func AppendIPv4(b []byte, a netip.AddrPort) []byte {
	ip := a.Addr().As4()
	b = append(b, ip[:]...)
	return binary.LittleEndian.AppendUint16(b, a.Port())
}
