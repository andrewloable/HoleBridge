package mux

import (
	"bytes"
	"io"
	"testing"
	"time"
)

// Edge case: the app reconnects before the host sees the old transport die. The old host session is still
// attached and holds the stream, so the reattach takes the stream from that live session. Both directions
// stay byte-exact across the takeover.
func TestReattachTakesOverALiveStream(t *testing.T) {
	tab := newTable(t, resumeCfg(), nil)
	app1, host1, up1, _ := resumeConn(t, tab, acceptEverything)
	st, err := openStream(app1, "web")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	target := hostTarget(t, host1, st.ID())
	want := pattern(3 * mib)
	written := make(chan error, 1)
	go func() {
		_, err := st.Write(want)
		written <- err
	}()
	head := make([]byte, mib)
	if err := doWithin(t, "the host's read before the takeover", func() error {
		_, err := io.ReadFull(target, head)
		return err
	}); err != nil {
		t.Fatalf("read at the host before the takeover: %v", err)
	}

	// The app's transport is gone, but the host session is not told: it stays attached.
	up1.cutOff()
	detach(t, app1)
	app2, _, _, _ := resumeConn(t, tab, acceptEverything)
	adopt(t, app2, app1)

	tail := make([]byte, len(want)-len(head))
	if err := doWithin(t, "the host's read after the takeover", func() error {
		_, err := io.ReadFull(target, tail)
		return err
	}); err != nil {
		t.Fatalf("read at the host after the takeover: %v", err)
	}
	select {
	case err := <-written:
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
	case <-time.After(resumeWait):
		t.Fatal("the app's Write did not finish after the takeover")
	}
	if !bytes.Equal(append(head, tail...), want) {
		t.Fatal("the bytes arrived altered across the takeover")
	}
}

// Edge case: the app closes its write side while its reattach is in flight. The close waits for the
// reattached, so the host reads the bytes sent before the drop and then io.EOF, and nothing is lost.
func TestCloseMadeDuringReattachReachesThePeerAfterIt(t *testing.T) {
	tab := newTable(t, resumeCfg(), nil)
	app1, host1, up1, down1 := resumeConn(t, tab, acceptEverything)
	st, err := openStream(app1, "web")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	target := hostTarget(t, host1, st.ID())
	if _, err := st.Write([]byte("before")); err != nil {
		t.Fatalf("Write before the drop: %v", err)
	}
	dropConn(t, app1, host1, up1, down1)

	app2, _, _, _ := resumeConn(t, tab, acceptEverything)
	adopt(t, app2, app1)
	if err := returnsWithin(t, "CloseWrite during the reattach", st.CloseWrite); err != nil {
		t.Fatalf("CloseWrite during the reattach: %v", err)
	}

	var got []byte
	if err := doWithin(t, "the host's read to EOF", func() error {
		var err error
		got, err = io.ReadAll(target)
		return err
	}); err != nil {
		t.Fatalf("read at the host to EOF: %v", err)
	}
	if string(got) != "before" {
		t.Fatalf("the host read %q, want the bytes sent before the drop", got)
	}
}
