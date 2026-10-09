package host

import (
	"sort"
	"sync/atomic"

	"github.com/andrewloable/HoleBridge/internal/mux"
)

// The routes a session runs on, as Session.Route names them (docs/architecture.md, Routes). A session that came
// over the DHT is direct, or relay when its stream was claimed through a relay pairing (hyperdht.AcceptedConn.Relayed).
const (
	routeLAN    = "lan"
	routeDirect = "direct"
	routeRelay  = "relay"
)

// Session is one live session as Sessions reports it: the route it runs on, its open streams and UDP flows, and
// the bytes it carried each way. It holds no key and no target address.
type Session struct {
	Route    string // routeLAN, routeDirect or routeRelay
	Streams  int    // streams open now
	Flows    int    // UDP flows open now
	BytesIn  uint64 // bytes the app sent to the host
	BytesOut uint64 // bytes the host sent to the app
}

// meter counts the traffic of one link's session: the streams it owns that are open, and the bytes those streams
// carried each way. A stream counts on the meter of the link that owns it, so a resumed stream moves to the meter
// of the link that takes it over (streamMeter). The counts are atomics, so the data path takes no lock for them.
type meter struct {
	streams atomic.Int64  // streams whose forward runs and that the link owns
	in      atomic.Uint64 // bytes from the app
	out     atomic.Uint64 // bytes to the app
}

// streamMeter is the accounting of one accepted stream: the stream, and the meter it counts on now. The stream
// counts on the link that owns it: the link that accepted it, until a reattach moves it to the link that takes it
// over (Host.followReattach). cur changes only under Host.mu, and the data path reads it without a lock.
type streamMeter struct {
	st  *mux.Stream
	cur atomic.Pointer[meter]
}

// beginStream counts a stream that starts, on m, the meter of the link that accepted it, and adds it to the streams
// the host holds until endStream. A reattach may already have moved the stream to another link's session, so the
// stream is then placed on the link that owns it.
func (h *Host) beginStream(sm *streamMeter, m *meter) {
	h.mu.Lock()
	defer h.mu.Unlock()
	sm.cur.Store(m)
	m.streams.Add(1)
	h.streamMeters[sm] = struct{}{}
	for l := range h.links {
		moveLocked(sm, l)
	}
}

// endStream ends a stream whose forward returned: it leaves the set, and its gauge comes off the meter it counts on.
// Both happen under Host.mu, so no reattach moves the stream between the two.
func (h *Host) endStream(sm *streamMeter) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.streamMeters, sm)
	sm.cur.Load().streams.Add(-1)
}

// followReattach runs on link l after its session takes streams over by a reattach: each stream the session owns
// now moves its count and its gauge to l's meter. It runs once per reattach, on the link's read path, and never on
// the data path.
func (h *Host) followReattach(l *link) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sm := range h.streamMeters {
		moveLocked(sm, l)
	}
}

// moveLocked moves the count of sm to the meter of l, and its gauge with it, when the stream is in l's session and
// counts on another meter: -1 on the old meter and +1 on the new one. The caller holds Host.mu.
func moveLocked(sm *streamMeter, l *link) {
	old := sm.cur.Load()
	if old == &l.meter || !sm.st.InSession(l.sess) {
		return
	}
	old.streams.Add(-1)
	l.meter.streams.Add(1)
	sm.cur.Store(&l.meter)
}

// Sessions returns the live sessions of the host, one per connection. The list is sorted by route and then by the
// bytes each way, so the same state always gives the same list.
func (h *Host) Sessions() []Session {
	links := h.liveLinks()
	out := make([]Session, 0, len(links))
	for _, l := range links {
		out = append(out, l.session())
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Route != b.Route {
			return a.Route < b.Route
		}
		if a.BytesIn != b.BytesIn {
			return a.BytesIn < b.BytesIn
		}
		return a.BytesOut < b.BytesOut
	})
	return out
}

// session returns the snapshot of the link: its route, its open streams, its open UDP flows and its bytes. Each
// count is read on its own, so the snapshot is not one instant; a session that moves between two reads is
// reported with the counts it had at each read.
func (l *link) session() Session {
	s := Session{
		Route:    l.routeName,
		Streams:  int(l.meter.streams.Load()),
		BytesIn:  l.meter.in.Load(),
		BytesOut: l.meter.out.Load(),
	}
	l.udpMu.RLock()
	defer l.udpMu.RUnlock()
	for _, t := range l.udp {
		s.Flows += t.Stats().Open
	}
	return s
}
