package protomux

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/internal/testvec"
)

// batchEntries reports how many messages a batch frame carries, and false when frame is not a batch.
// A batch is the control session's batch type, then a local id, then length-prefixed bodies, where a zero
// length is followed by a new local id.
func batchEntries(t *testing.T, frame []byte) (int, bool) {
	t.Helper()
	remote, b, err := readUint(frame)
	if err != nil {
		t.Fatalf("frame: %v", err)
	}
	typ, b, err := readUint(b)
	if err != nil {
		t.Fatalf("frame: %v", err)
	}
	if remote != 0 || typ != ctlBatch {
		return 0, false
	}
	if _, b, err = readUint(b); err != nil {
		t.Fatalf("batch: %v", err)
	}
	n := 0
	for len(b) > 0 {
		var l uint64
		if l, b, err = readUint(b); err != nil {
			t.Fatalf("batch: %v", err)
		}
		if l == 0 {
			if _, b, err = readUint(b); err != nil {
				t.Fatalf("batch: %v", err)
			}
			continue
		}
		b = b[l:]
		n++
	}
	return n, true
}

// sentFrames returns the frames a memStream has written, and how many of them are batches with their
// entry counts.
func sentFrames(t *testing.T, s *memStream) (frames [][]byte, batches []int) {
	t.Helper()
	frames = s.sent()
	for _, f := range frames {
		if n, ok := batchEntries(t, f); ok {
			batches = append(batches, n)
		}
	}
	return frames, batches
}

// Test case 1: a corked batch whose messages total more than 8 MiB goes out as more than one batch frame,
// as upstream's _pushBatch splits it at MAX_BATCH. Each frame is at most MAX_BATCH plus one message, and the
// receiver gets every message in order.
func TestBatchSplitsAboveMaxBatch(t *testing.T) {
	const msgs, size = 9, 1 << 20
	sent := newMemStream()
	defer sent.Close()
	m := New(sent)
	ch := m.CreateChannel(ChannelOptions{Protocol: "bulk"})
	msg := ch.AddMessage(ignore)
	mustNoError(t, ch.Open(nil))
	m.Cork()
	for i := 0; i < msgs; i++ {
		mustNoError(t, msg.Send(bytes.Repeat([]byte{byte(i + 1)}, size)))
	}
	mustNoError(t, m.Uncork())
	frames, batches := sentFrames(t, sent)
	if len(batches) < 2 {
		t.Fatalf("a batch of 9 MiB went out as %d batch frames, want at least 2", len(batches))
	}
	total := 0
	for _, n := range batches {
		total += n
	}
	if total != msgs {
		t.Errorf("batch frames carry %d messages, want %d", total, msgs)
	}
	for i, f := range frames {
		if len(f) > maxBatch+size+64 {
			t.Errorf("frame %d is %d bytes, want at most MAX_BATCH plus one message", i, len(f))
		}
	}

	recv := newMemStream()
	defer recv.Close()
	got := make(chan []byte, msgs)
	r := New(recv)
	r.Pair("bulk", nil, func() {
		rc := r.CreateChannel(ChannelOptions{Protocol: "bulk"})
		rc.AddMessage(func(p []byte) { got <- bytes.Clone(p) })
	})
	for _, f := range frames {
		recv.in <- f
	}
	for i := 0; i < msgs; i++ {
		p := recvWithin(t, got)
		if !bytes.Equal(p, bytes.Repeat([]byte{byte(i + 1)}, size)) {
			t.Fatalf("message %d differs from the one sent", i)
		}
	}
}

// Test case 2: a received batch of several messages is answered as one batch. The handler replies to each
// message while the batch is corked, so the three replies go out in one batch frame, not three frames.
func TestReceivedBatchRepliesAreBatched(t *testing.T) {
	bs := newMemStream()
	defer bs.Close()
	b := New(bs)
	bch := b.CreateChannel(ChannelOptions{Protocol: "echo"})
	bmsg := bch.AddMessage(ignore)
	mustNoError(t, bch.Open(nil))
	b.Cork()
	for i := 0; i < 3; i++ {
		mustNoError(t, bmsg.Send([]byte{byte('a' + i)}))
	}
	mustNoError(t, b.Uncork())

	as := newMemStream()
	defer as.Close()
	a := New(as)
	handled := make(chan struct{}, 3)
	a.Pair("echo", nil, func() {
		ch := a.CreateChannel(ChannelOptions{Protocol: "echo"})
		var reply *Message
		ch.AddMessage(func(p []byte) {
			mustNoError(t, reply.Send(p))
			handled <- struct{}{}
		})
		reply = ch.AddMessage(ignore)
		mustNoError(t, ch.Open(nil))
	})
	for _, f := range bs.sent() {
		as.in <- f
	}
	for i := 0; i < 3; i++ {
		recvWithin(t, handled)
	}
	time.Sleep(50 * time.Millisecond) // the batch of replies is written once the batch has been read
	_, batches := sentFrames(t, as)
	if len(batches) != 1 || batches[0] != 3 {
		t.Errorf("replies went out as batch frames %v, want one batch of 3 messages", batches)
	}
}

// Test case 3: a frame of 1 MiB, over the 64 KiB read buffer the Mux had, is read whole, so its message
// arrives intact. Upstream frames up to MAX_ATOMIC_WRITE (maxFrame) are read whole.
func TestLargeFrameReadWhole(t *testing.T) {
	const size = 1 << 20
	bs := newMemStream()
	defer bs.Close()
	b := New(bs)
	bch := b.CreateChannel(ChannelOptions{Protocol: "bulk"})
	bmsg := bch.AddMessage(ignore)
	mustNoError(t, bch.Open(nil))
	payload := bytes.Repeat([]byte{0x5a}, size)
	mustNoError(t, bmsg.Send(payload))

	as := newMemStream()
	defer as.Close()
	a := New(as)
	got := make(chan []byte, 1)
	a.Pair("bulk", nil, func() {
		rc := a.CreateChannel(ChannelOptions{Protocol: "bulk"})
		rc.AddMessage(func(p []byte) { got <- bytes.Clone(p) })
	})
	for _, f := range bs.sent() {
		as.in <- f
	}
	if p := recvWithin(t, got); !bytes.Equal(p, payload) {
		t.Errorf("a 1 MiB message arrived as %d bytes that differ from the payload", len(p))
	}
}

// Test case 4: a message whose frame would be over maxFrame, the largest frame upstream's secret stream
// writes atomically, is refused by Send, and no frame is written for it.
func TestSendRefusesFrameOverLimit(t *testing.T) {
	s := newMemStream()
	defer s.Close()
	m := New(s)
	ch := m.CreateChannel(ChannelOptions{Protocol: "bulk"})
	msg := ch.AddMessage(ignore)
	mustNoError(t, ch.Open(nil))
	before := len(s.sent())
	if err := msg.Send(make([]byte, maxFrame)); err == nil {
		t.Error("Send of a message over the frame limit returned no error")
	}
	if n := len(s.sent()); n != before {
		t.Errorf("%d frames were written after the refused Send, want %d", n-before, 0)
	}
}

// batchVectors mirrors the scenarios that spec/gen/protomux.js adds to spec/vectors/protomux.json.
type batchVectors struct {
	ReplyBatch struct {
		Input   []vectorFrame `json:"input"`
		Replies []vectorFrame `json:"replies"`
	} `json:"reply_batch"`
	SplitBatch []splitFrame `json:"split_batch"`
}

// splitFrame is one frame of the split scenario, by its length and SHA-256: the frames are too large to
// keep as hex.
type splitFrame struct {
	Name   string `json:"name"`
	Length int    `json:"length"`
	SHA256 string `json:"sha256"`
}

// loadBatchVectors reads the batch scenarios of spec/vectors/protomux.json.
func loadBatchVectors(t *testing.T) batchVectors {
	t.Helper()
	var v batchVectors
	testvec.Load(t, "protomux.json", &v)
	return v
}

// Test case 5: a received batch of three messages is answered as upstream answers it. The frames the Mux
// writes are its open, then one batch of the three replies, byte for byte as upstream wrote them.
func TestReplyBatchMatchesUpstream(t *testing.T) {
	defer failOnPanic(t)
	v := loadBatchVectors(t).ReplyBatch
	s := newMemStream()
	defer s.Close()
	m := New(s)
	m.Pair("echo", nil, func() {
		ch := m.CreateChannel(ChannelOptions{Protocol: "echo"})
		var reply *Message
		ch.AddMessage(func(p []byte) {
			if err := reply.Send(p); err != nil {
				t.Error(err)
			}
		})
		reply = ch.AddMessage(ignore)
		if err := ch.Open(nil); err != nil {
			t.Error(err)
		}
	})
	s.feed(t, v.Input)
	deadline := time.Now().Add(5 * time.Second)
	for len(s.sent()) < len(v.Replies) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // an extra frame would show up too
	got := s.sent()
	if len(got) != len(v.Replies) {
		t.Fatalf("wrote %d frames, upstream wrote %d", len(got), len(v.Replies))
	}
	for i, f := range v.Replies {
		if hex.EncodeToString(got[i]) != f.Hex {
			t.Errorf("frame %d (%s): got %x, want %s", i, f.Name, got[i], f.Hex)
		}
	}
}

// Test case 6: nine messages of 1 MiB sent corked go out as the frames upstream wrote, the first eight as one
// batch and the ninth as another. Each frame has upstream's length and SHA-256.
func TestSplitBatchMatchesUpstream(t *testing.T) {
	defer failOnPanic(t)
	v := loadBatchVectors(t).SplitBatch
	s := newMemStream()
	defer s.Close()
	m := New(s)
	ch := m.CreateChannel(ChannelOptions{Protocol: "bulk"})
	msg := ch.AddMessage(ignore)
	mustNoError(t, ch.Open(nil))
	m.Cork()
	for i := 0; i < 9; i++ {
		mustNoError(t, msg.Send(bytes.Repeat([]byte{byte(i + 1)}, 1<<20)))
	}
	mustNoError(t, m.Uncork())
	got := s.sent()
	if len(got) != len(v) {
		t.Fatalf("wrote %d frames, upstream wrote %d", len(got), len(v))
	}
	for i, f := range v {
		sum := sha256.Sum256(got[i])
		if len(got[i]) != f.Length || hex.EncodeToString(sum[:]) != f.SHA256 {
			t.Errorf("frame %d (%s): %d bytes with SHA-256 %x, want %d bytes with %s",
				i, f.Name, len(got[i]), sum, f.Length, f.SHA256)
		}
	}
}
