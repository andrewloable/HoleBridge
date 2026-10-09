// Ported from dht-rpc 6.27.0 index.js (_natAdd, _updateNetworkState, _checkIfFirewalled) and from
// nat-sampler 1.0.1 index.js, MIT License, Copyright (c) 2021 Mathias Buus.
//
// NAT sampling: the addresses that peers report for us, and the pings that reach our listening port. A
// New node with Ephemeral false probes its port before ready, and becomes persistent when the probe
// passes. Not ported: upstream's adaptive re-check of an ephemeral node, and its separate client socket.
// The IO has one socket, so the probe's replies stand in for the pings that upstream reads from its
// server socket. A NAT that blocks unsolicited packets can still pass this check, because the probe
// opens the mapping itself.
package dhtrpc

import (
	"encoding/binary"
	"net"
	"sync"

	"github.com/andrewloable/HoleBridge/pears/dhtrpc/table"
)

// NATInfo is the node's address as its peers report it, and whether the node is firewalled.
type NATInfo struct {
	Host       string // the IPv4 host that peers report for us; "" until the samples name one
	Port       int    // the port that peers report; 0 when the NAT maps the port randomly
	Firewalled bool   // true until a ping from outside reaches our listening port
	Randomized bool   // the host is consistent, but the port is not
}

// NAT returns our address and firewall state, as upstream samples them from the 'to' addresses that
// peers report. A node stays ephemeral until NAT sampling shows that it is reachable.
func (n *Node) NAT() NATInfo {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.nat.info()
}

// natSampler collects the addresses that peers report for us, as the nat-sampler package does, and
// the pings that reach our listening port unasked. It is the state behind NAT.
type natSampler struct {
	listen    int             // our listening port
	host      string          // the sampled host, "" until the samples name one
	port      int             // the sampled port, 0 when the NAT maps it randomly
	size      int             // adds counted while the ring of samples is filling
	threshold int             // hits a sample needs before its address is the one we have
	top       int             // the next slot of the ring of 32 samples
	samples   []*natSample    // the ring: an address sample and a port-0 sample per add
	a, b      *natSample      // the most hit sample with a port, and the most hit with port 0
	pings     map[string]bool // hosts whose pings reached the listening port
}

// natSample is one sampled address and how many samples matched it.
type natSample struct {
	host string
	port int
	hits int
}

// newNATSampler returns a sampler for a node that listens on port.
func newNATSampler(port int) *natSampler {
	return &natSampler{listen: port, pings: make(map[string]bool)}
}

// add records one address that a peer reported for us.
func (s *natSampler) add(host string, port int) {
	a := s.bump(host, port, 2)
	b := s.bump(host, 0, 1)
	if len(s.samples) < 32 {
		s.size++
		switch {
		case s.size < 4:
			s.threshold = s.size
		case s.size < 8:
			s.threshold = s.size - 1
		case s.size < 12:
			s.threshold = s.size - 2
		default:
			s.threshold = s.size - 3
		}
		s.samples = append(s.samples, a, b)
		s.top += 2
	} else {
		if s.top == 32 {
			s.top = 0
		}
		oa := s.samples[s.top]
		s.samples[s.top] = a
		s.top++
		oa.hits--
		ob := s.samples[s.top]
		s.samples[s.top] = b
		s.top++
		ob.hits--
	}
	if s.a == nil || s.a.hits < a.hits {
		s.a = a
	}
	if s.b == nil || s.b.hits < b.hits {
		s.b = b
	}
	switch {
	case s.a.hits >= s.threshold:
		s.host, s.port = s.a.host, s.a.port
	case s.b.hits >= s.threshold:
		s.host, s.port = s.b.host, 0
	default:
		s.host, s.port = "", 0
	}
}

// bump returns the recent sample that matches host and port, with its hits raised, or a new sample. It
// looks at four samples, spaced two slots apart, from the slot that inc names.
func (s *natSampler) bump(host string, port, inc int) *natSample {
	for i := range 4 {
		j := (s.top - inc - 2*i) & 31
		if j >= len(s.samples) {
			break
		}
		if x := s.samples[j]; x.port == port && x.host == host {
			x.hits++
			return x
		}
	}
	return &natSample{host: host, port: port, hits: 1}
}

// unsolicitedPing records a ping that reached our listening port from host without our asking for it.
func (s *natSampler) unsolicitedPing(host string) {
	s.pings[host] = true
}

// info returns our address and firewall state from the samples and pings recorded so far. A node is
// reachable when the samples name its listening port and a ping has reached that port.
func (s *natSampler) info() NATInfo {
	reachable := s.host != "" && s.port == s.listen && len(s.pings) > 0
	return NATInfo{
		Host:       s.host,
		Port:       s.port,
		Firewalled: !reachable,
		Randomized: s.host != "" && s.port == 0,
	}
}

// natCheck asks up to five table nodes, or the bootstrap nodes when the table has fewer, to ping our
// listening port (PING_NAT), as upstream's _checkIfFirewalled does. It returns the sampler of their
// replies, and true when the replies name our listening port at the address that our own samples name.
func (n *Node) natCheck() (*natSampler, bool) {
	n.mu.Lock()
	cur := n.nat.info()
	port := n.nat.listen
	var addrs []*net.UDPAddr
	for _, nd := range n.table.Closest(n.id, 5) {
		addrs = append(addrs, udpOf(nd))
	}
	n.mu.Unlock()
	if len(addrs) < 5 {
		addrs = append(addrs, n.boot...)
	}
	if len(addrs) == 0 || cur.Host == "" || cur.Port == 0 {
		return nil, false
	}

	val := binary.LittleEndian.AppendUint16(nil, uint16(port))
	probe := newNATSampler(port)
	var (
		mu    sync.Mutex
		count int
		wg    sync.WaitGroup
	)
	for _, a := range addrs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := n.request(a, Request{Internal: true, Command: cmdPingNAT, Value: val})
			if err != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			count++
			probe.add(resp.To.Host.String(), int(resp.To.Port))
			probe.unsolicitedPing(a.IP.String())
		}()
	}
	wg.Wait()

	need := 1
	if len(addrs) >= 5 {
		need = 3
	}
	pi := probe.info()
	if count < need || pi.Firewalled || pi.Host != cur.Host {
		return nil, false
	}
	return probe, true
}

// becomePersistent moves the node to the id of the address that nat names, and keeps the nodes in its
// table. From now on the id goes out with its requests and replies.
func (n *Node) becomePersistent(nat *natSampler) {
	info := nat.info()
	id := nodeID(&net.UDPAddr{IP: net.ParseIP(info.Host), Port: info.Port})
	n.mu.Lock()
	defer n.mu.Unlock()
	nodes := n.table.Closest(n.id, n.table.Len())
	n.table = table.New(id, tableK)
	for _, nd := range nodes {
		n.table.Add(nd)
	}
	n.id = id
	n.persistent = true
	n.nat = nat
}
