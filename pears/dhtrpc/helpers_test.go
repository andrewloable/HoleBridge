package dhtrpc

import (
	"testing"
	"time"
)

// punchWait bounds the wait for a holepunch datagram or a reply in these tests. On loopback a datagram arrives in microseconds.
const punchWait = 5 * time.Second

// stubCall runs f and fails the test with "not implemented" when f panics, which is how the stubs of punch.go
// report a method that has no body yet.
func stubCall(t *testing.T, what string, f func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s: not implemented", what)
		}
	}()
	f()
}
