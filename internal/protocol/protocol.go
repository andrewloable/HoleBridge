// Package protocol is the Go codec for HoleBridge protocol v1: the handshake, the messages on the
// session channel and the unordered datagram. The layout is docs/architecture.md, "Wire protocol v1"
// and "Encoding"; the golden frames are spec/vectors/frames.json.
package protocol

import (
	"errors"

	"github.com/andrewloable/HoleBridge/pears/compact"
)

// maxPayload is the largest data payload in bytes.
const maxPayload = 65536

// Errors returned by Decode and its variants.
var (
	// ErrTruncated means the frame ends before its last field.
	ErrTruncated = errors.New("protocol: truncated frame")
	// ErrTrailing means bytes remain after the last field.
	ErrTrailing = errors.New("protocol: trailing bytes")
	// ErrInvalid means a value the protocol does not allow: an unknown index, a service kind above 4,
	// or a data payload of 0 or more than 65536 bytes.
	ErrInvalid = errors.New("protocol: invalid frame")
)

// Kind is the kind of a service on the wire: 0 unknown, 1 https, 2 http, 3 tcp, 4 udp.
type Kind uint8

// The service kinds.
const (
	KindUnknown Kind = 0
	KindHTTPS   Kind = 1
	KindHTTP    Kind = 2
	KindTCP     Kind = 3
	KindUDP     Kind = 4
)

// Handshake flags: bit 0 stream resume, bit 1 lan present, bit 2 unordered datagrams.
const (
	FlagResume    = 1
	FlagLAN       = 2
	FlagDatagrams = 4
)

// Service is one entry of a services list.
type Service struct {
	Name    string
	Kind    Kind
	Port    uint64
	Origins []string
}

// LAN is the host's LAN addresses and port, present when FlagLAN is set.
type LAN struct {
	Addresses []string
	Port      uint64
}

// Handshake is the first frame each side sends when the channel opens.
type Handshake struct {
	Version  uint64
	Flags    uint64
	Services []Service
	LAN      *LAN
}

// Open is message 0: the app opens a stream to a service.
type Open struct {
	Stream  uint64
	Service string
	Window  uint64
}

// Opened is message 1: the host accepts a stream. Token is zero when resume is off.
type Opened struct {
	Stream uint64
	Window uint64
	Token  [16]byte
}

// Reject is message 2: the host refuses a stream.
type Reject struct {
	Stream uint64
	Code   uint64
	Reason string
}

// Data is message 3: payload bytes on a stream, 1 to 65536 of them.
type Data struct {
	Stream  uint64
	Payload []byte
}

// Window is message 4: a credit grant and the total bytes received so far.
type Window struct {
	Stream   uint64
	Credit   uint64
	Received uint64
}

// Close is message 5: the stream is finished.
type Close struct {
	Stream uint64
}

// Reattach is message 6: the app asks to resume a stream.
type Reattach struct {
	Stream   uint64
	Token    [16]byte
	Received uint64
	Limit    uint64
}

// Reattached is message 7: the host accepts a resumed stream.
type Reattached struct {
	Stream   uint64
	Received uint64
	Limit    uint64
}

// Services is message 8: the new service list after a config reload.
type Services struct {
	Services []Service
}

// Flow is message 9: the app opens a UDP flow with its first datagram.
type Flow struct {
	Flow    uint64
	Service string
	Payload []byte
}

// Datagram is message 10, and also the unordered datagram that rides outside the channel.
type Datagram struct {
	Flow    uint64
	Payload []byte
}

// Index returns the message index, 0 to 10, of m, which must be one of the message structs. It
// panics for any other value.
func Index(m any) int {
	switch m.(type) {
	case Open:
		return 0
	case Opened:
		return 1
	case Reject:
		return 2
	case Data:
		return 3
	case Window:
		return 4
	case Close:
		return 5
	case Reattach:
		return 6
	case Reattached:
		return 7
	case Services:
		return 8
	case Flow:
		return 9
	case Datagram:
		return 10
	}
	panic("protocol: Index of a value that is not a message")
}

// Encode returns the frame of the message m, one of the message structs. The frame carries the
// fields only: the message index is the channel's, given by Index. A data payload of 0 or more than
// 65536 bytes is ErrInvalid.
func Encode(m any) ([]byte, error) {
	var e compact.Encoder
	switch m := m.(type) {
	case Open:
		e.Uint(m.Stream)
		e.String(m.Service)
		e.Uint(m.Window)
	case Opened:
		e.Uint(m.Stream)
		e.Uint(m.Window)
		e.Fixed(m.Token[:])
	case Reject:
		e.Uint(m.Stream)
		e.Uint(m.Code)
		e.String(m.Reason)
	case Data:
		if n := len(m.Payload); n == 0 || n > maxPayload {
			return nil, ErrInvalid
		}
		e.Uint(m.Stream)
		e.Buffer(m.Payload)
	case Window:
		e.Uint(m.Stream)
		e.Uint(m.Credit)
		e.Uint(m.Received)
	case Close:
		e.Uint(m.Stream)
	case Reattach:
		e.Uint(m.Stream)
		e.Fixed(m.Token[:])
		e.Uint(m.Received)
		e.Uint(m.Limit)
	case Reattached:
		e.Uint(m.Stream)
		e.Uint(m.Received)
		e.Uint(m.Limit)
	case Services:
		encodeServices(&e, m.Services)
	case Flow:
		e.Uint(m.Flow)
		e.String(m.Service)
		e.Buffer(m.Payload)
	case Datagram:
		e.Uint(m.Flow)
		e.Buffer(m.Payload)
	default:
		return nil, ErrInvalid
	}
	return e.Bytes(), nil
}

// Decode returns the message with the given index, decoded from b, the frame without the index. An
// index outside 0 to 10 is ErrInvalid.
func Decode(index int, b []byte) (any, error) {
	r := reader{d: compact.NewDecoder(b)}
	var m any
	switch index {
	case 0:
		m = Open{Stream: r.uint(), Service: r.str(), Window: r.uint()}
	case 1:
		m = Opened{Stream: r.uint(), Window: r.uint(), Token: r.token()}
	case 2:
		m = Reject{Stream: r.uint(), Code: r.uint(), Reason: r.str()}
	case 3:
		m = Data{Stream: r.uint(), Payload: r.data()}
	case 4:
		m = Window{Stream: r.uint(), Credit: r.uint(), Received: r.uint()}
	case 5:
		m = Close{Stream: r.uint()}
	case 6:
		m = Reattach{Stream: r.uint(), Token: r.token(), Received: r.uint(), Limit: r.uint()}
	case 7:
		m = Reattached{Stream: r.uint(), Received: r.uint(), Limit: r.uint()}
	case 8:
		m = Services{Services: r.services()}
	case 9:
		m = Flow{Flow: r.uint(), Service: r.str(), Payload: r.buffer()}
	case 10:
		m = Datagram{Flow: r.uint(), Payload: r.buffer()}
	default:
		return nil, ErrInvalid
	}
	if err := r.finish(); err != nil {
		return nil, err
	}
	return m, nil
}

// EncodeHandshake returns the frame of h. The lan fields are written only when FlagLAN is set; a nil
// LAN then writes an empty one.
func EncodeHandshake(h Handshake) []byte {
	var e compact.Encoder
	e.Uint(h.Version)
	e.Uint(h.Flags)
	encodeServices(&e, h.Services)
	if h.Flags&FlagLAN != 0 {
		var lan LAN
		if h.LAN != nil {
			lan = *h.LAN
		}
		encodeStrings(&e, lan.Addresses)
		e.Uint(lan.Port)
	}
	return e.Bytes()
}

// DecodeHandshake decodes a handshake frame from b. The lan fields are read only when FlagLAN is set.
func DecodeHandshake(b []byte) (Handshake, error) {
	r := reader{d: compact.NewDecoder(b)}
	h := Handshake{Version: r.uint(), Flags: r.uint(), Services: r.services()}
	if h.Flags&FlagLAN != 0 {
		h.LAN = &LAN{Addresses: r.strings(), Port: r.uint()}
	}
	if err := r.finish(); err != nil {
		return Handshake{}, err
	}
	return h, nil
}

// EncodeUnordered returns the frame of d, an unordered datagram.
func EncodeUnordered(d Datagram) []byte {
	var e compact.Encoder
	e.Uint(d.Flow)
	e.Buffer(d.Payload)
	return e.Bytes()
}

// DecodeUnordered decodes an unordered datagram frame from b.
func DecodeUnordered(b []byte) (Datagram, error) {
	r := reader{d: compact.NewDecoder(b)}
	d := Datagram{Flow: r.uint(), Payload: r.buffer()}
	if err := r.finish(); err != nil {
		return Datagram{}, err
	}
	return d, nil
}

// encodeServices writes a services list: its length, then each service's name, kind, port and origins.
func encodeServices(e *compact.Encoder, list []Service) {
	e.Len(len(list))
	for _, s := range list {
		e.String(s.Name)
		e.Uint(uint64(s.Kind))
		e.Uint(s.Port)
		encodeStrings(e, s.Origins)
	}
}

// encodeStrings writes a string list: its length, then each string.
func encodeStrings(e *compact.Encoder, list []string) {
	e.Len(len(list))
	for _, s := range list {
		e.String(s)
	}
}

// reader decodes one frame. The first error sticks: later reads return zero values, so a decode
// reads its fields in order and checks the error once, in finish.
type reader struct {
	d   *compact.Decoder
	err error
}

// fail records err unless an earlier error is recorded. The compact-encoding errors become the
// protocol's: a short frame is ErrTruncated, a length over its limit is ErrInvalid.
func (r *reader) fail(err error) {
	switch {
	case err == nil || r.err != nil:
	case errors.Is(err, compact.ErrOutOfBounds):
		r.err = ErrTruncated
	case errors.Is(err, compact.ErrTooLong):
		r.err = ErrInvalid
	case errors.Is(err, compact.ErrTrailing):
		r.err = ErrTrailing
	default:
		r.err = err
	}
}

// finish reports the first error, or ErrTrailing when bytes remain after the last field.
func (r *reader) finish() error {
	r.fail(r.d.Done())
	return r.err
}

func (r *reader) uint() uint64 {
	v, err := r.d.Uint()
	r.fail(err)
	return v
}

func (r *reader) str() string {
	s, err := r.d.String()
	r.fail(err)
	return s
}

// buffer reads a length-prefixed buffer. The result is a copy, so it does not alias b.
func (r *reader) buffer() []byte {
	b, err := r.d.Buffer()
	r.fail(err)
	return append([]byte(nil), b...)
}

// data reads a data payload of 1 to 65536 bytes. The length is checked before the bytes are read.
func (r *reader) data() []byte {
	n, err := r.d.Len(maxPayload)
	r.fail(err)
	if n == 0 {
		r.fail(ErrInvalid)
	}
	b, err := r.d.Fixed(n)
	r.fail(err)
	return append([]byte(nil), b...)
}

func (r *reader) token() [16]byte {
	var t [16]byte
	b, err := r.d.Fixed(len(t))
	r.fail(err)
	copy(t[:], b)
	return t
}

// kind reads a service kind, which is 0 to 4. It is checked before narrowing to Kind.
func (r *reader) kind() Kind {
	k := r.uint()
	if k > uint64(KindUDP) {
		r.fail(ErrInvalid)
	}
	return Kind(k)
}

// strings reads a string list. Each element takes at least one byte, so a bad length ends the loop
// at the end of the frame.
func (r *reader) strings() []string {
	var list []string
	for n := r.uint(); n > 0 && r.err == nil; n-- {
		list = append(list, r.str())
	}
	return list
}

// services reads a services list. Each entry takes at least four bytes, so a bad length ends the loop
// at the end of the frame.
func (r *reader) services() []Service {
	var list []Service
	for n := r.uint(); n > 0 && r.err == nil; n-- {
		list = append(list, Service{Name: r.str(), Kind: r.kind(), Port: r.uint(), Origins: r.strings()})
	}
	return list
}
