package secretstream

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/andrewloable/HoleBridge/pears/noise"
)

// newKeyPair returns a fresh Ed25519 key pair in the layout the noise package uses.
func newKeyPair(t *testing.T) noise.KeyPair {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var kp noise.KeyPair
	copy(kp.Public[:], pub)
	copy(kp.Secret[:], priv)
	return kp
}

// pipe returns the two ends of an in-memory connection, closed when the test ends. The deadline
// turns a stalled stream into an error rather than a test that never ends.
func pipe(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	c1, c2 := net.Pipe()
	t.Cleanup(func() {
		c1.Close()
		c2.Close()
	})
	for _, c := range []net.Conn{c1, c2} {
		if err := c.SetDeadline(time.Now().Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	return c1, c2
}

// newStream calls New, and fails the test rather than the whole binary while New is a stub.
func newStream(t *testing.T, conn io.ReadWriteCloser, isInitiator bool, opts Options) *Stream {
	t.Helper()
	var s *Stream
	mustNotPanic(t, func() { s = New(conn, isInitiator, opts) })
	return s
}

// handshake runs both handshakes at once, because the in-memory pipe moves data only while both
// sides read and write. It fails the test if either side fails.
func handshake(t *testing.T, initiator, responder *Stream) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	errc := make(chan error, 1)
	go func() { errc <- responder.Handshake(ctx) }()
	if err := initiator.Handshake(ctx); err != nil {
		t.Fatalf("initiator handshake: %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("responder handshake: %v", err)
	}
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// transfer writes data on from and reads it back on to. It returns an error unless the bytes arrive
// intact.
func transfer(from, to *Stream, data []byte) error {
	werr := make(chan error, 1)
	go func() {
		_, err := from.Write(data)
		werr <- err
	}()
	got := make([]byte, len(data))
	if _, err := io.ReadFull(to, got); err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if err := <-werr; err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if !bytes.Equal(got, data) {
		return errors.New("received bytes differ from the bytes sent")
	}
	return nil
}

// countingConn counts the bytes written to the connection under it.
type countingConn struct {
	io.ReadWriteCloser
	written atomic.Int64
}

func (c *countingConn) Write(p []byte) (int, error) {
	n, err := c.ReadWriteCloser.Write(p)
	c.written.Add(int64(n))
	return n, err
}

// Two streams over an in-memory pipe complete the handshake and move 10 MB each way intact.
func TestHandshakeAndTenMegabytesBothWays(t *testing.T) {
	kpA, kpB := newKeyPair(t), newKeyPair(t)
	c1, c2 := pipe(t)
	a := newStream(t, c1, true, Options{KeyPair: kpA, RemotePublicKey: &kpB.Public})
	b := newStream(t, c2, false, Options{KeyPair: kpB})
	handshake(t, a, b)

	const size = 10 << 20
	ab, ba := randomBytes(t, size), randomBytes(t, size)
	done := make(chan error, 2)
	go func() { done <- transfer(a, b, ab) }()
	go func() { done <- transfer(b, a, ba) }()
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

// After the handshake, each side's RemotePublicKey is the other side's static public key.
func TestRemotePublicKeyIsThePeersPublicKey(t *testing.T) {
	kpA, kpB := newKeyPair(t), newKeyPair(t)
	c1, c2 := pipe(t)
	a := newStream(t, c1, true, Options{KeyPair: kpA, RemotePublicKey: &kpB.Public})
	b := newStream(t, c2, false, Options{KeyPair: kpB})
	handshake(t, a, b)

	var gotA, gotB [32]byte
	mustNotPanic(t, func() {
		gotA = a.RemotePublicKey()
		gotB = b.RemotePublicKey()
	})
	if gotA != kpB.Public {
		t.Errorf("initiator RemotePublicKey = %x, want the responder's public key %x", gotA, kpB.Public)
	}
	if gotB != kpA.Public {
		t.Errorf("responder RemotePublicKey = %x, want the initiator's public key %x", gotB, kpA.Public)
	}
}

// An initiator that expects a different remote public key fails the handshake. A control with the
// right key runs first, so the failure can only come from the wrong key.
func TestHandshakeFailsForWrongRemotePublicKey(t *testing.T) {
	kpA, kpB, kpC := newKeyPair(t), newKeyPair(t), newKeyPair(t)

	c1, c2 := pipe(t)
	a := newStream(t, c1, true, Options{KeyPair: kpA, RemotePublicKey: &kpB.Public})
	b := newStream(t, c2, false, Options{KeyPair: kpB})
	handshake(t, a, b)

	// The responder closes its end on a bad message 1, so the initiator must fail before the deadline.
	c1, c2 = pipe(t)
	a = newStream(t, c1, true, Options{KeyPair: kpA, RemotePublicKey: &kpC.Public})
	b = newStream(t, c2, false, Options{KeyPair: kpB})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = b.Handshake(ctx) }()
	if err := a.Handshake(ctx); err == nil {
		t.Fatal("initiator handshake with a wrong remote public key succeeded")
	} else if ctx.Err() != nil {
		t.Fatalf("initiator handshake only failed at the deadline: %v", err)
	}
}

// While idle, a stream with Keepalive set writes keepalive frames, and Read does not return for them.
func TestKeepaliveFramesAreSentWhenIdleAndIgnoredByReader(t *testing.T) {
	kpA, kpB := newKeyPair(t), newKeyPair(t)
	c1, c2 := pipe(t)
	sent := &countingConn{ReadWriteCloser: c1}
	const keepalive = 20 * time.Millisecond
	a := newStream(t, sent, true, Options{KeyPair: kpA, RemotePublicKey: &kpB.Public, Keepalive: keepalive})
	b := newStream(t, c2, false, Options{KeyPair: kpB, Keepalive: keepalive})
	handshake(t, a, b)

	before := sent.written.Load()
	type read struct {
		data string
		err  error
	}
	reads := make(chan read, 1)
	go func() {
		buf := make([]byte, 64)
		n, err := b.Read(buf)
		reads <- read{string(buf[:n]), err}
	}()

	time.Sleep(200 * time.Millisecond)
	if sent.written.Load() == before {
		t.Fatal("no keepalive frame was written while the stream was idle")
	}
	select {
	case r := <-reads:
		t.Fatalf("Read returned %q, %v before any data was sent", r.data, r.err)
	default:
	}

	const msg = "after idle"
	if _, err := a.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-reads:
		if r.err != nil || r.data != msg {
			t.Fatalf("Read = %q, %v, want %q, nil", r.data, r.err, msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Read did not return the message sent after the idle period")
	}
}

// Close on one side gives io.EOF on the other, after the data written before Close.
func TestCloseGivesEOFAfterPendingData(t *testing.T) {
	kpA, kpB := newKeyPair(t), newKeyPair(t)
	c1, c2 := pipe(t)
	a := newStream(t, c1, true, Options{KeyPair: kpA, RemotePublicKey: &kpB.Public})
	b := newStream(t, c2, false, Options{KeyPair: kpB})
	handshake(t, a, b)

	const chunk = 1 << 10
	want := randomBytes(t, 3*chunk)
	closed := make(chan error, 1)
	go func() {
		for i := 0; i < len(want); i += chunk {
			if _, err := a.Write(want[i : i+chunk]); err != nil {
				closed <- err
				return
			}
		}
		closed <- a.Close()
	}()

	got, err := io.ReadAll(b)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("read %d bytes before EOF, want the %d bytes written before Close", len(got), len(want))
	}
	if err := <-closed; err != nil {
		t.Fatalf("writing or closing on the sending side: %v", err)
	}
	if n, err := b.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Errorf("Read after the data = %d, %v, want 0, io.EOF", n, err)
	}
}

// A write longer than one message is split into several messages, and the peer reads all of it.
func TestWriteLongerThanOneMessage(t *testing.T) {
	kpA, kpB := newKeyPair(t), newKeyPair(t)
	c1, c2 := pipe(t)
	a := newStream(t, c1, true, Options{KeyPair: kpA, RemotePublicKey: &kpB.Public})
	b := newStream(t, c2, false, Options{KeyPair: kpB})
	handshake(t, a, b)

	if err := transfer(a, b, randomBytes(t, maxPlain+1)); err != nil {
		t.Fatal(err)
	}
}

// A responder that expects a different initiator fails the handshake, and the initiator then fails too.
func TestResponderRejectsAnUnexpectedInitiator(t *testing.T) {
	kpA, kpB, kpC := newKeyPair(t), newKeyPair(t), newKeyPair(t)
	c1, c2 := pipe(t)
	a := newStream(t, c1, true, Options{KeyPair: kpA, RemotePublicKey: &kpB.Public})
	b := newStream(t, c2, false, Options{KeyPair: kpB, RemotePublicKey: &kpC.Public})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- b.Handshake(ctx) }()
	if err := a.Handshake(ctx); err == nil {
		t.Fatal("initiator handshake succeeded against a responder that rejected it")
	}
	if err := <-errc; !errors.Is(err, errWrongKey) {
		t.Fatalf("responder handshake error = %v, want errWrongKey", err)
	}
}

// Send needs a transport with unordered messages. A net.Pipe has none, so Send returns an error.
func TestSendNeedsAnUnorderedTransport(t *testing.T) {
	kpA, kpB := newKeyPair(t), newKeyPair(t)
	c1, c2 := pipe(t)
	a := newStream(t, c1, true, Options{KeyPair: kpA, RemotePublicKey: &kpB.Public})
	b := newStream(t, c2, false, Options{KeyPair: kpB})
	handshake(t, a, b)

	if err := a.Send([]byte("hello")); !errors.Is(err, errNoUnordered) {
		t.Fatalf("Send error = %v, want errNoUnordered", err)
	}
}

// The namespaces and unordered keys match upstream. The expected values are output of
// @hyperswarm/secret-stream 6.9.2's namespace function and sodium-universal's keyed BLAKE2b, run under
// node for a hash of bytes 0 to 63. The initiator's send key is the responder's receive key.
func TestUnorderedKeysMatchUpstream(t *testing.T) {
	var hash [64]byte
	for i := range hash {
		hash[i] = byte(i)
	}
	want := func(name string, got [32]byte, wantHex string) {
		t.Helper()
		if h := hex.EncodeToString(got[:]); h != wantHex {
			t.Errorf("%s = %s, want %s", name, h, wantHex)
		}
	}
	want("NS_INITIATOR", nsInitiator, "a931a0155b5c09e6d28628236af83c4b8a6af9af60986edeede9dc5d63192bf7")
	want("NS_RESPONDER", nsResponder, "742c9d833d430af4c48a8705e91631eecf295442bbca18996e597097723b1061")
	want("NS_SEND", nsSend, "aab97d097471f56651b0460ffc8aaa7d3342f7377597d60b925a1dac689adcbf")
	want("initiator stream id", keyedHash(hash, nsInitiator), "6a6344079ec552b1b651071f41f6731d07896fe7381cf27ed929c44d972bfcf9")
	want("responder stream id", keyedHash(hash, nsResponder), "4858b029dfec176e417def1e8004aab76bceffa36b0d3ceacd656175608e2372")
	iSend, iRecv := unorderedKeys(hash, true)
	want("initiator send key", iSend, "a59fcf110bb53be32ddaf5fe0662ea66f9189774f3690daf9875914c42711328")
	want("initiator receive key", iRecv, "a2defe021bbc3fc5b75c0e4caa45a6261b5a17c25afb03df2ec8d245d43b2a4b")
	if rSend, rRecv := unorderedKeys(hash, false); rSend != iRecv || rRecv != iSend {
		t.Error("the responder's keys are not the initiator's keys swapped")
	}
}

// Resume starts the streams from a Noise handshake that ran before them, as HyperDHT does over the DHT. The
// keys of each side are what the handshake gave it, the peers see each other's public keys, data moves both
// ways, and the connection carries no Noise message: the first bytes written are the stream header frame.
func TestResumeAfterANoiseHandshake(t *testing.T) {
	kpA, kpB := newKeyPair(t), newKeyPair(t)
	ini := noise.NewInitiator(kpA, kpB.Public, nil)
	res := noise.NewResponder(kpB, nil)
	msg1, err := ini.Send(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := res.Recv(msg1); err != nil {
		t.Fatal(err)
	}
	msg2, err := res.Send(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ini.Recv(msg2); err != nil {
		t.Fatal(err)
	}
	keysOf := func(h *noise.Handshake) Keys {
		tx, rx, hash, peer := h.Result()
		return Keys{Tx: tx, Rx: rx, Hash: hash, Peer: peer}
	}

	c1, c2 := pipe(t)
	cw := &countingConn{ReadWriteCloser: c1}
	a := Resume(cw, true, Options{RemotePublicKey: &kpB.Public}, keysOf(ini))
	b := Resume(c2, false, Options{}, keysOf(res))
	handshake(t, a, b)

	if a.RemotePublicKey() != kpB.Public {
		t.Error("initiator's RemotePublicKey is not the responder's public key")
	}
	if b.RemotePublicKey() != kpA.Public {
		t.Error("responder's RemotePublicKey is not the initiator's public key")
	}
	if got := cw.written.Load(); got != 3+32+HeaderSize {
		t.Errorf("initiator wrote %d bytes before data, want one header frame of %d", got, 3+32+HeaderSize)
	}
	data := randomBytes(t, 1<<16)
	done := make(chan error, 2)
	go func() { done <- transfer(a, b, data) }()
	go func() { done <- transfer(b, a, data) }()
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

// Resume refuses a handshake whose peer is not the RemotePublicKey the stream was given.
func TestResumeRefusesWrongRemotePublicKey(t *testing.T) {
	kpA, kpB, other := newKeyPair(t), newKeyPair(t), newKeyPair(t)
	ini := noise.NewInitiator(kpA, kpB.Public, nil)
	res := noise.NewResponder(kpB, nil)
	msg1, err := ini.Send(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := res.Recv(msg1); err != nil {
		t.Fatal(err)
	}
	msg2, err := res.Send(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ini.Recv(msg2); err != nil {
		t.Fatal(err)
	}
	tx, rx, hash, peer := ini.Result()
	c1, _ := pipe(t)
	a := Resume(c1, true, Options{RemotePublicKey: &other.Public}, Keys{Tx: tx, Rx: rx, Hash: hash, Peer: peer})
	if err := a.Handshake(context.Background()); !errors.Is(err, errWrongKey) {
		t.Fatalf("Handshake error = %v, want errWrongKey", err)
	}
}

// allocSlack is how much a test may allocate and still show that nothing reserved the frame a length prefix
// named. A 16 MiB frame costs 16 MiB, so the slack sits far below that and far above the few KiB a test allocates.
const allocSlack = 1 << 20

// totalAlloc is the number of bytes allocated since the program started. The difference across a call is what
// the call allocated, plus whatever the test's other goroutines allocated in the meantime, which is small here.
func totalAlloc() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.TotalAlloc
}

// noiseKeys runs a Noise IK handshake between kpA as initiator and kpB as responder, and returns each side's keys.
func noiseKeys(t *testing.T, kpA, kpB noise.KeyPair) (Keys, Keys) {
	t.Helper()
	ini := noise.NewInitiator(kpA, kpB.Public, nil)
	res := noise.NewResponder(kpB, nil)
	msg1, err := ini.Send(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := res.Recv(msg1); err != nil {
		t.Fatal(err)
	}
	msg2, err := res.Send(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ini.Recv(msg2); err != nil {
		t.Fatal(err)
	}
	keysOf := func(h *noise.Handshake) Keys {
		tx, rx, hash, peer := h.Result()
		return Keys{Tx: tx, Rx: rx, Hash: hash, Peer: peer}
	}
	return keysOf(ini), keysOf(res)
}

// expectRefusedAtOnce runs a handshake that a peer's huge length prefix must fail. The handshake must fail with
// want well before its 2 second deadline, and must allocate less than allocSlack while it fails.
func expectRefusedAtOnce(t *testing.T, handshake func(context.Context) error, want error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	before := totalAlloc()
	err := handshake(ctx)
	grew := totalAlloc() - before
	if !errors.Is(err, want) {
		t.Errorf("handshake error = %v, want %v", err, want)
	}
	if grew > allocSlack {
		t.Errorf("handshake allocated %d bytes after a huge length prefix, want under %d", grew, allocSlack)
	}
}

// A peer that sends only a length prefix naming 16 MiB in place of a pre-authentication frame fails the handshake
// at once, and the stream does not reserve the 16 MiB. The three frames are the responder's message 1, the
// initiator's message 2 and the stream header.
func TestPreAuthHugePrefixIsRefusedAtOnce(t *testing.T) {
	huge := []byte{0xff, 0xff, 0xff}

	t.Run("responder reads message 1", func(t *testing.T) {
		kpB := newKeyPair(t)
		c1, c2 := pipe(t)
		b := newStream(t, c2, false, Options{KeyPair: kpB})
		go func() { _, _ = c1.Write(huge) }()
		expectRefusedAtOnce(t, b.Handshake, errFrameTooLarge)
	})

	t.Run("initiator reads message 2", func(t *testing.T) {
		kpA, kpB := newKeyPair(t), newKeyPair(t)
		c1, c2 := pipe(t)
		a := newStream(t, c1, true, Options{KeyPair: kpA, RemotePublicKey: &kpB.Public})
		go func() {
			// Read message 1 whole, then answer with the huge prefix in place of message 2.
			var hdr [3]byte
			if _, err := io.ReadFull(c2, hdr[:]); err != nil {
				return
			}
			if _, err := io.CopyN(io.Discard, c2, int64(int(hdr[0])|int(hdr[1])<<8|int(hdr[2])<<16)); err != nil {
				return
			}
			_, _ = c2.Write(huge)
		}()
		expectRefusedAtOnce(t, a.Handshake, errFrameTooLarge)
	})

	t.Run("stream header", func(t *testing.T) {
		kpA, kpB := newKeyPair(t), newKeyPair(t)
		_, kb := noiseKeys(t, kpA, kpB)
		c1, c2 := pipe(t)
		b := Resume(c2, false, Options{}, kb)
		go func() { _, _ = c1.Write(huge) }()
		expectRefusedAtOnce(t, b.Handshake, errFrameTooLarge)
	})
}

// After the handshake, a peer that declares a 16 MiB data frame and sends only 1 KiB of it costs the reader about
// 1 KiB. The stream reads a data frame as its bytes arrive, so it does not reserve the length the frame declares.
func TestDataFrameDeclaringAHugeLengthDoesNotAllocateIt(t *testing.T) {
	kpA, kpB := newKeyPair(t), newKeyPair(t)
	c1, c2 := pipe(t)
	a := newStream(t, c1, true, Options{KeyPair: kpA, RemotePublicKey: &kpB.Public})
	b := newStream(t, c2, false, Options{KeyPair: kpB})
	handshake(t, a, b)

	// a sends nothing more, so the bytes written to its connection below are the only frame b reads.
	reader := make(chan error, 1)
	go func() {
		_, err := b.Read(make([]byte, 1))
		reader <- err
	}()
	before := totalAlloc()
	frame := append([]byte{0xff, 0xff, 0xff}, randomBytes(t, 1<<10)...)
	if _, err := c1.Write(frame); err != nil {
		t.Fatal(err)
	}
	grew := totalAlloc() - before
	if grew > allocSlack {
		t.Errorf("reading a frame that declared 16 MiB allocated %d bytes after 1 KiB, want under %d", grew, allocSlack)
	}
	c1.Close() // the frame never completes, so the read ends with an error
	if err := <-reader; err == nil {
		t.Error("Read returned no error for a frame that never completed")
	}
}

// readFrame accepts a payload of exactly its limit, and refuses one byte more on the prefix alone: the reader
// holds only the prefix, so a read that went past it would fail with io.ErrUnexpectedEOF instead.
func TestReadFrameLimitIsInclusive(t *testing.T) {
	payload := randomBytes(t, 56)
	got, err := readFrame(bytes.NewReader(append([]byte{56, 0, 0}, payload...)), 56)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("readFrame at its limit = %d bytes, %v, want the 56 bytes sent", len(got), err)
	}
	if _, err := readFrame(bytes.NewReader([]byte{57, 0, 0}), 56); !errors.Is(err, errFrameTooLarge) {
		t.Fatalf("readFrame one byte over its limit: error = %v, want errFrameTooLarge", err)
	}
}

// A frame longer than the first allocation arrives in pieces, and readFrame returns all of it, here one byte
// at a time so that each read is short.
func TestReadFrameAssemblesAFrameLongerThanItsFirstChunk(t *testing.T) {
	want := randomBytes(t, 3*frameStart+17)
	n := len(want)
	frame := append([]byte{byte(n), byte(n >> 8), byte(n >> 16)}, want...)
	got, err := readFrame(iotest.OneByteReader(bytes.NewReader(frame)), maxFrame)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("readFrame returned %d bytes that differ from the %d sent", len(got), n)
	}
}
