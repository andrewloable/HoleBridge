// Ported from hyperdht 6.34.1 lib/holepuncher.js (the punch state machine), lib/nat.js (the NAT sampler, fed by
// the caller through observe, since this port has no DHT to ping for samples), lib/secure-payload.js (the encrypted
// holepunch payload), and the holepunch parts of lib/connect.js (localAddresses, matchAddress and the
// double-randomized check), MIT License, Copyright (c) 2018-2019 Mathias Buus, David Mark Clements & Contributors.
package hyperdht

import (
	"context"
	crand "crypto/rand"
	"errors"
	rand "math/rand/v2"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/andrewloable/HoleBridge/pears/dhtrpc"
	"golang.org/x/crypto/blake2b"
	"golang.org/x/crypto/nacl/secretbox"
)

// Firewall states of a peer or a NAT (lib/constants.js FIREWALL). A consistent NAT keeps one external port for every
// destination; a randomizing NAT picks a new port for each destination.
const (
	firewallUnknown    uint64 = 0
	firewallOpen       uint64 = 1
	firewallConsistent uint64 = 2
	firewallRandom     uint64 = 3
)

// TTLs of a holepunch datagram (lib/holepuncher.js). A datagram sent with the low TTL opens a mapping on the local
// NAT and dies before the peer; the default TTL reaches the peer.
const (
	punchTTLLow     = 5
	punchTTLDefault = 64
)

// Modes of a holepunch message (lib/router.js): from the client, from the server, from the relay, and the reply to
// the client.
const (
	holepunchFromClient = 0
	holepunchFromServer = 1
	holepunchFromRelay  = 2
	holepunchReply      = 4
)

// cmdPeerHolepunch is PEER_HOLEPUNCH (lib/constants.js COMMANDS): a holepunch message relayed to the server.
const cmdPeerHolepunch = 1

// Pauses of the punch loops that punchTiming does not set (lib/holepuncher.js).
const keepAliveWait = 100 * time.Millisecond

// maxReopens is upstream's MAX_REOPENS: an unstable socket is reopened on at most this many fresh sockets.
const maxReopens = 3

// errPunchGated is what punch returns when a randomized punch may not begin yet: the DHT's gate on randomized punches
// is at its limit, or the interval after the last one has not passed. No datagram is sent.
var errPunchGated = errors.New("hyperdht: randomized punches are at their limit")

// ErrHolepunchDoubleRandomized is upstream's HOLEPUNCH_DOUBLE_RANDOMIZED_NATS: both the local and the remote NAT
// randomize their ports, so no punch is tried. punch returns it without sending a datagram.
var ErrHolepunchDoubleRandomized = errors.New("hyperdht: both remote and local NATs are randomized")

// punchSocket is a UDP socket a holepuncher punches from. Upstream's socket is a UDX socket; the port needs its address,
// the one-byte holepunch datagram, the handler for holepunch datagrams that arrive on it, and the dht-rpc requests and
// NAT samples that the socket makes (upstream's socket option of request and nat sampling).
type punchSocket interface {
	// Local returns the socket's own address.
	Local() *net.UDPAddr
	// SendPunch sends the one-byte holepunch datagram to to, with the given TTL.
	SendPunch(to *net.UDPAddr, ttl int) error
	// OnPunch sets the handler for holepunch datagrams that arrive on the socket. The handler gets the source
	// address the receiver sees.
	OnPunch(handler func(from *net.UDPAddr))
	// Request sends a dht-rpc request from the socket to to, and returns the reply that comes back to the socket. The
	// probes of a punch that run from this socket are sent this way (upstream updateHolepunch's socket option).
	Request(ctx context.Context, to *net.UDPAddr, req dhtrpc.Request) (*dhtrpc.Response, error)
	// Observe pings to from the socket, and returns the address that to reports for the socket: one NAT sample.
	Observe(ctx context.Context, to *net.UDPAddr) (Address, error)
}

// punchPool hands out the sockets a holepuncher punches from, as upstream's socket pool does. Acquire returns a
// socket that is not in use; Release gives it back.
type punchPool interface {
	Acquire() punchSocket
	Release(punchSocket)
}

// holepunchTryLater is ERROR.TRY_LATER (lib/constants.js): the peer asks the punch to wait while randomized punches run.
const holepunchTryLater uint64 = 3

// randomGate is the DHT's limit on randomized punches (upstream dht._randomPunchLimit, _randomPunchInterval,
// _randomPunches and _lastRandomPunch). begin counts a randomized punch that starts, and reports false while the
// limit is in use or the interval after the last one has not passed; end counts it down and stamps its end. A nil
// gate never refuses. mu guards the counters.
type randomGate struct {
	limit    int           // randomized punches that may run at once: 1
	interval time.Duration // the wait after the last one ended before the next may start: 20 s
	mu       sync.Mutex
	running  int       // randomized punches running (upstream _randomPunches)
	last     time.Time // when the last one ended (upstream _lastRandomPunch)
}

// ready reports whether a randomized punch could begin at now, without counting one.
func (g *randomGate) ready(now time.Time) bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.running < g.limit && now.Sub(g.last) >= g.interval
}

// begin counts a randomized punch that starts at now, and reports whether it may start.
func (g *randomGate) begin(now time.Time) bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.running >= g.limit || now.Sub(g.last) < g.interval {
		return false
	}
	g.running++
	return true
}

// end counts down a randomized punch that ended at now.
func (g *randomGate) end(now time.Time) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.running > 0 {
		g.running--
	}
	g.last = now
}

// sampledFrom reports whether the puncher has taken a NAT sample from the observer from.
func (p *holepuncher) sampledFrom(from Address) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.nat.visited[from]
}

// birthdayPool is a punch pool that also hands out sockets of its own, each with its own NAT mapping, for the birthday
// punch (upstream openBirthdaySockets). A pool without it gives the birthday punch the sockets of Acquire.
type birthdayPool interface {
	AcquireBirthday() (punchSocket, error)
}

// punchTiming sets the loops of the punch state machine. A zero field takes upstream's value.
type punchTiming struct {
	ConsistentRounds int           // consistent probe rounds before the punch gives up: 10
	ConsistentPause  time.Duration // pause after each consistent round: 1 s
	ResponderDelay   time.Duration // a responder waits this long before its first round: 1 s
	RandomProbes     int           // random-port probes before the punch gives up: 1750
	RandomPause      time.Duration // pause after each random probe: 20 ms
	BirthdaySockets  int           // sockets a randomizing side opens for the birthday punch: 256
}

// withDefaults returns t with upstream's value in each zero field.
func (t punchTiming) withDefaults() punchTiming {
	if t.ConsistentRounds == 0 {
		t.ConsistentRounds = 10
	}
	if t.ConsistentPause == 0 {
		t.ConsistentPause = time.Second
	}
	if t.ResponderDelay == 0 {
		t.ResponderDelay = time.Second
	}
	if t.RandomProbes == 0 {
		t.RandomProbes = 1750
	}
	if t.RandomPause == 0 {
		t.RandomPause = 20 * time.Millisecond
	}
	if t.BirthdaySockets == 0 {
		t.BirthdaySockets = 256
	}
	return t
}

// punchConfig sets up a holepuncher. Pool supplies its sockets, and the first one is taken when it is made.
// Initiator is true for the side that connects. RemoteFirewall is the firewall state the peer reported in its
// handshake. OnConnect is called once, on the initiator, when a holepunch datagram from the peer arrives; it gets
// the socket the datagram arrived on and the peer's address. OnAbort is called once when the puncher is destroyed
// before it connects.
type punchConfig struct {
	Pool           punchPool
	Initiator      bool
	RemoteFirewall uint64
	OnConnect      func(sock punchSocket, remote *net.UDPAddr)
	OnAbort        func()
	// OnPunchFrom is called once on a responder, when a holepunch datagram arrives from one of the peer's
	// addresses. A server claims the stream of its handshake there. It is never called on an initiator.
	OnPunchFrom func(sock punchSocket, remote *net.UDPAddr)
	// Gate is the DHT's limit on randomized punches. A randomized punch counts against it from the moment it begins
	// until it connects or is destroyed. Nil means no limit.
	Gate *randomGate
	// Sample takes the NAT samples of the puncher from sock, a socket that analyze has reopened onto (upstream
	// nat.autoSample on the new socket). Nil means the puncher takes no samples of its own, and an unstable socket
	// stays unstable.
	Sample func(ctx context.Context, sock punchSocket, p *holepuncher) error
	Timing punchTiming
}

// holepuncher is the punch state of one connect or one handshake (lib/holepuncher.js): the sockets it punches
// from, its NAT samples, and what the peer reported. Its probe loops run in the background, and stop when it is
// destroyed or connects. mu guards every field below it.
type holepuncher struct {
	pool        punchPool
	initiator   bool
	timing      punchTiming
	onConnect   func(sock punchSocket, remote *net.UDPAddr)
	onAbort     func()
	onPunchFrom func(sock punchSocket, remote *net.UDPAddr)
	gate        *randomGate // the DHT's limit on randomized punches, or nil
	sample      func(ctx context.Context, sock punchSocket, p *holepuncher) error
	stop        chan struct{} // closed by destroy, which ends the pauses

	mu                 sync.Mutex
	holders            []punchSocket // holders[0] is the socket probes go out from; the connected one, once connected
	nat                natSamples
	remoteFirewall     uint64
	remoteAddresses    []remoteAddress
	remoteHolepunching bool
	punching           bool
	isConnected        bool
	isDestroyed        bool
	heard              bool // a responder has called onPunchFrom
	gated              bool // this puncher counts against the gate: a randomized punch has begun and not ended
	reopenOnce         sync.Once
	reopenStable       bool  // the result of the reopen, which runs once
	reopenErr          error // the error of the reopen, if any
}

// remoteAddress is an address the peer reported, and whether it is verified: the peer echoed the token of its host.
type remoteAddress struct {
	addr     Address
	verified bool
}

// newHolepuncher returns a holepuncher, with its first socket taken from cfg.Pool.
func newHolepuncher(cfg punchConfig) *holepuncher {
	p := &holepuncher{
		pool:           cfg.Pool,
		initiator:      cfg.Initiator,
		timing:         cfg.Timing.withDefaults(),
		onConnect:      cfg.OnConnect,
		onAbort:        cfg.OnAbort,
		onPunchFrom:    cfg.OnPunchFrom,
		gate:           cfg.Gate,
		sample:         cfg.Sample,
		stop:           make(chan struct{}),
		remoteFirewall: cfg.RemoteFirewall,
	}
	if p.onConnect == nil {
		p.onConnect = func(punchSocket, *net.UDPAddr) {}
	}
	if p.onAbort == nil {
		p.onAbort = func() {}
	}
	p.adoptHolder(cfg.Pool.Acquire())
	return p
}

// adoptHolder makes sock one of the puncher's holders and routes the holepunch datagrams it receives to the puncher.
// A destroyed or connected puncher releases the socket instead, and returns false.
func (p *holepuncher) adoptHolder(sock punchSocket) bool {
	sock.OnPunch(func(from *net.UDPAddr) { p.onPunchMessage(sock, from) })
	p.mu.Lock()
	if p.isDestroyed || p.isConnected {
		p.mu.Unlock()
		p.pool.Release(sock) // a puncher that has connected keeps no more sockets
		return false
	}
	p.holders = append(p.holders, sock)
	p.mu.Unlock()
	return true
}

// updateRemote records what the peer reported: its firewall state, whether it is punching, its addresses, and
// the host whose token it echoed (the zero address when none). Addresses on the echoed host are verified, and so
// are the addresses that were verified before (upstream holepuncher updateRemote).
func (p *holepuncher) updateRemote(firewall uint64, punching bool, addresses []Address, echoed netip.Addr) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var next []remoteAddress
	for _, a := range addresses {
		verified := (echoed.IsValid() && a.Host == echoed) || p.isVerifiedLocked(a.Host)
		next = append(next, remoteAddress{addr: a, verified: verified})
	}
	p.remoteFirewall = firewall
	p.remoteAddresses = next
	p.remoteHolepunching = punching
}

// isRemoteLocked reports whether addr is one of the peer's addresses: its host, and its port when the peer gave
// one (a randomizing peer gives its host only). The caller holds mu.
func (p *holepuncher) isRemoteLocked(addr Address) bool {
	for _, a := range p.remoteAddresses {
		if a.addr.Host == addr.Host && (a.addr.Port == 0 || a.addr.Port == addr.Port) {
			return true
		}
	}
	return false
}

// isVerifiedLocked reports whether host is a verified remote address. The caller holds mu.
func (p *holepuncher) isVerifiedLocked(host netip.Addr) bool {
	for _, a := range p.remoteAddresses {
		if a.verified && a.addr.Host == host {
			return true
		}
	}
	return false
}

// observe records one NAT sample: the address seen is the puncher's own address as the observer from saw it. The
// NAT's firewall state and addresses follow from the samples, as upstream's nat.js computes them. An observer
// counts once.
func (p *holepuncher) observe(seen, from Address) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nat.add(seen, from)
}

// natFirewall returns the firewall state of the puncher's NAT, from its samples. It is firewallUnknown until the
// samples decide.
func (p *holepuncher) natFirewall() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.nat.firewall
}

// natAddresses returns the addresses the NAT samples give, or nil while the firewall state is unknown.
func (p *holepuncher) natAddresses() []Address {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.nat.addrs == nil {
		return nil
	}
	return append([]Address(nil), p.nat.addrs...)
}

// firewalls returns the peer's firewall state as the puncher has it, the puncher's own NAT state, and whether the
// peer is punching.
func (p *holepuncher) firewalls() (remote, local uint64, remotePunching bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.remoteFirewall, p.nat.firewall, p.remoteHolepunching
}

// probeSocket returns the socket that probes go out from, or nil once the puncher has none.
func (p *holepuncher) probeSocket() punchSocket {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.holders) == 0 {
		return nil
	}
	return p.holders[0]
}

// openSession sends a low-TTL datagram to addr from the probe socket. It opens a mapping on the local NAT and
// dies before the peer (upstream holepuncher openSession).
func (p *holepuncher) openSession(addr Address) {
	if sock := p.probeSocket(); sock != nil {
		sendHolepunch(sock, addr, true)
	}
}

// analyze reports whether the puncher's NAT is stable (upstream holepuncher analyze). A socket is unstable when both
// the peer and this NAT randomize, or when this NAT's firewall state is still unknown. An unstable socket is not stable
// unless allowReopen is set, and then the puncher reopens it: each new socket is a birthday socket of its own, whose NAT
// is sampled from it, up to MAX_REOPENS (3) sockets. The reopen runs once, and later calls return its result.
func (p *holepuncher) analyze(ctx context.Context, allowReopen bool) (bool, error) {
	p.mu.Lock()
	unstable := p.unstableLocked()
	p.mu.Unlock()
	if !unstable {
		return true, nil
	}
	if !allowReopen {
		return false, nil
	}
	p.reopenOnce.Do(func() { p.reopenStable, p.reopenErr = p.reopen(ctx) })
	return p.reopenStable, p.reopenErr
}

// unstableLocked reports whether the puncher's socket is unstable (upstream _unstable): the peer and this NAT both
// randomize, or this NAT's firewall state is still unknown. The caller holds mu.
func (p *holepuncher) unstableLocked() bool {
	return (p.remoteFirewall >= firewallRandom && p.nat.firewall >= firewallRandom) || p.nat.firewall == firewallUnknown
}

// reopen moves the puncher onto fresh sockets while its NAT is unstable (upstream _reopen). Each fresh socket is taken
// from the pool, a birthday socket of its own when the pool has them, and its NAT is sampled from it. It stops when the
// NAT is stable, when the puncher is done or punching, or after maxReopens sockets. It reports whether the NAT ends
// consistent. Without a Sample hook the puncher has no samples for a fresh socket, so it does not reopen at all.
func (p *holepuncher) reopen(ctx context.Context) (bool, error) {
	if p.sample == nil {
		return false, nil
	}
	for i := 0; i < maxReopens; i++ {
		p.mu.Lock()
		again := p.unstableLocked() && !p.isDestroyed && !p.isConnected && !p.punching
		p.mu.Unlock()
		if !again {
			break
		}
		sock, err := p.acquireBirthday()
		if err != nil {
			return false, err
		}
		if !p.resetNAT(sock) {
			return false, nil
		}
		if err := p.sample(ctx, sock, p); err != nil {
			return false, err
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return coerceFirewall(p.nat.firewall) == firewallConsistent, nil
}

// resetNAT makes sock the socket the puncher probes from, in place of the one it had, and starts its NAT samples afresh
// (upstream _reset). The socket it replaces is released. It returns false, and releases sock, when the puncher is
// destroyed or connected.
func (p *holepuncher) resetNAT(sock punchSocket) bool {
	sock.OnPunch(func(from *net.UDPAddr) { p.onPunchMessage(sock, from) })
	p.mu.Lock()
	if p.isDestroyed || p.isConnected || len(p.holders) == 0 {
		p.mu.Unlock()
		p.pool.Release(sock)
		return false
	}
	old := p.holders[0]
	p.holders[0] = sock
	p.nat = natSamples{}
	p.mu.Unlock()
	p.pool.Release(old)
	return true
}

// punch starts the punch for the firewall states of both sides, and returns whether a punch is running: a punch
// that runs already is left as it is, so a repeated call returns true. It returns ErrHolepunchDoubleRandomized,
// with no datagram sent, when both NATs randomize. The probe loops run in the background, and end when the
// puncher connects or is destroyed.
func (p *holepuncher) punch() (bool, error) {
	p.mu.Lock()
	if p.isDestroyed || p.isConnected {
		p.mu.Unlock()
		return false, nil
	}
	if p.punching {
		// A punch that runs already answers for the peer's probes (upstream punch returns the running one).
		p.mu.Unlock()
		return true, nil
	}
	if p.remoteFirewall >= firewallRandom && p.nat.firewall >= firewallRandom {
		p.mu.Unlock()
		return false, ErrHolepunchDoubleRandomized
	}

	// Coerce into consistency for now, as upstream does. Most of these probes run in the background, so they
	// are not awaited here.
	local := coerceFirewall(p.nat.firewall)
	remote := coerceFirewall(p.remoteFirewall)

	if local == firewallConsistent && remote == firewallConsistent {
		p.punching = true
		p.mu.Unlock()
		go p.consistentProbe()
		return true, nil
	}

	verified, ok := p.verifiedAddressLocked()
	if !ok {
		p.mu.Unlock()
		return false, nil
	}
	switch {
	case local == firewallConsistent && remote >= firewallRandom:
		if !p.beginRandomLocked() {
			p.mu.Unlock()
			return false, errPunchGated
		}
		p.punching = true
		sock := p.holders[0]
		p.mu.Unlock()
		// The first random probe goes out before punch returns, as upstream's does.
		sendHolepunch(sock, randomAddress(verified.addr.Host), false)
		go p.randomProbes(sock, verified.addr.Host, p.timing.RandomProbes-1)
		return true, nil
	case local >= firewallRandom && remote == firewallConsistent:
		if !p.beginRandomLocked() {
			p.mu.Unlock()
			return false, errPunchGated
		}
		p.punching = true
		p.mu.Unlock()
		go p.birthdayProbes(verified.addr)
		return true, nil
	}
	p.mu.Unlock()
	return false, nil
}

// beginRandomLocked counts a randomized punch against the gate, and reports whether it may begin. The caller holds mu.
func (p *holepuncher) beginRandomLocked() bool {
	if p.gate == nil {
		return true
	}
	if !p.gate.begin(time.Now()) {
		return false
	}
	p.gated = true
	return true
}

// endRandomLocked counts this puncher's randomized punch off the gate, once, when it ends at now. The caller holds mu.
func (p *holepuncher) endRandomLocked(now time.Time) {
	if p.gated {
		p.gated = false
		p.gate.end(now)
	}
}

// verifiedAddressLocked returns the first verified remote address. The caller holds mu.
func (p *holepuncher) verifiedAddressLocked() (remoteAddress, bool) {
	for _, a := range p.remoteAddresses {
		if a.verified {
			return a, true
		}
	}
	return remoteAddress{}, false
}

// consistentProbe sends the probes of a consistent punch: each round goes to every verified address, and to the
// unverified ones on every fourth round. A responder waits ResponderDelay first, since the initiator's fast open
// has already sent its probe. It never reports an error; the puncher ends when the rounds run out.
func (p *holepuncher) consistentProbe() {
	if !p.initiator {
		p.pause(p.timing.ResponderDelay)
	}
	for tries := 1; tries <= p.timing.ConsistentRounds && p.isPunching(); tries++ {
		sock, addrs, ok := p.roundTargets()
		if !ok {
			break
		}
		for _, a := range addrs {
			// Only try unverified addresses every fourth round (upstream: tries & 3).
			if !a.verified && tries&3 != 0 {
				continue
			}
			sendHolepunch(sock, a.addr, false)
		}
		p.pause(p.timing.ConsistentPause)
	}
	p.autoDestroy()
}

// roundTargets returns the socket a consistent round sends from, and the remote addresses it goes to. ok is false
// when the puncher is destroyed, since destroy releases the sockets.
func (p *holepuncher) roundTargets() (sock punchSocket, addrs []remoteAddress, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.isDestroyed || len(p.holders) == 0 {
		return nil, nil, false
	}
	return p.holders[0], append([]remoteAddress(nil), p.remoteAddresses...), true
}

// randomProbes sends left probes to random ports of host, one after each pause, with the default TTL. The first
// probe was sent by punch. The puncher ends when the probes run out.
func (p *holepuncher) randomProbes(sock punchSocket, host netip.Addr, left int) {
	for ; left > 0; left-- {
		if !p.pause(p.timing.RandomPause) {
			break
		}
		sendHolepunch(sock, randomAddress(host), false)
	}
	p.autoDestroy()
}

// birthdayProbes is the punch of a randomizing side toward a consistent peer: it opens sockets toward the peer's
// verified address with the low TTL, so each one gets a mapping on the local NAT, then keeps them alive.
func (p *holepuncher) birthdayProbes(remote Address) {
	p.openBirthdaySockets(remote)
	if p.isPunching() {
		p.keepAliveRandomNat(remote)
	}
}

// openBirthdaySockets adds sockets until the puncher has BirthdaySockets of them, and sends each one a low-TTL
// datagram to remote, which opens its mapping on the local NAT without reaching the peer.
func (p *holepuncher) openBirthdaySockets(remote Address) {
	for p.isPunching() && p.holderCount() < p.timing.BirthdaySockets {
		sock, err := p.acquireBirthday()
		if err != nil {
			return
		}
		if !p.adoptHolder(sock) {
			return
		}
		sendHolepunch(sock, remote, true)
	}
}

// acquireBirthday returns a socket for the birthday punch: a socket of its own when the pool has them (birthdayPool),
// else the pool's next socket.
func (p *holepuncher) acquireBirthday() (punchSocket, error) {
	if bp, ok := p.pool.(birthdayPool); ok {
		return bp.AcquireBirthday()
	}
	return p.pool.Acquire(), nil
}

// keepAliveRandomNat keeps the birthday mappings open. Each holder sends to remote, the first pass with the low
// TTL, so that every mapping is refreshed, then with the default TTL. It ends after RandomProbes datagrams.
func (p *holepuncher) keepAliveRandomNat(remote Address) {
	p.pause(keepAliveWait)
	i := 0
	lowTTLRounds := 1
	for tries := p.timing.RandomProbes; tries > 0 && p.isPunching(); tries-- {
		socks := p.holderSockets()
		if len(socks) == 0 {
			break
		}
		if i >= len(socks) {
			i = 0
			if lowTTLRounds > 0 {
				lowTTLRounds--
			}
		}
		sendHolepunch(socks[i], remote, lowTTLRounds > 0)
		i++
		p.pause(p.timing.RandomPause)
	}
	p.autoDestroy()
}

// holderCount returns how many sockets the puncher holds.
func (p *holepuncher) holderCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.holders)
}

// holderSockets returns a copy of the sockets the puncher holds.
func (p *holepuncher) holderSockets() []punchSocket {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]punchSocket(nil), p.holders...)
}

// onPunchMessage handles a holepunch datagram that arrived on sock from from. A responder answers it, so the peer
// can connect; the initiator connects on the first one, and releases the other sockets.
//
// The node's socket is shared, so a datagram can come from any address. An initiator connects only on a datagram
// from an address the peer named, and a responder answers and calls onPunchFrom only for one.
func (p *holepuncher) onPunchMessage(sock punchSocket, from *net.UDPAddr) {
	addr := addressOf(from)
	p.mu.Lock()
	if p.isDestroyed {
		p.mu.Unlock()
		return
	}
	if !p.initiator {
		// A responder answers only a datagram from an address its peer named. The node's socket is shared by every live
		// responder, so answering each datagram would multiply it by the number of live handshakes; upstream's responder
		// answers what its own socket receives, which only its peer's datagrams reach.
		named := p.isRemoteLocked(addr)
		fire := p.onPunchFrom != nil && !p.heard && named
		if fire {
			p.heard = true
		}
		p.mu.Unlock()
		if fire {
			p.onPunchFrom(sock, from)
		}
		if named {
			sendHolepunch(sock, addr, false) // never fails
		}
		return
	}
	if p.isConnected || !p.isRemoteLocked(addr) {
		p.mu.Unlock()
		return
	}
	p.isConnected = true
	p.punching = false
	p.endRandomLocked(time.Now())
	var others []punchSocket
	for _, h := range p.holders {
		if h != sock {
			others = append(others, h)
		}
	}
	p.holders = []punchSocket{sock}
	p.mu.Unlock()

	for _, h := range others {
		p.pool.Release(h)
	}
	p.onConnect(sock, from)
}

// connected reports whether a holepunch datagram from the peer has arrived on the initiator.
func (p *holepuncher) connected() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.isConnected
}

// destroyed reports whether the puncher is destroyed, by destroy or by ending its probes.
func (p *holepuncher) destroyed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.isDestroyed
}

// destroy stops the puncher and releases its sockets. OnAbort runs when the puncher had not connected. A connected
// puncher keeps its socket, which OnConnect handed to the caller.
func (p *holepuncher) destroy() {
	p.mu.Lock()
	if p.isDestroyed {
		p.mu.Unlock()
		return
	}
	p.isDestroyed = true
	p.punching = false
	p.endRandomLocked(time.Now())
	close(p.stop)
	connected := p.isConnected
	holders := p.holders
	p.holders = nil
	p.mu.Unlock()

	if !connected {
		for _, h := range holders {
			p.pool.Release(h)
		}
		p.onAbort()
	}
}

// autoDestroy ends the puncher when its probes ran out without a connection.
func (p *holepuncher) autoDestroy() {
	p.mu.Lock()
	connected := p.isConnected
	p.mu.Unlock()
	if !connected {
		p.destroy()
	}
}

// isPunching reports whether the puncher is still punching: it is neither destroyed nor connected.
func (p *holepuncher) isPunching() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.punching && !p.isDestroyed
}

// pause waits for d, or until the puncher is destroyed. It reports whether the puncher is still punching.
func (p *holepuncher) pause(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-p.stop:
	}
	return p.isPunching()
}

// sendHolepunch sends the one-byte holepunch datagram from sock to to, with the low TTL when lowTTL is set.
func sendHolepunch(sock punchSocket, to Address, lowTTL bool) error {
	ttl := punchTTLDefault
	if lowTTL {
		ttl = punchTTLLow
	}
	return sock.SendPunch(udpAddrOf(to), ttl)
}

// randomAddress returns a random port of host, in upstream's range for random probes (1000 to 65535).
func randomAddress(host netip.Addr) Address {
	return Address{Host: host, Port: uint16(1000 + rand.IntN(64536))}
}

// coerceFirewall treats an open firewall as consistent, for the punch (upstream coerceFirewall).
func coerceFirewall(fw uint64) uint64 {
	if fw == firewallOpen {
		return firewallConsistent
	}
	return fw
}

// natSamples is the NAT sampler of lib/nat.js, without the pings: each observer counts once, and the samples decide
// the firewall state and the addresses. A DHT that is not firewalled is not modelled here, so every NAT is
// firewalled.
type natSamples struct {
	hosts    []natSample      // samples by host (port 0), most hits first (upstream _samplesHost)
	full     []natSample      // samples by host and port, most hits first (upstream _samplesFull)
	visited  map[Address]bool // observers that have sampled (upstream _visited)
	sampled  int
	firewall uint64
	addrs    []Address // nil while the firewall state is unknown
}

// natSample is one sample: the host and port seen, and how many observers saw them.
type natSample struct {
	host netip.Addr
	port uint16
	hits int
}

// add records a sample of seen from the observer from. From the third sample on, the firewall state and the
// addresses are decided (upstream Nat.add and update).
func (n *natSamples) add(seen, from Address) {
	if n.visited == nil {
		n.visited = map[Address]bool{}
	}
	if n.visited[from] {
		return
	}
	n.visited[from] = true
	n.hosts = addSample(n.hosts, seen.Host, 0)
	n.full = addSample(n.full, seen.Host, seen.Port)
	n.sampled++
	if n.sampled >= 3 {
		n.updateFirewall()
		n.updateAddresses()
	}
}

// updateFirewall decides the firewall state from the samples (upstream Nat._updateFirewall). It leaves the state
// as it is when the samples do not decide it.
func (n *natSamples) updateFirewall() {
	if n.sampled < 3 {
		return
	}
	switch top := n.full[0].hits; {
	case top >= 3:
		n.firewall = firewallConsistent
		return
	case top == 1:
		n.firewall = firewallRandom
		return
	}
	// The most hit port has two hits. One host with more than three samples is randomizing; two hosts with a
	// double hit on the second one is consistent. Four samples with no decision are taken as randomizing.
	if len(n.hosts) == 1 && n.sampled > 3 {
		n.firewall = firewallRandom
		return
	}
	if len(n.hosts) > 1 && n.full[1].hits > 1 {
		n.firewall = firewallConsistent
		return
	}
	if n.sampled > 4 {
		n.firewall = firewallRandom
	}
}

// updateAddresses sets the addresses the samples give (upstream Nat._updateAddresses): a randomizing NAT has its
// host only, and a consistent one its ports that were seen twice, or at least two.
func (n *natSamples) updateAddresses() {
	switch n.firewall {
	case firewallUnknown:
		n.addrs = nil
	case firewallRandom:
		n.addrs = []Address{{Host: n.hosts[0].host}}
	case firewallConsistent:
		n.addrs = []Address{}
		for _, s := range n.full {
			if s.hits >= 2 || len(n.addrs) < 2 {
				n.addrs = append(n.addrs, Address{Host: s.host, Port: s.port})
			}
		}
	}
}

// addSample counts one sample of host and port, and keeps the samples sorted by hits, most first.
func addSample(samples []natSample, host netip.Addr, port uint16) []natSample {
	for i := range samples {
		if samples[i].host != host || samples[i].port != port {
			continue
		}
		samples[i].hits++
		for ; i > 0 && samples[i-1].hits < samples[i].hits; i-- {
			samples[i-1], samples[i] = samples[i], samples[i-1]
		}
		return samples
	}
	return append(samples, natSample{host: host, port: port, hits: 1})
}

// localAddresses returns the addresses of a socket that a peer on the same network can reach: the socket's own
// address on a loopback socket, else the IPv4 addresses of its interfaces with the socket's port (lib/holepuncher.js
// localAddresses).
func localAddresses(sock punchSocket) []Address {
	local := sock.Local()
	if ip4 := local.IP.To4(); ip4 != nil && ip4.IsLoopback() {
		return []Address{addressOf(local)}
	}
	var out []Address
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipn.IP.To4()
			if ip4 == nil {
				continue
			}
			out = append(out, Address{Host: netip.AddrFrom4([4]byte(ip4)), Port: uint16(local.Port)})
		}
	}
	if len(out) == 0 {
		out = []Address{{Host: netip.AddrFrom4([4]byte{127, 0, 0, 1}), Port: uint16(local.Port)}}
	}
	return out
}

// matchAddress returns the remote address that shares the longest prefix of the local addresses, as upstream's
// matchAddress does: the first remote address sharing three leading octets with a local address wins at once; else
// the first sharing two; else the first sharing one. ok is false when no remote address shares the first octet.
func matchAddress(local, remote []Address) (Address, bool) {
	if len(remote) == 0 {
		return Address{}, false
	}
	var best Address
	found := false
	segment := 1
	for _, l := range local {
		if !l.Host.Is4() {
			continue
		}
		a := l.Host.As4()
		for _, r := range remote {
			if !r.Host.Is4() {
				continue
			}
			b := r.Host.As4()
			if a[0] != b[0] {
				continue
			}
			if segment == 1 {
				best, found, segment = r, true, 2
			}
			if a[1] != b[1] {
				continue
			}
			if segment == 2 {
				best, segment = r, 3
			}
			if a[2] == b[2] {
				return r, true
			}
		}
	}
	return best, found
}

// securePayload encrypts the holepunch payloads of one connect with its holepunch secret (lib/secure-payload.js):
// XSalsa20-Poly1305 with a random 24-byte nonce in front of the message, then the MAC and the ciphertext. It also
// makes the token of an address, which the peer echoes to show it reached that address.
type securePayload struct {
	shared [32]byte // the holepunch secret of the handshake
	local  [32]byte // this side's token secret (upstream _localSecret)
}

// newSecurePayload returns the payload coder for a holepunch secret.
func newSecurePayload(secret [32]byte) *securePayload {
	s := &securePayload{shared: secret}
	crand.Read(s.local[:]) // crypto/rand does not fail on the supported platforms
	return s
}

// encrypt encodes and encrypts a holepunch payload.
func (s *securePayload) encrypt(p HolepunchPayload) ([]byte, error) {
	msg, err := EncodeHolepunchPayload(p)
	if err != nil {
		return nil, err
	}
	var nonce [24]byte
	crand.Read(nonce[:])
	out := make([]byte, 24, 24+secretbox.Overhead+len(msg))
	copy(out, nonce[:])
	return secretbox.Seal(out, msg, &nonce, &s.shared), nil
}

// decrypt decrypts and decodes a holepunch payload. ok is false when the message does not decrypt or decode.
func (s *securePayload) decrypt(b []byte) (p HolepunchPayload, ok bool) {
	if len(b) <= 24+secretbox.Overhead {
		return p, false
	}
	var nonce [24]byte
	copy(nonce[:], b[:24])
	msg, ok := secretbox.Open(nil, b[24:], &nonce, &s.shared)
	if !ok {
		return p, false
	}
	p, err := DecodeHolepunchPayload(msg)
	if err != nil {
		return p, false
	}
	return p, true
}

// token returns the token of addr: BLAKE2b-256 of the host, keyed with this side's token secret.
func (s *securePayload) token(addr Address) [32]byte {
	h, _ := blake2b.New256(s.local[:])
	h.Write([]byte(addr.Host.String()))
	var out [32]byte
	h.Sum(out[:0])
	return out
}

// punchSecret returns the holepunch secret of a handshake whose hash is hash: BLAKE2b-256 of the namespace
// NS.PEER_HOLEPUNCH, keyed by the handshake hash (lib/noise-wrap.js final).
func punchSecret(hash [64]byte) [32]byte {
	ns := dhtNamespace(cmdPeerHolepunch)
	h, _ := blake2b.New256(hash[:])
	h.Write(ns[:])
	var out [32]byte
	h.Sum(out[:0])
	return out
}
