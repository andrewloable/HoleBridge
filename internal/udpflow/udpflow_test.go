package udpflow

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/internal/protocol"
)

// maxDatagram is the largest payload on every route: 1144 bytes. The largest unordered message is 1156
// bytes (docs/spike-m1.md, "## Unordered datagrams"), and the frame of protocol.EncodeUnordered adds 12
// bytes to the payload at the widest flow id.
const maxDatagram = 1144

// sent is one datagram that came back through send.
type sent struct {
	flow    uint64
	payload []byte
}

// fakeClock is a clock the test moves by hand.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// config returns the limits of docs/architecture.md, "Limits", with the measured maxDatagram. Each call
// makes its own counters, so the config of one call is the limits of one session.
func config() Config {
	return Config{
		MaxDatagram:  maxDatagram,
		Idle:         60 * time.Second,
		PerSession:   NewCounter(256),
		Total:        NewCounter(4096),
		OrderedQueue: 256 << 10,
	}
}

// discard is a send that drops everything.
func discard(uint64, []byte) {}

// failOnPanic turns a panic from a stub into a test failure, so the other tests still run.
// Defer it first in a test that calls a stub which panics.
func failOnPanic(t *testing.T) {
	t.Helper()
	if r := recover(); r != nil {
		t.Fatalf("%v", r)
	}
}

// listen opens a UDP socket on 127.0.0.1 that the test closes.
func listen(t *testing.T) net.PacketConn {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// resolver maps the service "echo" to the target socket and rejects any other service.
func resolver(target net.PacketConn) func(string) (string, bool) {
	return func(service string) (string, bool) {
		if service != "echo" {
			return "", false
		}
		return target.LocalAddr().String(), true
	}
}

// recorder returns a send that queues what it gets, and the queue.
func recorder() (func(uint64, []byte), <-chan sent) {
	got := make(chan sent, 16)
	send := func(flow uint64, p []byte) {
		got <- sent{flow, append([]byte(nil), p...)}
	}
	return send, got
}

// nextSent returns the next datagram that came back through send, or fails after 2 s.
func nextSent(t *testing.T, got <-chan sent) sent {
	t.Helper()
	select {
	case s := <-got:
		return s
	case <-time.After(2 * time.Second):
		t.Fatal("no datagram came back through send within 2 s")
	}
	return sent{}
}

// read returns the next datagram the target gets, and the address it came from.
func read(t *testing.T, conn net.PacketConn) ([]byte, net.Addr) {
	t.Helper()
	buf := make([]byte, 65536)
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	n, from, err := conn.ReadFrom(buf)
	if err != nil {
		t.Fatalf("target got no datagram within 2 s: %v", err)
	}
	return buf[:n], from
}

// write sends p from conn to addr.
func write(t *testing.T, conn net.PacketConn, p []byte, addr net.Addr) {
	t.Helper()
	if _, err := conn.WriteTo(p, addr); err != nil {
		t.Fatal(err)
	}
}

// waitOpen waits up to 2 s for the table to report n open flows.
func waitOpen(t *testing.T, tb *Table, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for tb.Stats().Open != n {
		if time.Now().After(deadline) {
			t.Fatalf("Open = %d after 2 s, want %d", tb.Stats().Open, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Case 1: the first payload reaches the target, and its reply comes back through send with the same
// flow id.
func TestFlowEchoesFirstPayload(t *testing.T) {
	defer failOnPanic(t)
	target := listen(t)
	send, got := recorder()
	tb := NewTable(config(), send, resolver(target), time.Now)
	t.Cleanup(tb.Close)

	tb.OnFlow(protocol.Flow{Flow: 7, Service: "echo", Payload: []byte("hello")})
	payload, from := read(t, target)
	if string(payload) != "hello" {
		t.Fatalf("target got %q, want the first payload %q", payload, "hello")
	}
	write(t, target, []byte("hello back"), from)
	back := nextSent(t, got)
	if back.flow != 7 || string(back.payload) != "hello back" {
		t.Fatalf("send got flow %d payload %q, want flow 7 payload %q", back.flow, back.payload, "hello back")
	}
}

// Case 2: a datagram from any source other than the target never reaches send, because the flow's
// socket is connected to the target.
func TestOtherSourceNeverReachesSend(t *testing.T) {
	defer failOnPanic(t)
	target := listen(t)
	send, got := recorder()
	tb := NewTable(config(), send, resolver(target), time.Now)
	t.Cleanup(tb.Close)

	tb.OnFlow(protocol.Flow{Flow: 7, Service: "echo", Payload: []byte("hello")})
	_, flowAddr := read(t, target) // the flow's own socket
	write(t, target, []byte("hello back"), flowAddr)
	nextSent(t, got) // the flow works, so the silence below means something

	stranger := listen(t)
	write(t, stranger, []byte("intruder"), flowAddr)
	select {
	case s := <-got:
		t.Fatalf("a datagram from another source reached send: flow %d payload %q", s.flow, s.payload)
	case <-time.After(300 * time.Millisecond):
	}
}

// Case 3: a datagram over maxDatagram is dropped and counted as TooLarge; one at maxDatagram passes.
func TestOversizeDatagramDropped(t *testing.T) {
	defer failOnPanic(t)
	target := listen(t)
	tb := NewTable(config(), discard, resolver(target), time.Now)
	t.Cleanup(tb.Close)

	tb.OnFlow(protocol.Flow{Flow: 7, Service: "echo", Payload: []byte("hello")})
	read(t, target) // the first payload
	tb.OnDatagram(7, make([]byte, maxDatagram+1))
	tb.OnDatagram(7, make([]byte, maxDatagram))
	payload, _ := read(t, target)
	if len(payload) != maxDatagram {
		t.Fatalf("target got a %d-byte datagram, want the %d-byte one: the oversize one must not be sent", len(payload), maxDatagram)
	}
	if n := tb.Stats().Dropped.TooLarge; n != 1 {
		t.Fatalf("Dropped.TooLarge = %d, want 1", n)
	}
}

// Case 4: a flow with no datagram either way for 60 s closes, and a service idle of 5 s closes it in
// 5 s. The clock is fake, so the test moves time by hand.
func TestIdleFlowCloses(t *testing.T) {
	t.Run("60 s with no datagram either way", func(t *testing.T) {
		defer failOnPanic(t)
		target := listen(t)
		clock := &fakeClock{now: time.Unix(0, 0)}
		tb := NewTable(config(), discard, resolver(target), clock.Now)
		t.Cleanup(tb.Close)

		tb.OnFlow(protocol.Flow{Flow: 7, Service: "echo", Payload: []byte("hello")})
		read(t, target)
		clock.Advance(30 * time.Second)
		tb.OnDatagram(7, []byte("ping")) // a datagram restarts the 60 s
		read(t, target)
		clock.Advance(59 * time.Second) // 59 s after the last datagram
		if n := tb.Stats().Open; n != 1 {
			t.Fatalf("flow closed 59 s after its last datagram: Open = %d, want 1", n)
		}
		clock.Advance(time.Second) // 60 s after the last datagram
		waitOpen(t, tb, 0)
	})

	t.Run("service idle of 5 s", func(t *testing.T) {
		defer failOnPanic(t)
		target := listen(t)
		clock := &fakeClock{now: time.Unix(0, 0)}
		cfg := config()
		cfg.Idle = 5 * time.Second // the service's idle setting
		tb := NewTable(cfg, discard, resolver(target), clock.Now)
		t.Cleanup(tb.Close)

		tb.OnFlow(protocol.Flow{Flow: 7, Service: "echo", Payload: []byte("hello")})
		read(t, target)
		clock.Advance(4 * time.Second)
		if n := tb.Stats().Open; n != 1 {
			t.Fatalf("flow closed 4 s into a 5 s idle: Open = %d, want 1", n)
		}
		clock.Advance(time.Second)
		waitOpen(t, tb, 0)
	})
}

// Case 5: the 257th flow in a session is refused and counted, and so is the 4097th across tables.
func TestFlowLimits(t *testing.T) {
	t.Run("257th flow in a session", func(t *testing.T) {
		defer failOnPanic(t)
		target := listen(t)
		tb := NewTable(config(), discard, resolver(target), time.Now)
		t.Cleanup(tb.Close)

		for f := uint64(1); f <= 256; f++ {
			tb.OnFlow(protocol.Flow{Flow: f, Service: "echo", Payload: []byte("hi")})
		}
		if s := tb.Stats(); s.Open != 256 || s.Dropped.Limit != 0 {
			t.Fatalf("after 256 flows: Open = %d, Dropped.Limit = %d, want 256 and 0", s.Open, s.Dropped.Limit)
		}
		tb.OnFlow(protocol.Flow{Flow: 257, Service: "echo", Payload: []byte("hi")})
		if s := tb.Stats(); s.Open != 256 || s.Dropped.Limit != 1 {
			t.Fatalf("after the 257th flow: Open = %d, Dropped.Limit = %d, want 256 and 1", s.Open, s.Dropped.Limit)
		}
	})

	t.Run("4097th flow across tables", func(t *testing.T) {
		defer failOnPanic(t)
		target := listen(t)
		cfg := config() // one Total counter of 4096, shared by the tables below
		var tables []*Table
		for i := 0; i < 16; i++ {
			own := cfg
			own.PerSession = NewCounter(256) // each table is a session of its own, with its own cap
			tb := NewTable(own, discard, resolver(target), time.Now)
			t.Cleanup(tb.Close)
			for f := uint64(1); f <= 256; f++ {
				tb.OnFlow(protocol.Flow{Flow: f, Service: "echo", Payload: []byte("hi")})
			}
			tables = append(tables, tb)
		}
		open := 0
		for _, tb := range tables {
			open += tb.Stats().Open
		}
		if open != 4096 {
			t.Fatalf("%d flows open across 16 tables, want 4096", open)
		}

		extra := NewTable(cfg, discard, resolver(target), time.Now)
		t.Cleanup(extra.Close)
		extra.OnFlow(protocol.Flow{Flow: 1, Service: "echo", Payload: []byte("hi")})
		if s := extra.Stats(); s.Open != 0 || s.Dropped.Limit != 1 {
			t.Fatalf("4097th flow: Open = %d, Dropped.Limit = %d, want 0 and 1", s.Open, s.Dropped.Limit)
		}
	})
}

// Two tables of one session share its cap: together they hold no more than PerSession flows. A flow closed
// in one table frees its slot for the other. The cap here is 3, so the test is quick.
func TestPerSessionCapCountsAcrossTables(t *testing.T) {
	defer failOnPanic(t)
	target := listen(t)
	cfg := config()
	cfg.PerSession = NewCounter(3) // the session's cap, given to both of its tables
	a := NewTable(cfg, discard, resolver(target), time.Now)
	b := NewTable(cfg, discard, resolver(target), time.Now)
	t.Cleanup(a.Close)
	t.Cleanup(b.Close)

	a.OnFlow(protocol.Flow{Flow: 1, Service: "echo", Payload: []byte("hi")})
	a.OnFlow(protocol.Flow{Flow: 2, Service: "echo", Payload: []byte("hi")})
	b.OnFlow(protocol.Flow{Flow: 1, Service: "echo", Payload: []byte("hi")})
	if n := a.Stats().Open + b.Stats().Open; n != 3 {
		t.Fatalf("3 flows across the session's tables: open = %d, want 3", n)
	}

	b.OnFlow(protocol.Flow{Flow: 2, Service: "echo", Payload: []byte("hi")})
	a.OnFlow(protocol.Flow{Flow: 3, Service: "echo", Payload: []byte("hi")})
	if s := b.Stats(); s.Open != 1 || s.Dropped.Limit != 1 {
		t.Fatalf("b past the session cap: Open = %d, Dropped.Limit = %d, want 1 and 1", s.Open, s.Dropped.Limit)
	}
	if s := a.Stats(); s.Open != 2 || s.Dropped.Limit != 1 {
		t.Fatalf("a past the session cap: Open = %d, Dropped.Limit = %d, want 2 and 1", s.Open, s.Dropped.Limit)
	}

	a.Close()
	b.OnFlow(protocol.Flow{Flow: 2, Service: "echo", Payload: []byte("hi")})
	if s := b.Stats(); s.Open != 2 {
		t.Fatalf("after a is closed: b Open = %d, want 2: a closed table frees its session slots", s.Open)
	}
}

// Case 6: a flow for an unknown service is dropped and counted.
func TestUnknownServiceDropped(t *testing.T) {
	defer failOnPanic(t)
	target := listen(t)
	tb := NewTable(config(), discard, resolver(target), time.Now)
	t.Cleanup(tb.Close)

	tb.OnFlow(protocol.Flow{Flow: 7, Service: "nope", Payload: []byte("hi")})
	if n := tb.Stats().Open; n != 0 {
		t.Fatalf("a flow for an unknown service opened a socket: Open = %d, want 0", n)
	}
	if err := target.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := target.ReadFrom(make([]byte, 64)); err == nil {
		t.Fatal("the first payload of a flow for an unknown service reached a target")
	}
	if n := tb.Stats().Dropped.Unknown; n != 1 {
		t.Fatalf("Dropped.Unknown = %d, want 1", n)
	}
}

// Case 7: with the ordered queue full (LAN route), new datagrams are dropped and never delayed.
func TestFullOrderedQueueDropsNotDelays(t *testing.T) {
	defer failOnPanic(t)
	target := listen(t)
	release := make(chan struct{})
	send := func(uint64, []byte) { <-release } // a stalled ordered channel
	tb := NewTable(config(), send, resolver(target), time.Now)
	t.Cleanup(tb.Close)
	t.Cleanup(func() { close(release) }) // runs before tb.Close, so a blocked send can return

	tb.OnFlow(protocol.Flow{Flow: 7, Service: "echo", Payload: []byte("hi")})
	_, from := read(t, target)
	// 400 KB of replies against a 256 KiB queue, while send is still blocked on the first one.
	chunk := make([]byte, 1000)
	for i := 0; i < 400; i++ {
		write(t, target, chunk, from)
	}
	deadline := time.Now().Add(2 * time.Second)
	for tb.Stats().Dropped.QueueFull == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no datagram was dropped as QueueFull within 2 s: a full queue must drop, not delay")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// slowAddr is the target of the service "slow" in the tests below. Its dial is a hook that blocks until the
// test releases it, which stands in for a name lookup that takes seconds.
const slowAddr = "192.0.2.1:9"

// slowResolver maps "echo" to the target socket and "slow" to slowAddr, and rejects any other service.
func slowResolver(target net.PacketConn) func(string) (string, bool) {
	return func(service string) (string, bool) {
		switch service {
		case "echo":
			return target.LocalAddr().String(), true
		case "slow":
			return slowAddr, true
		}
		return "", false
	}
}

// Case 8 (review fix): a slow lookup for one flow stalls no other flow of the table. While the lookup is
// blocked, a datagram for an open flow still reaches its target, Stats and a new flow return at once, and
// the blocked flow holds its slot, so a flow past the session cap is refused.
func TestSlowLookupStallsNoOtherFlow(t *testing.T) {
	defer failOnPanic(t)
	target := listen(t)
	send, _ := recorder()
	release := make(chan struct{})
	entered := make(chan struct{})
	cfg := config()
	cfg.PerSession = NewCounter(2) // flow 1 and the slow flow 2 take both slots
	cfg.Dial = func(network, address string) (net.Conn, error) {
		if address == slowAddr {
			close(entered)
			<-release
		}
		return net.Dial(network, target.LocalAddr().String())
	}
	tb := NewTable(cfg, send, slowResolver(target), time.Now)
	t.Cleanup(tb.Close)

	tb.OnFlow(protocol.Flow{Flow: 1, Service: "echo", Payload: []byte("hello")})
	read(t, target) // the first payload of flow 1

	slowDone := make(chan struct{})
	go func() {
		tb.OnFlow(protocol.Flow{Flow: 2, Service: "slow", Payload: []byte("slow")})
		close(slowDone)
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the slow lookup did not start within 2 s")
	}

	// While the lookup is blocked, these must all return at once.
	ok := make(chan struct{})
	go func() {
		tb.OnDatagram(1, []byte("ping"))
		tb.Stats()
		tb.OnFlow(protocol.Flow{Flow: 3, Service: "echo", Payload: []byte("third")})
		close(ok)
	}()
	select {
	case <-ok:
	case <-time.After(time.Second):
		t.Fatal("datagrams, Stats and a new flow stalled behind a slow lookup of another flow")
	}
	if payload, _ := read(t, target); string(payload) != "ping" {
		t.Fatalf("target got %q while the lookup was blocked, want the datagram %q of flow 1", payload, "ping")
	}
	if s := tb.Stats(); s.Open != 1 || s.Dropped.Limit != 1 {
		t.Fatalf("while the slow lookup is blocked: Open = %d, Dropped.Limit = %d, want 1 and 1: the blocked flow holds its slot", s.Open, s.Dropped.Limit)
	}

	close(release)
	select {
	case <-slowDone:
	case <-time.After(2 * time.Second):
		t.Fatal("the slow flow did not return after its lookup was released")
	}
	waitOpen(t, tb, 2)
	if payload, _ := read(t, target); string(payload) != "slow" {
		t.Fatalf("target got %q from the slow flow, want its first payload %q", payload, "slow")
	}
	tb.OnDatagram(2, []byte("second"))
	if payload, _ := read(t, target); string(payload) != "second" {
		t.Fatalf("target got %q from the slow flow, want %q", payload, "second")
	}
}

// Case 9 (review fix): Close while a lookup is under way leaves no socket behind and gives the slot back.
func TestCloseDuringSlowLookupReleasesSlot(t *testing.T) {
	defer failOnPanic(t)
	target := listen(t)
	release := make(chan struct{})
	entered := make(chan struct{})
	dialed := make(chan net.Conn, 1)
	cfg := config()
	cfg.PerSession = NewCounter(1)
	cfg.Dial = func(network, address string) (net.Conn, error) {
		close(entered)
		<-release
		conn, err := net.Dial(network, target.LocalAddr().String())
		dialed <- conn
		return conn, err
	}
	tb := NewTable(cfg, discard, resolver(target), time.Now)

	done := make(chan struct{})
	go func() {
		tb.OnFlow(protocol.Flow{Flow: 7, Service: "echo", Payload: []byte("hi")})
		close(done)
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the lookup did not start within 2 s")
	}
	tb.Close()
	close(release)
	<-done

	conn := <-dialed
	if conn == nil {
		t.Fatal("the dial hook returned no socket")
	}
	if _, err := conn.Write([]byte("late")); err == nil {
		t.Fatal("a socket dialed after Close is still open")
	}
	// The slot is free again: a second table of the same session takes it.
	cfg.Dial = net.Dial
	next := NewTable(cfg, discard, resolver(target), time.Now)
	t.Cleanup(next.Close)
	next.OnFlow(protocol.Flow{Flow: 8, Service: "echo", Payload: []byte("hi")})
	waitOpen(t, next, 1)
}
