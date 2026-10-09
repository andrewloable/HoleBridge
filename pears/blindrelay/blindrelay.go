// Package blindrelay is the pears-go port of blind-relay 1.6.1: a server that pairs two streams by
// a token and forwards their UDX packets without reading them, and the client that asks for the
// pairing. Both ride the Protomux channel "blind-relay", with no handshake. The channel's id is the
// public key of the relay connection: the client opens it under the key it dialed the relay with, and
// the server accepts it under the connection's remote public key. Upstream hyperdht does the same, so
// its clients and relays pair with these ones.
//
// Ported from blind-relay 1.6.1 (index.js and its message encodings), Apache License 2.0, author
// Holepunch. The npm package has no copyright line of its own; its repository is
// holepunchto/blind-relay.
package blindrelay

import (
	"errors"
	"math"
	"math/rand/v2"
	"net"
	"sync"
	"time"

	"github.com/andrewloable/HoleBridge/pears/compact"
	"github.com/andrewloable/HoleBridge/pears/protomux"
	"github.com/andrewloable/HoleBridge/pears/udx"
)

// protocol is the Protomux channel name. Its id is the relay connection's public key.
const protocol = "blind-relay"

// pairTimeout is how long Pair waits for the server to match the peer. HyperDHT gives up on a
// relay pairing after 15 seconds (setTimeout in lib/connect.js and lib/server.js); blind-relay has
// no timeout of its own.
const pairTimeout = 15 * time.Second

var (
	errAlreadyPairing = errors.New("blindrelay: already pairing this token")
	errPairTimeout    = errors.New("blindrelay: pairing timed out")
	errChannelClosed  = errors.New("blindrelay: channel closed")
	errUint32         = errors.New("blindrelay: id or seq does not fit in 32 bits")
)

// ServerOptions configures a Server.
type ServerOptions struct {
	// Socket is the UDX socket the server opens its relay streams on.
	Socket *udx.Socket
}

// Server pairs the streams of clients that ask for the same token, and forwards the packets of
// each pair to the other side without decrypting them.
type Server struct {
	sock    *udx.Socket
	mu      sync.Mutex
	pending map[[32]byte]*pendingPair
	matched uint64 // the pairings matched so far, guarded by mu
}

// session is one client's channel on the server. closed is closed when the channel closes.
type session struct {
	pair   *protomux.Message // the server's pair message, which carries the replies
	closed chan struct{}
}

// ask is one side of a pairing. localID is the client's stream id: packets sent to this side carry
// it as their remote id. relayID is the server's stream id for this side, and ch carries the
// packets the socket routes to it. addr is the side's address, learned from its first datagram.
type ask struct {
	sess        *session
	isInitiator bool
	localID     uint32
	relayID     uint32
	ch          <-chan udx.Packet
	addr        net.Addr
}

// pendingPair holds the asks under one token. links[0] is the non-initiator's and links[1] the
// initiator's, as upstream indexes them.
type pendingPair struct {
	links [2]*ask
}

// NewServer returns a Server on opts.Socket.
func NewServer(opts ServerOptions) *Server {
	return &Server{sock: opts.Socket, pending: map[[32]byte]*pendingPair{}}
}

// Pairings returns how many pairings the server has matched: the tokens that both sides asked for.
func (s *Server) Pairings() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.matched
}

// Accept serves the blind-relay channel on mux. id is the remote public key of the connection on mux,
// which is the id the client opens its channel under; upstream accepts with accept(stream, { id:
// stream.remotePublicKey }). The server's channel opens when the client's open arrives, so Accept does
// not write to the stream and can return before the client connects.
func (s *Server) Accept(mux *protomux.Mux, id []byte) error {
	mux.Pair(protocol, id, func() { s.open(mux, id) })
	return nil
}

// open runs when a client opens its channel. The server's channel takes that open, and opens back.
func (s *Server) open(mux *protomux.Mux, id []byte) {
	sess := &session{closed: make(chan struct{})}
	ch := mux.CreateChannel(protomux.ChannelOptions{
		Protocol: protocol,
		ID:       id,
		OnClose:  func(bool) { close(sess.closed); s.forget(sess) },
	})
	if ch == nil {
		return
	}
	// Message indexes follow upstream: pair is 0, unpair is 1.
	sess.pair = ch.AddMessage(func(b []byte) { s.onPair(sess, b) })
	ch.AddMessage(s.onUnpair)
	_ = ch.Open(nil) // if the open fails, the replies fail on Send
}

// onPair records a pair message. Once both sides of its token have asked, it replies to each side,
// with the id of the server's stream for that side. Upstream sends that reply with Seq 0.
func (s *Server) onPair(sess *session, b []byte) {
	m, err := DecodePair(b)
	if err != nil {
		return
	}
	i := 0
	if m.IsInitiator {
		i = 1
	}
	s.mu.Lock()
	p := s.pending[m.Token]
	if p == nil {
		p = &pendingPair{}
		s.pending[m.Token] = p
	}
	if p.links[i] != nil {
		s.mu.Unlock()
		return
	}
	p.links[i] = &ask{sess: sess, isInitiator: m.IsInitiator, localID: m.ID}
	if p.links[0] == nil || p.links[1] == nil {
		s.mu.Unlock()
		return
	}
	delete(s.pending, m.Token)
	s.matched++
	s.mu.Unlock()

	// The registration, the forwarding and the replies go out without the lock, so a slow client
	// cannot hold up other pairings. Each side's stream id is routed before its reply is sent.
	for _, a := range p.links {
		if s.register(a) != nil {
			return
		}
	}
	go s.relay(p.links[0], p.links[1])
	for _, a := range p.links {
		reply, _ := EncodePair(PairMessage{IsInitiator: a.isInitiator, Token: m.Token, ID: a.relayID})
		_ = a.sess.pair.Send(reply)
	}
}

// register routes a random stream id on the socket to a channel, and sets it as a's relay id and
// a's channel. It draws again when the id is taken, and fails only when the socket is closed.
func (s *Server) register(a *ask) error {
	for {
		id := newStreamID()
		ch, err := s.sock.Register(id)
		if err == nil {
			a.relayID, a.ch = id, ch
			return nil
		}
		if errors.Is(err, net.ErrClosed) {
			return err
		}
	}
}

// relay forwards the packets of one pairing until either side's session closes or the socket
// closes. The server never reads the payloads: it sends each packet on to the other side. When it
// stops, the pairing's two relay stream ids are freed on the socket.
func (s *Server) relay(a, b *ask) {
	defer s.sock.Unregister(a.relayID)
	defer s.sock.Unregister(b.relayID)
	for {
		select {
		case pk, ok := <-a.ch:
			if !ok {
				return
			}
			s.forward(a, b, pk)
		case pk, ok := <-b.ch:
			if !ok {
				return
			}
			s.forward(b, a, pk)
		case <-a.sess.closed:
			return
		case <-b.sess.closed:
			return
		}
	}
}

// forward sends pk from one side on to the other. The sender's address is the one its first
// datagram came from. Nothing is sent to a side whose address is not known yet, as upstream does.
// The header is rebuilt with the other side's stream id as the remote id, and the payload goes out
// as it came.
func (s *Server) forward(from, to *ask, pk udx.Packet) {
	if from.addr == nil {
		from.addr = pk.Addr
	}
	if to.addr == nil {
		return
	}
	pk.Header.RemoteID = to.localID
	_, _ = s.sock.Raw().WriteTo(udx.EncodeHeader(pk.Header, pk.Payload), to.addr)
}

// onUnpair cancels the pending pairing under the token.
func (s *Server) onUnpair(b []byte) {
	m, err := DecodeUnpair(b)
	if err != nil {
		return
	}
	s.mu.Lock()
	delete(s.pending, m.Token)
	s.mu.Unlock()
}

// forget drops the pending asks of a session that closed, so that no later pairing waits on it.
func (s *Server) forget(sess *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for token, p := range s.pending {
		for i, a := range p.links {
			if a != nil && a.sess == sess {
				p.links[i] = nil
			}
		}
		if p.links[0] == nil && p.links[1] == nil {
			delete(s.pending, token)
		}
	}
}

// newStreamID returns a random nonzero stream id for the server's side of a pairing. register
// checks that the socket has no stream under it.
func newStreamID() uint32 {
	for {
		if id := rand.Uint32(); id != 0 {
			return id
		}
	}
}

// Client asks a Server to pair a local UDX stream with its peer's stream.
type Client struct {
	pair    *protomux.Message // nil when the channel could not be created
	closed  chan struct{}     // closed when the channel closes
	mu      sync.Mutex
	waiting map[[32]byte]*request
}

// request is one Pair call that waits for the server's reply.
type request struct {
	isInitiator bool
	done        chan uint32 // receives the server's stream id for this side
}

// Pairing is a matched pairing. RemoteID is the id of the server's stream for this side: connect
// the local stream to it, at the address of the server's socket.
type Pairing struct {
	RemoteID uint32
}

// NewClient returns a Client that speaks the blind-relay channel of mux under id, the public key the
// relay connection was dialed with. The channel opens at once.
func NewClient(mux *protomux.Mux, id []byte) *Client {
	c := &Client{closed: make(chan struct{}), waiting: map[[32]byte]*request{}}
	ch := mux.CreateChannel(protomux.ChannelOptions{
		Protocol: protocol,
		ID:       id,
		OnClose:  func(bool) { close(c.closed) },
	})
	if ch == nil {
		return c
	}
	c.pair = ch.AddMessage(c.onPair)
	_ = ch.Open(nil) // if the open fails, Pair fails on Send
	return c
}

// Pair asks the server to pair local with the peer that asks for the same token. It blocks until
// the server reports the pairing, and gives up after 15 seconds, the relay pairing timeout that
// HyperDHT uses upstream.
func (c *Client) Pair(isInitiator bool, token [32]byte, local *udx.Stream) (*Pairing, error) {
	if c.pair == nil {
		return nil, errChannelClosed
	}
	req := &request{isInitiator: isInitiator, done: make(chan uint32, 1)}
	c.mu.Lock()
	if _, ok := c.waiting[token]; ok {
		c.mu.Unlock()
		return nil, errAlreadyPairing
	}
	c.waiting[token] = req
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.waiting, token)
		c.mu.Unlock()
	}()

	msg, err := EncodePair(PairMessage{IsInitiator: isInitiator, Token: token, ID: local.ID()})
	if err != nil {
		return nil, err
	}
	if err := c.pair.Send(msg); err != nil {
		return nil, err
	}
	timer := time.NewTimer(pairTimeout)
	defer timer.Stop()
	select {
	case id := <-req.done:
		return &Pairing{RemoteID: id}, nil
	case <-c.closed:
		return nil, errChannelClosed
	case <-timer.C:
		return nil, errPairTimeout
	}
}

// onPair hands the server's reply to the Pair call that waits for it. A reply for a token nobody is
// pairing, or for the other side of the pairing, is ignored.
func (c *Client) onPair(b []byte) {
	m, err := DecodePair(b)
	if err != nil {
		return
	}
	c.mu.Lock()
	req := c.waiting[m.Token]
	c.mu.Unlock()
	if req == nil || req.isInitiator != m.IsInitiator {
		return
	}
	select {
	case req.done <- m.ID:
	default:
	}
}

// PairMessage is the pair message. A client sends it to ask for a pairing, with ID its local
// stream id. The server sends one back to each side once both sides have asked, with ID the
// server's stream id for that side. Upstream always sends Seq as 0.
type PairMessage struct {
	IsInitiator bool
	Token       [32]byte
	ID          uint32
	Seq         uint32
}

// UnpairMessage cancels the pending pairing under Token.
type UnpairMessage struct {
	Token [32]byte
}

// EncodePair returns the bytes blind-relay 1.6.1 writes for m: a flags byte whose bit 0 is
// IsInitiator, then the 32-byte token, then ID and Seq as compact uints.
func EncodePair(m PairMessage) ([]byte, error) {
	var e compact.Encoder
	var flags uint8
	if m.IsInitiator {
		flags = 1
	}
	e.Uint8(flags)
	e.Fixed(m.Token[:])
	e.Uint(uint64(m.ID))
	e.Uint(uint64(m.Seq))
	return e.Bytes(), nil
}

// DecodePair parses the bytes of a pair message. It fails on truncated input and on an ID or Seq
// that does not fit in 32 bits.
func DecodePair(b []byte) (PairMessage, error) {
	d := compact.NewDecoder(b)
	flags, err := d.Uint8()
	if err != nil {
		return PairMessage{}, err
	}
	var m PairMessage
	m.IsInitiator = flags&1 != 0
	if m.Token, err = decodeToken(d); err != nil {
		return PairMessage{}, err
	}
	if m.ID, err = decodeUint32(d); err != nil {
		return PairMessage{}, err
	}
	if m.Seq, err = decodeUint32(d); err != nil {
		return PairMessage{}, err
	}
	return m, nil
}

// EncodeUnpair returns the bytes blind-relay 1.6.1 writes for m: a zero flags byte, then the token.
func EncodeUnpair(m UnpairMessage) ([]byte, error) {
	var e compact.Encoder
	e.Uint8(0)
	e.Fixed(m.Token[:])
	return e.Bytes(), nil
}

// DecodeUnpair parses the bytes of an unpair message.
func DecodeUnpair(b []byte) (UnpairMessage, error) {
	d := compact.NewDecoder(b)
	if _, err := d.Uint8(); err != nil {
		return UnpairMessage{}, err
	}
	token, err := decodeToken(d)
	if err != nil {
		return UnpairMessage{}, err
	}
	return UnpairMessage{Token: token}, nil
}

// decodeToken reads a 32-byte token.
func decodeToken(d *compact.Decoder) ([32]byte, error) {
	var t [32]byte
	b, err := d.Fixed(32)
	if err != nil {
		return t, err
	}
	copy(t[:], b)
	return t, nil
}

// decodeUint32 reads a compact uint that must fit in 32 bits.
func decodeUint32(d *compact.Decoder) (uint32, error) {
	v, err := d.Uint()
	if err != nil {
		return 0, err
	}
	if v > math.MaxUint32 {
		return 0, errUint32
	}
	return uint32(v), nil
}
