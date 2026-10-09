package dhtrpc

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"
)

// Upstream dht-rpc 6.27.0 lib/io.js: a request is sent once and then retried upstream's count of 3
// times (Request.retries), and each send waits 1000 ms for a reply (the default in _sendNow). After the
// last wait the request fails. So 4 datagrams arrive, and the failure comes at least 4 s after the
// first send.
const (
	upstreamRetries = 3
	upstreamWait    = 1000 * time.Millisecond
)

// listen opens a UDP socket on a free port of 127.0.0.1. The test closes it when it ends.
func listen(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// newIO starts an IO with handler on a new socket, and returns it with the socket. The test closes
// both when it ends.
func newIO(t *testing.T, handler func(*Request, *net.UDPAddr) *Response) (*IO, *net.UDPConn) {
	t.Helper()
	conn := listen(t)
	rpc := NewIO(conn, handler)
	if rpc == nil {
		t.Fatal("NewIO: not implemented")
	}
	t.Cleanup(func() { rpc.Close() })
	return rpc, conn
}

// noReply is a handler that never answers.
func noReply(*Request, *net.UDPAddr) *Response { return nil }

// addrOf returns the local address of conn.
func addrOf(conn *net.UDPConn) *net.UDPAddr {
	return conn.LocalAddr().(*net.UDPAddr)
}

// dhtAddr returns a as the packet layout names it: an IPv4 host and a port. The tests use loopback only.
func dhtAddr(a *net.UDPAddr) Addr {
	return Addr{Host: netip.MustParseAddr("127.0.0.1"), Port: uint16(a.Port)}
}

// checkImplemented fails the test with "not implemented" when err is the stub signal.
func checkImplemented(t *testing.T, op string, err error) {
	t.Helper()
	if errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("%s: not implemented", op)
	}
}

// silentPeer listens on a free port and reports each datagram it receives on the returned channel.
// It never replies.
func silentPeer(t *testing.T) (*net.UDPAddr, <-chan []byte) {
	t.Helper()
	conn := listen(t)
	got := make(chan []byte, 100)
	go func() {
		buf := make([]byte, 2048)
		for {
			n, _, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			got <- bytes.Clone(buf[:n])
		}
	}()
	return addrOf(conn), got
}

// countDatagrams counts the datagrams that arrive on got within wait.
func countDatagrams(got <-chan []byte, wait time.Duration) int {
	n := 0
	deadline := time.After(wait)
	for {
		select {
		case <-got:
			n++
		case <-deadline:
			return n
		}
	}
}

// isTimeout reports whether err is a timeout: a context deadline, or an error whose Timeout method
// reports true. The spec does not name the error type, so either shape counts.
func isTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}

// readRequest reads one datagram from conn, which must hold a request, and returns it with its sender.
func readRequest(conn *net.UDPConn) (*Request, *net.UDPAddr, error) {
	buf := make([]byte, 2048)
	n, from, err := conn.ReadFrom(buf)
	if err != nil {
		return nil, nil, err
	}
	v, err := Decode(buf[:n])
	if err != nil {
		return nil, nil, err
	}
	req, ok := v.(*Request)
	if !ok {
		return nil, nil, fmt.Errorf("datagram decodes to %T, want *Request", v)
	}
	return req, from.(*net.UDPAddr), nil
}

// reply sends r from conn to the requester at to, as dht-rpc replies do. r.To is set to the requester.
func reply(conn *net.UDPConn, to *net.UDPAddr, r Response) {
	r.To = dhtAddr(to)
	conn.WriteTo(EncodeResponse(r), to)
}

// Test case 1: a request gets the handler's reply, and the reply carries the request's tid. The
// handler does not set Tid: the IO sets it, as upstream does.
func TestRequestGetsHandlerResponse(t *testing.T) {
	seen := make(chan *Request, 10)
	_, serverConn := newIO(t, func(req *Request, from *net.UDPAddr) *Response {
		seen <- req
		return &Response{Value: []byte("pong")}
	})
	client, _ := newIO(t, noReply)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := client.Request(ctx, addrOf(serverConn), Request{Value: []byte("ping")})
	checkImplemented(t, "Request", err)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	var req *Request
	select {
	case req = <-seen:
	default:
		t.Fatal("handler did not see the request")
	}
	if string(req.Value) != "ping" {
		t.Errorf("handler got value %q, want %q", req.Value, "ping")
	}
	if resp.Tid != req.Tid {
		t.Errorf("reply tid = %d, want the request tid %d", resp.Tid, req.Tid)
	}
	if string(resp.Value) != "pong" {
		t.Errorf("reply value = %q, want %q", resp.Value, "pong")
	}
}

// Test case 2: a request to a silent address is sent 4 times (the first send and upstream's 3
// retries), then fails with a timeout error.
func TestRequestRetriesThenTimesOut(t *testing.T) {
	to, got := silentPeer(t)
	client, _ := newIO(t, noReply)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	_, err := client.Request(ctx, to, Request{Value: []byte("ping")})
	elapsed := time.Since(start)
	checkImplemented(t, "Request", err)
	if !isTimeout(err) {
		t.Fatalf("Request error = %v, want a timeout", err)
	}
	if sent, want := countDatagrams(got, 500*time.Millisecond), upstreamRetries+1; sent != want {
		t.Errorf("silent peer got %d datagrams, want %d (the send and %d retries)", sent, want, upstreamRetries)
	}
	if least := (upstreamRetries + 1) * upstreamWait; elapsed < least {
		t.Errorf("Request failed after %v, want at least %v (one %v wait per send)", elapsed, least, upstreamWait)
	}
}

// Test case 3: a reply whose tid matches no request in flight is ignored. The peer answers the request
// with a reply for tid+1 first, then with the reply for the request's tid. The request must get the
// second one.
func TestUnknownTidReplyIgnored(t *testing.T) {
	client, _ := newIO(t, noReply)
	peer := listen(t)
	go func() {
		req, from, err := readRequest(peer)
		if err != nil {
			return
		}
		reply(peer, from, Response{Tid: req.Tid + 1, Value: []byte("stray")})
		reply(peer, from, Response{Tid: req.Tid, Value: []byte("answer")})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := client.Request(ctx, addrOf(peer), Request{Value: []byte("ping")})
	checkImplemented(t, "Request", err)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if string(resp.Value) != "answer" {
		t.Errorf("Request got reply %q, want %q: the reply with an unknown tid was not ignored", resp.Value, "answer")
	}
}

// Test case 4: a malformed datagram is dropped, and the read loop keeps serving. The bad datagrams
// are sent before a valid request. That request must get its reply, and the handler must have seen
// only the valid request.
func TestMalformedDatagramDropped(t *testing.T) {
	seen := make(chan *Request, 10)
	_, serverConn := newIO(t, func(req *Request, from *net.UDPAddr) *Response {
		seen <- req
		return &Response{Value: []byte("pong")}
	})
	client, _ := newIO(t, noReply)
	to := addrOf(serverConn)
	valid := EncodeRequest(Request{Tid: 7, To: dhtAddr(to), Value: []byte("ping")})
	bad := [][]byte{
		{},                                   // no type byte
		{0x03},                               // a request type byte with no flags
		{0x55, 0x00, 0x00, 0x00, 0x00, 0x00}, // unknown type byte
		valid[:len(valid)-1],                 // request cut short, so its value runs past the end
		{0x13, 0x00, 0x00},                   // reply cut short before its tid
	}
	raw := listen(t)
	for _, b := range bad {
		if _, err := raw.WriteTo(b, to); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := client.Request(ctx, to, Request{Value: []byte("ping")})
	checkImplemented(t, "Request", err)
	if err != nil {
		t.Fatalf("Request after malformed datagrams: %v", err)
	}
	if string(resp.Value) != "pong" {
		t.Errorf("reply value = %q, want %q", resp.Value, "pong")
	}
	if n := len(seen); n != 1 {
		t.Fatalf("handler ran %d times, want 1: malformed datagrams must not reach it", n)
	}
	if req := <-seen; string(req.Value) != "ping" {
		t.Errorf("handler got value %q, want %q", req.Value, "ping")
	}
}

// Test case 5: 100 concurrent requests on one IO all complete, each with the reply to its own request.
func TestConcurrentRequestsGetOwnReplies(t *testing.T) {
	_, serverConn := newIO(t, func(req *Request, from *net.UDPAddr) *Response {
		return &Response{Value: req.Value}
	})
	client, _ := newIO(t, noReply)
	to := addrOf(serverConn)
	const n = 100
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			want := []byte(fmt.Sprintf("request %d", i))
			resp, err := client.Request(ctx, to, Request{Value: want})
			if err != nil {
				errs <- fmt.Errorf("request %d: %w", i, err)
				return
			}
			if !bytes.Equal(resp.Value, want) {
				errs <- fmt.Errorf("request %d got reply %q, want %q", i, resp.Value, want)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		checkImplemented(t, "Request", err)
		t.Error(err)
	}
}

// Test case 6: Close makes a pending request fail promptly, well before its retries would run out.
func TestCloseFailsPendingRequests(t *testing.T) {
	to, got := silentPeer(t)
	client, _ := newIO(t, noReply)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := client.Request(ctx, to, Request{Value: []byte("ping")})
		done <- err
	}()
	// The first datagram at the silent peer shows that the request is pending.
	select {
	case <-got:
	case err := <-done:
		checkImplemented(t, "Request", err)
		t.Fatalf("Request returned before Close: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("silent peer got no datagram within 2s")
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Error("pending Request returned no error after Close")
		}
	case <-time.After(time.Second):
		t.Fatal("pending Request did not fail within 1s of Close")
	}
}

// Edge case: upstream's peer id, token and secret rotation. The values were computed with dht-rpc
// 6.27.0 (peer.id) and sodium-universal under node 24.
func TestUpstreamVectors(t *testing.T) {
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234}
	if got, want := hex.EncodeToString(peerID(addr)), "c151d5e4efb09c79aad416d2bf496fec7b7db045499b1ab7afb9dbdb53b370c6"; got != want {
		t.Errorf("peerID(127.0.0.1:1234) = %s, want %s", got, want)
	}
	rpc := &IO{}
	copy(rpc.secrets[1][:], bytes.Repeat([]byte{2}, 32))
	host := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}
	if got, want := hex.EncodeToString(rpc.token(host, 1)), "4afe340b2ee2790612f061108c4a3f00033b14ccc89e10288446347da5bd8368"; got != want {
		t.Errorf("token(127.0.0.1, 1) = %s, want %s", got, want)
	}
	copy(rpc.secrets[0][:], bytes.Repeat([]byte{1}, 32))
	rpc.rotate()
	if got, want := hex.EncodeToString(rpc.secrets[1][:]), "f40ceaf86e5776923332b8d8fd3bef849cadb19c6996bc272af1f648d9566a4c"; got != want {
		t.Errorf("secret 1 after rotate = %s, want %s", got, want)
	}
}

// Edge case: a reply whose id does not match its sender still reaches the request, with its ID cleared.
// A reply with the sender's own id keeps it.
func TestReplierIDChecked(t *testing.T) {
	client, _ := newIO(t, noReply)
	peer := listen(t)
	wrong := bytes.Repeat([]byte{0x11}, 32)
	right := peerID(addrOf(peer))
	go func() {
		for _, id := range [][]byte{wrong, right} {
			req, from, err := readRequest(peer)
			if err != nil {
				return
			}
			reply(peer, from, Response{Tid: req.Tid, ID: id, Value: []byte("answer")})
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i, want := range [][]byte{nil, right} {
		resp, err := client.Request(ctx, addrOf(peer), Request{Value: []byte("ping")})
		checkImplemented(t, "Request", err)
		if err != nil {
			t.Fatalf("Request %d: %v", i, err)
		}
		if string(resp.Value) != "answer" {
			t.Errorf("Request %d got value %q, want %q", i, resp.Value, "answer")
		}
		if !bytes.Equal(resp.ID, want) {
			t.Errorf("Request %d reply ID = %x, want %x", i, resp.ID, want)
		}
	}
}

// Edge case: a token from a reply is accepted on the next request. A token that matches neither secret
// gets an INVALID_TOKEN reply, and its request never reaches the handler.
func TestRequestTokens(t *testing.T) {
	seen := make(chan *Request, 10)
	_, serverConn := newIO(t, func(req *Request, from *net.UDPAddr) *Response {
		seen <- req
		return &Response{Value: []byte("pong")}
	})
	client, _ := newIO(t, noReply)
	to := addrOf(serverConn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first, err := client.Request(ctx, to, Request{Value: []byte("ping")})
	checkImplemented(t, "Request", err)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if len(first.Token) != 32 {
		t.Fatalf("reply token is %d bytes, want 32", len(first.Token))
	}
	resp, err := client.Request(ctx, to, Request{Token: first.Token, Value: []byte("ping")})
	if err != nil {
		t.Fatalf("Request with the reply token: %v", err)
	}
	if string(resp.Value) != "pong" {
		t.Errorf("reply value = %q, want %q", resp.Value, "pong")
	}
	resp, err = client.Request(ctx, to, Request{Token: bytes.Repeat([]byte{0x42}, 32), Value: []byte("ping")})
	if err != nil {
		t.Fatalf("Request with a bad token: %v", err)
	}
	if resp.Error != codeInvalidToken {
		t.Errorf("bad token: reply error = %d, want %d", resp.Error, codeInvalidToken)
	}
	if n := len(seen); n != 2 {
		t.Errorf("handler ran %d times, want 2: a request with a bad token must not reach it", n)
	}
}

// Edge case: a datagram from port 0 or from a non-IPv4 sender is dropped, and a valid one from an IPv4
// port is not.
func TestUnlistedSendersDropped(t *testing.T) {
	seen := make(chan *Request, 10)
	rpc, _ := newIO(t, func(req *Request, from *net.UDPAddr) *Response {
		seen <- req
		return nil
	})
	valid := EncodeRequest(Request{Tid: 9, To: Addr{Host: netip.MustParseAddr("127.0.0.1"), Port: 1}, Value: []byte("ping")})
	rpc.onDatagram(valid, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	rpc.onDatagram(valid, &net.UDPAddr{IP: net.ParseIP("::1"), Port: 1000})
	if n := len(seen); n != 0 {
		t.Fatalf("handler ran %d times for dropped senders, want 0", n)
	}
	rpc.onDatagram(valid, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1000})
	if n := len(seen); n != 1 {
		t.Errorf("handler ran %d times for a valid sender, want 1", n)
	}
}

// Test case 9: a reply carries the address it came from in From, which the IO sets when it receives the
// reply. A caller that asked a node checks From against that node.
func TestReplyCarriesSender(t *testing.T) {
	server := listen(t)
	go func() {
		req, from, err := readRequest(server)
		if err != nil {
			return
		}
		reply(server, from, Response{Tid: req.Tid, Value: []byte("pong")})
	}()
	client, _ := newIO(t, noReply)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	want := addrOf(server)
	resp, err := client.Request(ctx, want, Request{Value: []byte("ping")})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if resp.From == nil || resp.From.String() != want.String() {
		t.Errorf("reply From = %v, want %v", resp.From, want)
	}
}
