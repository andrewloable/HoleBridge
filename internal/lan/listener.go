package lan

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/andrewloable/HoleBridge/pears/noise"
	"github.com/andrewloable/HoleBridge/pears/secretstream"
)

// ListenerConfig holds the limits of the LAN TCP listener (docs/architecture.md, Limits).
type ListenerConfig struct {
	HandshakeDeadline time.Duration // a connection that has not proven its client key by then is closed
	MaxUnauth         int           // unauthenticated connections in total
	MaxUnauthPerIP    int           // unauthenticated connections from one source IP
	Keepalive         time.Duration // interval of empty keepalive frames on each accepted stream while idle; zero means 5 s
	Log               *slog.Logger  // refusals are logged at most once a minute

	now func() time.Time // the clock of the log rate limit; nil means time.Now (tests set it)
}

// defaultKeepalive is the keepalive interval of an accepted stream when ListenerConfig.Keepalive is zero.
// It matches the keepalive const in internal/host/host.go, which the streams of the DHT route use.
const defaultKeepalive = 5 * time.Second

// maxNoiseMessage bounds the frames an unauthenticated peer may send. The Noise handshake messages are
// read under it, and so is every frame until the peer is proven (frameGuard): a frame that names more is
// refused on its length prefix, before any of its bytes is read. It is the bound secretstream puts on
// handshake messages.
const maxNoiseMessage = 65535

var (
	errNotAdmitted   = errors.New("lan: remote key is not admitted")
	errFrameTooLarge = errors.New("lan: handshake frame too large")
)

// Listener accepts LAN TCP connections, runs the Noise handshake on each under the host key, and
// returns the streams whose remote static key the admit function accepts.
//
// Each accepted connection holds an unauthenticated slot, in total and for its source IP, until it is
// proven: until the first message it sends decrypts under the session keys (see handshake). A
// connection that arrives when a cap is full is closed at accept and counted, and so is one that
// reaches the handshake deadline unproven.
type Listener struct {
	ln      net.Listener
	hostKey noise.KeyPair
	admit   func(remote [32]byte) bool
	cfg     ListenerConfig
	now     func() time.Time

	streams chan *secretstream.Stream // admitted streams, until Accept takes them
	done    chan struct{}             // closed when the accept loop ends, which is when ln is closed

	mu       sync.Mutex
	unauth   int            // connections still in their handshake, in total
	perIP    map[string]int // the same, by source IP
	rejected uint64         // connections the caps refused at accept
	lastLog  time.Time      // when a refusal was last logged; zero before the first
}

// NewListener serves the LAN TCP port ln under hostKey, and starts accepting connections at once.
// admit decides which remote static keys get a stream. It is called from several goroutines, so it
// must be safe for concurrent use. cfg sets the limits.
func NewListener(ln net.Listener, hostKey noise.KeyPair, admit func(remote [32]byte) bool, cfg ListenerConfig) *Listener {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.Keepalive == 0 {
		cfg.Keepalive = defaultKeepalive
	}
	now := cfg.now
	if now == nil {
		now = time.Now
	}
	l := &Listener{
		ln:      ln,
		hostKey: hostKey,
		admit:   admit,
		cfg:     cfg,
		now:     now,
		streams: make(chan *secretstream.Stream),
		done:    make(chan struct{}),
		perIP:   make(map[string]int),
	}
	go l.acceptLoop()
	return l
}

// Accept returns the next stream from an admitted client. It returns net.ErrClosed once ln is closed.
func (l *Listener) Accept() (*secretstream.Stream, error) {
	select {
	case s := <-l.streams:
		return s, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

// Rejected returns how many connections were refused: the caps refused them at accept, or they reached
// the handshake deadline unproven. A refusal is counted only once its log line, if it has one, is written.
func (l *Listener) Rejected() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rejected
}

// acceptLoop takes connections from ln until ln is closed. A connection the caps refuse is closed
// here, at once; any other gets its handshake on its own goroutine.
func (l *Listener) acceptLoop() {
	defer close(l.done)
	var backoff time.Duration
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// A transient error, such as running out of file descriptors: wait, then try again.
			backoff = min(max(2*backoff, 5*time.Millisecond), time.Second)
			time.Sleep(backoff)
			continue
		}
		backoff = 0
		ip := sourceIP(conn)
		if which, ok := l.reserve(ip); !ok {
			l.refuse(conn, which)
			continue
		}
		go l.serve(conn, ip)
	}
}

// reserve takes an unauthenticated slot for a connection from ip. It fails, and takes nothing, when
// the total or the per-IP cap is full, and then names the cap that is full.
func (l *Listener) reserve(ip string) (which string, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.unauth >= l.cfg.MaxUnauth:
		return "total cap", false
	case l.perIP[ip] >= l.cfg.MaxUnauthPerIP:
		return "per-IP cap", false
	}
	l.unauth++
	l.perIP[ip]++
	return "", true
}

// release gives back the slot that reserve took for ip, once the connection's handshake has ended.
func (l *Listener) release(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.unauth--
	l.perIP[ip]--
	if l.perIP[ip] == 0 {
		delete(l.perIP, ip)
	}
}

// refuse closes a connection the caps turned away, and counts it as a refusal at accept.
func (l *Listener) refuse(conn net.Conn, which string) {
	conn.Close()
	l.count("LAN connection refused at accept", which)
}

// count counts one refusal and logs it at most once a minute. msg says what happened and which names the
// limit involved. The log holds no address and no key.
func (l *Listener) count(msg, which string) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rejected++
	if l.lastLog.IsZero() || now.Sub(l.lastLog) >= time.Minute {
		l.lastLog = now
		l.cfg.Log.Warn(msg, "cap", which, "refused", l.rejected)
	}
}

// serve runs the handshake on a connection that holds a slot, then hands its stream to Accept. The
// slot is given back once the connection is proven, so a proven stream waiting for Accept holds none.
// A connection that is not proven by the handshake deadline is closed and counted as a refusal, and so
// is any connection that fails its handshake.
func (l *Listener) serve(conn net.Conn, ip string) {
	st, err := l.handshake(conn)
	l.release(ip)
	if err != nil {
		switch {
		case errors.Is(err, os.ErrDeadlineExceeded):
			l.count("LAN connection closed before its key was proven", "handshake deadline")
		case errors.Is(err, errFrameTooLarge):
			l.count("LAN connection closed for a frame over the limit before its key was proven", "frame size")
		}
		conn.Close()
		return
	}
	select {
	case l.streams <- st:
	case <-l.done:
		st.Destroy()
	}
}

// handshake runs the responder's side of the Noise IK handshake on conn, then the stream header
// exchange, then waits for proof, all within the handshake deadline, which is absolute from the start of
// the handshake. The remote key is judged after message 1, before message 2 is sent, so a client that is
// not admitted gets no reply at all.
//
// Proof is the first message the peer sends that decrypts under the session keys. A replayed message 1
// can finish the header exchange, but it cannot send a message that decrypts, and keepalives decrypt but
// are not messages. The proof is read without consuming it: a zero-length Read waits for the first
// non-empty message and leaves it pending in the stream, so the consumer's first Read or ReadFrame
// returns it whole. It returns the stream once the proof has arrived. Until the proof, every frame is read
// through a frameGuard, which refuses a frame that names more than maxNoiseMessage bytes.
func (l *Listener) handshake(conn net.Conn) (*secretstream.Stream, error) {
	if err := conn.SetDeadline(time.Now().Add(l.cfg.HandshakeDeadline)); err != nil {
		return nil, err
	}
	guard := &frameGuard{Conn: conn, limit: maxNoiseMessage}
	hs := noise.NewResponder(l.hostKey, nil)
	msg1, err := readFrame(guard, maxNoiseMessage)
	if err != nil {
		return nil, err
	}
	if _, err := hs.Recv(msg1); err != nil {
		return nil, err
	}
	msg2, err := hs.Send(nil)
	if err != nil {
		return nil, err
	}
	tx, rx, hash, peer := hs.Result()
	if !l.admit(peer) {
		return nil, errNotAdmitted
	}
	if err := writeFrame(conn, msg2); err != nil {
		return nil, err
	}
	// The Noise handshake is done above, so Resume runs only the header exchange (as HyperDHT does).
	st := secretstream.Resume(guard, false, secretstream.Options{Keepalive: l.cfg.Keepalive}, secretstream.Keys{Tx: tx, Rx: rx, Hash: hash, Peer: peer})
	if err := st.Handshake(context.Background()); err != nil {
		return nil, err
	}
	// The proof. Destroy also stops the stream's goroutines, which Handshake started.
	if _, err := st.Read(nil); err != nil {
		st.Destroy()
		return nil, err
	}
	// Proven: the frames that follow are no longer limited to maxNoiseMessage.
	guard.proven = true
	if err := conn.SetDeadline(time.Time{}); err != nil {
		st.Destroy()
		return nil, err
	}
	return st, nil
}

// readFrame reads one frame: a 3-byte little-endian length, then the payload. A frame longer than
// limit is refused on its prefix. The payload's memory grows only as its bytes arrive, so a peer
// that names a long frame and sends little costs about what it sent.
func readFrame(r io.Reader, limit int) ([]byte, error) {
	var hdr [3]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := int(hdr[0]) | int(hdr[1])<<8 | int(hdr[2])<<16
	if n > limit {
		return nil, errFrameTooLarge
	}
	payload, err := io.ReadAll(io.LimitReader(r, int64(n)))
	if err != nil {
		return nil, err
	}
	if len(payload) != n {
		return nil, io.ErrUnexpectedEOF
	}
	return payload, nil
}

// frameGuard is the connection the handshake reads through until the peer is proven. It follows the
// length prefixes of the frames as they pass, and refuses a frame whose prefix names more than limit
// bytes, before any of its payload is read, so a peer cannot make the listener buffer a frame it may not
// send. A refusal returns no bytes and repeats; the connection is closed after it. Once proven is set,
// bytes pass through unexamined. Reads come from one goroutine, the handshake's.
type frameGuard struct {
	net.Conn
	limit  int
	proven bool
	hdr    []byte // length-prefix bytes of the current frame seen so far
	left   int    // payload bytes of the current frame still to pass
	err    error  // the refusal, once given
}

// Read reads from the connection, and checks the frames the bytes belong to.
func (g *frameGuard) Read(p []byte) (int, error) {
	if g.err != nil {
		return 0, g.err
	}
	n, err := g.Conn.Read(p)
	if g.proven {
		return n, err
	}
	for i := 0; i < n; {
		if g.left > 0 {
			take := min(g.left, n-i)
			g.left -= take
			i += take
			continue
		}
		g.hdr = append(g.hdr, p[i])
		i++
		if len(g.hdr) < 3 {
			continue
		}
		size := int(g.hdr[0]) | int(g.hdr[1])<<8 | int(g.hdr[2])<<16
		g.hdr = g.hdr[:0]
		if size > g.limit {
			g.err = errFrameTooLarge
			return 0, g.err
		}
		g.left = size
	}
	return n, err
}

// CloseWrite ends the outgoing side of the connection, when it can, so the stream keeps the half-close
// it has on TCP. Otherwise it closes the connection whole.
func (g *frameGuard) CloseWrite() error {
	if cw, ok := g.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return g.Conn.Close()
}

// writeFrame writes payload behind its 3-byte little-endian length, in one Write, as secretstream
// frames it.
func writeFrame(w io.Writer, payload []byte) error {
	n := len(payload)
	if n > 1<<24-1 {
		return errFrameTooLarge
	}
	buf := append([]byte{byte(n), byte(n >> 8), byte(n >> 16)}, payload...)
	_, err := w.Write(buf)
	return err
}

// sourceIP is the address part of the connection's remote address. It is the key of the per-IP cap.
func sourceIP(conn net.Conn) string {
	addr := conn.RemoteAddr().String()
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}
