package secretstream

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/pears/udx"
)

// wait bounds how long a test waits for a message or for a channel to close.
const wait = 10 * time.Second

// Expected values come from @hyperswarm/secret-stream 6.9.2, run under node (spec/gen has the package), as
// TestUnorderedKeysMatchUpstream's keys do. The handshake hash is upstreamHash, the initiator sends both
// messages, and node sets its counter to zero before the first send, so the first envelope carries counter 1.
// Upstream draws the counter at random, but the envelope layout does not depend on where it starts. An
// envelope is the 8-byte counter, the 16-byte MAC, then the ciphertext.
const (
	upstreamHello         = "hello from upstream"
	upstreamHelloEnvelope = "010000000000000061a8828680f87c13100bf81ae7c7838c947270f441f15b0ec32c1ad29a453ccbd2605d"
	upstreamEmptyEnvelope = "020000000000000054a3ccafe8941d1703e685227c720dd3"
)

// upstreamHash is the handshake hash of the upstream vectors: bytes 0 to 63.
func upstreamHash() [64]byte {
	var h [64]byte
	for i := range h {
		h[i] = byte(i)
	}
	return h
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad vector: %v", err)
	}
	return b
}

// msgConn is an in-memory connection with unordered messages, as a UDX stream has. SendMessage records a
// copy on sent. A test puts a message the peer sent on msgs, which is what Messages returns.
type msgConn struct {
	io.ReadWriteCloser
	msgs chan []byte
	sent chan []byte
}

func (c *msgConn) SendMessage(b []byte) error {
	c.sent <- append([]byte(nil), b...)
	return nil
}

func (c *msgConn) Messages() <-chan []byte {
	return c.msgs
}

// resumedPair returns an initiator and a responder over msgConns. Both run Resume with upstreamHash and zero
// stream keys, so no Noise handshake runs and the unordered keys are the upstream ones.
func resumedPair(t *testing.T) (a, b *Stream, ca, cb *msgConn) {
	t.Helper()
	c1, c2 := pipe(t)
	ca = &msgConn{ReadWriteCloser: c1, msgs: make(chan []byte, 16), sent: make(chan []byte, 16)}
	cb = &msgConn{ReadWriteCloser: c2, msgs: make(chan []byte, 16), sent: make(chan []byte, 16)}
	keys := Keys{Hash: upstreamHash()}
	mustNotPanic(t, func() {
		a = Resume(ca, true, Options{}, keys)
		b = Resume(cb, false, Options{}, keys)
	})
	handshake(t, a, b)
	return a, b, ca, cb
}

// udxPair returns two UDX streams that reach each other over loopback, each on its own socket.
func udxPair(t *testing.T) (a, b *udx.Stream) {
	t.Helper()
	cA, cB := loopbackUDP(t), loopbackUDP(t)
	sockA, err := udx.NewSocket(cA)
	if err != nil {
		t.Fatalf("NewSocket: %v", err)
	}
	t.Cleanup(func() { sockA.Close() })
	sockB, err := udx.NewSocket(cB)
	if err != nil {
		t.Fatalf("NewSocket: %v", err)
	}
	t.Cleanup(func() { sockB.Close() })
	a, b = sockA.NewStream(1001), sockB.NewStream(2002)
	t.Cleanup(func() { a.Close(); b.Close() })
	if err := a.Connect(2002, cB.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := b.Connect(1001, cA.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	return a, b
}

// loopbackUDP opens a UDP socket on 127.0.0.1 with a free port.
func loopbackUDP(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("ListenUDP: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// streamsOver returns an initiator and a responder over the two connections, handshaken.
func streamsOver(t *testing.T, ca, cb io.ReadWriteCloser) (a, b *Stream) {
	t.Helper()
	kpA, kpB := newKeyPair(t), newKeyPair(t)
	a = newStream(t, ca, true, Options{KeyPair: kpA, RemotePublicKey: &kpB.Public})
	b = newStream(t, cb, false, Options{KeyPair: kpB})
	handshake(t, a, b)
	return a, b
}

// messagesOf returns s.Messages, failing the test while the method is a stub.
func messagesOf(t *testing.T, s *Stream) <-chan []byte {
	t.Helper()
	var ch <-chan []byte
	mustNotPanic(t, func() { ch = s.Messages() })
	return ch
}

// next returns the next message on ch, failing the test if none arrives within wait.
func next(t *testing.T, ch <-chan []byte) []byte {
	t.Helper()
	select {
	case m, ok := <-ch:
		if !ok {
			t.Fatal("the channel closed before a message arrived")
		}
		return m
	case <-time.After(wait):
		t.Fatal("no message arrived")
	}
	return nil
}

// expectMessage fails the test unless the next message on ch is want.
func expectMessage(t *testing.T, ch <-chan []byte, want []byte) {
	t.Helper()
	if got := next(t, ch); !bytes.Equal(got, want) {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

// waitClosed fails the test unless ch closes within wait. Messages still queued in ch are read first.
func waitClosed(t *testing.T, ch <-chan []byte) {
	t.Helper()
	timeout := time.After(wait)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-timeout:
			t.Fatal("Messages did not close")
		}
	}
}

// A message that upstream 6.9.2 sends opens on the other side to its plaintext, and an empty message opens
// to an empty one.
func TestMessagesFromUpstreamOpen(t *testing.T) {
	_, b, _, cb := resumedPair(t)
	msgs := messagesOf(t, b)
	cb.msgs <- mustHex(t, upstreamHelloEnvelope)
	cb.msgs <- mustHex(t, upstreamEmptyEnvelope)
	expectMessage(t, msgs, []byte(upstreamHello))
	expectMessage(t, msgs, []byte{})
}

// Send writes the envelopes upstream writes for the same key and counter, byte for byte.
func TestSendMatchesUpstreamEnvelopes(t *testing.T) {
	a, _, ca, _ := resumedPair(t)
	if err := a.Send([]byte(upstreamHello)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := a.Send([]byte{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := next(t, ca.sent); !bytes.Equal(got, mustHex(t, upstreamHelloEnvelope)) {
		t.Errorf("first envelope = %x, want the upstream envelope %s", got, upstreamHelloEnvelope)
	}
	if got := next(t, ca.sent); !bytes.Equal(got, mustHex(t, upstreamEmptyEnvelope)) {
		t.Errorf("second envelope = %x, want the upstream envelope %s", got, upstreamEmptyEnvelope)
	}
}

// Messages that do not open are dropped, and the valid message after them still arrives. The dropped ones are
// a message shorter than an envelope, one sealed with this side's own send key, and one with a flipped bit.
func TestMessagesThatDoNotOpenAreDropped(t *testing.T) {
	_, b, _, cb := resumedPair(t)
	msgs := messagesOf(t, b)
	if err := b.Send([]byte("own")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	own := next(t, cb.sent)
	tampered := mustHex(t, upstreamHelloEnvelope)
	tampered[len(tampered)-1] ^= 1
	cb.msgs <- make([]byte, 23)
	cb.msgs <- own
	cb.msgs <- tampered
	cb.msgs <- mustHex(t, upstreamHelloEnvelope)
	expectMessage(t, msgs, []byte(upstreamHello))
}

// Unordered messages go both ways over UDX streams and are delivered on the peer's Messages channel.
func TestUnorderedMessagesRoundTripOverUDX(t *testing.T) {
	ua, ub := udxPair(t)
	a, b := streamsOver(t, ua, ub)
	msgsA, msgsB := messagesOf(t, a), messagesOf(t, b)
	if err := a.Send([]byte("from a")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	expectMessage(t, msgsB, []byte("from a"))
	if err := b.Send([]byte("from b")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	expectMessage(t, msgsA, []byte("from b"))
}

// The largest unordered payload is the UDX MaxMessage less the 24-byte envelope, which is 1156 bytes as
// docs/spike-m1.md measured. A payload of that size arrives, and one byte more is refused by Send.
func TestLargestUnorderedPayloadOverUDX(t *testing.T) {
	ua, ub := udxPair(t)
	a, b := streamsOver(t, ua, ub)
	msgs := messagesOf(t, b)
	const envelope = 24
	limit := ua.MaxMessage() - envelope
	if limit != 1156 {
		t.Fatalf("largest payload = %d, want 1156 (MaxMessage %d less the %d-byte envelope)", limit, ua.MaxMessage(), envelope)
	}
	big := randomBytes(t, limit)
	if err := a.Send(big); err != nil {
		t.Fatalf("Send of %d bytes: %v", limit, err)
	}
	expectMessage(t, msgs, big)
	if err := a.Send(make([]byte, limit+1)); err == nil {
		t.Fatalf("Send of %d bytes = nil, want an error", limit+1)
	}
}

// Unordered messages sent while the stream carries bulk data all arrive, and the bulk data arrives intact.
func TestMessagesDuringBulkTransferOverUDX(t *testing.T) {
	ua, ub := udxPair(t)
	a, b := streamsOver(t, ua, ub)
	msgs := messagesOf(t, b)
	data := randomBytes(t, 256<<10)
	errc := make(chan error, 1)
	go func() { errc <- transfer(a, b, data) }()
	const count = 10
	for i := range count {
		if err := a.Send(fmt.Appendf(nil, "message %d", i)); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}
	seen := map[string]bool{}
	for range count {
		seen[string(next(t, msgs))] = true
	}
	for i := range count {
		if want := fmt.Sprintf("message %d", i); !seen[want] {
			t.Errorf("%q did not arrive", want)
		}
	}
	if err := <-errc; err != nil {
		t.Fatalf("bulk transfer: %v", err)
	}
}

// Close ends only the local write side over UDX, as upstream's end does, so the peer's unordered messages
// still arrive afterwards.
func TestMessagesKeepArrivingAfterClose(t *testing.T) {
	ua, ub := udxPair(t)
	a, b := streamsOver(t, ua, ub)
	msgs := messagesOf(t, a)
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := b.Send([]byte("after close")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	expectMessage(t, msgs, []byte("after close"))
}

// The Messages channel closes when the UDX stream is torn down.
func TestMessagesCloseWhenUDXStreamIsDestroyed(t *testing.T) {
	ua, ub := udxPair(t)
	_, b := streamsOver(t, ua, ub)
	msgs := messagesOf(t, b)
	if err := ub.Destroy(); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	waitClosed(t, msgs)
}

// A connection without unordered messages has none to deliver, so Messages closes at Close.
func TestMessagesEndAtCloseWithoutUnorderedTransport(t *testing.T) {
	kpA, kpB := newKeyPair(t), newKeyPair(t)
	c1, c2 := pipe(t)
	a := newStream(t, c1, true, Options{KeyPair: kpA, RemotePublicKey: &kpB.Public})
	b := newStream(t, c2, false, Options{KeyPair: kpB})
	handshake(t, a, b)
	msgs := messagesOf(t, a)
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitClosed(t, msgs)
}
