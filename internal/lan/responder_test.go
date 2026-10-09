package lan

import (
	"context"
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
