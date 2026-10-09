package hyperdht

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/netip"
	"reflect"
	"testing"

	"github.com/andrewloable/HoleBridge/internal/testvec"
	"github.com/andrewloable/HoleBridge/pears/compact"
)

// vectorFile mirrors spec/vectors/hyperdht.json, written by spec/gen/hyperdht.js.
type vectorFile struct {
	Cases []vectorCase `json:"cases"`
}

// vectorCase is one value of one encoding: the encoding's type name, its JSON value (binary fields
// as base64) and its encoding as hex.
type vectorCase struct {
	Name  string          `json:"name"`
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
	Hex   string          `json:"hex"`
}

// listedTypes are the encodings of messages.js the vectors must cover, with at least two values each.
var listedTypes = []string{
	"address", "handshake", "holepunch", "holepunchPayload", "noisePayload",
	"peer", "peers", "lookupRawReply", "announce",
}

// codec is the Go encoder and decoder of one encoding. value decodes a JSON value as the Go type,
// for comparison with a decoded value.
type codec struct {
	value  func(raw json.RawMessage) (any, error)
	encode func(raw json.RawMessage) ([]byte, error)
	decode func(b []byte) (any, error)
}

// codecs maps each vector type to its Go codec.
var codecs = map[string]codec{
	"address":          typeCodec(EncodeAddress, DecodeAddress),
	"handshake":        typeCodec(EncodeHandshake, DecodeHandshake),
	"holepunch":        typeCodec(EncodeHolepunch, DecodeHolepunch),
	"holepunchPayload": typeCodec(EncodeHolepunchPayload, DecodeHolepunchPayload),
	"noisePayload":     typeCodec(EncodeNoisePayload, DecodeNoisePayload),
	"peer":             typeCodec(EncodePeer, DecodePeer),
	"peers":            typeCodec(EncodePeers, DecodePeers),
	"lookupRawReply":   typeCodec(EncodeLookupRawReply, DecodeLookupRawReply),
	"announce":         typeCodec(EncodeAnnounce, DecodeAnnounce),
}

// typeCodec builds the codec of the Go type T from its encoder and decoder.
func typeCodec[T any](enc func(T) ([]byte, error), dec func([]byte) (T, error)) codec {
	return codec{
		value: func(raw json.RawMessage) (any, error) {
			var v T
			err := json.Unmarshal(raw, &v)
			return v, err
		},
		encode: func(raw json.RawMessage) ([]byte, error) {
			var v T
			if err := json.Unmarshal(raw, &v); err != nil {
				return nil, err
			}
			return enc(v)
		},
		decode: func(b []byte) (any, error) {
			v, err := dec(b)
			return v, err
		},
	}
}

// Test case 1: hyperdht.json holds at least two values of every listed encoding, and each value
// encodes to its hex.
func TestEncodeVectors(t *testing.T) {
	v := loadVectors(t)
	count := make(map[string]int, len(v.Cases))
	for _, c := range v.Cases {
		count[c.Type]++
	}
	for _, typ := range listedTypes {
		if count[typ] < 2 {
			t.Errorf("hyperdht.json has %d values of %q, want at least 2", count[typ], typ)
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

// Test case 2: every hex decodes to the value of its case.
func TestDecodeVectors(t *testing.T) {
	for _, c := range loadVectors(t).Cases {
		t.Run(c.Name, func(t *testing.T) {
			cd := codecFor(t, c)
			got, err := cd.decode(testvec.Hex(t, c.Hex))
			must(t, err)
			want, err := cd.value(c.Value)
			must(t, err)
			if !sameValue(got, want) {
				t.Errorf("decoded %+v, want %+v", got, want)
			}
		})
	}
}

// Test case 3: every strict prefix of an encoding fails with compact.ErrOutOfBounds. The one
// exception is a lookupRawReply prefix that ends before its bump: the reference reads a missing
// bump as 0, so that prefix decodes with Bump 0.
func TestDecodeTruncatedFailsWithErrOutOfBounds(t *testing.T) {
	for _, c := range loadVectors(t).Cases {
		t.Run(c.Name, func(t *testing.T) {
			cd := codecFor(t, c)
			full := testvec.Hex(t, c.Hex)
			skip := -1
			if c.Type == "lookupRawReply" {
				skip = len(full) - bumpSize(t, c)
			}
			for n := 0; n < len(full); n++ {
				if n == skip {
					got, err := cd.decode(full[:n])
					must(t, err)
					if bump := got.(LookupRawReply).Bump; bump != 0 {
						t.Errorf("prefix without bump: Bump = %d, want 0", bump)
					}
					continue
				}
				_, err := cd.decode(full[:n])
				failIfStub(t, err)
				if !errors.Is(err, compact.ErrOutOfBounds) {
					t.Fatalf("prefix of %d of %d bytes: err = %v, want ErrOutOfBounds", n, len(full), err)
				}
			}
		})
	}
}

// encodeCase encodes the value of c with the encoder of its type.
func encodeCase(t *testing.T, c vectorCase) []byte {
	t.Helper()
	got, err := codecFor(t, c).encode(c.Value)
	must(t, err)
	return got
}

// codecFor returns the codec of the type c names. It fails the test for an unknown type.
func codecFor(t *testing.T, c vectorCase) codec {
	t.Helper()
	cd, ok := codecs[c.Type]
	if !ok {
		t.Fatalf("%s: unknown type %q", c.Name, c.Type)
	}
	return cd
}

// bumpSize returns the encoded length of the bump of a lookupRawReply case.
func bumpSize(t *testing.T, c vectorCase) int {
	t.Helper()
	var v LookupRawReply
	if err := json.Unmarshal(c.Value, &v); err != nil {
		t.Fatalf("%s: %v", c.Name, err)
	}
	var e compact.Encoder
	e.Uint(v.Bump)
	return len(e.Bytes())
}

// addrType is the type of a Host, which sameValue compares as a whole.
var addrType = reflect.TypeOf(netip.Addr{})

// sameValue reports whether got and want hold the same data. A nil slice equals an empty one, so
// the test does not pin how an absent or empty list decodes. The encode tests pin that.
func sameValue(got, want any) bool {
	return sameReflect(reflect.ValueOf(got), reflect.ValueOf(want))
}

func sameReflect(a, b reflect.Value) bool {
	if a.Type() != b.Type() {
		return false
	}
	if a.Type() == addrType {
		return a.Interface() == b.Interface()
	}
	switch a.Kind() {
	case reflect.Pointer:
		if a.IsNil() || b.IsNil() {
			return a.IsNil() == b.IsNil()
		}
		return sameReflect(a.Elem(), b.Elem())
	case reflect.Slice:
		if a.Len() != b.Len() {
			return false
		}
		for i := range a.Len() {
			if !sameReflect(a.Index(i), b.Index(i)) {
				return false
			}
		}
		return true
	case reflect.Struct:
		for i := range a.NumField() {
			if !sameReflect(a.Field(i), b.Field(i)) {
				return false
			}
		}
		return true
	default:
		return a.Interface() == b.Interface()
	}
}

// loadVectors reads spec/vectors/hyperdht.json.
func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	var v vectorFile
	testvec.Load(t, "hyperdht.json", &v)
	return v
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
