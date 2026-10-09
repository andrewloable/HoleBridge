// Ported from libudx udx.c (ae8bff7): the send path (stream_may_send, _send_packet, send_new_packet,
// retransmit_packet, send_packets), the ack path of process_packet and ack_packet, RACK loss
// detection (rack_detect_loss, udx_rack_reo_timeout), the retransmission timeout (udx_rto_timeout),
// and the token bucket that paces sends (update_pacing_time, pacing_timer_timeout). Apache License
// 2.0, Copyright (c) 2021 Holepunch Inc.
//
// Tail loss probes (rack 7.2 to 7.4) are ported. Not ported: zero window probes (the persist timer;
// a closed window is handled by the RTO, see stream.go), MTU probes, and keepalive.
package udx

import (
	"encoding/binary"
	"net"
	"syscall"
	"time"
)

const (
	rttMinWindowMS   = 300000 // UDX_RTT_MIN_WINDOW_MS, the min RTT filter window
	rttMaxMS         = 30000  // UDX_RTT_MAX_MS, the cap on an RTT sample and the outlier clamp
	rttOutlierMinMS  = 5000   // clamp_rtt only clamps samples above this
	tlpMaxAckDelayMS = 2      // UDX_TLP_MAX_ACK_DELAY, added to the probe timeout with one packet in flight
	initPacingRate   = 25000  // UDX_INIT_PACING_RATE, bytes per ms until BBR sets the rate
)

// sentPkt is one DATA or END packet kept until the peer acks it, so it can be resent. The embedded
// pktRate holds the send-time state the rate sample reads (udx_packet_t's rate fields, seq and
// retransmitted).
type sentPkt struct {
	pktRate
	typ     uint8
	payload []byte
	lost    bool // queued in the retransmit queue, waiting to be resent
	prev    *sentPkt
	next    *sentPkt
	list    *pktList // the queue this packet is in, if any
	// to and toID are the remote the packet was first sent to, bound at its first transmit (bind_packet_remote), so
	// a resend goes there even after the stream changed its remote.
	to   *net.UDPAddr
	toID uint32
}

// pktList is an intrusive doubly linked list of packets: the inflight queue (packets in the
// network, in send order) and the retransmit queue (lost packets, in the order they were marked).
type pktList struct {
	head, tail *sentPkt
	n          int
}

func (l *pktList) pushBack(p *sentPkt) {
	p.list = l
	p.prev, p.next = l.tail, nil
	if l.tail != nil {
		l.tail.next = p
	} else {
		l.head = p
	}
	l.tail = p
	l.n++
}

// remove unlinks p, which must be in l.
func (l *pktList) remove(p *sentPkt) {
	if p.prev != nil {
		p.prev.next = p.next
	} else {
		l.head = p.next
	}
	if p.next != nil {
		p.next.prev = p.prev
	} else {
		l.tail = p.prev
	}
	p.prev, p.next, p.list = nil, nil, nil
	l.n--
}

// seqDiff is seq_diff: the signed distance a - b in the 32-bit seq space.
func seqDiff(a, b uint32) int32 {
	return int32(a - b)
}

// seqCompare is seq_compare: -1, 0 or 1 as a is before, at or after b.
func seqCompare(a, b uint32) int {
	d := seqDiff(a, b)
	switch {
	case d < 0:
		return -1
	case d > 0:
		return 1
	}
	return 0
}

// monoStart is the origin of nowMS.
var monoStart = time.Now()

// clockBaseMS is added to the clock. uv_now is milliseconds since the loop started and is never 0 in
// practice, but a 0 delivery time means "no sample" to the rate code (udx__rate_pkt_delivered), so
// the first send of a process must not read 0.
const clockBaseMS = 1000

// nowMS reads the clock in ms: the loop time of libudx (uv_now) on a monotonic clock.
func nowMS() uint64 {
	return clockBaseMS + uint64(time.Since(monoStart)/time.Millisecond)
}

// tick sets the stream's loop clock at the start of an event: a timer, a packet, a pacing tick or a
// call from the application. libudx's uv_now is cached per loop iteration, so every send in one
// callback sees one time. The pacing bucket depends on it: a refill overwrites the bucket with the
// time since the last refill times the rate, so a clock that moved mid-burst would drop tokens.
func (st *Stream) tick() {
	st.loopMS = nowMS()
}

// inflightAdd puts p at the tail of the inflight queue. The count the BBR code reads follows it.
func (st *Stream) inflightAdd(p *sentPkt) {
	st.inflightQ.pushBack(p)
	st.inflightBytes += uint32(len(p.payload))
	st.bf.inflight = uint32(st.inflightQ.n)
}

// inflightDel takes p out of the inflight queue (acked, sacked or lost).
func (st *Stream) inflightDel(p *sentPkt) {
	st.inflightQ.remove(p)
	st.inflightBytes -= uint32(len(p.payload))
	st.bf.inflight = uint32(st.inflightQ.n)
}

// mayTransmit is stream_may_send: the token bucket has room and the window has room. A retransmit
// ignores the peer's receive window, which the receiver may have shrunk while data was in flight.
// A new packet keeps the peer's window, and with nothing in flight one packet may go anyway, so a
// zero peer window cannot stall the stream for good. An empty bucket arms the pacing timer: a timer
// that fires in the same ms as the last refill finds the bucket still empty, and without a timer
// armed nothing would send again (HoleBridge-85m.4.19).
func (st *Stream) mayTransmit(retransmit bool) bool {
	st.updatePacingTime(st.loopMS)
	if st.tbAvailable == 0 {
		st.armPacing()
		return false
	}
	if st.inflightQ.n >= st.window() {
		return false
	}
	if retransmit {
		return true
	}
	return st.inflightBytes == 0 || st.inflightBytes+mss <= st.peerWnd
}

// updatePacingTime refills the token bucket for the time since the last refill (update_pacing_time).
func (st *Stream) updatePacingTime(now uint64) {
	if now > st.tbLastRefill {
		st.tbAvailable = (now - st.tbLastRefill) * uint64(st.bf.pacingBytesPerMS)
		st.tbLastRefill = now
	}
}

// armPacing starts the 1 ms refill timer once the bucket is empty (refill_pacing_timer).
func (st *Stream) armPacing() {
	if st.paceTimer != nil {
		return
	}
	st.paceTimer = time.AfterFunc(time.Millisecond, st.onPaceTimer)
}

// onPaceTimer is pacing_timer_timeout: refill the bucket and send what the window allows.
func (st *Stream) onPaceTimer() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.tick()
	st.paceTimer = nil
	if st.err != nil {
		return
	}
	st.sendPackets()
	st.cond.Broadcast()
}

// sendPackets is send_packets: resend the retransmit queue while the window allows, then send new
// data from the write queue and, once it is empty, END.
func (st *Stream) sendPackets() {
	if st.err != nil || st.peer == nil {
		return
	}
	for st.rtxQ.n > 0 && st.mayTransmit(true) {
		st.retransmit(st.rtxQ.head)
	}
	for (len(st.wq) > 0 || st.wqEnd) && st.mayTransmit(false) {
		st.sendFromQueue(false)
	}
}

// sendFromQueue sends the next packet of the write queue: up to mss bytes of data, or END once the
// data is gone. A tail loss probe (tlp) takes the same packet and goes out past the window.
func (st *Stream) sendFromQueue(tlp bool) {
	if len(st.wq) == 0 {
		st.wqEnd = false
		st.sendNew(FlagEnd, nil, tlp)
		return
	}
	k := min(mss, len(st.wq))
	payload := append([]byte(nil), st.wq[:k]...)
	st.wq = st.wq[k:]
	if len(st.wq) == 0 {
		st.wq = nil
	}
	st.sendNew(FlagData, payload, tlp)
}

// sendNew sends a packet that takes the next seq and keeps it until the peer acks it (send_new_packet).
// A tail loss probe (tlp) is the one packet that goes out past the window; it arms the probe state.
func (st *Stream) sendNew(typ uint8, payload []byte, tlp bool) {
	p := &sentPkt{typ: typ, payload: payload}
	p.seq = st.seq
	idle := st.seq == st.remoteAcked
	st.seq++
	st.outgoing = append(st.outgoing, p)
	wasEmpty := st.inflightQ.n == 0
	st.transmit(p, false, idle)
	if wasEmpty {
		st.bf.onTransmitStart(st.loopMS)
	}
	st.armTimers(!tlp)
	if tlp {
		st.tlpIsRetrans = false
		st.tlpInFlight = true
		st.tlpEndSeq = p.seq
		st.tlpPermitted = false
	}
}

// retransmit resends a lost packet (retransmit_packet).
func (st *Stream) retransmit(p *sentPkt) {
	wasEmpty := st.inflightQ.n == 0
	st.transmit(p, true, st.seq == st.remoteAcked)
	if wasEmpty {
		st.bf.onTransmitStart(st.loopMS)
	}
	st.armTimers(false)
}

// transmit is _send_packet: it stamps the packet for the rate sample, moves it to the inflight
// queue, writes it to the wire and charges the token bucket.
func (st *Stream) transmit(p *sentPkt, retx bool, idle bool) {
	st.bf.rate.checkAppLimited(st.writesQueued, mss, st.inflightQ.n, int(st.bf.cwnd), st.rtxQ.n)
	if retx {
		p.retransmitted = true
	}
	now := st.loopMS
	p.timeSent = now
	st.bf.rate.onSend(&p.pktRate, now, idle)
	if p.lost {
		st.rtxQ.remove(p)
		p.lost = false
	}
	st.inflightAdd(p)
	if p.to == nil {
		p.to, p.toID = st.peer, st.remoteID
	}
	_ = st.sendTo(p.to, p.toID, p.typ, p.seq, p.payload) // a failed send is recovered by the timers, as in udx
	size := uint64(headerSize + len(p.payload))
	if size > st.tbAvailable {
		st.tbAvailable = 0
	} else {
		st.tbAvailable -= size
	}
	if st.tbAvailable == 0 {
		st.armPacing()
	}
}

// onAck is the ack path of process_packet (udx.c 1514 to 1799): the cumulative ack, the SACK blocks,
// RACK loss detection, the rate sample and BBR. The caller has already taken the peer's window.
func (st *Stream) onAck(h Header, payload []byte) {
	now := st.loopMS
	delivered0 := st.bf.rate.delivered
	lost0 := st.lost
	prior := st.remoteAcked
	ackAdvanced := seqDiff(h.Ack, prior) > 0
	dataInflight := st.remoteAcked != st.seq

	// an ack for data we have not sent is out of order: ignore it
	if seqCompare(st.seq, h.Ack) < 0 {
		return
	}

	// recovery ends once the ack passes the seq that started it
	if seqCompare(h.Ack, st.highSeq) > 0 && (st.bf.caState == caRecovery || st.bf.caState == caLoss) {
		st.bf.caState = caOpen
	}

	rs := rateSample{rttMS: -1}
	for p := prior; seqCompare(p, h.Ack) < 0; p++ {
		if st.ackPacket(p, false, &rs) {
			st.bf.rate.delivered++
		}
	}
	if ackAdvanced {
		st.outgoing = st.outgoing[int(h.Ack-prior):]
		st.remoteAcked = h.Ack
		st.rtoCount = 0
		st.rto.Progress()
		st.finishChange()
	}

	// rack 7.4.2: an ack past the probe means the probe is no longer outstanding
	if st.tlpInFlight && seqCompare(h.Ack, st.tlpEndSeq) >= 0 {
		if !st.tlpIsRetrans || seqCompare(h.Ack, st.tlpEndSeq) > 0 {
			st.tlpInFlight = false
		}
	}

	if h.Type&FlagSack != 0 {
		for b := payload; len(b) >= 8; b = b[8:] {
			start, end := binary.LittleEndian.Uint32(b), binary.LittleEndian.Uint32(b[4:])
			if !st.sackValid(start, end) {
				continue
			}
			for p := start; p != end; p++ {
				if st.ackPacket(p, true, &rs) {
					st.bf.rate.delivered++
				}
			}
		}
	}

	delivered := st.bf.rate.delivered - delivered0
	lost := st.lost - lost0
	armRTOOrTLP := ackAdvanced && dataInflight
	if delivered > 0 {
		if st.remoteAcked == st.seq {
			armRTOOrTLP = false
			st.stopTimer()
		} else {
			st.nextRTO = now + uint64(st.rto.base()/time.Millisecond)
		}
		// rack 6.2.5: a reordering timer replaces the RTO until it fires
		if st.rackDetectLossAndArmTimer() {
			armRTOOrTLP = false
		}
	}
	if armRTOOrTLP && !st.scheduleLossProbe(true) {
		st.rearmRTO(true)
	}

	// no rate sample when nothing was in flight: nothing could be acked
	if dataInflight {
		st.bf.rate.gen(now, delivered, lost, st.rttMin.get(), &rs)
		st.bf.onRateSample(&rs, now)
	}
	st.sendPackets()
}

// sackValid is udx_sack_is_valid: a block must lie inside the sent, unacked range.
func (st *Stream) sackValid(start, end uint32) bool {
	if seqDiff(end, st.seq) > 0 {
		return false
	}
	if seqDiff(start, end) >= 0 {
		return false
	}
	if seqDiff(start, st.seq) >= 0 {
		return false
	}
	return seqDiff(start, st.remoteAcked) >= 0
}

// ackPacket is ack_packet: the packet at seq is acked, cumulatively or by a SACK block. It leaves the
// outgoing and inflight (or retransmit) queues, takes an RTT sample when it was not resent (Karn),
// and updates the RACK state. It reports whether the packet was newly delivered.
func (st *Stream) ackPacket(seq uint32, sack bool, rs *rateSample) bool {
	i := int(seq - st.remoteAcked)
	p := st.outgoing[i]
	if p == nil {
		// already sacked: a cumulative ack no longer counts it as sacked
		if !sack {
			st.sacks--
		}
		return false
	}
	st.outgoing[i] = nil
	// a packet leaves outgoing once, so its bytes come off writesQueued once (on_bytes_acked)
	st.writesQueued -= len(p.payload)
	if sack {
		st.sacks++
	}
	if p.lost {
		st.rtxQ.remove(p)
		p.lost = false
	} else {
		st.inflightDel(p)
	}
	st.bf.rate.onAck(&p.pktRate, rs)

	now := st.loopMS
	rtt := st.clampRTT(now - p.timeSent)
	next := seq + 1
	if !p.retransmitted {
		// a 0 ms sample means under a millisecond on a 1 ms clock, not "no sample"
		if rtt == 0 {
			rtt = 1
		}
		st.rttMin.applyMin(rttMinWindowMS, now, rtt)
		st.rto.sample(time.Duration(rtt) * time.Millisecond)
		st.bf.srtt = uint32(st.rto.srttMS())
		st.tlpPermitted = true
	}

	// rack 6.2 step 2: the most recently sent packet acked sets the RACK reference
	if !p.retransmitted || rtt >= st.rttMin.get() {
		st.rackRTT = rtt
		rs.rttMS = int64(rtt)
		if rackSentAfter(p.timeSent, next, st.rackTimeSent, st.rackNextSeq) {
			st.rackTimeSent = p.timeSent
			st.rackNextSeq = next
		}
	}

	// rack 6.2 step 3: detect reordering
	if seqCompare(next, st.rackFack) > 0 {
		st.rackFack = next
	} else if seqCompare(next, st.rackFack) < 0 && !p.retransmitted {
		st.reorderingSeen = true
	}
	return true
}

// clampRTT is clamp_rtt: after the first sample a sample far above the smoothed RTT is clamped, so a
// suspended machine does not produce a huge RTO (udx.c 1243).
func (st *Stream) clampRTT(rtt uint64) uint32 {
	if !st.rto.sampled {
		return uint32(min(rtt, rttMaxMS))
	}
	outlier := st.rto.srttMS() + 5*st.rto.rttvarMS()
	if rtt > uint64(outlier) && rtt > rttOutlierMinMS {
		rtt = uint64(min(outlier, rttMaxMS))
	}
	return uint32(rtt)
}

// rackUpdateReoWnd is rack_update_reo_wnd (rack 6.2.4): the reordering window is zero until
// reordering is seen, except outside recovery and with fewer than three sacks.
func (st *Stream) rackUpdateReoWnd() uint32 {
	if !st.reorderingSeen {
		if st.bf.caState == caRecovery || st.bf.caState == caLoss {
			return 0
		}
		if st.sacks >= 3 {
			return 0
		}
	}
	r := st.rttMin.get() / 4
	return min(r, st.bf.srtt)
}

// rackDetectLoss is rack_detect_loss (rack 6.2): a packet sent before the most recently acked one,
// and older than the RACK RTT plus the reordering window, is lost. Lost packets move to the
// retransmit queue. It returns how long until the next packet may be declared lost, 0 when none is
// pending. The first loss puts the sender in recovery.
func (st *Stream) rackDetectLoss() uint64 {
	reoWnd := st.rackUpdateReoWnd()
	now := st.loopMS
	var timeout uint64
	resending := 0
	for p := st.inflightQ.head; p != nil; {
		next := p.next
		if p.timeSent > st.rackTimeSent {
			break
		}
		if rackSentAfter(st.rackTimeSent, st.rackNextSeq, p.timeSent, p.seq+1) {
			remaining := int64(p.timeSent+uint64(st.rackRTT)+uint64(reoWnd)) - int64(now)
			if remaining <= 0 {
				p.lost = true
				st.lost++
				st.inflightDel(p)
				st.rtxQ.pushBack(p)
				resending++
			} else if uint64(remaining) > timeout {
				timeout = uint64(remaining)
			}
		}
		p = next
	}

	if resending > 0 && st.bf.caState == caOpen {
		// recover until the window in flight now is acked
		st.bf.caState = caRecovery
		st.bf.saveCwnd()
		st.highSeq = st.seq
		st.tlpInFlight = false // rack 7.1 TLP_init
		st.tlpIsRetrans = false
	}

	st.sendPackets()
	return timeout
}

// rackDetectLossAndArmTimer runs rackDetectLoss and arms the reordering timer when a packet may still
// be lost later. It reports whether the timer was armed.
func (st *Stream) rackDetectLossAndArmTimer() bool {
	timeout := st.rackDetectLoss()
	if timeout > 0 {
		st.startTimer(timerRACKReo, time.Duration(timeout)*time.Millisecond)
		return true
	}
	return false
}

// onRACKReo is udx_rack_reo_timeout: detect loss, then put the RTO back at its own deadline.
func (st *Stream) onRACKReo() {
	st.rackDetectLoss()
	if st.pending != timerRTO {
		st.rearmRTO(false)
	}
}

// armTimers is arm_stream_timers: a send starts the RTO when no timer is running, and a send of new
// data (armTLP) moves the timer to the tail loss probe when the probe may run.
func (st *Stream) armTimers(armTLP bool) {
	if st.pending == timerNone {
		st.startTimer(timerRTO, st.rto.Timeout())
	}
	if armTLP && st.bf.caState == caOpen && st.sacks == 0 {
		st.scheduleLossProbe(false)
	}
}

// scheduleLossProbe is schedule_loss_probe (rack 7.2): the probe fires about two smoothed RTTs after
// the last send, or at the RTO when that is sooner. It reports whether the probe was armed.
func (st *Stream) scheduleLossProbe(advancingRTO bool) bool {
	if st.seq == st.remoteAcked || st.bf.caState != caOpen || st.sacks != 0 {
		return false
	}
	timeout := uint32(1000)
	if st.bf.srtt != 0 {
		timeout = st.bf.srtt * 2
		if st.inflightQ.n+st.rtxQ.n == 1 {
			timeout += tlpMaxAckDelayMS
		}
	}
	rtoDelta := uint32(st.rto.base() / time.Millisecond)
	if !advancingRTO {
		delta := int64(st.nextRTO) - int64(st.loopMS)
		if delta < 0 {
			delta = 1
		}
		rtoDelta = uint32(delta)
	}
	timeout = min(timeout, rtoDelta)
	st.startTimer(timerTLP, time.Duration(timeout)*time.Millisecond)
	return true
}

// onTLP is udx_tlp_timeout (rack 7.3): send one new packet past the window, or, with no data left,
// resend the last packet. Either one tells the peer about a tail loss.
func (st *Stream) onTLP() {
	if st.remoteAcked == st.seq {
		return
	}
	if st.tlpInFlight || !st.tlpPermitted {
		st.rearmRTO(false)
		return
	}
	if len(st.wq) > 0 || st.wqEnd {
		st.sendFromQueue(true)
	} else {
		last := st.outgoing[len(st.outgoing)-1]
		if last == nil || last.lost {
			st.rearmRTO(false)
			return
		}
		st.inflightDel(last)
		st.retransmit(last)
		st.tlpIsRetrans = true
		st.tlpInFlight = true
		st.tlpEndSeq = last.seq
		st.tlpPermitted = false
	}
	st.rearmRTO(true)
}

// rearmRTO is rearm_rto. From now, the timer restarts at the base timeout. Otherwise it runs to the
// deadline kept in nextRTO, which a reordering timer must not move.
func (st *Stream) rearmRTO(fromNow bool) {
	if st.remoteAcked == st.seq {
		st.stopTimer()
		return
	}
	d := st.rto.Timeout()
	if !fromNow {
		delta := int64(st.nextRTO) - int64(st.loopMS)
		if delta < 0 {
			delta = 1
		}
		d = time.Duration(delta) * time.Millisecond
	}
	st.startTimer(timerRTO, d)
}

// onRTO is udx_rto_timeout: the retransmission timer fired, so every packet not yet sacked that was
// sent at least one RACK RTT ago is lost (the head always is). The window becomes the packets still
// in flight plus one, and the sender resends from the retransmit queue.
func (st *Stream) onRTO() {
	st.pending = timerNone
	st.bf.saveCwnd()
	st.highSeq = st.seq
	st.rtoCount++
	st.bf.caState = caLoss
	st.tlpInFlight = false // rack 7.1 TLP_init
	st.tlpIsRetrans = false
	st.rto.Backoff()
	st.startTimer(timerRTO, st.rto.Timeout())
	if st.rtoCount >= maxRTOExpiries {
		st.teardown(syscall.ETIMEDOUT)
		return
	}

	// the retransmit queue is rebuilt from the lost packets, in seq order
	st.rtxQ = pktList{}
	now := st.loopMS
	reoWnd := st.rackUpdateReoWnd()
	for _, p := range st.outgoing {
		if p == nil {
			continue
		}
		if p.lost {
			st.rtxQ.pushBack(p)
			continue
		}
		remaining := int64(p.timeSent+uint64(st.rackRTT)+uint64(reoWnd)) - int64(now)
		if p.seq == st.remoteAcked || remaining < 0 {
			st.lost++
			p.lost = true
			st.inflightDel(p)
			st.rtxQ.pushBack(p)
		}
	}

	st.bf.cwnd = uint32(st.inflightQ.n + 1)
	st.bf.onRTO()
	st.sendPackets()
}
