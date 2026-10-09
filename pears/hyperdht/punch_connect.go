// The punch sockets of a DHT node, and the NAT samples its pings give, for the connect and server hole punching
// (upstream hyperdht 6.34.1 lib/connect.js holepunch, probeRound and roundPunch, lib/server.js setupHolepuncher and
// _onpeerholepunch, lib/nat.js autoSample, lib/holepuncher.js openBirthdaySockets and lib/socket-pool.js). A punch
// handle is one of two kinds. The first is the DHT's own UDP socket, shared by the node's handles: the handle sends
// the holepunch datagrams from that socket, and the punch hub gives it every holepunch datagram that arrives on the
// node's socket while it is live. The second is a birthday socket: a UDP socket of its own, bound to port 0 on the
// node's host, with its own NAT mapping, so the birthday punch of a randomizing NAT gets one mapping per socket. A
// birthday socket gets only the datagrams sent to its own port: the one-byte holepunch datagrams go to its handler, and
// the dht-rpc replies to its own requests go to its IO (dhtrpc.NewFedIO), so a request and a NAT sample made from it
// name its own mapping. Its UDX side carries the stream of a connection that a punch connects on it. A holepuncher
// ignores the datagrams from addresses its peer did not name.
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
	conn    *net.UDPConn   // the birthday socket's conn; nil for the node's own socket
	ud      *udx.Socket    // the birthday socket's UDX side, whose raw reader gives it its one-byte datagrams
	raw     net.PacketConn // the reader of the one-byte datagrams: ud.Raw(), replaced by a fake in the tests
	io      *dhtrpc.IO     // the birthday socket's dht-rpc side: its requests and the replies to them (nil for the node's socket)
	mu      sync.Mutex     // birthday handles: guards handler, closed and kept, and serializes the TTL of their sends
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
		return reachableAddr(s.conn.LocalAddr().(*net.UDPAddr))
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

// Request sends a dht-rpc request from the socket to to, and returns the reply that comes back to it. The node's
// handle sends it from the node's socket, as the node's requests do; a birthday socket sends it from its own.
func (s *dhtPunchSocket) Request(ctx context.Context, to *net.UDPAddr, req dhtrpc.Request) (*dhtrpc.Response, error) {
	if s.conn == nil {
		return s.d.node.Request(ctx, to, req)
	}
	return s.io.Request(ctx, to, req)
}

// Observe pings to from the socket, and returns the address that to reports for the socket: one NAT sample. From the
// node's handle that is the node's address; from a birthday socket it is the socket's own, behind its own mapping.
func (s *dhtPunchSocket) Observe(ctx context.Context, to *net.UDPAddr) (Address, error) {
	var seen dhtrpc.Addr
	var err error
	if s.conn == nil {
		seen, err = s.d.node.Observed(ctx, to)
	} else {
		seen, err = s.io.Observed(ctx, to)
	}
	if err != nil {
		return Address{}, err
	}
	return Address{Host: seen.Host, Port: seen.Port}, nil
}

// reachableAddr returns the address a local peer reaches a socket bound to a at: a socket on all interfaces is reached at
// 127.0.0.1 on its port, as dhtrpc's Node.Addr reports a node (macOS refuses to send to 0.0.0.0). Any other address is
// returned as it is.
func reachableAddr(a *net.UDPAddr) *net.UDPAddr {
	if a.IP.IsUnspecified() {
		return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: a.Port}
	}
	return a
}

// newBirthdaySocket opens a birthday socket: a UDP socket on the node's host, bound to port 0, with a UDX side
// that reads its datagrams. Its raw reader starts here. It binds to the IP of the node's own socket, unmapped, so a
// node on all interfaces gets a socket on all interfaces, and a socket that can send off the machine.
func (d *DHT) newBirthdaySocket() (*dhtPunchSocket, error) {
	own, ok := d.node.Socket().Raw().LocalAddr().(*net.UDPAddr)
	if !ok {
		return nil, errors.New("hyperdht: the node's socket has no UDP address")
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: own.IP, Port: 0})
	if err != nil {
		return nil, err
	}
	ud, err := udx.NewSocket(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	s := &dhtPunchSocket{d: d, conn: conn, ud: ud, raw: ud.Raw()}
	s.io = dhtrpc.NewFedIO(s.raw)
	go s.readPunches()
	return s, nil
}

// birthdayReadSize is the buffer that a birthday socket reads into. The dht-rpc messages it gets are the replies to its
// own PING and PEER_HOLEPUNCH requests, far smaller than this; a longer one would be cut short and dropped as malformed.
const birthdayReadSize = 2048

// readPunches gives each one-byte datagram of the birthday socket to its handler. A longer datagram is dht-rpc's: the
// socket's IO takes it, so the replies to the socket's own requests come back to it. The UDX side reads the UDX packets,
// and the rest reach this reader. It returns when the socket closes. Another read error, such as an ICMP reset on
// Windows, is skipped, as dhtrpc's read loop skips it.
func (s *dhtPunchSocket) readPunches() {
	buf := make([]byte, birthdayReadSize)
	for {
		n, from, err := s.raw.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) || s.isClosed() {
				return
			}
			continue
		}
		addr, ok := from.(*net.UDPAddr)
		if !ok {
			continue
		}
		if n >= 2 {
			if s.io != nil {
				s.io.Feed(buf[:n], addr)
			}
			continue
		}
		s.mu.Lock()
		h := s.handler
		s.mu.Unlock()
		if h != nil {
			h(addr)
		}
	}
}

// isClosed reports whether closeSocket has run on a birthday socket.
func (s *dhtPunchSocket) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
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

// closeSocket closes a birthday socket, with its UDX side. Its requests in flight fail, since the socket they went out
// from is closed. A node handle has nothing to close.
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
	if s.io != nil {
		s.io.Close() // closes the raw side, and with it the UDX socket
	}
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
	return d.sampleNATFrom(ctx, d.punchSocket(), p, observers)
}

// sampleNATFrom is sampleNATFromPings from sock: the pings leave sock, so each answer names sock's address behind its
// own NAT mapping. A puncher's samples of a reopened socket are taken this way (upstream nat.autoSample on the socket).
func (d *DHT) sampleNATFrom(ctx context.Context, sock punchSocket, p *holepuncher, observers []*net.UDPAddr) (int, error) {
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
			seen, err := sock.Observe(ctx, o)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				answers++
				p.observe(seen, addressOf(o))
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

// sampleSocket takes p's NAT samples from sock, a socket that p has reopened onto, from the observers of the DHT. It is
// the Sample hook of the punchers that the DHT makes (punchConfig.Sample).
func (d *DHT) sampleSocket(ctx context.Context, sock punchSocket, p *holepuncher) error {
	_, err := d.sampleNATFrom(ctx, sock, p, d.observers())
	return err
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
