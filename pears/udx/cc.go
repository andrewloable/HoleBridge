// Congestion control glue for the UDX sender. The window and pacing rate come from BBR (bbr.go,
// udx_bbr.c in libudx ae8bff7, which udx-native 1.21.3 runs), driven by the rate samples of the
// ack path (rate.go). This file holds the window the sender reads and the constants it starts from.
// Apache License 2.0, Copyright (c) 2021 Holepunch Inc.
package udx

const ccInitCwnd = 10 // stream->cwnd at init (udx_stream_init)

// ccParams are the controller constants the tests check, named as libudx names them.
type ccParams struct {
	InitCwnd       uint32 // packets in the first window (stream->cwnd at init)
	InitPacingRate uint32 // bytes per ms until BBR sets the rate (UDX_INIT_PACING_RATE)
	RTTMinWindowMS uint32 // the min RTT filter window in ms (UDX_RTT_MIN_WINDOW_MS)
}

// upstreamCCParams returns the constants the controller uses.
func upstreamCCParams() ccParams {
	return ccParams{
		InitCwnd:       ccInitCwnd,
		InitPacingRate: initPacingRate,
		RTTMinWindowMS: rttMinWindowMS,
	}
}

// pinWindow makes the send window a fixed number of packets with no congestion control. The tests
// use it for the fixed-window baseline. Pacing still applies.
func (st *Stream) pinWindow(pkts int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.pinned = pkts
}

// window is the send window in packets: the pinned one if set, else the BBR congestion window.
func (st *Stream) window() int {
	if st.pinned > 0 {
		return st.pinned
	}
	return int(st.bf.cwnd)
}
