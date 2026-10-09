// Package udx is a throwaway port of one libudx stream, enough to move bytes reliably in each
// direction over localhost. It covers the packet header, DATA and END flags, cumulative and SACK
// acknowledgements, retransmission on timeout and on SACK holes, and a fixed send window. It
// does not port the cubic congestion control, RACK/TLP, MTU probing, messages, relays or
// destroy handling.
//
// Upstream: holepunchto/libudx (Apache-2.0). Read from the libudx copy vendored in udx-native
// 1.12.0 (vendor/libudx/src/udx.c, internal.h, include/udx.h), not from udx-native 1.21.3, whose
// libudx is fetched at build time and is not in the npm tarball.
package udx

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"time"
)

// Header (include/udx.h: UDX_MAGIC_BYTE, UDX_VERSION, UDX_HEADER_*, UDX_HEADER_SIZE).
const (
	magic      = 255
	version    = 1
	HeaderSize = 20

	flagData    = 0b00001
	flagEnd     = 0b00010
	flagSack    = 0b00100
	flagMessage = 0b01000
	flagDestroy = 0b10000

	// UDX_MTU_BASE 1200 minus UDX_IPV4_HEADER_SIZE 48 (20 IP + 8 UDP + 20 UDX header).
	MaxPayload = 1200 - 48
	window     = 128
	ackEvery   = 2
	initRTO    = 300 * time.Millisecond
	maxRTO     = 5 * time.Second
)

// Header is the 20-byte UDX header: magic, version, type flags, data offset, then four
// little-endian uint32s: remote id (the recipient's stream id), receive window, seq, ack.
type Header struct {
	Type       byte
	DataOffset byte
	RemoteID   uint32
	RecvWin    uint32
	Seq        uint32
	Ack        uint32
}

// Parse splits a datagram into header and payload (process_packet in udx.c).
func Parse(b []byte) (Header, []byte, error) {
	if len(b) < HeaderSize {
		return Header{}, nil, errors.New("udx: short datagram")
	}
	if b[0] != magic || b[1] != version {
		return Header{}, nil, errors.New("udx: bad magic or version")
	}
	h := Header{
		Type:       b[2],
		DataOffset: b[3],
		RemoteID:   binary.LittleEndian.Uint32(b[4:]),
		RecvWin:    binary.LittleEndian.Uint32(b[8:]),
		Seq:        binary.LittleEndian.Uint32(b[12:]),
		Ack:        binary.LittleEndian.Uint32(b[16:]),
	}
	payload := b[HeaderSize:]
	return h, payload, nil
}

// Append writes a header for the given fields. Receive window is hard-coded to 0xffffffff, as
// init_stream_packet does.
func Append(dst []byte, typ byte, remoteID, seq, ack uint32) []byte {
	dst = append(dst, magic, version, typ, 0)
	dst = binary.LittleEndian.AppendUint32(dst, remoteID)
	dst = binary.LittleEndian.AppendUint32(dst, 0xffffffff)
	dst = binary.LittleEndian.AppendUint32(dst, seq)
	dst = binary.LittleEndian.AppendUint32(dst, ack)
	return dst
}

// Stream is one UDX stream on a UDP socket. A single direction is used at a time: Send or
// Receive. Each runs its own event loop, so state needs no locks.
type Stream struct {
	sock     *net.UDPConn
	peer     *net.UDPAddr
	localID  uint32
	remoteID uint32
	rx       chan []byte
	stop     chan struct{}
	// Counters for the report.
	Retransmits int
	SackCount   int
	Packets     int
}

// New makes a stream on a bound UDP socket. Packets from any other address are dropped.
func New(sock *net.UDPConn, peer netip.AddrPort, localID, remoteID uint32) *Stream {
	s := &Stream{
		sock:     sock,
		peer:     net.UDPAddrFromAddrPort(peer),
		localID:  localID,
		remoteID: remoteID,
		rx:       make(chan []byte, 4096),
		stop:     make(chan struct{}),
	}
	go s.reader()
	return s
}

func (s *Stream) reader() {
	buf := make([]byte, 65536)
	for {
		n, from, err := s.sock.ReadFromUDPAddrPort(buf)
		if err != nil {
			close(s.rx)
			return
		}
		if from.Addr().Unmap() != s.peer.AddrPort().Addr().Unmap() || from.Port() != s.peer.AddrPort().Port() {
			continue
		}
		cp := make([]byte, n)
		copy(cp, buf[:n])
		select {
		case s.rx <- cp:
		case <-s.stop:
			return
		}
	}
}

// Close stops the reader. The caller closes the socket.
func (s *Stream) Close() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
}

type sent struct {
	seq    uint32
	buf    []byte
	end    bool
	sacked bool
	at     time.Time
}

// Send writes everything from src as DATA packets, the last one flagged END, and returns when
// the peer has acknowledged all of it. It returns the number of payload bytes sent.
func (s *Stream) Send(src io.Reader, deadline time.Time) (int64, error) {
	var (
		nextSeq   uint32 // seq of the next new packet
		una       uint32 // lowest unacknowledged seq
		inflight  []*sent
		total     int64
		srcDone   bool
		endQueued bool
		lastFast  uint32 = ^uint32(0)
		rto              = initRTO
		timer            = time.NewTimer(rto)
	)
	defer timer.Stop()
	payloadBuf := make([]byte, MaxPayload)

	fill := func() error {
		for len(inflight) < window && !srcDone {
			n, err := io.ReadFull(src, payloadBuf)
			if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
				return err
			}
			if n == 0 && (err == io.EOF) {
				srcDone = true
				break
			}
			isEnd := err == io.EOF || err == io.ErrUnexpectedEOF
			if isEnd {
				srcDone = true
			}
			typ := byte(flagData)
			if isEnd {
				typ |= flagEnd
			}
			p := append([]byte(nil), payloadBuf[:n]...)
			if err := s.transmit(typ, nextSeq, p); err != nil {
				return err
			}
			inflight = append(inflight, &sent{seq: nextSeq, buf: p, end: isEnd, at: time.Now()})
			if isEnd {
				endQueued = true
			}
			total += int64(n)
			nextSeq++
		}
		if srcDone && !endQueued {
			// Source ended exactly on a packet boundary: send an empty END packet.
			typ := byte(flagEnd)
			if err := s.transmit(typ, nextSeq, nil); err != nil {
				return err
			}
			inflight = append(inflight, &sent{seq: nextSeq, end: true, at: time.Now()})
			endQueued = true
			nextSeq++
		}
		return nil
	}

	if err := fill(); err != nil {
		return total, err
	}
	for {
		if srcDone && una == nextSeq {
			return total, nil
		}
		if time.Now().After(deadline) {
			return total, fmt.Errorf("udx send: deadline hit with una=%d next=%d", una, nextSeq)
		}
		select {
		case d, ok := <-s.rx:
			if !ok {
				return total, errors.New("udx send: socket closed")
			}
			h, payload, err := Parse(d)
			if err != nil || h.RemoteID != s.localID {
				continue
			}
			progressed := false
			// Cumulative ack: everything below h.Ack is delivered.
			for una < h.Ack && una < nextSeq {
				una++
				progressed = true
			}
			// Drop acked packets from the head of inflight.
			for len(inflight) > 0 && inflight[0].seq < una {
				inflight = inflight[1:]
			}
			// SACK ranges: each is (start, end) little-endian, end exclusive.
			if h.Type&flagSack != 0 {
				for i := 0; i+8 <= len(payload); i += 8 {
					start := binary.LittleEndian.Uint32(payload[i:])
					end := binary.LittleEndian.Uint32(payload[i+4:])
					s.SackCount++
					for q := start; q != end && q < nextSeq; q++ {
						for _, p := range inflight {
							if p.seq == q {
								p.sacked = true
								break
							}
						}
					}
				}
			}
			if progressed {
				rto = initRTO
				timer.Reset(rto)
			}
			// Fast retransmit: the head is unacked, and a later packet is SACKed.
			if len(inflight) > 0 && inflight[0].seq == una && lastFast != una {
				head := inflight[0]
				if !head.sacked && hasSackedAbove(inflight) {
					lastFast = una
					s.Retransmits++
					if err := s.transmit(headerType(head), head.seq, head.buf); err != nil {
						return total, err
					}
					head.at = time.Now()
				}
			}
			if err := fill(); err != nil {
				return total, err
			}
		case <-timer.C:
			// RTO: resend every unsacked packet in flight, then back off.
			for _, p := range inflight {
				if p.sacked {
					continue
				}
				s.Retransmits++
				if err := s.transmit(headerType(p), p.seq, p.buf); err != nil {
					return total, err
				}
				p.at = time.Now()
			}
			rto *= 2
			if rto > maxRTO {
				rto = maxRTO
			}
			timer.Reset(rto)
		}
	}
}

func hasSackedAbove(inflight []*sent) bool {
	for _, p := range inflight {
		if p.sacked {
			return true
		}
	}
	return false
}

func headerType(p *sent) byte {
	if p.end {
		if len(p.buf) > 0 {
			return flagData | flagEnd
		}
		return flagEnd
	}
	return flagData
}

// transmit sends one stream packet: the header with our ack field (0, we receive nothing in
// this direction), then the payload.
func (s *Stream) transmit(typ byte, seq uint32, payload []byte) error {
	pkt := Append(make([]byte, 0, HeaderSize+len(payload)), typ, s.remoteID, seq, 0)
	pkt = append(pkt, payload...)
	_, err := s.sock.WriteToUDP(pkt, s.peer)
	s.Packets++
	return err
}

// Receive reads DATA packets and calls sink for each payload in order. It returns when the
// END packet and everything before it has been delivered and acknowledged.
func (s *Stream) Receive(sink func(p []byte), deadline time.Time) (int64, error) {
	var (
		rcvNxt  uint32
		total   int64
		ooo     = map[uint32]oooPkt{}
		endSeq  uint32
		endSeen bool
		unacked int
		timer   = time.NewTimer(time.Hour)
	)
	defer timer.Stop()
	for {
		if endSeen && rcvNxt > endSeq {
			if err := s.sendAck(rcvNxt, nil); err != nil {
				return total, err
			}
			return total, nil
		}
		if time.Now().After(deadline) {
			return total, fmt.Errorf("udx receive: deadline hit at seq %d", rcvNxt)
		}
		timer.Reset(time.Until(deadline))
		select {
		case d, ok := <-s.rx:
			if !ok {
				return total, errors.New("udx receive: socket closed")
			}
			h, payload, err := Parse(d)
			if err != nil || h.RemoteID != s.localID {
				continue
			}
			s.Packets++
			if h.Type&(flagData|flagEnd) == 0 {
				continue // pure ack from the peer, nothing to receive
			}
			// data_offset: skip MTU-probe padding before the data (process_packet).
			body := payload
			if h.DataOffset > 0 && int(h.DataOffset) < len(body) {
				body = body[h.DataOffset:]
			}
			isEnd := h.Type&flagEnd != 0
			isData := h.Type&flagData != 0
			switch {
			case h.Seq == rcvNxt:
				if isData {
					sink(body)
					total += int64(len(body))
				}
				rcvNxt++
				if isEnd {
					endSeen, endSeq = true, h.Seq
				}
				// Drain buffered out-of-order packets that are now in order.
				for {
					p, ok := ooo[rcvNxt]
					if !ok {
						break
					}
					delete(ooo, rcvNxt)
					if p.data {
						sink(p.buf)
						total += int64(len(p.buf))
					}
					if p.end {
						endSeen, endSeq = true, rcvNxt
					}
					rcvNxt++
				}
				unacked++
				if unacked >= ackEvery || endSeen {
					unacked = 0
					if err := s.sendAck(rcvNxt, nil); err != nil {
						return total, err
					}
				}
			case h.Seq > rcvNxt:
				if _, dup := ooo[h.Seq]; !dup {
					ooo[h.Seq] = oooPkt{data: isData, end: isEnd, buf: append([]byte(nil), body...)}
				}
				if err := s.sendAck(rcvNxt, sackRanges(ooo, rcvNxt)); err != nil {
					return total, err
				}
				unacked = 0
			default:
				// Duplicate of something already delivered: re-ack so the sender can move on.
				if err := s.sendAck(rcvNxt, nil); err != nil {
					return total, err
				}
			}
		case <-timer.C:
			return total, fmt.Errorf("udx receive: timed out waiting, rcvNxt=%d", rcvNxt)
		}
	}
}

type oooPkt struct {
	data bool
	end  bool
	buf  []byte
}

// sackRanges lists contiguous runs of buffered seqs above rcvNxt as (start, end) pairs.
func sackRanges(ooo map[uint32]oooPkt, rcvNxt uint32) []byte {
	var seqs []uint32
	for q := range ooo {
		seqs = append(seqs, q)
	}
	sortU32(seqs)
	var out []byte
	for i := 0; i < len(seqs); {
		start := seqs[i]
		end := start + 1
		j := i + 1
		for j < len(seqs) && seqs[j] == end {
			end++
			j++
		}
		out = binary.LittleEndian.AppendUint32(out, start)
		out = binary.LittleEndian.AppendUint32(out, end)
		i = j
	}
	return out
}

func sortU32(a []uint32) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

func (s *Stream) sendAck(ack uint32, sack []byte) error {
	typ := byte(0)
	if len(sack) > 0 {
		typ = flagSack
		s.SackCount++
	}
	// seq is our own next seq, which is 0 because nothing is sent in this direction.
	pkt := Append(make([]byte, 0, HeaderSize+len(sack)), typ, s.remoteID, 0, ack)
	pkt = append(pkt, sack...)
	_, err := s.sock.WriteToUDP(pkt, s.peer)
	return err
}
