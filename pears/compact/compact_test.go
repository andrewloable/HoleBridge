package compact

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/andrewloable/HoleBridge/internal/testvec"
)

// vectorFile mirrors spec/vectors/compact.json, written by spec/gen/compact.js.
type vectorFile struct {
	Cases        []vectorCase `json:"cases"`
	DecodeErrors []vectorCase `json:"decode_errors"`
	DecodeOnly   []vectorCase `json:"decode_only"`
}

// vectorCase is one value: the type name of the Encoder and Decoder method for it, its JSON value
// (hex for buffer and fixed), and its encoding.
type vectorCase struct {
	Name  string          `json:"name"`
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
	Hex   string          `json:"hex"`
}

// listedCases are the values compact.json must hold, as the task lists them.
var listedCases = []string{
	"uint 0", "uint 1", "uint 252", "uint 253", "uint 65535", "uint 65536",
	"uint 4294967295", "uint 4294967296", "uint 9007199254740991",
	"int -1", "int 0", "int 1", "int -2147483648",
	"uint8 0", "uint8 255", "uint16 0", "uint16 65535", "uint24 0", "uint24 16777215",
	"uint32 0", "uint32 4294967295", "uint64 0", "uint64 9007199254740991",
	"bool true", "bool false", "float64 0.5", "float64 -1e300",
	"buffer empty", "buffer 300 bytes", "string ascii", "string unicode", "fixed 32",
}

// Test case 1: compact.json holds every listed value, and every entry encodes to its hex.
func TestEncodeVectors(t *testing.T) {
	v := loadVectors(t)
	have := make(map[string]bool, len(v.Cases))
	for _, c := range v.Cases {
		have[c.Name] = true
	}
	for _, name := range listedCases {
		if !have[name] {
			t.Errorf("compact.json has no case %q", name)
		}
	}
	for _, c := range v.Cases {
		t.Run(c.Name, func(t *testing.T) {
			got := encodeCase(t, c)
			if want := testvec.Hex(t, c.Hex); !bytes.Equal(got, want) {
				t.Errorf("encode = %x, want %x", got, want)
			}
		})
	}
}

// Test case 2: every entry's hex decodes to its value, and no bytes are left over.
func TestDecodeVectors(t *testing.T) {
	for _, c := range loadVectors(t).Cases {
		t.Run(c.Name, func(t *testing.T) {
			checkDecodes(t, c)
		})
	}
}

// Test case 3: a truncated uint, buffer or string fails with ErrOutOfBounds.
func TestDecodeTruncatedFailsWithErrOutOfBounds(t *testing.T) {
	v := loadVectors(t)
	if len(v.DecodeErrors) == 0 {
		t.Fatal("compact.json has no decode_errors cases")
	}
	for _, c := range v.DecodeErrors {
		t.Run(c.Name, func(t *testing.T) {
			_, err := decodeCase(t, NewDecoder(testvec.Hex(t, c.Hex)), c)
			failIfStub(t, err)
			if !errors.Is(err, ErrOutOfBounds) {
				t.Errorf("err = %v, want ErrOutOfBounds", err)
			}
		})
	}
}

// Test case 4: Done returns ErrTrailing while bytes remain, and nil once they are all read.
func TestDoneReturnsErrTrailing(t *testing.T) {
	d := NewDecoder([]byte{0x05, 0x06})
	_, err := d.Uint8()
	must(t, err)
	if err := d.Done(); !errors.Is(err, ErrTrailing) {
		t.Errorf("Done with one byte left = %v, want ErrTrailing", err)
	}
	_, err = d.Uint8()
	must(t, err)
	must(t, d.Done())
}

// Test case 5: Len returns ErrTooLong when the length prefix is over max, and the length otherwise.
func TestLenReturnsErrTooLong(t *testing.T) {
	defer failOnPanic(t)
	var e Encoder
	e.Len(300)
	_, err := NewDecoder(e.Bytes()).Len(299)
	failIfStub(t, err)
	if !errors.Is(err, ErrTooLong) {
		t.Errorf("Len(299) err = %v, want ErrTooLong", err)
	}
	n, err := NewDecoder(e.Bytes()).Len(300)
	must(t, err)
	if n != 300 {
		t.Errorf("Len(300) = %d, want 300", n)
	}
}

// Test case 6: a non-minimal uint decodes as the reference decodes it, to the value compact.json
// records, with no bytes left over.
func TestDecodeNonMinimalUintMatchesJS(t *testing.T) {
	v := loadVectors(t)
	if len(v.DecodeOnly) == 0 {
		t.Fatal("compact.json has no decode_only cases")
	}
	for _, c := range v.DecodeOnly {
		t.Run(c.Name, func(t *testing.T) {
			checkDecodes(t, c)
		})
	}
}

// The design: a nil and an empty buffer both encode as length 0, the bytes of the empty buffer case.
func TestEncodeNilBufferAsEmpty(t *testing.T) {
	defer failOnPanic(t)
	want := namedHex(t, "buffer empty")
	for _, in := range [][]byte{nil, {}} {
		var e Encoder
		e.Buffer(in)
		if got := e.Bytes(); !bytes.Equal(got, want) {
			t.Errorf("Buffer(%v) = %x, want %x", in, got, want)
		}
	}
}

// encodeCase encodes the value of c with the Encoder method its type names.
func encodeCase(t *testing.T, c vectorCase) []byte {
	t.Helper()
	defer failOnPanic(t)
	var e Encoder
	switch c.Type {
	case "uint":
		e.Uint(valueAs[uint64](t, c))
	case "uint8":
		e.Uint8(valueAs[uint8](t, c))
	case "uint16":
		e.Uint16(valueAs[uint16](t, c))
	case "uint24":
		e.Uint24(valueAs[uint32](t, c))
	case "uint32":
		e.Uint32(valueAs[uint32](t, c))
	case "uint64":
		e.Uint64(valueAs[uint64](t, c))
	case "int":
		e.Int(valueAs[int64](t, c))
	case "bool":
		e.Bool(valueAs[bool](t, c))
	case "float64":
		e.Float64(valueAs[float64](t, c))
	case "buffer":
		e.Buffer(valueHex(t, c))
	case "string":
		e.String(valueAs[string](t, c))
	case "fixed":
		e.Fixed(valueHex(t, c))
	default:
		t.Fatalf("%s: unknown type %q", c.Name, c.Type)
	}
	return e.Bytes()
}

// decodeCase reads one value of c.Type from d with the Decoder method its type names.
func decodeCase(t *testing.T, d *Decoder, c vectorCase) (any, error) {
	t.Helper()
	switch c.Type {
	case "uint":
		return d.Uint()
	case "uint8":
		return d.Uint8()
	case "uint16":
		return d.Uint16()
	case "uint24":
		return d.Uint24()
	case "uint32":
		return d.Uint32()
	case "uint64":
		return d.Uint64()
	case "int":
		return d.Int()
	case "bool":
		return d.Bool()
	case "float64":
		return d.Float64()
	case "buffer":
		return d.Buffer()
	case "string":
		return d.String()
	case "fixed":
		return d.Fixed(len(valueHex(t, c)))
	}
	t.Fatalf("%s: unknown type %q", c.Name, c.Type)
	return nil, nil
}

// checkDecodes decodes c.Hex as c.Type, then checks the value and that no bytes are left over.
func checkDecodes(t *testing.T, c vectorCase) {
	t.Helper()
	d := NewDecoder(testvec.Hex(t, c.Hex))
	got, err := decodeCase(t, d, c)
	must(t, err)
	if want := valueOf(t, c); !sameValue(got, want) {
		t.Errorf("decoded %v, want %v", got, want)
	}
	must(t, d.Done())
}

// valueOf returns the value of c as the Go type its Decoder method returns.
func valueOf(t *testing.T, c vectorCase) any {
	t.Helper()
	switch c.Type {
	case "uint", "uint64":
		return valueAs[uint64](t, c)
	case "uint8":
		return valueAs[uint8](t, c)
	case "uint16":
		return valueAs[uint16](t, c)
	case "uint24", "uint32":
		return valueAs[uint32](t, c)
	case "int":
		return valueAs[int64](t, c)
	case "bool":
		return valueAs[bool](t, c)
	case "float64":
		return valueAs[float64](t, c)
	case "string":
		return valueAs[string](t, c)
	case "buffer", "fixed":
		return valueHex(t, c)
	}
	t.Fatalf("%s: unknown type %q", c.Name, c.Type)
	return nil
}

// sameValue reports whether got equals want. Byte slices compare by content, so nil and empty match.
func sameValue(got, want any) bool {
	if g, ok := got.([]byte); ok {
		w, _ := want.([]byte)
		return bytes.Equal(g, w)
	}
	return reflect.DeepEqual(got, want)
}

// valueAs unmarshals the JSON value of c as T.
func valueAs[T any](t *testing.T, c vectorCase) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(c.Value, &v); err != nil {
		t.Fatalf("%s: value %s does not decode as %T: %v", c.Name, c.Value, v, err)
	}
	return v
}

// valueHex returns the bytes of a buffer or fixed case, whose JSON value is hex.
func valueHex(t *testing.T, c vectorCase) []byte {
	t.Helper()
	return testvec.Hex(t, valueAs[string](t, c))
}

// namedHex returns the encoding of the case called name in compact.json.
func namedHex(t *testing.T, name string) []byte {
	t.Helper()
	for _, c := range loadVectors(t).Cases {
		if c.Name == name {
			return testvec.Hex(t, c.Hex)
		}
	}
	t.Fatalf("compact.json has no case %q", name)
	return nil
}

// loadVectors reads spec/vectors/compact.json.
func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	var v vectorFile
	testvec.Load(t, "compact.json", &v)
	return v
}

// failOnPanic turns a panic from a stub into a test failure, so the other tests still run.
// Defer it first in a test that calls a stub which panics.
func failOnPanic(t *testing.T) {
	t.Helper()
	if r := recover(); r != nil {
		t.Fatalf("%v", r)
	}
}

// failIfStub fails the test as not implemented when err is the errors.ErrUnsupported a stub returns.
func failIfStub(t *testing.T, err error) {
	t.Helper()
	if errors.Is(err, errors.ErrUnsupported) {
		t.Fatal("not implemented")
	}
}

// must fails the test on any error, reporting a stub's errors.ErrUnsupported as not implemented.
func must(t *testing.T, err error) {
	t.Helper()
	failIfStub(t, err)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Raw and Fixed write their bytes with no length prefix.
func TestRawAndFixedHaveNoPrefix(t *testing.T) {
	var e Encoder
	e.Raw([]byte{1, 2})
	e.Fixed([]byte{3})
	if got, want := e.Bytes(), []byte{1, 2, 3}; !bytes.Equal(got, want) {
		t.Errorf("Raw then Fixed = %x, want %x", got, want)
	}
}

// Bool reads only the byte 1 as true. Any other byte reads as false, as the reference does.
func TestDecodeBoolOnlyOneIsTrue(t *testing.T) {
	for _, b := range []byte{0x00, 0x02, 0xff} {
		got, err := NewDecoder([]byte{b}).Bool()
		must(t, err)
		if got {
			t.Errorf("Bool(%#x) = true, want false", b)
		}
	}
}

// Int and Uint round trip across the full Go range, past the reference's safe-integer limits.
func TestIntAndUintRoundTripFullRange(t *testing.T) {
	for _, v := range []int64{math.MinInt64, -1, 0, math.MaxInt64} {
		var e Encoder
		e.Int(v)
		got, err := NewDecoder(e.Bytes()).Int()
		must(t, err)
		if got != v {
			t.Errorf("Int(%d) decoded as %d", v, got)
		}
	}
	for _, v := range []uint64{0xfc, 0xfd, 1 << 32, math.MaxUint64} {
		var e Encoder
		e.Uint(v)
		got, err := NewDecoder(e.Bytes()).Uint()
		must(t, err)
		if got != v {
			t.Errorf("Uint(%d) decoded as %d", v, got)
		}
	}
}
