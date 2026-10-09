// Package mux is the host's stream multiplexer for protocol v1: the streams of one session, their
// credit flow control, their close handshake, their limits and the receive budget (docs/architecture.md,
// "Flow control", "Limits" and "Malformed input"). Stream resume, which moves streams between sessions,
// is in resume.go.
//
// A session is fed the decoded messages of its channel with Receive and sends its own through its
// Sender. Receive must be called from one goroutine, in arrival order.
package mux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andrewloable/HoleBridge/internal/protocol"
)

// Role is the side of a session. The host answers streams; the app opens them.
type Role int

// The two roles.
const (
	RoleHost Role = iota
	RoleApp
)

// Sender carries one protocol message to the other side. The messages are the protocol structs,
// such as protocol.Open, protocol.Opened and protocol.Data.
type Sender interface{ Send(msg any) error }

// AcceptResult is what the host's accept callback returns for a service. Code and Reason reject the
// stream with one of the reject codes of docs/architecture.md (1 unknown service, 2 limit reached,
// 3 target refused, 4 target timed out). Code 0 accepts it. An accepted stream is passed to Target,
// if set, on its own goroutine: Target connects the stream to the service and reads and writes it.
type AcceptResult struct {
	Target func(st *Stream)
	Code   uint64
	Reason string
}

// RejectError is what Open returns when the host rejects the stream.
type RejectError struct {
	Code   uint64
	Reason string
}

func (e *RejectError) Error() string {
	return fmt.Sprintf("stream rejected (code %d): %s", e.Code, e.Reason)
}

// Counter is the stream count shared by the sessions of a process (Config.Limits). It admits a fixed
// number of streams at once.
type Counter struct {
	mu    sync.Mutex
	limit int
	used  int
}

// NewCounter returns a counter that admits limit streams at once.
func NewCounter(limit int) *Counter {
	return &Counter{limit: limit}
}

// take claims a slot for a stream and reports whether one was free.
func (c *Counter) take() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.used >= c.limit {
		return false
	}
	c.used++
	return true
}

// give returns a slot that take claimed.
func (c *Counter) give() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.used > 0 {
		c.used--
	}
}

// Config is the per-session configuration. Window is the credit granted per stream (2 MiB by
// default); MaxStreams is the streams per session (128 by default, zero for no session limit); Limits
// is the stream count shared by the process (1024 by default, nil for none); Clock is the time source
// (nil for time.Now).
type Config struct {
	Window     uint64
	MaxStreams int
	Limits     *Counter
	Clock      func() time.Time
}

const (
	// maxChunk is the largest data payload a data message carries: the wire limit of 65536 bytes. Its
	// frame, with the channel and stream ids, is about 65544 bytes, which pears/protomux reads whole (its
	// readBufSize fits the largest upstream frame).
	maxChunk = 64 << 10

	// closedGrace is how long the id of a finished or refused stream is remembered. Messages that raced
	// its close (window, close, opened, reject) are ignored in that time instead of ending the session.
	closedGrace = 60 * time.Second

	// codeLimit is the reject code for a stream refused by the session's or the process's limit.
	codeLimit = 2
)

var (
	// errReset is what a stream's reader and writer get when the stream is reset.
	errReset = errors.New("mux: stream reset")
	// errStreamClosed is what a stream's reader and writer get after the local side closed it.
	errStreamClosed = errors.New("mux: stream closed")
	// errSessionClosed is what the streams of a closed session get, and what a closed session refuses.
	errSessionClosed = errors.New("mux: session closed")
	// errKept is what a writer gets when its stream would keep more unacknowledged bytes than the caps allow.
	errKept = errors.New("mux: too many unacknowledged bytes kept for resume")
)

// Session is one protocol v1 session: its streams, their credit and their share of the budget.
type Session struct {
	role   Role
	send   Sender
	cfg    Config
	budget *Budget
	accept func(service string) AcceptResult

	mu      sync.Mutex
	changed *sync.Cond           // signalled when a stream's data, credit or state changes
	streams map[uint64]*Stream   // the streams the session still holds
	gone    map[uint64]time.Time // ids finished or refused, and when
	slots   int                  // streams holding a slot of this session
	closed  bool
	lastID  uint64 // the app's last stream id

	resumable bool         // stream resume is on: opened carries a token, and streams keep the bytes they sent
	detached  bool         // the transport died: the streams wait for a reattach
	tab       *ResumeTable // the host's resume table, when the session is bound to one
}

// NewSession returns a session in role. send carries its messages to the other side, cfg sets its
// limits, budget is the process budget it draws grants from, and accept decides, for the host,
// whether a service can be opened.
func NewSession(role Role, send Sender, cfg Config, budget *Budget, accept func(service string) AcceptResult) *Session {
	s := &Session{role: role, send: send, cfg: cfg, budget: budget, accept: accept,
		streams: map[uint64]*Stream{}, gone: map[uint64]time.Time{}}
	s.changed = sync.NewCond(&s.mu)
	return s
}

// Receive feeds one decoded message to the session. A returned error means the session must
// close. A fault on one stream resets that stream and returns nil.
func (s *Session) Receive(msg any) error {
	switch m := msg.(type) {
	case protocol.Open:
		return s.onOpen(m)
	case protocol.Opened:
		return s.onOpened(m)
	case protocol.Reject:
		return s.onReject(m)
	case protocol.Data:
		return s.onData(m)
	case protocol.Window:
		return s.onWindow(m)
	case protocol.Close:
		return s.onClose(m)
	case protocol.Reattach:
		return s.onReattach(m)
	case protocol.Reattached:
		return s.onReattached(m)
	}
	return errNotImplemented(fmt.Sprintf("Session.Receive(%T)", msg))
}

func (s *Session) onOpen(m protocol.Open) error {
	if s.role != RoleHost {
		return errors.New("mux: the app side got an open message")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errSessionClosed
	}
	if s.known(m.Stream) {
		s.mu.Unlock()
		return fmt.Errorf("mux: open for stream %d, which is already open", m.Stream)
	}
	if !s.takeSlot() {
		s.tombstone(m.Stream)
		s.mu.Unlock()
		return s.emit(protocol.Reject{Stream: m.Stream, Code: codeLimit, Reason: "limit reached"})
	}
	s.mu.Unlock()
	// The accept callback may block (a dial to the service), so it runs without the lock. The slot is
	// held meanwhile.
	res := s.accept(m.Service)
	s.mu.Lock()
	if s.closed {
		s.giveSlot()
		s.mu.Unlock()
		return errSessionClosed
	}
	if res.Code != 0 {
		s.giveSlot()
		s.tombstone(m.Stream)
		s.mu.Unlock()
		return s.emit(protocol.Reject{Stream: m.Stream, Code: res.Code, Reason: res.Reason})
	}
	g := s.budget.admit(s.cfg.Window)
	st := &Stream{id: m.Stream, live: true, slot: true, limit: g, sendLimit: m.Window, failed: make(chan struct{})}
	st.owner.Store(s)
	if s.resumable {
		st.token = newToken()
		s.tab.track(st)
	}
	s.streams[m.Stream] = st
	token := st.token
	s.mu.Unlock()
	err := s.emit(protocol.Opened{Stream: m.Stream, Window: g, Token: token})
	if err == nil {
		// Opened is out, so the budget may top the stream up from now on: no window goes out before it.
		s.mu.Lock()
		w := st.announce()
		s.mu.Unlock()
		if w != nil {
			err = s.emit(*w)
		}
	}
	if err != nil {
		// The app never gets the stream, so it fails here. Its target still runs, since the accept already
		// dialed it: the target ends once the stream has failed, so the dialed connection is closed.
		s.mu.Lock()
		st.fail(err)
		s.mu.Unlock()
	}
	if res.Target != nil {
		go res.Target(st)
	}
	return err
}

func (s *Session) onOpened(m protocol.Opened) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.streams[m.Stream]
	if st == nil {
		if s.recent(m.Stream) {
			return nil // the answer to a cancelled open
		}
		return fmt.Errorf("mux: opened for stream %d, which is not waiting", m.Stream)
	}
	if st.err != nil {
		return nil // the stream was reset while opening; the answer came too late
	}
	if !st.pending {
		return fmt.Errorf("mux: opened for stream %d, which is not waiting", m.Stream)
	}
	st.sendLimit = m.Window
	st.token = m.Token
	st.settle()
	s.changed.Broadcast()
	return nil
}

func (s *Session) onReject(m protocol.Reject) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.streams[m.Stream]
	if st == nil {
		if s.recent(m.Stream) {
			return nil // the answer to a cancelled open
		}
		return fmt.Errorf("mux: reject for stream %d, which is not waiting", m.Stream)
	}
	if st.err != nil {
		return nil // the stream was reset while opening; the answer came too late
	}
	if !st.pending {
		return fmt.Errorf("mux: reject for stream %d, which is not waiting", m.Stream)
	}
	st.fail(&RejectError{Code: m.Code, Reason: m.Reason})
	s.forget(st)
	return nil
}

func (s *Session) onData(m protocol.Data) error {
	s.mu.Lock()
	st := s.streams[m.Stream]
	if st == nil {
		recent := s.recent(m.Stream)
		s.mu.Unlock()
		if recent {
			return nil
		}
		return fmt.Errorf("mux: data for stream %d, which was never opened", m.Stream)
	}
	if st.err != nil {
		s.mu.Unlock()
		return nil
	}
	if st.reattaching {
		// Data sent before the peer knew of the reattach. The peer resends what it did not receive.
		s.mu.Unlock()
		return nil
	}
	if st.pending || st.rclosed || st.received+uint64(len(m.Payload)) > st.limit {
		// Data before opened, data after the peer's close, or data past the credit: the stream is
		// reset. The protocol has no reset message, so the peer is told with close.
		st.fail(errReset)
		send := st.finishWrite()
		s.mu.Unlock()
		if send {
			return s.emit(protocol.Close{Stream: m.Stream})
		}
		return nil
	}
	st.received += uint64(len(m.Payload))
	st.buf.Write(m.Payload)
	s.changed.Broadcast()
	s.mu.Unlock()
	return nil
}

func (s *Session) onWindow(m protocol.Window) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.streams[m.Stream]
	if st == nil {
		if s.recent(m.Stream) {
			return nil
		}
		return fmt.Errorf("mux: window for stream %d, which was never opened", m.Stream)
	}
	if st.err == nil && !st.reattaching {
		st.ack(m.Received)
		st.sendLimit += m.Credit
		s.changed.Broadcast()
	}
	return nil
}

// onClose handles the peer's close. It ends the local read side: the local side reads the bytes the peer
// sent, then io.EOF. Our own close is not sent until the local side closes or CloseWrites, so the local side
// can still write after the peer has closed (half-close). A close for a stream that never opened resets it,
// and is answered at once. A second close for a stream is ignored: the peer sends it again after a reattach,
// in case the first was lost.
func (s *Session) onClose(m protocol.Close) error {
	s.mu.Lock()
	st := s.streams[m.Stream]
	if st == nil {
		recent := s.recent(m.Stream)
		s.mu.Unlock()
		if recent {
			return nil
		}
		return fmt.Errorf("mux: close for stream %d, which is not open", m.Stream)
	}
	if st.rclosed {
		s.mu.Unlock()
		return nil
	}
	st.rclosed = true
	var send bool
	if st.pending {
		st.fail(errReset) // the peer ended the stream before opened
		send = st.finishWrite()
	} else {
		// A close in place of reattached refuses the reattach. Our own close, held back for it, is owed now.
		send = st.reattaching && st.wclosed
		st.reattaching = false
		if st.wclosed {
			s.complete(st)
		}
	}
	s.changed.Broadcast()
	s.mu.Unlock()
	if send {
		return s.emit(protocol.Close{Stream: m.Stream})
	}
	return nil
}

// Open opens a stream to service and waits for the host's answer. Only the app role opens streams.
// A stream the session or the process has no room for is refused with a RejectError of code 2
// without being sent. A cancelled open gives its slot back and sends close, so the host frees its side.
func (s *Session) Open(ctx context.Context, service string) (*Stream, error) {
	if s.role != RoleApp {
		return nil, errors.New("mux: only the app side opens streams")
	}
	s.mu.Lock()
	if s.closed || s.detached {
		s.mu.Unlock()
		return nil, errSessionClosed
	}
	if !s.takeSlot() {
		s.mu.Unlock()
		return nil, &RejectError{Code: codeLimit, Reason: "limit reached"}
	}
	s.lastID++
	g := s.budget.admit(s.cfg.Window)
	st := &Stream{id: s.lastID, live: true, slot: true, limit: g, pending: true, ready: make(chan struct{}),
		failed: make(chan struct{})}
	st.owner.Store(s)
	s.streams[st.id] = st
	s.mu.Unlock()
	if err := s.send.Send(protocol.Open{Stream: st.id, Service: service, Window: g}); err != nil {
		s.mu.Lock()
		st.fail(err)
		s.forget(st)
		s.mu.Unlock()
		_ = s.lost(err)
		return nil, err
	}
	// Open is out, so the budget may top the stream up from now on: no window goes out before it.
	s.mu.Lock()
	w := st.announce()
	s.mu.Unlock()
	if w != nil {
		_ = s.emit(*w)
	}
	select {
	case <-st.ready:
	case <-ctx.Done():
		s.mu.Lock()
		cancelled := st.pending
		if cancelled {
			st.fail(ctx.Err())
			s.forget(st)
		}
		s.mu.Unlock()
		if cancelled {
			// The host may already have accepted the stream, so it is told to close. Its late answer is
			// ignored.
			_ = s.emit(protocol.Close{Stream: st.id})
			return nil, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if st.err != nil {
		return nil, st.err
	}
	return st, nil
}

// Close closes the session: every stream's Read and Write fail, and no stream opens any more. It sends
// nothing; the caller closes the channel.
func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeLocked()
	return nil
}

// closeLocked ends the session as Close does. A session that is closed or detached no longer forgets
// its streams: the caller holds s.mu.
func (s *Session) closeLocked() {
	if s.closed {
		return
	}
	s.closed = true
	for _, st := range s.streams {
		st.fail(errSessionClosed)
		if s.tab != nil {
			s.tab.remove(st)
		}
	}
	s.changed.Broadcast()
}

// emit sends msg on the session's transport. On a resumable session a send that fails means the
// transport died: the session detaches, and the message is covered by the resend of a reattach, so no
// error is reported. The caller does not hold s.mu.
func (s *Session) emit(msg any) error {
	if err := s.send.Send(msg); err != nil {
		return s.lost(err)
	}
	return nil
}

// lost reports a failed send. A session without resume has lost its streams with the transport, so the
// error goes to the caller; a resumable session detaches instead.
func (s *Session) lost(err error) error {
	if !s.resumable {
		return err
	}
	s.Detach()
	return nil
}

// known reports whether id is a stream the session holds, or one remembered as finished. The caller
// holds s.mu.
func (s *Session) known(id uint64) bool {
	_, ok := s.streams[id]
	return ok || s.recent(id)
}

// recent reports whether id finished or was refused less than closedGrace ago. The caller holds s.mu.
func (s *Session) recent(id uint64) bool {
	t, ok := s.gone[id]
	return ok && s.now().Sub(t) < closedGrace
}

// tombstone remembers id as finished now and forgets the ids that are older than closedGrace. The
// caller holds s.mu.
func (s *Session) tombstone(id uint64) {
	now := s.now()
	for old, t := range s.gone {
		if now.Sub(t) >= closedGrace {
			delete(s.gone, old)
		}
	}
	s.gone[id] = now
}

// forget drops the record of st and remembers its id as finished. The caller holds s.mu.
func (s *Session) forget(st *Stream) {
	delete(s.streams, st.id)
	s.tombstone(st.id)
	if s.tab != nil {
		s.tab.remove(st)
	}
}

// complete ends a stream that both sides have closed. The caller holds s.mu.
func (s *Session) complete(st *Stream) {
	st.release()
	s.forget(st)
}

// takeSlot takes a stream slot: one of the session's MaxStreams and one of the shared Limits. It
// reports false when either is full. The caller holds s.mu.
func (s *Session) takeSlot() bool {
	if s.cfg.MaxStreams > 0 && s.slots >= s.cfg.MaxStreams {
		return false
	}
	if s.cfg.Limits != nil && !s.cfg.Limits.take() {
		return false
	}
	s.slots++
	return true
}

// giveSlot returns a slot that takeSlot took. The caller holds s.mu.
func (s *Session) giveSlot() {
	s.slots--
	if s.cfg.Limits != nil {
		s.cfg.Limits.give()
	}
}

// now is the session's time source.
func (s *Session) now() time.Time {
	if s.cfg.Clock != nil {
		return s.cfg.Clock()
	}
	return time.Now()
}

// Stream is one forwarded connection inside a session.
//
// The receive side has credit: the peer may send up to limit bytes in all, and limit grows only as
// the local reader takes bytes (Read). The send side is limited by the peer's grants (Write). Each
// side closes its write side with close; the stream is kept until both closes have come back.
//
// A stream belongs to one session at a time. Resume moves it to the session of a reattach, so its
// state is read under the lock of the session it is in, which is checked again after each wait.
type Stream struct {
	owner   atomic.Pointer[Session] // the session the stream is in; a reattach moves it
	id      uint64
	wmu     sync.Mutex    // one Write at a time, so the data of a stream stays in order
	pending bool          // the app is waiting for opened or reject
	ready   chan struct{} // closed when pending ends
	live    bool          // counted in the budget
	slot    bool          // holds a slot of the session and of the process
	err     error         // set when the stream is reset, rejected or closed locally
	failed  chan struct{} // closed with err, the first time err is set: see Failed

	limit    uint64       // bytes the peer may send in all: our grants
	received uint64       // bytes received from the peer
	consumed uint64       // bytes the local reader has taken
	buf      bytes.Buffer // received bytes not yet taken

	sendLimit uint64 // bytes we may send in all: the peer's grants
	sent      uint64

	wclosed bool // our close is sent, or the write side is closed locally
	rclosed bool // the peer's close came
	readers int  // Reads waiting for bytes

	token       [16]byte     // the resume token of opened: zero without resume
	kept        bytes.Buffer // bytes sent and not yet acknowledged, kept for a resend
	keptFrom    uint64       // stream offset of the first kept byte
	counted     uint64       // kept bytes counted in the table of the host's resume
	reattaching bool         // a reattach is in flight: writes wait, window and close are held back
	reported    uint64       // the limit that the reattach or reattached message carried
}

// own returns the session the stream is in. It changes only under the locks of both sessions of a move.
func (st *Stream) own() *Session {
	return st.owner.Load()
}

// InSession reports whether the stream is in session s now. A reattach moves a stream to the session that takes
// it over, so a caller that counts streams per session asks this to find the session that owns the stream.
func (st *Stream) InSession(s *Session) bool {
	return st.owner.Load() == s
}

// lock locks the session the stream is in and returns it locked.
func (st *Stream) lock() *Session {
	s := st.own()
	s.mu.Lock()
	return st.relock(s)
}

// relock is called with s locked, after a wait or a lock. If the stream moved to another session meanwhile,
// relock unlocks s and locks that session. It returns the session the stream is in, locked.
func (st *Stream) relock(s *Session) *Session {
	for {
		cur := st.own()
		if cur == s {
			return s
		}
		s.mu.Unlock()
		s = cur
		s.mu.Lock()
	}
}

// extend grants the peer g more bytes of credit and returns the window message to send, or nil when g is 0.
// A reattach in flight holds the window back: finishReattach sends the credit. The stream is kept in the
// budget's starved set while it has no credit for its peer. The caller holds the lock of its session.
func (st *Stream) extend(g uint64) *protocol.Window {
	var w *protocol.Window
	if g > 0 {
		st.limit += g
		if !st.reattaching {
			w = &protocol.Window{Stream: st.id, Credit: g, Received: st.received}
		}
	}
	st.track()
	return w
}

// track keeps the stream in its budget's starved set while it is live and has no credit for its peer. The
// caller holds the lock of its session.
func (st *Stream) track() {
	st.own().budget.track(st, st.live && st.limit == st.consumed)
}

// announce runs once the peer has been sent the stream's opened or open message. A stream with no credit
// for its peer is then topped up, so a top-up never sends a window ahead of the message. A stream that holds
// its first grant is left alone until its reader takes bytes. The caller holds the lock of its session.
func (st *Stream) announce() *protocol.Window {
	if st.limit != st.consumed {
		return nil
	}
	return st.topUp()
}

// topUp grants a stream what its budget allows now, and returns the window message to send, or nil. The budget
// calls it for a starved stream when credit frees, and announce calls it once the peer has the stream's opened
// or open message. The caller holds the lock of the stream's session.
func (st *Stream) topUp() *protocol.Window {
	if !st.live {
		return nil
	}
	s := st.own()
	return st.extend(s.budget.take(sub(s.cfg.Window, st.limit-st.consumed)))
}

// fail resets the stream with err (the first error stays): it drops the bytes not yet read, gives its
// credit and slot back, ends a pending open and wakes the readers and writers. The caller holds the lock
// of the stream's session.
func (st *Stream) fail(err error) {
	if st.err == nil {
		st.err = err
		close(st.failed)
	}
	st.buf.Reset()
	st.release()
	st.settle()
	st.own().changed.Broadcast()
}

// release gives the stream's credit back to the budget, its slot back to the limits and its kept bytes'
// memory back, once. Its kept bytes stay counted in the table until the stream leaves it. The caller holds
// the lock of the stream's session.
func (st *Stream) release() {
	s := st.own()
	st.kept.Reset()
	if st.live {
		st.live = false
		s.budget.track(st, false)
		s.budget.retire(sub(st.limit, st.consumed))
	}
	if st.slot {
		st.slot = false
		s.giveSlot()
	}
}

// moveTo makes dst the session of st, which old holds. Its credit and its slot move with it, and old
// forgets the id, so that late messages for it on old are ignored. The caller holds the locks of old and dst.
func (st *Stream) moveTo(old, dst *Session) {
	old.budget.track(st, false)
	if st.live {
		untaken := sub(st.limit, st.consumed)
		old.budget.retire(untaken)
		dst.budget.reopen(untaken)
	}
	if st.slot {
		old.slots--
		dst.slots++
	}
	delete(old.streams, st.id)
	old.tombstone(st.id)
	dst.streams[st.id] = st
	st.owner.Store(dst)
	st.track()
}

// settle ends a pending open. The caller holds the lock of the stream's session.
func (st *Stream) settle() {
	if st.pending {
		st.pending = false
		close(st.ready)
	}
}

// finishWrite closes the write side. It reports whether the close must be sent now: not while a reattach
// is in flight, since finishReattach sends it after the reattached message. It completes the stream when
// the peer's close has already come. The caller holds the lock of the stream's session.
func (st *Stream) finishWrite() bool {
	if st.wclosed {
		return false
	}
	st.wclosed = true
	s := st.own()
	if st.rclosed {
		s.complete(st)
	}
	s.changed.Broadcast()
	return !st.reattaching
}

// keep appends the bytes of p, which are about to be sent, to the bytes the stream keeps for a resend. It
// reports false when a cap would be exceeded: the stream's own cap, or the table's total. The caller holds
// the lock of the stream's session.
func (st *Stream) keep(s *Session, p []byte) bool {
	perStream := uint64(keepPerStream)
	if s.tab != nil {
		perStream = s.tab.cfg.PerStream
	}
	n := uint64(len(p))
	if uint64(st.kept.Len())+n > perStream {
		return false
	}
	if s.tab != nil {
		if !s.tab.keep(n) {
			return false
		}
		st.counted += n
	}
	st.kept.Write(p)
	return true
}

// ack drops the kept bytes below r, the count of the stream's bytes the peer has received: they are
// acknowledged and will not be resent. The caller holds the lock of the stream's session.
func (st *Stream) ack(r uint64) {
	s := st.own()
	if !s.resumable {
		return
	}
	if r > st.sent {
		r = st.sent
	}
	if r <= st.keptFrom {
		return
	}
	n := r - st.keptFrom
	if n > uint64(st.kept.Len()) {
		n = uint64(st.kept.Len())
	}
	st.kept.Next(int(n))
	st.keptFrom += n
	if s.tab != nil {
		st.counted = sub(st.counted, n)
		s.tab.free(n)
	}
}

// resendData returns the kept bytes as data messages, copied, in order. The caller holds the lock of the
// stream's session.
func (st *Stream) resendData() []any {
	var msgs []any
	b := st.kept.Bytes()
	for len(b) > 0 {
		n := min(len(b), maxChunk)
		msgs = append(msgs, protocol.Data{Stream: st.id, Payload: append([]byte(nil), b[:n]...)})
		b = b[n:]
	}
	return msgs
}

// Read reads the bytes the other side has sent on the stream. It returns io.EOF once the peer has
// closed and the bytes it sent are read. Reading frees credit: the other side is granted as much again
// as the budget allows, up to the stream's window.
func (st *Stream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	s := st.lock()
	st.readers++
	for st.buf.Len() == 0 && st.err == nil && !st.rclosed {
		s.changed.Wait()
		s = st.relock(s)
	}
	st.readers--
	if st.err != nil {
		s.mu.Unlock()
		return 0, st.err
	}
	if st.buf.Len() == 0 {
		s.mu.Unlock()
		return 0, io.EOF
	}
	n, _ := st.buf.Read(p)
	st.consumed += uint64(n)
	var w *protocol.Window
	if st.live {
		outstanding := st.limit - st.consumed
		w = st.extend(s.budget.refill(uint64(n), sub(s.cfg.Window, outstanding)))
	}
	s.mu.Unlock()
	if w != nil {
		if err := s.emit(*w); err != nil {
			return n, err
		}
	}
	return n, nil
}

// Write sends p on the stream. It blocks while the stream has no credit, or while it waits for a
// reattach. On a resumable session the bytes are kept until the peer acknowledges them.
func (st *Stream) Write(p []byte) (int, error) {
	st.wmu.Lock()
	defer st.wmu.Unlock()
	n := 0
	for n < len(p) {
		k, s, err := st.reserve(p[n:])
		if err != nil {
			return n, err
		}
		if err := s.emit(protocol.Data{Stream: st.id, Payload: p[n : n+k]}); err != nil {
			return n, err
		}
		n += k
	}
	return n, nil
}

// reserve waits until the peer has credit, then takes up to len(p) bytes of it, at most one data
// message's worth. It returns the count and the session the bytes go out on. On a resumable session the
// bytes are kept as they are reserved.
func (st *Stream) reserve(p []byte) (int, *Session, error) {
	s := st.lock()
	for st.err == nil && !st.wclosed && (st.reattaching || s.detached || st.sendLimit == st.sent) {
		s.changed.Wait()
		s = st.relock(s)
	}
	if st.err != nil {
		s.mu.Unlock()
		return 0, nil, st.err
	}
	if st.wclosed {
		s.mu.Unlock()
		return 0, nil, errStreamClosed
	}
	k := min(uint64(len(p)), st.sendLimit-st.sent, maxChunk)
	if s.resumable && !st.keep(s, p[:k]) {
		st.fail(errKept)
		send := st.finishWrite()
		s.mu.Unlock()
		if send {
			_ = s.emit(protocol.Close{Stream: st.id})
		}
		return 0, nil, errKept
	}
	st.sent += k
	s.mu.Unlock()
	return int(k), s, nil
}

// Close closes both directions of the stream: reads and writes fail from now on, unread bytes are
// dropped, and the close is sent. The stream is kept until the peer's close comes back.
func (st *Stream) Close() error {
	s := st.lock()
	st.fail(errStreamClosed)
	s.mu.Unlock()
	return st.CloseWrite()
}

// CloseWrite ends the write side of the stream: the peer reads what was written and then io.EOF, and
// the stream can still be read until the peer's close comes back.
func (st *Stream) CloseWrite() error {
	s := st.lock()
	send := st.finishWrite()
	s.mu.Unlock()
	if !send {
		return nil
	}
	return s.emit(protocol.Close{Stream: st.id})
}

// ID returns the stream id, which the app chose.
func (st *Stream) ID() uint64 {
	return st.id
}

// Failed returns a channel that is closed when the stream fails: it is reset, rejected, closed locally, or its
// session closes or its grace period ends. A reader or writer learns that from its own call; Failed lets a
// goroutine that waits on something else, such as a silent target, learn it too.
func (st *Stream) Failed() <-chan struct{} {
	return st.failed
}

// errNotImplemented is what the parts still to come return. It wraps errors.ErrUnsupported.
func errNotImplemented(name string) error {
	return fmt.Errorf("%s: not implemented: %w", name, errors.ErrUnsupported)
}
