package blindrelay

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/internal/testvec"
	"github.com/andrewloable/HoleBridge/pears/protomux"
	"github.com/andrewloable/HoleBridge/pears/udx"
)

// upstreamPairTimeout is how long HyperDHT waits for a relay pairing before it gives up
// (setTimeout(onabort, 15000) in hyperdht lib/connect.js and lib/server.js). blind-relay has no
// timeout of its own.
const upstreamPairTimeout = 15 * time.Second

// pairWait bounds how long a test waits for a Pair call that should match.
const pairWait = 10 * time.Second

// streamID is the local id of each UDX stream in these tests. Every stream has a socket of its own.
const streamID = 1

// pairToken is the token both peers ask for.
var pairToken = [32]byte{'b', 'l', 'i', 'n', 'd', '-', 'r', 'e', 'l', 'a', 'y', '-', 't', 'e', 's', 't'}

// clientIDA and clientIDB are the channel ids of two clients. Each stands for the public key of the relay
// connection of one peer, which is the id upstream hyperdht opens the channel under, and which the relay
// accepts the connection under. They differ, as two peers' keys do.
var (
	clientIDA = [32]byte{'b', 'l', 'i', 'n', 'd', '-', 'a'}
	clientIDB = [32]byte{'b', 'l', 'i', 'n', 'd', '-', 'b'}
)

// vectorFile is spec/vectors/blindrelay.json, written by spec/gen/blindrelay.js from blind-relay 1.6.1.
type vectorFile struct {
	Pair []struct {
		IsInitiator bool   `json:"is_initiator"`
		Token       string `json:"token"`
		ID          uint32 `json:"id"`
		Seq         uint32 `json:"seq"`
		Hex         string `json:"hex"`
	} `json:"pair"`
	Unpair []struct {
		Token string `json:"token"`
		Hex   string `json:"hex"`
	} `json:"unpair"`
}

// mustNoError fails the test on any error. A stub's errNotImplemented is reported as not implemented.
func mustNoError(t *testing.T, err error) {
	t.Helper()
	if errors.Is(err, errors.ErrUnsupported) {
		t.Fatal(err)
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// mustNotPanic runs f and fails the test, with the panic value as the message, if f panics. The
// stubs panic until the IMPL task lands; this keeps one stub call from ending the whole test binary.
func mustNotPanic(t *testing.T, f func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic: %v", r)
		}
	}()
	f()
}

// token32 decodes a 32-byte hex token from the vector.
func token32(t *testing.T, s string) [32]byte {
	t.Helper()
	b := testvec.Hex(t, s)
	if len(b) != 32 {
		t.Fatalf("token is %d bytes, want 32", len(b))
	}
	var out [32]byte
	copy(out[:], b)
	return out
}

// payload returns n pseudo-random bytes from a fixed seed, so a failure can be reproduced.
func payload(n int) []byte {
	b := make([]byte, n)
	seed := [32]byte{'b', 'l', 'i', 'n', 'd', '-', 'p', 'a', 'y', 'l', 'o', 'a', 'd'}
	rand.NewChaCha8(seed).Read(b)
	return b
}

// framedConn gives a net.Conn the framing the Mux expects: each Write goes out as one frame, a
// 4-byte big-endian length then the bytes, and each Read returns one whole frame.
type framedConn struct{ conn net.Conn }

func (f framedConn) Write(p []byte) (int, error) {
	buf := make([]byte, 4+len(p))
	binary.BigEndian.PutUint32(buf, uint32(len(p)))
	copy(buf[4:], p)
	if _, err := f.conn.Write(buf); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (f framedConn) Read(p []byte) (int, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(f.conn, hdr[:]); err != nil {
		return 0, err
	}
	n := int(binary.BigEndian.Uint32(hdr[:]))
	if n > len(p) {
		return 0, io.ErrShortBuffer
	}
	return io.ReadFull(f.conn, p[:n])
}

func (f framedConn) Close() error { return f.conn.Close() }

// newSocket opens a UDX socket on a loopback UDP port. It is closed when the test ends.
func newSocket(t *testing.T) (*udx.Socket, *net.UDPAddr) {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	sock, err := udx.NewSocket(conn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sock.Close() })
	return sock, conn.LocalAddr().(*net.UDPAddr)
}

// startRelay runs a server on a loopback UDX socket. It returns the server and the address that
// the clients' streams connect to.
func startRelay(t *testing.T) (*Server, *net.UDPAddr) {
	t.Helper()
	sock, addr := newSocket(t)
	var srv *Server
	mustNotPanic(t, func() { srv = NewServer(ServerOptions{Socket: sock}) })
	return srv, addr
}

// dial connects a new client to srv over a framed in-memory pipe, which is what the secret stream
// gives the mux upstream. id is the public key of that connection: the server accepts its end under it,
// and the client opens its channel under it, as upstream does. The server accepts before the client
// opens its channel.
func dial(t *testing.T, srv *Server, id []byte) *Client {
	t.Helper()
	c, _ := dialConn(t, srv, id)
	return c
}

// dialConn is dial that also returns the client's end of the connection, so a test can close it.
func dialConn(t *testing.T, srv *Server, id []byte) (*Client, net.Conn) {
	t.Helper()
	c1, c2 := net.Pipe()
	t.Cleanup(func() {
		c1.Close()
		c2.Close()
	})
	mustNoError(t, srv.Accept(protomux.New(framedConn{c2}), id))
	var c *Client
	mustNotPanic(t, func() { c = NewClient(protomux.New(framedConn{c1}), id) })
	return c, c1
}

// waitFreed waits up to 5 seconds for id to be free on sock, then frees it again.
func waitFreed(t *testing.T, sock *udx.Socket, id uint32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := sock.Register(id); err == nil {
			sock.Unregister(id)
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("relay stream id %d still registered 5 s after its pairing ended: %v", id, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// peer is one client of the relay: its Pair calls, its UDX socket, and the address its stream
// dials, which is the relay itself or a tap in front of it.
type peer struct {
	client *Client
	sock   *udx.Socket
	via    *net.UDPAddr
}

// pairResult is what a Pair call returned.
type pairResult struct {
	pairing *Pairing
	err     error
}

// pairAsync runs c.Pair in the background, so that two peers can wait for each other.
func pairAsync(c *Client, isInitiator bool, local *udx.Stream) <-chan pairResult {
	ch := make(chan pairResult, 1)
	go func() {
		p, err := c.Pair(isInitiator, pairToken, local)
		ch <- pairResult{p, err}
	}()
	return ch
}

// awaitPair waits up to pairWait for a Pair call to return. It fails the test if the call failed.
func awaitPair(t *testing.T, ch <-chan pairResult) *Pairing {
	t.Helper()
	select {
	case r := <-ch:
		mustNoError(t, r.err)
		return r.pairing
	case <-time.After(pairWait):
		t.Fatal("Pair did not return")
	}
	return nil
}

// pairPeers pairs peer a, as the initiator, with peer b under pairToken. Then it connects each local
// stream to the relay stream its pairing names, at the address its peer dials. Both Pair calls start
// before either is awaited: the server replies to a only once b has asked too.
func pairPeers(t *testing.T, a, b peer) (*udx.Stream, *udx.Stream) {
	t.Helper()
	la := a.sock.NewStream(streamID)
	lb := b.sock.NewStream(streamID)
	pa := pairAsync(a.client, true, la)
	pb := pairAsync(b.client, false, lb)
	ra := awaitPair(t, pa)
	rb := awaitPair(t, pb)
	mustNoError(t, la.Connect(ra.RemoteID, a.via))
	mustNoError(t, lb.Connect(rb.RemoteID, b.via))
	// The relay forwards to a side only after that side has sent a datagram: with one side silent,
	// upstream blind-relay 1.6.1 forwards nothing to it (measured with udx-native 1.21.3). So each side
	// sends one message first. Messages are not part of the byte stream the tests read.
	mustNoError(t, la.SendMessage([]byte("hello from a")))
	mustNoError(t, lb.SendMessage([]byte("hello from b")))
	return la, lb
}

// sendAndCheck writes data on from, and checks that to reads exactly those bytes within wait.
func sendAndCheck(t *testing.T, from, to *udx.Stream, data []byte, wait time.Duration) {
	t.Helper()
	werr := make(chan error, 1)
	go func() {
		_, err := from.Write(data)
		werr <- err
	}()
	got := make([]byte, len(data))
	rerr := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(to, got)
		rerr <- err
	}()
	select {
	case err := <-rerr:
		if err != nil {
			t.Fatalf("read of %d bytes: %v", len(data), err)
		}
	case <-time.After(wait):
		t.Fatalf("%d bytes did not arrive within %v", len(data), wait)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("the %d bytes that arrived differ from the bytes sent", len(data))
	}
	select {
	case err := <-werr:
		if err != nil {
			t.Fatalf("write of %d bytes: %v", len(data), err)
		}
	case <-time.After(wait):
		t.Fatal("Write did not return")
	}
}

// withRemoteID returns a copy of the UDX datagram d with its remote id, header bytes 4 to 8, set to id.
func withRemoteID(d []byte, id uint32) []byte {
	b := append([]byte(nil), d...)
	binary.LittleEndian.PutUint32(b[4:8], id)
	return b
}

// checkForwarded checks what the relay sent to one peer against what the other peer sent to the
// relay. Each datagram the relay sent must match a datagram the other peer sent, byte for byte,
// except for the UDX remote id, which the relay sets to the receiving peer's stream id.
func checkForwarded(t *testing.T, forwarded, sent [][]byte, peerStreamID uint32) {
	t.Helper()
	if len(forwarded) == 0 {
		t.Fatal("the relay forwarded no datagrams")
	}
	sentSet := make(map[string]bool, len(sent))
	for _, d := range sent {
		sentSet[string(withRemoteID(d, 0))] = true
	}
	for i, d := range forwarded {
		h, _, err := udx.DecodeHeader(d)
		if err != nil {
			t.Fatalf("datagram %d from the relay: %v", i, err)
		}
		if h.RemoteID != peerStreamID {
			t.Fatalf("datagram %d from the relay is for stream %d, want %d", i, h.RemoteID, peerStreamID)
		}
		if !sentSet[string(withRemoteID(d, 0))] {
			t.Fatalf("datagram %d from the relay (%d bytes, type %#x) matches no datagram the other peer sent",
				i, len(d), h.Type)
		}
	}
}

// tap is a UDP proxy between one peer and the relay. It forwards each datagram to the other side,
// and keeps a copy of it, so that the test can compare the raw bytes.
type tap struct {
	conn      *net.UDPConn
	relay     *net.UDPAddr
	mu        sync.Mutex
	peerAddr  *net.UDPAddr // the peer's address, from its first datagram
	toRelay   [][]byte
	fromRelay [][]byte
}

// newTap starts a tap in front of the relay at relay. It stops when the test ends.
func newTap(t *testing.T, relay *net.UDPAddr) *tap {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	tp := &tap{conn: conn, relay: relay}
	go tp.run()
	return tp
}

// addr is the address the peer dials.
func (tp *tap) addr() *net.UDPAddr { return tp.conn.LocalAddr().(*net.UDPAddr) }

// run forwards each datagram, from the relay to the peer or from the peer to the relay, until the
// connection closes.
func (tp *tap) run() {
	buf := make([]byte, 1<<16)
	for {
		n, from, err := tp.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		d := append([]byte(nil), buf[:n]...)
		tp.mu.Lock()
		var to *net.UDPAddr
		if from.Port == tp.relay.Port {
			tp.fromRelay = append(tp.fromRelay, d)
			to = tp.peerAddr
		} else {
			tp.peerAddr = from
			tp.toRelay = append(tp.toRelay, d)
			to = tp.relay
		}
		tp.mu.Unlock()
		if to != nil {
			_, _ = tp.conn.WriteToUDP(d, to)
		}
	}
}

// snapshot returns copies of the datagrams the peer sent to the relay, and of those the relay sent
// to the peer.
func (tp *tap) snapshot() (toRelay, fromRelay [][]byte) {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	return append([][]byte(nil), tp.toRelay...), append([][]byte(nil), tp.fromRelay...)
}

// TestMessagesRoundTripToVector checks that every pair and unpair message encodes to the bytes
// blind-relay 1.6.1 writes, and that those bytes decode back to the same fields.
func TestMessagesRoundTripToVector(t *testing.T) {
	var v vectorFile
	testvec.Load(t, "blindrelay.json", &v)
	if len(v.Pair) == 0 || len(v.Unpair) == 0 {
		t.Fatal("the vector has no pair or no unpair messages")
	}
	for i, e := range v.Pair {
		want := testvec.Hex(t, e.Hex)
		m := PairMessage{IsInitiator: e.IsInitiator, Token: token32(t, e.Token), ID: e.ID, Seq: e.Seq}
		got, err := EncodePair(m)
		mustNoError(t, err)
		if !bytes.Equal(got, want) {
			t.Errorf("pair %d encodes to %x, want %x", i, got, want)
		}
		back, err := DecodePair(want)
		mustNoError(t, err)
		if back != m {
			t.Errorf("pair %d decodes to %+v, want %+v", i, back, m)
		}
	}
	for i, e := range v.Unpair {
		want := testvec.Hex(t, e.Hex)
		m := UnpairMessage{Token: token32(t, e.Token)}
		got, err := EncodeUnpair(m)
		mustNoError(t, err)
		if !bytes.Equal(got, want) {
			t.Errorf("unpair %d encodes to %x, want %x", i, got, want)
		}
		back, err := DecodeUnpair(want)
		mustNoError(t, err)
		if back != m {
			t.Errorf("unpair %d decodes to %+v, want %+v", i, back, m)
		}
	}
}

// TestTwoClientsPairedThroughServerJoinStreams pairs two clients through one server. Each client opens
// its channel under its own id, as two upstream peers do, and the server accepts each connection under the
// same id. The clients pair, and their streams are joined: 10 MB written on one arrives intact at the
// other, and a reply comes back.
func TestTwoClientsPairedThroughServerJoinStreams(t *testing.T) {
	srv, relayAddr := startRelay(t)
	sockA, _ := newSocket(t)
	sockB, _ := newSocket(t)
	a := peer{client: dial(t, srv, clientIDA[:]), sock: sockA, via: relayAddr}
	b := peer{client: dial(t, srv, clientIDB[:]), sock: sockB, via: relayAddr}

	la, lb := pairPeers(t, a, b)
	sendAndCheck(t, la, lb, payload(10<<20), 2*time.Minute)
	sendAndCheck(t, lb, la, []byte("reply from the second client"), 30*time.Second)
}

// TestUnmatchedPairingTimesOutAsUpstream asks for a pairing that no other client asks for. Pair
// must give up after the upstream relay timeout, not wait forever.
func TestUnmatchedPairingTimesOutAsUpstream(t *testing.T) {
	srv, _ := startRelay(t)
	sock, _ := newSocket(t)
	c := dial(t, srv, clientIDA[:])
	local := sock.NewStream(streamID)

	start := time.Now()
	p, err := c.Pair(true, pairToken, local)
	elapsed := time.Since(start)

	if errors.Is(err, errors.ErrUnsupported) {
		t.Fatal(err)
	}
	if err == nil {
		t.Fatalf("Pair returned %+v and no error; want a timeout", p)
	}
	if elapsed < upstreamPairTimeout-time.Second || elapsed > upstreamPairTimeout+5*time.Second {
		t.Fatalf("Pair gave up after %v; want about %v", elapsed, upstreamPairTimeout)
	}
}

// TestServerForwardsPacketsUnchanged checks that the server never decrypts: it forwards the bytes
// unchanged. Taps in front of the relay keep the raw datagrams. Every datagram the relay sends to
// a peer must be one the other peer sent to the relay, apart from the UDX remote id.
func TestServerForwardsPacketsUnchanged(t *testing.T) {
	srv, relayAddr := startRelay(t)
	tapA := newTap(t, relayAddr)
	tapB := newTap(t, relayAddr)
	sockA, _ := newSocket(t)
	sockB, _ := newSocket(t)
	a := peer{client: dial(t, srv, clientIDA[:]), sock: sockA, via: tapA.addr()}
	b := peer{client: dial(t, srv, clientIDB[:]), sock: sockB, via: tapB.addr()}

	la, lb := pairPeers(t, a, b)
	sendAndCheck(t, la, lb, payload(256<<10), 30*time.Second)
	sendAndCheck(t, lb, la, payload(256<<10), 30*time.Second)

	// Each datagram the relay sent to a peer was recorded by the other peer's tap before the relay
	// could send it on. So the relay's sends are read first, and the peer's sends after.
	_, relayToB := tapB.snapshot()
	aToRelay, _ := tapA.snapshot()
	checkForwarded(t, relayToB, aToRelay, streamID)

	_, relayToA := tapA.snapshot()
	bToRelay, _ := tapB.snapshot()
	checkForwarded(t, relayToA, bToRelay, streamID)
}

// When a pairing ends, the relay frees the two stream ids it registered for it. The pairing ends here
// when one client's connection to the relay closes, which closes that side's session.
func TestPairingEndFreesRelayStreamIDs(t *testing.T) {
	srv, relayAddr := startRelay(t)
	sockA, _ := newSocket(t)
	sockB, _ := newSocket(t)
	cA, connA := dialConn(t, srv, clientIDA[:])
	cB := dial(t, srv, clientIDB[:])
	la := sockA.NewStream(streamID)
	lb := sockB.NewStream(streamID)
	// Both Pair calls start before either is awaited: the server replies to A only once B has asked.
	pa := pairAsync(cA, true, la)
	pb := pairAsync(cB, false, lb)
	ra := awaitPair(t, pa)
	rb := awaitPair(t, pb)
	mustNoError(t, la.Connect(ra.RemoteID, relayAddr))
	mustNoError(t, lb.Connect(rb.RemoteID, relayAddr))
	mustNoError(t, la.SendMessage([]byte("hello from a")))
	mustNoError(t, lb.SendMessage([]byte("hello from b")))
	sendAndCheck(t, la, lb, payload(64<<10), 30*time.Second)

	ids := []uint32{ra.RemoteID, rb.RemoteID}
	for _, id := range ids {
		if _, err := srv.sock.Register(id); err == nil {
			t.Fatalf("relay stream id %d is free while its pairing is up", id)
		}
	}

	connA.Close()
	for _, id := range ids {
		waitFreed(t, srv.sock, id)
	}
}
