package host

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/internal/config"
	"github.com/andrewloable/HoleBridge/internal/host/hosttest"
	"github.com/andrewloable/HoleBridge/internal/keys"
	"github.com/andrewloable/HoleBridge/internal/protocol"
	"github.com/andrewloable/HoleBridge/internal/relay"
	"github.com/andrewloable/HoleBridge/pears/hyperdht"
	"github.com/andrewloable/HoleBridge/pears/noise"
)

// awaitSession polls h.Sessions until exactly one session is listed and ok holds for it, and returns it. The
// counts move as bytes move, so a test waits for the counts it expects. It fails the test when that does not
// happen within readWait. The failure message holds the routes and counts only.
func awaitSession(t *testing.T, h *Host, what string, ok func(Session) bool) Session {
	t.Helper()
	deadline := time.Now().Add(readWait)
	for {
		ss := h.Sessions()
		if len(ss) == 1 && ok(ss[0]) {
			return ss[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("Sessions did not report %s within %v; last answer %+v", what, readWait, ss)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A live session of the DHT route is listed with its route, its open streams and flows, and the bytes it carried
// each way. The app is a real DHT client, so the session is live when the test looks. The answer carries no key
// and no target address, and the session is gone once the app closes it.
func TestSessionsReportsLiveDHTSession(t *testing.T) {
	r := newRig(t)
	echo := echoTarget(t)
	dns, _ := udpEcho(t)
	h := r.newWireHost(t, r.hostConfig(t, map[string]config.Service{
		"echo": {Target: echo, Kind: "tcp"},
		"dns":  {Target: dns, Kind: "udp"},
	}, nil), nil)
	r.runWire(t, h)
	c := r.connect(t, r.clientKey)
	awaitSession(t, h, "the new session with no traffic", func(s Session) bool {
		return s.Route == "direct" && s.Streams == 0 && s.Flows == 0 && s.BytesIn == 0 && s.BytesOut == 0
	})

	st, err := c.Open("echo")
	failIfStub(t, err)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	want := make([]byte, 4096)
	if _, err := rand.Read(want); err != nil {
		t.Fatalf("rand: %v", err)
	}
	if got := roundTrip(t, st, want); !bytes.Equal(got, want) {
		t.Fatal("the echo differs from the bytes sent")
	}
	awaitSession(t, h, "one stream and 4096 bytes each way", func(s Session) bool {
		return s.Streams == 1 && s.BytesIn == 4096 && s.BytesOut == 4096
	})

	if err := c.Send(protocol.Flow{Flow: 1, Service: "dns", Payload: []byte("first")}); err != nil {
		t.Fatalf("Send flow: %v", err)
	}
	expectDatagram(t, c, 1, "first")
	awaitSession(t, h, "one flow, one stream and 4101 bytes each way", func(s Session) bool {
		return s.Flows == 1 && s.Streams == 1 && s.BytesIn == 4101 && s.BytesOut == 4101
	})

	raw, err := json.Marshal(h.Sessions())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, secret := range []string{r.key, keys.Format(r.key), hex.EncodeToString(r.appKey[:]), echo, dns} {
		if strings.Contains(string(raw), secret) {
			t.Error("Sessions carries a key or a target address")
		}
	}

	c.Close()
	deadline := time.Now().Add(readWait)
	for len(h.Sessions()) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("Sessions still lists the session %v after the app closed it", readWait)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A session on the LAN route is listed with the route lan. The LAN listener hands its stream to ServeConn, and the
// session counts its streams and bytes as a DHT session does.
func TestSessionsLabelsLANSession(t *testing.T) {
	r := newRig(t)
	h := r.newWireHost(t, r.hostConfig(t, echoServices(t), nil), nil)
	hostSide, appSide := r.lanPair(t, kpOf(r.clientKey))
	serveConn(t, h, hostSide)

	c, err := hosttest.Attach(appSide)
	failIfStub(t, err)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	st, err := c.Open("echo")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	want := make([]byte, 1000)
	if _, err := rand.Read(want); err != nil {
		t.Fatalf("rand: %v", err)
	}
	if got := roundTrip(t, st, want); !bytes.Equal(got, want) {
		t.Fatal("the echo differs from the bytes sent")
	}
	awaitSession(t, h, "a lan session with one stream and 1000 bytes each way", func(s Session) bool {
		return s.Route == "lan" && s.Streams == 1 && s.BytesIn == 1000 && s.BytesOut == 1000
	})
}

// A stream that resumes after a reconnect counts on the session that owns it now. The app drops its transport and
// reconnects, and the stream is reattached to the new session: the new session lists the stream, and the bytes that
// move on it after the reconnect count on it. Once the app closes the stream and the session, the host holds none.
func TestSessionsFollowsResumedStream(t *testing.T) {
	r := newRig(t)
	h := r.newWireHost(t, r.hostConfig(t, echoServices(t), nil), nil)
	r.runWire(t, h)
	c := r.connect(t, r.clientKey)
	st, err := c.Open("echo")
	failIfStub(t, err)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if got := roundTrip(t, st, []byte("before the drop")); !bytes.Equal(got, []byte("before the drop")) {
		t.Fatalf("echo before the drop = %q", got)
	}
	awaitSession(t, h, "one stream and 15 bytes each way", func(s Session) bool {
		return s.Streams == 1 && s.BytesIn == 15 && s.BytesOut == 15
	})
	if err := c.Drop(); err != nil {
		t.Fatalf("Drop: %v", err)
	}
	if err := c.Reconnect(); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
	after := []byte("after the reconnect")
	if got := roundTrip(t, st, after); !bytes.Equal(got, after) {
		t.Fatalf("echo after the reconnect = %q", got)
	}
	// One session is listed (the dropped one is gone), it owns the resumed stream, and the bytes that moved
	// after the reconnect count on it. A lower bound, so a design that keeps the stream's earlier bytes passes too.
	awaitSession(t, h, "the resumed stream on the new session", func(s Session) bool {
		return s.Streams == 1 && s.BytesIn >= uint64(len(after)) && s.BytesOut >= uint64(len(after))
	})

	// The app closes the stream and then the session: nothing is listed, and the host holds no stream any more.
	st.Close()
	c.Close()
	deadline := time.Now().Add(readWait)
	for {
		h.mu.Lock()
		held := len(h.streamMeters)
		h.mu.Unlock()
		listed := len(h.Sessions())
		if listed == 0 && held == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the app closed the stream and the session: %d sessions listed, %d streams held", listed, held)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A session whose stream came through a relay is listed with the route relay (HoleBridge-awf.8). The app's handshake
// offers a relay, and the host runs the relay pairing as the responder. Both DHT nodes take the relay path only
// (hyperdht.ForceRelayForTest), so the bytes can only cross the relay. The relay is a holebridge relay on its own node,
// and the host and the app use the relay's member key pair as their DHT default key pair, the one the relay admits.
func TestSessionsLabelsRelayedSession(t *testing.T) {
	r := newRig(t)
	relayKey := keys.Generate()
	d, err := keys.DeriveRelay(relayKey, r.appKey)
	if err != nil {
		t.Fatalf("DeriveRelay: %v", err)
	}
	member := kpOf(d.Member)
	relayPub := [32]byte(d.Server.Public().(ed25519.PublicKey))

	relayDHT := newTestDHT(t, r.tn, nil)
	relayCtx, cancelRelay := context.WithCancel(context.Background())
	t.Cleanup(cancelRelay)
	if _, err := relay.Run(relayCtx, relayKey, r.appKey, relayDHT, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("relay.Run: %v", err)
	}
	hostDHT := newTestDHT(t, r.tn, &member)
	appDHT := newTestDHT(t, r.tn, &member)
	hyperdht.ForceRelayForTest(t, hostDHT)
	hyperdht.ForceRelayForTest(t, appDHT)

	h := r.newWireHost(t, r.hostConfig(t, echoServices(t), nil), func(o *Options) { o.DHT = hostDHT })
	r.runWire(t, h)
	c := r.dialThroughRelay(t, appDHT, relayPub)

	st, err := c.Open("echo")
	failIfStub(t, err)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	want := make([]byte, 1000)
	if _, err := rand.Read(want); err != nil {
		t.Fatalf("rand: %v", err)
	}
	if got := roundTrip(t, st, want); !bytes.Equal(got, want) {
		t.Fatal("the echo differs from the bytes sent")
	}
	awaitSession(t, h, "a relayed session with one stream and 1000 bytes each way", func(s Session) bool {
		return s.Route == "relay" && s.Streams == 1 && s.BytesIn == 1000 && s.BytesOut == 1000
	})
}

// newTestDHT starts a DHT node on tn whose default key pair is kp, or a random one when kp is nil, and waits until it
// is ready. The node is closed when the test ends.
func newTestDHT(t *testing.T, tn *hyperdht.Testnet, kp *noise.KeyPair) *hyperdht.DHT {
	t.Helper()
	d, err := hyperdht.New(hyperdht.Config{Bootstrap: tn.Bootstrap, DefaultKeyPair: kp})
	if err != nil {
		t.Fatalf("hyperdht.New: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), readWait)
	defer cancel()
	if err := d.Ready(ctx); err != nil {
		t.Fatalf("DHT node not ready: %v", err)
	}
	return d
}

// dialThroughRelay connects the app to the host over d, offering the relay with public key relayPub in its handshake, and
// returns the app-role client once the host's handshake has arrived. The host starts announcing as it runs, so a dial
// that finds nothing yet is retried for readWait, as connect does.
func (r *rig) dialThroughRelay(t *testing.T, d *hyperdht.DHT, relayPub [32]byte) *hosttest.Client {
	t.Helper()
	kp := kpOf(r.clientKey)
	policy := func(bool) *[32]byte { return &relayPub }
	deadline := time.Now().Add(readWait)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), readWait)
		conn, err := d.Connect(ctx, r.hostPub, hyperdht.ConnectOptions{KeyPair: &kp, RelayThrough: policy})
		cancel()
		if err == nil {
			c, err := hosttest.Attach(conn)
			failIfStub(t, err)
			if err != nil {
				t.Fatalf("Attach: %v", err)
			}
			t.Cleanup(func() { c.Close() })
			return c
		}
		failIfStub(t, err)
		if time.Now().After(deadline) {
			t.Fatalf("Connect through the relay: %v", err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}
