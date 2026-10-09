//go:build !unix

package dhtrpc

import (
	"errors"
	"net"
)

// ipTTL and setIPTTL report errors.ErrUnsupported where the socket TTL is not set per socket here, so hole
// punching does not run from this node on such a platform.
func ipTTL(*net.UDPConn) (int, error) {
	return 0, errors.ErrUnsupported
}

func setIPTTL(*net.UDPConn, int) error {
	return errors.ErrUnsupported
}
