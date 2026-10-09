// Framing, header exchange, keepalive, close and unordered messages follow @hyperswarm/secret-stream 6.9.2
// (index.js and lib/handshake.js), Apache License 2.0, author Mathias Buus. The npm package has no copyright line of
// its own; its repository is holepunchto/hyperswarm-secret-stream. The namespaces and unordered message
// keys follow the namespace function of hypercore-crypto 3.7.0 (index.js), MIT License:
//
//	Copyright (c) 2018 Mathias Buus

package secretstream

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andrewloable/HoleBridge/pears/noise"
	"golang.org/x/crypto/blake2b"
	"golang.org/x/crypto/nacl/secretbox"
)

// Options configures a Stream.
type Options struct {
	KeyPair         noise.KeyPair // this side's static key pair
	RemotePublicKey *[32]byte     // the peer's static public key; an initiator requires it, nil accepts any
	Keepalive       time.Duration // interval of empty keepalive frames while idle; zero disables them
}

// maxFrame is the largest payload that the 3-byte length prefix can name. maxPlain is the most
// plaintext one message carries, so that its frame still fits the prefix. maxNoiseMessage is the largest
// Noise handshake message upstream accepts (noise-handshake cipher.js), which bounds the frames read
// before the peer is authenticated. The header frame needs no constant: it is the 32-byte stream id and
// the 24-byte header, which upstream checks for exactly.
const (
	maxFrame        = 1<<24 - 1
	maxPlain        = maxFrame - ABytes
	maxNoiseMessage = 65535
)

var (
	errNotConnected  = errors.New("secretstream: handshake not complete")
	errHandshaken    = errors.New("secretstream: handshake already run")
	errNoRemoteKey   = errors.New("secretstream: an initiator needs RemotePublicKey")
	errWrongKey      = errors.New("secretstream: peer public key is not RemotePublicKey")
	errBadHeader     = errors.New("secretstream: invalid stream header")
	errFrameTooLarge = errors.New("secretstream: frame too large")
	errNoUnordered   = errors.New("secretstream: connection has no unordered messages")
)

// closeWriter is implemented by TCP connections, which can end their outgoing side alone.
type closeWriter interface{ CloseWrite() error }

// unorderedSender is implemented by UDX streams, which send unordered messages.
type unorderedSender interface{ SendMessage([]byte) error }

// unorderedReceiver is implemented by UDX streams, which deliver the peer's unordered messages.
type unorderedReceiver interface{ Messages() <-chan []byte }

// doneSignaler is implemented by UDX streams, which close Done when they are torn down: by Destroy or Close, by the
// peer's DESTROY, or by a timeout. The unordered messages of a torn-down connection end with it.
type doneSignaler interface{ Done() <-chan struct{} }

// Stream is a Noise-authenticated secret stream over a duplex connection.
type Stream struct {
	conn        io.ReadWriteCloser
	isInitiator bool
	keyPair     noise.KeyPair
	remote      *[32]byte
	keepalive   time.Duration

	resumed   *Keys    // the outcome of a Noise handshake run before the stream, from Resume; nil otherwise
	peer      [32]byte // the peer's static public key, set by Handshake
	hash      [64]byte // the Noise handshake hash, set by Handshake
	enc       *Push
	dec       *Pull
	sendKey   [32]byte // seals this side's unordered messages
	recvKey   [32]byte // opens the peer's unordered messages
	sendCount uint64   // the unordered message counter; the key is unique per handshake, so it starts at zero

	wmu       sync.Mutex    // serializes writes: data, keepalive and unordered messages
	last      time.Time     // when a message was last written, for keepalive; guarded by wmu
	done      chan struct{} // closed by Close
	torn      chan struct{} // closed when the connection is closed whole, by Destroy or by Close after the peer ended
	msgs      chan []byte   // the peer's unordered messages, opened; see Messages
	loopEnded chan struct{} // closed when receiveMessages returns

	closeOnce   sync.Once
	closeErr    error
	destroyOnce sync.Once
	destroyErr  error
	tornOnce    sync.Once
	readEnded   atomic.Bool // set when Read hits an error, such as the peer's end

	pending []byte // decrypted data that Read has not returned yet
	rerr    error  // the first read error, which repeats
}

var _ io.ReadWriteCloser = (*Stream)(nil)

// New wraps conn in a secret stream that is not yet connected. Handshake connects it.
func New(conn io.ReadWriteCloser, isInitiator bool, opts Options) *Stream {
	return &Stream{
		conn:        conn,
		isInitiator: isInitiator,
		keyPair:     opts.KeyPair,
		remote:      opts.RemotePublicKey,
		keepalive:   opts.Keepalive,
		done:        make(chan struct{}),
		torn:        make(chan struct{}),
		msgs:        make(chan []byte),
		loopEnded:   make(chan struct{}),
	}
}

// Keys is the outcome of a Noise handshake that ran before the secret stream: the transport keys, the
// handshake hash and the peer's static public key, as noise.Handshake.Result gives them.
type Keys struct {
	Tx, Rx [32]byte
	Hash   [64]byte
	Peer   [32]byte
}

// Resume wraps conn in a secret stream whose Noise handshake the caller already ran, with keys as its
// outcome. It is how upstream's secret stream starts after a HyperDHT handshake: Handshake then runs only
// the header exchange, and no Noise message crosses conn. opts.KeyPair is not used, since no Noise
// handshake runs here.
func Resume(conn io.ReadWriteCloser, isInitiator bool, opts Options, keys Keys) *Stream {
	s := New(conn, isInitiator, opts)
	s.resumed = &keys
	return s
}

// Handshake runs the Noise IK handshake and the header exchange over the connection. The initiator
// sends message 1 and the responder sends message 2. It returns when the stream is ready, fails, or
// ctx is done. A failed handshake closes the connection, as upstream destroys the stream.
func (s *Stream) Handshake(ctx context.Context) error {
	if s.enc != nil {
		return errHandshaken
	}
	stop := context.AfterFunc(ctx, func() { s.conn.Close() })
	err := s.handshake()
	if !stop() && err == nil {
		err = ctx.Err() // ctx ended as the handshake finished, so the connection is closing
	}
	if err != nil {
		s.conn.Close()
		return err
	}
	if s.keepalive > 0 {
		go s.keepAlive()
	}
	go s.receiveMessages()
	return nil
}

// handshake gets the outcome of the Noise handshake, from the connection or from Resume, then exchanges the
// stream headers. The header order differs by role, so that neither side writes while the other is also
// writing, which an unbuffered pipe cannot carry.
func (s *Stream) handshake() error {
	k, err := s.keys()
	if err != nil {
		return err
	}
	if s.remote != nil && k.Peer != *s.remote {
		return errWrongKey
	}
	s.peer, s.hash = k.Peer, k.Hash
	s.sendKey, s.recvKey = unorderedKeys(k.Hash, s.isInitiator)

	me, them := roles(s.isInitiator)
	ownID, peerID := keyedHash(k.Hash, me), keyedHash(k.Hash, them)
	if s.isInitiator {
		if err := s.sendHeader(k.Tx, ownID); err != nil {
			return err
		}
		return s.recvHeader(k.Rx, peerID)
	}
	if err := s.recvHeader(k.Rx, peerID); err != nil {
		return err
	}
	return s.sendHeader(k.Tx, ownID)
}

// keys returns the outcome of the Noise handshake that Resume was given, or else runs the handshake over
// the connection.
func (s *Stream) keys() (Keys, error) {
	if s.resumed != nil {
		return *s.resumed, nil
	}
	return s.noiseHandshake()
}

// noiseHandshake exchanges the two Noise IK messages over the connection.
func (s *Stream) noiseHandshake() (Keys, error) {
	if s.isInitiator && s.remote == nil {
		return Keys{}, errNoRemoteKey
	}
	var hs *noise.Handshake
	if s.isInitiator {
		hs = noise.NewInitiator(s.keyPair, *s.remote, nil)
		msg1, err := hs.Send(nil)
		if err != nil {
			return Keys{}, err
		}
		if err := writeFrame(s.conn, msg1); err != nil {
			return Keys{}, err
		}
		msg2, err := readFrame(s.conn, maxNoiseMessage)
		if err != nil {
			return Keys{}, err
		}
		if _, err := hs.Recv(msg2); err != nil {
			return Keys{}, err
		}
	} else {
		hs = noise.NewResponder(s.keyPair, nil)
		msg1, err := readFrame(s.conn, maxNoiseMessage)
		if err != nil {
			return Keys{}, err
		}
		if _, err := hs.Recv(msg1); err != nil {
			return Keys{}, err
		}
		msg2, err := hs.Send(nil)
		if err != nil {
			return Keys{}, err
		}
		if err := writeFrame(s.conn, msg2); err != nil {
			return Keys{}, err
		}
	}
	tx, rx, hash, peer := hs.Result()
	return Keys{Tx: tx, Rx: rx, Hash: hash, Peer: peer}, nil
}

// sendHeader sends this side's stream id and secret stream header, then seals under tx.
func (s *Stream) sendHeader(tx, id [32]byte) error {
	enc, header, err := NewPush(tx)
	if err != nil {
		return err
	}
	s.enc = enc
	return writeFrame(s.conn, append(id[:], header[:]...))
}

// recvHeader reads the peer's stream id and secret stream header, then opens under rx.
func (s *Stream) recvHeader(rx, id [32]byte) error {
	f, err := readFrame(s.conn, 32+HeaderSize)
	if err != nil {
		return err
	}
	if len(f) != 32+HeaderSize || !bytes.Equal(f[:32], id[:]) {
		return errBadHeader
	}
	s.dec = NewPull(rx, [HeaderSize]byte(f[32:]))
	return nil
}

// Read returns decrypted application data. It returns io.EOF after the peer closes and the pending
// data is read. Keepalive messages are skipped.
func (s *Stream) Read(p []byte) (int, error) {
	if s.dec == nil {
		return 0, errNotConnected
	}
	if err := s.fill(); err != nil {
		return 0, err
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}

// ReadFrame returns the next message's plaintext whole, as the peer wrote it, so a caller that reads frames needs
// no buffer of its own. Any data a Read left pending is returned first. It returns io.EOF after the peer closes and
// the pending data is read. Keepalive messages are skipped.
func (s *Stream) ReadFrame() ([]byte, error) {
	if s.dec == nil {
		return nil, errNotConnected
	}
	if err := s.fill(); err != nil {
		return nil, err
	}
	p := s.pending
	s.pending = nil
	return p, nil
}

// fill reads messages until pending holds data, or a read fails. The first read error repeats.
func (s *Stream) fill() error {
	for len(s.pending) == 0 {
		if s.rerr != nil {
			return s.rerr
		}
		if s.rerr = s.readMessage(); s.rerr != nil {
			s.readEnded.Store(true)
			if s.isClosed() {
				s.closeWhole() // Close ran first and only ended the outgoing side
			}
		}
	}
	return nil
}

// readMessage reads the next frame and opens it into pending. An empty message is a keepalive.
func (s *Stream) readMessage() error {
	frame, err := readFrame(s.conn, maxFrame)
	if err != nil {
		return err
	}
	s.pending, _, err = s.dec.Open(frame, nil)
	return err
}

// Write sends p as encrypted application data, in messages of at most maxPlain bytes.
func (s *Stream) Write(p []byte) (int, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.enc == nil {
		return 0, errNotConnected
	}
	if s.isClosed() {
		return 0, io.ErrClosedPipe
	}
	n := 0
	for len(p) > 0 {
		chunk := p[:min(len(p), maxPlain)]
		if err := s.writeMessage(chunk); err != nil {
			return n, err
		}
		n += len(chunk)
		p = p[len(chunk):]
	}
	return n, nil
}

// writeMessage seals plain as one message and writes it. The caller holds wmu.
func (s *Stream) writeMessage(plain []byte) error {
	if err := writeFrame(s.conn, s.enc.Seal(plain, nil, TagMessage)); err != nil {
		return err
	}
	s.last = time.Now()
	return nil
}

// keepAlive sends an empty message after each keepalive interval without a write, until the stream
// closes or a write fails.
func (s *Stream) keepAlive() {
	t := time.NewTicker(s.keepalive)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			s.wmu.Lock()
			var err error
			if time.Since(s.last) >= s.keepalive && !s.isClosed() {
				err = s.writeMessage(nil)
			}
			s.wmu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

// Close ends the outgoing side of the stream. The peer reads io.EOF after the data written before it.
// On a connection that can end its outgoing side alone (TCP), the read side stays open until the peer
// ends. Any other connection is closed whole.
func (s *Stream) Close() error {
	s.closeOnce.Do(func() {
		close(s.done)
		cw, ok := s.conn.(closeWriter)
		if !ok {
			s.closeErr = s.closeWhole()
			return
		}
		s.closeErr = cw.CloseWrite()
		if s.readEnded.Load() {
			s.closeWhole() // the peer ended first, so nothing is left to read
		}
	})
	return s.closeErr
}

// Destroy closes the connection whole: the read side ends as well as the write side, at once. Close only ends the
// write side on a connection that can (TCP, UDX), so a peer that ignores END keeps the connection and its reads
// open; Destroy does not leave it that way. For a UDX stream the transport sends DESTROY, and for TCP the socket
// closes. Unordered messages stop too: Messages closes even while a message from the peer waits for a reader.
// It is safe to call more than once, and after Close.
func (s *Stream) Destroy() error {
	s.closeOnce.Do(func() { close(s.done) })
	s.destroyOnce.Do(func() { s.destroyErr = s.closeWhole() })
	return s.destroyErr
}

// closeWhole closes the connection whole. Its unordered messages stop with it, so the receive loop ends too, even
// while it waits to deliver one. It is safe to call more than once.
func (s *Stream) closeWhole() error {
	s.tornOnce.Do(func() { close(s.torn) })
	return s.conn.Close()
}

// isClosed reports whether Close has run.
func (s *Stream) isClosed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// Keepalive is the interval of empty keepalive messages the stream sends while idle, as Options set it; zero
// means none.
func (s *Stream) Keepalive() time.Duration {
	return s.keepalive
}

// RemotePublicKey is the peer's static public key. It is valid after the handshake.
func (s *Stream) RemotePublicKey() [32]byte {
	return s.peer
}

// HandshakeHash is the Noise handshake hash, which both sides share. It is valid after the handshake.
func (s *Stream) HandshakeHash() [64]byte {
	return s.hash
}

// Send writes b as an unordered message, which the transport may deliver out of order and without
// waiting for the stream. It needs a connection with a SendMessage method, which UDX streams have. Any
// other connection returns an error.
func (s *Stream) Send(b []byte) error {
	t, ok := s.conn.(unorderedSender)
	if !ok {
		return errNoUnordered
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.enc == nil {
		return errNotConnected
	}
	s.sendCount++
	var nonce [24]byte
	binary.LittleEndian.PutUint64(nonce[:8], s.sendCount)
	env := binary.LittleEndian.AppendUint64(nil, s.sendCount)
	return t.SendMessage(secretbox.Seal(env, b, &nonce, &s.sendKey))
}

// Messages returns the channel that carries the peer's unordered messages, opened, in the order they arrive.
// A message that does not open is dropped without an error, as upstream drops it. The channel closes when the
// connection's Messages channel closes, which UDX does when the stream is torn down. A connection without
// unordered messages has none to deliver, so the channel closes at Close. The channel also closes when the
// connection is closed whole, by Destroy or by Close after the peer ended, and when the peer tears a UDX stream
// down, even while a message waits for a reader. It is never closed when Handshake fails, so read it only after
// Handshake succeeds.
func (s *Stream) Messages() <-chan []byte {
	return s.msgs
}

// receiveMessages opens the peer's unordered messages and delivers them on msgs. It runs once, after a
// successful Handshake, and ends when the connection's Messages channel closes, or when the connection is
// closed whole, even while it waits for a reader to take a message. A UDX connection also ends it when the
// peer tears the stream down, which closes its Done channel. A message that a reader is ready to take is
// delivered first, even when the teardown is already seen; only a message that waits with no reader is dropped
// at teardown. Messages keep arriving after Close, which ends only this side's write side on UDX, as upstream's
// end does.
func (s *Stream) receiveMessages() {
	defer close(s.loopEnded)
	defer close(s.msgs)
	r, ok := s.conn.(unorderedReceiver)
	if !ok {
		<-s.done
		return
	}
	// A connection without Done gives a nil channel here, which the select below never picks.
	var connDone <-chan struct{}
	if d, ok := s.conn.(doneSignaler); ok {
		connDone = d.Done()
	}
	for m := range r.Messages() {
		if plain, ok := openUnordered(s.recvKey, m); ok {
			// The blocking select below picks at random when a reader and a seen teardown are both ready, so a
			// reader that is already parked gets the message here first.
			select {
			case s.msgs <- plain:
				continue
			default:
			}
			select {
			case s.msgs <- plain:
			case <-s.torn:
				return
			case <-connDone:
				return
			}
		}
	}
}

// openUnordered opens one unordered message: an 8-byte counter, which is the start of the 24-byte nonce whose
// rest is zero, then the secret box. It returns false for a message too short to hold an envelope or one that
// does not open.
func openUnordered(key [32]byte, msg []byte) ([]byte, bool) {
	if len(msg) < 8+secretbox.Overhead {
		return nil, false
	}
	var nonce [24]byte
	copy(nonce[:8], msg[:8])
	return secretbox.Open(nil, msg[8:], &nonce, &key)
}

// nsInitiator, nsResponder and nsSend are the hyperswarm/secret-stream namespaces. They are derived
// as hypercore-crypto's namespace does: BLAKE2b-256 of the name, then of that digest and a one-byte index.
var nsInitiator, nsResponder, nsSend = namespaces("hyperswarm/secret-stream")

func namespaces(name string) (initiator, responder, send [32]byte) {
	base := blake2b.Sum256([]byte(name))
	at := func(i byte) [32]byte { return blake2b.Sum256(append(base[:], i)) }
	return at(0), at(1), at(2)
}

// roles returns this side's namespace and the peer's.
func roles(isInitiator bool) (me, them [32]byte) {
	if isInitiator {
		return nsInitiator, nsResponder
	}
	return nsResponder, nsInitiator
}

// unorderedKeys returns the key that seals this side's unordered messages and the key that opens the
// peer's. The peer's send key is this side's receive key.
func unorderedKeys(hash [64]byte, isInitiator bool) (send, recv [32]byte) {
	me, them := roles(isInitiator)
	return keyedHash(hash, me, nsSend), keyedHash(hash, them, nsSend)
}

// keyedHash is BLAKE2b-256 keyed with key over the parts, as libsodium's generichash computes it.
func keyedHash(key [64]byte, parts ...[32]byte) [32]byte {
	h, _ := blake2b.New256(key[:]) // a 64-byte key is always valid
	for _, p := range parts {
		h.Write(p[:])
	}
	var out [32]byte
	h.Sum(out[:0])
	return out
}

// writeFrame writes payload behind its 3-byte little-endian length, in one Write.
func writeFrame(w io.Writer, payload []byte) error {
	if len(payload) > maxFrame {
		return errFrameTooLarge
	}
	n := len(payload)
	buf := append([]byte{byte(n), byte(n >> 8), byte(n >> 16)}, payload...)
	_, err := w.Write(buf)
	return err
}

// frameStart is the first allocation for a frame's payload. A longer payload then doubles as its bytes arrive.
const frameStart = 64 << 10

// readFrame reads one length-prefixed frame whose payload is at most limit bytes. A longer frame fails on its
// prefix, before any payload byte is read or allocated. The payload's memory grows only as its bytes arrive, so
// a peer that names a long frame and sends little costs the reader about what it sent. It returns io.EOF only at
// a frame boundary.
func readFrame(r io.Reader, limit int) ([]byte, error) {
	var hdr [3]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := int(hdr[0]) | int(hdr[1])<<8 | int(hdr[2])<<16
	if n > limit {
		return nil, errFrameTooLarge
	}
	payload := make([]byte, 0, min(n, frameStart))
	for len(payload) < n {
		if len(payload) == cap(payload) {
			// Double the capacity, never past n. Past the first chunk, the buffer stays within twice the bytes received.
			grown := make([]byte, len(payload), min(n, 2*cap(payload)))
			copy(grown, payload)
			payload = grown
		}
		end := cap(payload)
		if _, err := io.ReadFull(r, payload[len(payload):end]); err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return nil, err
		}
		payload = payload[:end]
	}
	return payload, nil
}
