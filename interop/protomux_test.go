// Interop tests for pears/protomux: the Go Mux and upstream protomux 3.12.1 run over a secret stream to the JS
// peer interop/js/protomux-peer.js, in both roles, over TCP. The peer is a child process driven over stdio, one
// command at a time, and it is stopped when the test ends. The tests skip when node or interop/js/node_modules is
// missing (run npm ci in interop/js). They reuse the key seeds, the timeouts and ssRandomMessages from
// secretstream_test.go.
package interop

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/pears/protomux"
	"github.com/andrewloable/HoleBridge/pears/secretstream"
)

const (
	pmProtocol  = "holebridge-test"
	pmOpenedID  = "js-opened"           // the channel the JS peer opens; the Go side pairs on this ID
	pmGoHello   = "go-hello"            // the handshake of the channel Go opens
	pmJSAnswer  = "js-answer"           // what protomux-peer.js answers Go's open with (fixed there)
	pmJSHello   = "js-hello"            // the handshake the JS peer opens its channel with
	pmGoAnswer  = "go-answer"           // what the Go side answers the channel JS opened with
	pmUnpaired  = "holebridge-unpaired" // no side pairs on this protocol, so JS refuses the open
	pmMaxMsg    = 4 << 10               // the largest message in a round
	pmBatchSize = 3                     // messages in a batch: indexes 0, 1, 0
	pmInbox     = 4 * ssMessages        // room for the replies one round can leave waiting
)

// pmLine is one line the JS peer writes. A line sets only the fields it carries.
type pmLine struct {
	OK              *bool  `json:"ok"`
	Error           string `json:"error"`
	Ready           bool   `json:"ready"`
	Connected       bool   `json:"connected"`
	Port            int    `json:"port"`
	PublicKey       string `json:"publicKey"`
	Handshake       string `json:"handshake"`
	RemotePublicKey string `json:"remotePublicKey"`
	Event           string `json:"event"`
	IsRemote        bool   `json:"isRemote"`
	Count           int    `json:"count"`
	EchoCount       int    `json:"echoCount"`
	Sent            string `json:"sent"`
	Echo            string `json:"echo"`
}

// pmPeer is a protomux-peer.js child process. Its stdout lines arrive on lines, and stderr is kept for failure
// messages.
type pmPeer struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  chan string
	quit   chan struct{} // closed when the peer is stopping, so the reader stops handing out lines
	done   chan struct{} // closed when the reader has read stdout to its end
	stderr *syncBuffer
	waited bool // Wait has returned, so stop has nothing left to do
}

// startPMPeer starts protomux-peer.js with args. The child is stopped when the test ends, whether it passed or
// failed: closing its stdin makes it exit, and it is killed if it does not exit in time.
func startPMPeer(t *testing.T, args ...string) *pmPeer {
	t.Helper()
	node := requireNodeJS(t)
	requireJSModule(t, "protomux")
	requireJSModule(t, "@hyperswarm/secret-stream")
	dir, err := filepath.Abs("js")
	if err != nil {
		t.Fatalf("interop/js: %v", err)
	}
	cmd := exec.Command(node, append([]string{"protomux-peer.js"}, args...)...)
	cmd.Dir = dir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("JS peer stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("JS peer stdout: %v", err)
	}
	p := &pmPeer{
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
func (p *pmPeer) readLines(r io.Reader) {
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
func (p *pmPeer) next(timeout time.Duration, what string) pmLine {
	p.t.Helper()
	var l pmLine
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

// call sends one command and returns its reply, which must be an ok reply.
func (p *pmPeer) call(cmd map[string]any, what string) pmLine {
	p.t.Helper()
	b, err := json.Marshal(cmd)
	if err != nil {
		p.t.Fatalf("encode command for the JS peer: %v", err)
	}
	if _, err := p.stdin.Write(append(b, '\n')); err != nil {
		p.t.Fatalf("send command to the JS peer: %v: stderr: %s", err, p.stderr.String())
	}
	l := p.next(ssReplyTimeout, what)
	if l.OK == nil {
		p.t.Fatalf("the JS peer's line for %s is not a reply: %+v", what, l)
	}
	return l
}

// exit waits for the peer to exit on its own after a clean end, and fails the test unless it exits with status 0.
func (p *pmPeer) exit(timeout time.Duration) {
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
func (p *pmPeer) stop() {
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

// pmEntry is one message: its index and its payload.
type pmEntry struct {
	index   int
	payload []byte
}

// pmChannel is a channel on the Go side and what it reports: the handshake the remote side sent (open), the
// close (closed, with isRemote), and the messages it receives (inbox). An echo channel sends each message back on
// the same index instead of queueing it. failed carries an error that a callback could not return.
type pmChannel struct {
	mux    *protomux.Mux
	ch     *protomux.Channel
	msgs   [2]*protomux.Message
	open   chan []byte
	closed chan bool
	inbox  chan pmEntry
	failed chan error
}

// newPMChannel creates a channel on mux with message indexes 0 and 1. It returns an error rather than failing the
// test, because the Pair callback that creates a channel runs on the read loop.
func newPMChannel(mux *protomux.Mux, protocol string, id []byte, echo bool) (*pmChannel, error) {
	pc := &pmChannel{
		mux:    mux,
		open:   make(chan []byte, 1),
		closed: make(chan bool, 1),
		inbox:  make(chan pmEntry, pmInbox),
		failed: make(chan error, 1),
	}
	pc.ch = mux.CreateChannel(protomux.ChannelOptions{
		Protocol: protocol,
		ID:       id,
		OnOpen:   func(handshake []byte) { pmSignal(pc.open, append([]byte(nil), handshake...)) },
		OnClose:  func(isRemote bool) { pmSignal(pc.closed, isRemote) },
	})
	if pc.ch == nil {
		return nil, fmt.Errorf("CreateChannel returned nil for protocol %q", protocol)
	}
	for i := range pc.msgs {
		pc.msgs[i] = pc.ch.AddMessage(func(payload []byte) { pc.received(i, payload, echo) })
	}
	return pc, nil
}

// received handles one message. An echo sends it back on the same index; otherwise it is queued on inbox.
func (pc *pmChannel) received(index int, payload []byte, echo bool) {
	if echo {
		if err := pc.msgs[index].Send(payload); err != nil {
			pmSignal(pc.failed, fmt.Errorf("echo on index %d: %w", index, err))
		}
		return
	}
	select {
	case pc.inbox <- pmEntry{index: index, payload: append([]byte(nil), payload...)}:
	default:
		pmSignal(pc.failed, errors.New("inbox full"))
	}
}

// pmSignal sends v on ch unless ch already holds a value, so that a callback never blocks the read loop.
func pmSignal[T any](ch chan T, v T) {
	select {
	case ch <- v:
	default:
	}
}

// pmWait returns the next value from ch. It fails the test if pc reports a failure first, or if no value comes in
// time.
func pmWait[T any](t *testing.T, pc *pmChannel, ch chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case err := <-pc.failed:
		t.Fatalf("%s: %v", what, err)
	case <-time.After(ssReplyTimeout):
		t.Fatalf("no %s within %v", what, ssReplyTimeout)
	}
	var zero T
	return zero
}

// pmWire is the secret stream as the Go Mux sees it. It counts the frames written and read, so that a batch can be
// checked to go out as one frame in each direction.
type pmWire struct {
	*secretstream.Stream
	writes atomic.Int64
	reads  atomic.Int64
}

func (w *pmWire) Write(p []byte) (int, error) {
	w.writes.Add(1)
	return w.Stream.Write(p)
}

// ReadFrame makes pmWire a protomux.FrameReader, as the Stream is, so the Mux reads whole frames.
func (w *pmWire) ReadFrame() ([]byte, error) {
	f, err := w.Stream.ReadFrame()
	if err == nil {
		w.reads.Add(1)
	}
	return f, err
}

// pmDigest hashes a sequence of messages the way protomux-peer.js does: for each one, its index, its length as
// four big-endian bytes, and its payload.
func pmDigest(entries []pmEntry) string {
	h := sha256.New()
	var head [5]byte
	for _, e := range entries {
		head[0] = byte(e.index)
		binary.BigEndian.PutUint32(head[1:], uint32(len(e.payload)))
		h.Write(head[:])
		h.Write(e.payload)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// pmSendAll sends msgs on pc, message i on index i%2, corked as one batch when batch is set. It returns what it
// sent, in order.
func pmSendAll(t *testing.T, pc *pmChannel, msgs [][]byte, batch bool) []pmEntry {
	t.Helper()
	sent := make([]pmEntry, len(msgs))
	if batch {
		pc.mux.Cork()
	}
	for i, m := range msgs {
		sent[i] = pmEntry{index: i % 2, payload: m}
		if err := pc.msgs[i%2].Send(m); err != nil {
			t.Fatalf("send message %d on index %d: %v", i, i%2, err)
		}
	}
	if batch {
		if err := pc.mux.Uncork(); err != nil {
			t.Fatalf("uncork the batch: %v", err)
		}
	}
	return sent
}

// pmEchoRound sends msgs on pc, which the JS peer echoes, and checks that the echoes come back in order with the
// same indexes and payloads.
func pmEchoRound(t *testing.T, pc *pmChannel, msgs [][]byte, batch bool) {
	t.Helper()
	sent := pmSendAll(t, pc, msgs, batch)
	var echoed []pmEntry
	for len(echoed) < len(sent) {
		echoed = append(echoed, pmWait(t, pc, pc.inbox, "an echo from the JS peer"))
	}
	if pmDigest(echoed) != pmDigest(sent) {
		t.Fatalf("the JS peer's %d echoes differ from the %d messages Go sent (batch %v)", len(echoed), len(sent), batch)
	}
}

// pmPeerRound has the JS peer send count messages on the channel Go paired with, and checks the echoes it reports:
// as many as it sent, with the same digest.
func pmPeerRound(t *testing.T, peer *pmPeer, count int, batch bool) {
	t.Helper()
	reply := peer.call(map[string]any{
		"cmd": "send", "protocol": pmProtocol, "id": pmOpenedID,
		"count": count, "minBytes": 1, "maxBytes": pmMaxMsg, "batch": batch,
	}, "the JS peer's reply to send")
	if reply.Count != count || reply.EchoCount != count {
		t.Fatalf("JS peer sent %d messages and got %d echoes back, want %d each (batch %v)", reply.Count, reply.EchoCount, count, batch)
	}
	if reply.Sent != reply.Echo {
		t.Fatalf("the Go echoes differ from the messages the JS peer sent (batch %v)", batch)
	}
}

// pmCheckConnected checks the JS peer's connected line against the Go stream, as ssCheckHandshake does for the
// secret stream tests.
func pmCheckConnected(t *testing.T, s *secretstream.Stream, js pmLine, jsPub, goPub [32]byte) {
	t.Helper()
	ssCheckHandshake(t, s, ssLine{Connected: js.Connected, Handshake: js.Handshake, RemotePublicKey: js.RemotePublicKey}, jsPub, goPub)
}

// pmExercise runs the checks both role tests share, over the connected secret stream s and the JS peer. Go opens
// a channel that JS echoes, and JS opens a channel that Go echoes. It covers the handshakes both ways, pairing (and
// an open that no side pairs on, which JS refuses), 1000 echoed messages and a batch from each side, and a close
// from each side.
func pmExercise(t *testing.T, s *secretstream.Stream, peer *pmPeer) {
	t.Helper()
	wire := &pmWire{Stream: s}
	mux := protomux.New(wire)

	// Go pairs on the ID JS opens its channel with. The callback creates the channel, which takes that open, and
	// answers it with pmGoAnswer.
	paired := make(chan *pmChannel, 1)
	mux.Pair(pmProtocol, []byte(pmOpenedID), func() {
		pc, err := newPMChannel(mux, pmProtocol, []byte(pmOpenedID), true)
		if err == nil {
			err = pc.ch.Open([]byte(pmGoAnswer))
		}
		if err != nil {
			t.Errorf("answer the channel JS opened: %v", err)
			paired <- nil
			return
		}
		paired <- pc
	})

	// Go opens a channel with no ID. JS pairs on the protocol, answers with pmJSAnswer, and echoes.
	c1, err := newPMChannel(mux, pmProtocol, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := c1.ch.Open([]byte(pmGoHello)); err != nil {
		t.Fatalf("open the channel Go opens: %v", err)
	}
	if got := pmWait(t, c1, c1.open, "the handshake JS answers Go's open with"); string(got) != pmJSAnswer {
		t.Fatalf("JS answered Go's open with %q, want %q", got, pmJSAnswer)
	}

	// JS opens the channel with pmOpenedID. Go pairs, and its callback answers.
	peer.call(map[string]any{"cmd": "open", "protocol": pmProtocol, "id": pmOpenedID, "handshake": pmJSHello},
		"the JS peer's reply to open")
	var c2 *pmChannel
	select {
	case c2 = <-paired:
	case <-time.After(ssReplyTimeout):
		t.Fatalf("Go never saw the channel JS opened")
	}
	if c2 == nil {
		t.Fatalf("Go could not pair on the channel JS opened")
	}
	if got := pmWait(t, c2, c2.open, "the handshake JS opened its channel with"); string(got) != pmJSHello {
		t.Fatalf("Go received handshake %q from JS, want %q", got, pmJSHello)
	}
	ans := peer.call(map[string]any{"cmd": "await", "protocol": pmProtocol, "id": pmOpenedID, "event": "open"},
		"the JS peer's open event for its channel")
	if want := hex.EncodeToString([]byte(pmGoAnswer)); ans.Handshake != want {
		t.Fatalf("JS received handshake %s from Go, want %s", ans.Handshake, want)
	}

	// Rounds: 1000 messages from Go, then a batch from Go, 1000 from JS, then a batch from JS. A batch goes out
	// as one frame from its sender, and JS answers a batch of three with one batch, which Go reads as one frame.
	pmEchoRound(t, c1, ssRandomMessages(ssMessages, 1, pmMaxMsg), false)
	w0, r0 := wire.writes.Load(), wire.reads.Load()
	pmEchoRound(t, c1, ssRandomMessages(pmBatchSize, 1, pmMaxMsg), true)
	if w, r := wire.writes.Load()-w0, wire.reads.Load()-r0; w != 1 || r != 1 {
		t.Fatalf("Go's batch of %d went out in %d frames and its echoes came back in %d, want 1 and 1", pmBatchSize, w, r)
	}
	pmPeerRound(t, peer, ssMessages, false)
	w0, r0 = wire.writes.Load(), wire.reads.Load()
	pmPeerRound(t, peer, pmBatchSize, true)
	if w, r := wire.writes.Load()-w0, wire.reads.Load()-r0; w != 1 || r != 1 {
		t.Fatalf("JS's batch of %d: Go wrote %d frames for the echoes and read %d, want 1 and 1", pmBatchSize, w, r)
	}

	// Nothing pairs on pmUnpaired, so JS refuses the open and Go's channel closes with isRemote true.
	cu, err := newPMChannel(mux, pmUnpaired, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := cu.ch.Open([]byte("nobody")); err != nil {
		t.Fatalf("open the unpaired channel: %v", err)
	}
	if isRemote := pmWait(t, cu, cu.closed, "the close of the refused channel"); !isRemote {
		t.Fatalf("the refused channel closed with isRemote false, want true")
	}

	// Go closes its channel: Go sees isRemote false, JS sees isRemote true, and Go cannot send on it any more.
	if err := c1.ch.Close(); err != nil {
		t.Fatalf("close Go's channel: %v", err)
	}
	if isRemote := pmWait(t, c1, c1.closed, "the close of Go's channel"); isRemote {
		t.Fatalf("Go's close reported isRemote true, want false")
	}
	if err := c1.msgs[0].Send([]byte("late")); err == nil {
		t.Fatalf("Send on a channel Go closed succeeded")
	}
	ev := peer.call(map[string]any{"cmd": "await", "protocol": pmProtocol, "id": "", "event": "close"},
		"the JS peer's close event for Go's channel")
	if !ev.IsRemote {
		t.Fatalf("JS saw Go's close with isRemote false, want true")
	}

	// JS closes its channel: JS sees isRemote false, Go sees isRemote true, and Go cannot send on it any more.
	peer.call(map[string]any{"cmd": "close", "protocol": pmProtocol, "id": pmOpenedID}, "the JS peer's reply to close")
	if isRemote := pmWait(t, c2, c2.closed, "the close of the channel JS closed"); !isRemote {
		t.Fatalf("Go saw JS's close with isRemote false, want true")
	}
	if err := c2.msgs[0].Send([]byte("late")); err == nil {
		t.Fatalf("Send on a channel the remote side closed succeeded")
	}
	ev = peer.call(map[string]any{"cmd": "await", "protocol": pmProtocol, "id": pmOpenedID, "event": "close"},
		"the JS peer's close event for its channel")
	if ev.IsRemote {
		t.Fatalf("JS's close reported isRemote true, want false")
	}

	if err := s.Close(); err != nil {
		t.Fatalf("end the secret stream: %v", err)
	}
}

// TestProtomux_GoInitiatorJSResponder: the Go side dials the JS peer, which serves the connection.
func TestProtomux_GoInitiatorJSResponder(t *testing.T) {
	goKP := ssKeyPair(t, ssInitiatorSeed)
	jsPub := ssKeyPair(t, ssResponderSeed).Public

	peer := startPMPeer(t, "responder", "0")
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
	pmCheckConnected(t, s, peer.next(ssReplyTimeout, "its connected line"), jsPub, goKP.Public)
	conn.SetDeadline(time.Now().Add(ssTransfer))

	pmExercise(t, s, peer)
	peer.exit(ssStopTimeout)
}

// TestProtomux_JSInitiatorGoResponder: the JS peer dials the Go side, which serves the connection.
func TestProtomux_JSInitiatorGoResponder(t *testing.T) {
	goKP := ssKeyPair(t, ssResponderSeed)
	jsPub := ssKeyPair(t, ssInitiatorSeed).Public

	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	port := ln.Addr().(*net.TCPAddr).Port

	peer := startPMPeer(t, "initiator", strconv.Itoa(port), hex.EncodeToString(goKP.Public[:]))
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
	pmCheckConnected(t, s, peer.next(ssReplyTimeout, "its connected line"), jsPub, goKP.Public)
	conn.SetDeadline(time.Now().Add(ssTransfer))

	pmExercise(t, s, peer)
	peer.exit(ssStopTimeout)
}
