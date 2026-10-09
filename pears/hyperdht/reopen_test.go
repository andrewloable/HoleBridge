package hyperdht

// Expected values come from hyperdht 6.34.1 lib/holepuncher.js: analyze(allowReopen) and _reopen reopen an unstable
// socket on a fresh socket, at most MAX_REOPENS (3) times, and sample its NAT from each new socket; _unstable is a NAT
// that randomizes while the peer randomizes too, or a NAT whose firewall state is still unknown. lib/connect.js
// probeRound calls analyze(false), then analyze(true) only when that is unstable, and probes again when the reopen made
// the socket stable.

import (
	"context"
	"net/netip"
	"sync"
	"testing"
)

// reopenObservers are the four nodes that the reopen tests take their NAT samples from.
var reopenObservers = []Address{addr("198.51.100.1", 6881), addr("198.51.100.2", 6881), addr("198.51.100.3", 6881), addr("198.51.100.4", 6881)}

// reopenPool is the punch pool of the reopen tests. Acquire hands out the sockets of next in order, then the sockets of
// fallback, and it keeps the sockets it handed out. Release keeps the sockets given back.
type reopenPool struct {
	fallback punchPool
	mu       sync.Mutex
	next     []punchSocket
	got      []punchSocket
	released []punchSocket
}

// Acquire returns the next socket of the pool.
func (r *reopenPool) Acquire() punchSocket {
	r.mu.Lock()
	defer r.mu.Unlock()
	var s punchSocket
	if len(r.next) > 0 {
		s, r.next = r.next[0], r.next[1:]
	} else {
		s = r.fallback.Acquire()
	}
	r.got = append(r.got, s)
	return s
}

// Release gives a socket back to the pool.
func (r *reopenPool) Release(s punchSocket) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.released = append(r.released, s)
}

// acquired returns the sockets the pool handed out, in order.
func (r *reopenPool) acquired() []punchSocket {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]punchSocket(nil), r.got...)
}

// wasReleased reports whether s was given back to the pool.
func (r *reopenPool) wasReleased(s punchSocket) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, x := range r.released {
		if x == s {
			return true
		}
	}
	return false
}

// reopenRig is a puncher on a simulated network. Its sockets come from reopenPool: the first comes from the list the
// test sets, and each reopen takes the next one, then a fresh socket behind the randomizing NAT. sampled records the
// socket of each reopen that took NAT samples, in order.
type reopenRig struct {
	sim        *simNet
	random     *simNAT // a randomizing NAT
	consistent *simNAT // a consistent NAT
	host       netip.Addr
	pool       *reopenPool
	p          *holepuncher
	sampled    []punchSocket
}

// newReopenRig returns a rig with no puncher yet. Its pool falls back to fresh sockets behind the randomizing NAT.
func newReopenRig(t *testing.T) *reopenRig {
	t.Helper()
	sim := newSimNet()
	r := &reopenRig{
		sim:        sim,
		random:     sim.addNAT(netip.MustParseAddr("203.0.113.10"), natRandomized),
		consistent: sim.addNAT(netip.MustParseAddr("203.0.113.20"), natConsistent),
		host:       netip.MustParseAddr("192.168.1.10"),
	}
	r.pool = &reopenPool{fallback: r.random.pool(r.host)}
	return r
}

// start makes the rig's puncher for a peer with the firewall state remote, and takes the NAT samples of its first
// socket. Its Sample hook takes the samples of each reopened socket from reopenObservers, and records the socket.
func (r *reopenRig) start(t *testing.T, remote uint64) {
	t.Helper()
	cfg := punchConfig{
		Pool:           r.pool,
		Initiator:      true,
		RemoteFirewall: remote,
		Sample: func(ctx context.Context, sock punchSocket, p *holepuncher) error {
			r.sampled = append(r.sampled, sock)
			sampleNAT(t, r.sim, p, sock.(*simSocket), reopenObservers)
			return nil
		},
	}
	r.p = newPuncher(t, cfg)
	sampleNAT(t, r.sim, r.p, r.pool.acquired()[0].(*simSocket), reopenObservers)
}

// analyzeOf calls p.analyze and returns whether the NAT is stable. It fails the test as not implemented while the stub
// stands, and on any error.
func analyzeOf(t *testing.T, p *holepuncher, allowReopen bool) bool {
	t.Helper()
	var stable bool
	var err error
	callStub(t, "analyze", func() { stable, err = p.analyze(context.Background(), allowReopen) })
	failIfStub(t, err)
	must(t, err)
	return stable
}

// Test case 1: an unstable socket is left alone when the caller does not allow a reopen. The peer randomizes and the
// NAT in front of the socket randomizes, so analyze(false) reports it unstable and takes no socket.
func TestAnalyzeLeavesUnstableSocketWhenReopenIsNotAllowed(t *testing.T) {
	r := newReopenRig(t)
	r.pool.next = []punchSocket{r.random.addSocket(r.host, 40001), r.consistent.addSocket(r.host, 40002)}
	r.start(t, firewallRandom)
	if analyzeOf(t, r.p, false) {
		t.Error("analyze(false) reports a randomized socket as stable")
	}
	if n := len(r.pool.acquired()); n != 1 {
		t.Errorf("analyze(false) took %d sockets, want the first one only", n)
	}
	if len(r.sampled) != 0 {
		t.Errorf("analyze(false) sampled %d reopened sockets, want none", len(r.sampled))
	}
}

// Test case 2: with the reopen allowed, the unstable socket moves onto a fresh socket, and the NAT is sampled from that
// fresh socket alone. The puncher probes from the fresh socket afterwards, and the old socket goes back to the pool.
func TestAnalyzeReopensOntoFreshSocketSampledFromIt(t *testing.T) {
	r := newReopenRig(t)
	first := r.random.addSocket(r.host, 40001)
	fresh := r.consistent.addSocket(r.host, 40002)
	r.pool.next = []punchSocket{first, fresh}
	r.start(t, firewallRandom)
	if !analyzeOf(t, r.p, true) {
		t.Fatal("analyze(true) reports the NAT unstable after a reopen onto a consistent socket")
	}
	if got := r.pool.acquired(); len(got) != 2 || got[1] != punchSocket(fresh) {
		t.Fatalf("the reopen took %d sockets, want the first and then the fresh one", len(got))
	}
	if sock := r.p.probeSocket(); sock != punchSocket(fresh) {
		t.Error("after a reopen the puncher does not probe from the fresh socket")
	}
	if fw := r.p.natFirewall(); fw != firewallConsistent {
		t.Errorf("the NAT firewall after a reopen is %d, want consistent (%d)", fw, firewallConsistent)
	}
	if len(r.sampled) != 1 || r.sampled[0] != punchSocket(fresh) {
		t.Errorf("the reopen sampled %d sockets, want the fresh one only", len(r.sampled))
	}
	if !r.pool.wasReleased(first) {
		t.Error("the unstable socket was not given back to the pool when the puncher reopened")
	}
}

// Test case 3: a NAT that stays unstable is reopened at most MAX_REOPENS (3) times, and then analyze reports it unstable.
// A second analyze(true) gives the same answer and takes no more sockets: the reopen runs once.
func TestAnalyzeGivesUpAfterThreeReopens(t *testing.T) {
	r := newReopenRig(t)
	r.pool.next = []punchSocket{r.random.addSocket(r.host, 40001)}
	r.start(t, firewallRandom)
	if analyzeOf(t, r.p, true) {
		t.Fatal("analyze(true) reports a stable NAT after reopens onto randomized sockets")
	}
	if n := len(r.pool.acquired()); n != 1+3 {
		t.Errorf("analyze(true) took %d sockets, want the first and three reopens (4)", n)
	}
	if len(r.sampled) != 3 {
		t.Errorf("analyze(true) sampled %d reopened sockets, want 3", len(r.sampled))
	}
	if analyzeOf(t, r.p, true) {
		t.Error("a second analyze(true) reports a stable NAT")
	}
	if n := len(r.pool.acquired()); n != 1+3 {
		t.Errorf("a second analyze(true) took sockets: %d in all, want still 4", n)
	}
}

// Test case 4: a socket that is stable is not reopened. A consistent NAT is stable whatever the peer, and a randomized
// NAT is stable when the peer is consistent (the birthday punch covers it).
func TestAnalyzeStableSocketIsNotReopened(t *testing.T) {
	t.Run("a consistent NAT", func(t *testing.T) {
		r := newReopenRig(t)
		r.pool.next = []punchSocket{r.consistent.addSocket(r.host, 40001)}
		r.start(t, firewallRandom)
		if !analyzeOf(t, r.p, true) {
			t.Error("analyze(true) reports a consistent NAT unstable")
		}
		if n := len(r.pool.acquired()); n != 1 {
			t.Errorf("analyze(true) took %d sockets for a stable NAT, want the first one only", n)
		}
		if len(r.sampled) != 0 {
			t.Errorf("analyze(true) sampled %d reopened sockets for a stable NAT, want none", len(r.sampled))
		}
	})
	t.Run("a randomized NAT and a consistent peer", func(t *testing.T) {
		r := newReopenRig(t)
		r.pool.next = []punchSocket{r.random.addSocket(r.host, 40001)}
		r.start(t, firewallConsistent)
		if !analyzeOf(t, r.p, true) {
			t.Error("analyze(true) reports a randomized NAT unstable when the peer is consistent")
		}
		if n := len(r.pool.acquired()); n != 1 {
			t.Errorf("analyze(true) took %d sockets when the peer is consistent, want the first one only", n)
		}
	})
}
