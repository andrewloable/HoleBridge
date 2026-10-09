//go:build windows

package kinds

import "syscall"

// platformResets are the reset errors that only Windows reports: WSAECONNRESET (10054).
var platformResets = []error{syscall.WSAECONNRESET}
