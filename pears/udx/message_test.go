package udx

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// Packet sizes measured from udx-native 1.21.3, the pinned JS reference (see docs/spike-m1.md): the
// stream's mtu is 1200, and an unordered send of n bytes goes out as one packet of n+20 bytes, type 8
// (MESSAGE). So the largest message that fits one 1200-byte packet is 1180 bytes.
const (
	upstreamMTU       = 1200
	upstreamHeaderLen = 20
)

// msg returns the i-th test message. It names its index, so a test can tell which messages arrived.
func msg(i int) []byte {
	return fmt.Appendf(nil, "message %06d", i)
}

// countingRelay forwards each datagram that reaches relay to dst, and drops each one with probability
// loss. It counts the MESSAGE packets before it drops anything, so the count is what the sender sent.
func countingRelay(loss float64, relay *net.UDPConn, dst *net.UDPAddr, sent *atomic.Int64) {
	rng := rand.New(rand.NewPCG(7, 1))
	buf := make([]byte, 65536)
	for {
		n, _, err := relay.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if h, _, err := DecodeHeader(buf[:n]); err == nil && h.Type&FlagMessage != 0 {
			sent.Add(1)
		}
		if rng.Float64() < loss {
			continue
		}
		relay.WriteToUDP(buf[:n], dst)
	}
}

// Messages sent over a perfect link all arrive, in any order.
func TestMessagesOverPerfectLinkAllArrive(t *testing.T) {
	defer failOnPanic(t)
	a, b := pairStreams(t, linkConfig{})
	msgs := b.Messages()
	const n = 100
	want := map[string]bool{}
	for i := range n {
		want[string(msg(i))] = true
		if err := a.SendMessage(msg(i)); err != nil {
			t.Fatalf("SendMessage %d: %v", i, err)
		}
	}
	for range n {
		select {
		case m := <-msgs:
			if !want[string(m)] {
				t.Fatalf("unexpected or repeated message %q", m)
			}
			delete(want, string(m))
		case <-time.After(ioTimeout):
			t.Fatalf("%d messages did not arrive", len(want))
		}
	}
}

// Over a link that drops 10% of datagrams, about 90% of the messages arrive, and none is sent again.
// The sender emits one packet per SendMessage, so a retransmission would show as an extra packet.
func TestMessagesOverLossyLinkAreNotRetransmitted(t *testing.T) {
	defer failOnPanic(t)
	cA, cB, relayConn := loopbackUDP(t), loopbackUDP(t), loopbackUDP(t)
	sockA, err := NewSocket(cA)
	if err != nil {
		t.Fatalf("NewSocket: %v", err)
	}
	t.Cleanup(func() { sockA.Close() })
	sockB, err := NewSocket(cB)
	if err != nil {
		t.Fatalf("NewSocket: %v", err)
	}
	t.Cleanup(func() { sockB.Close() })
	a := sockA.NewStream(1001)
	b := sockB.NewStream(2002)
	t.Cleanup(func() { a.Close(); b.Close() })
	if err := a.Connect(2002, relayConn.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	var sent atomic.Int64
	go countingRelay(0.10, relayConn, cB.LocalAddr().(*net.UDPAddr), &sent)

	msgs := b.Messages()
	const n = 1000
	arrived := make(chan int, 1)
	stop := make(chan struct{})
	go func() { // reads while the sender runs, so the receiver's queue does not back up
		got := 0
		for {
			select {
			case <-msgs:
				got++
			case <-stop:
				arrived <- got
				return
			}
		}
	}()
	for i := range n {
		if err := a.SendMessage(msg(i)); err != nil {
			t.Fatalf("SendMessage %d: %v", i, err)
		}
	}
	time.Sleep(500 * time.Millisecond) // messages are not acked, so give the last ones time to land
	close(stop)
	got := <-arrived
	if s := sent.Load(); s != n {
		t.Fatalf("sender emitted %d packets for %d messages, want one each", s, n)
	}
	if got < n*85/100 || got > n*95/100 {
		t.Fatalf("%d of %d messages arrived over a 10%% loss link, want about 90%%", got, n)
	}
}

// SendMessage of more than MaxMessage bytes returns an error. A message of exactly MaxMessage bytes
// is accepted and arrives whole.
func TestSendMessageLargerThanMaxMessageFails(t *testing.T) {
	defer failOnPanic(t)
	a, b := pairStreams(t, linkConfig{})
	limit := a.MaxMessage()
	if limit <= 0 || limit > upstreamMTU-upstreamHeaderLen {
		t.Fatalf("MaxMessage() = %d, want from 1 to %d (one packet of %d bytes less its %d-byte header)",
			limit, upstreamMTU-upstreamHeaderLen, upstreamMTU, upstreamHeaderLen)
	}
	msgs := b.Messages()
	if err := a.SendMessage(make([]byte, limit+1)); err == nil {
		t.Fatalf("SendMessage of %d bytes = nil, want an error above MaxMessage %d", limit+1, limit)
	}
	if err := a.SendMessage(make([]byte, limit)); err != nil {
		t.Fatalf("SendMessage of MaxMessage %d bytes: %v", limit, err)
	}
	select {
	case m := <-msgs:
		if len(m) != limit {
			t.Fatalf("message of %d bytes arrived, want %d", len(m), limit)
		}
	case <-time.After(ioTimeout):
		t.Fatal("message of MaxMessage bytes did not arrive")
	}
}

// Messages and stream bytes sent on one pair of streams interleave without corrupting either: every
// message arrives intact, once, and the stream bytes arrive intact and in order.
func TestMessagesInterleaveWithStreamBytes(t *testing.T) {
	defer failOnPanic(t)
	a, b := pairStreams(t, linkConfig{})
	msgs := b.Messages()
	const sends, chunk = 200, 5000
	data := payload(sends * chunk)
	wrote := make(chan error, 1)
	go func() {
		for i := range sends {
			if _, err := a.Write(data[i*chunk : (i+1)*chunk]); err != nil {
				wrote <- err
				return
			}
			if err := a.SendMessage(msg(i)); err != nil {
				wrote <- err
				return
			}
		}
		wrote <- a.CloseWrite()
	}()
	seen := make(chan map[string]int, 1)
	go func() {
		got := map[string]int{}
		for range sends {
			select {
			case m := <-msgs:
				got[string(m)]++
			case <-time.After(time.Minute):
				seen <- got
				return
			}
		}
		seen <- got
	}()

	stream, err := readWithin(t, b, len(data), time.Minute)
	if err != nil || !bytes.Equal(stream, data) {
		t.Fatalf("stream bytes: err %v, intact %v", err, bytes.Equal(stream, data))
	}
	got := <-seen
	for i := range sends {
		if c := got[string(msg(i))]; c != 1 {
			t.Fatalf("message %d arrived %d times, want once (%d distinct arrived)", i, c, len(got))
		}
	}
	if err := <-wrote; err != nil {
		t.Fatalf("Write, SendMessage or CloseWrite: %v", err)
	}
}

// SendMessage before Connect, and after the stream is destroyed, returns an error.
func TestSendMessageFailsBeforeConnectAndAfterDestroy(t *testing.T) {
	defer failOnPanic(t)
	sock, err := NewSocket(loopbackUDP(t))
	if err != nil {
		t.Fatalf("NewSocket: %v", err)
	}
	t.Cleanup(func() { sock.Close() })
	st := sock.NewStream(3003)
	if err := st.SendMessage(msg(0)); err == nil {
		t.Fatal("SendMessage before Connect = nil, want an error")
	}
	a, _ := pairStreams(t, linkConfig{})
	a.Destroy()
	if err := a.SendMessage(msg(0)); err == nil {
		t.Fatal("SendMessage after Destroy = nil, want an error")
	}
}

// Destroy closes the Messages channel, so a reader ends instead of waiting forever.
func TestMessagesChannelClosesOnDestroy(t *testing.T) {
	defer failOnPanic(t)
	a, _ := pairStreams(t, linkConfig{})
	msgs := a.Messages()
	a.Destroy()
	select {
	case _, ok := <-msgs:
		if ok {
			t.Fatal("Messages delivered a value after Destroy, want it closed")
		}
	case <-time.After(ioTimeout):
		t.Fatal("Messages is not closed after Destroy")
	}
}

// A MESSAGE packet is never stream data, even when the DATA flag is also set: its payload goes to
// Messages and the stream's receive state does not move.
func TestMessageFlagNeverReachesStreamData(t *testing.T) {
	defer failOnPanic(t)
	sender := loopbackUDP(t)
	conn := loopbackUDP(t)
	sock, err := NewSocket(conn)
	if err != nil {
		t.Fatalf("NewSocket: %v", err)
	}
	t.Cleanup(func() { sock.Close() })
	b := sock.NewStream(2002)
	t.Cleanup(func() { b.Close() })
	if err := b.Connect(1001, sender.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	pkt := EncodeHeader(Header{Type: FlagMessage | FlagData, RemoteID: 2002, RecvWindow: recvWindow}, []byte("hello"))
	sendUDP(t, sender, conn, pkt)
	select {
	case m := <-b.Messages():
		if string(m) != "hello" {
			t.Fatalf("message %q, want %q", m, "hello")
		}
	case <-time.After(ioTimeout):
		t.Fatal("MESSAGE packet with the DATA flag did not arrive on Messages")
	}
	b.mu.Lock()
	moved := len(b.rbuf) != 0 || b.rcvNxt != 0
	b.mu.Unlock()
	if moved {
		t.Fatal("MESSAGE packet with the DATA flag moved the stream's receive state")
	}
}
