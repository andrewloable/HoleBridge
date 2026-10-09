package hyperdht

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
)

// TestCloseFreesPortAndGoroutines closes a DHT and checks that its UDP port can be bound again, and that the
// goroutines it started have ended: the count returns to what it was before New.
func TestCloseFreesPortAndGoroutines(t *testing.T) {
	before := runtime.NumGoroutine()
	d, err := New(Config{})
	must(t, err)
	addr, err := d.addr()
	must(t, err)
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	again, err := New(Config{Port: addr.Port})
	if err != nil {
		t.Fatalf("New on the port of the closed DHT: %v", err)
	}
	if err := again.Close(); err != nil {
		t.Fatalf("Close of the second DHT: %v", err)
	}

	waitUntil(t, 10*time.Second, "the goroutines of the closed DHTs ending", func() bool {
		return runtime.NumGoroutine() <= before
	})
}

// TestCloseStopsServersAndRefusesWork closes a DHT that has a listening server. The server stops accepting and
// its key pair is unannounced, Listen and Connect fail, Lookup returns nothing, and a second Close is harmless.
func TestCloseStopsServersAndRefusesWork(t *testing.T) {
	tn := startTestnet(t, 10)
	host := testKeyPair(3)
	d, err := New(Config{Bootstrap: tn.Bootstrap})
	must(t, err)
	t.Cleanup(func() { d.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := d.node.Ready(ctx); err != nil {
		t.Fatalf("DHT not ready: %v", err)
	}
	srv := d.CreateServer(ServerOptions{})
	listenOn(t, srv, host)
	conns := acceptAll(srv)

	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	expectNoAccepts(t, conns)

	if err := srv.Listen(ctx, testKeyPair(4)); err == nil {
		t.Error("Listen on a server of a closed DHT succeeded")
	} else {
		failIfStub(t, err)
	}
	_, err = d.Connect(ctx, host.Public, ConnectOptions{})
	if !errors.Is(err, ErrDHTClosed) {
		t.Errorf("Connect after Close: error %v, want ErrDHTClosed", err)
	}
	if _, ok := <-d.Lookup(ctx, hashKey(host.Public)); ok {
		t.Error("Lookup after Close returned a result")
	}
	if err := d.Close(); err != nil {
		t.Errorf("second Close: %v, want nil", err)
	}

	if p, ok := peerOf(lookupAll(t, tn.Nodes[9], hashKey(host.Public)), host.Public); ok {
		t.Errorf("after Close, Lookup still finds the server's record %v", p)
	}
}
