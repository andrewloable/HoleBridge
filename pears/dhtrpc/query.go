// Ported from dht-rpc 6.27.0 lib/query.js, MIT License, Copyright (c) 2021 Mathias Buus.
//
// An iterative query walks towards a target. It asks the nodes it knows for the nodes nearest the
// target, asks those in turn, and keeps the k nearest replies. When the walk ends, the commit runs on
// each of them. Concurrency (10, plus one for each request that has waited a resend cycle), k (20), the
// 5 retries of a query request, the early end and the DOWN_HINT on a timeout are upstream's. Not
// ported: the closest-nodes option, the slowdown of a query that reuses cached nodes, and the retry from
// the routing table, which never runs because every query starts from the table.
package dhtrpc

import (
	"context"
	"errors"
	"net"
	"os"
	"slices"
	"sync"
)

const (
	queryConcurrency = 10 // requests in flight at once (upstream DEFAULTS.concurrency)
	queryRetries     = 5  // resends of a query request (upstream Query retries)
)

// errTooFew is the error of a query with a commit when no reply came back to commit.
var errTooFew = errors.New("dhtrpc: too few nodes responded")

// QueryOpts sets up an iterative query. Target is the 32-byte id the query walks towards. Command and
// Value go to each node the query asks. Internal marks a built-in command such as FIND_NODE: a
// request's Internal flag is a separate namespace from custom commands. Commit, when set, is called on
// each closest reply once the walk ends, as upstream's commit option is; nil commits nothing.
type QueryOpts struct {
	Target   []byte
	Command  uint
	Internal bool
	Value    []byte
	Commit   func(ctx context.Context, r Reply) error
}

// Reply is a reply that a query keeps: the address it came from and the reply itself. Its Response
// carries the node id and the token that a commit sends back.
type Reply struct {
	From     *net.UDPAddr
	Response *Response
}

// Query is an iterative query started by Node.Query. The walk runs in the background. Next reads the
// replies as they arrive; Closest and Err wait for the walk to end.
type Query struct {
	n      *Node
	ctx    context.Context
	opts   QueryOpts
	target [32]byte

	// The walk's state. Only the walk goroutine reads and writes it. closest is complete before done
	// closes, so Closest reads it after that. slow counts the requests in flight that have waited one
	// resend cycle without a reply.
	results  chan answer
	inflight int
	slow     int
	pending  []candidate
	seen     map[[32]byte]*nodeState
	closest  []Reply

	// The replies that Next has not returned yet, and the end of the walk. err is set before done closes.
	mu      sync.Mutex
	cond    *sync.Cond
	replies []Reply
	ended   bool
	err     error
	done    chan struct{}
}

// nodeState is what a walk knows of a node it has queued or asked. refs are the nodes that named it as
// closer; each hears a DOWN_HINT if the node times out. done is set once the node has replied, and down
// once it has timed out.
type nodeState struct {
	refs []*net.UDPAddr
	done bool
	down bool
}

// candidate is a node the walk may ask: its id and its address.
type candidate struct {
	id   [32]byte
	addr *net.UDPAddr
}

// answer is what a request sends the walk. A request that has waited one resend cycle without a reply
// sends an answer with cycle set. Its result follows as an answer with cycle clear: the reply, or the
// error that ended the request. slowed says that the request sent its cycle answer, so the walk counted
// it as slow.
type answer struct {
	addr   *net.UDPAddr
	cycle  bool
	resp   *Response
	err    error
	slowed bool
}

// Query starts an iterative query on n and returns at once. The walk starts from the routing table and,
// when the table gives fewer than k nodes, from the bootstrap nodes.
func (n *Node) Query(ctx context.Context, q QueryOpts) *Query {
	qu := &Query{
		n:       n,
		ctx:     ctx,
		opts:    q,
		seen:    make(map[[32]byte]*nodeState),
		results: make(chan answer, queryConcurrency),
		done:    make(chan struct{}),
	}
	copy(qu.target[:], q.Target)
	qu.cond = sync.NewCond(&qu.mu)
	go qu.run()
	return qu
}

// Next returns the next reply without an error code, with the address it came from, as it arrives, and
// false once the query has ended.
func (q *Query) Next() (Reply, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.replies) == 0 && !q.ended {
		q.cond.Wait()
	}
	if len(q.replies) == 0 {
		return Reply{}, false
	}
	r := q.replies[0]
	q.replies = q.replies[1:]
	return r, true
}

// Closest returns the closest replies, nearest first. It waits for the query to end.
func (q *Query) Closest() []Reply {
	<-q.done
	return q.closest
}

// Err waits for the query to end and returns why it ended: nil; the context error when the context ended
// first; errTooFew when a commit had no reply to commit; or the first commit error when every commit
// failed.
func (q *Query) Err() error {
	<-q.done
	return q.err
}

// run is the walk. It asks candidates while any are pending and requests are in flight, then commits.
func (q *Query) run() {
	q.addFromTable()
	if len(q.pending) < tableK {
		for _, b := range q.n.boot {
			q.add(b, nil)
		}
	}
	q.fill()
	for !q.walkEnds() {
		select {
		case a := <-q.results:
			q.handle(a)
		case <-q.ctx.Done():
			q.finish(q.ctx.Err())
			return
		}
		q.fill()
	}
	if err := q.ctx.Err(); err != nil {
		q.finish(err)
		return
	}
	q.commit()
}

// walkEnds reports whether the walk is over: nothing is pending, and either nothing is in flight, or every
// request in flight has waited one resend cycle and the closest set is full. The second case is upstream's
// early end, which does not wait for the slow requests.
func (q *Query) walkEnds() bool {
	if len(q.pending) > 0 {
		return false
	}
	return q.inflight == 0 || (q.slow == q.inflight && len(q.closest) >= tableK)
}

// handle takes in one answer from a request: a cycle, or a result.
func (q *Query) handle(a answer) {
	if a.cycle {
		q.slow++
		return
	}
	q.inflight--
	if a.slowed {
		q.slow--
	}
	q.onAnswer(a)
}

// addFromTable queues the routing table nodes nearest the target, up to k.
func (q *Query) addFromTable() {
	q.n.mu.Lock()
	nodes := q.n.table.Closest(q.target, tableK)
	q.n.mu.Unlock()
	for _, nd := range nodes {
		q.add(udpOf(nd), nil)
	}
}

// add queues addr unless it was queued or asked before. ref is the node that named addr as closer, or nil
// for a table or bootstrap node. add reports whether addr is closer than the closest replies so far; the
// closer-nodes loop stops at the first node that is not. A node that already timed out gets a DOWN_HINT
// from ref, as upstream's _addPending sends.
func (q *Query) add(addr, ref *net.UDPAddr) bool {
	id := nodeID(addr)
	closer := q.isCloser(id[:])
	st, seen := q.seen[id]
	switch {
	case seen && st.done:
		return closer
	case seen && st.down:
		if ref != nil {
			q.n.sendDownHint(ref, addr)
		}
		return closer
	case seen:
		if ref != nil {
			st.refs = append(st.refs, ref)
		}
		return closer
	case !closer:
		return false
	}
	st = &nodeState{}
	if ref != nil {
		st.refs = []*net.UDPAddr{ref}
	}
	q.seen[id] = st
	q.pending = append(q.pending, candidate{id: id, addr: addr})
	return true
}

// fill asks pending candidates while fewer than queryConcurrency requests, plus one for each slow request,
// are in flight. A candidate that is no longer closer than the closest replies is dropped.
func (q *Query) fill() {
	for q.inflight < queryConcurrency+q.slow && len(q.pending) > 0 {
		c := q.pending[len(q.pending)-1]
		q.pending = q.pending[:len(q.pending)-1]
		if q.isCloser(c.id[:]) {
			q.ask(c)
		}
	}
}

// ask sends the query's request to c in its own goroutine. The request's cycle and its result go to the
// walk on q.results. The cycle is sent once, at the first resend cycle with no reply.
func (q *Query) ask(c candidate) {
	q.inflight++
	go func() {
		req := Request{Internal: q.opts.Internal, Command: uint64(q.opts.Command), Target: q.target[:], Value: q.opts.Value}
		slowed := false
		cycle := func() {
			if !slowed {
				slowed = true
				q.send(answer{addr: c.addr, cycle: true})
			}
		}
		resp, err := q.n.requestRetry(q.ctx, c.addr, req, queryRetries, cycle)
		q.send(answer{addr: c.addr, resp: resp, err: err, slowed: slowed})
	}()
}

// send hands a to the walk. It gives up once the query has ended, so a request never stays blocked on a
// walk that has stopped reading.
func (q *Query) send(a answer) {
	select {
	case q.results <- a:
	case <-q.done:
	}
}

// onAnswer takes in the result of one request. A request that timed out marks its node DOWN. Any reply marks
// its node DONE, and its closer nodes are queued. A reply without an error code joins the closest replies
// when it is closer, and goes to Next.
func (q *Query) onAnswer(a answer) {
	id := nodeID(a.addr)
	if a.err != nil {
		if errors.Is(a.err, os.ErrDeadlineExceeded) {
			q.timedOut(id, a.addr)
		}
		return
	}
	st := q.seen[id]
	st.done = true
	st.refs = nil
	m := a.resp
	if m.Error == 0 && m.ID != nil && q.isCloser(m.ID) {
		q.pushClosest(Reply{From: a.addr, Response: m})
	}
	for _, c := range m.CloserNodes {
		addr := &net.UDPAddr{IP: net.IP(c.Host.AsSlice()), Port: int(c.Port)}
		if nodeID(addr) == q.n.id {
			continue
		}
		if !q.add(addr, a.addr) {
			break
		}
	}
	if m.Error == 0 {
		q.emit(Reply{From: a.addr, Response: m})
	}
}

// timedOut marks the node with id and address addr DOWN, and sends a DOWN_HINT with its address to each
// node that named it, as upstream's _onerror does.
func (q *Query) timedOut(id [32]byte, addr *net.UDPAddr) {
	st := q.seen[id]
	st.down = true
	for _, ref := range st.refs {
		q.n.sendDownHint(ref, addr)
	}
	st.refs = nil
}

// isCloser reports whether id is closer to the target than the closest replies so far, or there are
// fewer than k of them.
func (q *Query) isCloser(id []byte) bool {
	return len(q.closest) < tableK || q.cmp(id, q.closest[len(q.closest)-1].Response.ID) < 0
}

// pushClosest adds r to the closest replies, nearest first, unless its id is already there. At most k
// replies are kept.
func (q *Query) pushClosest(r Reply) {
	i := 0
	for i < len(q.closest) && q.cmp(q.closest[i].Response.ID, r.Response.ID) < 0 {
		i++
	}
	if i < len(q.closest) && q.cmp(q.closest[i].Response.ID, r.Response.ID) == 0 {
		return
	}
	q.closest = slices.Insert(q.closest, i, r)
	if len(q.closest) > tableK {
		q.closest = q.closest[:tableK]
	}
}

// cmp compares the ids a and b by their XOR distance from the target, as upstream's _compare does. It
// returns a negative number when a is nearer.
func (q *Query) cmp(a, b []byte) int {
	for i := range q.target {
		da, db := q.target[i]^a[i], q.target[i]^b[i]
		if da != db {
			return int(da) - int(db)
		}
	}
	return 0
}

// emit hands a reply to Next.
func (q *Query) emit(r Reply) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.replies = append(q.replies, r)
	q.cond.Broadcast()
}

// commit ends the walk. Without a commit it ends at once. With one, it runs on each closest reply, and
// the query succeeds when at least one commit does.
func (q *Query) commit() {
	if q.opts.Commit == nil {
		q.finish(nil)
		return
	}
	if len(q.closest) == 0 {
		q.finish(errTooFew)
		return
	}
	errs := make([]error, len(q.closest))
	var wg sync.WaitGroup
	for i, r := range q.closest {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = q.opts.Commit(q.ctx, r)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err == nil {
			q.finish(nil)
			return
		}
	}
	q.finish(errs[0])
}

// finish ends the query with err: it wakes Next and releases Closest and Err.
func (q *Query) finish(err error) {
	q.mu.Lock()
	q.err = err
	q.ended = true
	q.cond.Broadcast()
	q.mu.Unlock()
	close(q.done)
}
