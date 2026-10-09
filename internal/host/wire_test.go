package host

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"log/slog"
	"math"
	"net"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/internal/config"
	"github.com/andrewloable/HoleBridge/internal/host/hosttest"
	"github.com/andrewloable/HoleBridge/internal/keys"
	"github.com/andrewloable/HoleBridge/internal/log"
	"github.com/andrewloable/HoleBridge/internal/protocol"
	"github.com/andrewloable/HoleBridge/pears/noise"
	"github.com/andrewloable/HoleBridge/pears/secretstream"
)

// kpOf returns the noise key pair of an ed25519 private key.
func kpOf(priv ed25519.PrivateKey) noise.KeyPair {
	var kp noise.KeyPair
	copy(kp.Public[:], priv.Public().(ed25519.PublicKey))
	copy(kp.Secret[:], priv)
	return kp
}

// newWireHost returns the host for cfg, as New makes it, after tweak has set the options. It does not run the
// host on the DHT: the LAN tests hand it streams through ServeConn.
func (r *rig) newWireHost(t *testing.T, cfg *config.Config, tweak func(*Options)) *Host {
	t.Helper()
	opts := Options{DHT: r.tn.Nodes[0], Clock: time.Now, Log: log.New(r.logs, slog.LevelDebug)}
	if tweak != nil {
		tweak(&opts)
	}
	h, err := New(cfg, r.appKey, opts)
	failIfStub(t, err)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

// runWire runs h on the DHT until the test ends, as startHost does for the host core.
func (r *rig) runWire(t *testing.T, h *Host) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(readWait):
			t.Errorf("Run did not return within %v of its context ending", readWait)
		}
	})
}

// lanPair runs the LAN route's secret stream over an in-memory pipe. The host side is the stream the LAN
// listener hands to ServeConn, keyed with the host key pair. The app side is the initiator, keyed with clientKP
// and expecting the host's public key. Both handshakes must succeed.
func (r *rig) lanPair(t *testing.T, clientKP noise.KeyPair) (hostSide, appSide *secretstream.Stream) {
	t.Helper()
	d, err := keys.Derive(r.key, r.appKey)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	hp, ap := net.Pipe()
	hostSide = secretstream.New(hp, false, secretstream.Options{KeyPair: kpOf(d.Host)})
	appSide = secretstream.New(ap, true, secretstream.Options{KeyPair: clientKP, RemotePublicKey: &r.hostPub})
	ctx, cancel := context.WithTimeout(context.Background(), readWait)
	defer cancel()
	hostErr := make(chan error, 1)
	go func() { hostErr <- hostSide.Handshake(ctx) }()
	if err := appSide.Handshake(ctx); err != nil {
		t.Fatalf("app side Handshake: %v", err)
	}
	if err := <-hostErr; err != nil {
		t.Fatalf("host side Handshake: %v", err)
	}
	t.Cleanup(func() { hostSide.Close(); appSide.Close() })
	return hostSide, appSide
}

// serveConn runs h.ServeConn on s in a goroutine and returns a channel that closes when ServeConn returns. When
// the test ends, it cancels ServeConn, closes s and waits. A panic from a stub is reported as a test error.
func serveConn(t *testing.T, h *Host, s *secretstream.Stream) <-chan struct{} {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if p := recover(); p != nil {
				t.Errorf("ServeConn: %v", p)
			}
		}()
		h.ServeConn(ctx, s)
	}()
	t.Cleanup(func() {
		cancel()
		s.Close()
		select {
		case <-done:
		case <-time.After(readWait):
			t.Errorf("ServeConn did not return within %v of its context ending", readWait)
		}
	})
	return done
}

// udpEcho starts a UDP echo server on 127.0.0.1 and returns its address. The source of each datagram it echoes
// goes to the returned channel, which holds 16 and drops the rest rather than block the echo.
func udpEcho(t *testing.T) (string, <-chan net.Addr) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}
	t.Cleanup(func() { pc.Close() })
	sources := make(chan net.Addr, 16)
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			select {
			case sources <- from:
			default:
			}
			pc.WriteTo(buf[:n], from)
		}
	}()
	return pc.LocalAddr().String(), sources
}

// nextSource returns the source address of the next datagram the UDP echo target received.
func nextSource(t *testing.T, sources <-chan net.Addr) net.Addr {
	t.Helper()
	select {
	case src := <-sources:
		return src
	case <-time.After(readWait):
		t.Fatal("the UDP target received no datagram")
		return nil
	}
}

// expectDatagram waits for the next datagram from the host and checks its flow and payload.
func expectDatagram(t *testing.T, c *hosttest.Client, flow uint64, payload string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), readWait)
	defer cancel()
	dg, err := c.Datagram(ctx)
	failIfStub(t, err)
	if err != nil {
		t.Fatalf("Datagram: %v", err)
	}
	if dg.Flow != flow || string(dg.Payload) != payload {
		t.Fatalf("datagram is flow %d with %d bytes, want flow %d with %d bytes",
			dg.Flow, len(dg.Payload), flow, len(payload))
	}
}

// Case 1: every session's secret stream runs with keepalive on, at the 5 s interval of docs/architecture.md (Idle).
// The host reports the options each accepted stream is set up with, through its test hook.
func TestSessionStreamHasKeepalive(t *testing.T) {
	r := newRig(t)
	seen := make(chan secretstream.Options, 4)
	h := r.newWireHost(t, r.hostConfig(t, echoServices(t), nil), func(o *Options) {
		o.streamOptions = func(so secretstream.Options) { seen <- so }
	})
	r.runWire(t, h)
	r.connect(t, r.clientKey)

	select {
	case so := <-seen:
		if so.Keepalive != keepalive {
			t.Errorf("the session's stream keepalive is %v, want %v", so.Keepalive, keepalive)
		}
	case <-time.After(readWait):
		t.Fatal("the host reported no stream options for the session: not implemented")
	}
}

// Case 2: a service with idle 1 s closes a stream that carries no bytes for 1 s. The test keeps the stream busy
// for 2.4 s, longer than the idle time, so it must survive; then it goes quiet, and the host must close the
// stream about 1 s after the last byte.
func TestIdleServiceClosesQuietStream(t *testing.T) {
	r := newRig(t)
	cfg := r.hostConfig(t, map[string]config.Service{
		"echo": {Target: echoTarget(t), Kind: "tcp", Idle: config.Duration(time.Second)},
	}, nil)
	r.runWire(t, r.newWireHost(t, cfg, nil))
	c := r.connect(t, r.clientKey)

	st, err := c.Open("echo")
	failIfStub(t, err)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	var last time.Time
	for i := 0; i < 8; i++ {
		roundTrip(t, st, []byte("busy"))
		last = time.Now()
		time.Sleep(300 * time.Millisecond)
	}

	type result struct {
		err error
		at  time.Time
	}
	read := make(chan result, 1)
	go func() {
		_, err := st.Read(make([]byte, 1))
		read <- result{err, time.Now()}
	}()
	select {
	case res := <-read:
		if res.err == nil {
			t.Fatal("the quiet stream returned data nobody sent")
		}
		if quiet := res.at.Sub(last); quiet < 800*time.Millisecond {
			t.Errorf("the stream closed %v after its last byte, before the 1 s idle time", quiet)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("the quiet stream was still open 4 s after its last byte, past the 1 s idle time: not implemented")
	}
}

// Case 3: ServeConn on a secret stream over net.Pipe, the LAN route's stream, serves the same handshake and the
// same streams as a DHT session.
func TestServeConnServesHandshakeAndStreams(t *testing.T) {
	r := newRig(t)
	h := r.newWireHost(t, r.hostConfig(t, echoServices(t), nil), nil)
	hostSide, appSide := r.lanPair(t, kpOf(r.clientKey))
	serveConn(t, h, hostSide)

	c, err := hosttest.Attach(appSide)
	failIfStub(t, err)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	t.Cleanup(func() { c.Close() })

	// The handshake is the one a DHT session gets, with the LAN route off: no LAN block.
	hs := c.Handshake()
	if hs.Version != 1 || hs.Flags != protocol.FlagResume|protocol.FlagDatagrams || hs.LAN != nil {
		t.Errorf("handshake version %d flags %d, LAN block present %v; want version 1, resume and datagrams, no LAN",
			hs.Version, hs.Flags, hs.LAN != nil)
	}
	if len(hs.Services) != 1 || hs.Services[0].Name != "echo" || hs.Services[0].Kind != protocol.KindTCP {
		t.Errorf("services = %d entries, want one: echo as a tcp service", len(hs.Services))
	}

	st, err := c.Open("echo")
	failIfStub(t, err)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	want := make([]byte, 1<<20)
	if _, err := rand.Read(want); err != nil {
		t.Fatalf("rand: %v", err)
	}
	if got := roundTrip(t, st, want); !bytes.Equal(got, want) {
		t.Fatal("the echo differs from the 1 MB sent")
	}
}

// Case 4: a flow message for a udp service reaches a UDP echo target, and the reply comes back. The test runs on
// the LAN route, where datagrams and replies are messages on the channel (message 10). The DHT route's unordered
// datagrams are covered by TestUDPFlowDHTRouteComesBack. A flow to a service the host does not have gets no
// reply and leaves the session and its other flows working.
func TestUDPFlowReplyComesBack(t *testing.T) {
	r := newRig(t)
	target, _ := udpEcho(t)
	h := r.newWireHost(t, r.hostConfig(t, map[string]config.Service{
		"dns": {Target: target, Kind: "udp"},
	}, nil), nil)
	hostSide, appSide := r.lanPair(t, kpOf(r.clientKey))
	serveConn(t, h, hostSide)

	c, err := hosttest.Attach(appSide)
	failIfStub(t, err)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	t.Cleanup(func() { c.Close() })

	if err := c.Send(protocol.Flow{Flow: 1, Service: "dns", Payload: []byte("first")}); err != nil {
		t.Fatalf("Send flow: %v", err)
	}
	expectDatagram(t, c, 1, "first")

	if err := c.Send(protocol.Datagram{Flow: 1, Payload: []byte("second")}); err != nil {
		t.Fatalf("Send datagram: %v", err)
	}
	expectDatagram(t, c, 1, "second")

	// Flow 2 names a service the host does not have, so nothing comes back for it. The next datagram to arrive
	// must be flow 1's reply.
	if err := c.Send(protocol.Flow{Flow: 2, Service: "missing", Payload: []byte("lost")}); err != nil {
		t.Fatalf("Send flow: %v", err)
	}
	if err := c.Send(protocol.Datagram{Flow: 1, Payload: []byte("third")}); err != nil {
		t.Fatalf("Send datagram: %v", err)
	}
	expectDatagram(t, c, 1, "third")
}

// The DHT route of the same flow. The flow message and its first datagram ride the channel, and the reply comes
// back as an unordered message. The app's second datagram is an unordered message too, as the app sends its
// datagrams on the DHT route, so the host must route it to the flow for the echo to come back.
func TestUDPFlowDHTRouteComesBack(t *testing.T) {
	r := newRig(t)
	target, _ := udpEcho(t)
	cfg := r.hostConfig(t, map[string]config.Service{
		"dns": {Target: target, Kind: "udp"},
	}, nil)
	r.runWire(t, r.newWireHost(t, cfg, nil))
	c := r.connect(t, r.clientKey)

	if err := c.Send(protocol.Flow{Flow: 1, Service: "dns", Payload: []byte("first")}); err != nil {
		t.Fatalf("Send flow: %v", err)
	}
	expectDatagram(t, c, 1, "first")

	if err := c.SendDatagram(protocol.Datagram{Flow: 1, Payload: []byte("second")}); err != nil {
		t.Fatalf("SendDatagram: %v", err)
	}
	expectDatagram(t, c, 1, "second")
}

// A new flow is opened off the session's message path (startFlow). The dial of flow 1 stands in for a slow name
// lookup and blocks until the test releases it. While it is blocked, the session still serves its other messages:
// a flow to another udp service gets its reply, a stream opened on the session echoes, and a datagram on an open
// flow reaches its target. When the lookup returns, flow 1's first payload is the first datagram its target gets,
// and its reply comes back.
func TestSlowFlowLookupDoesNotBlockSession(t *testing.T) {
	r := newRig(t)
	fast, _ := udpEcho(t)
	slow, _ := udpEcho(t)
	cfg := r.hostConfig(t, map[string]config.Service{
		"echo": {Target: echoTarget(t), Kind: "tcp"},
		"fast": {Target: fast, Kind: "udp"},
		"slow": {Target: slow, Kind: "udp"},
	}, nil)

	// Only flow 1 dials the slow service. The dial waits for release, or for readWait so that a failing test does
	// not leave it blocked for good.
	entered := make(chan struct{})
	release := make(chan struct{})
	h := r.newWireHost(t, cfg, func(o *Options) {
		o.udpDial = func(network, address string) (net.Conn, error) {
			if address == slow {
				close(entered)
				select {
				case <-release:
				case <-time.After(readWait):
				}
			}
			return net.Dial(network, address)
		}
	})
	hostSide, appSide := r.lanPair(t, kpOf(r.clientKey))
	serveConn(t, h, hostSide)

	c, err := hosttest.Attach(appSide)
	failIfStub(t, err)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	t.Cleanup(func() { c.Close() })

	if err := c.Send(protocol.Flow{Flow: 1, Service: "slow", Payload: []byte("stuck")}); err != nil {
		t.Fatalf("Send flow 1: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(readWait):
		t.Fatal("the dial of the slow flow did not start")
	}

	// Flow 1's lookup stays blocked from here until release, so none of these may wait for it.
	if err := c.Send(protocol.Flow{Flow: 2, Service: "fast", Payload: []byte("quick")}); err != nil {
		t.Fatalf("Send flow 2: %v", err)
	}
	expectDatagram(t, c, 2, "quick")
	st, err := c.Open("echo")
	failIfStub(t, err)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if got := roundTrip(t, st, []byte("hello")); string(got) != "hello" {
		t.Fatalf("the stream echoed %q, want %q", got, "hello")
	}
	if err := c.Send(protocol.Datagram{Flow: 2, Payload: []byte("again")}); err != nil {
		t.Fatalf("Send datagram: %v", err)
	}
	expectDatagram(t, c, 2, "again")

	// The lookup returns: flow 1's first payload reaches its target, and the target's reply comes back.
	close(release)
	expectDatagram(t, c, 1, "stuck")
}

// The DHT route's size limit (docs/architecture.md, UDP services). The widest flow id takes 9 bytes in the
// frame and a payload of 253 bytes or more a 3-byte length prefix, so maxDatagram of 1144 bytes gives a 1156-byte
// frame, the largest unordered message. It passes both ways. One byte more is dropped by the host as too large.
// That test uses flow 2, whose frame is 4 bytes of header and so leaves the app: a frame over 1156 bytes would
// fail at the app's send before it reached the host.
func TestUDPFlowDHTRouteLimit(t *testing.T) {
	const maxDatagram = 1144
	r := newRig(t)
	target, sources := udpEcho(t)
	cfg := r.hostConfig(t, map[string]config.Service{
		"dns": {Target: target, Kind: "udp"},
	}, nil)
	if got := cfg.Limits.MaxDatagram; got != maxDatagram {
		t.Fatalf("maxDatagram = %d, want %d", got, maxDatagram)
	}
	r.runWire(t, r.newWireHost(t, cfg, nil))
	c := r.connect(t, r.clientKey)

	const widest = uint64(math.MaxUint64)
	big := string(bytes.Repeat([]byte{'a'}, maxDatagram))
	if n := len(protocol.EncodeUnordered(protocol.Datagram{Flow: widest, Payload: []byte(big)})); n != 1156 {
		t.Fatalf("the widest flow's frame is %d bytes at maxDatagram, want 1156", n)
	}
	if err := c.Send(protocol.Flow{Flow: widest, Service: "dns", Payload: []byte("open")}); err != nil {
		t.Fatalf("Send flow: %v", err)
	}
	expectDatagram(t, c, widest, "open")
	if err := c.SendDatagram(protocol.Datagram{Flow: widest, Payload: []byte(big)}); err != nil {
		t.Fatalf("SendDatagram of %d bytes: %v", maxDatagram, err)
	}
	expectDatagram(t, c, widest, big)

	// The target's sources so far belong to the widest flow. Clear them, then count flow 2's arrivals.
	for len(sources) > 0 {
		<-sources
	}
	if err := c.Send(protocol.Flow{Flow: 2, Service: "dns", Payload: []byte("open")}); err != nil {
		t.Fatalf("Send flow: %v", err)
	}
	expectDatagram(t, c, 2, "open")
	if err := c.SendDatagram(protocol.Datagram{Flow: 2, Payload: []byte(big + "a")}); err != nil {
		t.Fatalf("SendDatagram of %d bytes: %v", maxDatagram+1, err)
	}
	if err := c.SendDatagram(protocol.Datagram{Flow: 2, Payload: []byte("after")}); err != nil {
		t.Fatalf("SendDatagram: %v", err)
	}
	expectDatagram(t, c, 2, "after")
	// The target got the flow's first datagram and "after", and never the oversize one: the echo of each
	// datagram the target receives is queued before it is written back, so the count is complete here.
	if n := len(sources); n != 2 {
		t.Fatalf("the target received %d datagrams on flow 2, want 2: the oversize one must not reach it", n)
	}
}

// Case 5: the handshake reports the kind the Kinds option returns for a service. The service has no kind in
// host.json, so the kinds watcher is what knows it.
func TestHandshakeReportsKindsOption(t *testing.T) {
	r := newRig(t)
	cfg := r.hostConfig(t, map[string]config.Service{"web": {Target: "127.0.0.1:8080"}}, nil)
	r.runWire(t, r.newWireHost(t, cfg, func(o *Options) {
		o.Kinds = func(service string) protocol.Kind {
			if service == "web" {
				return protocol.KindHTTPS
			}
			return protocol.KindUnknown
		}
	}))
	hs := r.connect(t, r.clientKey).Handshake()

	for _, s := range hs.Services {
		if s.Name != "web" {
			continue
		}
		if s.Kind != protocol.KindHTTPS {
			t.Errorf("web kind = %d, want %d (https) from the Kinds option", s.Kind, protocol.KindHTTPS)
		}
		if s.Port != 8080 {
			t.Errorf("web port hint = %d, want 8080", s.Port)
		}
		return
	}
	t.Fatal("the handshake does not list the web service")
}

// With the LAN route on in host.json, the handshake carries the LAN flag and the LAN block with the configured
// LAN port. The LAN addresses are the host's own, so the test checks only that the block is there. ServeConn
// binds no port, so the test sets its own.
func TestLANHandshakeCarriesFlagAndPort(t *testing.T) {
	r := newRig(t)
	cfg := r.hostConfig(t, echoServices(t), nil)
	on := true
	cfg.LAN = config.LANConfig{Enabled: &on, DiscoveryPort: 4243, Port: 4242}
	h := r.newWireHost(t, cfg, nil)
	hostSide, appSide := r.lanPair(t, kpOf(r.clientKey))
	serveConn(t, h, hostSide)

	c, err := hosttest.Attach(appSide)
	failIfStub(t, err)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	t.Cleanup(func() { c.Close() })

	hs := c.Handshake()
	if hs.Flags&protocol.FlagLAN == 0 || hs.LAN == nil {
		t.Fatalf("flags = %d, LAN block present %v; want the LAN flag and a LAN block", hs.Flags, hs.LAN != nil)
	}
	if hs.LAN.Port != 4242 {
		t.Errorf("LAN port = %d, want 4242", hs.LAN.Port)
	}
	if hs.Flags&protocol.FlagResume == 0 || hs.Flags&protocol.FlagDatagrams == 0 {
		t.Errorf("flags = %d, want resume and datagrams kept with the LAN flag", hs.Flags)
	}
}

// The LAN listener admits only the client key pair, as the DHT firewall does (docs/architecture.md, LAN route).
// A secret stream proven with another key is closed, and its app gets no handshake.
func TestServeConnRefusesOtherKey(t *testing.T) {
	r := newRig(t)
	h := r.newWireHost(t, r.hostConfig(t, echoServices(t), nil), nil)
	_, other, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	hostSide, appSide := r.lanPair(t, kpOf(other))
	done := serveConn(t, h, hostSide)

	c, err := hosttest.Attach(appSide)
	failIfStub(t, err)
	if err == nil {
		c.Close()
		t.Fatal("the host gave its handshake to a key pair other than the client key pair")
	}
	select {
	case <-done:
	case <-time.After(readWait):
		t.Fatal("ServeConn kept a stream open for a key pair other than the client key pair")
	}
}

// A udp service's idle setting replaces the 60 s flow idle time (docs/cli.md, services.<name>.idle). A flow used
// within the idle time keeps its source port at the target. After 1 s with no datagram either way, the host
// forgets the flow, so the app's next flow message opens a new socket with a new source port.
func TestUDPFlowIdleFollowsService(t *testing.T) {
	r := newRig(t)
	target, sources := udpEcho(t)
	cfg := r.hostConfig(t, map[string]config.Service{
		"dns": {Target: target, Kind: "udp", Idle: config.Duration(time.Second)},
	}, nil)
	h := r.newWireHost(t, cfg, nil)
	hostSide, appSide := r.lanPair(t, kpOf(r.clientKey))
	serveConn(t, h, hostSide)

	c, err := hosttest.Attach(appSide)
	failIfStub(t, err)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	t.Cleanup(func() { c.Close() })

	if err := c.Send(protocol.Flow{Flow: 1, Service: "dns", Payload: []byte("open")}); err != nil {
		t.Fatalf("Send flow: %v", err)
	}
	expectDatagram(t, c, 1, "open")
	open := nextSource(t, sources)

	time.Sleep(300 * time.Millisecond)
	if err := c.Send(protocol.Datagram{Flow: 1, Payload: []byte("again")}); err != nil {
		t.Fatalf("Send datagram: %v", err)
	}
	expectDatagram(t, c, 1, "again")
	if src := nextSource(t, sources); src.String() != open.String() {
		t.Fatal("the flow changed its source port within the idle time")
	}

	time.Sleep(2500 * time.Millisecond)
	if err := c.Send(protocol.Flow{Flow: 1, Service: "dns", Payload: []byte("reopen")}); err != nil {
		t.Fatalf("Send flow: %v", err)
	}
	expectDatagram(t, c, 1, "reopen")
	if src := nextSource(t, sources); src.String() == open.String() {
		t.Fatal("the flow was still open 2.5 s after its last datagram, past the 1 s idle time: not implemented")
	}
}

// lanPairTCP is lanPair over a loopback TCP connection, which ends a write side alone as a LAN socket does.
func (r *rig) lanPairTCP(t *testing.T, clientKP noise.KeyPair) (hostSide, appSide *secretstream.Stream) {
	t.Helper()
	d, err := keys.Derive(r.key, r.appKey)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
	}()
	ac, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	hc := <-accepted
	hostSide = secretstream.New(hc, false, secretstream.Options{KeyPair: kpOf(d.Host)})
	appSide = secretstream.New(ac, true, secretstream.Options{KeyPair: clientKP, RemotePublicKey: &r.hostPub})
	ctx, cancel := context.WithTimeout(context.Background(), readWait)
	defer cancel()
	hostErr := make(chan error, 1)
	go func() { hostErr <- hostSide.Handshake(ctx) }()
	if err := appSide.Handshake(ctx); err != nil {
		t.Fatalf("app side Handshake: %v", err)
	}
	if err := <-hostErr; err != nil {
		t.Fatalf("host side Handshake: %v", err)
	}
	t.Cleanup(func() { hostSide.Close(); appSide.Close() })
	return hostSide, appSide
}
