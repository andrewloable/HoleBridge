package udx

import (
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// newTestSocket wraps conn in a Socket that is closed when the test ends.
func newTestSocket(t *testing.T, conn *net.UDPConn) *Socket {
	t.Helper()
	s, err := NewSocket(conn)
	if err != nil {
		t.Fatalf("NewSocket: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// relayedPair is pairStreams with the sockets, conns and relays returned. Stream a (id 1001) is on sockA, whose conn is
// cA, and b (id 2002) is on sockB, whose conn is cB. Each one reaches the other only through a relay in this process,
// rA for a and rB for b, which applies cfg to every datagram. Closing rA and rB ends the relays.
func relayedPair(t *testing.T, cfg linkConfig) (a, b *Stream, sockA, sockB *Socket, cA, cB, rA, rB *net.UDPConn) {
	t.Helper()
	cA, cB = loopbackUDP(t), loopbackUDP(t)
	rA, rB = loopbackUDP(t), loopbackUDP(t)
	sockA, sockB = newTestSocket(t, cA), newTestSocket(t, cB)
	go relay(cfg, 1, rA, rB, cB.LocalAddr().(*net.UDPAddr))
	go relay(cfg, 2, rB, rA, cA.LocalAddr().(*net.UDPAddr))

	a = sockA.NewStream(1001)
	b = sockB.NewStream(2002)
	t.Cleanup(func() { a.Close(); b.Close() })
	if err := a.Connect(2002, rA.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := b.Connect(1001, rB.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	return a, b, sockA, sockB, cA, cB, rA, rB
}

// waitChanged waits for the channel ChangeRemote returned, which closes once the packets sent to the old remote are
// acked. It fails the test if that takes longer than ioTimeout.
func waitChanged(t *testing.T, changed <-chan struct{}) {
	t.Helper()
	select {
	case <-changed:
	case <-time.After(ioTimeout):
		t.Fatal("the old remote's packets were not acked after the change")
	}
}

// waitRemoteAcked waits until every packet st has sent is acked by the peer. It fails the test after d.
func waitRemoteAcked(t *testing.T, st *Stream, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		st.mu.Lock()
		acked := st.remoteAcked == st.seq
		st.mu.Unlock()
		if acked {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("not every packet was acked within %v", d)
		}
		time.Sleep(time.Millisecond)
	}
}

// changeBothEnds moves a relayed pair onto a direct path while a bulk transfer is running. Each end changes its remote
// to the other's new address, and with moveSockets each end also moves to a new socket of its own. Data goes on through
// the relays for a while, while the packets sent before the change are acked. Once they are, the relays are closed, so
// the rest of the data can only arrive over the direct path. Every byte must arrive intact, in order and once.
func changeBothEnds(t *testing.T, moveSockets bool) {
	defer failOnPanic(t)
	cfg := linkConfig{Loss: 0.02, Reorder: 0.02, Delay: time.Millisecond, Seed: 11}
	a, b, sockA, sockB, cA, cB, rA, rB := relayedPair(t, cfg)

	data := payload(4 << 20)
	werr := make(chan error, 1)
	go func() {
		if _, err := a.Write(data); err != nil {
			werr <- err
			return
		}
		werr <- a.CloseWrite()
	}()

	first := len(data) / 4
	got, err := readWithin(t, b, first, time.Minute)
	if err != nil {
		t.Fatalf("read before the change: %v", err)
	}
	if string(got) != string(data[:first]) {
		t.Fatal("bytes before the change differ from what was sent")
	}

	a.mu.Lock()
	inFlight := a.seq != a.remoteAcked || len(a.wq) > 0
	a.mu.Unlock()
	if !inFlight {
		t.Fatal("nothing was in flight or queued at the change, so the test does not change a stream mid-transfer")
	}

	newA, newB := cA, cB
	sockANew, sockBNew := sockA, sockB
	if moveSockets {
		newA, newB = loopbackUDP(t), loopbackUDP(t)
		sockANew, sockBNew = newTestSocket(t, newA), newTestSocket(t, newB)
	}
	changedA, err := a.ChangeRemote(sockANew, 2002, newB.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatalf("a.ChangeRemote: %v", err)
	}
	changedB, err := b.ChangeRemote(sockBNew, 1001, newA.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatalf("b.ChangeRemote: %v", err)
	}

	// The next stretch goes on while the old path may still carry packets.
	second := len(data) / 4
	got, err = readWithin(t, b, second, time.Minute)
	if err != nil {
		t.Fatalf("read after the change: %v", err)
	}
	if string(got) != string(data[first:first+second]) {
		t.Fatal("bytes after the change differ from what was sent: lost, duplicated or reordered")
	}

	// Once the packets sent before the change are acked, nothing is left on the old path: close the relays.
	waitChanged(t, changedA)
	waitChanged(t, changedB)
	rA.Close()
	rB.Close()

	rest, err := readWithin(t, b, len(data)-first-second, time.Minute)
	if err != nil {
		t.Fatalf("read over the direct path: %v", err)
	}
	if string(rest) != string(data[first+second:]) {
		t.Fatal("bytes over the direct path differ from what was sent: lost, duplicated or reordered")
	}
	if _, err := readWithin(t, b, 1, ioTimeout); err != io.EOF {
		t.Fatalf("read after the data = %v, want io.EOF", err)
	}
	if err := <-werr; err != nil {
		t.Fatalf("Write or CloseWrite: %v", err)
	}

	if moveSockets {
		// libudx routes a stream by its id, so the old socket still delivers a's packets after the change. A pure ack
		// sent to a's id on the old socket must reach a: it sets a's peer window to probeWindow, which no transfer
		// advertises.
		sendUDP(t, newB, cA, EncodeHeader(Header{RemoteID: 1001, RecvWindow: probeWindow}, nil))
		waitPeerWindow(t, a, probeWindow)
	}
}

// probeWindow is the receive window a test ack carries to show the stream got it. The transfers advertise recvWindow.
const probeWindow = 12345

// waitPeerWindow waits until st has taken a peer window of want from a packet. It fails the test after ioTimeout.
func waitPeerWindow(t *testing.T, st *Stream, want uint32) {
	t.Helper()
	deadline := time.Now().Add(ioTimeout)
	for {
		st.mu.Lock()
		got := st.peerWnd
		st.mu.Unlock()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stream never got a packet on its old socket: peer window %d, want %d", got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// After ChangeRemote to a new socket, the data moves on with no loss or duplication, and the old socket keeps routing
// the stream until the old path is acked (libudx routes the stream by its id across the udx instance).
func TestChangeRemoteKeepsDataFlowingAcrossSocketChange(t *testing.T) {
	changeBothEnds(t, true)
}

// ChangeRemote on the same socket only moves the stream to the new address, and the data goes on as before.
func TestChangeRemoteOnTheSameSocketKeepsDataFlowing(t *testing.T) {
	changeBothEnds(t, false)
}

// With nothing in flight, the change is done at once: the channel it returns is already closed, and the sequence,
// the window and the RTT state of the stream are the same as before, so the next bytes carry on from the same seq.
func TestChangeRemoteWithNothingInFlightIsDoneAtOnce(t *testing.T) {
	defer failOnPanic(t)
	a, b, _, _ := directPair(t)
	writeAndRead(t, a, b, payload(64<<10))
	waitRemoteAcked(t, a, ioTimeout)

	a.mu.Lock()
	sock, peer := a.sock, a.peer
	seq, acked, srtt, cwnd := a.seq, a.remoteAcked, a.rto.srttMS(), a.bf.cwnd
	a.mu.Unlock()
	if srtt == 0 {
		t.Fatal("no RTT sample after the first transfer; the test needs one")
	}

	changed, err := a.ChangeRemote(sock, 2002, peer)
	if err != nil {
		t.Fatalf("ChangeRemote: %v", err)
	}
	select {
	case <-changed:
	default:
		t.Fatal("the change with nothing in flight is not done at once")
	}

	a.mu.Lock()
	if a.seq != seq || a.remoteAcked != acked || a.rto.srttMS() != srtt || a.bf.cwnd != cwnd {
		t.Errorf("ChangeRemote reset state: seq %d->%d, remoteAcked %d->%d, srtt %d->%d, cwnd %d->%d",
			seq, a.seq, acked, a.remoteAcked, srtt, a.rto.srttMS(), cwnd, a.bf.cwnd)
	}
	a.mu.Unlock()

	writeAndRead(t, a, b, payload(64<<10))
}

// writeAndRead writes data on a and checks that b reads exactly data. Unlike sendAndCheck it leaves a's write side
// open, so more data can follow.
func writeAndRead(t *testing.T, a, b *Stream, data []byte) {
	t.Helper()
	werr := make(chan error, 1)
	go func() {
		_, err := a.Write(data)
		werr <- err
	}()
	got, err := readWithin(t, b, len(data), ioTimeout)
	if err != nil {
		t.Fatalf("read %d bytes: %v", len(data), err)
	}
	if string(got) != string(data) {
		t.Fatal("bytes read differ from what was written")
	}
	if err := <-werr; err != nil {
		t.Fatalf("Write: %v", err)
	}
}

// directPair returns two streams that reach each other on a direct loopback path, with no relay.
func directPair(t *testing.T) (a, b *Stream, sockA, sockB *Socket) {
	t.Helper()
	cA, cB := loopbackUDP(t), loopbackUDP(t)
	sockA, sockB = newTestSocket(t, cA), newTestSocket(t, cB)
	a = sockA.NewStream(1001)
	b = sockB.NewStream(2002)
	t.Cleanup(func() { a.Close(); b.Close() })
	if err := a.Connect(2002, cB.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := b.Connect(1001, cA.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	return a, b, sockA, sockB
}

// A packet sent before a change is resent to the remote it was first sent to, until the peer acks it, and a packet sent
// after the change goes to the new remote (libudx binds each packet's remote when it is first sent). The old remote here
// is a conn that takes the packets and never answers. The test plays the old path by forwarding one copy of each old
// packet to the new remote, which then acks everything, and the change completes.
func TestChangeRemoteResendsOldPacketsToTheOldRemote(t *testing.T) {
	defer failOnPanic(t)
	cA, cB, sink := loopbackUDP(t), loopbackUDP(t), loopbackUDP(t)
	sockA, sockB := newTestSocket(t, cA), newTestSocket(t, cB)
	a := sockA.NewStream(1001)
	b := sockB.NewStream(2002)
	t.Cleanup(func() { a.Close(); b.Close() })
	if err := a.Connect(2002, sink.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := b.Connect(1001, cA.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	var mu sync.Mutex
	var got [][]byte // every datagram the sink took, in arrival order
	go func() {
		buf := make([]byte, maxDatagram)
		for {
			n, _, err := sink.ReadFromUDP(buf)
			if err != nil {
				return
			}
			mu.Lock()
			got = append(got, append([]byte(nil), buf[:n]...))
			mu.Unlock()
		}
	}()
	sunk := func() [][]byte {
		mu.Lock()
		defer mu.Unlock()
		return append([][]byte(nil), got...)
	}

	first := payload(3 * mss) // seq 0 to 2, all to the old remote
	if _, err := a.Write(first); err != nil {
		t.Fatalf("Write: %v", err)
	}
	waitFor(t, "the first three packets at the old remote", func() bool { return len(sunk()) == 3 })

	changed, err := a.ChangeRemote(sockA, 2002, cB.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatalf("ChangeRemote: %v", err)
	}
	second := payload(2 * mss) // seq 3 and 4, to the new remote
	if _, err := a.Write(second); err != nil {
		t.Fatalf("Write after the change: %v", err)
	}
	waitFor(t, "packets 3 and 4 held by the new remote", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		_, has3 := b.ooo[3]
		_, has4 := b.ooo[4]
		return has3 && has4
	})

	// The resends after the change go to the old remote, and no new packet does.
	waitFor(t, "a resend of packet 0 at the old remote", func() bool {
		for _, d := range sunk()[3:] {
			if h, _, err := DecodeHeader(d); err == nil && h.Type&FlagData != 0 && h.Seq == 0 {
				return true
			}
		}
		return false
	})
	forwarded := map[uint32]bool{}
	for _, d := range sunk() {
		h, _, err := DecodeHeader(d)
		if err != nil || h.Type&FlagData == 0 {
			continue
		}
		if h.Seq >= 3 {
			t.Fatalf("packet %d went to the old remote; packets sent after the change must go to the new one", h.Seq)
		}
		if !forwarded[h.Seq] {
			forwarded[h.Seq] = true
			if _, err := sink.WriteToUDP(d, cB.LocalAddr().(*net.UDPAddr)); err != nil {
				t.Fatalf("forward packet %d: %v", h.Seq, err)
			}
		}
	}

	waitChanged(t, changed)
	want := append(append([]byte(nil), first...), second...)
	got2, err := readWithin(t, b, len(want), ioTimeout)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got2) != string(want) {
		t.Fatal("bytes differ from what was sent: lost, duplicated or reordered")
	}
}

// waitFor polls cond until it is true. It fails the test with what after ioTimeout.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(ioTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// A change that is refused leaves the stream as it was: it still carries data to the same peer.
func TestChangeRemoteRefusedLeavesTheStreamAsItWas(t *testing.T) {
	defer failOnPanic(t)
	a, b, sockA, _ := directPair(t)
	writeAndRead(t, a, b, payload(1024))
	closed := newTestSocket(t, loopbackUDP(t))
	closed.Close()
	a.mu.Lock()
	to := a.peer
	a.mu.Unlock()

	cases := []struct {
		name   string
		socket *Socket
		id     uint32
		addr   *net.UDPAddr
	}{
		{"nil socket", nil, 2002, to},
		{"closed socket", closed, 2002, to},
		{"nil address", sockA, 2002, nil},
		{"port zero", sockA, 2002, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := a.ChangeRemote(tc.socket, tc.id, tc.addr); err == nil {
				t.Fatal("ChangeRemote succeeded, want an error")
			}
		})
	}

	a.mu.Lock()
	peer, remoteID := a.peer, a.remoteID
	a.mu.Unlock()
	if peer.String() != to.String() || remoteID != 2002 {
		t.Fatalf("refused changes moved the stream to %v (id %d)", peer, remoteID)
	}
	writeAndRead(t, a, b, payload(32<<10))
}

// A stream that is not connected cannot change its remote.
func TestChangeRemoteNeedsAConnectedStream(t *testing.T) {
	defer failOnPanic(t)
	sock := newTestSocket(t, loopbackUDP(t))
	st := sock.NewStream(77)
	defer st.Close()
	addr := loopbackUDP(t).LocalAddr().(*net.UDPAddr)
	if _, err := st.ChangeRemote(sock, 5, addr); err == nil {
		t.Fatal("ChangeRemote on a stream that is not connected succeeded, want an error")
	}
}

// startPendingChange connects a (id 1001) on a new socket sockA to sink, writes size bytes that sink never acks, and
// moves a to sockN with a change that stays pending. It returns the channel of that change. Write returns once every
// byte is in a packet sent, so the packets the change must wait for are all sent when it is made.
func startPendingChange(t *testing.T, size int) (a *Stream, sockA, sockN *Socket, cA, cN, sink *net.UDPConn, changed <-chan struct{}) {
	t.Helper()
	cA, cN, sink = loopbackUDP(t), loopbackUDP(t), loopbackUDP(t)
	sockA, sockN = newTestSocket(t, cA), newTestSocket(t, cN)
	a = sockA.NewStream(1001)
	t.Cleanup(func() { a.Close() })
	if err := a.Connect(2002, sink.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if _, err := a.Write(payload(size)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	changed, err := a.ChangeRemote(sockN, 2002, sink.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatalf("ChangeRemote: %v", err)
	}
	select {
	case <-changed:
		t.Fatal("the change is done with a packet still unacked; it must be pending")
	default:
	}
	return a, sockA, sockN, cA, cN, sink, changed
}

// The new socket of a pending change closes while the old socket still routes the stream's id to the stream. A datagram
// for that id then reaches the old socket, which must not send on the closed channel: the datagram is dropped, and the
// stream tears down with its socket. The stream's goroutine cannot tear down until the test releases a.mu, so the
// datagram arrives first. Before the fix this panics with "send on closed channel" in the old socket's read loop, which
// kills the test binary.
func TestChangeRemoteNewSocketClosedWhileOldSocketRoutes(t *testing.T) {
	a, sockA, sockN, cA, _, sink, changed := startPendingChange(t, 100)

	a.mu.Lock()
	sockN.Close()
	sendUDP(t, sink, cA, EncodeHeader(Header{RemoteID: 1001, RecvWindow: 5000}, nil))
	time.Sleep(300 * time.Millisecond)
	a.mu.Unlock()

	select {
	case <-a.Done():
	case <-time.After(ioTimeout):
		t.Fatal("the stream did not tear down after its new socket closed")
	}
	waitChanged(t, changed)
	sockA.mu.Lock()
	n := len(sockA.aliases)
	sockA.mu.Unlock()
	if n != 0 {
		t.Fatalf("the old socket still has %d aliases after the stream tore down", n)
	}
}

// The mirror case: the old socket closes while the change is pending. It stops routing the stream, and the new socket
// keeps the stream's route, so a datagram for the id that reaches the new socket still gets to the stream.
func TestChangeRemoteOldSocketClosedFirstKeepsRoutingOnTheNewSocket(t *testing.T) {
	a, sockA, _, _, cN, sink, _ := startPendingChange(t, 100)

	sockA.Close()
	sendUDP(t, sink, cN, EncodeHeader(Header{RemoteID: 1001, RecvWindow: probeWindow}, nil))
	waitPeerWindow(t, a, probeWindow)
}

// assertStaysOpen fails the test if changed closes within d.
func assertStaysOpen(t *testing.T, changed <-chan struct{}, d time.Duration) {
	t.Helper()
	select {
	case <-changed:
		t.Fatal("the change closed while packets sent before it are unacked")
	case <-time.After(d):
	}
}

// A pending change is done only when the packets sent before it are acked, as libudx's remote_changed. Its channel stays
// open while they are unacked, also while new packets go out on the new remote. An ack below the change's seq does not
// close it; the ack that reaches the seq does.
func TestChangeRemoteChannelStaysOpenUntilOldPacketsAcked(t *testing.T) {
	a, _, _, _, cN, sink, changed := startPendingChange(t, 3*mss)
	a.mu.Lock()
	seq := a.changeSeq
	a.mu.Unlock()
	if seq != 3 {
		t.Fatalf("the change was made at seq %d, want 3: the test needs three packets sent before it", seq)
	}
	assertStaysOpen(t, changed, 100*time.Millisecond)

	if _, err := a.Write([]byte{1}); err != nil {
		t.Fatalf("Write after the change: %v", err)
	}
	assertStaysOpen(t, changed, 100*time.Millisecond)

	sendUDP(t, sink, cN, EncodeHeader(Header{RemoteID: 1001, Ack: seq - 1, RecvWindow: recvWindow}, nil))
	waitFor(t, "the peer to ack below the change's seq", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.remoteAcked == seq-1
	})
	assertStaysOpen(t, changed, 100*time.Millisecond)

	sendUDP(t, sink, cN, EncodeHeader(Header{RemoteID: 1001, Ack: seq, RecvWindow: recvWindow}, nil))
	waitChanged(t, changed)
	a.mu.Lock()
	pending := a.changed
	a.mu.Unlock()
	if pending != nil {
		t.Fatal("the stream still holds a pending change after it was done")
	}
}

// A ChangeRemote while an earlier change is pending is refused, and the stream stays on its socket and peer. Once the
// pending change is done, a new change is accepted.
func TestChangeRemoteSecondChangeWhilePendingFails(t *testing.T) {
	a, sockA, sockN, _, cN, sink, changed := startPendingChange(t, 100)
	other := loopbackUDP(t)
	otherAddr := other.LocalAddr().(*net.UDPAddr)
	sinkAddr := sink.LocalAddr().(*net.UDPAddr)

	if _, err := a.ChangeRemote(sockA, 3003, otherAddr); !errors.Is(err, errRemoteChanging) {
		t.Fatalf("second ChangeRemote while pending: err = %v, want errRemoteChanging", err)
	}
	a.mu.Lock()
	sock, peer, remoteID := a.sock, a.peer, a.remoteID
	a.mu.Unlock()
	if sock != sockN || peer.String() != sinkAddr.String() || remoteID != 2002 {
		t.Fatalf("the refused change moved the stream to socket %p, peer %v, id %d", sock, peer, remoteID)
	}
	sockA.mu.Lock()
	_, primary := sockA.streams[1001]
	sockA.mu.Unlock()
	if primary {
		t.Fatal("the refused change registered the stream on its old socket")
	}

	a.mu.Lock()
	acked := a.changeSeq
	a.mu.Unlock()
	sendUDP(t, sink, cN, EncodeHeader(Header{RemoteID: 1001, Ack: acked, RecvWindow: recvWindow}, nil))
	waitChanged(t, changed)

	sockM := newTestSocket(t, loopbackUDP(t))
	if _, err := a.ChangeRemote(sockM, 3003, otherAddr); err != nil {
		t.Fatalf("ChangeRemote after the pending change is done: %v", err)
	}
	a.mu.Lock()
	sock, remoteID = a.sock, a.remoteID
	a.mu.Unlock()
	if sock != sockM || remoteID != 3003 {
		t.Fatalf("the new change did not take: socket %p, id %d", sock, remoteID)
	}
}

// Teardown while a change is pending ends the change and releases both routes: the alias on the old socket and the
// route on the new one. A DESTROY from the peer tears the stream down the same way Close does.
func TestChangeRemoteTeardownReleasesRoutes(t *testing.T) {
	t.Run("Close", func(t *testing.T) {
		a, sockA, sockN, _, _, _, changed := startPendingChange(t, 100)
		a.Close()
		assertPendingChangeReleased(t, a, sockA, sockN, changed)
	})
	t.Run("peer DESTROY", func(t *testing.T) {
		a, sockA, sockN, _, cN, sink, changed := startPendingChange(t, 100)
		sendUDP(t, sink, cN, EncodeHeader(Header{Type: FlagDestroy, RemoteID: 1001}, nil))
		assertPendingChangeReleased(t, a, sockA, sockN, changed)
	})
}

// assertPendingChangeReleased checks that a torn-down stream is done, its pending change closed, and that neither socket
// routes to it any more.
func assertPendingChangeReleased(t *testing.T, a *Stream, sockA, sockN *Socket, changed <-chan struct{}) {
	t.Helper()
	select {
	case <-a.Done():
	case <-time.After(ioTimeout):
		t.Fatal("the stream was not torn down")
	}
	waitChanged(t, changed)
	sockA.mu.Lock()
	aliases := len(sockA.aliases)
	sockA.mu.Unlock()
	sockN.mu.Lock()
	streams := len(sockN.streams)
	sockN.mu.Unlock()
	if aliases != 0 || streams != 0 {
		t.Fatalf("after teardown the old socket has %d aliases and the new socket %d streams, want 0 and 0", aliases, streams)
	}
}

// Closing the old socket while a change is pending clears the old socket's aliases. The stream keeps its route on the new
// socket: it can still write there, and closing it afterwards ends the change.
func TestChangeRemoteOldSocketClosedClearsAliases(t *testing.T) {
	a, sockA, sockN, _, _, _, changed := startPendingChange(t, 100)
	sockA.Close()
	sockA.mu.Lock()
	aliases := len(sockA.aliases)
	sockA.mu.Unlock()
	if aliases != 0 {
		t.Fatalf("the old socket has %d aliases after it closed, want 0", aliases)
	}

	if _, err := a.Write([]byte{1}); err != nil {
		t.Fatalf("Write on the new socket after the old one closed: %v", err)
	}
	a.mu.Lock()
	sock := a.sock
	a.mu.Unlock()
	if sock != sockN {
		t.Fatal("the stream left its new socket when the old one closed")
	}

	a.Close()
	waitChanged(t, changed)
}
