//go:build !windows

package kinds

// isWindowsReset is always false off Windows, where a reset is syscall.ECONNRESET alone.
func isWindowsReset(err error) bool {
	return false
}
