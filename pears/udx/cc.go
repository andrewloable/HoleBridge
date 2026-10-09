// Port of libudx's congestion control (udx.c: ack_update, update_congestion, increase_cwnd,
// reduce_cwnd and the UDX_CONG_* constants), Apache License 2.0, Copyright (c) 2021 Holepunch Inc.
//
// The growth is slow start (the window grows by the number of packets each ack covers) until the
// window reaches the slow start threshold, then CUBIC (the paper by Ha, Rhee and Xu, 2008). The
// window counts packets. A Stream keeps at most Window() packets in flight.
//
// The loss response is the one measured on udx-native 1.21.3 (HoleBridge-85m.4.12), not the 0.7 cut
// of the 1.12.0 source. 1.21.3 is built from libudx commit ae8bff7, which runs BBR (udx_bbr.c): a
// retransmission timeout sets the window to the packets still in flight plus one and leaves the
// threshold alone, so after an RTO from 80 packets and a full ack the window is 82 (2 + 80). Losses
// the SACK blocks reveal cut nothing. This port uses 2 after an RTO, the value with one packet still
// in flight, and it keeps CUBIC for the growth past the threshold, where 1.21.3 uses BBR. Porting
// BBR is not done (HoleBridge-85m.4.15 decides it).
package udx

import (
	"math"
	"time"
)

const (
	ccInitCwnd     = 10                                                    // UDX_CONG_INIT_CWND
	ccInitSsthresh = 255                                                   // stream->ssthresh at init
	ccMaxCwnd      = 65536                                                 // UDX_CONG_MAX_CWND
	ccRTOWindow    = 2                                                     // window after an RTO, measured on 1.21.3
	ccBeta         = 731                                                   // UDX_CONG_BETA, used by the CUBIC epoch
	ccBetaUnit     = 1024                                                  // UDX_CONG_BETA_UNIT
	ccC            = 400                                                   // UDX_CONG_C, C = 0.4 scaled by ccCScale
	ccCScale       = 1e12                                                  // UDX_CONG_C_SCALE
	ccCubeFactor   = ccCScale / ccC                                        // UDX_CONG_CUBE_FACTOR
	ccBetaScale    = 8 * (ccBetaUnit + ccBeta) / 3 / (ccBetaUnit - ccBeta) // UDX_CONG_BETA_SCALE
)

// ccParams are the controller constants the tests check, named as libudx names them.
type ccParams struct {
	InitCwnd     uint32 // packets in the first window (UDX_CONG_INIT_CWND)
	InitSsthresh uint32 // slow start threshold before the first loss (stream->ssthresh)
	MaxCwnd      uint32 // the window never grows past this (UDX_CONG_MAX_CWND)
	RTOWindow    uint32 // the window after a retransmission timeout, measured on udx-native 1.21.3
}

// congestion is the send window controller of a Stream. The fields after cwndCnt are the CUBIC
// epoch (udx_cong_t); times are in milliseconds.
type congestion struct {
	cwnd, ssthresh, cwndCnt uint32

	K, origin, cnt, ackCnt, delayMin uint32
	tcpCwnd, lastCwnd, lastMaxCwnd   uint32
	startTime, lastTime              uint64
}

// newCongestion returns a controller in its initial state, with the upstream initial window.
func newCongestion() *congestion {
	return &congestion{cwnd: ccInitCwnd, ssthresh: ccInitSsthresh}
}

// upstreamCCParams returns the constants the controller uses.
func upstreamCCParams() ccParams {
	return ccParams{
		InitCwnd:     ccInitCwnd,
		InitSsthresh: ccInitSsthresh,
		MaxCwnd:      ccMaxCwnd,
		RTOWindow:    ccRTOWindow,
	}
}

// OnAck reports that acked packets were newly acknowledged, with an RTT sample taken at now.
// The sample is the smoothed RTT (upstream uses stream->srtt here).
func (c *congestion) OnAck(acked int, rtt time.Duration, now time.Time) {
	n := uint32(acked)
	srtt := uint32(rtt / time.Millisecond)
	// Queueing delay: the epoch restarts while the RTT is four times the minimum.
	if c.delayMin > 0 && srtt > c.delayMin*4 {
		c.startTime = 0
		return
	}
	if c.delayMin == 0 || c.delayMin > srtt {
		c.delayMin = srtt
	}
	if c.cwnd < c.ssthresh {
		c.cwnd = min(c.cwnd+n, c.ssthresh)
		return
	}
	c.updateCongestion(n, uint64(now.UnixMilli()))
	c.increaseCwnd(n)
}

// updateCongestion works out the CUBIC step (cnt: acks per packet of growth) for this ack.
func (c *congestion) updateCongestion(acked uint32, now uint64) {
	c.ackCnt += acked

	// Acks in the same millisecond at the same window do not move the curve.
	if c.lastCwnd == c.cwnd && now-c.lastTime <= 3 {
		return
	}

	if c.startTime == 0 || now != c.lastTime {
		c.lastCwnd, c.lastTime = c.cwnd, now

		// Entering a new epoch: start from the window the loss left.
		if c.startTime == 0 {
			c.startTime = now
			c.ackCnt = acked
			c.tcpCwnd = c.cwnd
			if c.lastMaxCwnd <= c.cwnd {
				c.K, c.origin = 0, c.cwnd
			} else {
				c.K = uint32(math.Cbrt(ccCubeFactor * float64(c.lastMaxCwnd-c.cwnd)))
				c.origin = c.lastMaxCwnd
			}
		}

		// Time since the epoch, plus the minimum RTT.
		t := uint32(now - c.startTime + uint64(c.delayMin))
		var d uint64
		if t < c.K {
			d = uint64(c.K - t)
		} else {
			d = uint64(t - c.K)
		}
		delta := uint64(float64(ccC*d*d*d) / ccCScale)
		var target uint32
		if t < c.K {
			target = uint32(uint64(c.origin) - delta)
		} else {
			target = uint32(uint64(c.origin) + delta)
		}
		if target > c.cwnd {
			c.cnt = c.cwnd / (target - c.cwnd)
		} else {
			c.cnt = 100 * c.cwnd // very slowly
		}
	}

	// TCP friendly mode: never grow slower than Reno would.
	for delta := ccBetaScale * c.cwnd >> 3; c.ackCnt > delta; c.ackCnt -= delta {
		c.tcpCwnd++
	}
	if c.tcpCwnd > c.cwnd {
		if maxCnt := c.cwnd / (c.tcpCwnd - c.cwnd); c.cnt > maxCnt {
			c.cnt = maxCnt
		}
	}

	// One update per 2 acks at most.
	if c.cnt < 2 {
		c.cnt = 2
	}
}

// increaseCwnd adds the packets the CUBIC step allows for acked packets.
func (c *congestion) increaseCwnd(acked uint32) {
	if c.cwndCnt >= c.cnt {
		c.cwndCnt = 0
		c.cwnd++
	}
	c.cwndCnt += acked
	if c.cwndCnt >= c.cnt {
		delta := c.cwndCnt / c.cnt
		c.cwndCnt -= delta * c.cnt
		c.cwnd += delta
	}
	c.cwnd = min(c.cwnd, ccMaxCwnd)
}

// OnLoss reports a retransmission timeout at now. The window becomes ccRTOWindow packets and the
// slow start threshold is kept (see the file comment). The CUBIC epoch restarts. now is unused.
func (c *congestion) OnLoss(now time.Time) {
	if c.cwnd < c.lastMaxCwnd {
		c.lastMaxCwnd = c.cwnd * (ccBetaUnit + ccBeta) / (2 * ccBetaUnit)
	} else {
		c.lastMaxCwnd = c.cwnd
	}
	c.startTime = 0
	c.cwnd = ccRTOWindow
	c.cwndCnt = 0
}

// Window returns the send window in packets.
func (c *congestion) Window() int {
	return int(c.cwnd)
}

// pinWindow makes the send window a fixed number of packets with no congestion control. The tests
// use it for the fixed-window baseline.
func (st *Stream) pinWindow(pkts int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.pinned = pkts
}

// window is the send window in packets: the pinned one if set, else the congestion window.
func (st *Stream) window() int {
	if st.pinned > 0 {
		return st.pinned
	}
	return st.cc.Window()
}
