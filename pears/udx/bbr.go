// Ported from libudx src/udx_bbr.c (commit ae8bff7), the BBR congestion control that udx-native
// 1.21.3 runs. libudx is Apache License 2.0, Copyright 2021 Holepunch Inc (its NOTICE file).
package udx

// BBR modes (UDX_BBR_STATE_*) and congestion avoidance states (UDX_CA_*).
const (
	bbrStartup uint8 = iota
	bbrDrain
	bbrProbeBW
	bbrProbeRTT
)

const (
	caOpen uint8 = iota + 1
	caRecovery
	caLoss
)

const (
	bbrCycleLen          = 8
	bbrBWFilterRounds    = bbrCycleLen + 2 // window of the max bw filter, in packet-timed rounds
	bbrMinRTTIntervalMS  = 10000           // the min RTT filter window
	bbrMinProbeRTTModeMS = 200
	bbrCwndMinTarget     = 4
	bbrPacingMargin      = 0.99
	bbrFullBWThresh      = 1.25
	bbrFullBWCount       = 3
	bbrExtraAckedWinRTTs = 5
	bbrExtraAckedMaxMS   = 100
)

// The gains are C doubles in udx_bbr.c. They are variables so that the drain gain, 1 / 2.88539, is
// computed at run time in float64, as libudx computes it.
var (
	bbrHighGain  float64 = 2.88539 // 2/ln(2)
	bbrDrainGain float64 = 1 / bbrHighGain
	bbrCwndGain  float64 = 2.0

	bbrPacingGainCycle = [bbrCycleLen]float64{1.25, 0.75, 1.0, 1.0, 1.0, 1.0, 1.0, 1.0}
)

// bbrState is the udx_stream_t.bbr block: the BBR state that udx_bbr.c keeps for one stream.
type bbrState struct {
	minRTTms              uint32
	minRTTStamp           uint64
	probeRTTDoneTime      uint64
	bw                    winFilter[float64] // max recent delivery rate, packets/ms
	rttCount              uint32
	nextRTTDelivered      uint32
	cycleTimestamp        uint64
	state                 uint8
	prevCAState           uint8
	usePacketConservation bool
	roundStart            bool
	idleRestart           bool
	probeRTTRoundDone     bool
	pacingGain            float32 // a C float: the gains are kept at float precision
	cwndGain              float32
	fullBWReached         bool
	fullBWCount           uint8
	cycleIndex            uint8
	hasSeenRTT            bool
	priorCwnd             uint32
	fullBW                float64
	ackEpochStart         uint64
	extraAcked            [2]uint16
	ackEpochAcked         uint32
	extraAckedWinRTTs     uint8
	extraAckedWinIndex    uint8
}

// bbrFlow is the udx_stream_t state that udx_bbr.c reads and writes, apart from the bbr block:
// the delivery rate fields (delivered, delivered_ts, app_limited), the window, the pacing rate, the
// smoothed RTT, the congestion state and the in-flight count. Times are in ms.
type bbrFlow struct {
	rate             rateState
	bbr              bbrState
	cwnd             uint32 // packets
	pacingBytesPerMS uint32
	srtt             uint32 // ms; 0 before the first RTT sample
	caState          uint8  // caOpen, caRecovery or caLoss
	inflight         uint32 // packets in flight
}

// initBBR is bbr_init. rttMinMS is udx_rtt_min, the min RTT filter's value (~0 before any sample).
func (f *bbrFlow) initBBR(nowMS uint64, rttMinMS uint32) {
	f.bbr.priorCwnd = 0
	f.bbr.rttCount = 0
	f.bbr.nextRTTDelivered = f.rate.delivered
	f.bbr.prevCAState = caOpen
	f.bbr.usePacketConservation = false

	f.bbr.probeRTTDoneTime = 0
	f.bbr.probeRTTRoundDone = false
	f.bbr.minRTTms = rttMinMS
	f.bbr.minRTTStamp = nowMS

	f.bbr.bw.reset(uint64(f.bbr.rttCount), 0)

	f.bbr.hasSeenRTT = false
	f.initPacingRateFromRTT()

	f.bbr.roundStart = false
	f.bbr.idleRestart = false
	f.bbr.fullBWReached = false
	f.bbr.fullBW = 0
	f.bbr.fullBWCount = 0
	f.bbr.cycleTimestamp = 0
	f.bbr.cycleIndex = 0
	f.resetStartupMode()

	f.bbr.ackEpochStart = nowMS
	f.bbr.ackEpochAcked = 0
	f.bbr.extraAckedWinRTTs = 0
	f.bbr.extraAckedWinIndex = 0
	f.bbr.extraAcked = [2]uint16{}
}

// saveCwnd is bbr_save_cwnd: it keeps the window to restore after recovery.
func (f *bbrFlow) saveCwnd() {
	if f.bbr.prevCAState < caRecovery && f.bbr.state != bbrProbeRTT {
		f.bbr.priorCwnd = f.cwnd
	} else {
		f.bbr.priorCwnd = max(f.bbr.priorCwnd, f.cwnd)
	}
}

// onTransmitStart is bbr_on_transmit_start, called when a packet is sent on an app limited stream.
func (f *bbrFlow) onTransmitStart(nowMS uint64) {
	if f.rate.appLimited != 0 {
		f.bbr.idleRestart = true
		f.bbr.ackEpochStart = nowMS
		f.bbr.ackEpochAcked = 0

		if f.bbr.state == bbrProbeBW {
			f.setPacingRate(f.maxBW(), 1.0)
		} else if f.bbr.state == bbrProbeRTT {
			f.checkProbeRTTDone(nowMS)
		}
	}
}

// onRateSample is bbr_main: it runs once per ack event with that event's rate sample.
func (f *bbrFlow) onRateSample(rs *rateSample, nowMS uint64) {
	f.updateModel(rs, nowMS)

	bw := f.maxBW()
	f.setPacingRate(bw, float64(f.bbr.pacingGain))
	f.setCwnd(rs, rs.ackedSacked, bw, float64(f.bbr.cwndGain))
}

// onRTO is bbr_on_rto: it simulates a loss rate sample when the retransmission timer fires.
func (f *bbrFlow) onRTO() {
	f.bbr.prevCAState = caLoss
	f.bbr.fullBW = 0
	f.bbr.roundStart = true
}

// maxBW is the windowed max delivery rate in packets/ms (bbr_max_bw and bbr_bw).
func (f *bbrFlow) maxBW() float64 { return f.bbr.bw.get() }

// extraAcked is the largest recent degree of ack aggregation, in packets.
func (f *bbrFlow) extraAcked() uint16 { return max(f.bbr.extraAcked[0], f.bbr.extraAcked[1]) }

// bwToPacingRate is bbr_bw_to_pacing_rate: bytes per ms at the pacing margin, at least 1.
func (f *bbrFlow) bwToPacingRate(bw, gain float64) uint64 {
	bpms := uint64(bw * float64(mss) * gain * bbrPacingMargin)
	return max(bpms, 1)
}

// bdp is bbr_bdp: bw x min RTT x gain in packets, at least 1, and 10 before the min RTT is known.
// bbr_inflight is the same value.
func (f *bbrFlow) bdp(bw, gain float64) uint32 {
	if f.bbr.minRTTms == ^uint32(0) {
		return 10 // cap at the initial cwnd
	}
	return max(uint32(bw*float64(f.bbr.minRTTms)*gain), 1)
}

// ackAggregationCwnd is bbr_ack_aggregation_cwnd: the extra window that absorbs ack aggregation,
// capped at 100 ms of packets at the current bandwidth. Its gain is 1.0, so it is not multiplied.
func (f *bbrFlow) ackAggregationCwnd() uint32 {
	if !f.bbr.fullBWReached {
		return 0
	}
	maxAggr := uint32(f.maxBW() * bbrExtraAckedMaxMS)
	return min(uint32(f.extraAcked()), maxAggr)
}

// setCwndToRecoverOrRestore is bbr_set_cwnd_to_recover_or_restore. It returns the window and whether
// packet conservation applies, which holds the window to the packets in flight.
func (f *bbrFlow) setCwndToRecoverOrRestore(rs *rateSample, acked uint32) (uint32, bool) {
	prevState := f.bbr.prevCAState
	state := f.caState
	cwnd := f.cwnd

	// an ack for p packets should release at most 2*p packets: first deduct the losses
	if rs.losses > 0 {
		cwnd = uint32(max(int64(cwnd)-int64(rs.losses), 1))
	}

	if state == caRecovery && prevState != caRecovery {
		f.bbr.usePacketConservation = true
		f.bbr.nextRTTDelivered = f.rate.delivered
		cwnd = f.inflight + acked
	} else if prevState >= caRecovery && state == caOpen {
		cwnd = max(cwnd, f.bbr.priorCwnd)
		f.bbr.usePacketConservation = false
	}

	f.bbr.prevCAState = state

	if f.bbr.usePacketConservation {
		return max(cwnd, f.inflight+acked), true
	}
	return cwnd, false
}

// setCwnd is bbr_set_cwnd: it grows the window toward the BDP target and caps it in PROBE_RTT.
func (f *bbrFlow) setCwnd(rs *rateSample, acked uint32, bw, gain float64) {
	cwnd := f.cwnd
	if acked != 0 {
		var conserving bool
		cwnd, conserving = f.setCwndToRecoverOrRestore(rs, acked)
		if !conserving {
			target := f.bdp(bw, gain) + f.ackAggregationCwnd()
			if f.bbr.fullBWReached {
				cwnd = min(cwnd+acked, target)
			} else if cwnd < target || f.rate.delivered < 10 {
				cwnd += acked
			}
			cwnd = max(cwnd, bbrCwndMinTarget)
		}
	}
	f.cwnd = cwnd
	if f.bbr.state == bbrProbeRTT {
		f.cwnd = min(f.cwnd, bbrCwndMinTarget)
	}
}

// timeDeltaMS is time_delta_ms. It is not clamped at 0: a negative difference wraps, as in libudx.
func timeDeltaMS(t1, t0 uint64) uint32 {
	return uint32(int64(t1) - int64(t0))
}

// timeDiff is time_diff: the signed difference t1 - t0 in ms.
func timeDiff(t1, t0 uint64) int64 {
	return int64(t1 - t0)
}

// isNextCyclePhase is bbr_is_next_cycle_phase: whether the PROBE_BW gain phase has run its course.
func (f *bbrFlow) isNextCyclePhase(rs *rateSample) bool {
	isFullLength := timeDeltaMS(f.rate.deliveredTS, f.bbr.cycleTimestamp) > f.bbr.minRTTms

	if f.bbr.pacingGain == 1.0 {
		return isFullLength
	}

	inflight := f.inflight
	bw := f.maxBW()
	if f.bbr.pacingGain > 1.0 {
		return isFullLength && (rs.losses != 0 || inflight > f.bdp(bw, float64(f.bbr.pacingGain)))
	}
	return isFullLength || inflight <= f.bdp(bw, 1.0)
}

// advanceCyclePhase moves PROBE_BW to the next gain phase.
func (f *bbrFlow) advanceCyclePhase() {
	f.bbr.cycleIndex = (f.bbr.cycleIndex + 1) & (bbrCycleLen - 1)
	f.bbr.cycleTimestamp = f.rate.deliveredTS
}

// updateCyclePhase is bbr_update_cycle_phase.
func (f *bbrFlow) updateCyclePhase(rs *rateSample) {
	if f.bbr.state == bbrProbeBW && f.isNextCyclePhase(rs) {
		f.advanceCyclePhase()
	}
}

// resetStartupMode enters STARTUP (4.3.1.1).
func (f *bbrFlow) resetStartupMode() {
	f.bbr.state = bbrStartup
}

// resetProbeBWMode enters PROBE_BW. The phase starts at 3, as in libudx, and advances at once.
func (f *bbrFlow) resetProbeBWMode() {
	f.bbr.state = bbrProbeBW
	f.bbr.cycleIndex = 3
	f.advanceCyclePhase()
}

// resetMode enters STARTUP until the pipe is full, then PROBE_BW.
func (f *bbrFlow) resetMode() {
	if !f.bbr.fullBWReached {
		f.resetStartupMode()
	} else {
		f.resetProbeBWMode()
	}
}

// updateBW is bbr_update_bw: packet-timed rounds, and the windowed max of the delivery rate.
func (f *bbrFlow) updateBW(rs *rateSample) {
	f.bbr.roundStart = false

	if rs.delivered < 0 || rs.intervalMS <= 0 {
		return
	}

	// see if we've reached the next round
	if int32(rs.priorDelivered-f.bbr.nextRTTDelivered) >= 0 {
		f.bbr.nextRTTDelivered = f.rate.delivered
		f.bbr.rttCount++
		f.bbr.roundStart = true
		f.bbr.usePacketConservation = false
	}

	// packets over the interval, in packets/ms
	bw := float64(rs.delivered) / float64(rs.intervalMS)
	if !rs.isAppLimited || bw >= f.maxBW() {
		f.bbr.bw.applyMax(bbrBWFilterRounds, uint64(f.bbr.rttCount), bw)
	}
}

// updateAckAggregation is bbr_update_ack_aggregation: the windowed max of how far acks ran ahead of
// the bandwidth estimate. libudx always aggregates acks on the sender.
func (f *bbrFlow) updateAckAggregation(rs *rateSample) {
	if rs.ackedSacked == 0 || rs.delivered < 0 || rs.intervalMS <= 0 {
		return
	}

	if f.bbr.roundStart {
		f.bbr.extraAckedWinRTTs = uint8(min(uint32(f.bbr.extraAckedWinRTTs)+1, 0xff))
		if f.bbr.extraAckedWinRTTs >= bbrExtraAckedWinRTTs {
			f.bbr.extraAckedWinRTTs = 0
			if f.bbr.extraAckedWinIndex != 0 {
				f.bbr.extraAckedWinIndex = 0
			} else {
				f.bbr.extraAckedWinIndex = 1
			}
			f.bbr.extraAcked[f.bbr.extraAckedWinIndex] = 0
		}
	}

	epochMS := timeDeltaMS(f.rate.deliveredTS, f.bbr.ackEpochStart)
	expectedAcked := uint32(f.maxBW() * float64(epochMS))

	// reset the epoch when acks come in below the expected rate, or when the count would overflow
	if f.bbr.ackEpochAcked <= expectedAcked || uint64(f.bbr.ackEpochAcked)+uint64(rs.ackedSacked) >= uint64(^uint32(0)) {
		f.bbr.ackEpochAcked = 0
		f.bbr.ackEpochStart = f.rate.deliveredTS
		expectedAcked = 0
	}

	f.bbr.ackEpochAcked += rs.ackedSacked

	extraAcked := min(f.bbr.ackEpochAcked-expectedAcked, f.cwnd)
	idx := f.bbr.extraAckedWinIndex
	if extraAcked > uint32(f.bbr.extraAcked[idx]) {
		f.bbr.extraAcked[idx] = uint16(extraAcked)
	}
}

// checkFullBWReached is bbr_check_full_bw_reached: the pipe is full once three packet-timed rounds
// that are not app limited bring no 25% growth in the max bandwidth.
func (f *bbrFlow) checkFullBWReached(rs *rateSample) {
	if f.bbr.fullBWReached || !f.bbr.roundStart || rs.isAppLimited {
		return
	}

	bwThresh := f.bbr.fullBW * bbrFullBWThresh
	if f.maxBW() >= bwThresh {
		f.bbr.fullBW = f.maxBW()
		f.bbr.fullBWCount = 0
		return
	}

	f.bbr.fullBWCount++
	f.bbr.fullBWReached = f.bbr.fullBWCount >= bbrFullBWCount
}

// checkDrain is bbr_check_drain: STARTUP drains once the pipe is full, and DRAIN ends once the
// in-flight count is back at the BDP.
func (f *bbrFlow) checkDrain() {
	if f.bbr.state == bbrStartup && f.bbr.fullBWReached {
		f.bbr.state = bbrDrain
	}

	if f.bbr.state == bbrDrain && f.inflight <= f.bdp(f.maxBW(), 1.0) {
		f.resetProbeBWMode()
	}
}

// checkProbeRTTDone is bbr_check_probe_rtt_done: it leaves PROBE_RTT once its end time has passed.
func (f *bbrFlow) checkProbeRTTDone(nowMS uint64) {
	if !(f.bbr.probeRTTDoneTime != 0 && timeDiff(nowMS, f.bbr.probeRTTDoneTime) > 0) {
		return
	}
	f.bbr.minRTTStamp = nowMS
	f.cwnd = max(f.cwnd, f.bbr.priorCwnd)
	f.resetMode()
}

// updateMinRTT is bbr_update_min_rtt. The min RTT filter is windowed for 10 s. When it expires,
// the sender enters PROBE_RTT, caps the window at 4 packets and holds for 200 ms and a round.
func (f *bbrFlow) updateMinRTT(rs *rateSample, nowMS uint64) {
	filterExpired := timeDiff(nowMS, f.bbr.minRTTStamp+bbrMinRTTIntervalMS) > 0

	if rs.rttMS >= 0 && (rs.rttMS < int64(f.bbr.minRTTms) || (filterExpired && !rs.isAckDelayed)) {
		// round up to 1 ms, since the timer has 1 ms resolution
		if rs.rttMS != 0 {
			f.bbr.minRTTms = uint32(rs.rttMS)
		} else {
			f.bbr.minRTTms = 1
		}
		f.bbr.minRTTStamp = nowMS
	}

	if filterExpired && !f.bbr.idleRestart && f.bbr.state != bbrProbeRTT {
		f.bbr.state = bbrProbeRTT
		f.saveCwnd()
		f.bbr.probeRTTDoneTime = 0
	}

	if f.bbr.state == bbrProbeRTT {
		// PROBE_RTT is app limited: a zero count means not app limited, so it is 1
		f.rate.appLimited = max(f.rate.delivered+f.inflight, 1)

		if f.bbr.probeRTTDoneTime == 0 && f.inflight <= bbrCwndMinTarget {
			f.bbr.probeRTTDoneTime = nowMS + bbrMinProbeRTTModeMS
			f.bbr.probeRTTRoundDone = false
			f.bbr.nextRTTDelivered = f.rate.delivered
		} else if f.bbr.probeRTTDoneTime != 0 {
			if f.bbr.roundStart {
				f.bbr.probeRTTRoundDone = true
			}
			if f.bbr.probeRTTRoundDone {
				f.checkProbeRTTDone(nowMS)
			}
		}
	}

	if rs.delivered > 0 {
		f.bbr.idleRestart = false
	}
}

// updateGains is bbr_update_gains: the pacing and cwnd gains of the current mode.
func (f *bbrFlow) updateGains() {
	switch f.bbr.state {
	case bbrStartup:
		f.bbr.pacingGain = float32(bbrHighGain)
		f.bbr.cwndGain = float32(bbrHighGain)
	case bbrDrain:
		f.bbr.pacingGain = float32(bbrDrainGain)
		f.bbr.cwndGain = float32(bbrHighGain)
	case bbrProbeBW:
		f.bbr.pacingGain = float32(bbrPacingGainCycle[f.bbr.cycleIndex])
		f.bbr.cwndGain = float32(bbrCwndGain)
	case bbrProbeRTT:
		f.bbr.pacingGain = 1
		f.bbr.cwndGain = 1
	}
}

// updateModel is bbr_update_model, in libudx's order.
func (f *bbrFlow) updateModel(rs *rateSample, nowMS uint64) {
	f.updateBW(rs)
	f.updateAckAggregation(rs)
	f.updateCyclePhase(rs)
	f.checkFullBWReached(rs)
	f.checkDrain()
	f.updateMinRTT(rs, nowMS)
	f.updateGains()
}

// initPacingRateFromRTT is bbr_init_pacing_rate_from_rtt: until there is a bandwidth estimate, the
// pacing rate is the window over the smoothed RTT, sent at the high gain.
func (f *bbrFlow) initPacingRateFromRTT() {
	srtt := uint32(1)
	if f.srtt != 0 {
		srtt = f.srtt
		f.bbr.hasSeenRTT = true
	}
	bw := float64(f.cwnd) / float64(srtt)
	f.pacingBytesPerMS = uint32(f.bwToPacingRate(bw, bbrHighGain))
}

// setPacingRate is bbr_set_pacing_rate. Outside full bandwidth the rate only rises.
func (f *bbrFlow) setPacingRate(bw, gain float64) {
	rateBPMS := f.bwToPacingRate(bw, gain)

	if !f.bbr.hasSeenRTT && f.srtt != 0 {
		f.initPacingRateFromRTT()
	}

	if f.bbr.fullBWReached || rateBPMS > uint64(f.pacingBytesPerMS) {
		f.pacingBytesPerMS = uint32(rateBPMS)
	}
}
