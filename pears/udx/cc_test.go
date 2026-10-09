package udx

import (
	"encoding/binary"
	"net"
	"testing"
	"time"
)

// Upstream values. The initial window and the pacing start are libudx ae8bff7's (udx_stream_init),
// and the recovery numbers were measured on udx-native 1.21.3, which is built from ae8bff7 and runs
// BBR. wantInitialWindow is the window of a new stream.
const (
	wantInitialWindow = 10
	wantInitialPacing = 25000 // bytes per ms until BBR sets the rate (UDX_INIT_PACING_RATE)
	wantMinRTTWindow  = 300000
	sendWindow        = 128 // packets in flight in the fixed-window baseline
)

// ccRTT is the RTT the synthetic acks carry in the BBR tests.
const ccRTT = 50 * time.Millisecond

// A new stream starts with the upstream window and token bucket, and no RTT sample yet.
func TestNewStreamStartsWithUpstreamWindowAndPacing(t *testing.T) {
	defer failOnPanic(t)
	sock, err := NewSocket(loopbackUDP(t))
	if err != nil {
		t.Fatalf("NewSocket: %v", err)
	}
	defer sock.Close()
	st := sock.NewStream(1001)
	defer st.Destroy()
	st.mu.Lock()
	defer st.mu.Unlock()
	if got := st.window(); got != wantInitialWindow {
		t.Fatalf("initial window = %d packets, want %d", got, wantInitialWindow)
	}
	if st.tbAvailable != wantInitialPacing {
		t.Fatalf("initial token bucket = %d bytes, want %d", st.tbAvailable, wantInitialPacing)
	}
	if got := upstreamCCParams(); got.InitCwnd != wantInitialWindow || got.InitPacingRate != wantInitialPacing || got.RTTMinWindowMS != wantMinRTTWindow {
		t.Fatalf("upstream constants = %+v", got)
	}
}

// libudx's app limit counts writes_queued_bytes, the bytes written and not yet acked (udx_rate.c 112),
// so a stream is app limited only while less than one payload is unacked and the window has room.
// Case 1: the three packets of a 3 * mss write are all unacked when they are sent, so the stream is not
// app limited and the limit keeps its initial value. The port used to count the bytes not yet in a
// packet, so the last packet saw an empty queue and set the limit to 2 (HoleBridge-85m.4.20).
// Case 2: one short packet is unacked, so the stream is app limited: delivered 0 plus inflight 0 gives
// 1, since 0 means not app limited.
func TestAppLimitedCountsUnackedBytesLikeUpstream(t *testing.T) {
	defer failOnPanic(t)
	silent := loopbackUDP(t)
	go func() { // a peer that reads and discards, and never acks
		buf := make([]byte, 65536)
		for {
			if _, _, err := silent.ReadFromUDP(buf); err != nil {
				return
			}
		}
	}()
	sock, err := NewSocket(loopbackUDP(t))
	if err != nil {
		t.Fatalf("NewSocket: %v", err)
	}
	defer sock.Close()
	to := silent.LocalAddr().(*net.UDPAddr)

	appLimitedAfter := func(id uint32, n int) uint32 {
		st := sock.NewStream(id)
		defer st.Destroy()
		if err := st.Connect(2002, to); err != nil {
			t.Fatalf("Connect: %v", err)
		}
		if _, err := st.Write(make([]byte, n)); err != nil {
			t.Fatalf("Write %d bytes: %v", n, err)
		}
		st.mu.Lock()
		defer st.mu.Unlock()
		return st.bf.rate.appLimited
	}

	if got := appLimitedAfter(1001, 3*mss); got != ^uint32(0) {
		t.Fatalf("app limit after 3 * mss written and unacked = %d, want %d (not app limited)", got, ^uint32(0))
	}
	if got := appLimitedAfter(1002, 100); got != 1 {
		t.Fatalf("app limit after 100 bytes written and unacked = %d, want 1 (app limited, nothing in flight)", got)
	}
}

// The scripted peer is the far end of a stream in the two recovery scenarios that HoleBridge-85m.4.12
// measured on udx-native 1.21.3. It acks each burst in full once the sender has been quiet for
// peerQuiet, so the sender grows its window through bursts of 10, 20, 40 and 80 packets.
//
// RTO scenario: the 80-packet burst is never acked. The RTO resends its head, and the peer then sends
// one full ack for everything. SACK scenario: the first packet of the 80-packet burst is dropped, and
// after the other 79 the peer sends one ack with the cumulative point at the loss and a SACK block for
// the 79 that arrived. The probe that produced these shapes ran against udx-native 1.21.3 outside the
// repo; with it the SACK scenario sent 160 DATA packets after the event, 159 new and the head resend,
// and the RTO scenario sent 82 DATA packets after the RTO, 81 new and the head resend.
const (
	peerQuiet  = 60 * time.Millisecond
	peerWindow = 0x00400000 // the receive window the peer advertises
	peerBurst  = 80         // the burst the RTO scenario stops acking
	peerDrop   = 40         // the burst after which the SACK scenario drops the next first packet
)

// recoveryScript names the two scenarios.
type recoveryScript int

const (
	scriptRTO recoveryScript = iota
	scriptSACK
)

// recoveryRun is what the scripted peer saw.
type recoveryRun struct {
	Bursts  []int // DATA packets received in each quiet period before the event
	NewSeqs int   // DATA packets after the event with seq past the last packet sent before it
	AllDATA int   // every DATA packet after the event, resends included
}

// peerAck sends a pure ack, or a SACK ack when ranges is set, from the peer to the stream.
func peerAck(conn *net.UDPConn, to *net.UDPAddr, localID, cum uint32, ranges [][2]uint32) {
	var typ uint8
	var payload []byte
	if len(ranges) > 0 {
		typ = FlagSack
		for _, r := range ranges {
			payload = binary.LittleEndian.AppendUint32(payload, r[0])
			payload = binary.LittleEndian.AppendUint32(payload, r[1])
		}
	}
	conn.WriteToUDP(EncodeHeader(Header{Type: typ, RemoteID: localID, RecvWindow: peerWindow, Ack: cum}, payload), to)
}

// runRecovery runs one scenario: a stream sends 4000 packets to the scripted peer and the peer
// reports what it received after the event.
func runRecovery(t *testing.T, script recoveryScript) recoveryRun {
	t.Helper()
	const localID, remoteID = 1001, 2002
	peerConn := loopbackUDP(t)
	sockConn := loopbackUDP(t)
	sock, err := NewSocket(sockConn)
	if err != nil {
		t.Fatalf("NewSocket: %v", err)
	}
	defer sock.Close()
	st := sock.NewStream(localID)
	defer st.Destroy()
	if err := st.Connect(remoteID, peerConn.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	go st.Write(make([]byte, mss*4000)) // returns when Destroy tears the stream down

	to := sockConn.LocalAddr().(*net.UDPAddr)
	got := map[uint32]bool{}
	var run recoveryRun
	var cum uint32 // what the peer has acked
	var maxSeq uint32
	var sawAny bool
	var burst int
	var silent, counting, dropped bool
	var dropSeq uint32
	var haveDrop bool
	var eventMax uint32 // last seq sent before the event
	var stopAt time.Time
	overall := time.Now().Add(30 * time.Second)
	buf := make([]byte, 65536)

	// contiguous returns the lowest seq the peer has not received, from cum.
	contiguous := func() uint32 {
		r := cum
		for got[r] {
			r++
		}
		return r
	}

	for {
		now := time.Now()
		if !stopAt.IsZero() && now.After(stopAt) {
			return run
		}
		if now.After(overall) {
			t.Fatalf("scenario did not finish; bursts %v", run.Bursts)
		}
		limit := overall
		if !stopAt.IsZero() && stopAt.Before(limit) {
			limit = stopAt
		}
		if sawAny && burst > 0 && !silent && !counting {
			if q := now.Add(peerQuiet); q.Before(limit) {
				limit = q
			}
		}
		peerConn.SetReadDeadline(limit)
		n, _, err := peerConn.ReadFromUDP(buf)
		if err != nil {
			// quiet: the sender has stopped sending for peerQuiet
			if !sawAny || burst == 0 || silent {
				continue
			}
			size := burst
			burst = 0
			run.Bursts = append(run.Bursts, size)
			if script == scriptRTO && size >= peerBurst {
				silent = true
				eventMax = maxSeq
				continue
			}
			if script == scriptSACK && dropped && !counting {
				counting = true
				eventMax = maxSeq
				var ranges [][2]uint32
				for s := cum; s <= maxSeq; s++ { // in order, so adjacent seqs merge
					if got[s] {
						ranges = appendRange(ranges, s)
					}
				}
				peerAck(peerConn, to, localID, cum, ranges)
				silent = true
				stopAt = time.Now().Add(800 * time.Millisecond)
				continue
			}
			cum = contiguous()
			if script == scriptSACK && size == peerDrop && !haveDrop {
				haveDrop = true
				dropSeq = cum
			}
			peerAck(peerConn, to, localID, cum, nil)
			continue
		}
		if n < 20 || buf[0] != 255 || buf[2]&FlagData == 0 {
			continue
		}
		seq := binary.LittleEndian.Uint32(buf[12:])
		sawAny = true
		if got[seq] {
			// a resend of a packet the peer already has: the RTO has fired
			if silent && script == scriptRTO && !counting {
				counting = true
				eventMax = maxSeq
				for s := uint32(0); s <= maxSeq; s++ {
					got[s] = true
				}
				cum = contiguous()
				peerAck(peerConn, to, localID, cum, nil)
				stopAt = time.Now().Add(500 * time.Millisecond)
			}
			if counting {
				run.AllDATA++
			}
			continue
		}
		if counting {
			run.AllDATA++
			if seq > eventMax {
				run.NewSeqs++
			}
		}
		if silent {
			continue
		}
		if script == scriptSACK && haveDrop && !dropped && seq == dropSeq {
			dropped = true
			continue
		}
		got[seq] = true
		if seq > maxSeq {
			maxSeq = seq
		}
		burst++
	}
}

// appendRange adds seq to the sorted SACK ranges, extending the last one when it is adjacent.
func appendRange(ranges [][2]uint32, seq uint32) [][2]uint32 {
	if n := len(ranges); n > 0 && ranges[n-1][1] == seq {
		ranges[n-1][1] = seq + 1
		return ranges
	}
	return append(ranges, [2]uint32{seq, seq + 1})
}

// After an RTO from a window of 80 packets, the full ack of those packets sends 82 DATA packets after
// the RTO, measured on udx-native 1.21.3 (HoleBridge-85m.4.12). The RTO marks all 80 lost, so the window
// becomes 0 + 1 and the head resend is the one packet in flight. The full ack of the 80 adds 80 to the
// window, so 81 new packets follow the resend: 82 DATA in all, of which 81 are new.
func TestRTOAfterFullAckSends82LikeUpstream(t *testing.T) {
	defer failOnPanic(t)
	run := runRecovery(t, scriptRTO)
	if !equalInts(run.Bursts[:4], []int{10, 20, 40, 80}) {
		t.Fatalf("burst sizes %v, want the window to grow 10, 20, 40, 80", run.Bursts)
	}
	if run.AllDATA != 82 {
		t.Fatalf("DATA packets after the RTO = %d (new %d), want 82 (the head resend and 81 new)", run.AllDATA, run.NewSeqs)
	}
	if run.NewSeqs != 81 {
		t.Fatalf("new packets after the full ack = %d, want 81 (window 1 + 80 acked)", run.NewSeqs)
	}
}

// After SACK loss covering 79 packets, the sender sends 159 new packets at once (HoleBridge-85m.4.12,
// measured on udx-native 1.21.3). The head resend is one more DATA packet after the event.
func TestSACKLossSends159LikeUpstream(t *testing.T) {
	defer failOnPanic(t)
	run := runRecovery(t, scriptSACK)
	if !equalInts(run.Bursts[:4], []int{10, 20, 40, 79}) {
		t.Fatalf("burst sizes %v, want 10, 20, 40, then the 79 that arrive of 80", run.Bursts)
	}
	if run.NewSeqs != 159 {
		t.Fatalf("new packets after the SACK loss = %d, want 159", run.NewSeqs)
	}
	if run.AllDATA != 160 {
		t.Fatalf("DATA packets after the SACK loss = %d, want 160 (159 new and the head resend)", run.AllDATA)
	}
}

// The controller's limits are the upstream constants. libudx has no pacing rule beyond the token
// bucket and the window.
func TestCongestionConstantsMatchUpstream(t *testing.T) {
	defer failOnPanic(t)
	got := upstreamCCParams()
	want := ccParams{
		InitCwnd:       wantInitialWindow,
		InitPacingRate: wantInitialPacing,
		RTTMinWindowMS: wantMinRTTWindow,
	}
	if got != want {
		t.Fatalf("upstream constants = %+v, want %+v", got, want)
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

// equalInts reports whether a and b hold the same values.
func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
