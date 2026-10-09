// Ported from dht-rpc 6.27.0 lib/io.js and lib/peer.js, MIT License, Copyright (c) 2021 Mathias
// Buus.
//
// The RPC socket. A request is matched to its reply by tid, and a request that gets no reply is sent
// again, up to upstream's retry count. Incoming requests go to a handler, and a request token is
// checked against two secrets that rotate. Timeouts, retries and tid allocation are upstream's
// defaults. Not ported: the congestion window, the adaptive timeout, suspend and resume, and the
// closer nodes (the handler fills them in).
package dhtrpc

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"os"
	"sync"
	"time"

	"golang.org/x/crypto/blake2b"
)

// PacketConn is the datagram socket an IO runs on. A Node runs it on the RPC side of a udx.Socket, which
// shares the UDP socket with UDX streams and hands dht-rpc the datagrams that are not UDX packets.
type PacketConn interface {
	ReadFrom([]byte) (int, net.Addr, error)
	WriteTo([]byte, net.Addr) (int, error)
	Close() error
}

// ErrClosed is returned by Request when the IO is closed before or during the request.
var ErrClosed = errors.New("dhtrpc: IO closed")

// errNotIPv4 is returned by Request for a destination that the packet layout cannot carry.
var errNotIPv4 = errors.New("dhtrpc: destination is not IPv4")

// codeInvalidToken is the error code of an INVALID_TOKEN reply (lib/errors.js).
const codeInvalidToken = 2

// Upstream defaults. A request is sent, then sent again requestRetries times, and each send waits
// requestWait for its reply. After the last wait the request fails with os.ErrDeadlineExceeded.
const (
	requestRetries = 3
	requestWait    = time.Second
	secretRotate   = 7500 * time.Millisecond // upstream rotates every 10 drains of 750 ms
	maxDatagram    = 1 << 16
)

// IO sends dht-rpc requests and answers the requests it receives, over one PacketConn.
type IO struct {
	conn     PacketConn
	handler  func(req *Request, from *net.UDPAddr) *Response
	closed   chan struct{}
	closeOne sync.Once
	closeErr error

	mu       sync.Mutex // guards inflight, nextTid, secrets and punch
	inflight map[uint16]*pending
	nextTid  uint16
	secrets  [2][32]byte
	punch    func(from *net.UDPAddr) // the holepunch datagrams, set by setPunch
}

// pending is a request that waits for its reply. The reader sends the reply on answer, once.
type pending struct {
	answer chan *Response
}

// NewIO returns an IO on conn that answers incoming requests with handler. The handler returns the
// reply, or nil for no reply; the IO sets the reply's Tid and To, and a Token when it has none.
// NewIO starts reading conn at once. Close stops it.
func NewIO(conn PacketConn, handler func(req *Request, from *net.UDPAddr) *Response) *IO {
	io := &IO{
		conn:     conn,
		handler:  handler,
		closed:   make(chan struct{}),
		inflight: make(map[uint16]*pending),
	}
	rand.Read(io.secrets[0][:])
	rand.Read(io.secrets[1][:])
	var tid [2]byte
	rand.Read(tid[:])
	io.nextTid = binary.LittleEndian.Uint16(tid[:])
	go io.readLoop()
	go io.rotateLoop()
	return io
}

// Request sends req to the address to and returns the reply that carries the same tid. Request sets
// req.Tid and req.To. Each send waits 1000 ms for a reply; after 4 sends without one, Request fails
// with os.ErrDeadlineExceeded. It fails with ctx.Err() when ctx ends first, and with ErrClosed when
// the IO is closed.
func (io *IO) Request(ctx context.Context, to *net.UDPAddr, req Request) (*Response, error) {
	return io.requestRetry(ctx, to, req, requestRetries, nil)
}

// requestRetry is Request with the resend count set by the caller: the request is sent retries+1 times
// before it fails. A query passes upstream's query retries. cycle, when set, is called on each send that
// gets no reply within its wait, before the request is sent again or fails, as upstream's oncycle is. It
// runs on the requesting goroutine.
func (io *IO) requestRetry(ctx context.Context, to *net.UDPAddr, req Request, retries int, cycle func()) (*Response, error) {
	if to.IP.To4() == nil {
		return nil, errNotIPv4
	}
	p := &pending{answer: make(chan *Response, 1)}
	tid, err := io.register(p)
	if err != nil {
		return nil, err
	}
	defer io.unregister(tid, p)
	req.Tid = tid
	req.To = toAddr(to)
	pkt := EncodeRequest(req)
	for sent := 1; ; sent++ {
		io.conn.WriteTo(pkt, to) // a failed send is left to the retries, as upstream does
		timer := time.NewTimer(requestWait)
		select {
		case resp := <-p.answer:
			timer.Stop()
			return resp, nil
		case <-timer.C:
			if cycle != nil {
				cycle()
			}
			if sent > retries {
				return nil, os.ErrDeadlineExceeded
			}
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-io.closed:
			timer.Stop()
			return nil, ErrClosed
		}
	}
}

// Close stops the IO and closes its PacketConn. Pending requests fail with ErrClosed. Close can be
// called more than once; every call returns the result of the first.
func (io *IO) Close() error {
	io.closeOne.Do(func() {
		close(io.closed)
		io.closeErr = io.conn.Close()
	})
	return io.closeErr
}

// readLoop handles the datagrams that arrive until the PacketConn is closed. Another read error, such
// as an ICMP reset on Windows, is skipped.
func (io *IO) readLoop() {
	buf := make([]byte, maxDatagram)
	for {
		n, from, err := io.conn.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) || io.isClosed() {
				io.Close()
				return
			}
			continue
		}
		if addr, ok := from.(*net.UDPAddr); ok {
			io.onDatagram(buf[:n], addr)
		}
	}
}

// rotateLoop moves the token secrets on every secretRotate until the IO is closed.
func (io *IO) rotateLoop() {
	t := time.NewTicker(secretRotate)
	defer t.Stop()
	for {
		select {
		case <-io.closed:
			return
		case <-t.C:
			io.rotate()
		}
	}
}

// onDatagram handles one datagram. Upstream drops a datagram from port 0, and this layout is IPv4
// only, so other senders are dropped too. A datagram under 2 bytes is a holepunch datagram: it goes to
// the punch handler, never to Decode, as upstream's socket pool routes it (lib/socket-pool.js).
func (io *IO) onDatagram(b []byte, from *net.UDPAddr) {
	if from.Port == 0 || from.IP.To4() == nil {
		return
	}
	if len(b) < 2 {
		io.mu.Lock()
		punch := io.punch
		io.mu.Unlock()
		if punch != nil {
			punch(from)
		}
		return
	}
	v, err := Decode(b)
	if err != nil {
		return
	}
	switch m := v.(type) {
	case *Request:
		io.onRequest(m, from)
	case *Response:
		io.onResponse(m, from)
	}
}

// onRequest checks the id and token of a request, then passes it to the handler. A token that matches
// neither secret gets an INVALID_TOKEN reply. An id that does not match the sender is cleared, and the
// request still goes to the handler, as upstream does.
func (io *IO) onRequest(req *Request, from *net.UDPAddr) {
	req.From = from
	if req.ID != nil && !bytes.Equal(req.ID, peerID(from)) {
		req.ID = nil
	}
	if req.Token != nil && !bytes.Equal(req.Token, io.token(from, 1)) && !bytes.Equal(req.Token, io.token(from, 0)) {
		io.send(from, Response{Tid: req.Tid, Error: codeInvalidToken, Token: io.token(from, 1)})
		return
	}
	if io.handler == nil {
		return
	}
	resp := io.handler(req, from)
	if resp == nil {
		return
	}
	resp.Tid = req.Tid
	if resp.Token == nil && resp.Error == 0 && !resp.NoToken {
		resp.Token = io.token(from, 1)
	}
	io.send(from, *resp)
}

// onResponse hands a reply to the request with its tid. A reply that matches no request in flight is
// ignored. An id that does not match the sender is cleared, and the reply is still delivered.
func (io *IO) onResponse(res *Response, from *net.UDPAddr) {
	res.From = from
	if res.ID != nil && !bytes.Equal(res.ID, peerID(from)) {
		res.ID = nil
	}
	io.mu.Lock()
	p := io.inflight[res.Tid]
	delete(io.inflight, res.Tid)
	io.mu.Unlock()
	if p != nil {
		p.answer <- res
	}
}

// register puts p in flight under the next tid. The tid counter wraps at 65536, as upstream's does.
func (io *IO) register(p *pending) (uint16, error) {
	io.mu.Lock()
	defer io.mu.Unlock()
	if io.isClosed() {
		return 0, ErrClosed
	}
	tid := io.nextTid
	io.nextTid++
	io.inflight[tid] = p
	return tid, nil
}

// unregister takes p out of flight, unless a later request has reused its tid.
func (io *IO) unregister(tid uint16, p *pending) {
	io.mu.Lock()
	defer io.mu.Unlock()
	if io.inflight[tid] == p {
		delete(io.inflight, tid)
	}
}

// setPunch sets the handler of the holepunch datagrams that arrive on the IO's conn. A nil handler drops them.
func (io *IO) setPunch(handler func(from *net.UDPAddr)) {
	io.mu.Lock()
	defer io.mu.Unlock()
	io.punch = handler
}

// isClosed reports whether Close has been called.
func (io *IO) isClosed() bool {
	select {
	case <-io.closed:
		return true
	default:
		return false
	}
}

// send writes r to to, with To set to to. A reply goes to the requester, or to another address when a
// relayed request is answered for its client.
func (io *IO) send(to *net.UDPAddr, r Response) {
	r.To = toAddr(to)
	io.conn.WriteTo(EncodeResponse(r), to)
}

// relay writes req to to, with the tid it carries and no token, and waits for no reply. It is how a
// node passes a request on, as upstream's request.relay does.
func (io *IO) relay(to *net.UDPAddr, req Request) {
	if to.IP.To4() == nil {
		return
	}
	req.To = toAddr(to)
	io.conn.WriteTo(EncodeRequest(req), to)
}

// token returns the token for addr under secret i: a keyed BLAKE2b-256 of the host, as upstream's
// token() computes it.
func (io *IO) token(addr *net.UDPAddr, i int) []byte {
	io.mu.Lock()
	secret := io.secrets[i]
	io.mu.Unlock()
	h, _ := blake2b.New256(secret[:])
	h.Write([]byte(addr.IP.String()))
	return h.Sum(nil)
}

// rotate moves the secrets on as upstream does: the first takes the second's value, and the second
// becomes the unkeyed BLAKE2b-256 of the old first.
func (io *IO) rotate() {
	io.mu.Lock()
	defer io.mu.Unlock()
	old := io.secrets[0]
	io.secrets[0] = io.secrets[1]
	io.secrets[1] = blake2b.Sum256(old[:])
}

// peerID returns the id upstream's peer.id gives an address: the unkeyed BLAKE2b-256 of its IPv4 host
// and its port as uint16 LE.
func peerID(addr *net.UDPAddr) []byte {
	var b [6]byte
	copy(b[:4], addr.IP.To4())
	binary.LittleEndian.PutUint16(b[4:], uint16(addr.Port))
	sum := blake2b.Sum256(b[:])
	return sum[:]
}

// toAddr returns a as the packet layout names it: its IPv4 host and port. The caller checks that a
// is IPv4.
func toAddr(a *net.UDPAddr) Addr {
	return Addr{Host: netip.AddrFrom4([4]byte(a.IP.To4())), Port: uint16(a.Port)}
}
