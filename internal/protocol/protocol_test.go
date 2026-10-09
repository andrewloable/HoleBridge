package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/andrewloable/HoleBridge/internal/testvec"
)

// frame is one entry of spec/vectors/frames.json, written by spec/gen/frames.js. Index is nil for the
// handshake and the unordered datagram. Value is set on valid frames and Error on invalid ones.
type frame struct {
	Name  string          `json:"name"`
	Index *int            `json:"index"`
	Value json.RawMessage `json:"value"`
	Hex   string          `json:"hex"`
	Error string          `json:"error"`
}

// frameTypes gives the Go type each frame name decodes into.
var frameTypes = map[string]any{
	"handshake":          Handshake{},
	"handshake-lan":      Handshake{},
	"open":               Open{},
	"opened":             Opened{},
	"reject":             Reject{},
	"data":               Data{},
	"window":             Window{},
	"close":              Close{},
	"reattach":           Reattach{},
	"reattached":         Reattached{},
	"services":           Services{},
	"flow":               Flow{},
	"datagram":           Datagram{},
	"unordered-datagram": Datagram{},
}

// frameErrors maps the error names in frames.json to the error each invalid frame must return.
var frameErrors = map[string]error{
	"truncated":         ErrTruncated,
	"trailing bytes":    ErrTrailing,
	"unknown kind":      ErrInvalid,
	"empty payload":     ErrInvalid,
	"payload too large": ErrInvalid,
}

// Test case 1: every valid frame in frames.json encodes to its hex.
func TestEncodeValidFrames(t *testing.T) {
	for _, f := range loadFrames(t) {
		if f.Error != "" {
			continue
		}
		t.Run(f.Name, func(t *testing.T) {
			defer failOnPanic(t)
			got := encodeFrame(t, f, frameValue(t, f))
			if want := testvec.Hex(t, f.Hex); !bytes.Equal(got, want) {
				t.Errorf("encode = %.32x (%d bytes), want %.32x (%d bytes)", got, len(got), want, len(want))
			}
		})
	}
}

// Test case 2: every valid frame's hex decodes to its value.
func TestDecodeValidFrames(t *testing.T) {
	for _, f := range loadFrames(t) {
		if f.Error != "" {
			continue
		}
		t.Run(f.Name, func(t *testing.T) {
			got, err := decodeFrame(f, testvec.Hex(t, f.Hex))
			must(t, err)
			if want := frameValue(t, f); !sameMessage(got, want) {
				t.Errorf("decoded value differs from the value in frames.json")
			}
		})
	}
}

// Test case 3: every invalid frame fails with the error its entry names.
func TestDecodeInvalidFramesFail(t *testing.T) {
	for _, f := range loadFrames(t) {
		if f.Error == "" {
			continue
		}
		want, ok := frameErrors[f.Error]
		if !ok {
			t.Fatalf("frames.json entry %q has error %q, which this test does not map", f.Name, f.Error)
		}
		t.Run(f.Name+" "+f.Error, func(t *testing.T) {
			_, err := decodeFrame(f, testvec.Hex(t, f.Hex))
			failIfStub(t, err)
			if !errors.Is(err, want) {
				t.Errorf("err = %v, want %v", err, want)
			}
		})
	}
}

// Test case 4: Decode with index 11 returns ErrInvalid.
func TestDecodeUnknownIndexIsInvalid(t *testing.T) {
	_, err := Decode(11, []byte{0x00})
	failIfStub(t, err)
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("Decode(11) err = %v, want ErrInvalid", err)
	}
}

// Test case 5: a handshake without FlagLAN encodes no lan bytes, and with FlagLAN its addresses
// round-trip.
func TestHandshakeLANOnlyWithFlagLAN(t *testing.T) {
	defer failOnPanic(t)
	// The LAN is set but FlagLAN is not, so the frame is the plain handshake with no lan bytes.
	plain := Handshake{Version: 1, Flags: FlagDatagrams, LAN: &LAN{Addresses: []string{"192.0.2.10"}, Port: 65535}}
	got := EncodeHandshake(plain)
	if want := testvec.Hex(t, frameNamed(t, "handshake", 0).Hex); !bytes.Equal(got, want) {
		t.Errorf("EncodeHandshake without FlagLAN = %x, want %x", got, want)
	}

	// With FlagLAN the frame carries the addresses, and decoding gives them back.
	lan := frameNamed(t, "handshake-lan", 0)
	value := frameValue(t, lan).(Handshake)
	if got := EncodeHandshake(value); !bytes.Equal(got, testvec.Hex(t, lan.Hex)) {
		t.Errorf("EncodeHandshake with FlagLAN = %x, want %x", got, testvec.Hex(t, lan.Hex))
	}
	back, err := DecodeHandshake(testvec.Hex(t, lan.Hex))
	must(t, err)
	if !sameMessage(back, value) {
		t.Errorf("DecodeHandshake with FlagLAN = %+v, want %+v", back, value)
	}
}

// Test case 6: a 65536-byte data payload round-trips, and a 65537-byte one is ErrInvalid.
func TestDataPayloadLimit(t *testing.T) {
	big := frameNamed(t, "data", 1)
	data := frameValue(t, big).(Data)
	if len(data.Payload) != 65536 {
		t.Fatalf("frames.json data payload is %d bytes, want 65536", len(data.Payload))
	}
	enc, err := Encode(data)
	must(t, err)
	if want := testvec.Hex(t, big.Hex); !bytes.Equal(enc, want) {
		t.Errorf("Encode of the 65536-byte payload = %d bytes, want %d bytes", len(enc), len(want))
	}
	back, err := Decode(3, enc)
	must(t, err)
	if !sameMessage(back, data) {
		t.Errorf("the 65536-byte payload did not round-trip")
	}

	// The header of the too-large frame in frames.json, then 65537 payload bytes.
	tooLarge := frameNamed(t, "data", 3)
	raw := append(testvec.Hex(t, tooLarge.Hex), make([]byte, 65537)...)
	_, err = Decode(3, raw)
	failIfStub(t, err)
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("Decode of a 65537-byte payload err = %v, want ErrInvalid", err)
	}
	_, err = Encode(Data{Stream: 252, Payload: make([]byte, 65537)})
	failIfStub(t, err)
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("Encode of a 65537-byte payload err = %v, want ErrInvalid", err)
	}
}

// Index is not among the six TEST CASES. Its values are the message indexes in frames.json, which
// the channel carries and the protocol freezes.
func TestIndexMatchesFrames(t *testing.T) {
	for _, f := range loadFrames(t) {
		if f.Error != "" || f.Index == nil {
			continue
		}
		t.Run(f.Name, func(t *testing.T) {
			defer failOnPanic(t)
			if got := Index(frameValue(t, f)); got != *f.Index {
				t.Errorf("Index = %d, want %d", got, *f.Index)
			}
		})
	}
}

// Edge cases the frames do not cover: a kind of 256 must not wrap to 0 when narrowed to a Kind, a
// services count far beyond the frame must fail instead of looping, and Encode refuses an empty data
// payload, which the wire cannot carry.
func TestDecodeEdgeCases(t *testing.T) {
	// Handshake with flags 0 and one service "a" of kind 256 (fd 00 01), port 0, no origins.
	_, err := DecodeHandshake([]byte{0x01, 0x00, 0x01, 0x01, 'a', 0xfd, 0x00, 0x01, 0x00, 0x00})
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("kind 256: err = %v, want ErrInvalid", err)
	}
	// A services list whose count is 2^64-1, and the frame ends after the count.
	_, err = Decode(8, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	if !errors.Is(err, ErrTruncated) {
		t.Errorf("huge services count: err = %v, want ErrTruncated", err)
	}
	if _, err := Encode(Data{Stream: 1}); !errors.Is(err, ErrInvalid) {
		t.Errorf("Encode of an empty data payload: err = %v, want ErrInvalid", err)
	}
}

// encodeFrame encodes v, the value of f, the way the frame's entry says: messages with Encode, the
// handshake with EncodeHandshake and the unordered datagram with EncodeUnordered.
func encodeFrame(t *testing.T, f frame, v any) []byte {
	t.Helper()
	switch {
	case f.Index != nil:
		b, err := Encode(v)
		must(t, err)
		return b
	case f.Name == "unordered-datagram":
		return EncodeUnordered(v.(Datagram))
	}
	return EncodeHandshake(v.(Handshake))
}

// decodeFrame decodes b the way the frame's entry says: messages with Decode, the handshake with
// DecodeHandshake and the unordered datagram with DecodeUnordered.
func decodeFrame(f frame, b []byte) (any, error) {
	switch {
	case f.Index != nil:
		return Decode(*f.Index, b)
	case f.Name == "unordered-datagram":
		return DecodeUnordered(b)
	}
	return DecodeHandshake(b)
}

// frameValue returns the value of a valid frame as the Go type its name gives, filled from its JSON.
func frameValue(t *testing.T, f frame) any {
	t.Helper()
	typ, ok := frameTypes[f.Name]
	if !ok {
		t.Fatalf("frames.json frame %q has no Go type in this test", f.Name)
	}
	var j any
	if err := json.Unmarshal(f.Value, &j); err != nil {
		t.Fatalf("frames.json frame %q has a value that does not decode: %v", f.Name, err)
	}
	v := reflect.New(reflect.TypeOf(typ)).Elem()
	fill(t, v, j)
	return v.Interface()
}

// fill sets v from j, a value decoded from JSON. Object keys match field names ignoring case,
// numbers fill integers, lists fill slices, and hex strings fill byte slices and byte arrays.
func fill(t *testing.T, v reflect.Value, j any) {
	t.Helper()
	switch v.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(uint64(j.(float64)))
	case reflect.String:
		v.SetString(j.(string))
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		fill(t, p.Elem(), j)
		v.Set(p)
	case reflect.Array:
		reflect.Copy(v, reflect.ValueOf(testvec.Hex(t, j.(string))))
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			v.SetBytes(testvec.Hex(t, j.(string)))
			return
		}
		items := j.([]any)
		s := reflect.MakeSlice(v.Type(), len(items), len(items))
		for i, item := range items {
			fill(t, s.Index(i), item)
		}
		v.Set(s)
	case reflect.Struct:
		for key, val := range j.(map[string]any) {
			fill(t, v.FieldByNameFunc(func(name string) bool { return strings.EqualFold(name, key) }), val)
		}
	}
}

// sameMessage reports whether got and want are the same type with the same fields.
func sameMessage(got, want any) bool {
	return reflect.TypeOf(got) == reflect.TypeOf(want) && sameValue(reflect.ValueOf(got), reflect.ValueOf(want))
}

// sameValue compares field by field. A nil list and an empty one are the same, since the wire
// cannot tell them apart.
func sameValue(got, want reflect.Value) bool {
	switch got.Kind() {
	case reflect.Pointer:
		if got.IsNil() || want.IsNil() {
			return got.IsNil() == want.IsNil()
		}
		return sameValue(got.Elem(), want.Elem())
	case reflect.Slice:
		if got.Len() != want.Len() {
			return false
		}
		for i := 0; i < got.Len(); i++ {
			if !sameValue(got.Index(i), want.Index(i)) {
				return false
			}
		}
		return true
	case reflect.Struct:
		for i := 0; i < got.NumField(); i++ {
			if !sameValue(got.Field(i), want.Field(i)) {
				return false
			}
		}
		return true
	}
	return got.Interface() == want.Interface()
}

// frameNamed returns the n-th frame called name in frames.json, counting from 0 across valid and
// invalid frames.
func frameNamed(t *testing.T, name string, n int) frame {
	t.Helper()
	k := 0
	for _, f := range loadFrames(t) {
		if f.Name != name {
			continue
		}
		if k == n {
			return f
		}
		k++
	}
	t.Fatalf("frames.json has no frame %d named %q", n, name)
	return frame{}
}

// loadFrames reads spec/vectors/frames.json. testvec reports a missing file by name.
func loadFrames(t *testing.T) []frame {
	t.Helper()
	var frames []frame
	testvec.Load(t, "frames.json", &frames)
	return frames
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
