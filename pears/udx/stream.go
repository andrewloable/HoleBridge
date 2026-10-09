// Ported from libudx udx.c (stream state, write, END and DESTROY, the receive side, the timers and
// teardown), Apache License 2.0, Copyright (c) 2021 Holepunch Inc. The send and ack paths are in
// sender.go and the congestion glue in cc.go. The values follow libudx ae8bff7, which udx-native
// 1.21.3 is built from, and were measured on 1.21.3 (initial RTO 1 s, sample cap 30 s, timer twice
// the RTO after an expiry, give-up at the 7th expiry).
package udx

import (
	"encoding/binary"
	"errors"
	"io"
	"maps"
	"net"
	"slices"
	"sync"
	"syscall"
	"time"
)

// errNotConnected is returned when a stream sends before Connect.
var errNotConnected = errors.New("udx: stream not connected")

const (
	mss            = 1152             // payload per DATA packet: UDX_MTU_BASE 1200 minus UDX_IPV4_HEADER_SIZE 48
	recvWindow     = 4 << 20          // bytes a stream buffers before it advertises zero (0x00400000 on the wire)
	rtoInitial     = time.Second      // before any RTT sample, measured from udx-native 1.21.3
	rtoMin         = time.Second      // the floor after RTT samples, measured from udx-native 1.21.3 (HoleBridge-85m.4.10)
	rtoMax         = 30 * time.Second // cap on the timeout from samples, UDX_RTO_MAX_MS (HoleBridge-85m.4.14)
	rtoGranularity = time.Millisecond // RFC 6298 clock granularity G
	maxRTOExpiries = 7                // measured: the 7th expiry, 13 s after the first send, is ETIMEDOUT
)

// timerKind is the one timer a stream runs at a time (udx_stream_timer_type_t, the pending_timer).
type timerKind uint8

const (
	timerNone    timerKind = iota
	timerRTO               // retransmission timeout
	timerRACKReo           // RACK reordering timeout: a packet may still be lost once the window passes
	timerTLP               // tail loss probe: a new packet, or a resend of the last one, after ~2 RTTs
)

// Stream is one reliable, ordered UDX stream on a Socket. It is an io.ReadWriteCloser.
// CloseWrite ends the write side (END) and leaves the read side open. Destroy tears the stream
// down at once and tells the peer (DESTROY). Close is the io.Closer and does what Destroy does.
type Stream struct {
	sock     *Socket
	localID  uint32
	mu       sync.Mutex
	cond     *sync.Cond // signalled when data, window, or the error changes
	done     chan struct{}
	peer     *net.UDPAddr
	remoteID uint32
	err      error // set once when the stream is torn down; every call returns it after that
	// writeMu runs one Write or CloseWrite at a time, so CloseWrite's END follows the whole of a Write.
	writeMu sync.Mutex
	// writeClosed is set by CloseWrite; Write then fails.
	writeClosed bool
	// wq is the data from Write that is not yet in a packet. wqEnd is set by CloseWrite: END goes
	// out after wq is empty.
	wq    []byte
	wqEnd bool
	// writesQueued is the bytes written and not yet acked (writes_queued_bytes). Write adds to it and
	// ackPacket takes it back; the app limit reads it (HoleBridge-85m.4.20).
	writesQueued int

	// Send side. The fields follow udx_stream_t (seq, remote_acked, inflight_queue, ...).
	seq            uint32     // next seq to send (stream->seq)
	remoteAcked    uint32     // oldest seq not acked cumulatively (stream->remote_acked)
	outgoing       []*sentPkt // packets from remoteAcked on, at index seq-remoteAcked; nil once acked or sacked
	inflightQ      pktList    // packets in the network, in send order (inflight_queue)
	inflightBytes  uint32     // payload bytes in inflightQ, for the peer window rule
	rtxQ           pktList    // lost packets waiting to be resent (retransmit_queue)
	peerWnd        uint32     // the peer's last advertised receive window
	rto            *rtoCalc   // smoothed RTT, RTT variance and the timeout (srtt, rttvar, rto)
	pending        timerKind  // which timer is armed (pending_timer)
	rtx            *time.Timer
	timerGen       uint64            // a timer callback acts only if its generation is still current
	nextRTO        uint64            // ms deadline of the RTO (next_rto_ts)
	rtoCount       int               // RTO expiries since the last ack that advanced (rto_count)
	highSeq        uint32            // seq when the sender last entered loss or recovery (high_seq)
	lost           uint32            // packets marked lost (lost)
	sacks          uint32            // packets acked by SACK and not yet acked cumulatively (sacks)
	rackRTT        uint32            // RTT of the most recently sent packet acked, ms (rack_rtt)
	rackTimeSent   uint64            // send time of that packet (rack_time_sent)
	rackNextSeq    uint32            // its seq plus one (rack_next_seq)
	rackFack       uint32            // highest seq acked plus one (rack_fack)
	reorderingSeen bool              // a packet was acked out of order (reordering_seen)
	tlpInFlight    bool              // a tail loss probe is unacked (tlp_in_flight)
	tlpIsRetrans   bool              // that probe was a resend of the last packet (tlp_is_retrans)
	tlpPermitted   bool              // an RTT sample arrived since the last probe (tlp_permitted)
	tlpEndSeq      uint32            // seq of the probe (tlp_end_seq)
	rttMin         winFilter[uint32] // the min RTT over 300 s (rtt_min), ms
	bf             bbrFlow           // rate sample, BBR, window, pacing rate, smoothed RTT, ca_state
	tbAvailable    uint64            // pacing token bucket: bytes that may still be sent now (tb_available)
	tbLastRefill   uint64            // ms of the last refill (tb_last_refill_ms)
	paceTimer      *time.Timer       // refills the bucket 1 ms after it empties (refill_pacing_timer)
	pinned         int               // fixed window in packets, when > 0 (see pinWindow)
	loopMS         uint64            // the loop clock of the current event (see tick)

	// Receive side.
	rcvNxt   uint32
	ooo      map[uint32]rxPkt // received above rcvNxt, not yet in order
	oooBytes uint32
	rbuf     []byte // in order, not yet read
	eof      bool   // END received in order

	msgs chan []byte // unordered messages from the peer, see message.go

	// firewall is the hook for the packets that arrive while the stream is not connected (SetFirewall).
	firewall func(from *net.UDPAddr) bool

	// The route of the stream and a remote change (ChangeRemote). ch is the route the stream's socket delivers its
	// packets to. aliased lists the sockets that still route to ch for the packets of the old path. changed is open
	// while the packets sent before the change are unacked, and changeSeq is the seq the old path's last packet had.
	ch        *route
	aliased   []*Socket
	changed   chan struct{}
	changeSeq uint32
}

// rxPkt is a packet received above rcvNxt.
type rxPkt struct {
	typ     uint8
	payload []byte
}

// NewStream returns a stream with the given local id. It is not connected until Connect.
// If the id is already in use on the socket, every call on the stream returns that error.
func (s *Socket) NewStream(localID uint32) *Stream {
	st := &Stream{
		sock:    s,
		localID: localID,
		done:    make(chan struct{}),
		peerWnd: recvWindow,
		rto:     newRTOCalc(),
		ooo:     map[uint32]rxPkt{},
		msgs:    make(chan []byte, queueLen),
	}
	st.cond = sync.NewCond(&st.mu)
	// udx_stream_init: window 10, no app limit until a network limit is seen, the min RTT filter
	// reset to its maximum, and BBR initialised from them.
	now := nowMS()
	st.loopMS = now
	st.bf.cwnd = ccInitCwnd
	st.bf.caState = caOpen
	st.bf.rate.appLimited = ^uint32(0)
	st.bf.rate.rateAppLimited = true
	st.rttMin.reset(now, ^uint32(0))
	st.bf.initBBR(now, st.rttMin.get())
	st.tbAvailable = initPacingRate
	st.tbLastRefill = now
	r, err := s.registerRoute(localID)
	if err != nil {
		st.err = err
		close(st.msgs)
		return st
	}
	st.ch = r
	go st.run(r.ch)
	return st
}

// Connect sets the peer's stream id and address. Packets go out from the socket's UDP conn to addr.
func (st *Stream) Connect(remoteID uint32, addr *net.UDPAddr) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.err != nil {
		return st.err
	}
	st.remoteID, st.peer = remoteID, addr
	return nil
}

// Read returns the next bytes in order. After the peer's END and all its data, it returns io.EOF.
func (st *Stream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.tick()
	for len(st.rbuf) == 0 && !st.eof && st.err == nil {
		st.cond.Wait()
	}
	if st.err != nil {
		return 0, st.err
	}
	if len(st.rbuf) == 0 {
		return 0, io.EOF
	}
	before := st.freeSpace()
	n := copy(p, st.rbuf)
	st.rbuf = st.rbuf[n:]
	if before < mss && st.freeSpace() >= mss {
		st.sendAck() // tell the peer the window has opened again
	}
	return n, nil
}

// Write queues p for the peer, reliably and in order. It returns once every byte is in a packet,
// which may be before the peer acks it. The packets go out as the window and pacing allow, and
// acks send the rest. Writes and CloseWrite run one at a time, so the bytes of one Write stay together.
func (st *Stream) Write(p []byte) (int, error) {
	st.writeMu.Lock()
	defer st.writeMu.Unlock()
	st.mu.Lock()
	defer st.mu.Unlock()
	st.tick()
	if st.err != nil {
		return 0, st.err
	}
	if st.writeClosed {
		return 0, io.ErrClosedPipe
	}
	if st.peer == nil {
		return 0, errNotConnected
	}
	st.wq = append(st.wq, p...)
	st.writesQueued += len(p)
	st.sendPackets()
	st.cond.Broadcast()
	for len(st.wq) > 0 && st.err == nil {
		st.cond.Wait()
	}
	if st.err != nil {
		return len(p) - len(st.wq), st.err
	}
	return len(p), nil
}

// CloseWrite sends END after the queued data, so the peer reads io.EOF after it. If a Write is in
// progress, CloseWrite waits for it to finish first, as udx-native end() waits for queued writes.
func (st *Stream) CloseWrite() error {
	st.writeMu.Lock()
	defer st.writeMu.Unlock()
	st.mu.Lock()
	defer st.mu.Unlock()
	st.tick()
	if st.err != nil {
		return st.err
	}
	if st.writeClosed {
		return nil
	}
	st.writeClosed = true
	if st.peer == nil {
		return errNotConnected
	}
	st.wqEnd = true
	st.sendPackets()
	st.cond.Broadcast()
	for st.wqEnd && st.err == nil {
		st.cond.Wait()
	}
	return st.err
}

// Destroy tears the stream down at once and sends DESTROY to the peer.
func (st *Stream) Destroy() error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.err != nil {
		return nil
	}
	st.send(FlagDestroy, st.seq, nil) // best effort: if it is lost, the peer times out
	st.teardown(net.ErrClosed)
	return nil
}

// Close closes both directions at once, the same as Destroy.
func (st *Stream) Close() error {
	return st.Destroy()
}

// Done returns a channel that is closed when the stream is torn down, such as by Destroy or Close, the peer's
// DESTROY, or a timeout. It is closed once, by teardown, and never replaced, so a reader can wait on it while it
// has other work to do.
func (st *Stream) Done() <-chan struct{} {
	return st.done
}

// run takes the packets the socket routes to this stream until the stream is torn down.
func (st *Stream) run(ch <-chan Packet) {
	for {
		select {
		case pk, ok := <-ch:
			if !ok {
				st.mu.Lock()
				st.teardown(net.ErrClosed)
				st.mu.Unlock()
				return
			}
			if st.admit(pk) {
				st.handle(pk)
			}
		case <-st.done:
			return
		}
	}
}

// handle processes one packet from the peer: DESTROY, then its ack fields, then its data or message.
// A MESSAGE packet never reaches onData.
func (st *Stream) handle(pk Packet) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.tick()
	if st.err != nil {
		return
	}
	h := pk.Header
	if h.Type&FlagDestroy != 0 {
		st.teardown(syscall.ECONNRESET)
		return
	}
	st.peerWnd = h.RecvWindow
	if h.RecvWindow < mss {
		// A closed window is persist, not loss: the peer still answers each probe, so the expiry
		// count restarts. A peer that stops answering still gives up (HoleBridge-85m.4.11).
		st.rtoCount = 0
	}
	st.onAck(h, pk.Payload)
	switch {
	case h.Type&FlagMessage != 0:
		st.onMessage(pk.Payload)
	case h.Type&(FlagData|FlagEnd) != 0:
		st.onData(h.Seq, h.Type, pk.Payload)
	}
	st.cond.Broadcast()
}

// onData takes a packet that uses a seq: DATA, END, or both. Every such packet is acked, even a
// duplicate, so the sender can move on. A DATA packet that does not fit the receive window is
// dropped; the ack that follows tells the sender the window (libudx process_data_packet).
func (st *Stream) onData(seq uint32, typ uint8, payload []byte) {
	switch {
	case seq == st.rcvNxt:
		if typ&FlagData != 0 && uint32(len(payload)) > st.freeSpace() {
			break
		}
		st.deliver(typ, payload)
		for {
			r, ok := st.ooo[st.rcvNxt]
			if !ok {
				break
			}
			delete(st.ooo, st.rcvNxt)
			st.oooBytes -= uint32(len(r.payload))
			st.deliver(r.typ, r.payload)
		}
	case seq > st.rcvNxt:
		if _, dup := st.ooo[seq]; !dup && (typ&FlagData == 0 || uint32(len(payload)) <= st.freeSpace()) {
			st.ooo[seq] = rxPkt{typ: typ, payload: payload}
			st.oooBytes += uint32(len(payload))
		}
	}
	st.sendAck()
}

// deliver takes the packet at rcvNxt in order.
func (st *Stream) deliver(typ uint8, payload []byte) {
	if typ&FlagData != 0 {
		st.rbuf = append(st.rbuf, payload...)
	}
	if typ&FlagEnd != 0 {
		st.eof = true
	}
	st.rcvNxt++
}

// sendAck sends a pure ack: the cumulative ack and our window. Packets held above rcvNxt go out
// as SACK blocks. A pure ack takes no seq.
func (st *Stream) sendAck() {
	var typ uint8
	var sack []byte
	if len(st.ooo) > 0 {
		typ, sack = FlagSack, st.sackRanges()
	}
	st.send(typ, st.seq, sack)
}

// sackRanges lists the runs of held seqs above rcvNxt as (start, end) pairs, as many as fit in
// one packet.
func (st *Stream) sackRanges() []byte {
	var out []byte
	seqs := slices.Sorted(maps.Keys(st.ooo))
	for i := 0; i < len(seqs) && len(out)+8 <= mss; {
		start, end := seqs[i], seqs[i]+1
		for i++; i < len(seqs) && seqs[i] == end; i++ {
			end++
		}
		out = binary.LittleEndian.AppendUint32(out, start)
		out = binary.LittleEndian.AppendUint32(out, end)
	}
	return out
}

// freeSpace is the receive window we advertise: buffered bytes are subtracted from recvWindow.
func (st *Stream) freeSpace() uint32 {
	used := uint32(len(st.rbuf)) + st.oooBytes
	if used >= recvWindow {
		return 0
	}
	return recvWindow - used
}

// send writes one packet to the peer, with our cumulative ack and window in the header. It goes to the current remote.
func (st *Stream) send(typ uint8, seq uint32, payload []byte) error {
	return st.sendTo(st.peer, st.remoteID, typ, seq, payload)
}

// sendTo writes one packet to addr, for the stream id remoteID there, from the stream's current socket. A data packet
// is sent to the remote it was first sent to, so a resend after a change still goes where the packet first went.
func (st *Stream) sendTo(addr *net.UDPAddr, remoteID uint32, typ uint8, seq uint32, payload []byte) error {
	pkt := EncodeHeader(Header{
		Type:       typ,
		RemoteID:   remoteID,
		RecvWindow: st.freeSpace(),
		Seq:        seq,
		Ack:        st.rcvNxt,
	}, payload)
	_, err := st.sock.conn.WriteToUDP(pkt, addr)
	return err
}

// startTimer arms the one timer with kind after d. Any timer already armed is replaced.
func (st *Stream) startTimer(kind timerKind, d time.Duration) {
	st.stopTimer()
	st.pending = kind
	if kind == timerRTO {
		st.nextRTO = st.loopMS + uint64(d/time.Millisecond)
	}
	gen := st.timerGen
	st.rtx = time.AfterFunc(d, func() { st.onTimer(gen) })
}

// stopTimer stops the timer. A callback that already started sees a new generation and does nothing.
func (st *Stream) stopTimer() {
	if st.rtx != nil {
		st.rtx.Stop()
		st.rtx = nil
	}
	st.timerGen++
	st.pending = timerNone
}

// onTimer runs the timer that fired, if it is still the armed one.
func (st *Stream) onTimer(gen uint64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.tick()
	if gen != st.timerGen || st.err != nil {
		return
	}
	switch st.pending {
	case timerRTO:
		st.onRTO()
	case timerRACKReo:
		st.onRACKReo()
	case timerTLP:
		st.onTLP()
	}
	st.cond.Broadcast()
}

// teardown ends the stream with err. Pending and later calls return it, the timers stop, the socket
// stops routing to the stream, and the Messages channel is closed. A change still waiting for its old
// path is done, so nothing waits on it. The caller holds mu.
func (st *Stream) teardown(err error) {
	if st.err != nil {
		return
	}
	st.err = err
	st.stopTimer()
	if st.paceTimer != nil {
		st.paceTimer.Stop()
		st.paceTimer = nil
	}
	close(st.done)
	close(st.msgs)
	st.sock.Unregister(st.localID)
	st.releaseOldPath()
	st.cond.Broadcast()
}

// rtoCalc is the retransmission timeout of one stream: RFC 6298 with the upstream minimum and
// maximum. rto is the base timeout from the samples. After a retransmission timer fires, the next
// timer is twice rto and stays there (udx-native's udx_rto_timeout restarts it at rto * 2), until an
// ack advances the window and the base timeout is armed again.
type rtoCalc struct {
	srtt, rttvar time.Duration
	sampled      bool
	rto          time.Duration
	backedOff    bool // a timer fired since the last ack that advanced the window
}

// newRTOCalc returns a calculator at the upstream initial RTO.
func newRTOCalc() *rtoCalc {
	return &rtoCalc{rto: rtoInitial}
}

// Timeout returns the current retransmission timeout.
func (c *rtoCalc) Timeout() time.Duration {
	if c.backedOff {
		return 2 * c.rto
	}
	return c.rto
}

// base returns the base timeout, without the backoff.
func (c *rtoCalc) base() time.Duration {
	return c.rto
}

// srttMS is the smoothed RTT in whole ms, 0 before the first sample.
func (c *rtoCalc) srttMS() uint32 {
	return uint32(c.srtt / time.Millisecond)
}

// rttvarMS is the RTT variance in whole ms.
func (c *rtoCalc) rttvarMS() uint32 {
	return uint32(c.rttvar / time.Millisecond)
}

// Backoff is called when a retransmission timer fires: the next timer is twice the base timeout.
func (c *rtoCalc) Backoff() {
	c.backedOff = true
}

// Progress is called when an ack advances the window: the timer goes back to the base timeout.
func (c *rtoCalc) Progress() {
	c.backedOff = false
}

// sample takes one RTT measurement (RFC 6298 sections 2.2, 2.3 and 2.4) and recomputes the timeout.
func (c *rtoCalc) sample(rtt time.Duration) {
	if !c.sampled {
		c.srtt, c.rttvar, c.sampled = rtt, rtt/2, true
	} else {
		d := c.srtt - rtt
		if d < 0 {
			d = -d
		}
		c.rttvar = (3*c.rttvar + d) / 4
		c.srtt = (7*c.srtt + rtt) / 8
	}
	c.rto = min(max(c.srtt+max(rtoGranularity, 4*c.rttvar), rtoMin), rtoMax)
}
