// The client side of HyperDHT connect, ported from hyperdht 6.34.1 lib/connect.js (findAndConnect,
// connectThroughNode, and the direct reply path of onsocket) and the connect of index.js, MIT License,
// Copyright (c) 2018-2019 Mathias Buus, David Mark Clements & Contributors.
//
// A connect walks to the hash of the key with FIND_PEER. Each node that holds a record of the key gets the
// client's handshake, which the node relays to the server. The first reply that verifies makes the
// connection: the client's UDX stream is connected to the server's stream, and the secret stream runs its
// header exchange over it. The direct path claims the stream at once. A connect that has a relay (relay.go)
// also dials that relay and pairs on it; the paired relay claims the stream when the direct path has not,
// as upstream's relayConnection does. A reply that came through another node is relayed, as upstream sees it: its
// first verified reply starts the punch of punch_flow.go when the server names relays, and the punch claims the
// stream; a relayed reply that cannot be punched claims the server address, as upstream's fallback does. A direct
// reply claims the stream at once. The test seam dht.forcePunch punches every connect, whatever the reply. The LAN
// ping of upstream connectThroughNode is not ported.
package hyperdht

import (
	"context"
	"errors"
	"math"
	"net"
	"sync"

	"golang.org/x/crypto/blake2b"

	"github.com/andrewloable/HoleBridge/pears/dhtrpc"
	"github.com/andrewloable/HoleBridge/pears/noise"
	"github.com/andrewloable/HoleBridge/pears/secretstream"
	"github.com/andrewloable/HoleBridge/pears/udx"
)

// ErrPeerNotFound is the error of a connect whose lookup finds no record of the key: upstream's
// PEER_NOT_FOUND. It is returned when no node answers with a record for the key.
var ErrPeerNotFound = errors.New("hyperdht: peer not found")

// ErrPeerConnectionFailed is the error of a connect that found the key but got no usable answer to its
// handshake. A key that the server's firewall refuses ends here too, since a refused handshake gets no
// reply: upstream's PEER_CONNECTION_FAILED.
var ErrPeerConnectionFailed = errors.New("hyperdht: could not connect to peer")

// ConnectOptions sets up a connect. KeyPair is the key pair the client proves, or nil for the DHT's own key
// pair. RelayThrough returns the public key of the relay to connect through, or nil to connect without one.
// It is asked once per connect, with force false, since no punch has failed yet.
type ConnectOptions struct {
	KeyPair      *noise.KeyPair
	RelayThrough func(force bool) *[32]byte
}

// Connect dials the server whose public key is remotePublicKey and returns the connection once the secret
// stream is up. It fails with ErrPeerNotFound when the lookup finds no record of the key, and with
// ErrPeerConnectionFailed when the handshake gets no usable answer, as for a refused key. ctx bounds the
// whole dial.
func (d *DHT) Connect(ctx context.Context, remotePublicKey [32]byte, opts ConnectOptions) (*Conn, error) {
	c, _, err := d.connect(ctx, remotePublicKey, opts, d.forceRelay)
	return c, err
}

// connect is Connect. forced says whether only a relay may claim the stream (dht.forceRelay). It also returns
// the address the connection's stream is connected to: the server's, or the relay's when a relay claimed it.
func (d *DHT) connect(ctx context.Context, pk [32]byte, opts ConnectOptions, forced bool) (*Conn, *net.UDPAddr, error) {
	if d.isClosed() {
		return nil, nil, ErrDHTClosed
	}
	kp := d.keyPair
	if opts.KeyPair != nil {
		kp = *opts.KeyPair
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // ends the walk, any handshake still waiting, and a relay pairing still waiting, once the dial returns
	offer, err := newRelayOffer(opts.RelayThrough, false)
	if err != nil {
		return nil, nil, err
	}
	return d.dial(ctx, d.newStream(), pk, kp, offer, forced)
}

// attempt is one connect: the client's UDX stream and handshake, and what the replies have started. The direct
// path and a paired relay both claim the stream through claim, and the first to take it wins.
type attempt struct {
	d       *DHT
	ctx     context.Context
	pk      [32]byte
	st      *udx.Stream
	hs      *noise.Handshake
	offer   *relayOffer // the relay this connect offers, or nil
	forced  bool        // only a relay may claim the stream
	punched bool        // the punch seam (dht.forcePunch): only the hole punch may claim the stream
	claim   streamClaim
	started bool // the first verified reply has been acted on; later replies are ignored
	// producers counts the relay path and the punch that have not reported their outcome yet. The first success
	// ends the dial; an error ends it only when no other producer is still pending (upstream keeps waiting for the
	// relay while a punch fails). Only the dial goroutine changes it.
	producers int
	relayed   chan claimResult // the outcomes of the relay path and the punch, buffered for both
	once      sync.Once        // sets the punch's outcome in relayed once (finish)
	punch     *holepuncher     // the puncher of a punched connect, stopped when the dial returns
}

// dial sends the client's handshake to each node that holds a record of pk, and returns the connection that the
// first reply that verifies makes, with the address its stream connected to. st is the client's UDX stream, which
// the handshake names. offer is the relay the handshake offers, or nil. forced says whether only a relay may claim
// st. When the dial fails, st is given up, and a relay pairing still running cannot claim it.
func (d *DHT) dial(ctx context.Context, st *udx.Stream, pk [32]byte, kp noise.KeyPair, offer *relayOffer, forced bool) (conn *Conn, addr *net.UDPAddr, err error) {
	hs := noise.NewInitiator(kp, pk, nsPeerHandshake[:])
	payload, err := EncodeNoisePayload(NoisePayload{
		UDX:          &UDXInfo{Version: 1, ID: uint64(st.ID())},
		SecretStream: &SecretStreamInfo{Version: 1},
		RelayThrough: offer.info(),
	})
	if err != nil {
		st.Destroy()
		return nil, nil, err
	}
	msg1, err := hs.Send(payload)
	if err != nil {
		st.Destroy()
		return nil, nil, err
	}
	value, err := EncodeHandshake(Handshake{Mode: handshakeFromClient, Noise: msg1})
	if err != nil {
		st.Destroy()
		return nil, nil, err
	}
	a := &attempt{d: d, ctx: ctx, pk: pk, st: st, hs: hs, offer: offer, forced: forced, punched: d.forcePunch, relayed: make(chan claimResult, 2)}
	defer func() {
		if a.punch != nil {
			a.punch.destroy() // a punch still running ends with the dial
		}
		if err != nil {
			a.claim.take() // a relay pairing still running must not claim a stream that is given up
			st.Destroy()
		}
	}()
	target := keyTarget(pk)

	type answer struct {
		addr *net.UDPAddr
		resp *dhtrpc.Response
		err  error
	}
	answers := make(chan answer)
	holders := d.holders(ctx, target)
	found, waiting := 0, 0
	for holders != nil || waiting > 0 || a.producers > 0 {
		select {
		case h, ok := <-holders:
			if !ok {
				holders = nil
				continue
			}
			found++
			waiting++
			go func() {
				resp, err := d.node.Request(ctx, h, dhtrpc.Request{Command: cmdPeerHandshake, Target: target[:], Value: value})
				select {
				case answers <- answer{h, resp, err}:
				case <-ctx.Done():
				}
			}()
		case an := <-answers:
			waiting--
			c, from, rerr := a.takeReply(ctx, an.addr, an.resp, an.err)
			if rerr != nil {
				return nil, nil, rerr
			}
			if c != nil {
				return c, from, nil
			}
		case r := <-a.relayed:
			a.producers--
			if r.err == nil {
				return r.conn, r.addr, nil
			}
			if a.producers == 0 {
				return nil, nil, r.err // no other path is still pending, so this error ends the dial
			}
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
	if cerr := ctx.Err(); cerr != nil {
		return nil, nil, cerr // the walk ended because ctx did, so the outcome is not known
	}
	if found == 0 {
		return nil, nil, ErrPeerNotFound
	}
	return nil, nil, ErrPeerConnectionFailed
}

// holders walks to target with FIND_PEER, and sends the address of each node that answers with a record for
// it, as upstream's findPeer does for a connect. The channel closes when the walk ends or ctx ends.
func (d *DHT) holders(ctx context.Context, target [32]byte) <-chan *net.UDPAddr {
	q := d.node.Query(ctx, dhtrpc.QueryOpts{Target: target[:], Command: cmdFindPeer})
	out := make(chan *net.UDPAddr)
	go func() {
		defer close(out)
		for {
			r, ok := q.Next()
			if !ok {
				return
			}
			if _, err := DecodePeer(r.Response.Value); err != nil {
				continue
			}
			select {
			case out <- r.From:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// takeReply takes one answer to the client's handshake request, sent to the node at addr. It returns the
// connection when the answer verifies and the direct path claims the stream, nil when the answer does not count,
// and an error when the connection fails after the handshake verified. An answer that does not verify leaves the
// handshake as it was, since the check runs on a copy, so the next answer can still verify. The first verified
// answer starts the relay path, if there is a relay on either side, and then the punch or the direct claim, as
// the connect's package comment says. An answer that comes from another address than addr does not count, as
// upstream's peerHandshake rejects it (BAD_HANDSHAKE_REPLY).
func (a *attempt) takeReply(ctx context.Context, addr *net.UDPAddr, resp *dhtrpc.Response, err error) (*Conn, *net.UDPAddr, error) {
	if err != nil || resp.Error != 0 {
		return nil, nil, nil
	}
	if resp.From == nil || resp.From.Port != addr.Port || !resp.From.IP.Equal(addr.IP) {
		return nil, nil, nil
	}
	h, err := DecodeHandshake(resp.Value)
	if err != nil || h.Mode != handshakeReply || len(h.Noise) == 0 {
		return nil, nil, nil
	}
	done := *a.hs
	body, err := done.Recv(h.Noise)
	if err != nil {
		return nil, nil, nil
	}
	p, err := DecodeNoisePayload(body)
	if err != nil || p.Version != 1 || p.Error != 0 || p.UDX == nil || p.UDX.ID > math.MaxUint32 {
		return nil, nil, nil
	}
	if a.started {
		return nil, nil, nil // only the first verified reply acts, as upstream ignores replies once the connect has its path
	}
	a.started = true
	keys := keysOf(&done)
	serverAddr := addr // the server's address as the relay saw it, else the node that answered
	if h.PeerAddress != nil {
		serverAddr = udpAddrOf(*h.PeerAddress)
	}
	_, _, hash, _ := done.Result()
	if a.punched {
		// The punch seam: the hole punch claims the stream, and neither the direct path nor a relay does.
		a.startPunch(punchReply{payload: p, serverAddr: addressOf(serverAddr), relayAddr: addressOf(addr), keys: keys, secret: punchSecret(hash)})
		return nil, nil, nil
	}
	// The server's reply names its relay as the responder's, else this side's offer makes it the initiator.
	switch {
	case p.RelayThrough != nil:
		a.startRelay(offerOf(p.RelayThrough), false, keys)
	case a.offer != nil:
		a.startRelay(*a.offer, true, keys)
	}
	if a.forced {
		return nil, nil, nil // only the relay path may claim the stream
	}
	// A reply that came through another node is relayed: the server's address is not the node asked (upstream's
	// relayed). Upstream does not claim a relayed reply at the server address. It holepunches when the server names
	// relays to punch through, and claims the server address only when it cannot (relayed and not holepunchable).
	relayed := !serverAddr.IP.Equal(addr.IP) || serverAddr.Port != addr.Port
	if relayed && holepunchable(p) {
		a.startPunch(punchReply{payload: p, serverAddr: addressOf(serverAddr), relayAddr: addressOf(addr), keys: keys, secret: punchSecret(hash)})
		return nil, nil, nil
	}
	if !a.claim.take() {
		return nil, nil, nil // the relay path claimed the stream first
	}
	if err := a.st.Connect(uint32(p.UDX.ID), serverAddr); err != nil {
		return nil, nil, err
	}
	c := secretstream.Resume(a.st, true, secretstream.Options{RemotePublicKey: &a.pk}, keys)
	if err := c.Handshake(ctx); err != nil {
		return nil, nil, err
	}
	return c, serverAddr, nil
}

// holepunchable reports whether the server's reply names relays to punch through (upstream's remoteHolepunchable).
func holepunchable(p NoisePayload) bool {
	return p.Holepunch != nil && len(p.Holepunch.Relays) > 0
}

// startRelay starts the relay path of the attempt: the stream pairs on the relay r and is claimed through the
// pairing, then the secret stream's header exchange runs over it. The outcome goes to a.relayed, once. The path
// does not end with the dial: as upstream's relayConnection, it dials the relay whether or not the direct path
// claims the stream first, and it gives the pairing up once it has lost the claim.
func (a *attempt) startRelay(r relayOffer, initiator bool, keys secretstream.Keys) {
	a.producers++
	go func() {
		rs, addr, err := a.d.pairRelay(context.Background(), &a.claim, a.st, r, initiator)
		if err != nil {
			a.relayed <- claimResult{err: err}
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), headerExchangeWait)
		defer cancel()
		c := secretstream.Resume(rs, true, secretstream.Options{RemotePublicKey: &a.pk}, keys)
		if err := c.Handshake(ctx); err != nil {
			a.relayed <- claimResult{err: err}
			return
		}
		a.relayed <- claimResult{conn: c, addr: addr}
	}()
}

// keyTarget returns the target under which a key's record and route are stored: the BLAKE2b-256 of its public
// key, as upstream's unslabbedHash gives it.
func keyTarget(pk [32]byte) [32]byte {
	return blake2b.Sum256(pk[:])
}

// keysOf returns the outcome of a completed Noise handshake h, for the secret stream that runs over it.
func keysOf(h *noise.Handshake) secretstream.Keys {
	tx, rx, hash, peer := h.Result()
	return secretstream.Keys{Tx: tx, Rx: rx, Hash: hash, Peer: peer}
}
