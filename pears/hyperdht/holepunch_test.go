package hyperdht

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/blake2b"

	"github.com/andrewloable/HoleBridge/pears/dhtrpc"
	"github.com/andrewloable/HoleBridge/pears/noise"
)

// Expected values come from hyperdht 6.34.1 (spec/gen/node_modules/hyperdht/lib/holepuncher.js, nat.js,
// connect.js, router.js and server.js), run under node 24.15.0 against an in-memory NAT model of the same
// behaviour (natsim_test.go). The figures are noted at each test.

// punchConnect is the time a connect may take in these tests. Upstream's consistent pair connects in about a
// second, and a consistent and a randomizing pair in about 7 s; the random probe budget alone is 35 s.
const punchConnect = 60 * time.Second

// sampleObservers are four DHT nodes on the documentation network 203.0.113.0/24. Four pings of a punching
// socket give four samples, as upstream's nat.js autoSample collects them.
var sampleObservers = []Address{
	addr("203.0.113.11", 6881),
	addr("203.0.113.12", 6881),
	addr("203.0.113.13", 6881),
	addr("203.0.113.14", 6881),
}

// addr returns the wire address of host and port, for the documentation ranges these tests use.
func addr(host string, port uint16) Address {
	return Address{Host: netip.MustParseAddr(host), Port: port}
}

// callStub runs f and fails the test with "not implemented" when f panics, which is how the stubs of holepunch.go
// report a function that has no body yet.
func callStub(t *testing.T, what string, f func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s: not implemented", what)
		}
	}()
	f()
}

// newPuncher makes a holepuncher with cfg, and stops it when the test ends.
func newPuncher(t *testing.T, cfg punchConfig) *holepuncher {
	t.Helper()
	var p *holepuncher
	callStub(t, "newHolepuncher", func() { p = newHolepuncher(cfg) })
	t.Cleanup(func() {
		defer func() { _ = recover() }() // a stub has nothing to stop
		p.destroy()
	})
	return p
}

// recordingPool is a punch pool that keeps the sockets it hands out, in order. The first one is the socket its
// puncher punches from and takes its NAT samples on.
type recordingPool struct {
	pool punchPool
	mu   sync.Mutex
	got  []*simSocket
}

// Acquire returns a socket from the pool and records it.
func (r *recordingPool) Acquire() punchSocket {
	s := r.pool.Acquire().(*simSocket)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, s)
	return s
}

// Release gives a socket back to the pool.
func (r *recordingPool) Release(s punchSocket) {
	r.pool.Release(s)
}

// sockets returns the sockets handed out so far, in order.
func (r *recordingPool) sockets() []*simSocket {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*simSocket(nil), r.got...)
}

// sampleNAT takes the NAT samples of sock for p: each observer sees the external address the NAT maps for it.
func sampleNAT(t *testing.T, sim *simNet, p *holepuncher, sock *simSocket, observers []Address) {
	t.Helper()
	for _, o := range observers {
		seen := sim.observe(sock, o)
		callStub(t, "observe", func() { p.observe(seen, o) })
	}
}

// connectEvent is one call of a puncher's OnConnect: the socket and the peer's address.
type connectEvent struct {
	sock   punchSocket
	remote *net.UDPAddr
}

// lowTTLCount returns how many distinct sockets of the pool sent a low-TTL datagram to dest.
func lowTTLCount(socks []*simSocket, dest Address) int {
	n := 0
	for _, s := range socks {
		for _, d := range s.datagrams() {
			if d.to == dest && d.ttl == punchTTLLow {
				n++
				break
			}
		}
	}
	return n
}

// Test case 1: two peers behind simulated consistent NATs connect by punching. Both sample one external port
// each, so both sides are consistent. The initiator's probes reach the responder's NAT first, which drops them.
// The responder waits a second and then punches toward the initiator's external address, which opens the way
// back, so the initiator connects with the responder's external address. Upstream connects at about 1.0 s.
// The responder never reports a connect.
func TestPunchConsistentNATsConnect(t *testing.T) {
	sim := newSimNet()
	natA := sim.addNAT(netip.MustParseAddr("198.51.100.1"), natConsistent)
	natB := sim.addNAT(netip.MustParseAddr("198.51.100.2"), natConsistent)
	poolA := &recordingPool{pool: natA.pool(netip.MustParseAddr("192.0.2.10"))}
	poolB := &recordingPool{pool: natB.pool(netip.MustParseAddr("192.0.2.20"))}

	connects := make(chan connectEvent, 4)
	responderConnects := make(chan connectEvent, 4)
	initiator := newPuncher(t, punchConfig{
		Pool:           poolA,
		Initiator:      true,
		RemoteFirewall: firewallConsistent,
		OnConnect:      func(s punchSocket, r *net.UDPAddr) { connects <- connectEvent{s, r} },
	})
	responder := newPuncher(t, punchConfig{
		Pool:           poolB,
		RemoteFirewall: firewallConsistent,
		OnConnect:      func(s punchSocket, r *net.UDPAddr) { responderConnects <- connectEvent{s, r} },
	})
	sockA := poolA.sockets()[0]
	sockB := poolB.sockets()[0]
	sampleNAT(t, sim, initiator, sockA, sampleObservers)
	sampleNAT(t, sim, responder, sockB, sampleObservers)

	var aFw, bFw uint64
	callStub(t, "natFirewall", func() {
		aFw = initiator.natFirewall()
		bFw = responder.natFirewall()
	})
	if aFw != firewallConsistent || bFw != firewallConsistent {
		t.Fatalf("NAT firewalls after four samples are %d and %d, want consistent (%d) on both", aFw, bFw, firewallConsistent)
	}
	var aAddrs, bAddrs []Address
	callStub(t, "natAddresses", func() {
		aAddrs = initiator.natAddresses()
		bAddrs = responder.natAddresses()
	})
	if len(aAddrs) != 1 || len(bAddrs) != 1 || aAddrs[0].Port == 0 || bAddrs[0].Port == 0 {
		t.Fatalf("consistent NATs give addresses %v and %v, want one external address with a port each", aAddrs, bAddrs)
	}
	callStub(t, "updateRemote", func() {
		initiator.updateRemote(firewallConsistent, true, bAddrs, bAddrs[0].Host)
		responder.updateRemote(firewallConsistent, true, aAddrs, aAddrs[0].Host)
	})

	var ok bool
	var err error
	callStub(t, "punch", func() { ok, err = initiator.punch() })
	if err != nil || !ok {
		t.Fatalf("initiator punch = %v, %v, want started with no error", ok, err)
	}
	callStub(t, "punch", func() { ok, err = responder.punch() })
	if err != nil || !ok {
		t.Fatalf("responder punch = %v, %v, want started with no error", ok, err)
	}

	select {
	case ev := <-connects:
		if addressOf(ev.remote) != bAddrs[0] {
			t.Errorf("initiator connected to %v, want the responder's external address %v", ev.remote, bAddrs[0])
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the initiator did not connect by punching within 10 s")
	}
	select {
	case ev := <-responderConnects:
		t.Errorf("the responder reported a connect to %v, want none (upstream reports connects on the initiator only)", ev.remote)
	case <-time.After(1500 * time.Millisecond):
	}
	if len(sockA.datagrams()) == 0 || len(sockB.datagrams()) == 0 {
		t.Error("a side sent no holepunch datagram; both sides punch")
	}
}

// Test case 2: one consistent and one randomizing NAT, behaviour as upstream's punch strategy. The consistent side
// (the initiator) probes random ports of the randomizing side's host with the default TTL, and the randomizing
// side opens 256 sockets, each with its own mapping toward the consistent side, sending low-TTL datagrams. A probe
// that lands on one of those mappings connects. Upstream connects after about 7 s here, and the random probes
// are bounded at 1750, about 35 s.
func TestPunchConsistentAndRandomizedNATs(t *testing.T) {
	sim := newSimNet()
	natA := sim.addNAT(netip.MustParseAddr("198.51.100.1"), natConsistent)
	natB := sim.addNAT(netip.MustParseAddr("198.51.100.2"), natRandomized)
	poolA := &recordingPool{pool: natA.pool(netip.MustParseAddr("192.0.2.10"))}
	poolB := &recordingPool{pool: natB.pool(netip.MustParseAddr("192.0.2.20"))}

	connects := make(chan connectEvent, 4)
	initiator := newPuncher(t, punchConfig{
		Pool:           poolA,
		Initiator:      true,
		RemoteFirewall: firewallRandom,
		OnConnect:      func(s punchSocket, r *net.UDPAddr) { connects <- connectEvent{s, r} },
	})
	responder := newPuncher(t, punchConfig{Pool: poolB, RemoteFirewall: firewallConsistent})
	sockA := poolA.sockets()[0]
	sockB := poolB.sockets()[0]
	sampleNAT(t, sim, initiator, sockA, sampleObservers)
	sampleNAT(t, sim, responder, sockB, sampleObservers)

	var aFw, bFw uint64
	callStub(t, "natFirewall", func() {
		aFw = initiator.natFirewall()
		bFw = responder.natFirewall()
	})
	if aFw != firewallConsistent || bFw != firewallRandom {
		t.Fatalf("NAT firewalls are %d and %d, want consistent (%d) and randomized (%d)", aFw, bFw, firewallConsistent, firewallRandom)
	}
	var aAddrs, bAddrs []Address
	callStub(t, "natAddresses", func() {
		aAddrs = initiator.natAddresses()
		bAddrs = responder.natAddresses()
	})
	// A randomizing NAT advertises its host only, with port 0 (upstream nat.js _updateAddresses).
	if len(bAddrs) != 1 || bAddrs[0].Host != natB.ext || bAddrs[0].Port != 0 {
		t.Fatalf("randomized NAT addresses = %v, want its host %v with port 0", bAddrs, natB.ext)
	}
	if len(aAddrs) != 1 || aAddrs[0].Port == 0 {
		t.Fatalf("consistent NAT addresses = %v, want one external address with a port", aAddrs)
	}
	callStub(t, "updateRemote", func() {
		initiator.updateRemote(firewallRandom, true, bAddrs, bAddrs[0].Host)
		responder.updateRemote(firewallConsistent, true, aAddrs, aAddrs[0].Host)
	})

	var ok bool
	var err error
	callStub(t, "punch", func() { ok, err = initiator.punch() })
	if err != nil || !ok {
		t.Fatalf("initiator punch = %v, %v, want started with no error", ok, err)
	}
	callStub(t, "punch", func() { ok, err = responder.punch() })
	if err != nil || !ok {
		t.Fatalf("responder punch = %v, %v, want started with no error", ok, err)
	}

	// The consistent side probes the randomizing host on ports it cannot know, with the default TTL.
	for _, d := range sockA.datagrams() {
		if d.to.Host != natB.ext || d.ttl != punchTTLDefault {
			t.Fatalf("initiator sent to %v with TTL %d, want the randomized host %v with the default TTL %d",
				d.to, d.ttl, natB.ext, punchTTLDefault)
		}
	}
	if len(sockA.datagrams()) == 0 {
		t.Error("the consistent side sent no random-port probe")
	}

	// The randomizing side opens BirthdaySockets (256) sockets toward the consistent side, one mapping each.
	towardA := aAddrs[0]
	waitUntil(t, 3*time.Second, "the randomized side opens 256 birthday sockets", func() bool {
		return lowTTLCount(poolB.sockets(), towardA) >= 256
	})
	if n := len(poolB.sockets()); n != 256 {
		t.Errorf("the randomized side holds %d sockets, want 256 (upstream BIRTHDAY_SOCKETS)", n)
	}

	select {
	case ev := <-connects:
		got := addressOf(ev.remote)
		if got.Host != natB.ext {
			t.Errorf("initiator connected to host %v, want the randomized NAT %v", got.Host, natB.ext)
		}
		if !containsPort(natB.mappedToward(towardA), got.Port) {
			t.Errorf("initiator connected to port %d, which is not a port the randomized NAT mapped toward the initiator", got.Port)
		}
	case <-time.After(punchConnect):
		t.Fatalf("no connect within %v; the random probes were bounded at 1750", punchConnect)
	}
}

// containsPort reports whether port is in ports.
func containsPort(ports []uint16, port uint16) bool {
	for _, p := range ports {
		if p == port {
			return true
		}
	}
	return false
}

// Test case 3: two randomizing NATs abort with the double-randomized error and make no punch attempt. Upstream
// decides this in connect.js (probeRound) before any punch round, and punch returns false: both sides send no
// holepunch datagram after the check. The error is the one HOLEPUNCH_DOUBLE_RANDOMIZED_NATS carries.
func TestPunchRandomizedNATsAbortWithoutPunch(t *testing.T) {
	sim := newSimNet()
	natA := sim.addNAT(netip.MustParseAddr("198.51.100.1"), natRandomized)
	natB := sim.addNAT(netip.MustParseAddr("198.51.100.2"), natRandomized)
	poolA := &recordingPool{pool: natA.pool(netip.MustParseAddr("192.0.2.10"))}
	poolB := &recordingPool{pool: natB.pool(netip.MustParseAddr("192.0.2.20"))}
	a := newPuncher(t, punchConfig{Pool: poolA, Initiator: true, RemoteFirewall: firewallRandom})
	b := newPuncher(t, punchConfig{Pool: poolB, RemoteFirewall: firewallRandom})
	sampleNAT(t, sim, a, poolA.sockets()[0], sampleObservers)
	sampleNAT(t, sim, b, poolB.sockets()[0], sampleObservers)

	var aFw, bFw uint64
	callStub(t, "natFirewall", func() {
		aFw = a.natFirewall()
		bFw = b.natFirewall()
	})
	if aFw != firewallRandom || bFw != firewallRandom {
		t.Fatalf("NAT firewalls are %d and %d, want randomized (%d) on both", aFw, bFw, firewallRandom)
	}
	var aAddrs, bAddrs []Address
	callStub(t, "natAddresses", func() {
		aAddrs = a.natAddresses()
		bAddrs = b.natAddresses()
	})
	if len(aAddrs) != 1 || len(bAddrs) != 1 {
		t.Fatalf("randomized NATs give addresses %v and %v, want one host each", aAddrs, bAddrs)
	}
	callStub(t, "updateRemote", func() {
		a.updateRemote(firewallRandom, true, bAddrs, bAddrs[0].Host)
		b.updateRemote(firewallRandom, true, aAddrs, aAddrs[0].Host)
	})

	before := sim.punches()
	var ok bool
	var err error
	callStub(t, "punch", func() { ok, err = a.punch() })
	if ok || !errors.Is(err, ErrHolepunchDoubleRandomized) {
		t.Fatalf("initiator punch = %v, %v, want no punch and ErrHolepunchDoubleRandomized", ok, err)
	}
	callStub(t, "punch", func() { ok, err = b.punch() })
	if ok || !errors.Is(err, ErrHolepunchDoubleRandomized) {
		t.Fatalf("responder punch = %v, %v, want no punch and ErrHolepunchDoubleRandomized", ok, err)
	}
	time.Sleep(300 * time.Millisecond)
	if n := sim.punches() - before; n != 0 {
		t.Errorf("%d holepunch datagrams were sent after the double-randomized check, want none", n)
	}
}

// Test case 4: two peers sharing a public IP try the local addresses first. Upstream connect.js first pings the
// peer's LAN address, the one that matches the local addresses (matchAddress), when both peers' external addresses
// have the same host; the connection then runs on the LAN and never crosses the NAT.
func TestSharedPublicIPTriesLocalAddressFirst(t *testing.T) {
	t.Run("matchAddress picks the most specific local network", func(t *testing.T) {
		cases := []struct {
			name   string
			local  []Address
			remote []Address
			want   Address
			found  bool
		}{
			{
				name:   "same /24 wins over other networks",
				local:  []Address{addr("192.0.2.10", 5000)},
				remote: []Address{addr("203.0.113.5", 1), addr("192.0.2.20", 6000), addr("192.0.2.30", 7000)},
				want:   addr("192.0.2.20", 6000),
				found:  true,
			},
			{
				name:   "first same /24 wins",
				local:  []Address{addr("192.0.2.10", 1)},
				remote: []Address{addr("192.0.9.1", 2), addr("192.0.2.99", 3), addr("192.0.2.98", 4)},
				want:   addr("192.0.2.99", 3),
				found:  true,
			},
			{
				name:   "same /16 beats same /8",
				local:  []Address{addr("198.51.100.7", 1)},
				remote: []Address{addr("198.18.0.1", 2), addr("198.51.1.9", 3)},
				want:   addr("198.51.1.9", 3),
				found:  true,
			},
			{
				name:   "no shared octet gives no match",
				local:  []Address{addr("192.0.2.10", 5000)},
				remote: []Address{addr("203.0.113.5", 1)},
				found:  false,
			},
			{
				name:  "no remote address gives no match",
				local: []Address{addr("192.0.2.10", 5000)},
				found: false,
			},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				var got Address
				var ok bool
				callStub(t, "matchAddress", func() { got, ok = matchAddress(c.local, c.remote) })
				if ok != c.found || (c.found && got != c.want) {
					t.Errorf("matchAddress = %v, %v, want %v, %v", got, ok, c.want, c.found)
				}
			})
		}
	})

	t.Run("localAddresses of a loopback socket is the socket", func(t *testing.T) {
		sim := newSimNet()
		loop := sim.addPublic(addr("127.0.0.1", 4242))
		var got []Address
		callStub(t, "localAddresses", func() { got = localAddresses(loop) })
		want := []Address{addr("127.0.0.1", 4242)}
		if len(got) != 1 || got[0] != want[0] {
			t.Errorf("localAddresses = %v, want %v", got, want)
		}
	})

	t.Run("peers on one public IP reach each other on the LAN", func(t *testing.T) {
		sim := newSimNet()
		shared := sim.addNAT(netip.MustParseAddr("198.51.100.1"), natConsistent)
		sockA := shared.pool(netip.MustParseAddr("192.0.2.10")).Acquire().(*simSocket)
		sockB := shared.pool(netip.MustParseAddr("192.0.2.20")).Acquire().(*simSocket)
		// B advertises its external address and its LAN address, in upstream's order (connectThroughNode).
		lanB := sockB.local
		publicB := sim.observe(sockB, sampleObservers[0])
		// The LAN addresses of A come from its own socket here: upstream reads them from the interfaces.
		local := []Address{sockA.local}

		var chosen Address
		var ok bool
		callStub(t, "matchAddress", func() { chosen, ok = matchAddress(local, []Address{publicB, lanB}) })
		if !ok || chosen != lanB {
			t.Fatalf("matchAddress = %v, %v, want the LAN address %v", chosen, ok, lanB)
		}

		got := make(chan *net.UDPAddr, 1)
		sockB.OnPunch(func(from *net.UDPAddr) { got <- from })
		if err := sockA.SendPunch(udpAddrOf(chosen), punchTTLDefault); err != nil {
			t.Fatalf("SendPunch on the LAN: %v", err)
		}
		select {
		case from := <-got:
			if addressOf(from) != sockA.local {
				t.Errorf("LAN datagram arrived from %v, want A's own LAN address %v (no translation)", from, sockA.local)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the LAN datagram did not reach B")
		}
		if n := len(shared.mappedToward(chosen)); n != 0 {
			t.Errorf("the LAN send made %d NAT mappings toward the LAN address, want none", n)
		}
	})
}

// Timeouts of the punch loops (lib/holepuncher.js). Initiator with a consistent NAT and unanswered remote
// addresses: 10 consistent rounds, every round to the verified address, and the unverified one only on rounds 4
// and 8 (tries & 3), so 12 datagrams in all. Then the puncher destroys itself and reports an abort once. Upstream
// takes about 10.1 s, here the pauses are 1 ms.
func TestPunchConsistentProbesGiveUp(t *testing.T) {
	sim := newSimNet()
	natA := sim.addNAT(netip.MustParseAddr("198.51.100.1"), natConsistent)
	pool := &recordingPool{pool: natA.pool(netip.MustParseAddr("192.0.2.10"))}
	aborts := make(chan struct{}, 4)
	connects := make(chan connectEvent, 4)
	p := newPuncher(t, punchConfig{
		Pool:           pool,
		Initiator:      true,
		RemoteFirewall: firewallConsistent,
		OnConnect:      func(s punchSocket, r *net.UDPAddr) { connects <- connectEvent{s, r} },
		OnAbort:        func() { aborts <- struct{}{} },
		Timing:         punchTiming{ConsistentPause: time.Millisecond},
	})
	sampleNAT(t, sim, p, pool.sockets()[0], sampleObservers)
	verified := addr("198.51.100.9", 5000)
	unverified := addr("198.51.100.10", 6000)
	callStub(t, "updateRemote", func() {
		p.updateRemote(firewallConsistent, true, []Address{verified, unverified}, verified.Host)
	})
	var ok bool
	var err error
	callStub(t, "punch", func() { ok, err = p.punch() })
	if err != nil || !ok {
		t.Fatalf("punch = %v, %v, want started", ok, err)
	}
	select {
	case <-aborts:
	case <-time.After(10 * time.Second):
		t.Fatal("the consistent probes did not give up within 10 s")
	}
	select {
	case <-aborts:
		t.Error("OnAbort ran twice")
	default:
	}
	var destroyed bool
	callStub(t, "destroyed", func() { destroyed = p.destroyed() })
	if !destroyed {
		t.Error("the puncher is not destroyed after its probes ended")
	}
	if len(connects) != 0 {
		t.Error("the puncher connected to an address nobody answers")
	}
	var toVerified, toUnverified int
	for _, d := range pool.sockets()[0].datagrams() {
		switch d.to {
		case verified:
			toVerified++
		case unverified:
			toUnverified++
		}
	}
	if toVerified != 10 || toUnverified != 2 {
		t.Errorf("probes to verified and unverified addresses = %d and %d, want 10 and 2", toVerified, toUnverified)
	}
}

// Timeouts of the random probes: a consistent initiator against a randomizing peer that answers nothing sends
// exactly 1750 probes, all to the peer's host on random ports with the default TTL, then gives up. Upstream takes
// about 36.7 s, here the pause is 1 ms.
func TestPunchRandomProbesGiveUp(t *testing.T) {
	sim := newSimNet()
	natA := sim.addNAT(netip.MustParseAddr("198.51.100.1"), natConsistent)
	pool := &recordingPool{pool: natA.pool(netip.MustParseAddr("192.0.2.10"))}
	aborts := make(chan struct{}, 4)
	p := newPuncher(t, punchConfig{
		Pool:           pool,
		Initiator:      true,
		RemoteFirewall: firewallRandom,
		OnAbort:        func() { aborts <- struct{}{} },
		Timing:         punchTiming{RandomPause: time.Millisecond},
	})
	sampleNAT(t, sim, p, pool.sockets()[0], sampleObservers)
	host := netip.MustParseAddr("198.51.100.9")
	callStub(t, "updateRemote", func() {
		p.updateRemote(firewallRandom, true, []Address{{Host: host}}, host)
	})
	var ok bool
	var err error
	callStub(t, "punch", func() { ok, err = p.punch() })
	if err != nil || !ok {
		t.Fatalf("punch = %v, %v, want started", ok, err)
	}
	select {
	case <-aborts:
	case <-time.After(20 * time.Second):
		t.Fatal("the random probes did not give up within 20 s")
	}
	ds := pool.sockets()[0].datagrams()
	if len(ds) != 1750 {
		t.Errorf("random probes = %d, want 1750 (upstream bound)", len(ds))
	}
	for _, d := range ds {
		if d.to.Host != host || d.ttl != punchTTLDefault {
			t.Fatalf("random probe to %v with TTL %d, want host %v with TTL %d", d.to, d.ttl, host, punchTTLDefault)
		}
	}
}

// NAT samples decide the firewall state and the addresses, as upstream's nat.js does: three samples that agree
// on a port make a consistent NAT, three that differ make a randomizing one, and a tie at two decides nothing.
// Each row is the output of upstream's Nat under node. Samples are [host, port, observer index].
func TestNATSamplesClassifyLikeUpstream(t *testing.T) {
	const a, b = "198.51.100.1", "198.51.100.2"
	cases := []struct {
		name    string
		samples [][3]any
		fw      uint64
		addrs   []Address
	}{
		{
			name:    "four samples on one port",
			samples: [][3]any{{a, 4000, 0}, {a, 4000, 1}, {a, 4000, 2}, {a, 4000, 3}},
			fw:      firewallConsistent,
			addrs:   []Address{addr(a, 4000)},
		},
		{
			name:    "three samples on one port",
			samples: [][3]any{{a, 4000, 0}, {a, 4000, 1}, {a, 4000, 2}},
			fw:      firewallConsistent,
			addrs:   []Address{addr(a, 4000)},
		},
		{
			name:    "three distinct ports",
			samples: [][3]any{{a, 4001, 0}, {a, 4002, 1}, {a, 4003, 2}},
			fw:      firewallRandom,
			addrs:   []Address{addr(a, 0)},
		},
		{
			name:    "two samples decide nothing",
			samples: [][3]any{{a, 4000, 0}, {a, 4000, 1}},
			fw:      firewallUnknown,
		},
		{
			name:    "two on one port and one other: no decision yet",
			samples: [][3]any{{a, 4000, 0}, {a, 4000, 1}, {a, 4001, 2}},
			fw:      firewallUnknown,
		},
		{
			name:    "two pairs on two ports of one host: randomized",
			samples: [][3]any{{a, 4000, 0}, {a, 4000, 1}, {a, 4001, 2}, {a, 4001, 3}},
			fw:      firewallRandom,
			addrs:   []Address{addr(a, 0)},
		},
		{
			name:    "two pairs on two hosts: consistent",
			samples: [][3]any{{a, 4000, 0}, {a, 4000, 1}, {b, 4001, 2}, {b, 4001, 3}},
			fw:      firewallConsistent,
			addrs:   []Address{addr(a, 4000), addr(b, 4001)},
		},
		{
			name:    "three on one port and one other: consistent",
			samples: [][3]any{{a, 4000, 0}, {a, 4000, 1}, {a, 4000, 2}, {b, 4001, 3}},
			fw:      firewallConsistent,
			addrs:   []Address{addr(a, 4000), addr(b, 4001)},
		},
		{
			name:    "four distinct hosts and ports: randomized",
			samples: [][3]any{{a, 4000, 0}, {a, 4001, 1}, {b, 4002, 2}, {"198.51.100.3", 4003, 3}},
			fw:      firewallRandom,
			addrs:   []Address{addr(a, 0)},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sim := newSimNet()
			nat := sim.addNAT(netip.MustParseAddr(a), natConsistent)
			pool := &recordingPool{pool: nat.pool(netip.MustParseAddr("192.0.2.10"))}
			p := newPuncher(t, punchConfig{Pool: pool, Initiator: true})
			for _, s := range c.samples {
				seen := addr(s[0].(string), uint16(s[1].(int)))
				from := sampleObservers[s[2].(int)]
				callStub(t, "observe", func() { p.observe(seen, from) })
			}
			var fw uint64
			var addrs []Address
			callStub(t, "natFirewall", func() {
				fw = p.natFirewall()
				addrs = p.natAddresses()
			})
			if fw != c.fw {
				t.Errorf("firewall = %d, want %d", fw, c.fw)
			}
			if len(addrs) != len(c.addrs) {
				t.Fatalf("addresses = %v, want %v", addrs, c.addrs)
			}
			for i := range addrs {
				if addrs[i] != c.addrs[i] {
					t.Errorf("address %d = %v, want %v", i, addrs[i], c.addrs[i])
				}
			}
		})
	}
}

// holepunchSecret returns the holepunch secret of a handshake whose hash is hash: BLAKE2b-256 of the namespace
// NS.PEER_HOLEPUNCH (command 1), keyed by the handshake hash (lib/noise-wrap.js final).
func holepunchSecret(t *testing.T, hash [64]byte) [32]byte {
	t.Helper()
	ns := dhtNamespace(cmdPeerHolepunch)
	h, err := blake2b.New256(hash[:])
	must(t, err)
	h.Write(ns[:])
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// punchHandshake sends the raw handshake of sendHandshake to relay, and returns the answer with the holepunch
// secret of the handshake. ok is false when the relay gives no answer that decodes as a handshake reply.
func punchHandshake(t *testing.T, relay *net.UDPAddr, host [32]byte, kp noise.KeyPair) (rawAnswer, [32]byte, bool) {
	t.Helper()
	prologue := dhtNamespace(cmdPeerHandshake)
	payload, err := EncodeNoisePayload(NoisePayload{
		UDX:          &UDXInfo{Version: 1, ID: 1},
		SecretStream: &SecretStreamInfo{Version: 1},
	})
	must(t, err)
	hs := noise.NewInitiator(kp, host, prologue[:])
	msg1, err := hs.Send(payload)
	must(t, err)
	value, err := EncodeHandshake(Handshake{Mode: handshakeFromClient, Noise: msg1})
	must(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), handshakeWait)
	defer cancel()
	target := hashKey(host)
	resp, err := newClient(t).Request(ctx, relay, dhtrpc.Request{Command: cmdPeerHandshake, Target: target[:], Value: value})
	if err != nil || resp.Error != 0 || len(resp.Value) == 0 {
		return rawAnswer{}, [32]byte{}, false
	}
	ans, err := DecodeHandshake(resp.Value)
	if err != nil || ans.Mode != handshakeReply || len(ans.Noise) == 0 {
		return rawAnswer{}, [32]byte{}, false
	}
	body, err := hs.Recv(ans.Noise)
	if err != nil || !hs.Complete() {
		return rawAnswer{}, [32]byte{}, false
	}
	p, err := DecodeNoisePayload(body)
	if err != nil {
		return rawAnswer{}, [32]byte{}, false
	}
	_, _, hash, serverKey := hs.Result()
	return rawAnswer{payload: p, serverKey: serverKey}, holepunchSecret(t, hash), true
}

// holderOtherThanServer returns the address of a node that holds the record of the server with public key host,
// other than the server's own node at server.
func holderOtherThanServer(t *testing.T, tn *Testnet, host [32]byte, server *net.UDPAddr) *net.UDPAddr {
	t.Helper()
	for _, a := range findPeerAll(t, tn, hashKey(host)) {
		if a.from.Port != server.Port && bytes.Equal(a.peer.PublicKey, host[:]) {
			return a.from
		}
	}
	t.Fatal("no node other than the server holds its record, so there is no relay to test")
	return nil
}

// PEER_HOLEPUNCH, the relayed holepunch message of a connect (lib/router.js onpeerholepunch, lib/server.js
// _onpeerholepunch). The server's handshake reply names a holepunch id and its relays. A client then sends a probe,
// encrypted with the holepunch secret, to a relay as FROM_CLIENT with the server's address as the relay sees it. The
// relay passes it to the server as FROM_RELAY, and the server's answer comes back to the client as REPLY, with error
// NONE, encrypted with the same secret. Upstream probes this way in probeRound (lib/connect.js).
func TestPeerHolepunchAnsweredByServer(t *testing.T) {
	tn := startTestnet(t, 10)
	hideRemoteAddress(t, tn.Nodes[0]) // the server answers with a holepunch, as a server that does not know its address does
	host := testKeyPair(3)
	srv := newServer(t, tn.Nodes[0], ServerOptions{})
	listenOn(t, srv, host)
	relay := holderOtherThanServer(t, tn, host.Public, nodeAddr(t, tn.Nodes[0]))

	ans, secret, ok := punchHandshake(t, relay, host.Public, testKeyPair(5))
	if !ok {
		t.Fatal("the handshake through a holder got no answer")
	}
	info := ans.payload.Holepunch
	if info == nil || len(info.Relays) == 0 {
		t.Fatal("not implemented: the handshake reply names no holepunch id and relays, so a connect cannot send PEER_HOLEPUNCH")
	}

	var sp *securePayload
	callStub(t, "newSecurePayload", func() { sp = newSecurePayload(secret) })
	enc, err := sp.encrypt(HolepunchPayload{Error: 0, Firewall: firewallUnknown})
	must(t, err)
	msg, err := EncodeHolepunch(Holepunch{
		Mode:        holepunchFromClient,
		ID:          info.ID,
		Payload:     enc,
		PeerAddress: &info.Relays[0].PeerAddress,
	})
	must(t, err)
	target := hashKey(host.Public)
	resp := mustReply(t, newClient(t), udpAddrOf(info.Relays[0].RelayAddress),
		dhtrpc.Request{Command: cmdPeerHolepunch, Target: target[:], Value: msg}, "PEER_HOLEPUNCH")
	hp, err := DecodeHolepunch(resp.Value)
	must(t, err)
	if hp.Mode != holepunchReply {
		t.Fatalf("PEER_HOLEPUNCH answer mode = %d, want REPLY (%d)", hp.Mode, holepunchReply)
	}
	var reply HolepunchPayload
	var decrypted bool
	callStub(t, "decrypt", func() { reply, decrypted = sp.decrypt(hp.Payload) })
	if !decrypted {
		t.Fatal("the answer does not decrypt with the holepunch secret of the handshake")
	}
	if reply.Error != 0 {
		t.Errorf("answer error = %d, want 0 (NONE)", reply.Error)
	}
}
