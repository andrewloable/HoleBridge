// The client side of the hole punch of a connect, ported from hyperdht 6.34.1 lib/connect.js (holepunch, probeRound,
// roundPunch, updateHolepunch, abort and pickServerRelay), MIT License, Copyright (c) 2018-2019 Mathias Buus, David
// Mark Clements & Contributors.
//
// A punch takes the first verified handshake reply of a connect that came through another node (a relayed reply),
// or of any connect under dht.forcePunch, and punches toward the server. The probe round asks the server, through
// its relay, for its NAT state and punches toward it, the round punch asks the server to punch, and the puncher
// connects on a datagram from the server, which claims the stream. A punch on a birthday socket moves the stream onto
// that socket. Not ported: the LAN ping of connectThroughNode, the reopen of an unstable socket, and the DHT's limit
// on randomized punches.
package hyperdht

import (
	"bytes"
	"context"
	"errors"
	"io"
	rand "math/rand/v2"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/andrewloable/HoleBridge/pears/dhtrpc"
	"github.com/andrewloable/HoleBridge/pears/secretstream"
	"github.com/andrewloable/HoleBridge/pears/udx"
)

// Errors of a hole punch: upstream's errors of lib/errors.js that a punched connect can end with.
var (
	errCannotHolepunch        = errors.New("hyperdht: remote is not holepunchable")
	errHolepunchInvalid       = errors.New("hyperdht: holepunch reply does not verify")
	errRemoteAborted          = errors.New("hyperdht: remote aborted the holepunch")
	errProbeTimeout           = errors.New("hyperdht: holepunch probe timed out")
	errRemoteNotHolepunching  = errors.New("hyperdht: remote is not holepunching")
	errRemoteNotHolepunchable = errors.New("hyperdht: remote is not holepunchable")
	errHolepunchAborted       = errors.New("hyperdht: holepunch aborted")
	errPunchDone              = errors.New("hyperdht: holepunch ended")
	errTryLater               = errors.New("hyperdht: the remote asks the punch to wait")
	probeRoundPause           = time.Second // the pause before a probe round that the server has not answered for
)

// tryLaterBase and tryLaterSpread set the pause of a punch that met TRY_LATER: upstream's tryLater waits
// 10 s plus up to 10 s at random. Tests shorten them.
var (
	tryLaterBase   = 10 * time.Second
	tryLaterSpread = 10 * time.Second
)

// punchReply is the verified handshake reply that a punched connect punches from: the server's payload, the server's
// address and the node that answered, the keys of the handshake and its holepunch secret.
type punchReply struct {
	payload    NoisePayload
	serverAddr Address
	relayAddr  Address
	keys       secretstream.Keys
	secret     [32]byte
}

// connectPunch is the state of one punched connect: its puncher, the coder of its messages, and the round counter
// (upstream c.puncher, c.payload and c.round).
type connectPunch struct {
	a      *attempt
	p      *holepuncher
	pool   punchPool
	sp     *securePayload
	id     uint64   // the server's holepunch id
	target [32]byte // the target of the server's key, which PEER_HOLEPUNCH is routed to
	round  uint64
}

// startPunch starts the hole punch of the attempt for the verified reply r, in the background. Its outcome goes to
// a.relayed once: the connection that the punch claims, or the error that ended the punch.
func (a *attempt) startPunch(r punchReply) {
	a.producers++
	cp := &connectPunch{
		a:      a,
		pool:   a.d.punchPool(),
		sp:     newSecurePayload(r.secret),
		target: keyTarget(a.pk),
	}
	if hp := r.payload.Holepunch; hp != nil {
		cp.id = hp.ID
	}
	cp.p = newHolepuncher(punchConfig{
		Pool:           cp.pool,
		Initiator:      true,
		RemoteFirewall: r.payload.Firewall,
		Gate:           a.d.gate(),
		OnConnect:      func(sock punchSocket, from *net.UDPAddr) { a.claimPunched(cp, r, sock, from) },
		OnAbort:        func() { a.finish(claimResult{err: errHolepunchAborted}) },
	})
	a.punch = cp.p
	go func() {
		if err := cp.run(a.ctx, r); err != nil && !cp.p.connected() {
			if !errors.Is(err, errPunchDone) {
				a.finish(claimResult{err: err})
			}
			cp.p.destroy()
		}
	}()
}

// finish sets the outcome of the attempt's punch, once.
func (a *attempt) finish(r claimResult) {
	a.once.Do(func() { a.relayed <- r })
}

// claimPunched claims the stream of the attempt when the puncher connects, and runs the secret stream over it. The
// stream connects to the address the server's punch came from, on the socket the punch connected on: a birthday
// socket takes the stream over, and the node's socket keeps it. The puncher runs on the read loop, so the handshake
// goes to its own goroutine.
func (a *attempt) claimPunched(cp *connectPunch, r punchReply, sock punchSocket, from *net.UDPAddr) {
	own := birthdayOf(sock)
	if !a.claim.take() {
		// The relay path or the direct path claimed the stream first. The punch still reports, so the dial does not
		// wait for an outcome that will not come.
		cp.pool.Release(sock)
		a.finish(claimResult{err: errClaimLost})
		return
	}
	if own == nil {
		cp.pool.Release(sock) // the stream runs on the node's socket, so the node's handle is not needed
	}
	go func() {
		c, err := a.streamTo(r, from, own)
		a.finish(claimResult{conn: c, addr: from, err: err})
	}()
}

// streamTo connects the attempt's UDX stream to the server's stream at from, on the birthday socket own or else on the
// node's socket, and runs the secret stream's header exchange over it. A stream that fails gives its socket up.
func (a *attempt) streamTo(r punchReply, from *net.UDPAddr, own *dhtPunchSocket) (*Conn, error) {
	conn, err := punchedConn(a.st, own, uint32(r.payload.UDX.ID), from)
	if err != nil {
		return nil, err
	}
	c := secretstream.Resume(conn, true, secretstream.Options{RemotePublicKey: &a.pk}, r.keys)
	if err := c.Handshake(a.ctx); err != nil {
		conn.Close()
		return nil, err
	}
	return c, nil
}

// birthdayOf returns the birthday socket that a punch connected on, or nil for the node's own socket.
func birthdayOf(sock punchSocket) *dhtPunchSocket {
	if h, ok := sock.(*dhtPunchSocket); ok && h.conn != nil {
		return h
	}
	return nil
}

// punchedConn connects the UDX stream st, made on the node's socket, to the peer's stream remoteID at from. On the
// node's socket (own nil) the stream stays where it is. On a birthday socket own, the stream moves onto that socket
// (rehomeStream), the socket is kept so that the puncher's Release leaves it open, and the connection closes it with
// itself. The caller has taken the stream's claim.
func punchedConn(st *udx.Stream, own *dhtPunchSocket, remoteID uint32, from *net.UDPAddr) (io.ReadWriteCloser, error) {
	if own == nil {
		if err := st.Connect(remoteID, from); err != nil {
			return nil, err
		}
		return st, nil
	}
	own.keep()
	moved, err := rehomeStream(st, own, remoteID, from)
	if err != nil {
		own.closeSocket()
		return nil, err
	}
	return &punchedStream{Stream: moved, sock: own}, nil
}

// punchedStream is a UDX stream on a birthday socket of its own. Closing it closes the socket with it.
type punchedStream struct {
	*udx.Stream
	sock *dhtPunchSocket
	once sync.Once
}

// Close closes the stream, then its birthday socket, once.
func (s *punchedStream) Close() error {
	err := s.Stream.Close()
	s.once.Do(s.sock.closeSocket)
	return err
}

// rehomeStream moves the stream old, made on the node's socket, onto the birthday socket keep: the stream keeps its id,
// and connects to the peer's stream remoteID at addr. The old stream is destroyed first, which unregisters its id on
// the node's socket. The stream is not connected yet, so nothing is sent.
func rehomeStream(old *udx.Stream, keep *dhtPunchSocket, remoteID uint32, addr *net.UDPAddr) (*udx.Stream, error) {
	id := old.ID()
	old.Destroy()
	st := keep.ud.NewStream(id)
	if err := st.Connect(remoteID, addr); err != nil {
		st.Destroy()
		return nil, err
	}
	return st, nil
}

// run is the punch of the attempt: the NAT samples of its puncher, the probe round, the move to the address the
// server reports when the same relay answered, and the round punch. It returns nil once the punch has started, which
// connects the attempt or aborts it later, and the error that ends the punch before that.
func (cp *connectPunch) run(ctx context.Context, r punchReply) error {
	hp := r.payload.Holepunch
	if hp == nil || len(hp.Relays) == 0 {
		return errCannotHolepunch
	}
	if _, err := cp.a.d.sampleNATFromPings(ctx, cp.p, cp.a.d.observers()); err != nil {
		return err
	}
	if cp.done() {
		return nil
	}
	serverRelay := pickServerRelay(hp.Relays, r.relayAddr)
	serverAddr := r.serverAddr
	token, peer, err := cp.probe(ctx, &serverAddr, serverRelay, true)
	if cp.done() {
		return nil
	}
	if err != nil {
		return err
	}
	if serverRelay.RelayAddress == r.relayAddr && peer != serverAddr {
		serverAddr = peer
		cp.p.openSession(serverAddr)
	}
	if cp.done() {
		return nil
	}
	return cp.punch(ctx, serverAddr, token, r.relayAddr)
}

// probe runs the probe round (upstream probeRound). It opens a session toward the address the server was reached at,
// asks the server through the relay for its NAT state, and opens another session when the server names an address of
// its own. It checks that the two NAT states allow a punch. It returns the server's token for this side, and the
// address the relay gives the server.
func (cp *connectPunch) probe(ctx context.Context, serverAddr *Address, relay RelayInfo, retry bool) ([]byte, Address, error) {
	if serverAddr != nil {
		cp.p.openSession(*serverAddr)
	}
	if cp.done() {
		return nil, Address{}, errPunchDone
	}
	pl := cp.payload(false, serverAddr, nil, nil)
	reply, hp, resp, err := cp.send(ctx, relay.RelayAddress, relay.PeerAddress, pl)
	if err != nil {
		return nil, Address{}, err
	}
	cp.p.observe(Address{Host: resp.To.Host, Port: resp.To.Port}, addressOf(resp.From))
	if remote, _, _ := cp.p.firewalls(); remote < firewallRandom && reply.RemoteAddress != nil && reply.RemoteAddress.Port != 0 && reply.RemoteAddress.Host.IsValid() && (serverAddr == nil || *reply.RemoteAddress != *serverAddr) {
		cp.p.openSession(*reply.RemoteAddress)
		if cp.done() {
			return nil, Address{}, errPunchDone
		}
	}
	if remote, _, _ := cp.p.firewalls(); remote == firewallUnknown {
		// The server has not said yet whether it is firewalled. Give its fast punch a moment to arrive first.
		if err := sleepContext(ctx, probeRoundPause); err != nil {
			return nil, Address{}, err
		}
		if cp.done() {
			return nil, Address{}, errPunchDone
		}
	}
	remote, local, _ := cp.p.firewalls()
	if (remote == firewallUnknown || len(reply.Token) == 0) && retry {
		return cp.probe(ctx, serverAddr, relay, false)
	}
	if remote == firewallUnknown || local == firewallUnknown {
		return nil, Address{}, cp.abort(ctx, relay, errProbeTimeout)
	}
	if remote >= firewallRandom && local >= firewallRandom {
		return nil, Address{}, cp.abort(ctx, relay, ErrHolepunchDoubleRandomized)
	}
	peer := relay.PeerAddress
	if hp.PeerAddress != nil {
		peer = *hp.PeerAddress
	}
	return reply.Token, peer, nil
}

// punch is the round punch (upstream roundPunch). It tells the server that this side punches, with the token it gave
// for the server's address, and starts the punch once the server answers that it punches too.
//
// The round punch waits while the server answers TRY_LATER, or while this side's own randomized punches are at their
// limit: upstream's roundPunch pauses (tryLater) and sends the round again.
func (cp *connectPunch) punch(ctx context.Context, serverAddr Address, remoteToken []byte, clientRelay Address) error {
	own := cp.sp.token(serverAddr)
	for {
		_, _, _, err := cp.send(ctx, clientRelay, serverAddr, cp.payload(true, nil, own[:], remoteToken))
		if errors.Is(err, errTryLater) {
			if err := cp.waitTryLater(ctx); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		break
	}
	if cp.done() {
		return errPunchDone
	}
	if _, _, punching := cp.p.firewalls(); !punching {
		return errRemoteNotHolepunching
	}
	ok, err := cp.p.punch()
	if errors.Is(err, errPunchGated) {
		if err := cp.waitTryLater(ctx); err != nil {
			return err
		}
		return cp.punch(ctx, serverAddr, remoteToken, clientRelay)
	}
	if err != nil {
		return err
	}
	if !ok {
		return errRemoteNotHolepunchable
	}
	return nil
}

// waitTryLater pauses a punch that was told to wait (upstream tryLater: tryLaterBase plus up to tryLaterSpread). It
// returns the error of ctx when ctx ends first, and errPunchDone when the attempt's punch is over.
func (cp *connectPunch) waitTryLater(ctx context.Context) error {
	d := tryLaterBase
	if tryLaterSpread > 0 {
		d += rand.N(tryLaterSpread)
	}
	if err := sleepContext(ctx, d); err != nil {
		return err
	}
	if cp.done() {
		return errPunchDone
	}
	return nil
}

// abort tells the server that the punch is off, so its puncher for the handshake stops, and ends the attempt's punch
// with err (upstream abort). It returns errPunchDone, since the outcome is set here.
func (cp *connectPunch) abort(ctx context.Context, relay RelayInfo, err error) error {
	if cp.p.connected() {
		return errPunchDone
	}
	pl := cp.payload(false, nil, nil, nil)
	pl.Error = handshakeAborted
	pl.Firewall = firewallUnknown
	pl.Addresses = nil
	_, _, _, _ = cp.send(ctx, relay.RelayAddress, relay.PeerAddress, pl) // best effort, as upstream's abort
	cp.a.finish(claimResult{err: err})
	cp.p.destroy()
	return errPunchDone
}

// done reports whether the attempt's punch is over: the puncher has connected or ended, or the attempt has.
func (cp *connectPunch) done() bool {
	return cp.p.connected() || cp.p.destroyed() || cp.a.ctx.Err() != nil
}

// payload returns this side's message payload: its NAT state and addresses, with the given fields.
func (cp *connectPunch) payload(punching bool, remoteAddr *Address, token, remoteToken []byte) HolepunchPayload {
	return HolepunchPayload{
		Firewall:      cp.p.natFirewall(),
		Addresses:     cp.p.natAddresses(),
		Punching:      punching,
		RemoteAddress: remoteAddr,
		Token:         token,
		RemoteToken:   remoteToken,
	}
}

// send sends one holepunch message to the server through the relay at dest, addressed to the server at peer, with the
// payload pl encrypted under the holepunch secret, and returns the server's decrypted answer, the holepunch part of
// the reply and the reply itself. An answer that does not verify, or that carries an error, is an error. A verified
// answer updates the puncher with the server's firewall, addresses and punching state (upstream updateHolepunch), and
// it echoes this side's token when the server returns it for peer.
func (cp *connectPunch) send(ctx context.Context, dest, peer Address, pl HolepunchPayload) (HolepunchPayload, Holepunch, *dhtrpc.Response, error) {
	pl.Round = cp.round
	cp.round++
	enc, err := cp.sp.encrypt(pl)
	if err != nil {
		return HolepunchPayload{}, Holepunch{}, nil, err
	}
	msg, err := EncodeHolepunch(Holepunch{Mode: holepunchFromClient, ID: cp.id, Payload: enc, PeerAddress: &peer})
	if err != nil {
		return HolepunchPayload{}, Holepunch{}, nil, err
	}
	resp, err := cp.a.d.node.Request(ctx, udpAddrOf(dest), dhtrpc.Request{Command: cmdPeerHolepunch, Target: cp.target[:], Value: msg})
	if err != nil {
		return HolepunchPayload{}, Holepunch{}, nil, err
	}
	if resp.Error != 0 {
		return HolepunchPayload{}, Holepunch{}, nil, errHolepunchInvalid
	}
	hp, err := DecodeHolepunch(resp.Value)
	if err != nil || hp.Mode != holepunchReply {
		return HolepunchPayload{}, Holepunch{}, nil, errHolepunchInvalid
	}
	reply, ok := cp.sp.decrypt(hp.Payload)
	if !ok {
		return HolepunchPayload{}, Holepunch{}, nil, errHolepunchInvalid
	}
	if reply.Error == holepunchTryLater && pl.Punching && cp.a.offer != nil {
		// The server's randomized punches are at their limit. A connect with a relay waits and asks again (upstream
		// updateHolepunch returns tryLater before it updates the puncher).
		return HolepunchPayload{}, Holepunch{}, nil, errTryLater
	}
	if reply.Error != 0 {
		return HolepunchPayload{}, Holepunch{}, nil, errRemoteAborted
	}
	var echo netip.Addr
	if len(reply.RemoteToken) > 0 && bytes.Equal(reply.RemoteToken, pl.Token) {
		echo = peer.Host
	}
	cp.p.updateRemote(reply.Firewall, reply.Punching, reply.Addresses, echo)
	return reply, hp, resp, nil
}

// pickServerRelay returns the relay of the server's record that the connect's answering node is, so the probe goes
// through the same relay; otherwise the server's first relay (upstream pickServerRelay).
func pickServerRelay(relays []RelayInfo, clientRelay Address) RelayInfo {
	for _, r := range relays {
		if r.RelayAddress == clientRelay {
			return r
		}
	}
	return relays[0]
}

// sleepContext waits for d, or until ctx ends, and returns the error of ctx when it ends first.
func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
