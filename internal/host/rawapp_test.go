package host

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/internal/protocol"
	"github.com/andrewloable/HoleBridge/pears/compact"
	"github.com/andrewloable/HoleBridge/pears/hyperdht"
	"github.com/andrewloable/HoleBridge/pears/protomux"
	"github.com/andrewloable/HoleBridge/pears/secretstream"
)

// appTransport is the app side's connection as its protomux sees it. Its Close does nothing, as a client that
// ignores the host's END keeps its transport open. Its reads can be held back, which stalls the host's writes.
type appTransport struct {
	conn *secretstream.Stream
	mu   sync.Mutex
	hold chan struct{} // nil: reads pass; otherwise reads wait until it is closed
}

func (a *appTransport) Read(p []byte) (int, error) {
	a.mu.Lock()
	hold := a.hold
	a.mu.Unlock()
	if hold != nil {
		<-hold
	}
	return a.conn.Read(p)
}

func (a *appTransport) Write(p []byte) (int, error) { return a.conn.Write(p) }

func (a *appTransport) Close() error { return nil }

// stall holds back the reads of a until release. Call it from a handler of the channel, so that no read is
// in progress when it takes effect.
func (a *appTransport) stall() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.hold = make(chan struct{})
}

// release lets the held reads through.
func (a *appTransport) release() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.hold != nil {
		close(a.hold)
		a.hold = nil
	}
}

// rawApp is an app-role client that the test drives by hand, with the handshake flags it is given. Its channel
// is opened through protomux, as an app's is. Its messages are written as frames, so it can keep writing after
// the host has ended the session.
type rawApp struct {
	t         *testing.T
	conn      *secretstream.Stream
	tr        *appTransport
	hs        chan protocol.Handshake
	got       chan any               // the channel messages the host sends, decoded, in arrival order
	unordered chan protocol.Datagram // the unordered messages the host sends: the DHT route's datagrams
}

// rawConnect dials the host over DHT with the rig's client key pair and opens the holebridge channel with a
// version 1 handshake of flags. It returns once the host's handshake has arrived. The host starts announcing as
// Run starts, so a dial that finds nothing yet is retried for readWait, as connect does.
func (r *rig) rawConnect(t *testing.T, flags uint64) *rawApp {
	t.Helper()
	kp := kpOf(r.clientKey)
	deadline := time.Now().Add(readWait)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), readWait)
		conn, err := r.tn.Nodes[1].Connect(ctx, r.hostPub, hyperdht.ConnectOptions{KeyPair: &kp})
		cancel()
		if err == nil {
			return rawOver(t, conn, flags, nil)
		}
		if time.Now().After(deadline) {
			t.Fatalf("Connect: %v", err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// rawOver runs the app side of the holebridge channel over conn, a handshaken secret stream, and returns once the
// host's handshake has arrived. onHandshake, if set, runs in the channel's handshake handler, before the next read.
func rawOver(t *testing.T, conn *secretstream.Stream, flags uint64, onHandshake func(*appTransport)) *rawApp {
	t.Helper()
	a := &rawApp{t: t, conn: conn, tr: &appTransport{conn: conn},
		hs: make(chan protocol.Handshake, 1), got: make(chan any, 256), unordered: make(chan protocol.Datagram, 256)}
	t.Cleanup(func() { conn.Close() })
	m := protomux.New(a.tr)
	ch := m.CreateChannel(protomux.ChannelOptions{
		Protocol: protocolName,
		OnOpen: func(raw []byte) {
			if hs, err := protocol.DecodeHandshake(raw); err == nil {
				select {
				case a.hs <- hs:
				default:
				}
			}
			if onHandshake != nil {
				onHandshake(a.tr)
			}
		},
	})
	if ch == nil {
		t.Fatal("CreateChannel returned nil")
	}
	for i := 0; i < messageCount; i++ {
		index := i
		ch.AddMessage(func(p []byte) {
			if msg, err := protocol.Decode(index, p); err == nil {
				select {
				case a.got <- msg:
				default:
				}
			}
		})
	}
	go func() {
		for raw := range conn.Messages() {
			if d, err := protocol.DecodeUnordered(raw); err == nil {
				select {
				case a.unordered <- d:
				default:
				}
			}
		}
	}()
	if err := ch.Open(protocol.EncodeHandshake(protocol.Handshake{Version: 1, Flags: flags})); err != nil {
		t.Fatalf("Open: %v", err)
	}
	select {
	case <-a.hs:
	case <-time.After(readWait):
		t.Fatalf("no host handshake within %v", readWait)
	}
	return a
}

// write sends msg on the holebridge channel as the channel's protomux would: the local id of the channel, which
// is 1 for the first channel of a connection, then the message index, then the payload. It writes to the
// connection itself, so it still works after the host has ended the channel's session.
func (a *rawApp) write(msg any) error {
	payload, err := protocol.Encode(msg)
	if err != nil {
		return err
	}
	var e compact.Encoder
	e.Uint(1)
	e.Uint(uint64(protocol.Index(msg)))
	e.Raw(payload)
	_, err = a.conn.Write(e.Bytes())
	return err
}

// await returns the next channel message the host sends, and fails the test if none arrives within readWait.
func (a *rawApp) await(what string) any {
	a.t.Helper()
	select {
	case msg := <-a.got:
		return msg
	case <-time.After(readWait):
		a.t.Fatalf("no %s from the host within %v", what, readWait)
		return nil
	}
}

// awaitOpened returns the next opened message the host sends, skipping the other messages.
func (a *rawApp) awaitOpened() protocol.Opened {
	a.t.Helper()
	deadline := time.Now().Add(readWait)
	for {
		select {
		case msg := <-a.got:
			if op, ok := msg.(protocol.Opened); ok {
				return op
			}
		case <-time.After(time.Until(deadline)):
			a.t.Fatalf("no opened from the host within %v", readWait)
		}
	}
}
