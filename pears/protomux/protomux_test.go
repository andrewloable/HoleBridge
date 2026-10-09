package protomux

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/internal/testvec"
)

// The expected frames and events come from spec/vectors/protomux.json, written by
// spec/gen/protomux.js from protomux 3.12.1. The Mux runs on a stream whose Write is one frame and
// whose Read returns one whole frame. memStream is that stream in memory. framedConn gives a
// net.Pipe the same contract with a length prefix.

var echoID = []byte("b1")

// vectorFile mirrors spec/vectors/protomux.json.
type vectorFile struct {
	Frames       []vectorFrame `json:"frames"`
	Events       []vectorEvent `json:"events"`
	UnknownIndex struct {
		Frames []vectorFrame `json:"frames"`
		Events []vectorEvent `json:"events"`
	} `json:"unknown_index"`
}

// vectorFrame is one frame upstream wrote, named by the call that wrote it.
type vectorFrame struct {
	Name string `json:"name"`
	Hex  string `json:"hex"`
}

// vectorEvent is one event a receiving Mux reports: an open with its handshake, a message on an
// index, or a close. ID is hex, empty when the channel has none.
type vectorEvent struct {
	Event     string `json:"event"`
	Protocol  string `json:"protocol"`
	ID        string `json:"id"`
	Handshake string `json:"handshake"`
	Index     int    `json:"index"`
	Payload   string `json:"payload"`
	IsRemote  bool   `json:"is_remote"`
}

// loadVectors reads spec/vectors/protomux.json.
func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	var v vectorFile
	testvec.Load(t, "protomux.json", &v)
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

// mustNoError fails the test on any error. A stub's errors.ErrUnsupported is reported as not
// implemented.
func mustNoError(t *testing.T, err error) {
	t.Helper()
	if errors.Is(err, errors.ErrUnsupported) {
		t.Fatal("not implemented")
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// recvWithin returns the next value on ch, failing the test if none arrives within 5 seconds.
func recvWithin[T any](t *testing.T, ch chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the Mux")
	}
	var zero T
	return zero
}

// ignore is a message handler that drops its payload.
func ignore([]byte) {}

// memStream is the stream under a Mux in these tests. Each Write is kept as one frame. Each Read
// returns one queued frame, and blocks while none is queued, until Close.
type memStream struct {
	in     chan []byte
	done   chan struct{}
	once   sync.Once
	mu     sync.Mutex
	writes [][]byte
}

func newMemStream() *memStream {
	// Room for any frame list these tests feed at once.
	return &memStream{in: make(chan []byte, 64), done: make(chan struct{})}
}

// feed queues frames for Read, in order.
func (s *memStream) feed(t *testing.T, frames []vectorFrame) {
	t.Helper()
	for _, f := range frames {
		s.in <- testvec.Hex(t, f.Hex)
	}
}

func (s *memStream) Read(p []byte) (int, error) {
	select {
	case f := <-s.in:
		if len(f) > len(p) {
			return 0, io.ErrShortBuffer
		}
		return copy(p, f), nil
	case <-s.done:
		return 0, io.EOF
	}
}

func (s *memStream) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes = append(s.writes, append([]byte(nil), p...))
	return len(p), nil
}

func (s *memStream) Close() error {
	s.once.Do(func() { close(s.done) })
	return nil
}

// sent returns the frames written so far.
func (s *memStream) sent() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.writes...)
}

// framedConn gives a net.Conn the framing the Mux expects: each Write goes out as one frame, a
// 4-byte big-endian length then the bytes, and each Read returns one whole frame.
type framedConn struct{ conn net.Conn }

func (f framedConn) Write(p []byte) (int, error) {
	buf := make([]byte, 4+len(p))
	binary.BigEndian.PutUint32(buf, uint32(len(p)))
	copy(buf[4:], p)
	if _, err := f.conn.Write(buf); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (f framedConn) Read(p []byte) (int, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(f.conn, hdr[:]); err != nil {
		return 0, err
	}
	n := int(binary.BigEndian.Uint32(hdr[:]))
	if n > len(p) {
		return 0, io.ErrShortBuffer
	}
	return io.ReadFull(f.conn, p[:n])
}

func (f framedConn) Close() error { return f.conn.Close() }

// eventLog collects the events a receiving Mux reports, in the order it reports them.
type eventLog struct {
	mu     sync.Mutex
	events []vectorEvent
}

func (l *eventLog) add(e vectorEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *eventLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.events)
}

// wait returns the events once n have arrived, or after 5 seconds. It then waits a moment more so
// that an extra event would show up too.
func (l *eventLog) wait(n int) []vectorEvent {
	deadline := time.Now().Add(5 * time.Second)
	for l.count() < n && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]vectorEvent(nil), l.events...)
}

// channelLog records one channel's events under its protocol and ID.
type channelLog struct {
	log      *eventLog
	protocol string
	id       []byte
}

func (c channelLog) event(e vectorEvent) {
	e.Protocol = c.protocol
	e.ID = hex.EncodeToString(c.id)
	c.log.add(e)
}

func (c channelLog) opened(handshake []byte) {
	c.event(vectorEvent{Event: "open", Handshake: hex.EncodeToString(handshake)})
}

func (c channelLog) closed(isRemote bool) {
	c.event(vectorEvent{Event: "close", IsRemote: isRemote})
}

func (c channelLog) message(index int) func([]byte) {
	return func(payload []byte) {
		c.event(vectorEvent{Event: "message", Index: index, Payload: hex.EncodeToString(payload)})
	}
}

// receiveSide sets up the receiving Mux of the vector session: "holebridge" with messages 0 to 3,
// and "echo" with ID "b1" and message 0. Its events go to log.
func receiveSide(m *Mux, log *eventLog) {
	hb := channelLog{log: log, protocol: "holebridge"}
	m.Pair("holebridge", nil, func() {
		ch := m.CreateChannel(ChannelOptions{Protocol: "holebridge", OnOpen: hb.opened, OnClose: hb.closed})
		for i := 0; i < 4; i++ {
			ch.AddMessage(hb.message(i))
		}
	})
	echo := channelLog{log: log, protocol: "echo", id: echoID}
	m.Pair("echo", echoID, func() {
		ch := m.CreateChannel(ChannelOptions{Protocol: "echo", ID: echoID, OnOpen: echo.opened, OnClose: echo.closed})
		ch.AddMessage(echo.message(0))
	})
}

// receive runs a receiving Mux on frames and returns the first want events it reports.
func receive(t *testing.T, frames []vectorFrame, want int) []vectorEvent {
	t.Helper()
	s := newMemStream()
	defer s.Close()
	log := &eventLog{}
	receiveSide(New(s), log)
	s.feed(t, frames)
	return log.wait(want)
}

// checkEvents fails the test unless got matches want, event for event.
func checkEvents(t *testing.T, got, want []vectorEvent) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events differ\n got: %+v\nwant: %+v", got, want)
	}
}

// pipePair is two Muxes over a net.Pipe with one "holebridge" channel open on both sides. Side 1
// (local) opens with the handshake "hello-1". Side 2 (remote) accepts it in its Pair callback and
// opens with "hello-2". Each side registers messages 0 and 1, and side 2 reports what arrives.
type pipePair struct {
	local, remote *Channel
	msgs          [2]*Message    // local side's messages 0 and 1
	localOpen     chan []byte    // handshake side 1's OnOpen reports
	remoteOpen    chan []byte    // handshake side 2's OnOpen reports
	localClosed   chan bool      // isRemote from side 1's OnClose
	remoteClosed  chan bool      // isRemote from side 2's OnClose
	remoteMsgs    [2]chan []byte // payloads side 2 got on index 0 and 1
}

func joinPipe(t *testing.T) *pipePair {
	t.Helper()
	c1, c2 := net.Pipe()
	t.Cleanup(func() {
		c1.Close()
		c2.Close()
	})

	p := &pipePair{
		localOpen:    make(chan []byte, 1),
		remoteOpen:   make(chan []byte, 1),
		localClosed:  make(chan bool, 1),
		remoteClosed: make(chan bool, 1),
	}
	for i := range p.remoteMsgs {
		p.remoteMsgs[i] = make(chan []byte, 1)
	}

	m1 := New(framedConn{c1})
	m2 := New(framedConn{c2})

	// Side 2 must be paired before side 1 opens, or the open arrives with nobody to accept it.
	accepted := make(chan *Channel, 1)
	m2.Pair("holebridge", nil, func() {
		ch := m2.CreateChannel(ChannelOptions{
			Protocol: "holebridge",
			OnOpen:   func(h []byte) { p.remoteOpen <- h },
			OnClose:  func(isRemote bool) { p.remoteClosed <- isRemote },
		})
		for i := range p.remoteMsgs {
			ch.AddMessage(func(payload []byte) { p.remoteMsgs[i] <- append([]byte(nil), payload...) })
		}
		accepted <- ch
	})

	p.local = m1.CreateChannel(ChannelOptions{
		Protocol: "holebridge",
		OnOpen:   func(h []byte) { p.localOpen <- h },
		OnClose:  func(isRemote bool) { p.localClosed <- isRemote },
	})
	for i := range p.msgs {
		p.msgs[i] = p.local.AddMessage(ignore)
	}
	mustNoError(t, p.local.Open([]byte("hello-1")))

	p.remote = recvWithin(t, accepted)
	mustNoError(t, p.remote.Open([]byte("hello-2")))
	return p
}

// TestFramesMatchUpstream makes the calls spec/gen/protomux.js makes, and checks that each Write
// is byte for byte the frame upstream wrote.
func TestFramesMatchUpstream(t *testing.T) {
	defer failOnPanic(t)
	v := loadVectors(t)

	s := newMemStream()
	defer s.Close()
	m := New(s)
	a := m.CreateChannel(ChannelOptions{Protocol: "holebridge"})
	a0 := a.AddMessage(ignore)
	a.AddMessage(ignore)
	a.AddMessage(ignore)
	a3 := a.AddMessage(ignore)
	mustNoError(t, a.Open([]byte("hello")))
	mustNoError(t, a0.Send([]byte("zero")))
	mustNoError(t, a3.Send([]byte("three")))

	b := m.CreateChannel(ChannelOptions{Protocol: "echo", ID: echoID})
	b0 := b.AddMessage(ignore)
	mustNoError(t, b.Open(nil))

	m.Cork()
	mustNoError(t, a0.Send([]byte("batch-zero")))
	mustNoError(t, a3.Send([]byte("batch-three")))
	mustNoError(t, b0.Send([]byte("b-zero")))
	mustNoError(t, m.Uncork())

	mustNoError(t, a.Close())

	got := s.sent()
	if len(got) != len(v.Frames) {
		t.Fatalf("wrote %d frames, upstream wrote %d", len(got), len(v.Frames))
	}
	for i, f := range v.Frames {
		if hex.EncodeToString(got[i]) != f.Hex {
			t.Errorf("frame %d (%s): got %x, want %s", i, f.Name, got[i], f.Hex)
		}
	}
}

// TestDecodesUpstreamFrames feeds every frame upstream wrote into a receiving Mux, and checks that
// it reports the same channel and message events upstream's receiver reported.
func TestDecodesUpstreamFrames(t *testing.T) {
	defer failOnPanic(t)
	v := loadVectors(t)
	checkEvents(t, receive(t, v.Frames, len(v.Events)), v.Events)
}

// TestUnknownIndexIgnored feeds upstream's frames for a message on index 5, which the receiver
// does not have, followed by one on index 0. The index 5 message is ignored and the channel stays
// open, so no close is reported, as upstream does.
func TestUnknownIndexIgnored(t *testing.T) {
	defer failOnPanic(t)
	v := loadVectors(t)
	checkEvents(t, receive(t, v.UnknownIndex.Frames, len(v.UnknownIndex.Events)), v.UnknownIndex.Events)
}

// TestTwoMuxesOpenAndSend joins two Muxes over net.Pipe. Each side sees the other's handshake, so
// the channel opened on both sides, and each message arrives on the index it was sent on.
func TestTwoMuxesOpenAndSend(t *testing.T) {
	defer failOnPanic(t)
	p := joinPipe(t)

	if got := recvWithin(t, p.localOpen); string(got) != "hello-2" {
		t.Fatalf("side 1 OnOpen handshake %q, want %q", got, "hello-2")
	}
	if got := recvWithin(t, p.remoteOpen); string(got) != "hello-1" {
		t.Fatalf("side 2 OnOpen handshake %q, want %q", got, "hello-1")
	}

	mustNoError(t, p.msgs[1].Send([]byte("one")))
	mustNoError(t, p.msgs[0].Send([]byte("zero")))
	if got := recvWithin(t, p.remoteMsgs[0]); string(got) != "zero" {
		t.Errorf("index 0 got %q, want %q", got, "zero")
	}
	if got := recvWithin(t, p.remoteMsgs[1]); string(got) != "one" {
		t.Errorf("index 1 got %q, want %q", got, "one")
	}
}

// TestCloseFiresOnCloseBothSides closes the channel on side 1. OnClose fires on both sides: on
// side 1 with isRemote false, and on side 2 with isRemote true, as upstream.
func TestCloseFiresOnCloseBothSides(t *testing.T) {
	defer failOnPanic(t)
	p := joinPipe(t)
	recvWithin(t, p.localOpen)
	recvWithin(t, p.remoteOpen)

	mustNoError(t, p.local.Close())
	if got := recvWithin(t, p.localClosed); got {
		t.Errorf("side 1 OnClose isRemote = true, want false")
	}
	if got := recvWithin(t, p.remoteClosed); !got {
		t.Errorf("side 2 OnClose isRemote = false, want true")
	}
}
