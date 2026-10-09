// Ported from hyperdht 6.34.1 lib/router.js (onpeerhandshake and its modes) and the route part of
// lib/persistent.js (onannounce for a key's own hash, _onrefresh and onfindpeer), MIT License, Copyright
// (c) 2018-2019 Mathias Buus, David Mark Clements & Contributors.
//
// Handshake routing. A key that announces on the hash of its public key gets a route on each node that
// stores the announce: on the server's own node, the server, which answers the handshakes itself; on the
// other nodes, the server's address, to which they relay the handshakes of clients. FIND_PEER answers
// with the route's record, and a LOOKUP reply carries it too. PEER_HOLEPUNCH is routed the same way
// (onPeerHolepunch), so a client's holepunch probe reaches the server and its answer comes back.
package hyperdht

import (
	"net"
	"net/netip"
	"sync"
	"time"

	"golang.org/x/crypto/blake2b"

	"github.com/andrewloable/HoleBridge/pears/dhtrpc"
)

// route is what a node keeps for a key that announces on the hash of its public key.
type route struct {
	peer    Peer         // the key and its relay addresses: the record that FIND_PEER returns
	relay   *net.UDPAddr // where a handshake for the key is relayed: the server's address; nil on the server
	serve   *Server      // the server on this node, which answers the handshakes; nil on the other nodes
	expires time.Time    // when the route lapses; zero for the server's own route, which does not
}

// routeTable is the routes a node keeps, by target. A route lapses recordMaxAge after its announce, unless
// it is set again.
type routeTable struct {
	mu     sync.Mutex
	routes map[[32]byte]route
}

func newRouteTable() *routeTable {
	return &routeTable{routes: make(map[[32]byte]route)}
}

// get returns the route of target, unless it has lapsed at now.
func (t *routeTable) get(target [32]byte, now time.Time) (route, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.routes[target]
	if !ok {
		return route{}, false
	}
	if !r.expires.IsZero() && now.After(r.expires) {
		delete(t.routes, target)
		return route{}, false
	}
	return r, true
}

// set stores r as the route of target. A new target is refused when the table is full, after the lapsed
// routes are dropped.
func (t *routeTable) set(target [32]byte, r route) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.routes[target]; !ok && len(t.routes) >= recordMaxCount {
		t.dropLapsed(time.Now())
		if len(t.routes) >= recordMaxCount {
			return
		}
	}
	t.routes[target] = r
}

// dropLapsed drops the routes that have lapsed at now. The caller holds mu.
func (t *routeTable) dropLapsed(now time.Time) {
	for target, r := range t.routes {
		if !r.expires.IsZero() && now.After(r.expires) {
			delete(t.routes, target)
		}
	}
}

// delete drops the route of target.
func (t *routeTable) delete(target [32]byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.routes, target)
}

// dropServer drops the route of target when it is the route of server s. Closing a server leaves the
// route of another server on the same node alone.
func (t *routeTable) dropServer(target [32]byte, s *Server) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if r, ok := t.routes[target]; ok && r.serve == s {
		delete(t.routes, target)
	}
}

// isHashOf reports whether target is the hash of the public key pk, which makes an announce of pk on
// target a route, not a record.
func isHashOf(target [32]byte, pk [32]byte) bool {
	return blake2b.Sum256(pk[:]) == target
}

// onFindPeer answers a FIND_PEER with the record of the route for the target, when this node has one. The
// reply names the closest nodes either way, so the requester can go on, as upstream's onfindpeer does.
func (d *DHT) onFindPeer(req *dhtrpc.Request) *dhtrpc.Response {
	if d.node.ID() == nil || req.Target == nil {
		return nil
	}
	var target [32]byte
	copy(target[:], req.Target)
	resp := &dhtrpc.Response{CloserNodes: d.node.Closest(req.Target)}
	if rt, ok := d.routes.get(target, time.Now()); ok {
		value, err := EncodePeer(rt.peer)
		if err != nil {
			return nil
		}
		resp.Value = value
	}
	return resp
}

// onPeerHandshake routes a PEER_HANDSHAKE, as upstream's router.onpeerhandshake does. On the server's
// node the server answers. Elsewhere the handshake is relayed towards the server, or the reply that
// comes back from it is relayed to the client.
func (d *DHT) onPeerHandshake(req *dhtrpc.Request) *dhtrpc.Response {
	hs, err := DecodeHandshake(req.Value)
	if err != nil {
		return nil
	}
	var rt route
	ok := false
	if req.Target != nil {
		var target [32]byte
		copy(target[:], req.Target)
		rt, ok = d.routes.get(target, time.Now())
	}
	if ok && rt.serve != nil {
		return d.serveHandshake(req, hs, rt.serve)
	}
	return d.routeHandshake(req, hs, rt, ok)
}

// maxHandshakes bounds the handshake decisions that run at once. A decision can wait on the server's firewall and
// relay policy, so each one runs off the read loop; a handshake that arrives while this many are running gets no
// reply, and the client sends it again, as it would a lost datagram.
const maxHandshakes = 64

// serveHandshake answers a handshake for the server s on this node. The decision runs off the read loop, since
// the firewall and the relay policy may take a while and may use this node: the loop must go on reading the
// replies they wait for. decideHandshake sends the reply when the decision is made.
func (d *DHT) serveHandshake(req *dhtrpc.Request, hs Handshake, s *Server) *dhtrpc.Response {
	if len(hs.Noise) == 0 {
		return nil
	}
	select {
	case d.decide <- struct{}{}:
	default:
		return nil
	}
	go func() {
		defer func() { <-d.decide }()
		d.decideHandshake(req, hs, s)
	}()
	return nil
}

// decideHandshake makes the decision for one handshake of the server s, and sends the reply. Each mode gets the
// reply that upstream's router sends for it: a reply to the client, or a relayed request that carries the reply
// back to the client through the relay that sent it.
func (d *DHT) decideHandshake(req *dhtrpc.Request, hs Handshake, s *Server) {
	// The client's address is the one a relay names, or else the address the request came from, as
	// upstream's server takes it.
	client := req.From
	if hs.PeerAddress != nil {
		client = udpAddrOf(*hs.PeerAddress)
	}
	reply := s.answer(hs.Noise, client, hs.Mode == handshakeFromClient)
	if reply == nil {
		return
	}
	switch hs.Mode {
	case handshakeFromClient:
		value, err := EncodeHandshake(Handshake{Mode: handshakeReply, Noise: reply})
		if err != nil {
			return
		}
		d.node.ReplyTo(req.From, dhtrpc.Response{Tid: req.Tid, Value: value, NoToken: true})
	case handshakeFromRelay:
		d.passOn(req, req.From, Handshake{Mode: handshakeFromServer, Noise: reply, PeerAddress: hs.PeerAddress})
	case handshakeFromSecondRelay:
		if hs.RelayAddress != nil {
			d.passOn(req, udpAddrOf(*hs.RelayAddress), Handshake{Mode: handshakeFromServer, Noise: reply, PeerAddress: hs.PeerAddress})
		}
	}
}

// routeHandshake handles a handshake on a node that is not the server's. rt is the route of the target,
// and ok says whether there is one. A client's handshake goes to the relay the client names, or to the
// server's address from the route. With neither, the reply names the closest nodes, so the client can
// route on.
func (d *DHT) routeHandshake(req *dhtrpc.Request, hs Handshake, rt route, ok bool) *dhtrpc.Response {
	switch hs.Mode {
	case handshakeFromClient:
		if len(hs.Noise) == 0 {
			return nil
		}
		next := rt.relay
		if hs.RelayAddress != nil {
			next = udpAddrOf(*hs.RelayAddress)
		}
		if next == nil {
			resp := &dhtrpc.Response{NoToken: true}
			if req.Target != nil {
				resp.CloserNodes = d.node.Closest(req.Target)
			}
			return resp
		}
		d.passOn(req, next, Handshake{Mode: handshakeFromRelay, Noise: hs.Noise, PeerAddress: ptr(addressOf(req.From))})
	case handshakeFromRelay:
		if !ok || rt.relay == nil || len(hs.Noise) == 0 {
			return nil
		}
		d.passOn(req, rt.relay, Handshake{Mode: handshakeFromSecondRelay, Noise: hs.Noise, PeerAddress: hs.PeerAddress, RelayAddress: ptr(addressOf(req.From))})
	case handshakeFromServer:
		if hs.PeerAddress == nil || len(hs.Noise) == 0 {
			return nil
		}
		value, err := EncodeHandshake(Handshake{Mode: handshakeReply, Noise: hs.Noise, PeerAddress: ptr(addressOf(req.From))})
		if err != nil {
			return nil
		}
		d.node.ReplyTo(udpAddrOf(*hs.PeerAddress), dhtrpc.Response{Tid: req.Tid, Value: value, NoToken: true})
	}
	return nil
}

// passOn sends the handshake message h on to the address to. The request keeps the tid and target of the
// request it passes on, and nothing waits for its reply, as upstream's request.relay does.
func (d *DHT) passOn(req *dhtrpc.Request, to *net.UDPAddr, h Handshake) {
	value, err := EncodeHandshake(h)
	if err != nil {
		return
	}
	d.node.Relay(to, dhtrpc.Request{Tid: req.Tid, Command: cmdPeerHandshake, Target: req.Target, Value: value})
}

// onPeerHolepunch routes a PEER_HOLEPUNCH, as upstream's router.onpeerholepunch does. A client's probe goes on to
// the address the client names for the server, or to the relay of the route. The server on this node answers a
// probe that a relay forwarded, and the answer goes back through that relay to the client. A node that is not the
// server passes the server's answer on to the client.
func (d *DHT) onPeerHolepunch(req *dhtrpc.Request) *dhtrpc.Response {
	hp, err := DecodeHolepunch(req.Value)
	if err != nil {
		return nil
	}
	var rt route
	ok := false
	if req.Target != nil {
		var target [32]byte
		copy(target[:], req.Target)
		rt, ok = d.routes.get(target, time.Now())
	}
	switch hp.Mode {
	case holepunchFromClient:
		next := rt.relay
		if hp.PeerAddress != nil {
			next = udpAddrOf(*hp.PeerAddress)
		}
		if next == nil {
			return nil
		}
		d.relayHolepunch(req, next, Holepunch{Mode: holepunchFromRelay, ID: hp.ID, Payload: hp.Payload, PeerAddress: ptr(addressOf(req.From))})
	case holepunchFromRelay:
		if !ok || rt.serve == nil || hp.PeerAddress == nil {
			return nil
		}
		// The relay saw this server at the address the probe names, so the probe is a NAT sample from that relay.
		if req.To.Host.IsValid() && req.From != nil {
			rt.serve.observeProbe(hp.ID, Address{Host: req.To.Host, Port: req.To.Port}, addressOf(req.From))
		}
		// The answer can wait for the puncher's NAT samples, so it is made off the read loop, and sent back through
		// the relay when it is made (as the handshake decision is).
		go func() {
			reply := rt.serve.answerHolepunch(hp.ID, hp.Payload, udpAddrOf(*hp.PeerAddress), req.From)
			if reply == nil {
				return
			}
			d.relayHolepunch(req, req.From, Holepunch{Mode: holepunchFromServer, Payload: reply, PeerAddress: hp.PeerAddress})
		}()
	case holepunchFromServer:
		if hp.PeerAddress == nil {
			return nil
		}
		value, err := EncodeHolepunch(Holepunch{Mode: holepunchReply, ID: hp.ID, Payload: hp.Payload, PeerAddress: ptr(addressOf(req.From))})
		if err != nil {
			return nil
		}
		d.node.ReplyTo(udpAddrOf(*hp.PeerAddress), dhtrpc.Response{Tid: req.Tid, Value: value, NoToken: true})
	}
	return nil
}

// relayHolepunch sends the holepunch message h on to the address to, under the tid and target of req, as passOn
// does for a handshake. Nothing waits for its reply.
func (d *DHT) relayHolepunch(req *dhtrpc.Request, to *net.UDPAddr, h Holepunch) {
	value, err := EncodeHolepunch(h)
	if err != nil {
		return
	}
	d.node.Relay(to, dhtrpc.Request{Tid: req.Tid, Command: cmdPeerHolepunch, Target: req.Target, Value: value})
}

// addressOf returns the wire address of a UDP address. The addresses on the wire are IPv4.
func addressOf(a *net.UDPAddr) Address {
	return Address{Host: netip.AddrFrom4([4]byte(a.IP.To4())), Port: uint16(a.Port)}
}

// udpAddrOf returns the UDP address of a wire address.
func udpAddrOf(a Address) *net.UDPAddr {
	return &net.UDPAddr{IP: net.IP(a.Host.AsSlice()), Port: int(a.Port)}
}
