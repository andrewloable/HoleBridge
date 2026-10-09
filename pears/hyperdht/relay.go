// The relay-through policy and the relay claim of HyperDHT connect and server, ported from hyperdht 6.34.1
// lib/connect.js and lib/server.js (selectRelay, relayConnection and _relayConnection), MIT License, Copyright
// (c) 2018-2019 Mathias Buus, David Mark Clements & Contributors.
//
// A relay connection is dialed with the DHT's default key pair (Config.DefaultKeyPair), not with the key pair of
// the connect or the server, so the relay's firewall decides on that key. The pairing on the relay claims the
// stream when the direct path has not: the stream connects to the relay at the id the pairing gave, and the relay
// forwards its packets. The relay connection stays open while the stream is in use, since the relay forwards only
// while its pairing is open.
package hyperdht

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andrewloable/HoleBridge/pears/blindrelay"
	"github.com/andrewloable/HoleBridge/pears/protomux"
	"github.com/andrewloable/HoleBridge/pears/udx"
)

// relayConnectWait bounds a relay dial and its pairing. It is upstream's relayConnection timeout (15 s), and
// the blind-relay pairing gives up at the same time.
const relayConnectWait = 15 * time.Second

// errClaimLost is the error of a relay pairing whose stream the direct path claimed first.
var errClaimLost = errors.New("hyperdht: stream already claimed")

// relayOffer is a relay a connection can go through: the public key the relay listens under, and the token
// that pairs the two sides on it.
type relayOffer struct {
	key   [32]byte
	token [32]byte
}

// streamClaim is the one claim of a connection's UDX stream. The direct path and a paired relay both take it,
// and the first to take it claims the stream.
type streamClaim struct {
	taken atomic.Bool
}

// take claims the stream, and reports whether this call did.
func (c *streamClaim) take() bool {
	return c.taken.CompareAndSwap(false, true)
}

// claimResult is the outcome of a relay path: the connection it made and the address its stream connected to,
// or the error that ended it.
type claimResult struct {
	conn *Conn
	addr *net.UDPAddr
	err  error
}

// relayedStream is a UDX stream claimed through a relay. It owns the relay connection that carries the pairing,
// and closes it once neither side of the stream can still use the relay: the stream is closed, or it failed, or
// both our writes and the peer's reads have ended.
type relayedStream struct {
	*udx.Stream
	relay     io.Closer
	wrote     atomic.Bool // CloseWrite has run
	readEnded atomic.Bool // a read has failed or reached the end of the peer's writes
	once      sync.Once
}

// release closes the relay connection, once.
func (r *relayedStream) release() {
	r.once.Do(func() { r.relay.Close() })
}

// Read reads from the stream. An error other than the end of the peer's writes ends the stream, so the relay goes
// at once; the end of the peer's writes keeps the relay until our writes end too.
func (r *relayedStream) Read(p []byte) (int, error) {
	n, err := r.Stream.Read(p)
	if err != nil {
		r.readEnded.Store(true)
		if r.wrote.Load() || !errors.Is(err, io.EOF) {
			r.release()
		}
	}
	return n, err
}

// CloseWrite ends our writes. The relay goes once the peer's writes have ended as well.
func (r *relayedStream) CloseWrite() error {
	err := r.Stream.CloseWrite()
	r.wrote.Store(true)
	if r.readEnded.Load() {
		r.release()
	}
	return err
}

// Close closes the stream and the relay connection.
func (r *relayedStream) Close() error {
	r.release()
	return r.Stream.Close()
}

// newRelayOffer asks the policy once, with force, and returns the offer for the relay it names, with a new
// random token. It returns nil when there is no policy or the policy names no relay.
func newRelayOffer(policy func(force bool) *[32]byte, force bool) (*relayOffer, error) {
	pk := selectRelay(policy, force)
	if pk == nil {
		return nil, nil
	}
	o := &relayOffer{key: *pk}
	if _, err := rand.Read(o.token[:]); err != nil {
		return nil, err
	}
	return o, nil
}

// info returns the relay-through part of a handshake payload for o, or nil when o is nil.
func (o *relayOffer) info() *RelayThroughInfo {
	if o == nil {
		return nil
	}
	return &RelayThroughInfo{Version: 1, PublicKey: o.key[:], Token: o.token[:]}
}

// offerOf returns the relay that the relay-through part of a handshake payload names.
func offerOf(t *RelayThroughInfo) relayOffer {
	var o relayOffer
	copy(o.key[:], t.PublicKey)
	copy(o.token[:], t.Token)
	return o
}

// selectRelay returns the public key of the relay that a connection goes through, or nil for a direct
// connection. It asks the caller's RelayThrough policy once, with force as given. A nil policy and a nil
// answer both mean no relay.
func selectRelay(policy func(force bool) *[32]byte, force bool) *[32]byte {
	if policy == nil {
		return nil
	}
	return policy(force)
}

// pairRelay dials the relay r.key with the DHT's default key pair, and pairs the stream st on it with initiator and
// r.token. The relay admits the dial or refuses it by that key. Once the pairing lands and no other path has claimed
// st, st connects to the relay at the id the pairing gave, which claims it; the returned stream owns the relay
// connection. It returns errClaimLost when the direct path claimed st first, with the pairing given up. The relay
// dial is direct (never forced), since it is not a stream under test. The blind-relay channel opens under the
// default key pair's public key, the id upstream gives it, so the relay pairs it with the same connection. ctx ends
// the pairing, and relayConnectWait bounds it. The address returned is the relay's, where st connected.
//
// The claimed stream sends one unordered message before anything else. The relay forwards a packet only to a side
// whose address it has seen in a packet from that side, and the responder of a secret stream waits for the
// initiator's header before it writes, so without this neither side would reach the other through the relay.
func (d *DHT) pairRelay(ctx context.Context, cl *streamClaim, st *udx.Stream, r relayOffer, initiator bool) (io.ReadWriteCloser, *net.UDPAddr, error) {
	ctx, cancel := context.WithTimeout(ctx, relayConnectWait)
	defer cancel()
	conn, addr, err := d.connect(ctx, r.key, ConnectOptions{}, false)
	if err != nil {
		return nil, nil, err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() }) // a pairing that waits ends when ctx ends
	id := d.keyPair.Public
	p, err := blindrelay.NewClient(protomux.New(conn), id[:]).Pair(initiator, r.token, st)
	if !stop() || err != nil {
		conn.Close()
		if err == nil {
			err = ctx.Err()
		}
		return nil, nil, err
	}
	if !cl.take() {
		conn.Close()
		return nil, nil, errClaimLost
	}
	if err := st.Connect(p.RemoteID, addr); err != nil {
		conn.Close()
		return nil, nil, err
	}
	_ = st.SendMessage(nil) // the relay learns this side's address from it; a lost message is harmless
	return &relayedStream{Stream: st, relay: conn}, addr, nil
}
