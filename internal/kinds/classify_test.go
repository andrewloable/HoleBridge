package kinds

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
)

// classifyCase is one row of TestClassify.
type classifyCase struct {
	name string
	err  error
	want result
}

// TestClassify maps each probe error to its result. A reset is failed on every platform: Unix reports
// syscall.ECONNRESET, and Windows reports syscall.WSAECONNRESET, which errors.Is does not match to
// ECONNRESET. The platform reset rows come from platformResets, which is set per GOOS.
func TestClassify(t *testing.T) {
	cases := []classifyCase{
		{"timeout is silence", &net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}, silent},
		{"EOF is failed", io.EOF, failed},
		{"unexpected EOF is failed", io.ErrUnexpectedEOF, failed},
		{"closed connection is failed", net.ErrClosed, failed},
		{"wrapped ECONNRESET is failed", fmt.Errorf("read: %w", syscall.ECONNRESET), failed},
		{"other error is non-HTTP", errors.New("garbage"), gotNonHTTP},
	}
	for _, reset := range platformResets {
		cases = append(cases, classifyCase{"wrapped platform reset is failed", fmt.Errorf("read: %w", reset), failed})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(tc.err); got != tc.want {
				t.Fatalf("classify(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
