package udx

import "testing"

// Upstream values. Each case was run through libudx ae8bff7 (src/udx_rate.c and src/internal.h,
// built with clang against the upstream headers) with the call order of _send_packet (udx.c 664)
// and the ack path (ack_packet 1260, then udx__rate_gen at 1789). The stream starts at seq 0 with a
// min RTT of 100 ms, cwnd 10 and a busy send queue, so the sender is not app limited unless a case
// says so. Times are in ms, as uv_now gives them.

// rateMaxPayload is udx__max_payload in the upstream runs (1152 bytes).
const rateMaxPayload = 1152

// rateFlow stands in for the stream fields the rate code reads: the send sequence, the cumulative
// ack, the packets in flight, the cwnd and the queued bytes. Its send and ack follow the upstream
// call order.
type rateFlow struct {
	r           rateState
	seq         uint32 // next sequence number to send
	remoteAcked uint32
	inflight    int
	cwnd        int
	queued      int
}

// send is _send_packet: the app limit check, the send time, the rate state, then the packet joins
// the flight and seq advances.
func (f *rateFlow) send(p *pktRate, now uint64) {
	f.r.checkAppLimited(f.queued, rateMaxPayload, f.inflight, f.cwnd, 0)
	p.seq = f.seq
	p.timeSent = now
	f.r.onSend(p, now, f.seq == f.remoteAcked)
	f.inflight++
	f.seq++
}

// ack is one packet of ack_packet: it leaves the flight, its sample is taken, then the ack path
// counts it as delivered.
func (f *rateFlow) ack(p *pktRate, rs *rateSample) {
	f.inflight--
	f.r.onAck(p, rs)
	f.r.delivered++
}

func TestRateSampleAfterAckBatchLikeUpstream(t *testing.T) {
	defer failOnPanic(t)
	const minRTT = 100
	f := &rateFlow{cwnd: 10, queued: 100000}
	p := make([]pktRate, 3)
	f.send(&p[0], 1000)
	f.send(&p[1], 1010)
	f.send(&p[2], 1020)
	wantSent := []pktRate{
		{seq: 0, timeSent: 1000, firstSentTS: 1000, deliveredTS: 1000},
		{seq: 1, timeSent: 1010, firstSentTS: 1000, deliveredTS: 1000},
		{seq: 2, timeSent: 1020, firstSentTS: 1000, deliveredTS: 1000},
	}
	for i := range wantSent {
		if p[i] != wantSent[i] {
			t.Fatalf("packet %d after send = %+v, want %+v", i, p[i], wantSent[i])
		}
	}

	// p0 acked alone at 1120 ms: 120 ms interval, 1 packet delivered.
	rs := rateSample{}
	f.ack(&p[0], &rs)
	f.remoteAcked = 1
	f.r.gen(1120, 1, 0, minRTT, &rs)
	if want := (rateSample{priorTimestamp: 1000, delivered: 1, intervalMS: 120, rcvIntervalMS: 120, ackedSacked: 1}); rs != want {
		t.Fatalf("sample after p0 = %+v, want %+v", rs, want)
	}
	if want := (rateState{delivered: 1, deliveredTS: 1120, firstSentTS: 1000, rateDelivered: 1, rateIntervalMS: 120}); f.r != want {
		t.Fatalf("rate state after p0 = %+v, want %+v", f.r, want)
	}

	// p1 and p2 acked together at 1200 ms: the reference is the later sent p2, which gives a 200 ms
	// interval from its send at 1020 ms, and 3 packets delivered since the start.
	rs = rateSample{}
	f.ack(&p[1], &rs)
	f.ack(&p[2], &rs)
	f.remoteAcked = 3
	f.r.gen(1200, 2, 0, minRTT, &rs)
	if want := (rateSample{priorTimestamp: 1000, delivered: 3, intervalMS: 200, sndIntervalMS: 20, rcvIntervalMS: 200, ackedSacked: 2, seq: 2}); rs != want {
		t.Fatalf("sample after p1 p2 = %+v, want %+v", rs, want)
	}
	if want := (rateState{delivered: 3, deliveredTS: 1200, firstSentTS: 1020, rateDelivered: 3, rateIntervalMS: 200}); f.r != want {
		t.Fatalf("rate state after p1 p2 = %+v, want %+v", f.r, want)
	}
}

// p3 is sent on an idle stream with nothing queued, so it is app limited and it starts a new
// interval. Its 120 ms sample does not replace the stored 3 packets in 200 ms, since 1 x 200 is
// less than 3 x 120, and the app limit clears once delivered passes it.
func TestRateSampleAppLimitedKeepsFasterStoredSampleLikeUpstream(t *testing.T) {
	defer failOnPanic(t)
	const minRTT = 100
	f := &rateFlow{cwnd: 10, seq: 3, remoteAcked: 3, r: rateState{
		delivered: 3, deliveredTS: 1200, firstSentTS: 1020, rateDelivered: 3, rateIntervalMS: 200,
	}}
	f.queued = 0
	var p pktRate
	f.send(&p, 1210)
	if want := (pktRate{seq: 3, timeSent: 1210, firstSentTS: 1210, deliveredTS: 1210, delivered: 3, appLimited: true}); p != want {
		t.Fatalf("app limited packet after send = %+v, want %+v", p, want)
	}
	if f.r.appLimited != 3 {
		t.Fatalf("app limit after send = %d, want 3", f.r.appLimited)
	}

	rs := rateSample{}
	f.ack(&p, &rs)
	f.remoteAcked = 4
	f.r.gen(1330, 1, 0, minRTT, &rs)
	if want := (rateSample{priorTimestamp: 1210, priorDelivered: 3, delivered: 1, intervalMS: 120, rcvIntervalMS: 120, ackedSacked: 1, seq: 3, isAppLimited: true}); rs != want {
		t.Fatalf("sample after p3 = %+v, want %+v", rs, want)
	}
	if want := (rateState{delivered: 4, deliveredTS: 1330, firstSentTS: 1210, rateDelivered: 3, rateIntervalMS: 200}); f.r != want {
		t.Fatalf("rate state after p3 = %+v, want %+v", f.r, want)
	}
}

// A sample under the min RTT is dropped: its interval is -1 and the stored rate is kept.
func TestRateSampleBelowMinRTTIsDroppedLikeUpstream(t *testing.T) {
	defer failOnPanic(t)
	f := &rateFlow{cwnd: 10, queued: 100000}
	var p pktRate
	f.send(&p, 1000)
	rs := rateSample{}
	f.ack(&p, &rs)
	f.remoteAcked = 1
	f.r.gen(1030, 1, 0, 50, &rs)
	if want := (rateSample{priorTimestamp: 1000, delivered: 1, intervalMS: -1, rcvIntervalMS: 30, ackedSacked: 1}); rs != want {
		t.Fatalf("sample below min RTT = %+v, want %+v", rs, want)
	}
	if want := (rateState{delivered: 1, deliveredTS: 1030, firstSentTS: 1000}); f.r != want {
		t.Fatalf("rate state below min RTT = %+v, want %+v", f.r, want)
	}
}

// A packet sent with delivered time 0 leaves the sample alone. With no reference, gen gives
// delivered -1 and interval -1, and still counts the acks and losses.
func TestRateSampleWithoutReferenceIsEmptyLikeUpstream(t *testing.T) {
	defer failOnPanic(t)
	var r rateState
	p := pktRate{seq: 4, timeSent: 900}
	rs := rateSample{}
	r.onAck(&p, &rs)
	if rs != (rateSample{}) {
		t.Fatalf("sample after a packet with delivered time 0 = %+v, want empty", rs)
	}
	r.gen(1000, 2, 1, 100, &rs)
	if want := (rateSample{delivered: -1, intervalMS: -1, losses: 1, ackedSacked: 2}); rs != want {
		t.Fatalf("sample without reference = %+v, want %+v", rs, want)
	}
	if want := (rateState{deliveredTS: 1000}); r != want {
		t.Fatalf("rate state without reference = %+v, want %+v", r, want)
	}
}

// The sender is app limited only when nothing queued fills the window: fewer than one payload
// queued, the flight below cwnd, and no retransmit queued. The limit is delivered plus the packets
// in flight, or 1 when that is 0, since 0 means not app limited.
func TestAppLimitedWhenWindowIsNotFilledLikeUpstream(t *testing.T) {
	defer failOnPanic(t)
	cases := []struct {
		name                                     string
		delivered                                uint32
		queued, inflight, cwnd, retransmitQueued int
		want                                     uint32
	}{
		{"nothing filling the window", 7, 0, 3, 10, 0, 10},
		{"window full", 7, 0, 10, 10, 0, 0},
		{"empty stream", 0, 0, 0, 10, 0, 1},
		{"retransmit queued", 5, 0, 2, 10, 1, 0},
		{"one payload queued", 5, rateMaxPayload, 2, 10, 0, 0},
	}
	for _, c := range cases {
		r := rateState{delivered: c.delivered}
		r.checkAppLimited(c.queued, rateMaxPayload, c.inflight, c.cwnd, c.retransmitQueued)
		if r.appLimited != c.want {
			t.Fatalf("%s: app limit = %d, want %d", c.name, r.appLimited, c.want)
		}
	}
}

// rack_sent_after from libudx src/internal.h: a later time wins, and at the same time the higher
// sequence number was sent later (serial arithmetic on the 32-bit sequence).
func TestRackSentAfterLikeUpstream(t *testing.T) {
	defer failOnPanic(t)
	cases := []struct {
		t1   uint64
		seq1 uint32
		t2   uint64
		seq2 uint32
		want bool
	}{
		{1300, 2, 1300, 1, true},
		{1300, 1, 1300, 2, false},
		{1300, 5, 1300, 5, false},
		{1310, 1, 1300, 9, true},
		{1290, 9, 1300, 1, false},
		{1300, 0xFFFFFFFF, 1300, 0, false},
		{1300, 0, 1300, 0xFFFFFFFF, true},
	}
	for _, c := range cases {
		if got := rackSentAfter(c.t1, c.seq1, c.t2, c.seq2); got != c.want {
			t.Fatalf("rackSentAfter(%d, %d, %d, %d) = %v, want %v", c.t1, c.seq1, c.t2, c.seq2, got, c.want)
		}
	}
}

// stamp_ms_delta clamps a negative interval to 0.
func TestStampMSDeltaClampsAtZeroLikeUpstream(t *testing.T) {
	defer failOnPanic(t)
	if got := stampMSDelta(1120, 1000); got != 120 {
		t.Fatalf("stampMSDelta(1120, 1000) = %d, want 120", got)
	}
	if got := stampMSDelta(1000, 1120); got != 0 {
		t.Fatalf("stampMSDelta(1000, 1120) = %d, want 0", got)
	}
}
