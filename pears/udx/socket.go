// Ported from libudx udx.c process_packet, Apache License 2.0, Copyright (c) 2021 Holepunch Inc.:
// a UDX packet is routed by its remote id, and a packet for an id nobody registered is dropped.
// The split with dht-rpc on the same connection is pears-go's own.
package udx

import (
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// queueLen is how many packets a stream or the dht-rpc side can hold before the socket drops.
const queueLen = 256

// maxDatagram is larger than any UDP datagram, so reads never truncate.
const maxDatagram = 65536

// Packet is a UDX packet delivered to the stream registered under its remote id. Addr is the address
// the datagram came from.
type Packet struct {
	Header  Header
	Payload []byte
	Addr    net.Addr
}

type rawPacket struct {
	data []byte
	addr net.Addr
}

// Socket owns one UDP connection. UDX packets go to the stream registered under their remote id,
// and every other datagram goes to the dht-rpc side returned by Raw.
type Socket struct {
	conn    *net.UDPConn
	raw     chan rawPacket
	rc      *rawConn
	mu      sync.Mutex
	streams map[uint32]chan Packet
	dropped atomic.Uint64
	done    chan struct{}
	once    sync.Once
}

// NewSocket takes ownership of conn and starts reading from it.
func NewSocket(conn *net.UDPConn) (*Socket, error) {
	s := &Socket{
		conn:    conn,
		raw:     make(chan rawPacket, queueLen),
		streams: map[uint32]chan Packet{},
		done:    make(chan struct{}),
	}
	s.rc = &rawConn{s: s, changed: make(chan struct{})}
	go s.readLoop()
	return s, nil
}

// Raw returns the dht-rpc side: every datagram that is not a UDX packet.
func (s *Socket) Raw() net.PacketConn {
	return s.rc
}

// Register routes the UDX packets whose remote id is id to the returned channel.
func (s *Socket) Register(id uint32) (<-chan Packet, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.done:
		return nil, net.ErrClosed
	default:
	}
	if _, ok := s.streams[id]; ok {
		return nil, fmt.Errorf("udx: stream id %d already registered", id)
	}
	ch := make(chan Packet, queueLen)
	s.streams[id] = ch
	return ch, nil
}

// Unregister stops routing packets for id, so the id can be registered again. Packets for it are
// dropped and counted from then on. The channel is not closed here: its reader stops reading it.
func (s *Socket) Unregister(id uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.streams, id)
}

// Dropped counts the packets the socket discarded: UDX packets for an unregistered stream id,
// undecodable UDX packets, and packets that found their queue full.
func (s *Socket) Dropped() uint64 {
	return s.dropped.Load()
}

// Close stops reading, closes the connection and closes every stream channel.
func (s *Socket) Close() error {
	var err error
	s.once.Do(func() {
		close(s.done)
		err = s.conn.Close()
		s.mu.Lock()
		defer s.mu.Unlock()
		for id, ch := range s.streams {
			close(ch)
			delete(s.streams, id)
		}
	})
	return err
}

func (s *Socket) readLoop() {
	buf := make([]byte, maxDatagram)
	for {
		n, addr, err := s.conn.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		b := append([]byte(nil), buf[:n]...)
		if !IsUDX(b) {
			s.deliverRaw(rawPacket{data: b, addr: addr})
			continue
		}
		h, payload, err := DecodeHeader(b)
		if err != nil {
			s.dropped.Add(1)
			continue
		}
		s.deliverStream(h, payload, addr)
	}
}

func (s *Socket) deliverRaw(p rawPacket) {
	select {
	case s.raw <- p:
	default:
		s.dropped.Add(1)
	}
}

func (s *Socket) deliverStream(h Header, payload []byte, addr net.Addr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, ok := s.streams[h.RemoteID]
	if !ok {
		s.dropped.Add(1)
		return
	}
	select {
	case ch <- Packet{Header: h, Payload: payload, Addr: addr}:
	default:
		s.dropped.Add(1)
	}
}

// rawConn is the net.PacketConn that dht-rpc reads and writes. Reads come from the socket's
// queue; writes go straight to the UDP connection.
type rawConn struct {
	s        *Socket
	mu       sync.Mutex
	deadline time.Time
	changed  chan struct{} // closed and replaced whenever the read deadline moves
}

func (r *rawConn) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		r.mu.Lock()
		deadline, changed := r.deadline, r.changed
		r.mu.Unlock()
		var expired <-chan time.Time
		if !deadline.IsZero() {
			expired = time.After(time.Until(deadline))
		}
		select {
		case pk := <-r.s.raw:
			return copy(p, pk.data), pk.addr, nil
		case <-expired:
			return 0, nil, os.ErrDeadlineExceeded
		case <-changed:
			// The deadline moved; wait again with the new one.
		case <-r.s.done:
			return 0, nil, net.ErrClosed
		}
	}
}

func (r *rawConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	return r.s.conn.WriteTo(p, addr)
}

func (r *rawConn) Close() error {
	return r.s.Close()
}

func (r *rawConn) LocalAddr() net.Addr {
	return r.s.conn.LocalAddr()
}

func (r *rawConn) SetDeadline(t time.Time) error {
	if err := r.SetReadDeadline(t); err != nil {
		return err
	}
	return r.SetWriteDeadline(t)
}

func (r *rawConn) SetReadDeadline(t time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deadline = t
	close(r.changed)
	r.changed = make(chan struct{})
	return nil
}

func (r *rawConn) SetWriteDeadline(t time.Time) error {
	return r.s.conn.SetWriteDeadline(t)
}
