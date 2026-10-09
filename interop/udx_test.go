// Interop tests for pears/udx: the Go UDX socket and stream against udx-native 1.21.3, run as JS peers
// (interop/js/udx-peer.js), in both directions on 127.0.0.1. They cover data integrity of large transfers
// (SHA-256 at both ends), unordered messages both ways, and the throughput table (Go to Go, JS to JS, Go to JS,
// JS to Go). The JS side is a child process driven over stdio, as in secretstream_test.go. The tests skip when
// node or interop/js/node_modules is missing (run npm ci in interop/js).
package interop

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/pears/udx"
)

const (
	udxBlock       = 1 << 20   // the payload block: one random 1 MiB block, repeated
	udxIntegrity   = 1 << 30   // bytes in each large integrity transfer
	udxBenchBytes  = 100 << 20 // bytes in each throughput run, as in docs/spike-m1.md
	udxBenchRuns   = 3         // throughput runs per row; the table gives the median
	udxMessages    = 256       // unordered messages in each direction
	udxMessageSize = 1000      // bytes in each message, index included

	// The two ends of each stream. Each socket has its own table of ids, so the ids only need to match
	// across a pair.
	udxGoID = 0x1001
	udxJSID = 0x2002

	udxStartTimeout = 20 * time.Second // the JS peer's start-up and its ready line
	udxTransfer     = 5 * time.Minute  // the deadline for one transfer's end, in either direction
	udxReply        = 2 * time.Minute  // one reply from the JS peer
	udxStopTimeout  = 5 * time.Second  // after stdin closes, before the child is killed

	// A 1 GiB transfer takes about 25 s on the development machine. A 100 MiB run takes about 1 s, so 30 s is a
	// stall, not a slow run.
	udxIntegrityDeadline = 3 * time.Minute
	udxBenchDeadline     = 30 * time.Second
)

// udxLine is one line the JS peer writes. A line sets only the fields it carries.
type udxLine struct {
	OK       *bool   `json:"ok"`
	Error    string  `json:"error"`
	Ready    bool    `json:"ready"`
	Port     int     `json:"port"`
	Ended    bool    `json:"ended"`
	Closed   bool    `json:"closed"`
	Bytes    int64   `json:"bytes"`
	Hash     string  `json:"hash"`
	Sent     string  `json:"sent"`
	Received string  `json:"received"`
	Seconds  float64 `json:"seconds"`
	Messages int     `json:"messages"`
	Valid    int     `json:"valid"`
	Bad      int     `json:"bad"`
	Dup      int     `json:"dup"`
}

// udxPeer is a udx-peer.js child process. Its stdout lines arrive on lines, and stderr is kept for failure
// messages.
type udxPeer struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  chan string
	quit   chan struct{} // closed when the peer is stopping, so the reader stops handing out lines
	done   chan struct{} // closed when the reader has read stdout to its end
	stderr *syncBuffer
	waited bool // Wait has returned, so stop has nothing left to do
}

// startUDXPeer starts udx-peer.js with args. The child is stopped when the test ends, whether it passed or failed:
// closing its stdin makes it exit, and it is killed if it does not exit in time.
func startUDXPeer(t *testing.T, args ...string) *udxPeer {
	t.Helper()
	node := requireNodeJS(t)
	requireJSModule(t, "udx-native")
	dir, err := filepath.Abs("js")
	if err != nil {
		t.Fatalf("interop/js: %v", err)
	}
	cmd := exec.Command(node, append([]string{"udx-peer.js"}, args...)...)
	cmd.Dir = dir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("JS peer stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("JS peer stdout: %v", err)
	}
	p := &udxPeer{
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
func (p *udxPeer) readLines(r io.Reader) {
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
func (p *udxPeer) next(timeout time.Duration, what string) udxLine {
	p.t.Helper()
	var l udxLine
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
func (p *udxPeer) command(cmd any) {
	p.t.Helper()
	b, err := json.Marshal(cmd)
	if err != nil {
		p.t.Fatalf("encode command for the JS peer: %v", err)
	}
	if _, err := p.stdin.Write(append(b, '\n')); err != nil {
		p.t.Fatalf("send command to the JS peer: %v: stderr: %s", err, p.stderr.String())
	}
}

// closePeer asks the peer to close and checks that it says so.
func (p *udxPeer) closePeer() {
	p.t.Helper()
	p.command(map[string]string{"cmd": "close"})
	if closed := p.next(udxReply, "its reply to close"); !closed.Closed {
		p.t.Fatalf("JS peer's reply to close is not a closed reply: %+v", closed)
	}
}

// exit waits for the peer to exit on its own after a clean end, and fails the test unless it exits with status 0.
func (p *udxPeer) exit(timeout time.Duration) {
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
func (p *udxPeer) stop() {
	if p.waited {
		return
	}
	close(p.quit)
	p.stdin.Close()
	select {
	case <-p.done:
	case <-time.After(udxStopTimeout):
		p.cmd.Process.Kill()
		<-p.done
	}
	p.cmd.Wait()
}

// udxEndpoint is a Go UDX socket on 127.0.0.1, over its own UDP connection.
type udxEndpoint struct {
	sock *udx.Socket
	addr *net.UDPAddr
}

func newUDXEndpoint(t *testing.T) *udxEndpoint {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen UDP: %v", err)
	}
	sock, err := udx.NewSocket(conn)
	if err != nil {
		conn.Close()
		t.Fatalf("UDX socket: %v", err)
	}
	t.Cleanup(func() { sock.Close() })
	return &udxEndpoint{sock: sock, addr: conn.LocalAddr().(*net.UDPAddr)}
}

// udxAddr is the address of a UDP port on 127.0.0.1.
func udxAddr(port int) *net.UDPAddr {
	return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}
}

// udxRandomBlock returns one random block that every transfer in a test repeats.
func udxRandomBlock(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, udxBlock)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random block: %v", err)
	}
	return b
}

// udxSinkResult is what a receiving stream got: its byte count, its SHA-256 in hex, and the span from its first
// byte to end of stream.
type udxSinkResult struct {
	n    int64
	sum  string
	span time.Duration
	err  error
}

// udxSink reads st to io.EOF and hashes what it reads.
func udxSink(st *udx.Stream) udxSinkResult {
	h := sha256.New()
	buf := make([]byte, udxBlock)
	var n int64
	var first time.Time
	for {
		m, err := st.Read(buf)
		if m > 0 {
			if first.IsZero() {
				first = time.Now()
			}
			h.Write(buf[:m])
			n += int64(m)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return udxSinkResult{n: n, err: err}
		}
	}
	r := udxSinkResult{n: n, sum: hex.EncodeToString(h.Sum(nil))}
	if !first.IsZero() {
		r.span = time.Since(first)
	}
	return r
}

// udxSource writes total bytes, the block repeated, then ends the write side. It returns the SHA-256 of what it
// wrote, in hex. The stream stays up after that: acks still have to arrive, so the caller destroys it later.
func udxSource(st *udx.Stream, block []byte, total int64) (string, error) {
	h := sha256.New()
	for sent := int64(0); sent < total; {
		n := min(int64(len(block)), total-sent)
		part := block[:n]
		h.Write(part)
		if _, err := st.Write(part); err != nil {
			return "", err
		}
		sent += n
	}
	if err := st.CloseWrite(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// udxCheckSink fails the test unless the stream delivered total bytes with the digest want.
func udxCheckSink(t *testing.T, res udxSinkResult, total int64, want string) {
	t.Helper()
	if res.err != nil {
		t.Fatalf("receive: %v", res.err)
	}
	if res.n != total {
		t.Fatalf("received %d bytes, want %d", res.n, total)
	}
	if res.sum != want {
		t.Fatalf("received bytes differ from the sent bytes: got sha256 %s, want %s", res.sum, want)
	}
}

// udxRun runs send and recv at the same time, because each side needs the other to make progress. If both are
// not done within d, it calls unblock, which must make the stalled side return (destroying the stream does), and
// reports false. A stall then fails its test or row at once, instead of hanging the run.
func udxRun[S, R any](d time.Duration, unblock func(), send func() S, recv func() R) (S, R, bool) {
	sc := make(chan S, 1)
	rc := make(chan R, 1)
	go func() { sc <- send() }()
	go func() { rc <- recv() }()
	timer := time.NewTimer(d)
	defer timer.Stop()
	var s S
	var r R
	gotS, gotR := false, false
	for !gotS || !gotR {
		select {
		case s = <-sc:
			gotS = true
		case r = <-rc:
			gotR = true
		case <-timer.C:
			unblock()
			if !gotS {
				s = <-sc
			}
			if !gotR {
				r = <-rc
			}
			return s, r, false
		}
	}
	return s, r, true
}

// udxSinkWithin reads st to io.EOF. If EOF does not come within d, it destroys st so the read returns, and reports
// false.
func udxSinkWithin(st *udx.Stream, d time.Duration) (udxSinkResult, bool) {
	ch := make(chan udxSinkResult, 1)
	go func() { ch <- udxSink(st) }()
	select {
	case r := <-ch:
		return r, true
	case <-time.After(d):
		st.Destroy()
		return <-ch, false
	}
}

// udxMessageBody returns message i in the layout udx-peer.js checks: the index as uint32 LE, then byte j from 4
// on is (index*31 + j) mod 256.
func udxMessageBody(i uint32) []byte {
	b := make([]byte, udxMessageSize)
	binary.LittleEndian.PutUint32(b, i)
	for j := 4; j < len(b); j++ {
		b[j] = byte(i*31 + uint32(j))
	}
	return b
}

// udxMessageIndex returns the index of a message that matches udxMessageBody, and whether it does.
func udxMessageIndex(b []byte) (uint32, bool) {
	if len(b) != udxMessageSize {
		return 0, false
	}
	i := binary.LittleEndian.Uint32(b)
	return i, bytes.Equal(udxMessageBody(i), b)
}

// udxMbit is the throughput of total bytes over span, in decimal megabits per second.
func udxMbit(total int64, span time.Duration) float64 {
	return float64(total) * 8 / span.Seconds() / 1e6
}

// TestUDX_GoToJSEcho: the Go side sends 1 GiB to a JS echo peer. The peer hashes what it received and writes
// every byte back. Go hashes what comes back. Both hashes must equal the hash of what Go sent, and both ends
// must end cleanly.
func TestUDX_GoToJSEcho(t *testing.T) {
	ep := newUDXEndpoint(t)
	st := ep.sock.NewStream(udxGoID)
	t.Cleanup(func() { st.Destroy() })
	peer := startUDXPeer(t, "echo", strconv.Itoa(ep.addr.Port), strconv.Itoa(udxGoID), strconv.Itoa(udxJSID))
	ready := peer.next(udxStartTimeout, "its ready line")
	if err := st.Connect(udxJSID, udxAddr(ready.Port)); err != nil {
		t.Fatalf("connect the Go stream to the JS echo: %v", err)
	}

	// Write and read the echo at the same time, so that neither side waits on a full pipe.
	block := udxRandomBlock(t)
	type written struct {
		sum string
		err error
	}
	w, echo, ok := udxRun(udxIntegrityDeadline, func() { st.Destroy() },
		func() written {
			sum, err := udxSource(st, block, udxIntegrity)
			return written{sum, err}
		},
		func() udxSinkResult { return udxSink(st) })
	if !ok {
		t.Fatalf("the Go sender or the echo did not finish %d bytes within %v: sender err %v, echo err %v, echo bytes %d",
			int64(udxIntegrity), udxIntegrityDeadline, w.err, echo.err, echo.n)
	}
	if w.err != nil {
		t.Fatalf("send to the JS echo: %v", w.err)
	}
	if echo.err != nil {
		t.Fatalf("read the echo: %v: stderr: %s", echo.err, peer.stderr.String())
	}
	udxCheckSink(t, echo, udxIntegrity, w.sum)

	ended := peer.next(udxTransfer, "its ended line")
	if !ended.Ended || ended.Bytes != udxIntegrity {
		t.Fatalf("JS echo ended with %d bytes, want %d: %+v", ended.Bytes, udxIntegrity, ended)
	}
	if ended.Hash != w.sum {
		t.Fatalf("JS echo received different bytes from those Go sent: got sha256 %s, want %s", ended.Hash, w.sum)
	}
	peer.closePeer()
	st.Destroy()
	peer.exit(udxStopTimeout)
}

// TestUDX_JSToGo: the JS side sends 1 GiB to Go, which hashes what it receives. The JS side reports the hash of
// what it sent, and it must equal Go's.
func TestUDX_JSToGo(t *testing.T) {
	ep := newUDXEndpoint(t)
	st := ep.sock.NewStream(udxGoID)
	t.Cleanup(func() { st.Destroy() })
	peer := startUDXPeer(t, "source", strconv.Itoa(ep.addr.Port), strconv.Itoa(udxGoID), strconv.Itoa(udxJSID))
	ready := peer.next(udxStartTimeout, "its ready line")
	if err := st.Connect(udxJSID, udxAddr(ready.Port)); err != nil {
		t.Fatalf("connect the Go stream to the JS source: %v", err)
	}

	peer.command(map[string]any{"cmd": "send", "bytes": udxIntegrity})
	got, ok := udxSinkWithin(st, udxIntegrityDeadline)
	if !ok {
		t.Fatalf("the JS sender did not end its %d bytes within %v: %d received", int64(udxIntegrity), udxIntegrityDeadline, got.n)
	}
	sent := peer.next(udxTransfer, "its reply to send")
	if sent.Bytes != udxIntegrity {
		t.Fatalf("JS source sent %d bytes, want %d", sent.Bytes, udxIntegrity)
	}
	udxCheckSink(t, got, udxIntegrity, sent.Sent)
	if err := st.CloseWrite(); err != nil {
		t.Fatalf("end the Go write side: %v", err)
	}
	peer.closePeer()
	st.Destroy()
	peer.exit(udxStopTimeout)
}

// TestUDX_Messages: 256 unordered messages each way. Every message must arrive once, with the content its index
// implies. Messages are not acked, so a lost one would show here as a missing index.
func TestUDX_Messages(t *testing.T) {
	t.Run("GoToJS", func(t *testing.T) {
		ep := newUDXEndpoint(t)
		st := ep.sock.NewStream(udxGoID)
		t.Cleanup(func() { st.Destroy() })
		peer := startUDXPeer(t, "sink", strconv.Itoa(ep.addr.Port), strconv.Itoa(udxGoID), strconv.Itoa(udxJSID))
		ready := peer.next(udxStartTimeout, "its ready line")
		if err := st.Connect(udxJSID, udxAddr(ready.Port)); err != nil {
			t.Fatalf("connect the Go stream to the JS sink: %v", err)
		}
		for i := range udxMessages {
			if err := st.SendMessage(udxMessageBody(uint32(i))); err != nil {
				t.Fatalf("send message %d: %v", i, err)
			}
		}
		// The messages travel on their own, so the JS side may not have read them yet. Ask until they all
		// have arrived or the deadline passes.
		deadline := time.Now().Add(udxReply)
		var rep udxLine
		for {
			peer.command(map[string]string{"cmd": "report"})
			rep = peer.next(udxReply, "its report")
			if rep.Valid == udxMessages || time.Now().After(deadline) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if rep.Valid != udxMessages || rep.Bad != 0 || rep.Dup != 0 {
			t.Fatalf("JS sink got %d of %d messages intact (%d bad, %d duplicate) after %d arrivals",
				rep.Valid, udxMessages, rep.Bad, rep.Dup, rep.Messages)
		}
		peer.closePeer()
		st.Destroy()
		peer.exit(udxStopTimeout)
	})

	t.Run("JSToGo", func(t *testing.T) {
		ep := newUDXEndpoint(t)
		st := ep.sock.NewStream(udxGoID)
		t.Cleanup(func() { st.Destroy() })
		peer := startUDXPeer(t, "source", strconv.Itoa(ep.addr.Port), strconv.Itoa(udxGoID), strconv.Itoa(udxJSID))
		ready := peer.next(udxStartTimeout, "its ready line")
		if err := st.Connect(udxJSID, udxAddr(ready.Port)); err != nil {
			t.Fatalf("connect the Go stream to the JS source: %v", err)
		}
		peer.command(map[string]any{"cmd": "messages", "count": udxMessages})
		if reply := peer.next(udxReply, "its reply to messages"); reply.Messages != udxMessages {
			t.Fatalf("JS source sent %d messages, want %d", reply.Messages, udxMessages)
		}
		seen := make(map[uint32]bool, udxMessages)
		bad, dup := 0, 0
		timeout := time.After(udxReply)
		for len(seen) < udxMessages {
			select {
			case m, ok := <-st.Messages():
				if !ok {
					t.Fatalf("the Go stream closed after %d of %d messages", len(seen), udxMessages)
				}
				i, ok := udxMessageIndex(m)
				switch {
				case !ok:
					bad++
				case seen[i]:
					dup++
				default:
					seen[i] = true
				}
			case <-timeout:
				t.Fatalf("only %d of %d messages arrived in %v (%d bad, %d duplicate)",
					len(seen), udxMessages, udxReply, bad, dup)
			}
		}
		if bad != 0 || dup != 0 {
			t.Fatalf("messages from JS: %d bad and %d duplicate", bad, dup)
		}
		peer.closePeer()
		st.Destroy()
		peer.exit(udxStopTimeout)
	})
}

// udxSent is what a sending call reports: the SHA-256 of the bytes it wrote, in hex, and its error.
type udxSent struct {
	sum string
	err error
}

// udxBenchGoToGo moves total bytes between two Go sockets in this process. It returns the receiver's Mbit/s, or an
// error when the transfer stalls or the bytes differ.
func udxBenchGoToGo(t *testing.T, total int64) (float64, error) {
	t.Helper()
	block := udxRandomBlock(t)
	a := newUDXEndpoint(t)
	b := newUDXEndpoint(t)
	sender := a.sock.NewStream(udxGoID)
	receiver := b.sock.NewStream(udxJSID)
	t.Cleanup(func() { sender.Destroy(); receiver.Destroy() })
	if err := sender.Connect(udxJSID, b.addr); err != nil {
		t.Fatalf("connect the Go sender: %v", err)
	}
	if err := receiver.Connect(udxGoID, a.addr); err != nil {
		t.Fatalf("connect the Go receiver: %v", err)
	}
	s, res, ok := udxRun(udxBenchDeadline, func() { sender.Destroy(); receiver.Destroy() },
		func() udxSent {
			sum, err := udxSource(sender, block, total)
			return udxSent{sum, err}
		},
		func() udxSinkResult { return udxSink(receiver) })
	if !ok {
		return 0, fmt.Errorf("stalled: after %v the receiver had %d of %d bytes and the sender err was %v",
			udxBenchDeadline, res.n, total, s.err)
	}
	if s.err != nil {
		return 0, fmt.Errorf("Go sender: %v", s.err)
	}
	if res.err != nil {
		return 0, fmt.Errorf("Go receiver: %v", res.err)
	}
	if res.n != total || res.sum != s.sum {
		return 0, fmt.Errorf("Go receiver got %d bytes, sha256 %s; want %d bytes, sha256 %s", res.n, res.sum, total, s.sum)
	}
	return udxMbit(total, res.span), nil
}

// udxBenchJSToJS runs the self mode of udx-peer.js: a sender and a receiver, both udx-native, in one process.
func udxBenchJSToJS(t *testing.T, total int64) (float64, error) {
	t.Helper()
	peer := startUDXPeer(t, "self", strconv.FormatInt(total, 10))
	res := peer.next(udxBenchDeadline, "its result")
	if res.Bytes != total || res.Sent != res.Received {
		return 0, fmt.Errorf("JS to JS moved %d bytes, want %d; sent sha256 %s, received sha256 %s",
			res.Bytes, total, res.Sent, res.Received)
	}
	peer.exit(udxStopTimeout)
	return udxMbit(total, time.Duration(res.Seconds*float64(time.Second))), nil
}

// udxBenchGoToJS sends total bytes from Go to a JS sink in another process. It returns the JS receiver's Mbit/s, or
// an error when the transfer stalls or the bytes differ.
func udxBenchGoToJS(t *testing.T, total int64) (float64, error) {
	t.Helper()
	block := udxRandomBlock(t)
	ep := newUDXEndpoint(t)
	st := ep.sock.NewStream(udxGoID)
	t.Cleanup(func() { st.Destroy() })
	peer := startUDXPeer(t, "sink", strconv.Itoa(ep.addr.Port), strconv.Itoa(udxGoID), strconv.Itoa(udxJSID))
	ready := peer.next(udxStartTimeout, "its ready line")
	if err := st.Connect(udxJSID, udxAddr(ready.Port)); err != nil {
		t.Fatalf("connect the Go stream to the JS sink: %v", err)
	}
	sent := make(chan udxSent, 1)
	go func() {
		sum, err := udxSource(st, block, total)
		sent <- udxSent{sum, err}
	}()
	var s udxSent
	select {
	case s = <-sent:
	case <-time.After(udxBenchDeadline):
		st.Destroy()
		<-sent
		return 0, fmt.Errorf("stalled: the Go sender had not finished %d bytes after %v", total, udxBenchDeadline)
	}
	if s.err != nil {
		return 0, fmt.Errorf("Go sender: %v", s.err)
	}
	ended := peer.next(udxTransfer, "its ended line")
	if !ended.Ended || ended.Bytes != total || ended.Hash != s.sum {
		return 0, fmt.Errorf("JS sink ended with %d bytes, sha256 %s; want %d bytes, sha256 %s",
			ended.Bytes, ended.Hash, total, s.sum)
	}
	peer.closePeer()
	st.Destroy()
	peer.exit(udxStopTimeout)
	return udxMbit(total, time.Duration(ended.Seconds*float64(time.Second))), nil
}

// udxBenchJSToGo has a JS source send total bytes to a Go receiver in this process. It returns the Go receiver's
// Mbit/s, or an error when the transfer stalls or the bytes differ.
func udxBenchJSToGo(t *testing.T, total int64) (float64, error) {
	t.Helper()
	ep := newUDXEndpoint(t)
	st := ep.sock.NewStream(udxGoID)
	t.Cleanup(func() { st.Destroy() })
	peer := startUDXPeer(t, "source", strconv.Itoa(ep.addr.Port), strconv.Itoa(udxGoID), strconv.Itoa(udxJSID))
	ready := peer.next(udxStartTimeout, "its ready line")
	if err := st.Connect(udxJSID, udxAddr(ready.Port)); err != nil {
		t.Fatalf("connect the Go stream to the JS source: %v", err)
	}
	peer.command(map[string]any{"cmd": "send", "bytes": total})
	res, ok := udxSinkWithin(st, udxBenchDeadline)
	if !ok {
		return 0, fmt.Errorf("stalled: the Go receiver had %d of %d bytes after %v", res.n, total, udxBenchDeadline)
	}
	if res.err != nil {
		return 0, fmt.Errorf("Go receiver: %v", res.err)
	}
	sent := peer.next(udxTransfer, "its reply to send")
	if res.n != total || res.sum != sent.Sent {
		return 0, fmt.Errorf("Go receiver got %d bytes, sha256 %s; the JS source sent %d bytes, sha256 %s",
			res.n, res.sum, total, sent.Sent)
	}
	if err := st.CloseWrite(); err != nil {
		t.Fatalf("end the Go write side: %v", err)
	}
	peer.closePeer()
	st.Destroy()
	peer.exit(udxStopTimeout)
	return udxMbit(total, res.span), nil
}

// TestUDX_Throughput prints the throughput table on 127.0.0.1: udxBenchRuns runs of udxBenchBytes per row, with
// the median, minimum and maximum of the runs that finished. Run it with -v to see the table. Each run checks its
// own digest, and a run that stalls is counted as failed and fails the test. Rows with both ends in one process
// share one scheduler, so compare the two-process rows with each other.
func TestUDX_Throughput(t *testing.T) {
	if testing.Short() {
		t.Skip("interop: the throughput table is skipped in -short mode")
	}
	requireNodeJS(t)
	rows := []struct {
		name string
		run  func(*testing.T, int64) (float64, error)
	}{
		{"Go to Go (one process)", udxBenchGoToGo},
		{"JS to JS (one process, udx-native both ends)", udxBenchJSToJS},
		{"Go to JS (two processes)", udxBenchGoToJS},
		{"JS to Go (two processes)", udxBenchJSToGo},
	}
	mbit := make([][]float64, len(rows))
	errs := make([][]error, len(rows))
	for run := 0; run < udxBenchRuns; run++ {
		for i, row := range rows {
			m, err := row.run(t, udxBenchBytes)
			if err != nil {
				errs[i] = append(errs[i], err)
				continue
			}
			mbit[i] = append(mbit[i], m)
		}
	}
	t.Logf("UDX throughput on 127.0.0.1, %d MiB per run, %d runs per row; the receiver's span, first byte to end of stream",
		udxBenchBytes>>20, udxBenchRuns)
	t.Log("| Pair | Runs finished | Median Mbit/s | Min | Max | Failed runs |")
	t.Log("|---|---|---|---|---|---|")
	failed := 0
	for i, row := range rows {
		failed += len(errs[i])
		xs := append([]float64(nil), mbit[i]...)
		if len(xs) == 0 {
			t.Logf("| %s | 0 of %d | - | - | - | %d |", row.name, udxBenchRuns, len(errs[i]))
			continue
		}
		sort.Float64s(xs)
		median := xs[len(xs)/2]
		if len(xs)%2 == 0 {
			median = (xs[len(xs)/2-1] + xs[len(xs)/2]) / 2
		}
		t.Logf("| %s | %d of %d | %.0f | %.0f | %.0f | %d |", row.name, len(xs), udxBenchRuns, median, xs[0], xs[len(xs)-1], len(errs[i]))
	}
	for i, row := range rows {
		for _, err := range errs[i] {
			t.Logf("%s: %v", row.name, err)
		}
	}
	if failed > 0 {
		t.Errorf("%d of %d throughput runs failed; the rows above say which", failed, udxBenchRuns*len(rows))
	}
}
