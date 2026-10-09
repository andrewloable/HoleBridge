package mux

// Stream resume (docs/architecture.md, "Sessions and reconnects" and "Encoding"): a stream whose transport
// dies keeps its bytes for a grace period, and the app's new session reattaches it with the token the host
// issued in opened. A stream moves to the session that reattaches it, and the moved stream keeps its
// Stream value, so the readers and writers that hold it carry on.

import (
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/andrewloable/HoleBridge/internal/protocol"
)

// ResumeConfig sets the caps and the grace period of a ResumeTable.
type ResumeConfig struct {
	PerStream uint64        // bytes one stream may keep for resending: 4 MiB
	Total     uint64        // bytes all streams of the table may keep: 32 MiB
	Grace     time.Duration // how long a dropped stream waits for its reattach: 60 s
}

// keepPerStream is the cap on the bytes an app session keeps per stream. An app session has no table,
// so this is its only cap: the same 4 MiB as the host's default.
const keepPerStream = 4 << 20

// errGrace is what a stream gets when its grace period ends without a reattach.
var errGrace = errors.New("mux: no reattach within the grace period")

// moveMu serialises the moves of streams between sessions: Adopt, and a host's reattach. A move locks two
// sessions, and this lock is always taken before either, so two moves never wait on each other's sessions.
var moveMu sync.Mutex

// ResumeTable is the host's table of resumable streams, shared by the host's sessions. Every stream of a
// bound session is in it while the session lives. When the session detaches, its streams wait in it for
// the grace period; a reattach takes them over, and the grace period ends them. The table caps the bytes
// its streams keep unacknowledged, in total, so a peer that never acknowledges cannot grow the host.
type ResumeTable struct {
	cfg   ResumeConfig
	clock func() time.Time

	mu      sync.Mutex
	used    uint64             // kept bytes counted over the streams of the table
	streams map[*Stream]*entry // the streams of the table
}

// entry is the table's record of a stream. While its session is detached, held is set and at is when the
// session detached; timer ends the grace period. Every change makes a new entry, so a timer that fires late
// finds its entry gone.
type entry struct {
	held  bool
	at    time.Time
	timer *time.Timer
}

// stop ends the grace timer of e, if it has one.
func (e *entry) stop() {
	if e.timer != nil {
		e.timer.Stop()
	}
}

// NewResumeTable returns a table with the caps and grace of cfg. clock is the time source (nil for time.Now).
func NewResumeTable(cfg ResumeConfig, clock func() time.Time) *ResumeTable {
	if clock == nil {
		clock = time.Now
	}
	return &ResumeTable{cfg: cfg, clock: clock, streams: map[*Stream]*entry{}}
}

// Bind makes host, a host session, resumable through t: its opened messages carry a fresh random token,
// its Detach keeps its streams in t, and the reattach messages it receives are answered from t. A session
// that is not bound has resume off. Bind a session before it opens streams.
func (t *ResumeTable) Bind(host *Session) error {
	if host.role != RoleHost {
		return errors.New("mux: only a host session binds to a resume table")
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if host.tab == t {
		return nil
	}
	if host.tab != nil || host.resumable {
		return errors.New("mux: the session is already resumable")
	}
	if host.closed || len(host.streams) > 0 {
		return errors.New("mux: bind the session before it opens streams")
	}
	host.tab = t
	host.resumable = true
	return nil
}

// EnableResume makes s, an app session, keep its streams on Detach, so that a new session can take them
// over with Adopt. A session that is not enabled closes its streams on Detach.
func (s *Session) EnableResume() error {
	if s.role != RoleApp {
		return errors.New("mux: only an app session enables resume")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resumable {
		return nil
	}
	if s.closed || len(s.streams) > 0 {
		return errors.New("mux: enable resume before the session opens streams")
	}
	s.resumable = true
	return nil
}

// Detach is called when the transport of s died. Its streams stall: Read and Write block, the bytes they
// keep are kept, and the grace period starts. A session without resume closes its streams at once.
func (s *Session) Detach() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.detachLocked()
}

// detachLocked is Detach. The caller holds s.mu.
func (s *Session) detachLocked() {
	if s.closed || s.detached {
		return
	}
	if !s.resumable {
		s.closeLocked()
		return
	}
	s.detached = true
	if s.tab != nil {
		now := s.tab.clock()
		for _, st := range s.streams {
			s.tab.hold(st, now)
		}
	}
	s.changed.Broadcast()
}

// Adopt makes s, a new app session, take over the streams that prev detached, and sends a reattach for
// each one with its token, received and limit. The Stream values stay the same. Adopt detaches prev first
// if its transport is still up. The streams wait for their reattached before their writes go out.
func (s *Session) Adopt(prev *Session) error {
	if s.role != RoleApp || prev.role != RoleApp {
		return errors.New("mux: only app sessions adopt streams")
	}
	if prev == s {
		return errors.New("mux: a session cannot adopt its own streams")
	}
	prev.Detach()

	moveMu.Lock()
	s.mu.Lock()
	prev.mu.Lock()
	if !s.resumable || s.closed || s.detached {
		prev.mu.Unlock()
		s.mu.Unlock()
		moveMu.Unlock()
		return errors.New("mux: the new session must be an open session with resume on")
	}
	var live []*Stream
	for _, st := range prev.streams {
		if st.err == nil {
			live = append(live, st)
		} else {
			prev.forget(st) // a stream that failed is not resumed
		}
	}
	if s.cfg.MaxStreams > 0 && s.slots+len(live) > s.cfg.MaxStreams {
		prev.mu.Unlock()
		s.mu.Unlock()
		moveMu.Unlock()
		return errors.New("mux: the new session has no room for the streams it adopts")
	}
	var msgs []any
	for _, st := range live {
		st.moveTo(prev, s)
		st.reattaching = true
		st.reported = st.limit
		msgs = append(msgs, protocol.Reattach{Stream: st.id, Token: st.token, Received: st.received, Limit: st.limit})
	}
	if prev.lastID > s.lastID {
		s.lastID = prev.lastID
	}
	prev.changed.Broadcast()
	s.changed.Broadcast()
	prev.mu.Unlock()
	s.mu.Unlock()
	moveMu.Unlock()
	for _, msg := range msgs {
		if err := s.emit(msg); err != nil {
			return err
		}
	}
	return nil
}

// newToken returns a fresh random resume token. crypto/rand does not fail since Go 1.24.
func newToken() (t [16]byte) {
	rand.Read(t[:])
	return t
}

// onReattach answers the app's reattach on this host session. The stream is taken over from the table when
// the token is the issued one and the grace period has not ended. Anything else is refused with close, and
// the session stays up.
func (s *Session) onReattach(m protocol.Reattach) error {
	if s.role != RoleHost {
		return errors.New("mux: the app side got a reattach message")
	}
	s.mu.Lock()
	usable := s.tab != nil && !s.closed && !s.detached
	s.mu.Unlock()
	if !usable {
		return s.refuseReattach(m.Stream)
	}
	st := s.tab.find(m.Stream, m.Token)
	if st == nil {
		return s.refuseReattach(m.Stream)
	}
	return s.takeOver(st, m)
}

// takeOver moves st, which the table holds, into this host session and resends its bytes from the offset the
// app received. A stream still attached to an older session is taken from that session, as a reconnect can
// arrive before the host sees the old transport die.
func (s *Session) takeOver(st *Stream, m protocol.Reattach) error {
	moveMu.Lock()
	s.mu.Lock()
	old := st.own()
	if old != s {
		old.mu.Lock()
	}
	at, held, tracked := s.tab.detachedAt(st)
	expired := tracked && held && s.tab.clock().Sub(at) >= s.tab.cfg.Grace
	if expired {
		st.fail(errGrace)
		old.forget(st)
	}
	ok := tracked && !expired && st.err == nil && !st.reattaching && old != s &&
		!s.closed && !s.detached && s.hasRoom() && !s.hasStream(m.Stream)
	if !ok {
		if old != s {
			old.mu.Unlock()
		}
		s.mu.Unlock()
		moveMu.Unlock()
		return s.refuseReattach(m.Stream)
	}
	s.tab.track(st)
	st.moveTo(old, s)
	st.reattaching = true
	st.reported = st.limit
	st.ack(m.Received)
	msgs := append([]any{protocol.Reattached{Stream: st.id, Received: st.received, Limit: st.limit}}, st.resendData()...)
	old.changed.Broadcast()
	s.changed.Broadcast()
	if old != s {
		old.mu.Unlock()
	}
	s.mu.Unlock()
	moveMu.Unlock()
	for _, msg := range msgs {
		if err := s.emit(msg); err != nil {
			return err
		}
	}
	return st.finishReattach()
}

// hasRoom reports whether the session may take one more stream. The caller holds s.mu.
func (s *Session) hasRoom() bool {
	return s.cfg.MaxStreams <= 0 || s.slots < s.cfg.MaxStreams
}

// hasStream reports whether the session holds the stream id. The caller holds s.mu.
func (s *Session) hasStream(id uint64) bool {
	_, ok := s.streams[id]
	return ok
}

// refuseReattach answers a reattach that is not taken with close. The id is remembered, so the close the
// app sends back for it is ignored.
func (s *Session) refuseReattach(id uint64) error {
	s.mu.Lock()
	s.tombstone(id)
	s.mu.Unlock()
	return s.emit(protocol.Close{Stream: id})
}

// onReattached completes the app's reattach: the bytes the host did not receive are resent, and the host's
// limit becomes the send limit. A stream that failed while it waited sends only its held-back close.
func (s *Session) onReattached(m protocol.Reattached) error {
	if s.role != RoleApp {
		return errors.New("mux: the host side got a reattached message")
	}
	s.mu.Lock()
	st := s.streams[m.Stream]
	if st == nil || !st.reattaching {
		s.mu.Unlock()
		return fmt.Errorf("mux: reattached for stream %d, which is not reattaching", m.Stream)
	}
	var msgs []any
	if st.err == nil {
		st.ack(m.Received)
		st.sendLimit = m.Limit
		msgs = st.resendData()
	}
	s.mu.Unlock()
	for _, msg := range msgs {
		if err := s.emit(msg); err != nil {
			return err
		}
	}
	return st.finishReattach()
}

// finishReattach ends the reattach of st once its reattached and its resent bytes are out. The close that
// waited for it goes out, and the credit granted meanwhile goes out as one window, after the reattached.
func (st *Stream) finishReattach() error {
	s := st.lock()
	st.reattaching = false
	var msgs []any
	if st.err == nil && st.limit > st.reported {
		msgs = append(msgs, protocol.Window{Stream: st.id, Credit: st.limit - st.reported, Received: st.received})
	}
	if st.wclosed {
		msgs = append(msgs, protocol.Close{Stream: st.id})
	}
	s.changed.Broadcast()
	s.mu.Unlock()
	for _, msg := range msgs {
		if err := s.emit(msg); err != nil {
			return err
		}
	}
	return nil
}

// endGrace ends the grace period of a detached stream whose entry e is still the table's: the stream fails,
// and its session forgets it, as a stream that was never reattached closes.
func (st *Stream) endGrace(e *entry) {
	s := st.lock()
	defer s.mu.Unlock()
	if s.tab == nil || !s.tab.current(st, e) {
		return
	}
	st.fail(errGrace)
	s.forget(st)
}

// set makes the entry of st, and stops the timer of the entry it replaces. The caller holds t.mu.
func (t *ResumeTable) set(st *Stream, e *entry) *entry {
	if old := t.streams[st]; old != nil {
		old.stop()
	}
	t.streams[st] = e
	return e
}

// track makes the table hold st as attached.
func (t *ResumeTable) track(st *Stream) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.set(st, &entry{})
}

// hold makes the table hold st as detached at now, and starts its grace period.
func (t *ResumeTable) hold(st *Stream, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.set(st, &entry{held: true, at: now})
	e.timer = time.AfterFunc(t.cfg.Grace, func() { st.endGrace(e) })
}

// current reports whether e is still the table's entry of st. The caller holds the lock of st's session.
func (t *ResumeTable) current(st *Stream, e *entry) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.streams[st] == e
}

// detachedAt returns when st's session detached, whether it is detached, and whether the table holds st.
func (t *ResumeTable) detachedAt(st *Stream) (at time.Time, held, tracked bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.streams[st]
	if e == nil {
		return time.Time{}, false, false
	}
	return e.at, e.held, true
}

// find returns the stream of the table that id and token name, or nil. Every stream is compared, and the
// tokens in constant time, so the time taken does not show how close a token is to the issued one.
func (t *ResumeTable) find(id uint64, token [16]byte) *Stream {
	t.mu.Lock()
	defer t.mu.Unlock()
	var found *Stream
	for st := range t.streams {
		if subtle.ConstantTimeCompare(st.token[:], token[:]) == 1 && st.id == id {
			found = st
		}
	}
	return found
}

// remove takes st out of the table and gives its kept bytes back to the total. The caller holds the lock of
// st's session, which owns the count of st.
func (t *ResumeTable) remove(st *Stream) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.used = sub(t.used, st.counted)
	st.counted = 0
	if e := t.streams[st]; e != nil {
		e.stop()
		delete(t.streams, st)
	}
}

// keep reserves n bytes of the total for kept bytes. It reports false when the total would be exceeded.
func (t *ResumeTable) keep(n uint64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.used+n > t.cfg.Total {
		return false
	}
	t.used += n
	return true
}

// free gives n bytes of the total back, once they are acknowledged.
func (t *ResumeTable) free(n uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.used = sub(t.used, n)
}
