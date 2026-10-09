package host

import (
	"bytes"
	"testing"
)

// Case 8: a client whose connection drops reconnects with the same key, and its stream resumes. The echo that
// follows the reconnect comes back on the stream that was open before the drop. The helpers rig, echoServices,
// roundTrip and failIfStub are in host_test.go. It needs Drop and Reconnect on hosttest.Client, which are stubs.
func TestClientReconnectResumesItsStream(t *testing.T) {
	r := newRig(t)
	r.startHost(t, r.hostConfig(t, echoServices(t), nil), nil)
	c := r.connect(t, r.clientKey)

	st, err := c.Open("echo")
	failIfStub(t, err)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	before := roundTrip(t, st, []byte("before the drop"))
	if !bytes.Equal(before, []byte("before the drop")) {
		t.Fatalf("echo before the drop = %q", before)
	}

	if err := c.Drop(); err != nil {
		failIfStub(t, err)
		t.Fatalf("Drop: %v", err)
	}
	if err := c.Reconnect(); err != nil {
		failIfStub(t, err)
		t.Fatalf("Reconnect: %v", err)
	}

	after := roundTrip(t, st, []byte("after the reconnect"))
	if !bytes.Equal(after, []byte("after the reconnect")) {
		t.Fatalf("echo after the reconnect = %q, want the bytes sent after the drop", after)
	}
}
