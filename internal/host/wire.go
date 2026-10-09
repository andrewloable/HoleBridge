package host

import (
	"context"
	"net"

	"github.com/andrewloable/HoleBridge/pears/secretstream"
)

// ServeConn serves a secret stream that the LAN listener has handshaken: the same holebridge channel, the
// same services handshake, and the same streams and UDP flows as a DHT session (docs/architecture.md, LAN
// route). The stream must prove the client key pair, as the DHT firewall requires; any other key is closed
// without a handshake. ServeConn returns when the stream ends or ctx is done.
func (h *Host) ServeConn(ctx context.Context, s *secretstream.Stream) {
	if h.refuse(h.clientKey(), s.RemotePublicKey()) || !h.trackAdmitted(s) {
		s.Destroy()
		return
	}
	stop := context.AfterFunc(ctx, func() { s.Destroy() })
	defer stop()
	h.serve(s, true, false)
}

// LANAddresses returns the host's LAN addresses (see lanAddresses), for the command's banner.
func LANAddresses() []string {
	return lanAddresses()
}

// lanAddresses returns the host's IPv4 addresses on its interfaces, leaving out loopback and link-local
// ones: the addresses a LAN app can reach the host at. The handshake's LAN block carries them.
func lanAddresses() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.To4() == nil || ipn.IP.IsLoopback() || ipn.IP.IsLinkLocalUnicast() {
			continue
		}
		out = append(out, ipn.IP.String())
	}
	return out
}
