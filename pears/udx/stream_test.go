package udx

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"syscall"
	"testing"
	"time"
)

// Upstream values, measured by running udx-native 1.21.3 (the pinned JS reference in
// spikes/pears-go/js) on localhost with node v24.15.0. Each measurement was repeated and agreed.
// A silent UDP peer that never acks gets the first DATA packet at t=0, then retransmissions at
// 1.0 s, 3.0 s, 5.0 s and so on: the gaps are 1 s, then 2 s and stay at 2 s. The seventh expiry
// (13 s) errors the stream with ETIMEDOUT. With no RTT sample yet, the initial RTO is 1 s.
const (
	wantInitialRTO   = time.Second
	wantBackedOffRTO = 2 * time.Second
	wantGiveUp       = 13 * time.Second
)

// ioTimeout bounds each read and write in these tests, so a stuck stream fails the test.
const ioTimeout = 10 * time.Second

// promptly bounds how long a peer may take to see a destroyed stream. udx-native sees it in about 2 ms.
const promptly = 2 * time.Second

var _ io.ReadWriteCloser = (*Stream)(nil)

// payload returns n pseudo-random bytes from a fixed seed, so a failure can be reproduced.
func payload(n int) []byte {
	b := make([]byte, n)
	src := rand.NewChaCha8([32]byte{'h', 'o', 'l', 'e', 'b', 'r', 'i', 'd', 'g', 'e'})
	src.Read(b)
	return b
}

// readWithin reads exactly n bytes from r. It fails the test if they do not arrive within d.
func readWithin(t *testing.T, r io.Reader, n int, d time.Duration) ([]byte, error) {
	t.Helper()
	type result struct {
		b   []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		b := make([]byte, n)
		k, err := io.ReadFull(r, b)
		done <- result{b[:k], err}
	}()
	select {
	case res := <-done:
		return res.b, res.err
	case <-time.After(d):
		t.Fatalf("read of %d bytes did not finish within %v", n, d)
		return nil, nil
	}
}

// sendAndCheck writes data on a, then closes its write side. It checks that b reads exactly data,
// in order, and then io.EOF. Each wait is bounded by d.
func sendAndCheck(t *testing.T, a, b *Stream, data []byte, d time.Duration) {
	t.Helper()
	werr := make(chan error, 1)
	go func() {
		if _, err := a.Write(data); err != nil {
			werr <- err
			return
		}
		werr <- a.CloseWrite()
	}()
	got, err := readWithin(t, b, len(data), d)
	if err != nil {
		t.Fatalf("read %d bytes: %v", len(data), err)
	}
	for i := range data {
		if got[i] != data[i] {
			t.Fatalf("byte %d = %#x, want %#x", i, got[i], data[i])
		}
	}
	if _, err := readWithin(t, b, 1, d); err != io.EOF {
		t.Fatalf("read after the data = %v, want io.EOF", err)
	}
	select {
	case err := <-werr:
		if err != nil {
			t.Fatalf("Write or CloseWrite: %v", err)
		}
	case <-time.After(d):
		t.Fatal("Write did not return")
	}
}

// Two streams over a perfect in-memory link move 50 MiB intact.
func TestStreamsMoveBulkDataIntact(t *testing.T) {
	defer failOnPanic(t)
	a, b := pairStreams(t, linkConfig{})
	sendAndCheck(t, a, b, payload(50<<20), time.Minute)
}

// CloseWrite gives the reader io.EOF after all the data, and the other direction still works.
func TestCloseWriteEndsReadSideAndStaysHalfOpen(t *testing.T) {
	defer failOnPanic(t)
	a, b := pairStreams(t, linkConfig{})
	if _, err := a.Write([]byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := a.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	got, err := readWithin(t, b, 5, ioTimeout)
	if err != nil || string(got) != "hello" {
		t.Fatalf("read before EOF = %q, %v, want hello", got, err)
	}
	if _, err := readWithin(t, b, 1, ioTimeout); err != io.EOF {
		t.Fatalf("read after the data = %v, want io.EOF", err)
	}
	if _, err := b.Write([]byte("reply")); err != nil {
		t.Fatalf("Write back after EOF: %v", err)
	}
	got, err = readWithin(t, a, 5, ioTimeout)
	if err != nil || string(got) != "reply" {
		t.Fatalf("read back = %q, %v, want reply", got, err)
	}
}

// Destroy makes the other side's Read return an error promptly. udx-native reports ECONNRESET.
func TestDestroyMakesPeerReadFailPromptly(t *testing.T) {
	defer failOnPanic(t)
	a, b := pairStreams(t, linkConfig{})
	if err := a.Destroy(); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	_, err := readWithin(t, b, 1, promptly)
	if !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("peer Read error = %v, want ECONNRESET", err)
	}
}

// A stream with no acks for longer than the upstream give-up time errors out with ETIMEDOUT.
func TestStreamWithoutAcksGivesUp(t *testing.T) {
	defer failOnPanic(t)
	sock, err := NewSocket(loopbackUDP(t))
	if err != nil {
		t.Fatalf("NewSocket: %v", err)
	}
	defer sock.Close()
	silent := loopbackUDP(t) // nothing reads it, so no ack ever comes back
	a := sock.NewStream(1001)
	defer a.Close()
	if err := a.Connect(2002, silent.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if _, err := a.Write(make([]byte, 1000)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	_, err = readWithin(t, a, 1, wantGiveUp+5*time.Second)
	if !errors.Is(err, syscall.ETIMEDOUT) {
		t.Fatalf("Read error = %v, want ETIMEDOUT after the give-up time %v", err, wantGiveUp)
	}
}

// wantRTOFloor is the least retransmission timeout once RTT samples exist. Measured from udx-native
// 1.21.3 on localhost with delays of 0 to 200 ms RTT: the retransmit gap after the tail-loss probe is
// 1000 to 1001 ms, where the RTT formula alone gives about 0.3 s at 100 ms (HoleBridge-85m.4.10).
const wantRTOFloor = time.Second

// After an RTT sample the timeout is the upstream floor, not the RTT formula, for every RTT below it:
// one sample, or many samples of one steady RTT.
func TestRTOFloorAfterRTTSample(t *testing.T) {
	defer failOnPanic(t)
	for _, rtt := range []time.Duration{0, time.Millisecond, 50 * time.Millisecond, 200 * time.Millisecond} {
		c := newRTOCalc()
		c.sample(rtt)
		if got := c.Timeout(); got != wantRTOFloor {
			t.Errorf("after one %v RTT sample the timeout is %v, want the floor %v", rtt, got, wantRTOFloor)
		}
	}
	c := newRTOCalc()
	for range 20 {
		c.sample(100 * time.Millisecond)
	}
	if got := c.Timeout(); got != wantRTOFloor {
		t.Errorf("after 20 samples of 100 ms the timeout is %v, want the floor %v", got, wantRTOFloor)
	}
}

// The retransmission timeout starts at the upstream initial RTO. After the first expiry the timer is
// twice that and stays there: 1 s, then 2 s, 2 s, 2 s. It does not keep doubling, as udx-native's
// udx_rto_timeout restarts the timer at rto * 2 (HoleBridge-85m.4.14).
func TestRTOBacksOffToTwiceTheBaseTimeout(t *testing.T) {
	defer failOnPanic(t)
	c := newRTOCalc()
	want := []time.Duration{wantInitialRTO, 2 * wantInitialRTO, wantBackedOffRTO, wantBackedOffRTO}
	for i, w := range want {
		if got := c.Timeout(); got != w {
			t.Fatalf("timeout after %d backoffs = %v, want %v", i, got, w)
		}
		c.Backoff()
	}
}

// The sample path is not capped at 2 s. Measured on udx-native 1.21.3 (localhost, 400 ms one-way delay,
// one warm-up packet): after one RTT sample of 800 ms the retransmission came 2.41 s after the tail-loss
// probe, which is srtt + 4 rttvar = 800 + 4 x 400 ms (HoleBridge-85m.4.14).
func TestRTOFromSampleIsNotCappedAtTwoSeconds(t *testing.T) {
	defer failOnPanic(t)
	c := newRTOCalc()
	c.sample(800 * time.Millisecond)
	if got := c.Timeout(); got != 2400*time.Millisecond {
		t.Fatalf("timeout after one 800 ms sample = %v, want 2.4 s (srtt + 4 rttvar)", got)
	}
}

// The sample path caps the timeout at 30 s, the upstream UDX_RTO_MAX_MS. A first sample of 11 s gives
// srtt 11 s and rttvar 5.5 s, so srtt + 4 rttvar is 33 s and the timeout is 30 s. On udx-native 1.21.3 a
// smoothed RTT of 11.5 s gave a retransmission 30.0 s after the tail-loss probe (HoleBridge-85m.4.14).
func TestRTOFromSamplesCapsAtThirtySeconds(t *testing.T) {
	defer failOnPanic(t)
	c := newRTOCalc()
	c.sample(11 * time.Second)
	if got := c.Timeout(); got != 30*time.Second {
		t.Fatalf("timeout after an 11 s sample = %v, want the 30 s cap", got)
	}
}

// After an expiry the timer is twice the timeout and has no cap, as udx-native's udx_rto_timeout restarts
// it at rto * 2. Measured after an 800 ms sample: the gaps between retransmissions were 4.83 s, twice
// 2.41 s, and they stayed there (HoleBridge-85m.4.14).
func TestRTOBackoffWithSamplesIsTwiceTheTimeout(t *testing.T) {
	defer failOnPanic(t)
	c := newRTOCalc()
	c.sample(800 * time.Millisecond)
	want := []time.Duration{2400 * time.Millisecond, 4800 * time.Millisecond, 4800 * time.Millisecond}
	for i, w := range want {
		if got := c.Timeout(); got != w {
			t.Fatalf("timeout after %d expiries = %v, want %v", i, got, w)
		}
		c.Backoff()
	}
}

// An ack that advances the window re-arms the timer at the base timeout, as udx-native does when it
// resets rto_count on ack progress.
func TestRTOProgressRearmsAtBaseTimeout(t *testing.T) {
	defer failOnPanic(t)
	c := newRTOCalc()
	c.sample(800 * time.Millisecond)
	c.Backoff()
	c.Backoff()
	c.Progress()
	if got := c.Timeout(); got != 2400*time.Millisecond {
		t.Fatalf("timeout after ack progress = %v, want the base 2.4 s", got)
	}
}

// A reader that is slow to start stops the sender at the peer's receive window, and nothing is
// lost when the reader catches up. The 8 MiB write cannot finish until the peer reads.
func TestSlowReaderStopsSenderWithoutLoss(t *testing.T) {
	defer failOnPanic(t)
	a, b := pairStreams(t, linkConfig{})
	data := payload(8 << 20)
	werr := make(chan error, 1)
	go func() {
		_, err := a.Write(data)
		if err == nil {
			err = a.CloseWrite()
		}
		werr <- err
	}()
	select {
	case err := <-werr:
		t.Fatalf("Write of 8 MiB finished before the peer read anything: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	got, err := readWithin(t, b, len(data), time.Minute)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read after the window opened: err %v, equal %v", err, bytes.Equal(got, data))
	}
	select {
	case err := <-werr:
		if err != nil {
			t.Fatalf("Write or CloseWrite: %v", err)
		}
	case <-time.After(ioTimeout):
		t.Fatal("Write did not return")
	}
}

// A reader that stays away longer than the give-up time does not end the stream. While the reader is
// slow the peer advertises a zero window and still acks every probe, so the sender keeps probing like
// a persist timer and no byte is lost. The 4 MiB receive bound stays; the give-up count is for a peer
// that stops answering (HoleBridge-85m.4.11, orchestrator decision 2026-10-09).
func TestZeroWindowPersistsPastGiveUpTime(t *testing.T) {
	defer failOnPanic(t)
	a, b := pairStreams(t, linkConfig{})
	data := payload(8 << 20)
	werr := make(chan error, 1)
	go func() {
		_, err := a.Write(data)
		if err == nil {
			err = a.CloseWrite()
		}
		werr <- err
	}()
	// Nothing reads for longer than wantGiveUp, so the window stays closed past the give-up time.
	select {
	case err := <-werr:
		t.Fatalf("Write ended while the reader was slow: %v", err)
	case <-time.After(wantGiveUp + 2*time.Second):
	}
	got, err := readWithin(t, b, len(data), time.Minute)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read after the long stall: err %v, equal %v", err, bytes.Equal(got, data))
	}
	if n, err := b.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatalf("read after the data = (%d, %v), want (0, io.EOF)", n, err)
	}
	select {
	case err := <-werr:
		if err != nil {
			t.Fatalf("Write or CloseWrite after the stall: %v", err)
		}
	case <-time.After(ioTimeout):
		t.Fatal("Write did not return after the reader caught up")
	}
}

// CloseWrite called while another goroutine is blocked in Write waits for that Write. Every byte of it
// goes out before END, the Write returns nil, and the reader gets all the bytes and then io.EOF. This
// matches udx-native 1.21.3, where end() waits for the queued write and END rides on its last DATA
// packet (HoleBridge-85m.4.11).
func TestCloseWriteWaitsForBlockedWrite(t *testing.T) {
	defer failOnPanic(t)
	a, b := pairStreams(t, linkConfig{})
	data := payload(8 << 20)
	werr := make(chan error, 1)
	go func() {
		_, err := a.Write(data)
		werr <- err
	}()
	// The write blocks at the peer's window, since nothing reads yet.
	time.Sleep(300 * time.Millisecond)
	cerr := make(chan error, 1)
	go func() { cerr <- a.CloseWrite() }()
	select {
	case err := <-cerr:
		t.Fatalf("CloseWrite returned (%v) while the write was still blocked", err)
	case <-time.After(300 * time.Millisecond):
	}
	got, err := readWithin(t, b, len(data), time.Minute)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read of the blocked write: err %v, equal %v", err, bytes.Equal(got, data))
	}
	if n, err := b.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatalf("read after the data = (%d, %v), want (0, io.EOF)", n, err)
	}
	select {
	case err := <-werr:
		if err != nil {
			t.Fatalf("blocked Write returned %v, want nil", err)
		}
	case <-time.After(ioTimeout):
		t.Fatal("blocked Write did not return")
	}
	select {
	case err := <-cerr:
		if err != nil {
			t.Fatalf("CloseWrite returned %v, want nil", err)
		}
	case <-time.After(ioTimeout):
		t.Fatal("CloseWrite did not return after the write")
	}
}

// A 100 MiB transfer over loopback, Go to Go, moves every byte and both ends finish. The sender writes
// in 1 MiB chunks, the receiver hashes what it reads. Before HoleBridge-85m.4.19 the sender stopped
// about 1.3 MB in: its token bucket was empty, no pacing timer was armed, no ack came, and Write waited
// for ever. The 30 s deadline covers the send and the receive together; HEAD takes about 1.3 s.
func TestLargeLoopbackTransferDoesNotStall(t *testing.T) {
	defer failOnPanic(t)
	const total, chunk = 100 << 20, 1 << 20
	connA, connB := loopbackUDP(t), loopbackUDP(t)
	sockA, err := NewSocket(connA)
	if err != nil {
		t.Fatalf("NewSocket: %v", err)
	}
	defer sockA.Close()
	sockB, err := NewSocket(connB)
	if err != nil {
		t.Fatalf("NewSocket: %v", err)
	}
	defer sockB.Close()
	snd := sockA.NewStream(0x1001)
	rcv := sockB.NewStream(0x2002)
	defer snd.Destroy()
	defer rcv.Destroy()
	if err := snd.Connect(0x2002, connB.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := rcv.Connect(0x1001, connA.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	data := payload(total)
	want := sha256.Sum256(data)
	type result struct {
		n   int64
		sum [sha256.Size]byte
		err error
	}
	got := make(chan result, 1)
	go func() {
		h := sha256.New()
		n, err := io.Copy(h, rcv)
		var r result
		r.n, r.err = n, err
		copy(r.sum[:], h.Sum(nil))
		got <- r
	}()

	start := time.Now()
	deadline := time.After(30 * time.Second)
	sendDone := make(chan error, 1)
	go func() {
		for off := 0; off < total; off += chunk {
			if _, err := snd.Write(data[off : off+chunk]); err != nil {
				sendDone <- err
				return
			}
		}
		sendDone <- snd.CloseWrite()
	}()
	select {
	case err := <-sendDone:
		if err != nil {
			t.Fatalf("Write or CloseWrite of 100 MiB: %v", err)
		}
	case <-deadline:
		snd.Destroy() // unblocks the stalled Write so the goroutine ends
		rcv.Destroy()
		t.Fatal("100 MiB Write did not return within 30 s: the sender stalled")
	}
	var r result
	select {
	case r = <-got:
	case <-deadline:
		rcv.Destroy()
		t.Fatal("100 MiB read did not finish within 30 s")
	}
	elapsed := time.Since(start).Round(time.Millisecond)
	snd.mu.Lock()
	lost := snd.lost
	snd.mu.Unlock()
	t.Logf("100 MiB Go to Go: %v, sender Stream.lost %d, Dropped sender socket %d, receiver socket %d",
		elapsed, lost, sockA.Dropped(), sockB.Dropped())
	if r.err != nil || r.n != total {
		t.Fatalf("received %d of %d bytes, err %v", r.n, total, r.err)
	}
	if r.sum != want {
		t.Fatal("received bytes differ from the bytes sent")
	}
}
