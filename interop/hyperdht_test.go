// Interop tests for pears/hyperdht: Go and JS HyperDHT 6.34.1 nodes on one network, in every direction, and a
// connection through a pears-go blind relay. The JS side is interop/js/hyperdht-peer.js, which each test starts as a
// child process and drives over stdio. Each test runs on two networks in turn: a Go testnet (hyperdht.NewTestnet) and
// a JS testnet (the peer script's testnet role). The tests skip when node or interop/js/node_modules/hyperdht is
// missing, and fail instead when CI is set (skipInterop).
package interop

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/blake2b"

	"github.com/andrewloable/HoleBridge/pears/blindrelay"
	"github.com/andrewloable/HoleBridge/pears/hyperdht"
	"github.com/andrewloable/HoleBridge/pears/noise"
	"github.com/andrewloable/HoleBridge/pears/protomux"
)

const (
	hdPayloadBytes    = 10 << 20         // each direction of a connection carries this many bytes
	hdHeaderBytes     = 40               // 8-byte big-endian length, then the 32-byte SHA-256 of the payload
	hdMaxBytes        = 64 << 20         // a declared length above this fails the transfer
	hdStartTimeout    = 30 * time.Second // a node bootstraps, or a server announces, within this
	hdLookupTimeout   = 60 * time.Second
	hdConnectTimeout  = 90 * time.Second
	hdTransferTimeout = 120 * time.Second
	hdCloseWait       = 10 * time.Second // after our write side ends, how long the peer's end may take
)

// hdKeyPair returns the seed and the key pair of the node named label. The seed is hashed from the label, so the Go
// node and the JS peers (--seed) agree on the key, and no key is stored in the repo.
func hdKeyPair(label string) ([32]byte, noise.KeyPair) {
	seed := sha256.Sum256([]byte("holebridge interop hyperdht " + label))
	priv := ed25519.NewKeyFromSeed(seed[:])
	var kp noise.KeyPair
	copy(kp.Public[:], priv.Public().(ed25519.PublicKey))
	copy(kp.Secret[:], priv)
	return seed, kp
}

// hdReady is the first line of a JS peer: its role, then its public key (server and client) or the bootstrap nodes
// of its testnet.
type hdReady struct {
	Ready     bool     `json:"ready"`
	Role      string   `json:"role"`
	PublicKey string   `json:"publicKey"`
	Bootstrap []string `json:"bootstrap"`
}

// hdResult is one side of a transfer as the JS peer reports it.
type hdResult struct {
	RemotePublicKey string `json:"remotePublicKey"`
	Sent            int    `json:"sent"`
	SentSha256      string `json:"sentSha256"`
	Received        int    `json:"received"`
	ReceivedSha256  string `json:"receivedSha256"`
	Verified        bool   `json:"verified"`
}

// hdEvent is a JS server's line for one accepted connection.
type hdEvent struct {
	Event           string    `json:"event"`
	RemotePublicKey string    `json:"remotePublicKey"`
	OK              bool      `json:"ok"`
	Error           string    `json:"error"`
	Result          *hdResult `json:"result"`
}

// hdReply is a JS client's reply to one command.
type hdReply struct {
	OK     bool      `json:"ok"`
	Error  string    `json:"error"`
	Found  bool      `json:"found"`
	Result *hdResult `json:"result"`
}

// hdOutcome is the Go side of one transfer: the digests of the bytes it sent and received, and whether the
// connection was claimed through a relay pairing.
type hdOutcome struct {
	sent, received [32]byte
	relayed        bool
	err            error
}

// hdSide is one end of a transfer, as either implementation reports it.
type hdSide struct {
	sent, received       [32]byte
	sentLen, receivedLen int
	verified             bool
}

// startHDPeer starts interop/js/hyperdht-peer.js with args and reads its ready line.
func startHDPeer(t *testing.T, args ...string) (*jsNode, hdReady) {
	t.Helper()
	requireJSModule(t, "hyperdht")
	n := startJSProcess(t, "hyperdht-peer.js", args...)
	var ready hdReady
	n.receive(&ready, hdStartTimeout, "its ready line")
	if !ready.Ready {
		t.Fatalf("JS peer sent a first line that is not ready: %+v", ready)
	}
	return n, ready
}

// hdCall sends one command to a JS peer and reads its reply into reply.
func hdCall(peer *jsNode, cmd any, timeout time.Duration, reply any) {
	peer.send(cmd)
	peer.receive(reply, timeout, "a reply")
}

// newHDGoNode starts a Go HyperDHT node that joins the network of bootstrap, and waits until it is bootstrapped.
func newHDGoNode(t *testing.T, bootstrap []string) *hyperdht.DHT {
	t.Helper()
	return newHDGoNodeWith(t, hyperdht.Config{Bootstrap: bootstrap})
}

// newHDGoNodeWith starts a Go HyperDHT node with cfg, and waits until it is bootstrapped.
func newHDGoNodeWith(t *testing.T, cfg hyperdht.Config) *hyperdht.DHT {
	t.Helper()
	d, err := hyperdht.New(cfg)
	if err != nil {
		t.Fatalf("start Go HyperDHT node: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), hdStartTimeout)
	defer cancel()
	if err := d.Ready(ctx); err != nil {
		t.Fatalf("Go HyperDHT node did not bootstrap on %v: %v", cfg.Bootstrap, err)
	}
	return d
}

// hdListen creates a server on d and listens under kp, which announces it on the network.
func hdListen(t *testing.T, d *hyperdht.DHT, kp noise.KeyPair, opts hyperdht.ServerOptions) *hyperdht.Server {
	t.Helper()
	srv := d.CreateServer(opts)
	ctx, cancel := context.WithTimeout(context.Background(), hdStartTimeout)
	defer cancel()
	if err := srv.Listen(ctx, kp); err != nil {
		t.Fatalf("Go server did not announce %x: %v", kp.Public, err)
	}
	return srv
}

// goFinds reports whether a Go node's lookup of the target hash(pk) finds an announce of pk.
func goFinds(t *testing.T, d *hyperdht.DHT, pk [32]byte) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), hdLookupTimeout)
	defer cancel()
	for r := range d.Lookup(ctx, blake2b.Sum256(pk[:])) {
		for _, p := range r.Peers {
			if bytes.Equal(p.PublicKey, pk[:]) {
				return true
			}
		}
	}
	return false
}

// jsFinds reports whether a JS peer's lookup of the key pk finds an announce of pk.
func jsFinds(t *testing.T, peer *jsNode, pk [32]byte) bool {
	t.Helper()
	var r hdReply
	hdCall(peer, map[string]string{"cmd": "lookup", "key": hex.EncodeToString(pk[:])}, hdLookupTimeout, &r)
	if !r.OK {
		t.Fatalf("JS lookup of %x: %s", pk, r.Error)
	}
	return r.Found
}

// hdExchange sends size random bytes on conn and receives the peer's bytes at the same time. Each side's bytes start
// with an 8-byte big-endian length and the SHA-256 of the payload. The receiver hashes what arrives and checks it
// against that digest, and it also checks that the peer declared the size it expects. It returns the digests of the
// bytes it sent and received.
func hdExchange(conn io.ReadWriter, size int) (sent, received [32]byte, err error) {
	payload := make([]byte, size)
	if _, err := rand.Read(payload); err != nil {
		return sent, received, fmt.Errorf("random payload: %w", err)
	}
	sent = sha256.Sum256(payload)
	header := make([]byte, hdHeaderBytes)
	binary.BigEndian.PutUint64(header, uint64(size))
	copy(header[8:], sent[:])

	written := make(chan error, 1)
	go func() {
		if _, err := conn.Write(header); err != nil {
			written <- err
			return
		}
		_, err := conn.Write(payload)
		written <- err
	}()

	head := make([]byte, hdHeaderBytes)
	if _, err := io.ReadFull(conn, head); err != nil {
		return sent, received, fmt.Errorf("read the peer's header: %w", err)
	}
	declared := binary.BigEndian.Uint64(head)
	if declared != uint64(size) {
		return sent, received, fmt.Errorf("peer declared %d bytes, want %d", declared, size)
	}
	h := sha256.New()
	if n, err := io.CopyN(h, conn, int64(declared)); err != nil {
		return sent, received, fmt.Errorf("read the peer's payload after %d of %d bytes: %w", n, declared, err)
	}
	copy(received[:], h.Sum(nil))
	if !bytes.Equal(received[:], head[8:]) {
		return sent, received, fmt.Errorf("the peer's payload does not match the digest in its header")
	}
	if err := <-written; err != nil {
		return sent, received, fmt.Errorf("write our payload: %w", err)
	}
	return sent, received, nil
}

// hdClose ends the write side of conn, then reads until the peer ends its own, so every byte either side wrote
// arrives before the connection is destroyed. It gives up after hdCloseWait and destroys the connection anyway.
func hdClose(conn *hyperdht.Conn) {
	conn.Close()
	drained := make(chan struct{})
	go func() {
		io.Copy(io.Discard, conn)
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(hdCloseWait):
	}
	conn.Destroy()
}

// hdServeOne accepts one connection on srv and runs hdExchange on it in the background. The outcome comes on the
// returned channel; the connection is closed after it.
func hdServeOne(srv *hyperdht.Server, size int) <-chan hdOutcome {
	out := make(chan hdOutcome, 1)
	go func() {
		ac, err := srv.Accept()
		if err != nil {
			out <- hdOutcome{err: fmt.Errorf("accept: %w", err)}
			return
		}
		sent, received, err := hdExchange(ac, size)
		out <- hdOutcome{sent: sent, received: received, relayed: ac.Relayed(), err: err}
		hdClose(ac.Conn)
	}()
	return out
}

// hdWait returns the outcome that ch delivers, failing the test on an error or when none comes within timeout.
func hdWait(t *testing.T, ch <-chan hdOutcome, timeout time.Duration, what string) hdOutcome {
	t.Helper()
	select {
	case o := <-ch:
		if o.err != nil {
			t.Fatalf("%s: %v", what, o.err)
		}
		return o
	case <-time.After(timeout):
		t.Fatalf("%s: no transfer within %v", what, timeout)
	}
	return hdOutcome{}
}

// hdDial connects from d to the server pk with opts, and runs hdExchange on the connection.
func hdDial(t *testing.T, d *hyperdht.DHT, pk [32]byte, opts hyperdht.ConnectOptions, size int) hdOutcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), hdConnectTimeout)
	defer cancel()
	conn, err := d.Connect(ctx, pk, opts)
	if err != nil {
		t.Fatalf("Go connect to %x: %v", pk, err)
	}
	t.Cleanup(func() { conn.Destroy() })
	out := make(chan hdOutcome, 1)
	go func() {
		sent, received, err := hdExchange(conn, size)
		out <- hdOutcome{sent: sent, received: received, err: err}
		hdClose(conn)
	}()
	return hdWait(t, out, hdTransferTimeout, "Go client")
}

// goSide is the Go end of a transfer, in the form hdCheckPair compares.
func goSide(o hdOutcome, size int) hdSide {
	return hdSide{sent: o.sent, received: o.received, sentLen: size, receivedLen: size, verified: true}
}

// jsSide is a JS end of a transfer, decoded from what the JS peer reported.
func jsSide(t *testing.T, r *hdResult) hdSide {
	t.Helper()
	if r == nil {
		t.Fatalf("the JS peer reported no transfer result")
	}
	return hdSide{
		sent:        hdDigest(t, r.SentSha256),
		received:    hdDigest(t, r.ReceivedSha256),
		sentLen:     r.Sent,
		receivedLen: r.Received,
		verified:    r.Verified,
	}
}

func hdDigest(t *testing.T, s string) [32]byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		t.Fatalf("digest %q is not 32 bytes of hex", s)
	}
	var d [32]byte
	copy(d[:], b)
	return d
}

// hdCheckPair checks the two ends of one connection against each other: each end verified what it received, each
// end sent and received size bytes, and what one end sent is what the other received, in both directions.
func hdCheckPair(t *testing.T, what string, a, b hdSide, size int) {
	t.Helper()
	if !a.verified || !b.verified {
		t.Errorf("%s: an end did not verify the bytes it received (first %v, second %v)", what, a.verified, b.verified)
	}
	if a.sentLen != size || a.receivedLen != size || b.sentLen != size || b.receivedLen != size {
		t.Errorf("%s: lengths sent %d and %d, received %d and %d; want %d each way",
			what, a.sentLen, b.sentLen, a.receivedLen, b.receivedLen, size)
	}
	if a.sent != b.received {
		t.Errorf("%s: the first end sent %x, but the second received %x", what, a.sent, b.received)
	}
	if b.sent != a.received {
		t.Errorf("%s: the second end sent %x, but the first received %x", what, b.sent, a.received)
	}
}

// startHDRelay runs a pears-go blind relay on a Go node that joins bootstrap. The relay listens under its own key pair,
// which the peers dial, and pairs the connections it accepts, as internal/relay does. It returns the relay's public key
// and its server, whose Pairings counts the connections it paired.
func startHDRelay(t *testing.T, bootstrap []string) ([32]byte, *blindrelay.Server) {
	t.Helper()
	d := newHDGoNode(t, bootstrap)
	_, kp := hdKeyPair("relay")
	srv := hdListen(t, d, kp, hyperdht.ServerOptions{})
	bs := blindrelay.NewServer(blindrelay.ServerOptions{Socket: d.Socket()})
	go func() {
		for {
			ac, err := srv.Accept()
			if err != nil {
				return
			}
			pk := ac.RemotePublicKey()
			bs.Accept(protomux.New(ac), pk[:])
		}
	}()
	return kp.Public, bs
}

// TestHyperDHT_GoTestnet runs every case on a Go testnet of three nodes.
func TestHyperDHT_GoTestnet(t *testing.T) {
	tn := hyperdht.NewTestnet(t, 3)
	runHyperDHT(t, tn.Bootstrap)
}

// TestHyperDHT_JSTestnet runs every case on a JS testnet: the peer script's testnet role starts three hyperdht nodes,
// and the Go nodes join them.
func TestHyperDHT_JSTestnet(t *testing.T) {
	_, testnet := startHDPeer(t, "testnet", "3")
	runHyperDHT(t, testnet.Bootstrap)
}

// hdJSConnect has the JS client connect to the Go server with the public key server, and waits for the Go server's
// side of the same connection. When the JS connect fails, the failure also says what the Go side saw, which is where a
// stalled claim shows up.
func hdJSConnect(t *testing.T, client *jsNode, server [32]byte, size int, served <-chan hdOutcome) (hdReply, hdOutcome) {
	t.Helper()
	var reply hdReply
	hdCall(client, map[string]any{"cmd": "connect", "server": hex.EncodeToString(server[:]), "size": size},
		hdConnectTimeout+hdTransferTimeout, &reply)
	if !reply.OK {
		var goSideOut string
		select {
		case o := <-served:
			goSideOut = fmt.Sprintf("the Go server's side: err=%v relayed=%v", o.err, o.relayed)
		case <-time.After(hdStartTimeout):
			goSideOut = "the Go server's side never returned from Accept"
		}
		t.Fatalf("JS connect to %x failed: %s; %s", server, reply.Error, goSideOut)
	}
	return reply, hdWait(t, served, hdTransferTimeout, "Go server")
}

// runHyperDHT runs the cases on the network of bootstrap. Each announces a key and looks it up across the
// implementations, then connects and exchanges hdPayloadBytes in each direction, checked by SHA-256.
func runHyperDHT(t *testing.T, bootstrap []string) {
	t.Helper()
	join := strings.Join(bootstrap, ",")

	t.Run("GoToGo", func(t *testing.T) {
		serverDHT := newHDGoNode(t, bootstrap)
		clientDHT := newHDGoNode(t, bootstrap)
		_, kp := hdKeyPair("go server")
		srv := hdListen(t, serverDHT, kp, hyperdht.ServerOptions{})
		if !goFinds(t, clientDHT, kp.Public) {
			t.Fatalf("Go lookup did not find the Go server's announce of %x", kp.Public)
		}
		served := hdServeOne(srv, hdPayloadBytes)
		dialed := hdDial(t, clientDHT, kp.Public, hyperdht.ConnectOptions{}, hdPayloadBytes)
		serverOut := hdWait(t, served, hdTransferTimeout, "Go server")
		hdCheckPair(t, "Go client and Go server", goSide(dialed, hdPayloadBytes), goSide(serverOut, hdPayloadBytes), hdPayloadBytes)
	})

	t.Run("GoServerJSClient", func(t *testing.T) {
		serverDHT := newHDGoNode(t, bootstrap)
		_, kp := hdKeyPair("go server")
		srv := hdListen(t, serverDHT, kp, hyperdht.ServerOptions{})
		clientSeed, _ := hdKeyPair("js client")
		client, _ := startHDPeer(t, "client", "--bootstrap", join, "--seed", hex.EncodeToString(clientSeed[:]))
		if !jsFinds(t, client, kp.Public) {
			t.Fatalf("JS lookup did not find the Go server's announce of %x", kp.Public)
		}
		served := hdServeOne(srv, hdPayloadBytes)
		reply, goOut := hdJSConnect(t, client, kp.Public, hdPayloadBytes, served)
		hdCheckPair(t, "JS client and Go server", jsSide(t, reply.Result), goSide(goOut, hdPayloadBytes), hdPayloadBytes)
	})

	t.Run("JSServerGoClient", func(t *testing.T) {
		serverSeed, serverKP := hdKeyPair("js server")
		server, ready := startHDPeer(t, "server", "--bootstrap", join, "--seed", hex.EncodeToString(serverSeed[:]),
			"--size", fmt.Sprint(hdPayloadBytes))
		if ready.PublicKey != hex.EncodeToString(serverKP.Public[:]) {
			t.Fatalf("JS server key %s, want %x: the seed does not give the Go key pair", ready.PublicKey, serverKP.Public)
		}
		clientDHT := newHDGoNode(t, bootstrap)
		if !goFinds(t, clientDHT, serverKP.Public) {
			t.Fatalf("Go lookup did not find the JS server's announce of %x", serverKP.Public)
		}
		dialed := hdDial(t, clientDHT, serverKP.Public, hyperdht.ConnectOptions{}, hdPayloadBytes)
		var event hdEvent
		server.receive(&event, hdTransferTimeout, "the JS server's transfer event")
		if !event.OK || event.Result == nil {
			t.Fatalf("JS server transfer failed: %s", event.Error)
		}
		hdCheckPair(t, "Go client and JS server", goSide(dialed, hdPayloadBytes), jsSide(t, event.Result), hdPayloadBytes)
	})

	t.Run("RelayGoServerJSClient", func(t *testing.T) {
		relayPK, relay := startHDRelay(t, bootstrap)
		// The server's node is ephemeral, so it has no id and answers no LOOKUP for its own key. If it did, the JS
		// client's walk would reach that node, which answers the handshake directly; the JS client would then connect
		// straight to it and drop the relay pairing. Ephemeral, the handshake reaches the server through a testnet node
		// that holds its route, so the connection is relayed.
		serverDHT := newHDGoNodeWith(t, hyperdht.Config{Bootstrap: bootstrap, Ephemeral: true})
		// ForceRelayForTest takes away the direct claim: the server's stream is claimed only by its relay pairing, and
		// Relayed reports it. The relay is the one server option the JS client also uses (--relay).
		hyperdht.ForceRelayForTest(t, serverDHT)
		_, serverKP := hdKeyPair("go relayed server")
		relayTo := func(bool) *[32]byte {
			pk := relayPK
			return &pk
		}
		srv := hdListen(t, serverDHT, serverKP, hyperdht.ServerOptions{RelayThrough: relayTo})
		clientSeed, _ := hdKeyPair("js relayed client")
		client, _ := startHDPeer(t, "client", "--bootstrap", join, "--seed", hex.EncodeToString(clientSeed[:]),
			"--relay", hex.EncodeToString(relayPK[:]))
		served := hdServeOne(srv, hdPayloadBytes)
		reply, goOut := hdJSConnect(t, client, serverKP.Public, hdPayloadBytes, served)
		if !goOut.relayed {
			t.Errorf("the Go server's connection was not claimed through a relay pairing")
		}
		hdCheckPair(t, "JS client and Go server through the relay", jsSide(t, reply.Result), goSide(goOut, hdPayloadBytes), hdPayloadBytes)
		if n := relay.Pairings(); n == 0 {
			t.Errorf("the Go relay paired no connection, so the transfer did not go through it")
		}
	})
}
