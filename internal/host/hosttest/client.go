// Package hosttest is a minimal app-role client for the host tests. It dials a host over a HyperDHT node
// with a client key pair, opens the holebridge channel with a version 1 handshake, and opens streams on
// the host's services. It is test support only: the app's own engine is in app/engine.
package hosttest

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/andrewloable/HoleBridge/internal/mux"
	"github.com/andrewloable/HoleBridge/internal/protocol"
	"github.com/andrewloable/HoleBridge/pears/hyperdht"
	"github.com/andrewloable/HoleBridge/pears/noise"
	"github.com/andrewloable/HoleBridge/pears/protomux"
	"github.com/andrewloable/HoleBridge/pears/secretstream"
)

const (
	protocolName = "holebridge"
	messageCount = 11 // message indexes 0 to 10

	dialWait      = 30 * time.Second // the whole dial, including the host's lookup
	handshakeWait = 10 * time.Second // the host's handshake, after the channel opens
	openWait      = 30 * time.Second // a stream's answer from the host

	// datagramQueue is how many received datagrams wait for Datagram. A datagram that finds the queue full
	// is dropped, as UDP drops it. servicesQueue is how many services lists wait for NextServices; a full
	// queue holds the channel's reads until one is taken, since a list is not dropped.
	datagramQueue = 256
	servicesQueue = 16

	// window and budget are the app's limits (docs/architecture.md, Limits): 2 MiB per stream, 64 MiB in all.
	window = 2 << 20
	budget = 64 << 20
)

// Client is one connected app-role session to a host. Its streams outlive a dropped connection: Drop ends the
// connection, and Reconnect runs a new one whose session takes the streams over.
type Client struct {
	dht      *hyperdht.DHT // the node Connect dialled, for Reconnect; nil for Attach
	hostPub  [32]byte
	clientKP ed25519.PrivateKey
	sess     *mux.Session // the app session; it keeps its streams when its connection drops
	cur      *connection  // the connection the session runs on now
}

// connection is one connection of a client: its secret stream, its holebridge channel and the channel's
// messages. It is the mux.Sender of the session that runs on it, so a stream that moves to a new session
// sends on the new connection.
type connection struct {
	stream *secretstream.Stream
	sess   *mux.Session
	ch     *protomux.Channel
	msgs   []*protomux.Message // one per message index, in index order
	hs     protocol.Handshake
	dgrams chan protocol.Datagram  // the datagrams the host sent on the channel, message 10
	servs  chan []protocol.Service // the services lists the host pushed, message 8, for NextServices
	closed chan struct{}           // closed when the channel closes
	once   sync.Once
}

// Connect dials the host whose public key is hostPub over dht, proving clientKP, and returns the client
// once the host's handshake has arrived. It fails when the dial fails (a refused key wraps
// hyperdht.ErrPeerConnectionFailed), and when the channel closes or no handshake arrives within 10 s.
func Connect(dht *hyperdht.DHT, hostPub [32]byte, clientKP ed25519.PrivateKey) (*Client, error) {
	conn, err := dial(dht, hostPub, clientKP)
	if err != nil {
		return nil, fmt.Errorf("hosttest: connect: %w", err)
	}
	sess, k, err := open(conn)
	if err != nil {
		return nil, err
	}
	return &Client{dht: dht, hostPub: hostPub, clientKP: clientKP, sess: sess, cur: k}, nil
}

// Attach runs the app-role session over conn, a secret stream that is already handshaken: a LAN stream in
// the tests, which has no DHT dial. It returns the client once the host's handshake has arrived, and waits
// for it as Connect does.
func Attach(conn *secretstream.Stream) (*Client, error) {
	sess, k, err := open(conn)
	if err != nil {
		return nil, err
	}
	return &Client{sess: sess, cur: k}, nil
}

// dial connects to the host over dht with the client key pair, and returns the secret stream.
func dial(dht *hyperdht.DHT, hostPub [32]byte, clientKP ed25519.PrivateKey) (*secretstream.Stream, error) {
	var kp noise.KeyPair
	copy(kp.Public[:], clientKP.Public().(ed25519.PublicKey))
	copy(kp.Secret[:], clientKP)
	ctx, cancel := context.WithTimeout(context.Background(), dialWait)
	defer cancel()
	return dht.Connect(ctx, hostPub, hyperdht.ConnectOptions{KeyPair: &kp})
}

// open runs a new app session over conn: it opens the holebridge channel with a version 1 handshake and
// returns the session, resumable, and its connection once the host's handshake has arrived. It closes conn
// when that fails.
func open(conn *secretstream.Stream) (*mux.Session, *connection, error) {
	k := &connection{
		stream: conn,
		closed: make(chan struct{}),
		dgrams: make(chan protocol.Datagram, datagramQueue),
		servs:  make(chan []protocol.Service, servicesQueue),
	}
	k.sess = mux.NewSession(mux.RoleApp, k, mux.Config{Window: window}, mux.NewBudget(budget), nil)
	if err := k.sess.EnableResume(); err != nil {
		conn.Close()
		return nil, nil, err
	}
	go k.readMessages()
	got := make(chan protocol.Handshake, 1)
	m := protomux.New(conn)
	k.ch = m.CreateChannel(protomux.ChannelOptions{
		Protocol: protocolName,
		OnOpen: func(raw []byte) {
			hs, err := protocol.DecodeHandshake(raw)
			if err != nil {
				conn.Close()
				return
			}
			got <- hs
		},
		OnClose: func(bool) {
			k.sess.Detach()
			k.markClosed()
		},
	})
	if k.ch == nil {
		conn.Close()
		return nil, nil, errors.New("hosttest: the connection closed before the channel opened")
	}
	for i := 0; i < messageCount; i++ {
		k.msgs = append(k.msgs, k.ch.AddMessage(k.receive(i)))
	}
	hs := protocol.EncodeHandshake(protocol.Handshake{Version: 1, Flags: protocol.FlagResume | protocol.FlagDatagrams})
	if err := k.ch.Open(hs); err != nil {
		k.close()
		return nil, nil, err
	}

	timer := time.NewTimer(handshakeWait)
	defer timer.Stop()
	select {
	case k.hs = <-got:
		return k.sess, k, nil
	case <-k.closed:
		k.close()
		return nil, nil, errors.New("hosttest: the channel closed before the host's handshake")
	case <-timer.C:
		k.close()
		return nil, nil, errors.New("hosttest: no host handshake within 10 s")
	}
}

// close ends the connection's channel and its secret stream.
func (k *connection) close() {
	k.ch.Close()
	k.stream.Close()
}

// readMessages queues the datagrams the host sends as unordered messages, the DHT route's replies, with the
// datagrams of message 10. It ends when the stream's message channel closes.
func (k *connection) readMessages() {
	for raw := range k.stream.Messages() {
		if d, err := protocol.DecodeUnordered(raw); err == nil {
			k.queue(d)
		}
	}
}

// queue hands a received datagram to Datagram. A full queue drops it, as UDP does.
func (k *connection) queue(d protocol.Datagram) {
	select {
	case k.dgrams <- d:
	default: // the queue is full: UDP drops it
	}
}

// queueServices hands a services list to NextServices. It waits while the queue is full, unless the channel
// closes first: a list is not dropped the way a datagram is.
func (k *connection) queueServices(list []protocol.Service) {
	select {
	case k.servs <- list:
	case <-k.closed:
	}
}

// receive returns the handler of message index: it decodes the payload and hands the message to the session.
// A message the session cannot take ends the connection.
func (k *connection) receive(index int) func([]byte) {
	return func(payload []byte) {
		msg, err := protocol.Decode(index, payload)
		if d, ok := msg.(protocol.Datagram); ok && err == nil {
			k.queue(d)
			return
		}
		if s, ok := msg.(protocol.Services); ok && err == nil {
			k.queueServices(s.Services)
			return
		}
		if err == nil {
			err = k.sess.Receive(msg)
		}
		if err != nil {
			k.stream.Close()
		}
	}
}

// markClosed closes k.closed once.
func (k *connection) markClosed() {
	k.once.Do(func() { close(k.closed) })
}

// Send carries one protocol message on the connection's channel, under its message index. It implements
// mux.Sender.
func (k *connection) Send(msg any) error {
	payload, err := protocol.Encode(msg)
	if err != nil {
		return err
	}
	return k.msgs[protocol.Index(msg)].Send(payload)
}

// Send carries one protocol message on the channel of the current connection, under its message index.
func (c *Client) Send(msg any) error {
	return c.cur.Send(msg)
}

// Handshake returns the handshake the host sent when the channel opened.
func (c *Client) Handshake() protocol.Handshake {
	return c.cur.hs
}

// Open asks the host for a stream to service and returns it once the host has answered opened. A host
// refusal is a *mux.RejectError. Open waits at most 30 s for the answer.
func (c *Client) Open(service string) (*mux.Stream, error) {
	ctx, cancel := context.WithTimeout(context.Background(), openWait)
	defer cancel()
	return c.sess.Open(ctx, service)
}

// SendDatagram sends one datagram to the host as an unordered message, which is how the app sends its
// datagrams on the DHT route. It fails on a LAN connection, which has no unordered messages.
func (c *Client) SendDatagram(d protocol.Datagram) error {
	return c.cur.stream.Send(protocol.EncodeUnordered(d))
}

// Datagram waits for the next datagram the host sends and returns it. On the LAN route it arrives on the
// channel as message 10; on the DHT route it arrives as an unordered message. It fails when ctx ends first,
// or when the channel closes.
func (c *Client) Datagram(ctx context.Context) (protocol.Datagram, error) {
	select {
	case d := <-c.cur.dgrams:
		return d, nil
	case <-ctx.Done():
		return protocol.Datagram{}, ctx.Err()
	case <-c.cur.closed:
		return protocol.Datagram{}, errors.New("hosttest: the channel closed before a datagram arrived")
	}
}

// NextServices waits for the next services list the host pushes (message 8) and returns its services. It fails
// when ctx ends first, or when the channel closes before a list arrives.
func (c *Client) NextServices(ctx context.Context) ([]protocol.Service, error) {
	select {
	case list := <-c.cur.servs:
		return list, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.cur.closed:
		return nil, errors.New("hosttest: the channel closed before a services list arrived")
	}
}

// WaitClosed waits until the host closes the client's channel, which ends the session the app sees, and
// returns nil. It returns ctx's error when ctx ends first.
func (c *Client) WaitClosed(ctx context.Context) error {
	select {
	case <-c.cur.closed:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close ends the session and its channel.
func (c *Client) Close() error {
	c.sess.Close()
	c.cur.ch.Close()
	return c.cur.stream.Close()
}
