//go:build unix

package dhtrpc

import (
	"net"
	"syscall"
)

// sockControl runs f with the file descriptor of conn's socket.
func sockControl(conn *net.UDPConn, f func(fd int) error) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var ferr error
	if err := raw.Control(func(fd uintptr) { ferr = f(int(fd)) }); err != nil {
		return err
	}
	return ferr
}

// ipTTL returns the IP TTL that the socket of conn sends with.
func ipTTL(conn *net.UDPConn) (int, error) {
	var ttl int
	err := sockControl(conn, func(fd int) error {
		v, err := syscall.GetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_TTL)
		ttl = v
		return err
	})
	return ttl, err
}

// setIPTTL sets the IP TTL that the socket of conn sends with.
func setIPTTL(conn *net.UDPConn, ttl int) error {
	return sockControl(conn, func(fd int) error {
		return syscall.SetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_TTL, ttl)
	})
}
