package dhtrpc

import (
	"context"
	"net"
	"slices"
	"testing"
	"time"
)

// natHost is an address from TEST-NET-3 (RFC 5737), so the sampler tests use no real host. Expected
// values come from upstream dht-rpc 6.27.0 (index.js) and nat-sampler 1.0.1, run with node 24.
const natHost = "203.0.113.7"

// natReport is one 'to' address that a peer reported for us.
type natReport struct {
	host string
	port int
}

// sampleNAT feeds a new sampler for a node on port with the reports and the pings, and returns its NAT
// info. The stubs panic, so the panic becomes a test failure here.
func sampleNAT(t *testing.T, port int, reports []natReport, pings []string) NATInfo {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("NAT sampler: %v", r)
		}
	}()
	s := newNATSampler(port)
	for _, r := range reports {
		s.add(r.host, r.port)
	}
	for _, h := range pings {
		s.unsolicitedPing(h)
	}
	return s.info()
}

// natOf returns n.NAT. The stub panics, so the panic becomes a test failure here.
func natOf(t *testing.T, n *Node) NATInfo {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("NAT: %v", r)
		}
	}()
	return n.NAT()
}

// waitNAT polls n.NAT until it names a host or wait ends, and returns the last reading.
func waitNAT(t *testing.T, n *Node, wait time.Duration) NATInfo {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		info := natOf(t, n)
		if info.Host != "" || time.Now().After(deadline) {
			return info
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Test case 1: five samples report 203.0.113.7 on port 5000, and the sampler listens on 5000. Upstream
// clears the firewall only when a datagram from a host that answered its probe reaches the server
// socket (dht-rpc _checkIfFirewalled), so the test also sends one unasked ping from that host.
func TestSamplesConsistentNotFirewalled(t *testing.T) {
	var reports []natReport
	for range 5 {
		reports = append(reports, natReport{natHost, 5000})
	}
	info := sampleNAT(t, 5000, reports, []string{natHost})
	if info.Host != natHost || info.Port != 5000 {
		t.Errorf("NAT = %s:%d, want %s:5000", info.Host, info.Port, natHost)
	}
	if info.Firewalled {
		t.Error("Firewalled = true, want false")
	}
	if info.Randomized {
		t.Error("Randomized = true, want false")
	}
}

// Test case 2: five samples report one host on five different ports. Upstream nat-sampler then reports
// that host with port 0, which dht-rpc calls randomized.
func TestSamplesSameHostRandomized(t *testing.T) {
	var reports []natReport
	for i := range 5 {
		reports = append(reports, natReport{natHost, 40001 + i})
	}
	info := sampleNAT(t, 5000, reports, nil)
	if info.Host != natHost || info.Port != 0 {
		t.Errorf("NAT = %s:%d, want %s:0", info.Host, info.Port, natHost)
	}
	if !info.Randomized {
		t.Error("Randomized = false, want true")
	}
}

// Test case 3: on a localhost testnet of five nodes, each node reports 127.0.0.1 and its own port. Upstream,
// measured with dht-rpc 6.27.0, gives the same result for persistent nodes on 127.0.0.1 by the time
// each is ready.
func TestTestnetNodeReportsLoopback(t *testing.T) {
	tn := startTestnet(t, 5)
	for i, n := range tn.Nodes {
		info := waitNAT(t, n, 10*time.Second)
		want := nodeAddr(t, n).Port
		if info.Host != "127.0.0.1" || info.Port != want {
			t.Errorf("node %d NAT = %s:%d, want 127.0.0.1:%d", i, info.Host, info.Port, want)
		}
	}
}

// Test case 4: a node that never receives an unsolicited ping stays firewalled. Upstream returns
// firewalled from _checkIfFirewalled unless replies arrive on the server socket from hosts that it
// names, so consistent samples alone do not clear the firewall. A sampler that has seen nothing is
// firewalled too.
func TestSamplesWithoutPingStayFirewalled(t *testing.T) {
	if info := sampleNAT(t, 5000, nil, nil); !info.Firewalled {
		t.Error("no samples: Firewalled = false, want true")
	}
	var reports []natReport
	for range 5 {
		reports = append(reports, natReport{natHost, 5000})
	}
	if info := sampleNAT(t, 5000, reports, nil); !info.Firewalled {
		t.Error("consistent samples and no ping: Firewalled = false, want true")
	}
}

// Test case 5 (added for the wildcard note on HoleBridge-85m.2.10): New listens on all interfaces, so a
// node made with Ephemeral false stays ephemeral until NAT sampling lands. After it lands, the node is
// not firewalled, its NAT names 127.0.0.1 and its port, and the bootstrap node holds its persistent id,
// the dht-rpc id of that address. Upstream, measured with dht-rpc 6.27.0 on localhost with forced
// persistence, is not firewalled and not ephemeral at ready, and the bootstrap node's table holds its id.
func TestWildcardNodeBecomesPersistent(t *testing.T) {
	tn := startTestnet(t, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	notEphemeral := false
	n := newNode(t, Config{Bootstrap: tn.Bootstrap, Ephemeral: &notEphemeral})
	err := n.Ready(ctx)
	checkImplemented(t, "Ready", err)
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	info := waitNAT(t, n, 10*time.Second)
	if info.Firewalled {
		t.Error("Firewalled = true after NAT sampling, want false")
	}
	want := nodeAddr(t, n).Port
	if info.Host != "127.0.0.1" || info.Port != want {
		t.Fatalf("NAT = %s:%d, want 127.0.0.1:%d", info.Host, info.Port, want)
	}
	id := idOfAddr(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: info.Port})
	deadline := time.Now().Add(15 * time.Second)
	for !hasPeer(nodePeers(t, tn.Nodes[0]), id) {
		if time.Now().After(deadline) {
			t.Fatal("bootstrap node does not hold the persistent id 15 s after Ready")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Extra case: the sampler past its ring of 32 samples, where nat-sampler overwrites the oldest slots.
// The expected values come from nat-sampler 1.0.1 run with node 24, with the same sequences.
func TestSamplerRingMatchesUpstream(t *testing.T) {
	consistent := func(n int) []natReport {
		var r []natReport
		for range n {
			r = append(r, natReport{natHost, 5000})
		}
		return r
	}
	distinct := func(n int) []natReport {
		var r []natReport
		for i := range n {
			r = append(r, natReport{natHost, 40001 + i})
		}
		return r
	}
	cases := []struct {
		name    string
		reports []natReport
		port    int // 0 means randomized
	}{
		{"40 consistent", consistent(40), 5000},
		{"20 distinct then 30 consistent", slices.Concat(distinct(20), consistent(30)), 5000},
		{"20 consistent then 20 distinct", slices.Concat(consistent(20), distinct(20)), 0},
		{"16 consistent then 40 distinct", slices.Concat(consistent(16), distinct(40)), 0},
	}
	for _, c := range cases {
		info := sampleNAT(t, 5000, c.reports, nil)
		if info.Host != natHost || info.Port != c.port || info.Randomized != (c.port == 0) {
			t.Errorf("%s: NAT = %s:%d randomized %v, want %s:%d", c.name, info.Host, info.Port, info.Randomized, natHost, c.port)
		}
	}
}

// Extra case: a New node with Ephemeral false and no bootstrap nodes has no addresses to probe, so it
// stays ephemeral. Upstream stays ephemeral here too.
func TestNewWithoutPeersStaysEphemeral(t *testing.T) {
	notEphemeral := false
	n := newNode(t, Config{Ephemeral: &notEphemeral})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := n.Ready(ctx)
	checkImplemented(t, "Ready", err)
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if n.selfID() != nil {
		t.Error("node with no peers became persistent, want ephemeral")
	}
}
