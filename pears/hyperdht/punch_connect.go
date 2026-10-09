// The punch sockets of a DHT node, and the NAT samples its pings give, for the connect and server hole punching
// (upstream hyperdht 6.34.1 lib/connect.js holepunch, probeRound and roundPunch, lib/server.js setupHolepuncher and
// _onpeerholepunch, lib/nat.js autoSample, lib/holepuncher.js openBirthdaySockets and lib/socket-pool.js). A punch
// handle is one of two kinds. The first is the DHT's own UDP socket, shared by the node's handles: the handle sends
// the holepunch datagrams from that socket, and the punch hub gives it every holepunch datagram that arrives on the
// node's socket while it is live. The second is a birthday socket: a UDP socket of its own, bound to port 0 on the
// node's host, with its own NAT mapping, so the birthday punch of a randomizing NAT gets one mapping per socket. A
// birthday socket gets only the one-byte datagrams sent to its own port, and its UDX side carries the stream of a
// connection that a punch connects on it. A holepuncher ignores the datagrams from addresses its peer did not name.
package hyperdht

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"

	"github.com/andrewloable/HoleBridge/pears/dhtrpc"
	"github.com/andrewloable/HoleBridge/pears/udx"
)

// punchObservers is how many table nodes a puncher takes its NAT samples from. Upstream's nat sampler decides
// from three agreeing samples, and one sample that does not answer is allowed for.
const punchObservers = 4

// dhtPunchSocket is a punch handle: the DHT's own socket (conn nil), or a birthday socket (conn set).
type dhtPunchSocket struct {
	d       *DHT
	conn    *net.UDPConn // the birthday socket's conn; nil for the node's own socket
	ud      *udx.Socket  // the birthday socket's UDX side, whose raw reader gives it its one-byte datagrams
	mu      sync.Mutex   // birthday handles: guards handler, closed and kept, and serializes the TTL of their sends
	handler func(from *net.UDPAddr)
	closed  bool // a birthday socket that is closed
	kept    bool // a birthday socket a stream runs on: Release leaves it open, the stream closes it
}

var _ punchSocket = (*dhtPunchSocket)(nil)

// punchHub is the set of live node handles of a DHT. The DHT's socket hands each holepunch datagram to all of them.
type punchHub struct {
	mu   sync.Mutex
	live map[*dhtPunchSocket]struct{}
}

// deliver calls the handler of every live node handle with the address the datagram came from. The handlers run
// without the hub's lock, on the read loop of the DHT's socket, so they must not block.
func (h *punchHub) deliver(from *net.UDPAddr) {
	h.mu.Lock()
	handlers := make([]func(*net.UDPAddr), 0, len(h.live))
	for s := range h.live {
		handlers = append(handlers, s.handler)
	}
	h.mu.Unlock()
	for _, f := range handlers {
		if f != nil {
			f(from)
		}
	}
}

// add makes s live: from now on it gets the holepunch datagrams that arrive on the DHT's socket.
func (h *punchHub) add(s *dhtPunchSocket) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.live == nil {
		h.live = make(map[*dhtPunchSocket]struct{})
	}
	h.live[s] = struct{}{}
}

// remove makes s not live. Its handler gets no more datagrams.
func (h *punchHub) remove(s *dhtPunchSocket) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.live, s)
}

// Local returns the address of the socket, as its peers reach it.
func (s *dhtPunchSocket) Local() *net.UDPAddr {
	if s.conn != nil {
		return s.conn.LocalAddr().(*net.UDPAddr)
	}
	a, err := s.d.addr()
	if err != nil {
		return nil
	}
	return a
}

// SendPunch sends the one-byte holepunch datagram from the socket to to, with the TTL ttl.
func (s *dhtPunchSocket) SendPunch(to *net.UDPAddr, ttl int) error {
	if s.conn == nil {
		return s.d.node.SendPunch(to, ttl)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return net.ErrClosed
	}
	return dhtrpc.SendPunchFrom(s.conn, to, ttl)
}

// OnPunch sets the handler of the holepunch datagrams that arrive on the socket, and makes a node handle live.
func (s *dhtPunchSocket) OnPunch(handler func(from *net.UDPAddr)) {
	if s.conn != nil {
		s.mu.Lock()
		s.handler = handler
		s.mu.Unlock()
		return
	}
	s.d.punch.mu.Lock()
	s.handler = handler
	s.d.punch.mu.Unlock()
	s.d.punch.add(s)
}

// newBirthdaySocket opens a birthday socket: a UDP socket on the node's host, bound to port 0, with a UDX side
// that reads its datagrams. Its raw reader starts here.
func (d *DHT) newBirthdaySocket() (*dhtPunchSocket, error) {
	a, err := d.addr()
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: a.IP, Port: 0})
	if err != nil {
		return nil, err
	}
	ud, err := udx.NewSocket(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	s := &dhtPunchSocket{d: d, conn: conn, ud: ud}
	go s.readPunches()
	return s, nil
}

// readPunches gives each one-byte datagram of the birthday socket to its handler, and no other. The UDX side reads
// the UDX packets, and the rest reach this reader. It returns when the socket closes.
func (s *dhtPunchSocket) readPunches() {
	buf := make([]byte, 64)
	for {
		n, from, err := s.ud.Raw().ReadFrom(buf)
		if err != nil {
			return
		}
		addr, ok := from.(*net.UDPAddr)
		if !ok || n >= 2 {
			continue // a holepunch datagram is one byte; a longer one is not for the punch
		}
		s.mu.Lock()
		h := s.handler
		s.mu.Unlock()
		if h != nil {
			h(addr)
		}
	}
}

// keep marks a birthday socket as the one a stream runs on, so that Release leaves it open.
func (s *dhtPunchSocket) keep() {
	if s.conn == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kept = true
}

// closeSocket closes a birthday socket, with its UDX side. A node handle has nothing to close.
func (s *dhtPunchSocket) closeSocket() {
	if s.conn == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	s.ud.Close()
}

// dhtPunchPool hands out the punch handles of a DHT. Acquire returns a handle on the node's own socket, which every
// node handle shares. AcquireBirthday returns a birthday socket of its own. Release makes a node handle not live, and
// closes a birthday socket unless a stream runs on it.
type dhtPunchPool struct {
	d *DHT
}

// Acquire returns a handle on the node's socket.
func (p dhtPunchPool) Acquire() punchSocket {
	return &dhtPunchSocket{d: p.d}
}

// AcquireBirthday returns a birthday socket of its own, for the birthday punch of a randomizing NAT.
func (p dhtPunchPool) AcquireBirthday() (punchSocket, error) {
	s, err := p.d.newBirthdaySocket()
	if err != nil {
		return nil, err
	}
	return s, nil
}

// Release gives a handle back: a node handle becomes not live, and a birthday socket is closed unless a stream runs on it.
func (p dhtPunchPool) Release(s punchSocket) {
	h, ok := s.(*dhtPunchSocket)
	if !ok {
		return
	}
	if h.conn == nil {
		p.d.punch.remove(h)
		return
	}
	h.mu.Lock()
	kept := h.kept
	h.mu.Unlock()
	if !kept {
		h.closeSocket()
	}
}

// punchSocket returns a handle on the node's socket. It gets the holepunch datagrams once OnPunch sets a handler.
func (d *DHT) punchSocket() punchSocket {
	return &dhtPunchSocket{d: d}
}

// punchPool returns the pool of d's punch handles.
func (d *DHT) punchPool() punchPool {
	return dhtPunchPool{d: d}
}

// sampleNATFromPings sends a PING from d's socket to each observer, and feeds each answer to p.observe with the
// address the observer reports for the socket and the observer's address. It returns how many answers came in,
// and an error when the pings fail for a reason other than a missing answer.
func (d *DHT) sampleNATFromPings(ctx context.Context, p *holepuncher, observers []*net.UDPAddr) (int, error) {
	var (
		mu       sync.Mutex
		answers  int
		firstErr error
		wg       sync.WaitGroup
	)
	for _, o := range observers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			seen, err := d.node.Observed(ctx, o)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				answers++
				p.observe(Address{Host: seen.Host, Port: seen.Port}, addressOf(o))
			case errors.Is(err, os.ErrDeadlineExceeded):
				// no answer: it is not a sample
			case firstErr == nil:
				firstErr = err
			}
		}()
	}
	wg.Wait()
	return answers, firstErr
}

// observers returns the table nodes that a puncher samples its NAT from: the nodes nearest the DHT's own key, up to
// punchObservers of them.
func (d *DHT) observers() []*net.UDPAddr {
	nodes := d.node.Closest(d.keyPair.Public[:])
	if len(nodes) > punchObservers {
		nodes = nodes[:punchObservers]
	}
	out := make([]*net.UDPAddr, 0, len(nodes))
	for _, a := range nodes {
		out = append(out, &net.UDPAddr{IP: net.IP(a.Host.AsSlice()), Port: int(a.Port)})
	}
	return out
}
