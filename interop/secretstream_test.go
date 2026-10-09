// Interop tests for pears/secretstream: the Go secret stream and @hyperswarm/secret-stream 6.9.2 run as JS peers,
// in both roles, over TCP. The JS side is interop/js/secretstream-peer.js, which each test starts as a child
// process and drives over stdio. The tests skip when node or interop/js/node_modules is missing (run npm ci in
// interop/js). The Go side speaks the IK pattern only, as pears/noise does, so these tests cover IK.
package interop

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/rand/v2"
	"net"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/pears/noise"
	"github.com/andrewloable/HoleBridge/pears/secretstream"
)

// The fixed Ed25519 seeds of the two peers. js/secretstream-peer.js uses the same seeds. They are test values.
const (
	ssResponderSeed = "3a5c9834e3a11d3d9f1a8dd610d09b984f19cf3437369e23c0df27fad39300ef"
	ssInitiatorSeed = "bbfd093816e33c8e726e19138035ae5b1d5d52874a6c25ce13c7e1b344cf4c93"
)

const (
	ssMessages   = 1000                            // messages per exchange
	ssMaxMessage = 64 << 10                        // the largest random message
	ssMaxPlain   = 1<<24 - 1 - secretstream.ABytes // the most one message carries: stream.go's maxPlain

	ssStartTimeout = 20 * time.Second // node start-up, the JS listen or dial
	ssHandshake    = 20 * time.Second // the Go side's handshake
	ssTransfer     = 2 * time.Minute  // the deadline on the TCP connection while messages move
	ssReplyTimeout = 2 * time.Minute  // one reply from the JS peer
	ssStopTimeout  = 5 * time.Second  // after stdin closes, before the child is killed
)

// ssLine is one line the JS peer writes. A line sets only the fields it carries.
type ssLine struct {
	OK              *bool  `json:"ok"`
	Error           string `json:"error"`
	Ready           bool   `json:"ready"`
	Connected       bool   `json:"connected"`
	Ended           bool   `json:"ended"`
	Port            int    `json:"port"`
	PublicKey       string `json:"publicKey"`
	Handshake       string `json:"handshake"`
	RemotePublicKey string `json:"remotePublicKey"`
	Sent            string `json:"sent"`
	Echo            string `json:"echo"`
	Received        string `json:"received"`
	Bytes           int    `json:"bytes"`
	EchoBytes       int    `json:"echoBytes"`
}

// ssPeer is a secretstream-peer.js child process. Its stdout lines arrive on lines, and stderr is kept for
// failure messages.
type ssPeer struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  chan string
	quit   chan struct{} // closed when the peer is stopping, so the reader stops handing out lines
	done   chan struct{} // closed when the reader has read stdout to its end
	stderr *syncBuffer
	waited bool // Wait has returned, so stop has nothing left to do
}

// startSSPeer starts secretstream-peer.js with args. The child is stopped when the test ends, whether it passed
// or failed: closing its stdin makes it exit, and it is killed if it does not exit in time.
func startSSPeer(t *testing.T, args ...string) *ssPeer {
	t.Helper()
	node := requireNodeJS(t)
	requireJSModule(t, "@hyperswarm/secret-stream")
	dir, err := filepath.Abs("js")
	if err != nil {
		t.Fatalf("interop/js: %v", err)
	}
	cmd := exec.Command(node, append([]string{"secretstream-peer.js"}, args...)...)
	cmd.Dir = dir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("JS peer stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("JS peer stdout: %v", err)
	}
	p := &ssPeer{
		t:      t,
		cmd:    cmd,
		stdin:  stdin,
		lines:  make(chan string),
		quit:   make(chan struct{}),
		done:   make(chan struct{}),
		stderr: &syncBuffer{},
	}
	cmd.Stderr = p.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start JS peer: %v", err)
	}
	t.Cleanup(p.stop)
	go p.readLines(stdout)
	return p
}

// readLines reads the peer's stdout one line at a time. A line is handed to a waiting call, or dropped once the
// peer is stopping. It runs until stdout ends, so the child never blocks on a full pipe.
func (p *ssPeer) readLines(r io.Reader) {
	defer close(p.done)
	defer close(p.lines)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		select {
		case p.lines <- sc.Text():
		case <-p.quit:
		}
	}
}

// next reads the peer's next line. It fails the test when no line comes in time, when the peer has exited, or
// when the line reports a failure.
func (p *ssPeer) next(timeout time.Duration, what string) ssLine {
	p.t.Helper()
	var l ssLine
	select {
	case text, ok := <-p.lines:
		if !ok {
			p.t.Fatalf("JS peer exited before %s: stderr: %s", what, p.stderr.String())
		}
		if err := json.Unmarshal([]byte(text), &l); err != nil {
			p.t.Fatalf("JS peer sent a line that is not JSON for %s: %q", what, text)
		}
	case <-time.After(timeout):
		p.t.Fatalf("no %s from the JS peer within %v: stderr: %s", what, timeout, p.stderr.String())
	}
	if l.OK != nil && !*l.OK {
		p.t.Fatalf("JS peer failed %s: %s", what, l.Error)
	}
	return l
}

// command writes one command line to the peer's stdin.
func (p *ssPeer) command(cmd any) {
	p.t.Helper()
	b, err := json.Marshal(cmd)
	if err != nil {
		p.t.Fatalf("encode command for the JS peer: %v", err)
	}
	if _, err := p.stdin.Write(append(b, '\n')); err != nil {
		p.t.Fatalf("send command to the JS peer: %v: stderr: %s", err, p.stderr.String())
	}
}

// exit waits for the peer to exit on its own after a clean end, and fails the test unless it exits with status 0.
func (p *ssPeer) exit(timeout time.Duration) {
	p.t.Helper()
	select {
	case <-p.done:
	case <-time.After(timeout):
		p.t.Fatalf("JS peer still running %v after the clean end: stderr: %s", timeout, p.stderr.String())
	}
	p.waited = true
	if err := p.cmd.Wait(); err != nil {
		p.t.Fatalf("JS peer did not exit cleanly: %v: stderr: %s", err, p.stderr.String())
	}
}

// stop closes the peer's stdin, waits for it to exit, and kills it if it does not.
func (p *ssPeer) stop() {
	if p.waited {
		return
	}
	close(p.quit)
	p.stdin.Close()
	select {
	case <-p.done:
	case <-time.After(ssStopTimeout):
		p.cmd.Process.Kill()
		<-p.done
	}
	p.cmd.Wait()
}

// ssKeyPair returns the noise key pair of the Ed25519 seed seedHex, in libsodium's layout: the seed, then the
// public key. crypto/ed25519 lays out its private key the same way.
func ssKeyPair(t *testing.T, seedHex string) noise.KeyPair {
	t.Helper()
	seed, err := hex.DecodeString(seedHex)
	if err != nil || len(seed) != ed25519.SeedSize {
		t.Fatalf("seed %q is not 32 bytes of hex", seedHex)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	var kp noise.KeyPair
	copy(kp.Secret[:], priv)
	copy(kp.Public[:], priv.Public().(ed25519.PublicKey))
	return kp
}

// ssRandomMessages returns count messages of minBytes to maxBytes each, drawn from a fixed seed, so that a failing
// run can be replayed.
func ssRandomMessages(count, minBytes, maxBytes int) [][]byte {
	src := rand.NewChaCha8([32]byte{'h', 'o', 'l', 'e', 'b', 'r', 'i', 'd', 'g', 'e', 's', 's'})
	sizes := rand.New(src)
	msgs := make([][]byte, count)
	for i := range msgs {
		msg := make([]byte, minBytes+sizes.IntN(maxBytes-minBytes+1))
		src.Read(msg)
		msgs[i] = msg
	}
	return msgs
}

// ssCheckHandshake checks that the JS peer and the Go stream finished the same handshake: the same handshake
// hash, the Go side's public key as the JS peer's remote key, and the JS peer's public key as the Go side's.
func ssCheckHandshake(t *testing.T, s *secretstream.Stream, js ssLine, jsPub, goPub [32]byte) {
	t.Helper()
	if !js.Connected {
		t.Fatalf("JS peer's line is not a connected line: %+v", js)
	}
	hash := s.HandshakeHash()
	if js.Handshake != hex.EncodeToString(hash[:]) {
		t.Fatalf("handshake hash: Go %x, JS %s", hash, js.Handshake)
	}
	if js.RemotePublicKey != hex.EncodeToString(goPub[:]) {
		t.Fatalf("JS peer's remote key %s, want the Go public key %x", js.RemotePublicKey, goPub)
	}
	if got := s.RemotePublicKey(); got != jsPub {
		t.Fatalf("Go stream's remote key %x, want the JS public key %x", got, jsPub)
	}
}

// TestSecretStream_GoInitiatorJSResponder: the Go side dials a JS responder and sends 1000 random messages of up
// to 64 KiB. The JS responder echoes every byte back. The Go side checks that the echo is intact, that the JS side
// received what Go sent, that the handshake hashes match, and that both ends close cleanly.
func TestSecretStream_GoInitiatorJSResponder(t *testing.T) {
	ssGoInitiate(t, ssRandomMessages(ssMessages, 1, ssMaxMessage))
}

// TestSecretStream_GoInitiatorJSResponderMaxMessage: one message of the most a single frame carries.
func TestSecretStream_GoInitiatorJSResponderMaxMessage(t *testing.T) {
	ssGoInitiate(t, ssRandomMessages(1, ssMaxPlain, ssMaxPlain))
}

// ssGoInitiate runs the Go side as the initiator against a JS responder, which echoes everything it reads.
func ssGoInitiate(t *testing.T, msgs [][]byte) {
	t.Helper()
	goKP := ssKeyPair(t, ssInitiatorSeed)
	jsPub := ssKeyPair(t, ssResponderSeed).Public

	peer := startSSPeer(t, "responder", "0")
	ready := peer.next(ssStartTimeout, "its ready line")
	if ready.PublicKey != hex.EncodeToString(jsPub[:]) {
		t.Fatalf("JS responder's public key %s, want %x from its seed", ready.PublicKey, jsPub)
	}

	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(ready.Port)), ssStartTimeout)
	if err != nil {
		t.Fatalf("dial the JS responder: %v", err)
	}
	s := secretstream.New(conn, true, secretstream.Options{KeyPair: goKP, RemotePublicKey: &jsPub})
	t.Cleanup(func() { s.Destroy() })
	ctx, cancel := context.WithTimeout(context.Background(), ssHandshake)
	defer cancel()
	if err := s.Handshake(ctx); err != nil {
		t.Fatalf("Go initiator handshake with the JS responder: %v: stderr: %s", err, peer.stderr.String())
	}
	ssCheckHandshake(t, s, peer.next(ssReplyTimeout, "its connected line"), jsPub, goKP.Public)
	conn.SetDeadline(time.Now().Add(ssTransfer))

	sent := sha256.New()
	total := 0
	for _, m := range msgs {
		sent.Write(m)
		total += len(m)
	}
	// Send on a goroutine while this one reads the echo, so that neither side waits on a full pipe. The write
	// side ends after the last message, so the JS side reads io.EOF after its last byte.
	writeErr := make(chan error, 1)
	go func() {
		for _, m := range msgs {
			if _, err := s.Write(m); err != nil {
				writeErr <- err
				return
			}
		}
		writeErr <- s.Close()
	}()
	echo := sha256.New()
	n, err := io.Copy(echo, s) // io.Copy returns nil only at a clean io.EOF from the JS side
	if err != nil {
		t.Fatalf("read the echo: %v: stderr: %s", err, peer.stderr.String())
	}
	if err := <-writeErr; err != nil {
		t.Fatalf("send to the JS responder: %v", err)
	}
	if n != int64(total) {
		t.Fatalf("the echo has %d bytes, want the %d Go sent", n, total)
	}
	if !bytes.Equal(echo.Sum(nil), sent.Sum(nil)) {
		t.Fatalf("the echo differs from what Go sent")
	}

	ended := peer.next(ssReplyTimeout, "its ended line")
	if !ended.Ended || ended.Bytes != total {
		t.Fatalf("JS responder ended with %d bytes, want %d: %+v", ended.Bytes, total, ended)
	}
	if want := hex.EncodeToString(sent.Sum(nil)); ended.Received != want {
		t.Fatalf("the JS responder received different bytes from those Go sent: got %s, want %s", ended.Received, want)
	}
	peer.exit(ssStopTimeout)
}

// TestSecretStream_JSInitiatorGoResponder: a JS initiator sends 1000 random messages of up to 64 KiB to the Go
// responder, which echoes them back. The JS side checks the echo and reports the hash of what it sent.
func TestSecretStream_JSInitiatorGoResponder(t *testing.T) {
	ssJSInitiate(t, ssMessages, 1, ssMaxMessage)
}

// TestSecretStream_JSInitiatorGoResponderMaxMessage: one message of the most a single frame carries.
func TestSecretStream_JSInitiatorGoResponderMaxMessage(t *testing.T) {
	ssJSInitiate(t, 1, ssMaxPlain, ssMaxPlain)
}

// ssJSInitiate runs the Go side as the responder. The JS initiator sends count messages of minBytes to maxBytes
// each, and the Go side echoes every byte it reads until the JS side ends its side.
func ssJSInitiate(t *testing.T, count, minBytes, maxBytes int) {
	t.Helper()
	goKP := ssKeyPair(t, ssResponderSeed)
	jsPub := ssKeyPair(t, ssInitiatorSeed).Public

	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	port := ln.Addr().(*net.TCPAddr).Port

	peer := startSSPeer(t, "initiator", strconv.Itoa(port), hex.EncodeToString(goKP.Public[:]))
	ln.SetDeadline(time.Now().Add(ssStartTimeout))
	conn, err := ln.Accept()
	if err != nil {
		t.Fatalf("accept the JS initiator: %v: stderr: %s", err, peer.stderr.String())
	}
	s := secretstream.New(conn, false, secretstream.Options{KeyPair: goKP})
	t.Cleanup(func() { s.Destroy() })
	ctx, cancel := context.WithTimeout(context.Background(), ssHandshake)
	defer cancel()
	if err := s.Handshake(ctx); err != nil {
		t.Fatalf("Go responder handshake with the JS initiator: %v: stderr: %s", err, peer.stderr.String())
	}
	ssCheckHandshake(t, s, peer.next(ssReplyTimeout, "its connected line"), jsPub, goKP.Public)
	conn.SetDeadline(time.Now().Add(ssTransfer))

	// The echo runs on a goroutine. It reads until the JS side ends, then ends its own side, so the JS side reads
	// io.EOF after its last echoed byte.
	type echoResult struct {
		sum []byte
		n   int64
		err error
	}
	result := make(chan echoResult, 1)
	go func() {
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(h, s), s)
		closeErr := s.Close()
		if err == nil {
			err = closeErr
		}
		result <- echoResult{sum: h.Sum(nil), n: n, err: err}
	}()

	peer.command(map[string]any{"cmd": "send", "count": count, "minBytes": minBytes, "maxBytes": maxBytes})
	sent := peer.next(ssReplyTimeout, "its reply to send")
	if sent.Bytes < count*minBytes || sent.Bytes > count*maxBytes {
		t.Fatalf("JS initiator sent %d bytes, outside %d to %d messages of %d to %d bytes", sent.Bytes, count, count, minBytes, maxBytes)
	}
	if sent.Echo != sent.Sent || sent.EchoBytes != sent.Bytes {
		t.Fatalf("the JS initiator's echo differs from what it sent: %d echoed bytes, want %d", sent.EchoBytes, sent.Bytes)
	}

	peer.command(map[string]string{"cmd": "close"})
	if closed := peer.next(ssReplyTimeout, "its reply to close"); !closed.Ended {
		t.Fatalf("JS initiator's reply to close is not an ended reply: %+v", closed)
	}

	var got echoResult
	select {
	case got = <-result:
	case <-time.After(ssReplyTimeout):
		t.Fatalf("the Go echo did not end after the JS initiator closed")
	}
	if got.err != nil {
		t.Fatalf("read the JS initiator's messages: %v", got.err)
	}
	if got.n != int64(sent.Bytes) {
		t.Fatalf("Go received %d bytes, want the %d the JS initiator sent", got.n, sent.Bytes)
	}
	if want := hex.EncodeToString(got.sum); sent.Sent != want {
		t.Fatalf("Go received different bytes from those the JS initiator sent: got %s, want %s", want, sent.Sent)
	}
	peer.exit(ssStopTimeout)
}
