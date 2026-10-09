package host

import (
	"sync"

	"github.com/andrewloable/HoleBridge/internal/protocol"
)

// replyQueue is a session's queue of the replies its udp services send on the channel, as datagram messages
// (message 10). It holds one byte budget for the whole session, the ordered datagram queue of the limits
// (docs/architecture.md, Limits), and one goroutine sends its replies in order, so the channel never stalls a
// flow. A reply that finds the queue full is dropped, as UDP drops it.
type replyQueue struct {
	limit  int                     // payload bytes the queue holds
	send   func(protocol.Datagram) // carries one reply on the channel
	mu     sync.Mutex
	cond   *sync.Cond // wakes the drainer
	items  []protocol.Datagram
	queued int // payload bytes in items
	closed bool
}

// newReplyQueue returns an empty queue of limit bytes that sends with send, and starts its drainer.
func newReplyQueue(limit int, send func(protocol.Datagram)) *replyQueue {
	q := &replyQueue{limit: limit, send: send}
	q.cond = sync.NewCond(&q.mu)
	go q.drain()
	return q
}

// offer queues a copy of a reply for flow, or drops it when the queue has no room for it. The payload may be
// reused as soon as offer returns.
func (q *replyQueue) offer(flow uint64, payload []byte) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.queued+len(payload) > q.limit {
		return
	}
	q.items = append(q.items, protocol.Datagram{Flow: flow, Payload: append([]byte(nil), payload...)})
	q.queued += len(payload)
	q.cond.Signal()
}

// drain sends the queued replies one at a time, in order, until the queue closes.
func (q *replyQueue) drain() {
	q.mu.Lock()
	for {
		for len(q.items) == 0 && !q.closed {
			q.cond.Wait()
		}
		if q.closed {
			q.mu.Unlock()
			return
		}
		d := q.items[0]
		q.items = q.items[1:]
		q.queued -= len(d.Payload)
		q.mu.Unlock()
		q.send(d)
		q.mu.Lock()
	}
}

// close drops the queued replies and stops the drainer after its current send.
func (q *replyQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	q.items, q.queued = nil, 0
	q.cond.Broadcast()
}
