package hyperdht

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"testing"
	"time"
)

// dialWait bounds one Connect in these tests. Upstream (hyperdht 6.34.1, docs/spike-m1.md) fails a refused
// dial after about 8 s and an unlisted key at once; an admitted dial connects in about 2 ms on loopback.
const dialWait = 30 * time.Second

// readWait bounds one read from a connection in these tests.
const readWait = 15 * time.Second

// dial has d connect to the key pk with opts, and returns the connection, which the test closes when it
// ends. It fails the test on error, and reports a stub as not implemented.
func dial(t *testing.T, d *DHT, pk [32]byte, opts ConnectOptions) *Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), dialWait)
	defer cancel()
	c, err := d.Connect(ctx, pk, opts)
	if err != nil {
		failIfStub(t, err)
		t.Fatalf("Connect: %v", err)
	}
	if c == nil {
		t.Fatal("Connect returned no connection and no error")
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// dialErr has d connect to the key pk with opts, within wait, and returns the error that Connect gives. It
// fails the test when Connect returns a connection, and reports a stub as not implemented.
func dialErr(t *testing.T, d *DHT, pk [32]byte, opts ConnectOptions, wait time.Duration) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	c, err := d.Connect(ctx, pk, opts)
	if err == nil {
		if c != nil {
			c.Close()
		}
		t.Fatal("Connect returned a connection, want an error")
	}
	failIfStub(t, err)
	return err
}

// acceptResult is what one call to Accept returned.
type acceptResult struct {
	c   *AcceptedConn
	err error
}

// acceptNext calls srv.Accept in the background, so that a test can dial while it waits. The result
// arrives on the returned channel.
func acceptNext(srv *Server) <-chan acceptResult {
	ch := make(chan acceptResult, 1)
	go func() {
		c, err := srv.Accept()
		ch <- acceptResult{c, err}
	}()
	return ch
}

// awaitAccept returns the connection that acceptNext delivers on ch, and closes it when the test ends. It
// fails the test when Accept fails, or when no connection arrives within d.
func awaitAccept(t *testing.T, ch <-chan acceptResult, d time.Duration) *Conn {
	t.Helper()
	return awaitAcceptConn(t, ch, d).Conn
}

// awaitAcceptConn is awaitAccept for the accepted connection itself, with its route.
func awaitAcceptConn(t *testing.T, ch <-chan acceptResult, d time.Duration) *AcceptedConn {
	t.Helper()
	select {
	case r := <-ch:
		if r.err != nil {
			failIfStub(t, r.err)
			t.Fatalf("Accept: %v", r.err)
		}
		if r.c == nil {
			t.Fatal("Accept returned no connection and no error")
		}
		t.Cleanup(func() { r.c.Close() })
		return r.c
	case <-time.After(d):
		t.Fatalf("Accept returned no connection within %v", d)
	}
	return nil
}

// readWithin reads exactly n bytes from c. It fails the test when they do not arrive within readWait.
func readWithin(t *testing.T, c *Conn, n int) []byte {
	t.Helper()
	type result struct {
		b   []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		buf := make([]byte, n)
		_, err := io.ReadFull(c, buf)
		ch <- result{buf, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("read %d bytes: %v", n, r.err)
		}
		return r.b
	case <-time.After(readWait):
		t.Fatalf("read of %d bytes did not finish within %v", n, readWait)
	}
	return nil
}

// exchange sends a message from the client end c to the server end s, and a reply back. It fails the test
// unless each arrives intact.
func exchange(t *testing.T, c, s *Conn) {
	t.Helper()
	ping := []byte("ping from the client")
	pong := []byte("pong from the server")
	_, err := c.Write(ping)
	must(t, err)
	if got := readWithin(t, s, len(ping)); !bytes.Equal(got, ping) {
		t.Errorf("the server read %q, want %q", got, ping)
	}
	_, err = s.Write(pong)
	must(t, err)
	if got := readWithin(t, c, len(pong)); !bytes.Equal(got, pong) {
		t.Errorf("the client read %q, want %q", got, pong)
	}
}

// Test case 1: Connect to a listening server succeeds, and data flows both ways between the client's
// connection and the connection the server accepts.
func TestConnectToListeningServer(t *testing.T) {
	tn := startTestnet(t, 10)
	host := testKeyPair(3)
	client := testKeyPair(5)
	srv := newServer(t, tn.Nodes[0], ServerOptions{})
	listenOn(t, srv, host)
	accepted := acceptNext(srv)
	c := dial(t, tn.Nodes[9], host.Public, ConnectOptions{KeyPair: &client})
	s := awaitAccept(t, accepted, readWait)
	exchange(t, c, s)
}

// Test case 2: a client whose key pair the server's firewall refuses fails to connect with the refused
// error, not with the not-found error. The firewall is asked about the client's key, which shows that the
// refusal comes from the firewall.
func TestConnectRefusedByFirewallFails(t *testing.T) {
	tn := startTestnet(t, 10)
	host := testKeyPair(3)
	refused := testKeyPair(5)
	var mu sync.Mutex
	var asked [][32]byte // the keys that the firewall was asked about
	srv := newServer(t, tn.Nodes[0], ServerOptions{Firewall: func(pk [32]byte, _ HandshakePayload) bool {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, pk)
		return pk == refused.Public
	}})
	listenOn(t, srv, host)

	err := dialErr(t, tn.Nodes[9], host.Public, ConnectOptions{KeyPair: &refused}, dialWait)
	if !errors.Is(err, ErrPeerConnectionFailed) {
		t.Fatalf("Connect error = %v, want the refused error ErrPeerConnectionFailed", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(asked, refused.Public) {
		t.Error("the firewall was never asked about the refused key pair, so the refusal did not come from the firewall")
	}
}

// Test case 3: Connect to a public key that nobody listens on fails after the lookup finds no record, with
// the not-found error.
func TestConnectToUnlistedKeyFailsNotFound(t *testing.T) {
	tn := startTestnet(t, 10)
	nobody := testKeyPair(8)
	err := dialErr(t, tn.Nodes[9], nobody.Public, ConnectOptions{}, dialWait)
	if !errors.Is(err, ErrPeerNotFound) {
		t.Fatalf("Connect error = %v, want the not-found error ErrPeerNotFound", err)
	}
}

// Test case 4: the connection's RemotePublicKey is the server's public key.
func TestConnectionRemotePublicKeyIsServerKey(t *testing.T) {
	tn := startTestnet(t, 10)
	host := testKeyPair(3)
	client := testKeyPair(5)
	srv := newServer(t, tn.Nodes[0], ServerOptions{})
	listenOn(t, srv, host)
	accepted := acceptNext(srv)
	c := dial(t, tn.Nodes[9], host.Public, ConnectOptions{KeyPair: &client})
	awaitAccept(t, accepted, readWait)
	if c.RemotePublicKey() != host.Public {
		t.Error("RemotePublicKey of the connection is not the server's public key")
	}
}

// Added case: a dial whose context ends while the handshake is still unanswered fails with the context's
// error. The outcome is not known then, so neither the not-found nor the connection-failed error applies.
// The server refuses every key, so the handshake gets no answer and the 1 s context ends the dial first.
func TestConnectEndsWithItsContext(t *testing.T) {
	tn := startTestnet(t, 10)
	host := testKeyPair(3)
	client := testKeyPair(5)
	srv := newServer(t, tn.Nodes[0], ServerOptions{Firewall: func([32]byte, HandshakePayload) bool { return true }})
	listenOn(t, srv, host)
	err := dialErr(t, tn.Nodes[9], host.Public, ConnectOptions{KeyPair: &client}, time.Second)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Connect error = %v, want the context's deadline error", err)
	}
}
