package lan

import (
	"errors"
	"log/slog"
	"net"
	"time"

	"github.com/andrewloable/HoleBridge/pears/noise"
	"github.com/andrewloable/HoleBridge/pears/secretstream"
)

// ListenerConfig holds the limits of the LAN TCP listener (docs/architecture.md, Limits).
type ListenerConfig struct {
	HandshakeDeadline time.Duration // a connection that has not proven its client key by then is closed
	MaxUnauth         int           // unauthenticated connections in total
	MaxUnauthPerIP    int           // unauthenticated connections from one source IP
	Log               *slog.Logger  // refusals are logged at most once a minute

	now func() time.Time // the clock of the log rate limit; nil means time.Now (tests set it)
}

// Listener accepts LAN TCP connections, runs the Noise handshake on each under the host key, and
// returns the streams whose remote static key the admit function accepts.
type Listener struct{}

// NewListener serves the LAN TCP port ln under hostKey. admit decides which remote static keys
// get a stream, and cfg sets the limits.
func NewListener(ln net.Listener, hostKey noise.KeyPair, admit func(remote [32]byte) bool, cfg ListenerConfig) *Listener {
	panic("not implemented")
}

// Accept returns the next stream from an admitted client.
func (l *Listener) Accept() (*secretstream.Stream, error) {
	return nil, errors.ErrUnsupported
}

// Rejected returns how many connections the caps refused at accept.
func (l *Listener) Rejected() uint64 {
	panic("not implemented")
}
