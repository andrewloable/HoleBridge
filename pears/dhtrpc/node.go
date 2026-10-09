// Ported from dht-rpc 6.27.0 index.js, lib/commands.js and lib/errors.js, MIT License, Copyright (c)
// 2021 Mathias Buus.
//
// The DHT node: a Kademlia routing table filled by the RPC socket, the built-in commands, bootstrap and
// table upkeep. Iterative queries are in query.go, and NAT sampling with the switch to persistent is in
// nat.go. The node runs on a udx.Socket, so its UDP port also carries UDX streams: the RPC side gets the
// datagrams that are not UDX packets. Not ported yet: bootstrap as a query (it is one round of FIND_NODE
// and PING), DELAYED_PING, and upstream's adaptive switch, which re-checks an ephemeral node once it is
// stable.
package dhtrpc

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"os"
	"sync"
	"time"

	"github.com/andrewloable/HoleBridge/pears/dhtrpc/table"
	"github.com/andrewloable/HoleBridge/pears/udx"
)

// Built-in commands, numbered as upstream does (dht-rpc 6.27.0 lib/commands.js).
const (
	cmdPing     = 0
	cmdPingNAT  = 1
	cmdFindNode = 2
	cmdDownHint = 3
)

const (
	errUnknownCommand  = 1  // upstream's UNKNOWN_COMMAND error (lib/errors.js)
	tableK             = 20 // nodes per bucket
	tickInterval       = 5 * time.Second
	pingTicks          = 8  // a random table node is pinged every 8 ticks
	refreshTicks       = 60 // the table is refreshed every 60 ticks
	maxChecks          = 10 // DOWN_HINT pings in flight at once
	downHintsRateLimit = 50 // DOWN_HINT requests a node sends in one tick (upstream default 10 * 5)
)

// Config sets up one DHT node.
type Config struct {
	Bootstrap []string // host:port of the bootstrap nodes
	Port      int      // UDP port to listen on
	Ephemeral *bool    // false asks for a persistent node; nil and true keep the node ephemeral
}

// Node is a DHT node: a Kademlia routing table, the built-in commands and the custom commands that
// Handle registers. Start one with New.
type Node struct {
	sock       *udx.Socket // the UDP socket: the RPC side is sock.Raw, and UDX streams use sock
	local      *net.UDPAddr
	rpc        *IO
	id         [32]byte    // our table id: the peer id of our address when persistent, else random
	persistent bool        // our id goes out in requests and replies
	persist    bool        // a New node with Ephemeral false: persistent once NAT sampling allows
	nat        *natSampler // our address as peers report it, and whether a ping reached our port
	boot       []*net.UDPAddr
	ready      chan struct{} // closed when bootstrap is done
	quit       chan struct{} // closed by Close
	closeOnce  sync.Once

	mu       sync.Mutex // guards table, handlers, checks, hints, rpc, nat, id and persistent
	table    *table.Table
	handlers map[uint64]func(*Request) *Response
	checks   int // DOWN_HINT pings in flight
	hints    int // DOWN_HINT requests sent in this tick
}

// New starts a node with cfg and begins bootstrapping in the background. The node listens on all IPv4
// interfaces. With Ephemeral false it becomes persistent once NAT sampling finds an address that its
// peers agree on and a probe reaches its port; until then it stays ephemeral.
func New(cfg Config) (*Node, error) {
	return open(cfg, nil)
}

// open starts a node listening on ip and cfg.Port, on all IPv4 interfaces when ip is nil. A node on a
// fixed address is persistent when cfg asks for it, and its id is the peer id of that address. That is
// how upstream's DHT.bootstrapper seeds a bootstrap node with its own address before bootstrap.
func open(cfg Config, ip net.IP) (*Node, error) {
	var boot []*net.UDPAddr
	for _, s := range cfg.Bootstrap {
		a, err := net.ResolveUDPAddr("udp4", s)
		if err != nil {
			return nil, err
		}
		boot = append(boot, a)
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip, Port: cfg.Port})
	if err != nil {
		return nil, err
	}
	sock, err := udx.NewSocket(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	local := conn.LocalAddr().(*net.UDPAddr)
	n := &Node{
		sock:     sock,
		local:    local,
		boot:     boot,
		ready:    make(chan struct{}),
		quit:     make(chan struct{}),
		handlers: make(map[uint64]func(*Request) *Response),
		nat:      newNATSampler(local.Port),
	}
	forced := cfg.Ephemeral != nil && !*cfg.Ephemeral
	if forced && ip != nil {
		n.persistent = true
		n.id = nodeID(local)
	} else {
		rand.Read(n.id[:])
	}
	n.persist = forced && ip == nil
	n.table = table.New(n.id, tableK)
	// handle reads rpc under mu, so a request that arrives while NewIO returns waits for rpc to be set.
	n.mu.Lock()
	n.rpc = NewIO(sock.Raw(), n.handle)
	n.mu.Unlock()
	go n.bootstrap()
	go n.upkeep()
	return n, nil
}

// Ready waits until the node has bootstrapped, or until ctx ends.
func (n *Node) Ready(ctx context.Context) error {
	select {
	case <-n.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops the node and closes its socket.
func (n *Node) Close() error {
	n.closeOnce.Do(func() { close(n.quit) })
	return n.rpc.Close()
}

// Handle answers the requests for the custom command cmd with h. The handler returns the reply, or
// nil for no reply.
func (n *Node) Handle(cmd uint, h func(*Request) *Response) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.handlers[uint64(cmd)] = h
}

// Request sends req to the address to, as a request of this node, and returns its reply. It is upstream's
// dht.request: a reply from a persistent node adds that node to the table, and a timeout removes it.
func (n *Node) Request(ctx context.Context, to *net.UDPAddr, req Request) (*Response, error) {
	return n.requestRetry(ctx, to, req, requestRetries, nil)
}

// Relay sends req to the address to with the tid req carries, once, and waits for no reply. A handler
// that passes a request on uses it, as upstream's request.relay does: the node it is sent to answers
// under the same tid.
func (n *Node) Relay(to *net.UDPAddr, req Request) {
	req.ID = n.selfID()
	n.mu.Lock()
	rpc := n.rpc
	n.mu.Unlock()
	rpc.relay(to, req)
}

// ReplyTo sends r to the address to, as a reply with the tid r.Tid. A handler uses it to answer a relayed
// request for the client, which is not the node that relayed it.
func (n *Node) ReplyTo(to *net.UDPAddr, r Response) {
	if to.IP.To4() == nil {
		return
	}
	if r.ID == nil {
		r.ID = n.selfID()
	}
	n.mu.Lock()
	rpc := n.rpc
	n.mu.Unlock()
	rpc.send(to, r)
}

// ID returns the id that goes out with our requests and replies, or nil while the node is ephemeral. A
// handler that checks a signature made over our id uses it, as upstream uses dht.id.
func (n *Node) ID() []byte {
	return n.selfID()
}

// Addr returns the UDP address the node listens on.
func (n *Node) Addr() (*net.UDPAddr, error) {
	return n.addr()
}

// Socket returns the UDP socket the node runs on. UDX streams made on it share the node's port.
func (n *Node) Socket() *udx.Socket {
	return n.sock
}

// Closest returns the table nodes nearest target. A custom command's reply carries them, as upstream's
// replies to a request with a target do, unless the reply opts out.
func (n *Node) Closest(target []byte) []Addr {
	return n.closest(target)
}

// addr returns the UDP address the node listens on. A node on all interfaces is addressed as 127.0.0.1,
// which is how a local peer reaches it: macOS refuses to send to 0.0.0.0.
func (n *Node) addr() (*net.UDPAddr, error) {
	a := n.local
	if a.IP.IsUnspecified() {
		return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: a.Port}, nil
	}
	return a, nil
}

// peers returns a snapshot of the nodes in the routing table.
func (n *Node) peers() ([]table.Node, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.table.Closest(n.id, n.table.Len()), nil
}

// handle answers one request, as upstream's _onrequest does. A request from a persistent node adds the
// sender to the table, then a built-in command is answered, and any other command goes to its Handle.
// A nil reply sends nothing.
func (n *Node) handle(req *Request, from *net.UDPAddr) *Response {
	// every request that reaches our port is a ping from outside, as the NAT check counts them
	n.mu.Lock()
	n.nat.unsolicitedPing(from.IP.String())
	n.mu.Unlock()
	if req.ID != nil {
		n.learn(from, req.To)
	}
	if !req.Internal {
		n.mu.Lock()
		h := n.handlers[req.Command]
		n.mu.Unlock()
		if h == nil {
			return &Response{ID: n.selfID(), Error: errUnknownCommand}
		}
		resp := h(req)
		if resp != nil && resp.ID == nil {
			resp.ID = n.selfID()
		}
		return resp
	}
	switch req.Command {
	case cmdPing:
		return &Response{ID: n.selfID()}
	case cmdPingNAT:
		if len(req.Value) < 2 {
			return nil
		}
		port := binary.LittleEndian.Uint16(req.Value)
		if port == 0 {
			return nil
		}
		n.mu.Lock()
		rpc := n.rpc
		n.mu.Unlock()
		// upstream replies to the port the request names, and that port is the reply's To
		rpc.send(&net.UDPAddr{IP: from.IP, Port: int(port)}, Response{Tid: req.Tid, ID: n.selfID(), Token: rpc.token(from, 1)})
		return nil
	case cmdFindNode:
		if req.Target == nil {
			return nil
		}
		return &Response{ID: n.selfID(), CloserNodes: n.closest(req.Target)}
	case cmdDownHint:
		if len(req.Value) < 6 {
			return nil
		}
		n.downHint(&net.UDPAddr{IP: net.IP(req.Value[:4]), Port: int(binary.LittleEndian.Uint16(req.Value[4:6]))})
		return &Response{ID: n.selfID()}
	}
	return &Response{ID: n.selfID(), Error: errUnknownCommand}
}

// selfID returns the id that goes out with our requests and replies. An ephemeral node sends none, as
// upstream sends the id only from a persistent node.
func (n *Node) selfID() []byte {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.persistent {
		return nil
	}
	id := n.id
	return id[:]
}

// closest returns the table nodes nearest target, as a FIND_NODE reply carries them.
func (n *Node) closest(target []byte) []Addr {
	var t [32]byte
	copy(t[:], target)
	n.mu.Lock()
	nodes := n.table.Closest(t, tableK)
	n.mu.Unlock()
	addrs := make([]Addr, len(nodes))
	for i, nd := range nodes {
		addrs[i] = Addr{Host: netip.MustParseAddr(nd.Host), Port: uint16(nd.Port)}
	}
	return addrs
}

// downHint pings the node that a DOWN_HINT names, when the table holds it. The ping removes the node if
// it times out. As upstream does, at most maxChecks pings run at once.
func (n *Node) downHint(addr *net.UDPAddr) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, ok := n.table.Get(nodeID(addr)); !ok || n.checks >= maxChecks {
		return
	}
	n.checks++
	go n.check(addr)
}

// sendDownHint asks the node at ref to drop down, which timed out in a query, as upstream's _downHint does:
// a DOWN_HINT request whose value is the address of down. The query does not wait for the reply. A node
// sends at most downHintsRateLimit hints in each tick.
func (n *Node) sendDownHint(ref, down *net.UDPAddr) {
	n.mu.Lock()
	if n.hints >= downHintsRateLimit {
		n.mu.Unlock()
		return
	}
	n.hints++
	n.mu.Unlock()
	req := Request{Internal: true, Command: cmdDownHint, Value: hintValue(down)}
	go n.requestRetry(context.Background(), ref, req, requestRetries, nil)
}

// hintValue returns addr as a DOWN_HINT carries it: the IPv4 host, then the port as uint16 LE.
func hintValue(addr *net.UDPAddr) []byte {
	b := make([]byte, 6)
	copy(b, addr.IP.To4())
	binary.LittleEndian.PutUint16(b[4:], uint16(addr.Port))
	return b
}

// check pings addr for a DOWN_HINT. It runs in its own goroutine, because the handler runs on the read
// loop, which must keep reading the reply.
func (n *Node) check(addr *net.UDPAddr) {
	n.request(addr, Request{Internal: true, Command: cmdPing})
	n.mu.Lock()
	n.checks--
	n.mu.Unlock()
}

// request sends req to addr. A reply from a persistent node adds it to the table. A timeout removes it,
// as upstream's ontimeout does.
func (n *Node) request(addr *net.UDPAddr, req Request) (*Response, error) {
	return n.requestRetry(context.Background(), addr, req, requestRetries, nil)
}

// requestRetry is request with a context and a resend count. A query sends its requests with upstream's
// query retries, and its replies and timeouts update the table as any request's do. cycle, when set, is
// called on each send that gets no reply, as IO.requestRetry does.
func (n *Node) requestRetry(ctx context.Context, addr *net.UDPAddr, req Request, retries int, cycle func()) (*Response, error) {
	req.ID = n.selfID()
	resp, err := n.rpc.requestRetry(ctx, addr, req, retries, cycle)
	switch {
	case err == nil && resp.ID != nil:
		n.learn(addr, resp.To)
	case errors.Is(err, os.ErrDeadlineExceeded):
		n.remove(addr)
	}
	return resp, err
}

// learn stores the persistent node at from in the table. It also samples the address that the node
// reports for us, to, once, when the node first joins the table: one peer repeated must not make a
// sample look consistent on its own.
func (n *Node) learn(from *net.UDPAddr, to Addr) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, ok := n.table.Get(nodeID(from)); !ok {
		n.nat.add(to.Host.String(), int(to.Port))
	}
	n.table.Add(nodeOf(from))
}

// remove drops the node at addr from the table.
func (n *Node) remove(addr *net.UDPAddr) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.table.Remove(nodeID(addr))
}

// random returns a random table node, and false when the table is empty.
func (n *Node) random() (table.Node, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.table.Random()
}

// bootstrap asks each bootstrap node for the nodes nearest our id, and closes ready when all have
// answered. A New node that asked to be persistent probes its port between the two rounds: when the
// probe passes, it takes its sampled id and asks again, so the bootstrap nodes learn that id before
// ready. Upstream runs an iterative query here. The query task replaces this single round.
func (n *Node) bootstrap() {
	n.findBoot()
	if n.persist {
		if nat, ok := n.natCheck(); ok {
			n.becomePersistent(nat)
			n.findBoot()
		}
	}
	close(n.ready)
}

// findBoot asks each bootstrap node for the nodes nearest our id, in parallel.
func (n *Node) findBoot() {
	var wg sync.WaitGroup
	for _, b := range n.boot {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n.findNear(b)
		}()
	}
	wg.Wait()
}

// findNear asks addr for the nodes nearest our id, and pings each node that it names. A ping that is
// answered by a persistent node adds that node to the table.
func (n *Node) findNear(addr *net.UDPAddr) {
	resp, err := n.request(addr, Request{Internal: true, Command: cmdFindNode, Target: n.id[:]})
	if err != nil {
		return
	}
	var wg sync.WaitGroup
	for _, a := range resp.CloserNodes {
		near := &net.UDPAddr{IP: net.IP(a.Host.AsSlice()), Port: int(a.Port)}
		if nodeID(near) == n.id {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			n.request(near, Request{Internal: true, Command: cmdPing})
		}()
	}
	wg.Wait()
}

// upkeep runs once the node is ready, every tickInterval. Every pingTicks ticks it pings a random table
// node, and every refreshTicks ticks it refreshes the table from a random node.
func (n *Node) upkeep() {
	select {
	case <-n.ready:
	case <-n.quit:
		return
	}
	t := time.NewTicker(tickInterval)
	defer t.Stop()
	for tick := 1; ; tick++ {
		select {
		case <-n.quit:
			return
		case <-t.C:
		}
		n.mu.Lock()
		n.hints = 0
		n.mu.Unlock()
		if tick%pingTicks == 0 {
			if nd, ok := n.random(); ok {
				n.request(udpOf(nd), Request{Internal: true, Command: cmdPing})
			}
		}
		if tick%refreshTicks == 0 {
			if nd, ok := n.random(); ok {
				n.findNear(udpOf(nd))
			}
		}
	}
}

// nodeID returns the dht-rpc id of addr, the peer id of its host and port.
func nodeID(addr *net.UDPAddr) [32]byte {
	var id [32]byte
	copy(id[:], peerID(addr))
	return id
}

// nodeOf returns the table entry for addr.
func nodeOf(addr *net.UDPAddr) table.Node {
	return table.Node{ID: nodeID(addr), Host: addr.IP.String(), Port: addr.Port}
}

// udpOf returns the address of a table entry.
func udpOf(nd table.Node) *net.UDPAddr {
	return &net.UDPAddr{IP: net.ParseIP(nd.Host), Port: nd.Port}
}
