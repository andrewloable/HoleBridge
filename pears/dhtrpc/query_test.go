package dhtrpc

import (
	"bytes"
	"context"
	"errors"
	"net"
	"slices"
	"sync"
	"testing"
	"time"
)

// Upstream's budget for a query against unresponsive nodes, measured with dht-rpc 6.27.0 under node 24:
// a query with 3 silent peers in its bootstrap list finished 6.0 s after it started. The query's
// requests use query.js's default of 5 retries, so each is sent 6 times; io.js waits 1000 ms per send
// when no adaptive timeout is set.
const (
	upstreamQueryRetries = 5
	upstreamSendTimeout  = time.Second
	budgetGrace          = time.Second // slack for a loaded machine
)

// queryTarget returns the 32-byte id that the query tests walk towards. It is fixed, so a failure
// reproduces.
func queryTarget() []byte {
	target := make([]byte, 32)
	for i := range target {
		target[i] = byte(7*i + 3)
	}
	return target
}

// checkStarted fails the test when Query returned nil, which is the stub's signal.
func checkStarted(t *testing.T, q *Query) {
	t.Helper()
	if q == nil {
		t.Fatal("Query: not implemented")
	}
}

// drainAsync reads q in its own goroutine until Next reports the end. The returned channel closes then.
func drainAsync(q *Query) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, ok := q.Next(); !ok {
				return
			}
		}
	}()
	return done
}

// drainQuery waits for the query to end, within 30 s, then checks its error. The stub reports
// errors.ErrUnsupported, so the test says not implemented.
func drainQuery(t *testing.T, q *Query) {
	t.Helper()
	select {
	case <-drainAsync(q):
	case <-time.After(30 * time.Second):
		t.Fatal("query did not end within 30 s")
	}
	err := q.Err()
	checkImplemented(t, "Query", err)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
}

// checkClosestAre fails the test unless closest holds exactly the nodes at want, in any order.
func checkClosestAre(t *testing.T, closest []Reply, want []*net.UDPAddr) {
	t.Helper()
	if len(closest) != len(want) {
		t.Fatalf("closest has %d replies, want %d", len(closest), len(want))
	}
	wanted := map[string]bool{}
	for _, a := range want {
		wanted[a.String()] = true
	}
	for _, r := range closest {
		if !wanted[r.From.String()] {
			t.Errorf("closest reply from %v is not one of the expected nodes", r.From)
		}
	}
}

// xorDistance returns the XOR distance from target to the dht-rpc id of addr, as 32 big-endian bytes.
func xorDistance(target []byte, addr *net.UDPAddr) []byte {
	id := idOfAddr(addr)
	d := make([]byte, len(id))
	for i := range id {
		d[i] = target[i] ^ id[i]
	}
	return d
}

// bruteForceClosest returns addrs sorted by XOR distance from target, nearest first. It is the reference
// the query is checked against.
func bruteForceClosest(target []byte, addrs []*net.UDPAddr) []*net.UDPAddr {
	sorted := slices.Clone(addrs)
	slices.SortFunc(sorted, func(a, b *net.UDPAddr) int {
		return bytes.Compare(xorDistance(target, a), xorDistance(target, b))
	})
	return sorted
}

// Test case 1: on a 20-node testnet, a FIND_NODE query for a target returns the 20 closest nodes. The
// brute-force check sorts the 20 node addresses by XOR distance from the target, and the closest replies
// must be those 20 nodes in that order.
func TestQueryFindNodeReturnsClosest(t *testing.T) {
	tn := startTestnet(t, 20)
	client := newNode(t, Config{Bootstrap: tn.Bootstrap})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	target := queryTarget()
	q := client.Query(ctx, QueryOpts{Target: target, Command: cmdFindNode, Internal: true})
	checkStarted(t, q)
	drainQuery(t, q)
	var addrs []*net.UDPAddr
	for _, n := range tn.Nodes {
		addrs = append(addrs, nodeAddr(t, n))
	}
	want := bruteForceClosest(target, addrs)
	closest := q.Closest()
	if len(closest) != 20 {
		t.Fatalf("closest has %d replies, want 20", len(closest))
	}
	for i, r := range closest {
		if r.From.String() != want[i].String() {
			t.Errorf("closest[%d] is %v, want %v", i, r.From, want[i])
		}
	}
}

// Test case 2: a query with a Commit function calls it on the closest replies. Each closest reply is
// committed once, and no other node is committed.
func TestQueryCommitsClosestReplies(t *testing.T) {
	tn := startTestnet(t, 5)
	client := newNode(t, Config{Bootstrap: tn.Bootstrap})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var mu sync.Mutex
	committed := map[string]int{}
	q := client.Query(ctx, QueryOpts{
		Target:   queryTarget(),
		Command:  cmdFindNode,
		Internal: true,
		Commit: func(_ context.Context, r Reply) error {
			mu.Lock()
			defer mu.Unlock()
			committed[r.From.String()]++
			return nil
		},
	})
	checkStarted(t, q)
	drainQuery(t, q)
	closest := q.Closest()
	var addrs []*net.UDPAddr
	for _, n := range tn.Nodes {
		addrs = append(addrs, nodeAddr(t, n))
	}
	checkClosestAre(t, closest, addrs)
	mu.Lock()
	defer mu.Unlock()
	for _, r := range closest {
		if n := committed[r.From.String()]; n != 1 {
			t.Errorf("closest reply from %v was committed %d times, want once", r.From, n)
		}
	}
	if len(committed) != len(closest) {
		t.Errorf("Commit was called for %d nodes, want the %d closest", len(committed), len(closest))
	}
}

// Test case 3: a query whose context is cancelled stops, and Err returns the context error. The only
// bootstrap node is silent, so the query is still waiting on it when the context is cancelled.
func TestQueryCancelStopsWithContextError(t *testing.T) {
	silent, _ := silentPeer(t)
	client := newNode(t, Config{Bootstrap: []string{silent.String()}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := client.Query(ctx, QueryOpts{Target: queryTarget(), Command: cmdFindNode, Internal: true})
	checkStarted(t, q)
	time.AfterFunc(500*time.Millisecond, cancel)
	select {
	case <-drainAsync(q):
	case <-time.After(3 * time.Second):
		t.Fatal("query did not stop within 3 s of cancel")
	}
	err := q.Err()
	checkImplemented(t, "Query", err)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Err = %v, want context.Canceled", err)
	}
}

// Test case 4: unresponsive nodes do not stall the query beyond the upstream timeout budget. Three silent
// peers sit in the client's bootstrap list next to the 5 testnet nodes. The query ends when its requests
// have all ended, and a silent peer's request ends after the upstream budget. The closest replies are
// the 5 live nodes.
func TestQueryUnresponsiveNodesWithinBudget(t *testing.T) {
	tn := startTestnet(t, 5)
	boot := append([]string{}, tn.Bootstrap...)
	for i := 0; i < 3; i++ {
		addr, _ := silentPeer(t)
		boot = append(boot, addr.String())
	}
	client := newNode(t, Config{Bootstrap: boot})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	q := client.Query(ctx, QueryOpts{Target: queryTarget(), Command: cmdFindNode, Internal: true})
	checkStarted(t, q)
	drainQuery(t, q)
	elapsed := time.Since(start)
	budget := (upstreamQueryRetries + 1) * upstreamSendTimeout
	if elapsed > budget+budgetGrace {
		t.Errorf("query took %v, want at most %v (the upstream budget %v plus %v grace)", elapsed, budget+budgetGrace, budget, budgetGrace)
	}
	var live []*net.UDPAddr
	for _, n := range tn.Nodes {
		live = append(live, nodeAddr(t, n))
	}
	checkClosestAre(t, q.Closest(), live)
}

// namingPeer listens on a free port and answers every FIND_NODE request with closer as its only closer
// node, as a peer whose table holds closer does. It reports each datagram it receives on the returned
// channel.
func namingPeer(t *testing.T, closer *net.UDPAddr) (*net.UDPAddr, <-chan []byte) {
	t.Helper()
	conn := listen(t)
	got := make(chan []byte, 100)
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			pkt := bytes.Clone(buf[:n])
			got <- pkt
			v, err := Decode(pkt)
			if req, ok := v.(*Request); ok && err == nil && req.Internal && req.Command == cmdFindNode {
				reply(conn, from.(*net.UDPAddr), Response{Tid: req.Tid, CloserNodes: []Addr{dhtAddr(closer)}})
			}
		}
	}()
	return addrOf(conn), got
}

// gotDownHint reports whether a DOWN_HINT carrying value arrives on got within wait.
func gotDownHint(got <-chan []byte, value []byte, wait time.Duration) bool {
	deadline := time.After(wait)
	for {
		select {
		case pkt := <-got:
			v, err := Decode(pkt)
			if req, ok := v.(*Request); ok && err == nil && req.Internal && req.Command == cmdDownHint && bytes.Equal(req.Value, value) {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

// Test case 5: when a node that a reply named as closer times out, the query sends DOWN_HINT to the node
// that named it, with the timed-out node's address as the value. The referrer answers FIND_NODE with the
// silent peer as its only closer node, and records the datagrams it receives.
func TestQueryTimeoutSendsDownHint(t *testing.T) {
	silent, _ := silentPeer(t)
	referrer, got := namingPeer(t, silent)
	client := newNode(t, Config{Bootstrap: []string{referrer.String()}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	q := client.Query(ctx, QueryOpts{Target: queryTarget(), Command: cmdFindNode, Internal: true})
	checkStarted(t, q)
	drainQuery(t, q)
	if !gotDownHint(got, downHintValue(silent), 2*time.Second) {
		t.Error("the referrer got no DOWN_HINT naming the silent peer after the query timed it out")
	}
}

// Test case 6: a query ends once every request in flight has waited one resend cycle and the closest set
// is full, as upstream's early end does. Twenty live nodes fill the closest set. The silent peer is the
// last bootstrap node, so the walk asks it first, and it is the only request in flight when the set
// fills. The query ends after that one cycle (1 s), not after the silent peer's whole budget (6 s).
func TestQueryEndsEarlyWhenOnlySlowRequestsRemain(t *testing.T) {
	tn := startTestnet(t, 20)
	silent, _ := silentPeer(t)
	boot := append(append([]string{}, tn.Bootstrap...), silent.String())
	client := newNode(t, Config{Bootstrap: boot})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	q := client.Query(ctx, QueryOpts{Target: queryTarget(), Command: cmdFindNode, Internal: true})
	checkStarted(t, q)
	drainQuery(t, q)
	elapsed := time.Since(start)
	if limit := upstreamSendTimeout + 3*time.Second; elapsed > limit {
		t.Errorf("query took %v, want at most %v: its closest set was full and only the silent peer was in flight", elapsed, limit)
	}
	var live []*net.UDPAddr
	for _, n := range tn.Nodes {
		live = append(live, nodeAddr(t, n))
	}
	checkClosestAre(t, q.Closest(), live)
}

// Test case 7: when the closest set is not full, the query waits for its silent requests and does not end
// early. Five live nodes cannot fill a set of 20, so the query waits out the silent peer's budget of 6 s.
func TestQueryWaitsForSlowRequestsWhenSetNotFull(t *testing.T) {
	tn := startTestnet(t, 5)
	silent, _ := silentPeer(t)
	boot := append(append([]string{}, tn.Bootstrap...), silent.String())
	client := newNode(t, Config{Bootstrap: boot})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	q := client.Query(ctx, QueryOpts{Target: queryTarget(), Command: cmdFindNode, Internal: true})
	checkStarted(t, q)
	drainQuery(t, q)
	elapsed := time.Since(start)
	budget := (upstreamQueryRetries + 1) * upstreamSendTimeout
	if elapsed < budget-budgetGrace {
		t.Errorf("query took %v, want at least %v: the silent peer's budget, since the closest set is not full", elapsed, budget-budgetGrace)
	}
}
