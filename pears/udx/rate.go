// Ported from libudx src/udx_rate.c (commit ae8bff7, the delivery rate sample) and from the
// rack_sent_after and stamp_ms_delta helpers in src/internal.h and src/udx_rate.c. Apache License 2.0,
// Copyright (c) 2021 Holepunch Inc.
package udx

// pktRate is the send-time state of one packet: the udx_packet_t fields that the rate code reads
// and writes. The caller sets seq, timeSent and retransmitted before onSend.
type pktRate struct {
	seq           uint32
	timeSent      uint64
	retransmitted bool
	firstSentTS   uint64
	deliveredTS   uint64
	delivered     uint32
	appLimited    bool
}

// rateSample is udx_rate_sample_t: what the acks of one ack event say about the delivery rate.
type rateSample struct {
	priorTimestamp uint64 // ms when the reference packet was sent; 0 when there is none
	priorDelivered uint32
	delivered      int32 // packets delivered over the interval; -1 when there is no reference
	intervalMS     int64 // the sample interval in ms; -1 when the sample is dropped
	sndIntervalMS  uint32
	rcvIntervalMS  uint32
	losses         int
	ackedSacked    uint32
	seq            uint32 // the reference packet
	isAppLimited   bool
	isRetrans      bool
	// rttMS is the ack's RTT sample in ms. The ack path starts it at -1 (udx.c 1687), since the zero
	// value is a 0 ms sample; BBR ignores a negative one.
	rttMS        int64
	isAckDelayed bool
}

// rateState is the rate sampling state of one stream: the udx_stream_t fields delivered,
// delivered_ts, first_sent_ts, app_limited and the saved rate sample (rate_delivered,
// rate_interval_ms and rate_sample_is_app_limited).
type rateState struct {
	delivered      uint32 // packets acked so far; the ack path adds one per packet after onAck
	deliveredTS    uint64
	firstSentTS    uint64
	appLimited     uint32 // the delivered count the app limit ends at; 0 when not app limited
	rateDelivered  uint32
	rateIntervalMS uint32
	rateAppLimited bool
}

// stampMSDelta is stamp_ms_delta: t1 minus t0 in ms, or 0 when t1 is before t0.
func stampMSDelta(t1, t0 uint64) uint32 {
	return uint32(max(int64(t1)-int64(t0), 0))
}

// rackSentAfter is rack_sent_after: packet 1 was sent after packet 2, by time or, at the same
// time, by sequence number.
func rackSentAfter(t1 uint64, seq1 uint32, t2 uint64, seq2 uint32) bool {
	return t1 > t2 || (t1 == t2 && int32(seq2-seq1) < 0)
}

// onSend is udx__rate_pkt_sent. idle is true when nothing is in flight, which starts a new sample
// interval at nowMS. Call checkAppLimited first, as _send_packet does.
func (r *rateState) onSend(p *pktRate, nowMS uint64, idle bool) {
	if idle {
		// no data in flight, so the rate sample intervals of this first flight start now
		r.firstSentTS = nowMS
		r.deliveredTS = nowMS
	}
	p.firstSentTS = r.firstSentTS
	p.deliveredTS = r.deliveredTS
	p.delivered = r.delivered
	p.appLimited = r.appLimited != 0
}

// onAck is udx__rate_pkt_delivered: an acked packet becomes the reference for the sample when it
// was sent after the current reference, or when there is none.
func (r *rateState) onAck(p *pktRate, rs *rateSample) {
	if p.deliveredTS == 0 {
		return
	}
	if rs.priorDelivered == 0 || rackSentAfter(p.timeSent, p.seq, r.firstSentTS, rs.seq) {
		rs.priorDelivered = p.delivered
		rs.priorTimestamp = p.deliveredTS
		rs.isAppLimited = p.appLimited
		rs.isRetrans = p.retransmitted
		rs.seq = p.seq
		// the send time of the most recently acked packet starts the next send interval
		r.firstSentTS = p.timeSent
		rs.intervalMS = int64(stampMSDelta(r.firstSentTS, p.firstSentTS))
	}
}

// gen is udx__rate_gen: it ends the sample of one ack event. delivered and lost are the packets
// acked and lost by the event, and minRTTms is the stream's min RTT.
func (r *rateState) gen(nowMS uint64, delivered, lost, minRTTms uint32, rs *rateSample) {
	// the app limit clears once delivered passes it
	if r.appLimited != 0 && int32(r.delivered-r.appLimited) > 0 {
		r.appLimited = 0
	}
	if delivered != 0 {
		r.deliveredTS = nowMS
	}
	rs.ackedSacked = delivered
	rs.losses = int(lost)
	if rs.priorTimestamp == 0 {
		rs.delivered = -1
		rs.intervalMS = -1
		return
	}
	rs.delivered = int32(r.delivered - rs.priorDelivered)
	sndMS := uint32(rs.intervalMS)
	ackMS := stampMSDelta(nowMS, rs.priorTimestamp)
	rs.intervalMS = int64(max(sndMS, ackMS))
	rs.sndIntervalMS = sndMS
	rs.rcvIntervalMS = ackMS
	if rs.intervalMS < int64(minRTTms) {
		rs.intervalMS = -1
		return
	}
	// an app limited sample replaces the saved one only when its rate is at least as high
	if !rs.isAppLimited || uint64(rs.delivered)*uint64(r.rateIntervalMS) >= uint64(r.rateDelivered)*uint64(rs.intervalMS) {
		r.rateDelivered = uint32(rs.delivered)
		r.rateIntervalMS = uint32(rs.intervalMS)
		r.rateAppLimited = rs.isAppLimited
	}
}

// checkAppLimited is udx__rate_check_app_limited: when nothing queued fills the window, the stream
// is app limited until the packets now in flight are acked.
func (r *rateState) checkAppLimited(queuedBytes, maxPayload, inflight, cwnd, retransmitQueued int) {
	if queuedBytes < maxPayload && inflight < cwnd && retransmitQueued == 0 {
		r.appLimited = r.delivered + uint32(inflight)
		if r.appLimited == 0 {
			// 0 means not app limited, so the limit runs to packet 1 when nothing is in flight
			r.appLimited = 1
		}
	}
}
