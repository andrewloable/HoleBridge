// Ported from hyperdht 6.34.1 lib/server.js (listen, close, the firewall and the handshake reply) and the
// createServer of index.js, MIT License, Copyright (c) 2018-2019 Mathias Buus, David Mark Clements &
// Contributors.
//
// The HyperDHT server. Listen announces the key pair on the hash of its public key, and keeps a route to
// the server on this node, so the handshakes that reach this node are answered here (router.go). The
// firewall decides each handshake once. An admitted client gets the server's reply, and a UDX stream on the
// node's socket is connected to the client, which the secret stream runs over, as upstream's server does for
// a direct connection. The reply also names a holepunch id and the relays the record is stored on, and a
// PEER_HOLEPUNCH for that id is answered here (answerHolepunch). The server does not punch yet: it has no
// NAT samples, so its own firewall stays unknown, and no punch socket is wired to the DHT socket. Its relay
// policy (relay.go) names the relay in each reply. A relayed handshake also pairs on that relay, but the direct
// stream claims the connection first; only a forced server (the test seam dht.forceRelay) lets the paired relay
// claim it, and then no direct stream is made.
package hyperdht

import (
	"context"
	"errors"
	"io"
	"math"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/blake2b"

	"github.com/andrewloable/HoleBridge/pears/noise"
	"github.com/andrewloable/HoleBridge/pears/secretstream"
	"github.com/andrewloable/HoleBridge/pears/udx"
)

// Command numbers of hyperdht's COMMANDS (lib/constants.js) that the server answers: the handshake, which
// the router relays, and FIND_PEER, which answers with the route's record for the hash of a key.
const (
	cmdPeerHandshake = 0
	cmdFindPeer      = 2
)

// Modes of a handshake message (lib/router.js): from the client, from the server, from the relay that
// forwards it, from the second relay, and the reply to the client.
const (
	handshakeFromClient      = 0
	handshakeFromServer      = 1
	handshakeFromRelay       = 2
	handshakeFromSecondRelay = 3
	handshakeReply           = 4
)

// Error codes of a handshake reply (lib/constants.js ERROR). The firewall state is left UNKNOWN (0), since
// this node does not yet learn its own address.
const (
	handshakeNone            = 0
	handshakeAborted         = 1
	handshakeVersionMismatch = 2
)

// Timing of upstream's server: a handshake is remembered for handshakeClearWait, the record is announced
// again every announceInterval so it does not lapse, and each walk of an announce or an unannounce is
// bounded.
const (
	handshakeClearWait = 10 * time.Second
	announceInterval   = 5 * time.Minute
	announceTimeout    = 30 * time.Second
	unannounceTimeout  = 30 * time.Second
	// headerExchangeWait is how long an admitted client has to finish the secret stream's header exchange. It is
	// upstream's HANDSHAKE_INITIAL_TIMEOUT, the time an admitted handshake gets to connect before it is dropped.
	headerExchangeWait = 10 * time.Second
)

// nsPeerHandshake is NS.PEER_HANDSHAKE, the Noise prologue of every handshake.
var nsPeerHandshake = dhtNamespace(cmdPeerHandshake)

var (
	errServerClosed     = errors.New("hyperdht: server closed")
	errAlreadyListening = errors.New("hyperdht: server already listening")
)

// ErrKeyPairAlreadyUsed is the error of a Listen on a key pair that another server on the same DHT listens on:
// upstream's KEYPAIR_ALREADY_USED. The first server keeps its record and its route.
var ErrKeyPairAlreadyUsed = errors.New("hyperdht: key pair already used by a server on this DHT")

// HandshakePayload is the payload a client sends inside its Noise handshake, as the NoisePayload encoding
// carries it.
type HandshakePayload = NoisePayload

// Conn is an accepted connection: a secret stream over a UDX stream. RemotePublicKey returns the key the
// client proved in its handshake.
type Conn = secretstream.Stream

// ServerOptions sets up a server. Firewall is called once for each handshake that reaches the server, with
// the key the client proved and the client's payload. It returns true to refuse the handshake. A refused
// handshake gets no reply, as upstream's firewall drops it. A nil Firewall admits every key. RelayThrough
// is the server's relay policy, as ConnectOptions has it: it is asked once per handshake, with force false,
// and returns the public key of the relay the server's connections go through, or nil for none. Both run
// off the DHT's read loop, so they may block and may use the same DHT, as a connect does. Calls for different
// handshakes may run at once, so they must be safe for concurrent use. A handshake that arrives while
// maxHandshakes others are being decided gets no reply.
// Keepalive is the interval of empty keepalive frames on each accepted connection's secret stream while it is
// idle; zero disables them. Upstream's default is 5 s (connectionKeepAlive).
type ServerOptions struct {
	Firewall     func(remotePublicKey [32]byte, payload HandshakePayload) bool
	RelayThrough func(force bool) *[32]byte
	Keepalive    time.Duration
}

// Server accepts the connections made to one key pair on a DHT node.
type Server struct {
	d            *DHT
	firewall     func(remotePublicKey [32]byte, payload HandshakePayload) bool
	relayThrough func(force bool) *[32]byte // the relay policy, asked once per handshake
	keepalive    time.Duration              // the keepalive interval of each accepted connection's secret stream
	ctx          context.Context            // canceled by Close
	cancel       context.CancelFunc         // cancels ctx

	mu            sync.Mutex
	keyPair       noise.KeyPair
	target        [32]byte
	listening     bool
	closed        bool
	handshakes    map[string][]byte         // the reply to each handshake message seen: nil when refused
	relays        []RelayInfo               // the nodes the record was stored on by the last announce
	holepunches   map[uint64]*securePayload // the holepunch payload of each admitted handshake, by its id
	nextHolepunch uint64
	conns         chan *Conn
}

// CreateServer returns a server on d. Nothing is announced until Listen. The server is closed by Close on d;
// on a DHT that is already closed, the server is closed from the start.
func (d *DHT) CreateServer(opts ServerOptions) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		d:            d,
		firewall:     opts.Firewall,
		relayThrough: opts.RelayThrough,
		keepalive:    opts.Keepalive,
		ctx:          ctx,
		cancel:       cancel,
		handshakes:   make(map[string][]byte),
		holepunches:  make(map[uint64]*securePayload),
		conns:        make(chan *Conn),
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		s.closed = true
		cancel()
		return s
	}
	if d.servers == nil {
		d.servers = make(map[*Server]struct{})
	}
	d.servers[s] = struct{}{}
	return s
}

// Listen starts the server on the key pair kp. It announces kp on the DHT under the hash of its public key,
// and keeps the route to this server on this node, so handshakes that reach the node are answered here. It
// returns ErrKeyPairAlreadyUsed when another server on this DHT listens on kp, and the error of the announce
// when no node stores the record, and then drops the route. The record is announced again every announceInterval
// until Close.
func (s *Server) Listen(ctx context.Context, kp noise.KeyPair) error {
	s.mu.Lock()
	switch {
	case s.closed:
		s.mu.Unlock()
		return errServerClosed
	case s.listening:
		s.mu.Unlock()
		return errAlreadyListening
	}
	if !s.d.claimKey(kp.Public, s) {
		s.mu.Unlock()
		return ErrKeyPairAlreadyUsed
	}
	s.listening = true
	s.keyPair = kp
	s.target = blake2b.Sum256(kp.Public[:])
	target := s.target
	s.mu.Unlock()

	s.d.routes.set(target, route{peer: Peer{PublicKey: kp.Public[:]}, serve: s})
	if err := s.announce(ctx, target, kp); err != nil {
		s.d.routes.dropServer(target, s)
		s.d.releaseKey(kp.Public, s)
		s.mu.Lock()
		s.listening = false
		s.mu.Unlock()
		return err
	}
	go s.reannounce(target, kp)
	return nil
}

// reannounce announces the record again every announceInterval, until Close. A failed announce is tried
// again at the next interval.
func (s *Server) reannounce(target [32]byte, kp noise.KeyPair) {
	t := time.NewTicker(announceInterval)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			s.announce(s.ctx, target, kp)
		}
	}
}

// announce announces kp on target, within announceTimeout, and keeps the relays the record was stored on.
func (s *Server) announce(ctx context.Context, target [32]byte, kp noise.KeyPair) error {
	ctx, cancel := context.WithTimeout(ctx, announceTimeout)
	defer cancel()
	relays, err := s.d.announceRelays(ctx, target, kp, nil)
	s.mu.Lock()
	s.relays = relays
	s.mu.Unlock()
	return err
}

// Close unannounces the key pair, refuses the handshakes that arrive after it, and makes Accept return an
// error. The unannounce is best effort: upstream's announcer ignores its failures on stop too.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	listening, kp, target := s.listening, s.keyPair, s.target
	s.mu.Unlock()

	s.d.forgetServer(s)
	s.cancel()
	if !listening {
		return nil
	}
	s.d.releaseKey(kp.Public, s)
	s.d.routes.dropServer(target, s)
	ctx, cancel := context.WithTimeout(context.Background(), unannounceTimeout)
	defer cancel()
	_ = s.d.Unannounce(ctx, target, kp)
	return nil
}

// Accept returns the next connection whose handshake the firewall admitted, once its secret stream's header
// exchange is done. It returns an error once the server is closed.
func (s *Server) Accept() (*Conn, error) {
	select {
	case c := <-s.conns:
		return c, nil
	case <-s.ctx.Done():
		return nil, errServerClosed
	}
}

// answer returns the server's reply to msg, a client's first handshake message sent from the address from,
// or nil when the handshake gets no reply: the server is closed, the message does not verify, or the
// firewall refuses the client's key. Each message is decided once. A repeat gets the same reply, and the
// firewall is not asked again and no second stream is made, since the client resends its message until it
// hears back, and upstream keeps each handshake for a while. A repeat that arrives while the first decision
// is still running gets no reply: the client resends with the same tid, so the first decision's reply
// answers it.
func (s *Server) answer(msg []byte, from *net.UDPAddr, direct bool) []byte {
	key := string(msg)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	if reply, seen := s.handshakes[key]; seen {
		s.mu.Unlock()
		return reply
	}
	s.handshakes[key] = nil
	kp := s.keyPair
	s.mu.Unlock()

	reply := s.admit(kp, msg, from, direct)

	s.mu.Lock()
	s.handshakes[key] = reply
	s.mu.Unlock()
	time.AfterFunc(handshakeClearWait, func() {
		s.mu.Lock()
		delete(s.handshakes, key)
		s.mu.Unlock()
	})
	return reply
}

// admit reads the client's handshake message, asks the firewall about the key the client proves, and returns
// the server's Noise reply. The reply carries the error code of the handshake, as upstream's does: a client
// with no UDX info, or a payload of another version, gets an error code and not a silence. An admitted client
// with UDX info gets a UDX stream on this node, connected to the client's stream at from, whose id is in the
// reply, and serve runs the secret stream over it once the reply is sent. It returns nil when the message
// does not verify, the firewall refuses the client, or the stream cannot be made. direct says whether the
// handshake came straight from the client; a relayed one also pairs on the relay (upstream's _relayConnection).
func (s *Server) admit(kp noise.KeyPair, msg []byte, from *net.UDPAddr, direct bool) []byte {
	hs := noise.NewResponder(kp, nsPeerHandshake[:])
	body, err := hs.Recv(msg)
	if err != nil {
		return nil
	}
	p, err := DecodeNoisePayload(body)
	if err != nil {
		return nil
	}
	_, _, _, remote := hs.Result()
	if s.firewall != nil && s.firewall(remote, p) {
		return nil
	}
	code := uint64(handshakeVersionMismatch)
	if p.Version == 1 {
		code = handshakeAborted
		if p.UDX != nil && p.UDX.ID <= math.MaxUint32 {
			code = handshakeNone
		}
	}
	offer, err := newRelayOffer(s.relayThrough, false)
	if err != nil {
		return nil
	}
	forced := s.d.forceRelay
	var st *udx.Stream
	var holepunch *HolepunchInfo
	var holepunchID uint64
	udxInfo := UDXInfo{Version: 1}
	cl := &streamClaim{}
	// fail returns no reply, and drops the stream made for this handshake.
	fail := func() []byte {
		if st != nil {
			st.Destroy()
		}
		return nil
	}
	if code == handshakeNone {
		st = s.d.newStream()
		if forced && offer == nil && p.RelayThrough == nil {
			return fail() // a forced stream is claimed only through a relay, and this handshake names none
		}
		if !forced {
			// The direct path claims the stream at once, as upstream does for a handshake that comes direct. A
			// forced stream waits for its relay pairing, which claims it once the reply is out.
			if err := st.Connect(uint32(p.UDX.ID), from); err != nil {
				return fail()
			}
			cl.take()
		}
		udxInfo.ID = uint64(st.ID())
		holepunchID = s.reserveHolepunch()
		holepunch = &HolepunchInfo{ID: holepunchID, Relays: s.relayList()}
	}
	payload, err := EncodeNoisePayload(NoisePayload{
		Error:        code,
		Holepunch:    holepunch,
		UDX:          &udxInfo,
		SecretStream: &SecretStreamInfo{Version: 1},
		RelayThrough: offer.info(),
	})
	if err != nil {
		return fail()
	}
	out, err := hs.Send(payload)
	if err != nil {
		return fail()
	}
	if st != nil {
		_, _, hash, _ := hs.Result()
		s.keepHolepunch(holepunchID, punchSecret(hash))
		keys := keysOf(hs)
		if forced {
			// The relay claims the stream: this side's own relay as initiator, else the one the client offered.
			if offer != nil {
				go s.serveRelayed(st, cl, *offer, true, keys)
			} else {
				go s.serveRelayed(st, cl, offerOf(p.RelayThrough), false, keys)
			}
			return out
		}
		go s.serve(st, keys)
		// A relayed handshake also pairs on the relay, as upstream does. The direct stream has claimed already, so
		// the pairing gives itself up when it lands.
		if !direct {
			switch {
			case offer != nil:
				go s.d.pairRelay(s.ctx, cl, st, *offer, true)
			case p.RelayThrough != nil:
				go s.d.pairRelay(s.ctx, cl, st, offerOf(p.RelayThrough), false)
			}
		}
	}
	return out
}

// reserveHolepunch returns a new holepunch id. The id goes in the reply, and the payload of the handshake is kept
// under it once the handshake is complete (keepHolepunch).
func (s *Server) reserveHolepunch() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextHolepunch
	s.nextHolepunch++
	return id
}

// keepHolepunch keeps the holepunch payload of an admitted handshake under id, for handshakeClearWait, as upstream
// keeps a handshake's holepunch slot.
func (s *Server) keepHolepunch(id uint64, secret [32]byte) {
	s.mu.Lock()
	s.holepunches[id] = newSecurePayload(secret)
	s.mu.Unlock()
	time.AfterFunc(handshakeClearWait, func() {
		s.mu.Lock()
		delete(s.holepunches, id)
		s.mu.Unlock()
	})
}

// relayList returns a copy of the relays the record was stored on by the last announce.
func (s *Server) relayList() []RelayInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]RelayInfo(nil), s.relays...)
}

// isRelayLocked reports whether addr is one of the relays the record was stored on. The caller holds mu.
func (s *Server) isRelayLocked(addr *net.UDPAddr) bool {
	if addr == nil || addr.IP.To4() == nil {
		return false
	}
	a := addressOf(addr)
	for _, r := range s.relays {
		if r.RelayAddress == a {
			return true
		}
	}
	return false
}

// answerHolepunch returns the server's encrypted reply to a holepunch probe for the handshake id. The probe's
// payload came from the client at peer, through the relay at from. It returns nil when no admitted handshake has
// id, or the payload does not decrypt. A probe that reports an error or asks to punch gets an abort, since the
// server's firewall is unknown and it does not punch yet. Upstream's server answers this way, with the token of
// the client's address when a relay of the record forwarded the probe.
func (s *Server) answerHolepunch(id uint64, payload []byte, peer, from *net.UDPAddr) []byte {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	sp, ok := s.holepunches[id]
	fromRelay := s.isRelayLocked(from)
	s.mu.Unlock()
	if !ok {
		return nil
	}
	remote, ok := sp.decrypt(payload)
	if !ok {
		return nil
	}
	reply := HolepunchPayload{Firewall: firewallUnknown, Round: remote.Round, RemoteToken: remote.Token}
	switch {
	case remote.Error != 0 || remote.Punching:
		reply = HolepunchPayload{Error: handshakeAborted, Firewall: firewallUnknown, Round: remote.Round}
	case fromRelay:
		token := sp.token(addressOf(peer))
		reply.Token = token[:]
	}
	out, err := sp.encrypt(reply)
	if err != nil {
		return nil
	}
	return out
}

// serveRelayed claims st through a relay pairing (pairRelay) on the relay offer r, as initiator or responder, and
// serves the claimed stream. A pairing that fails gives st up.
func (s *Server) serveRelayed(st *udx.Stream, cl *streamClaim, r relayOffer, initiator bool, keys secretstream.Keys) {
	rs, _, err := s.d.pairRelay(s.ctx, cl, st, r, initiator)
	if err != nil {
		st.Destroy()
		return
	}
	s.serve(rs, keys)
}

// serve runs the secret stream over conn, the stream of an admitted handshake, and hands the connection to Accept
// once the header exchange is done. The exchange has headerExchangeWait. A client that does not finish it in time
// has its stream closed, since Handshake closes the stream when its context ends, and so does a server that closes
// first.
func (s *Server) serve(conn io.ReadWriteCloser, keys secretstream.Keys) {
	ctx, cancel := context.WithTimeout(s.ctx, headerExchangeWait)
	defer cancel()
	c := secretstream.Resume(conn, false, secretstream.Options{Keepalive: s.keepalive}, keys)
	if err := c.Handshake(ctx); err != nil {
		return // Handshake has closed the stream
	}
	select {
	case s.conns <- c:
	case <-s.ctx.Done():
		c.Close()
	}
}
