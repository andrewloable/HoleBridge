package dhtrpc

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"runtime"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/pears/dhtrpc/table"
)

// newNode starts a node with cfg, and the test closes it when it ends.
func newNode(t *testing.T, cfg Config) *Node {
	t.Helper()
	n, err := New(cfg)
	checkImplemented(t, "New", err)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { n.Close() })
	return n
}

// startTestnet starts a testnet of size nodes. The stub returns nil, which fails the test.
func startTestnet(t *testing.T, size int) *Testnet {
	t.Helper()
	tn := NewTestnet(t, size)
	if tn == nil {
		t.Fatal("NewTestnet: not implemented")
	}
	return tn
}

// nodeAddr returns the UDP address that n listens on.
func nodeAddr(t *testing.T, n *Node) *net.UDPAddr {
	t.Helper()
	addr, err := n.addr()
	checkImplemented(t, "addr", err)
	if err != nil {
		t.Fatalf("addr: %v", err)
	}
	return addr
}

// nodePeers returns a snapshot of the routing table of n.
func nodePeers(t *testing.T, n *Node) []table.Node {
	t.Helper()
	peers, err := n.peers()
	checkImplemented(t, "peers", err)
	if err != nil {
		t.Fatalf("peers: %v", err)
	}
	return peers
}

// idOfAddr returns the dht-rpc id of addr: the BLAKE2b-256 of its IPv4 host and port (peerID).
func idOfAddr(addr *net.UDPAddr) [32]byte {
	var id [32]byte
	copy(id[:], peerID(addr))
	return id
}

// hasPeer reports whether a routing table holds the node with id.
func hasPeer(peers []table.Node, id [32]byte) bool {
	for _, p := range peers {
		if p.ID == id {
			return true
		}
	}
	return false
}

// downHintValue returns addr as a DOWN_HINT carries it: the IPv4 host, then the port as uint16 LE.
func downHintValue(addr *net.UDPAddr) []byte {
	b := make([]byte, 6)
	copy(b, addr.IP.To4())
	binary.LittleEndian.PutUint16(b[4:], uint16(addr.Port))
	return b
}

// registerHandler calls n.Handle. The stub panics, so the panic becomes a test failure here, which
// lets the other tests run.
func registerHandler(t *testing.T, n *Node, cmd uint, h func(*Request) *Response) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Handle: %v", r)
		}
	}()
	n.Handle(cmd, h)
}

// Test case 1: NewTestnet(t, 5). Every node becomes ready, and each routing table holds the other four
// nodes under their dht-rpc ids.
func TestTestnetReadyAndTables(t *testing.T) {
	tn := startTestnet(t, 5)
	if len(tn.Nodes) != 5 {
		t.Fatalf("testnet has %d nodes, want 5", len(tn.Nodes))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ids := make([][32]byte, len(tn.Nodes))
	for i, n := range tn.Nodes {
		ids[i] = idOfAddr(nodeAddr(t, n))
	}
	for i, n := range tn.Nodes {
		err := n.Ready(ctx)
		checkImplemented(t, "Ready", err)
		if err != nil {
			t.Fatalf("node %d Ready: %v", i, err)
		}
	}
	for i, n := range tn.Nodes {
		peers := nodePeers(t, n)
		if len(peers) != len(ids)-1 {
			t.Errorf("node %d routing table has %d nodes, want %d", i, len(peers), len(ids)-1)
		}
		for j, id := range ids {
			if j != i && !hasPeer(peers, id) {
				t.Errorf("node %d routing table lacks node %d", i, j)
			}
		}
	}
}

// Test case 2: PING is an internal built-in command. Its reply has no error and carries the node's
// id, which is the dht-rpc id of the node's address.
func TestPingReturnsID(t *testing.T) {
	tn := startTestnet(t, 5)
	addr := nodeAddr(t, tn.Nodes[0])
	client, _ := newIO(t, noReply)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := client.Request(ctx, addr, Request{Internal: true, Command: cmdPing})
	checkImplemented(t, "Request", err)
	if err != nil {
		t.Fatalf("PING: %v", err)
	}
	if resp.Error != 0 {
		t.Errorf("PING reply error = %d, want 0", resp.Error)
	}
	if want := peerID(addr); !bytes.Equal(resp.ID, want) {
		t.Errorf("PING reply id = %x, want %x", resp.ID, want)
	}
}

// Test case 3: FIND_NODE returns the nodes of the responder's table closest to the target. The table
// holds the four other nodes, fewer than its bucket size, so all four come back. The node whose id is
// the target is at distance 0, so it comes first.
func TestFindNodeReturnsClosest(t *testing.T) {
	tn := startTestnet(t, 5)
	addr := nodeAddr(t, tn.Nodes[0])
	targetAddr := nodeAddr(t, tn.Nodes[3])
	targetID := idOfAddr(targetAddr)
	client, _ := newIO(t, noReply)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := client.Request(ctx, addr, Request{Internal: true, Command: cmdFindNode, Target: targetID[:]})
	checkImplemented(t, "Request", err)
	if err != nil {
		t.Fatalf("FIND_NODE: %v", err)
	}
	if resp.Error != 0 {
		t.Errorf("FIND_NODE reply error = %d, want 0", resp.Error)
	}
	if len(resp.CloserNodes) != 4 {
		t.Fatalf("FIND_NODE returned %d closer nodes, want 4", len(resp.CloserNodes))
	}
	if got, want := resp.CloserNodes[0], dhtAddr(targetAddr); got != want {
		t.Errorf("first closer node is %v:%d, want the target node %v:%d", got.Host, got.Port, want.Host, want.Port)
	}
	others := map[Addr]bool{}
	for _, n := range tn.Nodes[1:] {
		others[dhtAddr(nodeAddr(t, n))] = true
	}
	for _, a := range resp.CloserNodes {
		if !others[a] {
			t.Errorf("closer node %v:%d is not one of the other nodes", a.Host, a.Port)
		}
		delete(others, a)
	}
	if len(others) != 0 {
		t.Errorf("%d other nodes are missing from the closer nodes", len(others))
	}
}

// Test case 4: a request for a command registered with Handle gets the handler's reply, and the
// handler sees the request's value.
func TestHandleAnswersCustomCommand(t *testing.T) {
	const cmd = 42
	node := newNode(t, Config{})
	got := make(chan []byte, 10)
	registerHandler(t, node, cmd, func(req *Request) *Response {
		got <- bytes.Clone(req.Value)
		return &Response{Value: []byte("pong")}
	})
	addr := nodeAddr(t, node)
	client, _ := newIO(t, noReply)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := client.Request(ctx, addr, Request{Command: cmd, Value: []byte("ping")})
	checkImplemented(t, "Request", err)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if string(resp.Value) != "pong" {
		t.Errorf("reply value = %q, want %q", resp.Value, "pong")
	}
	select {
	case v := <-got:
		if string(v) != "ping" {
			t.Errorf("handler got value %q, want %q", v, "ping")
		}
	default:
		t.Error("handler did not run")
	}
}

// A handler that replies with NoToken gets a reply with no token. A reply that does not set it gets one.
func TestNoTokenReplyCarriesNoToken(t *testing.T) {
	const bare, plain = 43, 44
	node := newNode(t, Config{})
	registerHandler(t, node, bare, func(*Request) *Response { return &Response{NoToken: true} })
	registerHandler(t, node, plain, func(*Request) *Response { return &Response{} })
	addr := nodeAddr(t, node)
	client, _ := newIO(t, noReply)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := client.Request(ctx, addr, Request{Command: bare})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if resp.Token != nil {
		t.Error("reply with NoToken carries a token")
	}
	resp, err = client.Request(ctx, addr, Request{Command: plain})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if resp.Token == nil {
		t.Error("reply without NoToken carries no token")
	}
}

// A relay passes a request on with Relay, and the node it reaches answers the client with ReplyTo under
// the client's tid, so the client gets the answer to its own request. Each handler sees the sender in From.
func TestRelayAndReplyToAnswerClient(t *testing.T) {
	const cmd = 45
	middle := newNode(t, Config{})
	server := newNode(t, Config{})
	middleAddr := nodeAddr(t, middle)
	serverAddr := nodeAddr(t, server)
	registerHandler(t, middle, cmd, func(req *Request) *Response {
		middle.Relay(serverAddr, Request{Tid: req.Tid, Command: cmd, Value: []byte(req.From.String())})
		return nil
	})
	registerHandler(t, server, cmd, func(req *Request) *Response {
		if req.From.Port != middleAddr.Port {
			t.Errorf("relayed request came from port %d, want the relay's %d", req.From.Port, middleAddr.Port)
		}
		client, err := net.ResolveUDPAddr("udp4", string(req.Value))
		if err != nil {
			t.Errorf("client address: %v", err)
			return nil
		}
		server.ReplyTo(client, Response{Tid: req.Tid, Value: []byte("answer")})
		return nil
	})
	client, _ := newIO(t, noReply)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := client.Request(ctx, middleAddr, Request{Command: cmd, Value: []byte("ask")})
	if err != nil {
		t.Fatalf("Request through the relay: %v", err)
	}
	if string(resp.Value) != "answer" {
		t.Errorf("reply value = %q, want %q", resp.Value, "answer")
	}
}

// Test case 5: when a node closes, its peers drop it from their routing tables. A DOWN_HINT that names
// the node makes each peer ping it, and the failed ping removes it. The test allows 15 s, and upstream
// removes the node about 4 s after the hint.
func TestClosedNodeDroppedByPeers(t *testing.T) {
	tn := startTestnet(t, 5)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for i, n := range tn.Nodes {
		err := n.Ready(ctx)
		checkImplemented(t, "Ready", err)
		if err != nil {
			t.Fatalf("node %d Ready: %v", i, err)
		}
	}
	closed := tn.Nodes[4]
	closedAddr := nodeAddr(t, closed)
	closedID := idOfAddr(closedAddr)
	err := closed.Close()
	checkImplemented(t, "Close", err)
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	client, _ := newIO(t, noReply)
	hint := downHintValue(closedAddr)
	others := tn.Nodes[:4]
	for i, n := range others {
		resp, err := client.Request(ctx, nodeAddr(t, n), Request{Internal: true, Command: cmdDownHint, Value: hint})
		if err != nil {
			t.Fatalf("DOWN_HINT to node %d: %v", i, err)
		}
		if resp.Error != 0 {
			t.Errorf("DOWN_HINT reply from node %d has error %d, want 0", i, resp.Error)
		}
	}
	for i, n := range others {
		deadline := time.Now().Add(15 * time.Second)
		for hasPeer(nodePeers(t, n), closedID) {
			if time.Now().After(deadline) {
				t.Fatalf("node %d still holds the closed node 15 s after DOWN_HINT", i)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// PING_NAT is an extra case, not one of the five. The reply goes to the port the request names, not to
// the port it came from, and it carries the request's tid.
func TestPingNATRepliesToNamedPort(t *testing.T) {
	node := newNode(t, Config{})
	nodeUDP := nodeAddr(t, node)
	sender := listen(t)
	named := listen(t)
	namedAddr := addrOf(named)
	const tid = 7
	value := binary.LittleEndian.AppendUint16(nil, uint16(namedAddr.Port))
	pkt := EncodeRequest(Request{Tid: tid, To: dhtAddr(nodeUDP), Internal: true, Command: cmdPingNAT, Value: value})
	if _, err := sender.WriteTo(pkt, nodeUDP); err != nil {
		t.Fatal(err)
	}
	named.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 2048)
	n, _, err := named.ReadFrom(buf)
	if err != nil {
		t.Fatalf("no reply on the named port: %v", err)
	}
	v, err := Decode(buf[:n])
	if err != nil {
		t.Fatalf("reply does not decode: %v", err)
	}
	resp, ok := v.(*Response)
	if !ok {
		t.Fatalf("datagram decodes to %T, want *Response", v)
	}
	if resp.Tid != tid {
		t.Errorf("reply tid = %d, want %d", resp.Tid, tid)
	}
	if want := dhtAddr(namedAddr); resp.To != want {
		t.Errorf("reply To = %v:%d, want %v:%d", resp.To.Host, resp.To.Port, want.Host, want.Port)
	}
	if resp.Error != 0 {
		t.Errorf("PING_NAT reply error = %d, want 0", resp.Error)
	}
}

// Test case: a query whose replies arrive while the node becomes persistent does not race on the node's id. The
// reply handling of the query reads the id to skip the node itself, and becomePersistent rewrites it when a probe
// passes. Under the race detector (go test -race) the unlocked read raced with that write; without it, the test
// checks that the query still walks and that the node ends persistent. Each call gets its own sampler, as the probe
// of bootstrap does: becomePersistent makes the sampler the node's own.
func TestQueryWhileNodeBecomesPersistent(t *testing.T) {
	tn := startTestnet(t, 8)
	n := newNode(t, Config{Bootstrap: tn.Bootstrap})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	q := n.Query(ctx, QueryOpts{Target: queryTarget(), Command: cmdFindNode, Internal: true})
	checkStarted(t, q)
	port := nodeAddr(t, n).Port
	for range 50 {
		probe := newNATSampler(port)
		for range 4 {
			probe.add("127.0.0.1", port)
		}
		n.becomePersistent(probe)
		runtime.Gosched()
	}
	drainQuery(t, q)
	if n.ID() == nil {
		t.Fatal("the node has no id after becoming persistent")
	}
}
