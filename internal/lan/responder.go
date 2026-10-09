package lan

import (
	"context"
	"errors"
	"net"
	"time"
)

const (
	// seenFor is how long a seen nonce stays in the replay cache, on the monotonic clock. A probe is
	// fresh for 24 h either side of its timestamp, so a probe first seen at T may still be fresh at
	// T+48 h; an entry is therefore dropped only once it is more than 48 h old.
	seenFor = 48 * time.Hour
	// maxSeen caps the replay cache. The oldest nonce goes first.
	maxSeen = 100000
)

// Responder answers valid probes on a UDP socket with the host's LAN TCP port. It is silent to every
// other packet. Its replay cache of seen nonces is in memory.
type Responder struct {
	lanKey  [32]byte
	tcpPort uint16
	conn    net.PacketConn
	now     func() time.Time // the host's wall clock, for the probe's freshness
	mono    func() time.Time // the clock the replay cache expires by: time.Now, or a test's clock

	seen  map[[16]byte]struct{}
	order []seenNonce // the same nonces, oldest first
}

// seenNonce is a nonce in the replay cache and when it was seen.
type seenNonce struct {
	nonce [16]byte
	at    time.Time
}

// NewResponder returns a responder for lanKey that reports tcpPort in its replies. It reads probes
// from conn and judges their age against now.
func NewResponder(lanKey [32]byte, tcpPort uint16, conn net.PacketConn, now func() time.Time) *Responder {
	return &Responder{
		lanKey:  lanKey,
		tcpPort: tcpPort,
		conn:    conn,
		now:     now,
		mono:    time.Now,
		seen:    make(map[[16]byte]struct{}),
	}
}

// Serve answers probes until ctx is done, and then returns nil. When ctx is done it sets conn's read
// deadline to now, so the caller closes conn or resets the deadline before it reuses the socket. It
// returns the read error when conn is closed.
func (r *Responder) Serve(ctx context.Context) error {
	stop := context.AfterFunc(ctx, func() { r.conn.SetReadDeadline(time.Now()) })
	defer stop()
	buf := make([]byte, 2048) // a probe is 256 bytes; a longer datagram fails the size check
	for {
		n, from, err := r.conn.ReadFrom(buf)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			continue // a transient error on one packet; the next one may be fine
		}
		nonce, ok := VerifyProbe(r.lanKey, buf[:n], r.now())
		if !ok || !r.remember(nonce) {
			continue
		}
		reply := EncodeReply(r.lanKey, nonce, r.tcpPort)
		r.conn.WriteTo(reply[:], from) // UDP: a failed write is a lost reply, as on any UDP path
	}
}

// remember records nonce as seen and reports whether it was new. Entries older than seenFor go first,
// then the oldest entry if the cache is still full.
func (r *Responder) remember(nonce [16]byte) bool {
	now := r.mono()
	for len(r.order) > 0 && now.Sub(r.order[0].at) > seenFor {
		r.forget()
	}
	if _, dup := r.seen[nonce]; dup {
		return false
	}
	if len(r.order) >= maxSeen {
		r.forget()
	}
	r.seen[nonce] = struct{}{}
	r.order = append(r.order, seenNonce{nonce: nonce, at: now})
	return true
}

// forget drops the oldest nonce from the replay cache.
func (r *Responder) forget() {
	delete(r.seen, r.order[0].nonce)
	r.order = r.order[1:]
}
