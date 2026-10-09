//go:build windows

package kinds

import (
	"errors"
	"syscall"
)

// isWindowsReset reports whether err is a connection reset as Windows reports it, WSAECONNRESET (10054).
// Windows has its own errno for a reset, and errors.Is does not match it to syscall.ECONNRESET.
func isWindowsReset(err error) bool {
	return errors.Is(err, syscall.WSAECONNRESET)
}
