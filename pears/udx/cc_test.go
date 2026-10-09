package udx

import (
	"testing"
	"time"
)

// Upstream values. The constants are libudx's (udx.c, UDX_CONG_INIT_CWND 10, ssthresh 255,
// UDX_CONG_MAX_CWND 65536). The initial window, the slow start doubling and the loss response were
// measured on udx-native 1.21.3, the pinned JS reference, run with node on localhost. A silent peer
// received first bursts of 10 packets, then 20, 40 and 80 after each full ack. After a retransmission
// timeout from a window of 80 and a full ack, 82 new packets went out, so the window after the RTO is
// 2 (2 + 80). A loss revealed by SACK blocks sent 159 packets at once, with no cut. The 1.21.3 build
// is libudx commit ae8bff7, which runs BBR; its RTO sets the window to the packets still in flight plus
// one (udx_rto_timeout), which is 2 with one packet in flight (HoleBridge-85m.4.12).
const (
	wantInitialWindow = 10
	wantMaxCwnd       = 65536
	wantSsthresh      = 255
	wantRTOWindow     = 2
)

// ccRTT is the RTT sample given with every synthetic ack in these tests.
const ccRTT = 50 * time.Millisecond

// ccStart is the clock the synthetic acks run on.
var ccStart = time.Unix(1000, 0)

// The window grows during the start phase as upstream does: each ack adds the packets it covers,
// so ten single-packet acks take the window from 10 to 20, and one ack for 20 packets takes it to 40.
func TestWindowGrowsInStartPhaseLikeUpstream(t *testing.T) {
	defer failOnPanic(t)
	c := newCongestion()
	if got := c.Window(); got != wantInitialWindow {
		t.Fatalf("initial window = %d packets, want %d", got, wantInitialWindow)
	}
	for i := 0; i < wantInitialWindow; i++ {
		c.OnAck(1, ccRTT, ccStart)
	}
	if got := c.Window(); got != 20 {
		t.Fatalf("window after ten single-packet acks = %d, want 20", got)
	}
	c.OnAck(20, ccRTT, ccStart)
	if got := c.Window(); got != 40 {
		t.Fatalf("window after one ack for 20 packets = %d, want 40", got)
	}
	c.OnAck(40, ccRTT, ccStart)
	if got := c.Window(); got != 80 {
		t.Fatalf("window after one ack for 40 packets = %d, want 80", got)
	}
}

// After a retransmission timeout the window is 2 packets and the slow start threshold is kept, so a
// full ack of the 80 packets sent before it takes the window to 82 (2 + 80), as measured on
// udx-native 1.21.3 (HoleBridge-85m.4.12).
func TestRTOResetsWindowLikeUpstream(t *testing.T) {
	defer failOnPanic(t)
	c := newCongestion()
	c.OnAck(10, ccRTT, ccStart)
	c.OnAck(20, ccRTT, ccStart)
	c.OnAck(40, ccRTT, ccStart)
	if got := c.Window(); got != 80 {
		t.Fatalf("window before the timeout = %d, want 80", got)
	}
	c.OnLoss(ccStart)
	if got := c.Window(); got != wantRTOWindow {
		t.Fatalf("window after the timeout = %d, want %d", got, wantRTOWindow)
	}
	c.OnAck(80, ccRTT, ccStart)
	if got := c.Window(); got != 82 {
		t.Fatalf("window after a full ack of the 80 packets = %d, want 82 (2 + 80)", got)
	}
}

// Over a lossy link with 50 ms one-way delay and 1% loss, 100 MiB moves at no less than 80% of the
// throughput a fixed window of sendWindow packets achieves on the same link (sanity bound).
func TestCongestionKeepsLossyLinkThroughput(t *testing.T) {
	defer failOnPanic(t)
	const size = 100 << 20
	link := linkConfig{Loss: 0.01, Delay: 50 * time.Millisecond, Seed: 3}
	data := payload(size)

	fixedA, fixedB := pairStreams(t, link)
	fixedA.pinWindow(sendWindow)
	fixed := timedTransfer(t, fixedA, fixedB, data)

	ccA, ccB := pairStreams(t, link)
	cc := timedTransfer(t, ccA, ccB, data)

	fixedRate := float64(size) / fixed.Seconds()
	ccRate := float64(size) / cc.Seconds()
	if ccRate < 0.8*fixedRate {
		t.Fatalf("congestion control moved %.0f B/s, fixed window %.0f B/s: below 80%%", ccRate, fixedRate)
	}
}

// timedTransfer sends data from a to b and returns how long the transfer took.
func timedTransfer(t *testing.T, a, b *Stream, data []byte) time.Duration {
	t.Helper()
	start := time.Now()
	sendAndCheck(t, a, b, data, 5*time.Minute)
	return time.Since(start)
}

// The controller's limits are the upstream constants. libudx has no pacing, so the window is the
// only burst limit.
func TestCongestionConstantsMatchUpstream(t *testing.T) {
	defer failOnPanic(t)
	got := upstreamCCParams()
	want := ccParams{
		InitCwnd:     wantInitialWindow,
		InitSsthresh: wantSsthresh,
		MaxCwnd:      wantMaxCwnd,
		RTOWindow:    wantRTOWindow,
	}
	if got != want {
		t.Fatalf("upstream constants = %+v, want %+v", got, want)
	}
}
