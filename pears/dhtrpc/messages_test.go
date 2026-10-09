package dhtrpc

import (
	"bytes"
	"errors"
	"math/rand/v2"
	"net/netip"
	"reflect"
	"testing"

	"github.com/andrewloable/HoleBridge/internal/testvec"
)

// Byte 0 of the two packet kinds: (type << 4) | 3.
const (
	requestType  = 0x03
	responseType = 0x13
)

// vectorFile mirrors spec/vectors/dhtrpc.json, written by spec/gen/dhtrpc.js. Its host and port keys
// decode into Addr, whose fields match them.
type vectorFile struct {
	Requests  []requestVector  `json:"requests"`
	Responses []responseVector `json:"responses"`
}

// requestVector is one request. An optional field is hex, and empty when the packet does not carry it.
type requestVector struct {
	Name     string `json:"name"`
	Tid      uint16 `json:"tid"`
	To       Addr   `json:"to"`
	Internal bool   `json:"internal"`
	Command  uint64 `json:"command"`
	ID       string `json:"id"`
	Token    string `json:"token"`
	Target   string `json:"target"`
	Value    string `json:"value"`
	Hex      string `json:"hex"`
}

// responseVector is one reply, with the same convention for optional fields.
type responseVector struct {
	Name        string `json:"name"`
	Tid         uint16 `json:"tid"`
	To          Addr   `json:"to"`
	ID          string `json:"id"`
	Token       string `json:"token"`
	CloserNodes []Addr `json:"closer_nodes"`
	Error       uint64 `json:"error"`
	Value       string `json:"value"`
	Hex         string `json:"hex"`
}

// packet returns the Request that v describes. Absent optional fields are nil.
func (v requestVector) packet(t *testing.T) Request {
	t.Helper()
	return Request{
		Tid:      v.Tid,
		To:       v.To,
		ID:       optionalHex(t, v.ID),
		Token:    optionalHex(t, v.Token),
		Internal: v.Internal,
		Command:  v.Command,
		Target:   optionalHex(t, v.Target),
		Value:    optionalHex(t, v.Value),
	}
}

// packet returns the Response that v describes. Absent optional fields are nil.
func (v responseVector) packet(t *testing.T) Response {
	t.Helper()
	return Response{
		Tid:         v.Tid,
		To:          v.To,
		ID:          optionalHex(t, v.ID),
		Token:       optionalHex(t, v.Token),
		CloserNodes: v.CloserNodes,
		Error:       v.Error,
		Value:       optionalHex(t, v.Value),
	}
}

// optionalHex decodes s, or returns nil when the field is absent.
func optionalHex(t *testing.T, s string) []byte {
	t.Helper()
	if s == "" {
		return nil
	}
	return testvec.Hex(t, s)
}

// loadVectors reads spec/vectors/dhtrpc.json.
func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	var v vectorFile
	testvec.Load(t, "dhtrpc.json", &v)
	return v
}

// encodeRequest calls EncodeRequest. A panic fails this test, not the whole test binary.
func encodeRequest(t *testing.T, r Request) []byte {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("EncodeRequest: %v", p)
		}
	}()
	return EncodeRequest(r)
}

// encodeResponse calls EncodeResponse. A panic fails this test, not the whole test binary.
func encodeResponse(t *testing.T, r Response) []byte {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("EncodeResponse: %v", p)
		}
	}()
	return EncodeResponse(r)
}

// decodePacket calls Decode on b. A panic fails the test. The stub returns errors.ErrUnsupported,
// which the tests report as not implemented; the codec must return its own error for bad input.
func decodePacket(t *testing.T, b []byte) (any, error) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("Decode(%x) panicked: %v", b, p)
		}
	}()
	v, err := Decode(b)
	if errors.Is(err, errors.ErrUnsupported) {
		t.Fatal("Decode: not implemented")
	}
	return v, err
}

// Test case 1 (coverage): dhtrpc.json has requests with and without each optional field, and
// responses with closer nodes, an error code and a value.
func TestVectorCoverage(t *testing.T) {
	v := loadVectors(t)
	seen := map[string]map[bool]bool{"id": {}, "token": {}, "target": {}, "value": {}}
	for _, r := range v.Requests {
		seen["id"][r.ID != ""] = true
		seen["token"][r.Token != ""] = true
		seen["target"][r.Target != ""] = true
		seen["value"][r.Value != ""] = true
	}
	for field, has := range seen {
		if !has[true] || !has[false] {
			t.Errorf("requests must include both with and without %s", field)
		}
	}
	var closer, errCode, value bool
	for _, r := range v.Responses {
		closer = closer || len(r.CloserNodes) > 0
		errCode = errCode || r.Error != 0
		value = value || r.Value != ""
	}
	if !closer {
		t.Error("responses must include one with closer nodes")
	}
	if !errCode {
		t.Error("responses must include one with an error code")
	}
	if !value {
		t.Error("responses must include one with a value")
	}
}

// Test case 1: every entry encodes to its hex.
func TestEncodeVectors(t *testing.T) {
	v := loadVectors(t)
	for _, r := range v.Requests {
		t.Run("request "+r.Name, func(t *testing.T) {
			got := encodeRequest(t, r.packet(t))
			if want := testvec.Hex(t, r.Hex); !bytes.Equal(got, want) {
				t.Errorf("EncodeRequest = %x, want %x", got, want)
			}
		})
	}
	for _, r := range v.Responses {
		t.Run("response "+r.Name, func(t *testing.T) {
			got := encodeResponse(t, r.packet(t))
			if want := testvec.Hex(t, r.Hex); !bytes.Equal(got, want) {
				t.Errorf("EncodeResponse = %x, want %x", got, want)
			}
		})
	}
}

// Test case 2: every entry decodes to its value.
func TestDecodeVectors(t *testing.T) {
	v := loadVectors(t)
	for _, r := range v.Requests {
		t.Run("request "+r.Name, func(t *testing.T) {
			want := r.packet(t)
			got, err := decodePacket(t, testvec.Hex(t, r.Hex))
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if !reflect.DeepEqual(got, &want) {
				t.Errorf("Decode = %+v, want %+v", got, &want)
			}
		})
	}
	for _, r := range v.Responses {
		t.Run("response "+r.Name, func(t *testing.T) {
			want := r.packet(t)
			got, err := decodePacket(t, testvec.Hex(t, r.Hex))
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if !reflect.DeepEqual(got, &want) {
				t.Errorf("Decode = %+v, want %+v", got, &want)
			}
		})
	}
}

// Test case 3: a packet whose type byte is neither the request byte nor the response byte returns an
// error. Every other byte value is tried in front of a request and in front of a reply.
func TestDecodeUnknownType(t *testing.T) {
	v := loadVectors(t)
	bases := [][]byte{testvec.Hex(t, v.Requests[0].Hex), testvec.Hex(t, v.Responses[0].Hex)}
	for _, base := range bases {
		for b := 0; b < 256; b++ {
			if b == requestType || b == responseType {
				continue
			}
			packet := append([]byte{byte(b)}, base[1:]...)
			if _, err := decodePacket(t, packet); err == nil {
				t.Errorf("Decode with type byte 0x%02x returned no error", b)
			}
		}
	}
}

// Test case 4: truncated packets return an error and never panic. The test cuts 1000 random lengths
// from the vector packets, from a fixed seed so that a failure repeats.
func TestDecodeTruncated(t *testing.T) {
	v := loadVectors(t)
	var packets [][]byte
	for _, r := range v.Requests {
		packets = append(packets, testvec.Hex(t, r.Hex))
	}
	for _, r := range v.Responses {
		packets = append(packets, testvec.Hex(t, r.Hex))
	}
	rng := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 1000; i++ {
		p := packets[rng.IntN(len(packets))]
		n := rng.IntN(len(p)) // 0 to len(p)-1, so always a strict prefix
		if _, err := decodePacket(t, p[:n]); err == nil {
			t.Fatalf("Decode of the first %d of %d bytes returned no error: %x", n, len(p), p[:n])
		}
	}
}

// Edge case: an encoder given a host that is not IPv4, or a fixed field that is not 32 bytes, panics
// rather than write a packet that the decoder would misread.
func TestEncodeRejectsBadFields(t *testing.T) {
	ok := Addr{Host: netip.MustParseAddr("192.0.2.1"), Port: 1}
	short := make([]byte, 31)
	long := make([]byte, 33)
	cases := map[string]func(){
		"request ipv6 host":  func() { EncodeRequest(Request{To: Addr{Host: netip.MustParseAddr("::1")}}) },
		"request short id":   func() { EncodeRequest(Request{To: ok, ID: short}) },
		"request long token": func() { EncodeRequest(Request{To: ok, Token: long}) },
		"request short target": func() {
			EncodeRequest(Request{To: ok, Target: short})
		},
		"response zero host": func() { EncodeResponse(Response{}) },
		"response long id":   func() { EncodeResponse(Response{To: ok, ID: long}) },
		"response closer ipv6": func() {
			EncodeResponse(Response{To: ok, CloserNodes: []Addr{{Host: netip.MustParseAddr("::1")}}})
		},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("encoder did not panic")
				}
			}()
			f()
		})
	}
}
