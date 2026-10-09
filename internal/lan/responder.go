package lan

import (
	"context"
	"errors"
	"net"
	"time"
)

// Responder answers valid probes on a UDP socket with the host's LAN TCP port. It is silent to every
// other packet. Its replay cache of seen nonces is in memory.
type Responder struct{}

// NewResponder returns a responder for lanKey that reports tcpPort in its replies. It reads probes
// from conn and judges their age against now.
func NewResponder(lanKey [32]byte, tcpPort uint16, conn net.PacketConn, now func() time.Time) *Responder {
	panic("not implemented")
}

// Serve answers probes until ctx is done, and then returns nil.
func (r *Responder) Serve(ctx context.Context) error {
	return errors.ErrUnsupported
}
