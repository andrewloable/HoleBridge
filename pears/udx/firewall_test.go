package udx

import (
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// udxData returns a DATA packet for the stream id remoteID, with seq and payload, as a peer sends it.
func udxData(remoteID, seq uint32, payload []byte) []byte {
	return EncodeHeader(Header{Type: FlagData, RemoteID: remoteID, RecvWindow: recvWindow, Seq: seq}, payload)
}

// waitCount waits until n reaches want, and fails the test if it does not within d.
func waitCount(t *testing.T, n *atomic.Int32, want int32, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for n.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("the hook was called %d times, want %d", n.Load(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

// The first packet for an unconnected stream goes to its firewall hook, with the address the packet came from. The
// hook connects the stream there, and the packet is then processed: its payload is read.
func TestFirstPacketFirewallConnectsStream(t *testing.T) {
	defer failOnPanic(t)
	conn, peer := loopbackUDP(t), loopbackUDP(t)
	sock := newTestSocket(t, conn)
	st := sock.NewStream(1001)
	called := make(chan *net.UDPAddr, 8)
	st.SetFirewall(func(from *net.UDPAddr) bool {
		called <- from
		return st.Connect(2002, from) == nil
	})

	sendUDP(t, peer, conn, udxData(1001, 0, []byte("first")))
	got, err := readWithin(t, st, 5, promptly)
	if err != nil || string(got) != "first" {
		t.Fatalf("read after the hook connected the stream = %q, %v, want first", got, err)
	}
	select {
	case from := <-called:
		if from.Port != peer.LocalAddr().(*net.UDPAddr).Port {
			t.Errorf("the hook got address port %d, want the peer's %d", from.Port, peer.LocalAddr().(*net.UDPAddr).Port)
		}
	default:
		t.Fatal("the hook was not called for the first packet")
	}
	if n := len(called); n != 0 {
		t.Errorf("the hook was called %d more times, want once", n)
	}
}

// A false answer drops the packet, and the stream stays unconnected: the packet is not read. The next packet is
// offered to the hook again, and a true answer connects the stream and delivers it.
func TestFirstPacketFirewallFalseDropsPacket(t *testing.T) {
	defer failOnPanic(t)
	conn, peer := loopbackUDP(t), loopbackUDP(t)
	sock := newTestSocket(t, conn)
	st := sock.NewStream(1001)
	var calls atomic.Int32
	st.SetFirewall(func(from *net.UDPAddr) bool {
		if calls.Add(1) == 1 {
			return false
		}
		return st.Connect(2002, from) == nil
	})

	sendUDP(t, peer, conn, udxData(1001, 0, []byte("dropped")))
	waitCount(t, &calls, 1, promptly)
	sendUDP(t, peer, conn, udxData(1001, 0, []byte("kept")))
	got, err := readWithin(t, st, 4, promptly)
	if err != nil || string(got) != "kept" {
		t.Fatalf("read = %q, %v, want kept: the refused packet must be dropped", got, err)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("the hook was called %d times, want 2 (once per packet while unconnected)", n)
	}
}

// A connected stream never calls its hook: the packets it gets are processed as before.
func TestConnectedStreamDoesNotCallFirewall(t *testing.T) {
	defer failOnPanic(t)
	conn, peer := loopbackUDP(t), loopbackUDP(t)
	sock := newTestSocket(t, conn)
	st := sock.NewStream(1001)
	if err := st.Connect(2002, peer.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	var calls atomic.Int32
	st.SetFirewall(func(*net.UDPAddr) bool {
		calls.Add(1)
		return true
	})

	sendUDP(t, peer, conn, udxData(1001, 0, []byte("hello")))
	got, err := readWithin(t, st, 5, promptly)
	if err != nil || string(got) != "hello" {
		t.Fatalf("read = %q, %v, want hello", got, err)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("the hook was called %d times on a connected stream, want 0", n)
	}
}
