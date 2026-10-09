// Package udpflow is the host side of UDP services: one connected UDP socket per flow, the table that
// holds a session's flows, and the limits on them. The design is docs/architecture.md, "UDP services"
// and "Limits".
package udpflow

import (
	"errors"
	"net"
	"sync"
	"time"

	"github.com/andrewloable/HoleBridge/internal/protocol"
)

// sweepEvery is how often a table checks its flows for idle timeouts.
const sweepEvery = 100 * time.Millisecond

// Config holds the limits of a flow table.
type Config struct {
	// MaxDatagram is the largest payload in bytes: 1144. The largest unordered message is 1156 bytes
	// (docs/spike-m1.md, "## Unordered datagrams"), and protocol.EncodeUnordered adds a 12-byte frame
	// header to the payload at the widest flow id (a 9-byte flow and a 3-byte length prefix). Larger
	// datagrams are dropped and counted.
	MaxDatagram int
	// Idle closes a flow after this long with no datagram either way: 60 s, or the service's idle. Zero
	// means no idle close.
	Idle time.Duration
	// PerSession caps the flows of one session across all of its tables: 256. Make one with NewCounter for
	// each session, and give it to every table of that session. Nil means no per-session cap.
	PerSession *Counter
	// Total caps the flows across every table that shares it: 4096. Nil means no shared cap.
	Total *Counter
	// OrderedQueue is the size in bytes of the ordered queue on the LAN route: 256 KiB. New datagrams
	// are dropped when it is full. Zero means no queue: replies go straight to send.
	OrderedQueue int

	// Dial opens a flow's socket: the name lookup of a host name in the address happens here. Nil means
	// net.Dial. Tests set it to slow the lookup down.
	Dial func(network, address string) (net.Conn, error)
}

// Counter caps the flows open across the tables that share it. Make one with NewCounter.
type Counter struct {
	mu    sync.Mutex
	limit int
	open  int
}

// NewCounter returns a Counter that refuses flows beyond limit.
func NewCounter(limit int) *Counter {
	return &Counter{limit: limit}
}

// acquire takes one flow slot, or reports false when the counter is full. A nil Counter has room.
func (c *Counter) acquire() bool {
	if c == nil {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.open >= c.limit {
		return false
	}
	c.open++
	return true
}

// release gives back one flow slot.
func (c *Counter) release() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.open--
}

// take gives a flow one slot of the session and one of the total, or reports false with neither taken.
func (c Config) take() bool {
	if !c.PerSession.acquire() {
		return false
	}
	if !c.Total.acquire() {
		c.PerSession.release()
		return false
	}
	return true
}

// give returns the slots that take gave.
func (c Config) give() {
	c.PerSession.release()
	c.Total.release()
}

// Table holds the UDP flows of one session.
type Table struct {
	cfg    Config
	send   func(flow uint64, payload []byte)
	target func(service string) (string, bool)
	clock  func() time.Time

	mu      sync.Mutex
	cond    *sync.Cond // wakes the queue drainer
	flows   map[uint64]*flow
	dialing map[uint64]bool // flow ids whose target lookup and dial are under way, without mu
	dropped Dropped
	queue   []reply
	queued  int // bytes in queue
	closed  bool
	done    chan struct{} // closed by Close, stops the sweeper
}

// flow is one UDP socket connected to a service's target.
type flow struct {
	conn net.Conn
	last time.Time // the last datagram either way, by the table's clock
}

// reply is a datagram from a flow's target, waiting in the ordered queue.
type reply struct {
	flow    uint64
	payload []byte
}

// Dropped counts what a table dropped, by reason.
type Dropped struct {
	TooLarge  int // a datagram over MaxDatagram
	Limit     int // a flow refused by PerSession or Total
	QueueFull int // a datagram dropped because the ordered queue was full
	Unknown   int // a flow for a service the host does not have
}

// Stats is a snapshot of a table.
type Stats struct {
	Dropped Dropped
	Open    int // flows open now
}

// NewTable returns an empty table. send gets each datagram that comes back from a flow's target, with
// the flow id. target resolves a service name to the host:port of its target. clock is the time
// source for idle timeouts.
//
// The payload send gets is only valid during the call. Target must not call back into the table.
// Call Close when the table is no longer used: it stops the table's goroutines.
func NewTable(cfg Config, send func(flow uint64, payload []byte), target func(service string) (string, bool), clock func() time.Time) *Table {
	if cfg.Dial == nil {
		cfg.Dial = net.Dial
	}
	t := &Table{
		cfg:     cfg,
		send:    send,
		target:  target,
		clock:   clock,
		flows:   make(map[uint64]*flow),
		dialing: make(map[uint64]bool),
		done:    make(chan struct{}),
	}
	t.cond = sync.NewCond(&t.mu)
	go t.sweep()
	if cfg.OrderedQueue > 0 {
		go t.drain()
	}
	return t
}

// OnFlow opens a flow: a UDP socket connected to the service's target, which sends the first payload.
// A flow id that is already open gets the payload as a datagram instead. The flow's slot and id are
// reserved under the lock; the target lookup and the dial run without it, so a slow lookup stalls no
// other flow. A flow id that is still being dialed gets its payload dropped, as UDP allows. The first
// payload is written before the flow is visible to OnDatagram, so a datagram that follows it on the
// session cannot overtake it, even when OnFlow runs on its own goroutine.
func (t *Table) OnFlow(f protocol.Flow) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	if _, open := t.flows[f.Flow]; open {
		t.mu.Unlock()
		t.OnDatagram(f.Flow, f.Payload)
		return
	}
	if t.dialing[f.Flow] { // a dial for this id is under way; this payload is dropped
		t.mu.Unlock()
		return
	}
	if !t.cfg.take() {
		t.dropped.Limit++
		t.mu.Unlock()
		return
	}
	t.dialing[f.Flow] = true
	t.mu.Unlock()

	addr, ok := t.target(f.Service)
	var conn net.Conn
	var err error
	if ok {
		conn, err = t.cfg.Dial("udp", addr) // a connected socket: the kernel drops other sources
	}
	if ok && err == nil && len(f.Payload) <= t.cfg.MaxDatagram {
		conn.Write(f.Payload) // not yet visible to OnDatagram: nothing later can overtake it
	}

	t.mu.Lock()
	delete(t.dialing, f.Flow)
	switch {
	case t.closed: // Close ran during the lookup and did not see this flow
		t.cfg.give()
		if conn != nil {
			conn.Close()
		}
	case !ok:
		t.dropped.Unknown++
		t.cfg.give()
	case err != nil:
		t.cfg.give()
	default:
		if len(f.Payload) > t.cfg.MaxDatagram {
			t.dropped.TooLarge++
		}
		fl := &flow{conn: conn, last: t.clock()}
		t.flows[f.Flow] = fl
		go t.pump(f.Flow, fl)
	}
	t.mu.Unlock()
}

// Has reports whether the table has the flow open. A flow that is still being dialed is not open.
func (t *Table) Has(flow uint64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.flows[flow]
	return ok
}

// OnDatagram sends a datagram from the session to a flow's target. A datagram for a flow the table
// does not have is dropped.
func (t *Table) OnDatagram(flow uint64, p []byte) {
	t.mu.Lock()
	fl := t.flows[flow]
	if fl == nil {
		t.mu.Unlock()
		return
	}
	fl.last = t.clock()
	if len(p) > t.cfg.MaxDatagram {
		t.dropped.TooLarge++
		t.mu.Unlock()
		return
	}
	t.mu.Unlock()
	fl.conn.Write(p) // UDP: a failed write is a lost datagram, as on any UDP path
}

// Close closes every flow in the table and stops its goroutines. Replies still queued are dropped.
func (t *Table) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.closed = true
	close(t.done)
	for id, fl := range t.flows {
		t.remove(id, fl)
	}
	t.queue, t.queued = nil, 0
	t.cond.Broadcast()
}

// Stats returns the table's counters and its number of open flows.
func (t *Table) Stats() Stats {
	t.mu.Lock()
	defer t.mu.Unlock()
	return Stats{Dropped: t.dropped, Open: len(t.flows)}
}

// remove closes a flow and gives back its slot. The caller holds t.mu.
func (t *Table) remove(id uint64, fl *flow) {
	delete(t.flows, id)
	fl.conn.Close()
	t.cfg.give()
}

// pump reads a flow's replies until the flow's socket closes.
func (t *Table) pump(id uint64, fl *flow) {
	// One byte past MaxDatagram, so a larger datagram shows up as too large instead of being cut short.
	buf := make([]byte, t.cfg.MaxDatagram+1)
	for {
		n, err := fl.conn.Read(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue // a connected socket reports a refused reply as an error; the flow lives on
		}
		t.reply(id, fl, buf[:n])
	}
}

// reply handles one datagram from a flow's target: dropped if too large, or if the ordered queue is
// full; otherwise queued, or sent at once when there is no queue.
func (t *Table) reply(id uint64, fl *flow, p []byte) {
	t.mu.Lock()
	if t.closed || t.flows[id] != fl {
		t.mu.Unlock()
		return
	}
	fl.last = t.clock()
	switch {
	case len(p) > t.cfg.MaxDatagram:
		t.dropped.TooLarge++
		t.mu.Unlock()
	case t.cfg.OrderedQueue == 0:
		t.mu.Unlock()
		t.send(id, p)
	case t.queued+len(p) > t.cfg.OrderedQueue:
		t.dropped.QueueFull++
		t.mu.Unlock()
	default:
		t.queue = append(t.queue, reply{flow: id, payload: append([]byte(nil), p...)})
		t.queued += len(p)
		t.cond.Signal()
		t.mu.Unlock()
	}
}

// drain passes queued replies to send, one at a time, until the table closes.
func (t *Table) drain() {
	t.mu.Lock()
	for {
		for len(t.queue) == 0 && !t.closed {
			t.cond.Wait()
		}
		if t.closed {
			t.mu.Unlock()
			return
		}
		r := t.queue[0]
		t.queue = t.queue[1:]
		t.queued -= len(r.payload)
		t.mu.Unlock()
		t.send(r.flow, r.payload)
		t.mu.Lock()
	}
}

// sweep closes the flows that have been idle for the table's Idle time, by the table's clock.
func (t *Table) sweep() {
	tick := time.NewTicker(sweepEvery)
	defer tick.Stop()
	for {
		select {
		case <-t.done:
			return
		case <-tick.C:
			t.mu.Lock()
			now := t.clock()
			for id, fl := range t.flows {
				if t.cfg.Idle > 0 && now.Sub(fl.last) >= t.cfg.Idle {
					t.remove(id, fl)
				}
			}
			t.mu.Unlock()
		}
	}
}
