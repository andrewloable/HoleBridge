package hyperdht

// An in-memory network with NATs, for the hole punch tests. It stands in for the Internet and for the NATs in
// front of the peers, so punching can be tested on 127.0.0.1 without real networks. It is test code: the
// behaviour it models is upstream's NAT behaviour as hole punching depends on it, not a port of upstream.
//
// A consistent NAT (kind natConsistent) keeps one external port for each internal socket, whatever the
// destination, and accepts a datagram only from an address the socket has sent to (port-restricted cone).
// A randomizing NAT (natRandomized) allocates a new external port for each internal socket and destination,
// and accepts a datagram only from that destination (symmetric). A datagram with a TTL below simHops dies
// after the local NAT, which keeps its mapping; upstream's low TTL is below simHops and the default is not.
// Sockets with no NAT in front are public. Sockets behind the same NAT are on its LAN, each on its own internal
// address: a datagram between them is not translated.
//
// Observations are not datagrams: observe returns the address an observer sees for a socket, as a DHT ping
// answer would, and it is not counted by punches.

import (
	"context"
	"errors"
	"math/rand/v2"
	"net"
	"net/netip"
	"sync"

	"github.com/andrewloable/HoleBridge/pears/dhtrpc"
)

// simHops is the number of hops a datagram crosses. Upstream's low TTL (5) dies before it, the default (64)
// does not.
const simHops = 8

// simPortLow and simPortHigh bound the external ports a NAT allocates, as upstream's random probes do.
const (
	simPortLow  = 1000
	simPortHigh = 65535
)

// natKind is the kind of a NAT.
type natKind int

const (
	natConsistent natKind = iota
	natRandomized
)

// simNet is the simulated network: the NATs by external address, the public sockets, and the sockets on each
// LAN. Every field is guarded by mu.
type simNet struct {
	mu       sync.Mutex
	nats     map[netip.Addr]*simNAT
	public   map[Address]*simSocket
	lan      map[Address]*simSocket
	count    int // holepunch datagrams sent on the network
	nextPort uint16
}

// newSimNet returns an empty network.
func newSimNet() *simNet {
	return &simNet{
		nats:     map[netip.Addr]*simNAT{},
		public:   map[Address]*simSocket{},
		lan:      map[Address]*simSocket{},
		nextPort: 30000,
	}
}

// simNAT is a NAT with external address ext. byKey finds a socket's mapping, byPort finds it by external port.
type simNAT struct {
	net    *simNet
	ext    netip.Addr
	kind   natKind
	byKey  map[simMapKey]*simMapping
	byPort map[uint16]*simMapping
}

// simMapKey identifies a mapping: the socket, and for a randomizing NAT the destination it was made for.
type simMapKey struct {
	sock *simSocket
	dest Address
}

// simMapping is one mapping: its socket, its external address, and what the NAT lets back in. allowed holds the
// destinations a consistent mapping has sent to. dest is the only destination a randomizing mapping accepts.
type simMapping struct {
	sock    *simSocket
	ext     Address
	dest    Address
	allowed map[Address]bool
}

// simSocket is a UDP socket of the simulated network. It implements punchSocket. nat is nil for a public socket.
// onPunch and sent are guarded by net.mu.
type simSocket struct {
	net     *simNet
	nat     *simNAT
	local   Address
	onPunch func(from *net.UDPAddr)
	sent    []simDatagram
}

// simDatagram is one holepunch datagram a socket sent: its destination and TTL.
type simDatagram struct {
	to  Address
	ttl int
}

// addNAT adds a NAT with external address ext and the given kind, and returns it.
func (n *simNet) addNAT(ext netip.Addr, kind natKind) *simNAT {
	nat := &simNAT{net: n, ext: ext, kind: kind, byKey: map[simMapKey]*simMapping{}, byPort: map[uint16]*simMapping{}}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.nats[ext] = nat
	return nat
}

// addPublic adds a public socket at addr, which has no NAT in front, and returns it.
func (n *simNet) addPublic(addr Address) *simSocket {
	n.mu.Lock()
	defer n.mu.Unlock()
	s := &simSocket{net: n, local: addr}
	n.public[addr] = s
	return s
}

// pool returns a pool of sockets behind nat, on the internal address host. Acquire returns a new socket on each
// call. Release does nothing, since no socket is reused in these tests.
func (nat *simNAT) pool(host netip.Addr) *simPool {
	return &simPool{nat: nat, host: host}
}

// simPool hands out sockets behind a NAT. It implements the punch pool.
type simPool struct {
	nat  *simNAT
	host netip.Addr
}

// Acquire returns a new socket behind the pool's NAT, on a fresh internal port.
func (p *simPool) Acquire() punchSocket {
	n := p.nat.net
	n.mu.Lock()
	defer n.mu.Unlock()
	port := n.nextPort
	n.nextPort++
	s := &simSocket{net: n, nat: p.nat, local: Address{Host: p.host, Port: port}}
	n.lan[s.local] = s
	return s
}

// Release gives a socket back to the pool. Nothing is reused, so it does nothing.
func (p *simPool) Release(punchSocket) {}

// addSocket adds a socket behind nat on the internal address host and internal port port, and returns it.
func (nat *simNAT) addSocket(host netip.Addr, port uint16) *simSocket {
	n := nat.net
	n.mu.Lock()
	defer n.mu.Unlock()
	s := &simSocket{net: n, nat: nat, local: Address{Host: host, Port: port}}
	n.lan[s.local] = s
	return s
}

// Local returns the socket's own address: the internal address for a socket behind a NAT.
func (s *simSocket) Local() *net.UDPAddr {
	return udpAddrOf(s.local)
}

// SendPunch sends one holepunch datagram from the socket to to, with ttl, and counts it.
func (s *simSocket) SendPunch(to *net.UDPAddr, ttl int) error {
	n := s.net
	dst := addressOf(to)
	n.mu.Lock()
	n.count++
	s.sent = append(s.sent, simDatagram{to: dst, ttl: ttl})
	src, target := n.route(s, dst, ttl)
	n.mu.Unlock()
	if target != nil {
		go target.receive(src)
	}
	return nil
}

// OnPunch sets the handler of the holepunch datagrams that arrive on the socket. The handler gets the source
// address the receiver sees.
func (s *simSocket) OnPunch(handler func(from *net.UDPAddr)) {
	s.net.mu.Lock()
	defer s.net.mu.Unlock()
	s.onPunch = handler
}

// Observe returns the address that to sees for the socket, as a ping's answer would, with no datagram counted.
func (s *simSocket) Observe(ctx context.Context, to *net.UDPAddr) (Address, error) {
	return s.net.observe(s, addressOf(to)), nil
}

// Request is not used by the simulated sockets: their tests run the holepuncher alone, which sends no dht-rpc request.
func (s *simSocket) Request(ctx context.Context, to *net.UDPAddr, req dhtrpc.Request) (*dhtrpc.Response, error) {
	return nil, errors.ErrUnsupported
}

// receive hands a datagram from src to the socket's handler, if it has one.
func (s *simSocket) receive(src Address) {
	s.net.mu.Lock()
	h := s.onPunch
	s.net.mu.Unlock()
	if h != nil {
		h(udpAddrOf(src))
	}
}

// datagrams returns the holepunch datagrams the socket has sent, in order.
func (s *simSocket) datagrams() []simDatagram {
	s.net.mu.Lock()
	defer s.net.mu.Unlock()
	return append([]simDatagram(nil), s.sent...)
}

// route returns the source address that the receiver of a datagram from s to dst sees, and the socket that
// gets the datagram, or nil when the network drops it. The caller holds n.mu.
func (n *simNet) route(s *simSocket, dst Address, ttl int) (Address, *simSocket) {
	if s.nat != nil {
		// Behind the same NAT: on its LAN, so no NAT between the two sockets. The check is by NAT, not by host,
		// since the sockets of one NAT may sit on different internal hosts (the shared-IP test).
		if peer := n.lan[dst]; peer != nil && peer.nat == s.nat {
			return s.local, peer
		}
	}
	src := s.local
	if s.nat != nil {
		src = s.nat.outbound(s, dst)
		if ttl < simHops {
			return src, nil
		}
	}
	if nat, ok := n.nats[dst.Host]; ok {
		return src, nat.inbound(dst.Port, src)
	}
	return src, n.public[dst]
}

// outbound returns the external address of s's mapping for dest, which it creates if there is none, and records
// what the mapping now accepts. The caller holds n.mu.
func (nat *simNAT) outbound(s *simSocket, dest Address) Address {
	key := simMapKey{sock: s}
	if nat.kind == natRandomized {
		key.dest = dest
	}
	m := nat.byKey[key]
	if m == nil {
		m = &simMapping{sock: s, dest: dest, allowed: map[Address]bool{}}
		m.ext = Address{Host: nat.ext, Port: nat.freePort()}
		nat.byKey[key] = m
		nat.byPort[m.ext.Port] = m
	}
	if nat.kind == natConsistent {
		m.allowed[dest] = true
	}
	return m.ext
}

// inbound returns the socket that a datagram from src to the external port port reaches, or nil when the NAT
// has no mapping on port or does not accept src. The caller holds n.mu.
func (nat *simNAT) inbound(port uint16, src Address) *simSocket {
	m := nat.byPort[port]
	if m == nil {
		return nil
	}
	if nat.kind == natConsistent {
		if m.allowed[src] {
			return m.sock
		}
		return nil
	}
	if m.dest == src {
		return m.sock
	}
	return nil
}

// freePort returns a random external port in the range upstream's random probes use, that no mapping has.
// The caller holds n.mu.
func (nat *simNAT) freePort() uint16 {
	for {
		p := uint16(simPortLow + rand.IntN(simPortHigh-simPortLow+1))
		if _, used := nat.byPort[p]; !used {
			return p
		}
	}
}

// mappedToward returns the external ports that the NAT has mapped for dest: for a consistent NAT, the mappings
// that accept dest; for a randomizing NAT, the mappings made for dest.
func (nat *simNAT) mappedToward(dest Address) []uint16 {
	nat.net.mu.Lock()
	defer nat.net.mu.Unlock()
	var ports []uint16
	for _, m := range nat.byKey {
		if (nat.kind == natConsistent && m.allowed[dest]) || (nat.kind == natRandomized && m.dest == dest) {
			ports = append(ports, m.ext.Port)
		}
	}
	return ports
}

// observe returns the address that observer sees when s sends to it: s's own address with no NAT, else the
// external address of s's mapping for observer. It sends no datagram and is not counted by punches.
func (n *simNet) observe(s *simSocket, observer Address) Address {
	n.mu.Lock()
	defer n.mu.Unlock()
	if s.nat == nil {
		return s.local
	}
	return s.nat.outbound(s, observer)
}

// punches returns how many holepunch datagrams have been sent on the network so far.
func (n *simNet) punches() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.count
}
