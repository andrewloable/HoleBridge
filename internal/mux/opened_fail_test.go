package mux

import (
	"errors"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/internal/protocol"
)

// brokenSender sends every message but fails the ones of one type, as a transport that dies at opened does.
type brokenSender struct {
	failType any
	err      error
}

func (b *brokenSender) Send(msg any) error {
	if _, ok := msg.(protocol.Opened); ok && b.failType != nil {
		return b.err
	}
	return nil
}

// A host session without resume cannot send opened: its transport is gone and the error goes to the caller. The
// accept has already dialed the target, so the stream's target must still run: it closes the target once the
// stream fails. Without it the dialed connection is never closed.
func TestOpenedSendFailureStillRunsTarget(t *testing.T) {
	sendErr := errors.New("transport gone")
	ran := make(chan *Stream, 1)
	accept := func(string) AcceptResult {
		return AcceptResult{Target: func(st *Stream) { ran <- st }}
	}
	host := NewSession(RoleHost, &brokenSender{failType: protocol.Opened{}, err: sendErr}, Config{Window: 1 << 20},
		NewBudget(1<<30), accept)

	err := host.Receive(protocol.Open{Stream: 1, Service: "x", Window: 1 << 16})
	if !errors.Is(err, sendErr) {
		t.Fatalf("Receive = %v, want the send error", err)
	}
	select {
	case st := <-ran:
		if st.ID() != 1 {
			t.Fatalf("the target ran for stream %d, want 1", st.ID())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the target never ran after opened failed: the dialed target would stay open")
	}
}
