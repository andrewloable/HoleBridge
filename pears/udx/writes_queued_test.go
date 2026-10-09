package udx

import (
	"math"
	"sync"
	"testing"
	"time"
)

// writesQueuedNow reads the stream's writesQueued under its lock.
func writesQueuedNow(st *Stream) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.writesQueued
}

// waitAcked waits until every packet st has sent is acked: nothing is still waiting to be packed,
// END has gone out, and the peer's cumulative ack has reached the next seq. It fails the test if
// that takes longer than d.
func waitAcked(t *testing.T, st *Stream, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		st.mu.Lock()
		err := st.err
		acked := len(st.wq) == 0 && !st.wqEnd && st.remoteAcked == st.seq
		st.mu.Unlock()
		if err != nil {
			t.Fatalf("stream failed before every packet was acked: %v", err)
		}
		if acked {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("not every packet was acked within %v", d)
		}
		time.Sleep(time.Millisecond)
	}
}

// queuedWatch samples writesQueued on its own goroutine, so a value that goes negative during the
// transfer is seen even if it is corrected before the transfer ends.
type queuedWatch struct {
	quit, done chan struct{}
	once       sync.Once
	min, max   int // lowest and highest sample
	n          int // number of samples
}

// watchWritesQueued starts sampling st.writesQueued until stop is called.
func watchWritesQueued(st *Stream) *queuedWatch {
	w := &queuedWatch{quit: make(chan struct{}), done: make(chan struct{}), min: math.MaxInt, max: math.MinInt}
	go func() {
		defer close(w.done)
		for {
			q := writesQueuedNow(st)
			w.min = min(w.min, q)
			w.max = max(w.max, q)
			w.n++
			select {
			case <-w.quit:
				return
			case <-time.After(50 * time.Microsecond):
			}
		}
	}()
	return w
}

// stop ends the sampling. It is safe to call more than once.
func (w *queuedWatch) stop() {
	w.once.Do(func() {
		close(w.quit)
		<-w.done
	})
}

// drainAndCheck sends data from a to b over the pair's link, waits until every byte is acked, and
// checks writesQueued. While the transfer runs it samples writesQueued, which must never be
// negative and must rise above 0, or the check saw nothing in flight. When everything is acked,
// writesQueued must be 0 again: ackPacket takes each acked payload back out of it.
func drainAndCheck(t *testing.T, a, b *Stream, data []byte) {
	t.Helper()
	w := watchWritesQueued(a)
	t.Cleanup(w.stop)
	sendAndCheck(t, a, b, data, time.Minute)
	waitAcked(t, a, time.Minute)
	w.stop()
	if w.n == 0 {
		t.Fatal("writesQueued was never sampled")
	}
	if w.min < 0 {
		t.Fatalf("writesQueued went to %d during the transfer, want never negative", w.min)
	}
	if w.max <= 0 {
		t.Fatalf("writesQueued never rose above 0 during the transfer (max %d); the check saw no bytes in flight", w.max)
	}
	if got := writesQueuedNow(a); got != 0 {
		t.Fatalf("writesQueued = %d after all %d bytes were acked, want 0", got, len(data))
	}
}

// Writing N bytes over a clean loopback pair and waiting until every packet is acked leaves
// writesQueued at 0, so the decrement in ackPacket takes back every byte the increment in Write added.
func TestWritesQueuedReturnsToZeroOnceAcked(t *testing.T) {
	defer failOnPanic(t)
	a, b := pairStreams(t, linkConfig{Seed: 1})
	drainAndCheck(t, a, b, payload(1<<20))
}

// The same holds over a link with 5% loss, 5% reordering and 1% duplication. Packets are sacked,
// resent and acked cumulatively in a different order, and writesQueued must still never go negative
// and must end at 0 (ackPacket skips a packet that was already sacked, so it is taken back once).
func TestWritesQueuedReturnsToZeroOverLossyLink(t *testing.T) {
	defer failOnPanic(t)
	a, b := pairStreams(t, linkConfig{Loss: 0.05, Reorder: 0.05, Duplicate: 0.01, Delay: time.Millisecond, Seed: 7})
	drainAndCheck(t, a, b, payload(4<<20))
}
