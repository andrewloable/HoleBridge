package mux

import "sync"

// Budget is the receive budget of a process. Every session in the process shares it: 256 MiB on the
// host and 64 MiB in the app (docs/architecture.md, "Limits").
//
// The budget holds the credit granted to streams that their local readers have not yet taken. A
// stream's first grant is min(window, budget left / open streams). A grant made when a reader takes
// bytes is capped by the stream's own window and by the same share, so once the budget is spent a
// stream gets credit only as the budget is drained. A stream left with no credit is starved: it is
// kept in starved, and each time credit is freed the budget tops those streams up.
type Budget struct {
	mu      sync.Mutex
	total   uint64 // bytes the process may hold as credit
	spent   uint64 // credit granted and not yet taken by a reader, over all streams
	open    int    // streams counted in the budget
	starved map[*Stream]bool
}

// NewBudget returns a budget of total bytes.
func NewBudget(total uint64) *Budget {
	return &Budget{total: total, starved: map[*Stream]bool{}}
}

// share is the fair share of what is left for one open stream. The caller holds mu.
func (b *Budget) share() uint64 {
	if b.spent >= b.total {
		return 0
	}
	left := b.total - b.spent
	if b.open <= 0 {
		return left
	}
	return left / uint64(b.open)
}

// grantLocked grants up to room from the share and counts it as spent. The caller holds mu. It does not
// top up the starved streams, so a top-up cannot start another one.
func (b *Budget) grantLocked(room uint64) uint64 {
	g := min(room, b.share())
	b.spent += g
	return g
}

// take grants a stream up to room from the share, as a top-up does.
func (b *Budget) take(room uint64) uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.grantLocked(room)
}

// admit counts a new stream and returns its first grant.
func (b *Budget) admit(window uint64) uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.open++
	return b.grantLocked(window)
}

// refill records that a reader took released bytes of a stream, then returns the stream's new grant:
// at most room, and at most its share. Credit freed this way tops up the starved streams.
func (b *Budget) refill(released, room uint64) uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.spent = sub(b.spent, released)
	g := b.grantLocked(room)
	b.kickLocked()
	return g
}

// retire drops a stream from the budget, with the credit it had not yet been taken. Credit freed this way
// tops up the starved streams.
func (b *Budget) retire(untaken uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.open--
	b.spent = sub(b.spent, untaken)
	b.kickLocked()
}

// reopen counts a stream that moved here from another session with the credit it had not yet been taken.
func (b *Budget) reopen(untaken uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.open++
	b.spent += untaken
}

// track adds st to the starved set when starved is true, and removes it when false.
func (b *Budget) track(st *Stream, starved bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if starved {
		b.starved[st] = true
	} else {
		delete(b.starved, st)
	}
}

// kickLocked starts a top-up of the starved streams when there are any. The top-up runs on its own goroutine:
// the caller holds the lock of one session, and a starved stream can belong to another session. The caller
// holds mu.
func (b *Budget) kickLocked() {
	if len(b.starved) > 0 {
		go b.topUp()
	}
}

// topUp grants each starved stream what the budget allows now. It takes the lock of one stream's session at
// a time, and sends the window after releasing it.
func (b *Budget) topUp() {
	b.mu.Lock()
	list := make([]*Stream, 0, len(b.starved))
	for st := range b.starved {
		list = append(list, st)
	}
	b.mu.Unlock()
	for _, st := range list {
		s := st.lock()
		w := st.topUp()
		s.mu.Unlock()
		if w != nil {
			_ = s.emit(*w)
		}
	}
}

// sub returns a minus b, or 0 when b is larger.
func sub(a, b uint64) uint64 {
	if b > a {
		return 0
	}
	return a - b
}
