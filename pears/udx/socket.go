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

// queueLen is how many packets the dht-rpc side and a stream's message queue can hold before the
// socket drops. A stream's packet queue is streamQueueLen: the window grows to hundreds of packets and
// sends them in bursts of 100 or more per ms, which overflows 256 slots on loopback (HoleBridge-85m.4.19).
const (
	queueLen       = 256
	streamQueueLen = 1024
)

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

// route is the channel a stream receives its packets on. A stream that moved to another socket (ChangeRemote) has its
// route registered on both sockets: as the primary route of the new one, and as an alias of the old one. Whichever
// socket routes to it, a packet is sent only while the route is open, so no socket sends on a closed channel. Only the
// socket that holds the route as primary closes it, in Close.
type route struct {
	mu     sync.RWMutex
	ch     chan Packet
	closed bool
}

// deliver queues p on the route's channel without blocking. It reports false when the route is closed or its queue is
// full, and the packet is dropped.
func (r *route) deliver(p Packet) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return false
	}
	select {
	case r.ch <- p:
		return true
	default:
		return false
	}
}

// close closes the route's channel, once. Packets sent to the route after this are refused.
func (r *route) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.closed = true
		close(r.ch)
	}
}

// isClosed reports whether close has run.
func (r *route) isClosed() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.closed
}

// Socket owns one UDP connection. UDX packets go to the stream registered under their remote id,
// and every other datagram goes to the dht-rpc side returned by Raw.
type Socket struct {
	conn    *net.UDPConn
	raw     chan rawPacket
	rc      *rawConn
	mu      sync.Mutex
	streams map[uint32]*route
	// aliases routes the packets of a stream that moved to another socket (ChangeRemote) and still has
	// packets in flight on the old path. A stream's route is closed only through streams.
	aliases map[uint32]*route
	dropped atomic.Uint64
	done    chan struct{}
	once    sync.Once
}

// NewSocket takes ownership of conn and starts reading from it.
func NewSocket(conn *net.UDPConn) (*Socket, error) {
	s := &Socket{
		conn:    conn,
		raw:     make(chan rawPacket, queueLen),
		streams: map[uint32]*route{},
		aliases: map[uint32]*route{},
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
	r, err := s.registerRoute(id)
	if err != nil {
		return nil, err
	}
	return r.ch, nil
}

// registerRoute is Register with the route itself, which a stream keeps so it can move to another socket.
func (s *Socket) registerRoute(id uint32) (*route, error) {
	r := &route{ch: make(chan Packet, streamQueueLen)}
	if err := s.attach(id, r); err != nil {
		return nil, err
	}
	return r, nil
}

// attach routes the packets for id to r, the route of a stream that moves here from another socket or is new. A closed
// route is refused: its stream is already gone.
func (s *Socket) attach(id uint32, r *route) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.isClosed() || r.isClosed() {
		return net.ErrClosed
	}
	if _, ok := s.streams[id]; ok {
		return fmt.Errorf("udx: stream id %d already registered", id)
	}
	s.streams[id] = r
	if s.aliases[id] == r {
		delete(s.aliases, id)
	}
	return nil
}

// handOver moves the route of id to r from primary to alias: the stream has moved to another socket, but this one
// keeps routing its packets, as libudx routes a stream by its id, since packets of the old path may still arrive here.
func (s *Socket) handOver(id uint32, r *route) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.streams[id] == r {
		delete(s.streams, id)
	}
	s.aliases[id] = r
}

// unalias stops the alias of id to r, if that is the alias there.
func (s *Socket) unalias(id uint32, r *route) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.aliases[id] == r {
		delete(s.aliases, id)
	}
}

// isClosed reports whether Close has run.
func (s *Socket) isClosed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
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

// Close stops reading, closes the connection and closes the route of every stream it holds as primary. Aliases are not
// closed: a stream that moved away is still open on its new socket, and other sockets may still route to it.
func (s *Socket) Close() error {
	var err error
	s.once.Do(func() {
		close(s.done)
		err = s.conn.Close()
		s.mu.Lock()
		defer s.mu.Unlock()
		for id, r := range s.streams {
			r.close()
			delete(s.streams, id)
		}
		clear(s.aliases)
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
	r, ok := s.streams[h.RemoteID]
	if !ok {
		r, ok = s.aliases[h.RemoteID]
	}
	if !ok {
		s.dropped.Add(1)
		return
	}
	if !r.deliver(Packet{Header: h, Payload: payload, Addr: addr}) {
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
