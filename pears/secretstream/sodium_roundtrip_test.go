package secretstream

import (
	"bytes"
	"testing"
)

// NewPush draws a random header, which NewPull must accept. The vectors never exercise that path.
func TestRoundTripRandomHeader(t *testing.T) {
	key := [32]byte{1, 2, 3}
	push, header, err := NewPush(key)
	if err != nil {
		t.Fatalf("NewPush: %v", err)
	}
	pull := NewPull(key, header)
	for i, tag := range []byte{TagMessage, TagPush, TagRekey, TagFinal} {
		msg := bytes.Repeat([]byte{byte(i)}, 100*i)
		got, gotTag, err := pull.Open(push.Seal(msg, nil, tag), nil)
		if err != nil {
			t.Fatalf("message %d: Open: %v", i, err)
		}
		if !bytes.Equal(got, msg) || gotTag != tag {
			t.Errorf("message %d: round trip changed the message or tag", i)
		}
	}
}

// A ciphertext shorter than ABytes cannot authenticate, so Open must fail rather than panic.
func TestPullRejectsShortCiphertext(t *testing.T) {
	var key [32]byte
	var header [24]byte
	if _, _, err := NewPull(key, header).Open(make([]byte, ABytes-1), nil); err == nil {
		t.Error("Open accepted a ciphertext shorter than ABytes")
	}
}
