package host

import (
	"bytes"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/internal/config"
	"github.com/andrewloable/HoleBridge/internal/protocol"
)

// An app whose handshake leaves FlagResume off gets a zero token in opened, so its streams are not resumable
// (docs/architecture.md, Encoding: the token is zeros when resume is off). Its streams still carry bytes.
func TestAppWithoutResumeGetsZeroToken(t *testing.T) {
	r := newRig(t)
	r.startHost(t, r.hostConfig(t, echoServices(t), nil), nil)
	a := r.rawConnect(t, protocol.FlagDatagrams)

	if err := a.write(protocol.Open{Stream: 1, Service: "echo", Window: 1 << 20}); err != nil {
		t.Fatalf("write open: %v", err)
	}
	if op := a.awaitOpened(); op.Token != ([16]byte{}) {
		t.Fatal("opened carries a resume token although the app did not set FlagResume")
	}
	if err := a.write(protocol.Data{Stream: 1, Payload: []byte("hello")}); err != nil {
		t.Fatalf("write data: %v", err)
	}
	for got := []byte{}; len(got) < len("hello"); {
		msg := a.await("the echo")
		if d, ok := msg.(protocol.Data); ok {
			got = append(got, d.Payload...)
		}
	}
}

// An app whose handshake leaves FlagDatagrams off gets the replies of its UDP flows as datagram messages on the
// channel (message 10), not as unordered messages (docs/architecture.md, UDP services: a route or app that cannot
// take unordered datagrams falls back to message 10).
func TestAppWithoutDatagramsGetsRepliesOnChannel(t *testing.T) {
	r := newRig(t)
	target, _ := udpEcho(t)
	cfg := r.hostConfig(t, map[string]config.Service{
		"dns": {Target: target, Kind: "udp"},
	}, nil)
	r.runWire(t, r.newWireHost(t, cfg, nil))
	a := r.rawConnect(t, protocol.FlagResume)

	if err := a.write(protocol.Flow{Flow: 1, Service: "dns", Payload: []byte("first")}); err != nil {
		t.Fatalf("write flow: %v", err)
	}
	msg := a.await("the flow's reply")
	d, ok := msg.(protocol.Datagram)
	if !ok || d.Flow != 1 || !bytes.Equal(d.Payload, []byte("first")) {
		t.Fatalf("the reply is %#v, want flow 1 with the first payload as a datagram message", msg)
	}
	time.Sleep(200 * time.Millisecond)
	select {
	case u := <-a.unordered:
		t.Fatalf("the reply came as an unordered message (flow %d), want message 10 on the channel", u.Flow)
	default:
	}
}

// The LAN route's ordered queue is one queue per session, 256 KiB in the default limits (docs/architecture.md,
// Limits), shared by all of the session's udp services. The app reads nothing while replies come in, so they
// queue. With a queue of 2500 bytes the session takes three 1000-byte replies: the one being sent, and two that
// fit. Per service, the queue would take three more.
func TestLANOrderedQueueSharedAcrossUDPServices(t *testing.T) {
	r := newRig(t)
	targetA, _ := udpEcho(t)
	targetB, _ := udpEcho(t)
	cfg := r.hostConfig(t, map[string]config.Service{
		"a": {Target: targetA, Kind: "udp"},
		"b": {Target: targetB, Kind: "udp"},
	}, map[string]any{"orderedDatagramQueue": 2500})
	h := r.newWireHost(t, cfg, nil)
	hostSide, appSide := r.lanPair(t, kpOf(r.clientKey))
	serveConn(t, h, hostSide)

	a := rawOver(t, appSide, protocol.FlagResume|protocol.FlagDatagrams, func(tr *appTransport) { tr.stall() })
	payload := bytes.Repeat([]byte{'x'}, 1000)
	for flow := uint64(1); flow <= 6; flow++ {
		service := "a"
		if flow%2 == 0 {
			service = "b"
		}
		if err := a.write(protocol.Flow{Flow: flow, Service: service, Payload: payload}); err != nil {
			t.Fatalf("write flow %d: %v", flow, err)
		}
	}
	time.Sleep(300 * time.Millisecond) // the replies reach the host's queue while the app reads nothing
	a.tr.release()

	delivered := 0
	timeout := time.After(time.Second)
	for drained := false; !drained; {
		select {
		case msg := <-a.got:
			if _, ok := msg.(protocol.Datagram); ok {
				delivered++
			}
		case <-timeout:
			drained = true
		}
	}
	if delivered != 3 {
		t.Fatalf("the app received %d replies, want 3: one being sent and two that fit the session's 2500-byte queue", delivered)
	}
}
