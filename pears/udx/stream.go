// Ported from libudx udx.c (stream send, ack, SACK, RTO, end and destroy), Apache License 2.0,
// Copyright (c) 2021 Holepunch Inc. The C source is not in this tree: the behaviour follows the
// libudx copy the spike read (seq and ack rules, SACK pairs, END and DESTROY) and the values
// measured from udx-native 1.21.3 (initial RTO 1 s, sample cap 30 s, timer twice the RTO after an
// expiry, give-up at the 7th expiry).
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
	sendWindow     = 128              // most packets in flight
	recvWindow     = 4 << 20          // bytes a stream buffers before it advertises zero (0x00400000 on the wire)
	rtoInitial     = time.Second      // before any RTT sample, measured from udx-native 1.21.3
	rtoMin         = time.Second      // the floor after RTT samples, measured from udx-native 1.21.3 (HoleBridge-85m.4.10)
	rtoMax         = 30 * time.Second // cap on the timeout from samples, UDX_RTO_MAX_MS (HoleBridge-85m.4.14)
	rtoGranularity = time.Millisecond // RFC 6298 clock granularity G
	maxRTOExpiries = 7                // measured: the 7th expiry, 13 s after the first send, is ETIMEDOUT
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

	// Send side.
	nextSeq       uint32
	inflight      []*sentPkt // unacknowledged packets, in seq order
	inflightBytes uint32
	peerWnd       uint32 // the peer's last advertised receive window
	rto           *rtoCalc
	rtx           *time.Timer
	rtoGen        uint64 // a timer callback acts only if its generation is still current
	retries       int    // RTO expiries since the last cumulative ack
	cc            *congestion
	pinned        int // fixed window in packets, when > 0 (see pinWindow)

	// Receive side.
	rcvNxt   uint32
	ooo      map[uint32]rxPkt // received above rcvNxt, not yet in order
	oooBytes uint32
	rbuf     []byte // in order, not yet read
	eof      bool   // END received in order

	msgs chan []byte // unordered messages from the peer, see message.go
}

// sentPkt is a packet kept until the peer acks it, so it can be resent.
type sentPkt struct {
	seq     uint32
	typ     uint8
	payload []byte
	sentAt  time.Time
	retx    bool // resent at least once: its ack gives no RTT sample (Karn)
	fastN   int  // SACKed packets above it when it was last fast-retransmitted
	sacked  bool
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
		cc:      newCongestion(),
		ooo:     map[uint32]rxPkt{},
		msgs:    make(chan []byte, queueLen),
	}
	st.cond = sync.NewCond(&st.mu)
	ch, err := s.Register(localID)
	if err != nil {
		st.err = err
		close(st.msgs)
		return st
	}
	go st.run(ch)
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

// Write queues p for the peer, reliably and in order. It returns once every byte is sent, which
// may be before the peer acks it. It waits while the send window is full. Writes and CloseWrite
// run one at a time, so the bytes of one Write stay together.
func (st *Stream) Write(p []byte) (int, error) {
	st.writeMu.Lock()
	defer st.writeMu.Unlock()
	st.mu.Lock()
	defer st.mu.Unlock()
	n := 0
	for n < len(p) {
		for st.err == nil && !st.writeClosed && !st.canSend() {
			st.cond.Wait()
		}
		if st.err != nil {
			return n, st.err
		}
		if st.writeClosed {
			return n, io.ErrClosedPipe
		}
		k := min(mss, len(p)-n)
		if err := st.sendNew(FlagData, append([]byte(nil), p[n:n+k]...)); err != nil {
			return n, err
		}
		n += k
	}
	return n, nil
}

// CloseWrite sends END after the queued data, so the peer reads io.EOF after it. If a Write is in
// progress, CloseWrite waits for it to finish first, as udx-native end() waits for queued writes.
func (st *Stream) CloseWrite() error {
	st.writeMu.Lock()
	defer st.writeMu.Unlock()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.err != nil {
		return st.err
	}
	if st.writeClosed {
		return nil
	}
	st.writeClosed = true
	return st.sendNew(FlagEnd, nil)
}

// Destroy tears the stream down at once and sends DESTROY to the peer.
func (st *Stream) Destroy() error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.err != nil {
		return nil
	}
	st.send(FlagDestroy, st.nextSeq, nil) // best effort: if it is lost, the peer times out
	st.teardown(net.ErrClosed)
	return nil
}

// Close closes both directions at once, the same as Destroy.
func (st *Stream) Close() error {
	return st.Destroy()
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
			st.handle(pk)
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
		st.retries = 0
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

// canSend reports whether a new packet fits the send window. With nothing in flight one packet
// always may go, so a zero peer window cannot stall the stream for good.
func (st *Stream) canSend() bool {
	return len(st.inflight) < st.window() && (st.inflightBytes == 0 || st.inflightBytes+mss <= st.peerWnd)
}

// sendNew sends a packet that takes the next seq, and keeps it until the peer acks it.
func (st *Stream) sendNew(typ uint8, payload []byte) error {
	if st.peer == nil {
		return errNotConnected
	}
	p := &sentPkt{seq: st.nextSeq, typ: typ, payload: payload, sentAt: time.Now()}
	st.nextSeq++
	st.inflight = append(st.inflight, p)
	st.inflightBytes += uint32(len(payload))
	if st.rtx == nil {
		st.armRTO()
	}
	return st.send(typ, p.seq, payload)
}

// onAck applies the peer's cumulative ack and SACK blocks to the packets in flight
// (libudx process_sacks and the ack path of process_packet).
func (st *Stream) onAck(h Header, payload []byte) {
	if h.Ack <= st.nextSeq {
		var rtt time.Duration
		sampled, removed := false, false
		n := 0
		for len(st.inflight) > 0 && st.inflight[0].seq < h.Ack {
			p := st.inflight[0]
			st.inflight = st.inflight[1:]
			st.inflightBytes -= uint32(len(p.payload))
			if !p.retx {
				rtt, sampled = time.Since(p.sentAt), true
			}
			removed = true
			n++
		}
		if removed {
			st.retries = 0
			st.rto.Progress()
			if sampled {
				st.rto.sample(rtt)
			}
			if len(st.inflight) == 0 {
				st.stopRTO()
			} else {
				st.armRTO()
			}
			st.cc.OnAck(n, st.rto.srtt, time.Now())
		}
	}
	if h.Type&FlagSack != 0 {
		st.markSacked(payload)
	}
	st.fastRetransmit()
}

// markSacked marks the packets covered by SACK blocks. Each block is a (start, end) pair of
// little-endian uint32s, end exclusive.
func (st *Stream) markSacked(b []byte) {
	for ; len(b) >= 8; b = b[8:] {
		start, end := binary.LittleEndian.Uint32(b), binary.LittleEndian.Uint32(b[4:])
		for _, p := range st.inflight {
			if p.seq >= start && p.seq < end {
				p.sacked = true
			}
		}
	}
}

// fastRetransmit resends the oldest packet when three packets above it are SACKed, and again
// after three more, so a lost resend does not wait for the RTO.
func (st *Stream) fastRetransmit() {
	if len(st.inflight) < 2 {
		return
	}
	head := st.inflight[0]
	if head.sacked {
		return
	}
	n := 0
	for _, p := range st.inflight[1:] {
		if p.sacked {
			n++
		}
	}
	if n >= 3 && (!head.retx || n >= head.fastN+3) {
		head.retx, head.fastN = true, n
		st.send(head.typ, head.seq, head.payload)
	}
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
	st.send(typ, st.nextSeq, sack)
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

// send writes one packet to the peer, with our cumulative ack and window in the header.
func (st *Stream) send(typ uint8, seq uint32, payload []byte) error {
	pkt := EncodeHeader(Header{
		Type:       typ,
		RemoteID:   st.remoteID,
		RecvWindow: st.freeSpace(),
		Seq:        seq,
		Ack:        st.rcvNxt,
	}, payload)
	_, err := st.sock.conn.WriteToUDP(pkt, st.peer)
	return err
}

// armRTO (re)starts the retransmission timer at the current timeout.
func (st *Stream) armRTO() {
	st.stopRTO()
	gen := st.rtoGen
	st.rtx = time.AfterFunc(st.rto.Timeout(), func() { st.onRTO(gen) })
}

// stopRTO stops the retransmission timer. A callback that already started sees a new generation
// and does nothing.
func (st *Stream) stopRTO() {
	if st.rtx != nil {
		st.rtx.Stop()
		st.rtx = nil
	}
	st.rtoGen++
}

// onRTO handles an expiry: after maxRTOExpiries the stream fails with ETIMEDOUT. Otherwise the
// oldest unsacked packet is resent and the timeout backs off.
func (st *Stream) onRTO(gen uint64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if gen != st.rtoGen || st.err != nil {
		return
	}
	st.retries++
	if st.retries >= maxRTOExpiries {
		st.teardown(syscall.ETIMEDOUT)
		return
	}
	st.cc.OnLoss(time.Now())
	for _, p := range st.inflight {
		if !p.sacked {
			p.retx = true
			st.send(p.typ, p.seq, p.payload)
			break
		}
	}
	st.rto.Backoff()
	st.armRTO()
}

// teardown ends the stream with err. Pending and later calls return it, the timer stops, the socket
// stops routing to the stream, and the Messages channel is closed. The caller holds mu.
func (st *Stream) teardown(err error) {
	if st.err != nil {
		return
	}
	st.err = err
	st.stopRTO()
	close(st.done)
	close(st.msgs)
	st.sock.Unregister(st.localID)
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
