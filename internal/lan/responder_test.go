package lan

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/internal/testvec"
)

// lanTCPPort is the host's LAN TCP port that the responder reports. It is the provisional default
// in internal/config; HoleBridge-dxe sets the final value. The tests pass it in, so they hold for
// any port.
const lanTCPPort = 27421

const (
	// replyWait is long enough for a reply on loopback, which takes microseconds.
	replyWait = 2 * time.Second
	// silenceWait is how long a test waits for a reply that must not come.
	silenceWait = 300 * time.Millisecond
	// serveCheckWait is how long startResponder waits to see whether Serve returns at once.
	serveCheckWait = 50 * time.Millisecond
)

// failIfStub fails the test as not implemented when err is the errors.ErrUnsupported a stub returns.
func failIfStub(t *testing.T, err error) {
	t.Helper()
	if errors.Is(err, errors.ErrUnsupported) {
		t.Fatal("not implemented")
	}
}

// vectorNow is the host's wall clock for the responder tests: the time of the first vector probe,
// so every vector probe is fresh.
func vectorNow(v lanVectors) func() time.Time {
	now := time.UnixMilli(int64(v.Probes[0].TimestampMs))
	return func() time.Time { return now }
}

// startResponder runs a Responder for lanKey on a loopback UDP socket until the test ends. It
// returns the socket that sends the probes and the responder's address. A Serve that returns while
// the test runs fails the test at once, so a stub cannot pass a silence check.
func startResponder(t *testing.T, lanKey [32]byte, now func() time.Time) (client net.PacketConn, server net.Addr) {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	r := NewResponder(lanKey, lanTCPPort, conn, now)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- r.Serve(ctx) }()
	select {
	case err := <-done:
		failIfStub(t, err)
		t.Fatalf("Serve returned before the test ended: %v", err)
	case <-time.After(serveCheckWait):
	}
	client, err = net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return client, conn.LocalAddr()
}

// exchange sends b from client to server and returns the reply, or false when none arrives within
// wait.
func exchange(t *testing.T, client net.PacketConn, server net.Addr, b []byte, wait time.Duration) ([]byte, bool) {
	t.Helper()
	if _, err := client.WriteTo(b, server); err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := client.SetReadDeadline(time.Now().Add(wait)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	buf := make([]byte, 1500)
	n, _, err := client.ReadFrom(buf)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return buf[:n], true
}

// Case 4: a valid probe gets a 64-byte reply that VerifyReply accepts, with the host's TCP port.
func TestResponderAnswersValidProbe(t *testing.T) {
	defer failOnPanic(t)
	v := loadLANVectors(t)
	p := v.Probes[0]
	key := vectorKey(t, p.LanKey)
	client, server := startResponder(t, key, vectorNow(v))
	reply, ok := exchange(t, client, server, testvec.Hex(t, p.Hex), replyWait)
	if !ok {
		t.Fatal("no reply to a valid probe")
	}
	if len(reply) != 64 {
		t.Fatalf("reply is %d bytes, want 64", len(reply))
	}
	port, ok := VerifyReply(key, reply, vectorNonce(t, p.Nonce))
	if !ok {
		t.Fatal("VerifyReply rejects the reply to a valid probe")
	}
	if port != lanTCPPort {
		t.Errorf("reply port = %d, want %d", port, lanTCPPort)
	}
}

// Case 5: the same probe sent twice gets one reply.
func TestResponderAnswersReplayOnce(t *testing.T) {
	defer failOnPanic(t)
	v := loadLANVectors(t)
	p := v.Probes[0]
	client, server := startResponder(t, vectorKey(t, p.LanKey), vectorNow(v))
	probe := testvec.Hex(t, p.Hex)
	if _, ok := exchange(t, client, server, probe, replyWait); !ok {
		t.Fatal("no reply to the first probe")
	}
	if _, ok := exchange(t, client, server, probe, silenceWait); ok {
		t.Error("the replayed probe got a second reply")
	}
}

// Case 6: invalid probes get no reply. A valid probe after them still gets its reply, so the
// silence is not a responder that has stopped.
func TestResponderSilentToInvalidProbes(t *testing.T) {
	defer failOnPanic(t)
	v := loadLANVectors(t)
	client, server := startResponder(t, vectorKey(t, v.Probes[0].LanKey), vectorNow(v))
	for i, bad := range v.Invalid {
		if _, ok := exchange(t, client, server, testvec.Hex(t, bad.Hex), silenceWait); ok {
			t.Errorf("invalid case %d (%s) got a reply", i, bad.Reason)
		}
	}
	// Probe 1 has the same key as probe 0 and a nonce no invalid case uses.
	p := v.Probes[1]
	if _, ok := exchange(t, client, server, testvec.Hex(t, p.Hex), replyWait); !ok {
		t.Error("a valid probe after the invalid ones got no reply")
	}
}

// Case 7: a reply is never longer than the probe that caused it.
func TestReplyNeverLongerThanProbe(t *testing.T) {
	defer failOnPanic(t)
	v := loadLANVectors(t)
	for i, p := range v.Probes {
		raw := testvec.Hex(t, p.Hex)
		client, server := startResponder(t, vectorKey(t, p.LanKey), vectorNow(v))
		reply, ok := exchange(t, client, server, raw, replyWait)
		if !ok {
			t.Fatalf("probe %d (%d bytes): no reply", i, len(raw))
		}
		if len(reply) > len(raw) {
			t.Errorf("probe %d (%d bytes): reply is %d bytes", i, len(raw), len(reply))
		}
	}
}

// fakeClock is a clock the test moves by hand.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

// nonceOf returns a distinct nonce for i.
func nonceOf(i int) [16]byte {
	var n [16]byte
	binary.LittleEndian.PutUint32(n[:], uint32(i))
	return n
}

// Case 10: a seen nonce stays in the replay cache for 48 h on the monotonic clock and is dropped
// after that. The bound is 48 h, not 24 h: a probe timestamped 24 h ahead is still fresh 48 h after it
// was first seen, so an earlier expiry would let a replay through.
func TestReplayCacheExpiresAfter48h(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1767225600, 0)}
	r := NewResponder([32]byte{}, lanTCPPort, nil, clock.Now)
	r.mono = clock.Now
	n := nonceOf(1)
	if !r.remember(n) {
		t.Fatal("a new nonce was not remembered")
	}
	clock.Advance(47*time.Hour + 59*time.Minute)
	if r.remember(n) {
		t.Fatal("a nonce seen 47 h 59 min ago was forgotten")
	}
	clock.Advance(time.Minute) // exactly 48 h since the first sighting
	if r.remember(n) {
		t.Fatal("a nonce seen exactly 48 h ago was forgotten: it is still fresh at that age")
	}
	clock.Advance(time.Second) // more than 48 h
	if !r.remember(n) {
		t.Fatal("a nonce seen more than 48 h ago is still in the replay cache")
	}
}

// Case 11: the replay cache holds at most 100000 nonces, and the oldest one goes first.
func TestReplayCacheCapEvictsOldest(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1767225600, 0)}
	r := NewResponder([32]byte{}, lanTCPPort, nil, clock.Now)
	r.mono = clock.Now
	for i := 0; i < maxSeen; i++ {
		if !r.remember(nonceOf(i)) {
			t.Fatalf("nonce %d was not new", i)
		}
	}
	if len(r.order) != maxSeen || len(r.seen) != maxSeen {
		t.Fatalf("cache holds %d and %d entries, want %d", len(r.order), len(r.seen), maxSeen)
	}
	if !r.remember(nonceOf(maxSeen)) {
		t.Fatal("a new nonce was not remembered")
	}
	if len(r.order) != maxSeen || len(r.seen) != maxSeen {
		t.Fatalf("cache holds %d and %d entries after one more, want %d", len(r.order), len(r.seen), maxSeen)
	}
	if r.remember(nonceOf(maxSeen - 1)) {
		t.Fatal("the newest nonce was evicted: it is new again")
	}
	if !r.remember(nonceOf(0)) {
		t.Fatal("the oldest nonce is still in the replay cache after the cap evicted it")
	}
}
