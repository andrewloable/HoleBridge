// Package protomux is the pears-go port of Protomux 3.12.1: channels multiplexed over one stream,
// each with a protocol name, an optional ID, a handshake, and numbered messages.
//
// Ported from protomux 3.12.1 (index.js), MIT License, Copyright (c) 2021 Mathias Buus. The
// stream carries one protomux frame per Write, and each Read returns one whole frame, as the
// Noise secret stream gives upstream.
//
// A received batch of several messages is answered as one batch, as upstream corks it while the
// handlers reply. A batch that reaches maxBatch is sent and a new one started, and a frame longer than
// maxFrame is refused. Not ported: channel aliases, and channels with unique set to false.
package protomux

import (
	"encoding/hex"
	"errors"
	"io"
	"sync"

	"github.com/andrewloable/HoleBridge/pears/compact"
)

// maxBatch is the batch size at which upstream starts a new batch (MAX_BATCH, 8 MiB). maxFrame is the
// largest frame upstream writes: the secret stream writes a frame atomically up to 2^24 - 1 bytes
// (MAX_ATOMIC_WRITE). readBufSize holds one whole frame, so a Read returns a frame in one piece; a frame
// longer than readBufSize fails the stream.
const (
	maxBatch    = 8 << 20
	maxFrame    = 1<<24 - 1
	readBufSize = maxFrame
)

// Control message types. They are sent with remote id 0, upstream's control session.
const (
	ctlBatch  = 0
	ctlOpen   = 1
	ctlReject = 2
	ctlClose  = 3
)

var (
	errFrame         = errors.New("protomux: malformed frame")
	errInvalidOpen   = errors.New("protomux: invalid open message")
	errInvalidReject = errors.New("protomux: invalid reject message")
	errClosed        = errors.New("protomux: channel closed")
	errNotOpen       = errors.New("protomux: channel not open")
	errAlreadyOpen   = errors.New("protomux: channel already open")
	errNotCorked     = errors.New("protomux: uncork without cork")
	errFrameSize     = errors.New("protomux: frame over the atomic write limit")
)

// Mux multiplexes channels over one stream.
type Mux struct {
	stream io.ReadWriteCloser

	// wmu serializes stream writes and guards corked and the batch. It is taken before mu. The batch is
	// encoded as its messages arrive: batch holds the frame so far, batchN its messages, batchLocal the
	// local id of the last one.
	wmu        sync.Mutex
	corked     int
	batch      compact.Encoder
	batchN     int
	batchLocal int

	// mu guards the fields below. It is never held during a stream write or a callback.
	mu     sync.Mutex
	closed bool
	local  []*Channel // index is local id - 1; nil when free or closed
	free   []int      // local indexes free for reuse
	remote []*Channel // index is remote id - 1; nil when unused
	infos  map[string]*keyInfo
	notify map[string]func() // Pair callbacks by key
}

// keyInfo tracks the channels of one protocol and ID.
type keyInfo struct {
	opened   int   // channels open locally with this key
	outgoing []int // local ids opened here, waiting for the remote open
	incoming []int // remote ids waiting for a Pair callback to create their channel
}

// ChannelOptions describes a channel. Protocol and ID name it: a remote channel is matched to a
// local one by the same pair. Handshakes are raw bytes: Open sends the bytes it is given, and
// OnOpen receives the bytes the remote side sent. OnClose gets isRemote true when the remote side
// closed the channel, false when this side did.
type ChannelOptions struct {
	Protocol string
	ID       []byte
	OnOpen   func(handshake []byte)
	OnClose  func(isRemote bool)
}

// Channel is one channel on a Mux.
type Channel struct {
	m        *Mux
	protocol string
	id       []byte
	key      string
	onOpen   func([]byte)
	onClose  func(bool)
	messages []*Message

	localID  int // 0 until Open
	remoteID int // 0 until the remote open is matched to this channel
	closed   bool
}

// Message is one message type of a Channel. Its index is the order of AddMessage calls.
type Message struct {
	c         *Channel
	index     int
	onMessage func([]byte)
}

// New starts a Mux on stream.
func New(stream io.ReadWriteCloser) *Mux {
	m := &Mux{stream: stream, infos: map[string]*keyInfo{}, notify: map[string]func(){}}
	go m.readLoop()
	return m
}

// CreateChannel makes a local channel. It returns nil when the stream is closed.
func (m *Mux) CreateChannel(opts ChannelOptions) *Channel {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	key := toKey(opts.Protocol, opts.ID)
	inf := m.infos[key]
	if inf != nil && inf.opened > 0 {
		return nil
	}
	c := &Channel{m: m, protocol: opts.Protocol, id: opts.ID, key: key, onOpen: opts.OnOpen, onClose: opts.OnClose}
	if inf != nil && len(inf.incoming) > 0 {
		// A remote open is waiting in its Pair callback, and this channel takes it.
		c.remoteID = inf.incoming[0]
		inf.incoming = inf.incoming[1:]
		m.remote[c.remoteID-1] = c
	}
	return c
}

// Pair registers onRemoteOpen for remote opens of protocol and id. The callback creates the
// matching local channel with CreateChannel, as upstream does.
func (m *Mux) Pair(protocol string, id []byte, onRemoteOpen func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notify[toKey(protocol, id)] = onRemoteOpen
}

// Cork starts batching: messages sent until Uncork go out as one frame.
func (m *Mux) Cork() {
	m.wmu.Lock()
	defer m.wmu.Unlock()
	m.corked++
}

// Uncork ends the batch started by Cork and writes it.
func (m *Mux) Uncork() error {
	m.wmu.Lock()
	defer m.wmu.Unlock()
	if m.corked == 0 {
		return errNotCorked
	}
	m.corked--
	if m.corked > 0 {
		return nil
	}
	return m.flushBatch()
}

// AddMessage adds the next message type to the channel. onMessage receives each payload.
func (c *Channel) AddMessage(onMessage func([]byte)) *Message {
	m := c.m
	m.mu.Lock()
	defer m.mu.Unlock()
	msg := &Message{c: c, index: len(c.messages), onMessage: onMessage}
	c.messages = append(c.messages, msg)
	return msg
}

// Open sends the open frame with handshake.
func (c *Channel) Open(handshake []byte) error {
	m := c.m
	m.wmu.Lock()
	defer m.wmu.Unlock()

	m.mu.Lock()
	if c.closed {
		m.mu.Unlock()
		return errClosed
	}
	if c.localID != 0 {
		m.mu.Unlock()
		return errAlreadyOpen
	}
	var id int
	if n := len(m.free); n > 0 {
		id = m.free[n-1]
		m.free = m.free[:n-1]
	} else {
		m.local = append(m.local, nil)
		id = len(m.local) - 1
	}
	localID := id + 1
	c.localID = localID
	m.local[id] = c
	inf := m.info(c.key)
	inf.opened++
	if c.remoteID == 0 {
		inf.outgoing = append(inf.outgoing, localID)
	}
	m.mu.Unlock()

	var e compact.Encoder
	e.Uint(ctlOpen)
	e.Uint(uint64(localID))
	e.String(c.protocol)
	if len(c.id) == 0 {
		e.Uint(0)
	} else {
		e.Buffer(c.id)
	}
	e.Raw(handshake)
	return m.emitLocked(0, e.Bytes())
}

// Close closes the channel and tells the remote side.
func (c *Channel) Close() error {
	m := c.m
	m.wmu.Lock()
	m.mu.Lock()
	if c.closed {
		m.mu.Unlock()
		m.wmu.Unlock()
		return nil
	}
	localID := c.localID
	onClose := c.shutLocked(false)
	m.mu.Unlock()

	var err error
	if localID != 0 {
		var e compact.Encoder
		e.Uint(ctlClose)
		e.Uint(uint64(localID))
		err = m.emitLocked(0, e.Bytes())
	}
	m.wmu.Unlock()
	if onClose != nil {
		onClose()
	}
	return err
}

// Send sends payload on the message's index.
func (msg *Message) Send(payload []byte) error {
	c, m := msg.c, msg.c.m
	m.wmu.Lock()
	defer m.wmu.Unlock()

	m.mu.Lock()
	closed, localID := c.closed, c.localID
	m.mu.Unlock()
	if closed {
		return errClosed
	}
	if localID == 0 {
		return errNotOpen
	}
	var e compact.Encoder
	e.Uint(uint64(msg.index))
	e.Raw(payload)
	return m.emitLocked(localID, e.Bytes())
}

// readLoop handles frames until the stream fails, then closes every local channel.
func (m *Mux) readLoop() {
	buf := make([]byte, readBufSize)
	for {
		n, err := m.stream.Read(buf)
		if err == nil && n > 0 {
			// Copy the frame: handlers keep the handshakes and payloads they are given.
			err = m.receive(append([]byte(nil), buf[:n]...))
		}
		if err != nil {
			m.stream.Close()
			m.shutdown()
			return
		}
	}
}

// receive handles one frame: a remote id, then the rest.
func (m *Mux) receive(frame []byte) error {
	remoteID, body, err := readUint(frame)
	if err != nil {
		return err
	}
	return m.decode(remoteID, body)
}

// decode handles a type and the rest after a remote id. Remote id 0 is the control session.
func (m *Mux) decode(remoteID uint64, b []byte) error {
	typ, b, err := readUint(b)
	if err != nil {
		return err
	}
	if remoteID == 0 {
		return m.control(typ, b)
	}
	m.message(remoteID, typ, b)
	return nil
}

func (m *Mux) control(typ uint64, b []byte) error {
	switch typ {
	case ctlBatch:
		return m.readBatch(b)
	case ctlOpen:
		return m.readOpen(b)
	case ctlReject:
		return m.readReject(b)
	case ctlClose:
		return m.readClose(b)
	}
	return nil // unknown control types are ignored, as upstream does
}

// readBatch handles a batch: a remote id, then length-prefixed frames. A zero length is followed
// by a new remote id. As upstream does, a batch of several messages is corked while its messages are
// handled, so the replies they make go out as one batch.
func (m *Mux) readBatch(b []byte) (err error) {
	remoteID, b, err := readUint(b)
	if err != nil {
		return err
	}
	corked := false
	defer func() {
		if corked {
			if uerr := m.Uncork(); err == nil {
				err = uerr
			}
		}
	}()
	for len(b) > 0 {
		var n uint64
		if n, b, err = readUint(b); err != nil {
			return err
		}
		if n == 0 {
			if remoteID, b, err = readUint(b); err != nil {
				return err
			}
			continue
		}
		if n > uint64(len(b)) {
			return errFrame
		}
		if !corked && uint64(len(b)) > n {
			m.Cork()
			corked = true
		}
		if err := m.decode(remoteID, b[:n]); err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}

// readOpen handles a remote open: its remote id, protocol, ID and handshake.
func (m *Mux) readOpen(b []byte) error {
	remoteID, b, err := readUint(b)
	if err != nil {
		return err
	}
	protocol, b, err := readBytes(b)
	if err != nil {
		return err
	}
	id, handshake, err := readBytes(b)
	if err != nil {
		return err
	}
	if remoteID == 0 {
		return m.rejectRemote(0)
	}

	m.mu.Lock()
	if remoteID == uint64(len(m.remote))+1 {
		m.remote = append(m.remote, nil)
	}
	if remoteID > uint64(len(m.remote)) || m.remote[remoteID-1] != nil {
		m.mu.Unlock()
		return errInvalidOpen
	}
	rid := int(remoteID)
	key := toKey(string(protocol), id)
	inf := m.info(key)
	if len(inf.outgoing) > 0 {
		// Both sides opened at once: this open answers the first local open with this key.
		localID := inf.outgoing[0]
		inf.outgoing = inf.outgoing[1:]
		c := m.local[localID-1]
		if c == nil {
			// The channel closed before its open was answered.
			m.free = append(m.free, localID-1)
			m.mu.Unlock()
			return nil
		}
		c.remoteID = rid
		m.remote[rid-1] = c
		m.mu.Unlock()
		c.fireOpen(handshake)
		return nil
	}

	inf.incoming = append(inf.incoming, rid)
	notify := m.notify[key]
	if notify == nil {
		notify = m.notify[toKey(string(protocol), nil)]
	}
	m.mu.Unlock()

	// The Pair callback creates the channel, which takes this open through CreateChannel.
	if notify != nil {
		notify()
	}

	m.mu.Lock()
	unclaimed := inf.incoming
	inf.incoming = nil
	c := m.remote[rid-1]
	m.gc(key)
	m.mu.Unlock()
	for _, r := range unclaimed {
		if err := m.rejectRemote(uint64(r)); err != nil {
			return err
		}
	}
	if c != nil {
		c.fireOpen(handshake)
	}
	return nil
}

// rejectRemote refuses the remote open with remote id rid. It must not be called with mu held.
func (m *Mux) rejectRemote(rid uint64) error {
	var e compact.Encoder
	e.Uint(ctlReject)
	e.Uint(rid)
	m.wmu.Lock()
	defer m.wmu.Unlock()
	return m.emitLocked(0, e.Bytes())
}

// readReject handles the remote refusing our open with local id localID.
func (m *Mux) readReject(b []byte) error {
	localID, _, err := readUint(b)
	if err != nil {
		return err
	}
	m.mu.Lock()
	for key, inf := range m.infos {
		for i, id := range inf.outgoing {
			if uint64(id) != localID {
				continue
			}
			inf.outgoing = append(inf.outgoing[:i], inf.outgoing[i+1:]...)
			m.free = append(m.free, id-1)
			var onClose func()
			if c := m.local[id-1]; c != nil {
				onClose = c.shutLocked(true)
			}
			m.gc(key)
			m.mu.Unlock()
			if onClose != nil {
				onClose()
			}
			return nil
		}
	}
	m.mu.Unlock()
	return errInvalidReject
}

// readClose handles the remote closing its channel with remote id rid.
func (m *Mux) readClose(b []byte) error {
	rid, _, err := readUint(b)
	if err != nil {
		return err
	}
	var onClose func()
	m.mu.Lock()
	if rid > 0 && rid <= uint64(len(m.remote)) {
		if c := m.remote[rid-1]; c != nil {
			onClose = c.shutLocked(true)
		}
	}
	m.mu.Unlock()
	if onClose != nil {
		onClose()
	}
	return nil
}

// message hands a payload from remote id rid to the handler of message typ. Messages for an
// unknown remote id or index are ignored, and the channel stays open, as upstream does.
func (m *Mux) message(rid, typ uint64, payload []byte) {
	var h func([]byte)
	m.mu.Lock()
	if rid > 0 && rid <= uint64(len(m.remote)) {
		if c := m.remote[rid-1]; c != nil && typ < uint64(len(c.messages)) {
			h = c.messages[typ].onMessage
		}
	}
	m.mu.Unlock()
	if h != nil {
		h(payload)
	}
}

// shutdown runs when the stream fails: every local channel closes with isRemote true, as upstream
// does when its stream closes.
func (m *Mux) shutdown() {
	m.mu.Lock()
	m.closed = true
	var onClose []func()
	for _, c := range m.local {
		if c == nil {
			continue
		}
		if f := c.shutLocked(true); f != nil {
			onClose = append(onClose, f)
		}
	}
	m.mu.Unlock()
	for _, f := range onClose {
		f()
	}
}

// emitLocked sends body as a frame for localID: the local id, then body. While corked it queues
// the body for the batch instead. Control frames use localID 0. The caller holds wmu.
func (m *Mux) emitLocked(localID int, body []byte) error {
	if m.corked > 0 {
		// As upstream's _pushBatch does, a batch that has reached maxBatch is sent before the next message joins.
		if m.batchN > 0 && len(m.batch.Bytes()) >= maxBatch {
			if err := m.flushBatch(); err != nil {
				return err
			}
		}
		if m.batchN == 0 {
			m.batch = compact.Encoder{}
			m.batch.Uint(0) // the control session
			m.batch.Uint(ctlBatch)
			m.batch.Uint(uint64(localID))
		} else if localID != m.batchLocal {
			m.batch.Uint(0)
			m.batch.Uint(uint64(localID))
		}
		m.batch.Buffer(body)
		m.batchN++
		m.batchLocal = localID
		return nil
	}
	var e compact.Encoder
	e.Uint(uint64(localID))
	e.Raw(body)
	return m.writeFrame(e.Bytes())
}

// writeFrame writes one frame to the stream. A frame over maxFrame is refused, as upstream's secret stream
// refuses a write that is not atomic. The caller holds wmu.
func (m *Mux) writeFrame(frame []byte) error {
	if len(frame) > maxFrame {
		return errFrameSize
	}
	_, err := m.stream.Write(frame)
	return err
}

// flushBatch writes the batch being built, if it holds a message, and starts an empty one. The caller holds wmu.
func (m *Mux) flushBatch() error {
	if m.batchN == 0 {
		return nil
	}
	frame := m.batch.Bytes()
	m.batch = compact.Encoder{}
	m.batchN = 0
	return m.writeFrame(frame)
}

// shutLocked marks c closed, frees its ids and drops it from the remote table. The caller holds mu.
// It returns the OnClose call to make after mu is released, or nil when c was already closed.
func (c *Channel) shutLocked(isRemote bool) func() {
	if c.closed {
		return nil
	}
	m := c.m
	c.closed = true
	if c.remoteID > 0 {
		m.remote[c.remoteID-1] = nil
		if c.localID > 0 {
			m.free = append(m.free, c.localID-1)
		}
		c.remoteID = 0
	}
	if c.localID > 0 {
		m.infos[c.key].opened--
		m.local[c.localID-1] = nil
		c.localID = 0
	}
	m.gc(c.key)
	return func() {
		if c.onClose != nil {
			c.onClose(isRemote)
		}
	}
}

// fireOpen runs OnOpen with the remote handshake. It is called without locks.
func (c *Channel) fireOpen(handshake []byte) {
	if c.onOpen != nil {
		c.onOpen(handshake)
	}
}

// info returns the keyInfo for key, creating it. The caller holds mu.
func (m *Mux) info(key string) *keyInfo {
	inf := m.infos[key]
	if inf == nil {
		inf = &keyInfo{}
		m.infos[key] = inf
	}
	return inf
}

// gc drops the keyInfo for key once nothing refers to it. The caller holds mu.
func (m *Mux) gc(key string) {
	if inf := m.infos[key]; inf != nil && inf.opened == 0 && len(inf.outgoing) == 0 && len(inf.incoming) == 0 {
		delete(m.infos, key)
	}
}

// toKey names a protocol and ID as upstream does: no ID and an empty ID are the same key.
func toKey(protocol string, id []byte) string {
	return protocol + "##" + hex.EncodeToString(id)
}

// readUint reads a compact-encoding uint from the front of b and returns it with the rest of b.
// The compact.Decoder cannot return the unread rest, which the handshake needs, so frames are
// read here.
func readUint(b []byte) (uint64, []byte, error) {
	if len(b) == 0 {
		return 0, nil, errFrame
	}
	var n int
	switch b[0] {
	case 0xfd:
		n = 2
	case 0xfe:
		n = 4
	case 0xff:
		n = 8
	default:
		return uint64(b[0]), b[1:], nil
	}
	if len(b) < 1+n {
		return 0, nil, errFrame
	}
	var v uint64
	for i := n; i >= 1; i-- {
		v = v<<8 | uint64(b[i])
	}
	return v, b[1+n:], nil
}

// readBytes reads a length-prefixed buffer from the front of b and returns it with the rest of b.
func readBytes(b []byte) ([]byte, []byte, error) {
	n, b, err := readUint(b)
	if err != nil {
		return nil, nil, err
	}
	if n > uint64(len(b)) {
		return nil, nil, errFrame
	}
	return b[:n], b[n:], nil
}
