package host

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/internal/config"
	"github.com/andrewloable/HoleBridge/internal/protocol"
)

// A session the host ends must destroy its transport, not only end its write side. The app here ignores the
// host's END and keeps writing: a destroyed transport refuses the writes, a half-closed one takes them.
func TestEndedSessionDestroysTransport(t *testing.T) {
	r := newRig(t)
	r.startHost(t, r.hostConfig(t, echoServices(t), nil), nil)
	a := r.rawConnect(t, protocol.FlagResume|protocol.FlagDatagrams)

	// A close for a stream the app never opened breaks the protocol, so the host ends the session.
	if err := a.write(protocol.Close{Stream: 99}); err != nil {
		t.Fatalf("write close: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := a.write(protocol.Close{Stream: 99}); err != nil {
			return // the transport is gone
		}
		if time.Now().After(deadline) {
			t.Fatal("the host kept the transport of an ended session: the app's writes still reach it")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Run returning ends every live connection. An app that ignores END can still write afterwards, and the host must
// act on none of it: here it would dial the echo target.
func TestRunShutdownDestroysLiveTransports(t *testing.T) {
	r := newRig(t)
	var dials atomic.Int32
	cfg := r.hostConfig(t, echoServices(t), nil)
	h := r.newWireHost(t, cfg, func(o *Options) {
		o.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			dials.Add(1)
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	a := r.rawConnect(t, protocol.FlagResume|protocol.FlagDatagrams)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(readWait):
		t.Fatalf("Run did not return within %v of its context ending", readWait)
	}
	// The write may fail, which is fine. What must not happen is the host opening the stream.
	_ = a.write(protocol.Open{Stream: 1, Service: "echo", Window: 1 << 20})
	time.Sleep(2 * time.Second)
	if n := dials.Load(); n != 0 {
		t.Fatalf("the host dialed %d target(s) after Run returned, want none", n)
	}
}

// ServeConn refuses a key other than the client key by closing the LAN stream. The close must reach the socket:
// a socket that only ends its write side keeps taking the app's bytes, where a closed one resets them.
func TestServeConnRefusalClosesSocket(t *testing.T) {
	r := newRig(t)
	h := r.newWireHost(t, r.hostConfig(t, echoServices(t), nil), nil)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	hostSide, appSide := r.lanPairTCP(t, kpOf(priv))
	serveConn(t, h, hostSide)

	for i := 0; i < 10; i++ {
		if _, err := appSide.Write(make([]byte, 1024)); err != nil {
			return // the host reset the socket
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("the host kept the socket open after refusing the key: the app's writes still succeed")
}

// closeRecorder is a target connection that reports its Close. Its CloseWrite is the real one, so a half-close
// reaches the target as it would without the recorder.
type closeRecorder struct {
	net.Conn
	onClose func()
}

func (c *closeRecorder) Close() error {
	c.onClose()
	return c.Conn.Close()
}

func (c *closeRecorder) CloseWrite() error {
	return c.Conn.(closeWriter).CloseWrite()
}

// silentTarget starts a TCP server that reads each connection to its end and never writes back. Its connections
// stay open until the test ends.
func silentTarget(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var mu sync.Mutex
	var held []net.Conn
	t.Cleanup(func() {
		l.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			c.Close()
		}
	})
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
			go io.Copy(io.Discard, c)
		}
	}()
	return l.Addr().String()
}

// The app half-closes a stream to a target that stays silent, then the session ends. The host's copy from the
// target waits for bytes that never come, so the target must be closed when the stream fails.
func TestSilentTargetClosedWhenSessionEnds(t *testing.T) {
	r := newRig(t)
	closed := make(chan struct{})
	var once sync.Once
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		c, err := d.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &closeRecorder{Conn: c, onClose: func() { once.Do(func() { close(closed) }) }}, nil
	}
	cfg := r.hostConfig(t, map[string]config.Service{"silent": {Target: silentTarget(t), Kind: "tcp"}}, nil)
	r.startHost(t, cfg, dial)
	a := r.rawConnect(t, protocol.FlagResume|protocol.FlagDatagrams)

	if err := a.write(protocol.Open{Stream: 1, Service: "silent", Window: 1 << 20}); err != nil {
		t.Fatalf("write open: %v", err)
	}
	a.awaitOpened()
	if err := a.write(protocol.Close{Stream: 1}); err != nil {
		t.Fatalf("write close: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	// A close for a stream never opened ends the session, and with it the stream.
	if err := a.write(protocol.Close{Stream: 99}); err != nil {
		t.Fatalf("write close: %v", err)
	}
	select {
	case <-closed:
	case <-time.After(readWait):
		t.Fatal("the target stayed open after its stream failed: nothing closed it")
	}
}
