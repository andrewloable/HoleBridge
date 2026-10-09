package host

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andrewloable/HoleBridge/internal/mux"
	"github.com/andrewloable/HoleBridge/internal/protocol"
	"github.com/andrewloable/HoleBridge/internal/udpflow"
	"github.com/andrewloable/HoleBridge/pears/hyperdht"
	"github.com/andrewloable/HoleBridge/pears/protomux"
)

const (
	// protocolName is the protomux channel of protocol v1.
	protocolName = "holebridge"
	// protocolVersion is the protocol version this host speaks.
	protocolVersion = 1
	// messageCount is the number of channel messages of protocol v1, indexes 0 to 10.
	messageCount = 11
)

// serve runs one accepted connection: the protomux channel holebridge with the services handshake, then a
// mux session in host role until the connection ends. Its accept callback dials the service's target with
// the target connect timeout and rejects with codes 1 to 4 (docs/architecture.md, "Messages" and "Limits").
// lan is true for a LAN stream, whose datagrams go out as message 10 on the channel, queued; a DHT stream
// sends them unordered on its UDX connection when the app takes unordered datagrams, and as message 10 when it
// does not. When the transport dies the session detaches and its streams wait for a reattach; when the host
// ends the session itself it closes them. It returns nil when the session ends normally, and the reason when the
// host ended it.
func (h *Host) serve(conn *hyperdht.Conn, lan bool) error {
	defer h.untrack(conn)
	defer conn.Destroy() // the transport goes whole: a peer that ignores END must not keep it

	l := &link{h: h, done: make(chan struct{})}
	h.addLink(l)
	defer h.removeLink(l)
	l.sess = mux.NewSession(mux.RoleHost, l, mux.Config{
		Window:     uint64(h.limits.ReceiveWindowPerStream),
		MaxStreams: h.limits.StreamsPerSession,
		Limits:     h.streams,
		Clock:      h.clock,
	}, h.budget, h.accept)
	// The app's handshake decides whether the session binds to the resume table (onOpen). Its replies go unordered
	// on a DHT connection whose app takes unordered datagrams, and otherwise as message 10 through the session's
	// ordered queue (docs/architecture.md, UDP services).
	q := newReplyQueue(h.limits.OrderedDatagramQueue, func(d protocol.Datagram) { _ = l.Send(d) })
	defer q.close()
	l.udpSend = func(flow uint64, payload []byte) {
		if !lan && l.datagrams.Load() {
			_ = conn.Send(protocol.EncodeUnordered(protocol.Datagram{Flow: flow, Payload: payload})) // a lost reply is as on any UDP path
			return
		}
		q.offer(flow, payload)
	}
	l.udpShare = udpflow.NewCounter(h.limits.UDPFlowsPerSession)
	l.udp = map[string]*udpflow.Table{}
	l.addUDP()
	defer l.closeUDP()
	// The app's unordered datagrams (the DHT route) are messages of the connection, not of the channel. They
	// are read until the connection's message channel closes at teardown, and go to the same tables as message
	// 10. A frame that does not decode is dropped, as UDP drops it.
	go func() {
		for raw := range conn.Messages() {
			if d, err := protocol.DecodeUnordered(raw); err == nil {
				_ = l.route(d) // a datagram never fails the session
			}
		}
	}()

	// protomux reads as soon as it is made, so the pairing is set up before the first read: until then the
	// watch holds the reads back, or an app's open that arrives first would be refused.
	ready := make(chan struct{})
	m := protomux.New(&watch{conn: conn, ready: ready, end: l.lost})
	m.Pair(protocolName, nil, func() { l.attach(m) })
	close(ready)

	<-l.done
	if l.transportLost {
		l.sess.Detach()
	} else {
		l.sess.Close()
	}
	return l.reason
}

// link is the host side of one connection: its protomux channel and its mux session. It is the mux.Sender
// of the session.
type link struct {
	h    *Host
	ch   *protomux.Channel
	msgs []*protomux.Message // one per message index, in index order
	sess *mux.Session
	done chan struct{} // closed when the connection ends
	// udp is the flow table of each udp service, by name. udpMu guards it, since a reload adds a table while the
	// session runs. udpShare is the flow cap the tables share, and udpSend carries each reply.
	udpMu     sync.RWMutex
	udp       map[string]*udpflow.Table
	udpClosed bool
	udpShare  *udpflow.Counter
	udpSend   func(flow uint64, payload []byte)
	// openMu serialises the handshake with a services push, so that a push goes out only after the handshake did.
	// opened is set once the handshake is out.
	openMu sync.Mutex
	opened bool
	// datagrams is set when the app's handshake has FlagDatagrams, and the host's too: a DHT connection then
	// carries the replies unordered. Set in onOpen, before any flow exists.
	datagrams atomic.Bool
	once      sync.Once
	reason    error // why the session ended, set before done closes
	// transportLost is set when the connection ended because its transport died, not because the host ended
	// it: the session then detaches, so its streams can be reattached.
	transportLost bool
	// refusal is the reason the host closed the channel itself, set by refuse. Every end call runs on
	// protomux's read goroutine, so refusal needs no lock.
	refusal error
}

// end ends the connection once, with reason (nil when it ends normally). The reason of a refusal wins over
// the close that refuse makes, since that close reaches end first.
func (l *link) end(reason error) {
	l.once.Do(func() {
		if l.refusal != nil {
			reason = l.refusal
		}
		l.reason = reason
		close(l.done)
	})
}

// lost ends the connection because its transport died (a read failed, or the channel closed under it). It is
// end for a transport, except that the session is detached rather than closed, so its streams wait for a
// reattach. A refusal still wins.
func (l *link) lost(reason error) {
	l.once.Do(func() {
		if l.refusal != nil {
			reason = l.refusal
		} else {
			l.transportLost = true
		}
		l.reason = reason
		close(l.done)
	})
}

// attach makes the channel for the app's holebridge open. It takes the first open only: a second channel on
// the connection is refused by protomux. The channel's messages are added before any frame is read.
func (l *link) attach(m *protomux.Mux) {
	if l.ch != nil {
		return
	}
	l.ch = m.CreateChannel(protomux.ChannelOptions{
		Protocol: protocolName,
		OnOpen:   l.onOpen,
		OnClose:  func(bool) { l.lost(nil) },
	})
	if l.ch == nil {
		l.end(errMalformed)
		return
	}
	for i := 0; i < messageCount; i++ {
		l.msgs = append(l.msgs, l.ch.AddMessage(l.receive(i)))
	}
}

// onOpen checks the app's handshake. The same version gets the host's handshake; another version gets the
// channel closed, and the host logs HB-VERSION-MISMATCH.
func (l *link) onOpen(raw []byte) {
	hs, err := protocol.DecodeHandshake(raw)
	switch {
	case err != nil:
		l.h.log.Warn("malformed handshake from the app")
		l.refuse(errMalformed)
	case hs.Version != protocolVersion:
		l.h.log.Warn("HB-VERSION-MISMATCH", "app_version", hs.Version, "host_version", protocolVersion)
		l.refuse(errVersion)
	default:
		if err := l.negotiate(hs.Flags); err != nil {
			l.end(err)
			return
		}
		l.openMu.Lock()
		defer l.openMu.Unlock()
		if err := l.ch.Open(protocol.EncodeHandshake(l.h.handshake())); err != nil {
			l.end(err)
			return
		}
		l.opened = true
	}
}

// pushServices sends a reload's services list to the app as message 8. A session whose handshake is not out yet
// is skipped: its handshake takes the list from the host when it is built, under openMu.
func (l *link) pushServices(list []protocol.Service) {
	l.openMu.Lock()
	defer l.openMu.Unlock()
	if !l.opened {
		return
	}
	_ = l.Send(protocol.Services{Services: list}) // a lost list ends with the session, as any message does
}

// negotiate sets the features of the session from the app's handshake flags. A feature is on when both sides
// set it: the host sets hostFlags. Resume binds the session to the resume table, which must happen before the
// session opens a stream, so that its opened messages carry tokens (docs/architecture.md, Sessions and reconnects).
func (l *link) negotiate(appFlags uint64) error {
	on := appFlags & hostFlags
	l.datagrams.Store(on&protocol.FlagDatagrams != 0)
	if on&protocol.FlagResume == 0 {
		return nil
	}
	return l.h.resume.Bind(l.sess)
}

// refuse closes the channel. A close reaches the app only after the channel is open, so the host opens it
// with its version alone, which tells the app which version to update to, and then closes it.
func (l *link) refuse(reason error) {
	_ = l.ch.Open(protocol.EncodeHandshake(protocol.Handshake{Version: protocolVersion}))
	l.refusal = reason
	_ = l.ch.Close() // its OnClose ends the connection with the refusal
	l.end(reason)
}

// receive returns the handler of message index. It decodes the payload and hands the message to the
// session. A payload that does not decode, or a message the session rejects, ends the connection.
func (l *link) receive(index int) func([]byte) {
	return func(payload []byte) {
		msg, err := protocol.Decode(index, payload)
		if err == nil {
			err = l.route(msg)
		}
		if err != nil {
			l.h.log.Warn("ending the session on a message it cannot take", "index", index)
			l.end(err)
		}
	}
}

// route hands a decoded message to the UDP flows, for flow (9) and datagram (10), and any other to the
// session. A datagram also comes here when it arrived unordered on a DHT stream. A flow to a service that is
// not a udp service gets no reply. Flow ids are unique per session, so a datagram reaches the one table that
// holds its flow; the others drop it.
func (l *link) route(msg any) error {
	switch m := msg.(type) {
	case protocol.Flow:
		if t := l.table(m.Service); t != nil {
			l.startFlow(t, m)
		}
		return nil
	case protocol.Datagram:
		l.udpMu.RLock()
		defer l.udpMu.RUnlock()
		for _, t := range l.udp {
			t.OnDatagram(m.Flow, m.Payload)
		}
		return nil
	}
	return l.sess.Receive(msg)
}

// startFlow takes a flow message on the session's message path. A flow that is open takes the payload here, in
// order with the datagrams after it. A new flow is opened on its own goroutine: its target lookup can be slow, and
// the session's other messages and its open flows must not wait for it. Datagrams that arrive while the flow is
// still being dialed are dropped (udpflow.Table.OnFlow), as UDP allows.
func (l *link) startFlow(t *udpflow.Table, f protocol.Flow) {
	if t.Has(f.Flow) {
		t.OnDatagram(f.Flow, f.Payload)
		return
	}
	go t.OnFlow(f)
}

// table returns the flow table of the udp service name, or nil when the session has none.
func (l *link) table(name string) *udpflow.Table {
	l.udpMu.RLock()
	defer l.udpMu.RUnlock()
	return l.udp[name]
}

// addUDP gives the session a flow table for each udp service of the host that has none yet: all of them when the
// session starts, and the new ones after a reload. Each table has its service's idle time, or the UDP flow idle of
// the limits when it has none (docs/architecture.md, UDP services), and it resolves the target at each new flow,
// so a reload's new target applies to new flows. The tables share the process's flow total and the session's flow
// cap, and udpSend carries each reply, at once: the session's ordered queue, if any, is in it.
func (l *link) addUDP() {
	services := l.h.udpServices()
	l.udpMu.Lock()
	defer l.udpMu.Unlock()
	if l.udpClosed {
		return
	}
	for name, svc := range services {
		if _, ok := l.udp[name]; ok {
			continue
		}
		idle := svc.idle
		if idle == 0 {
			idle = time.Duration(l.h.limits.UDPFlowIdle)
		}
		l.udp[name] = udpflow.NewTable(udpflow.Config{
			MaxDatagram: l.h.limits.MaxDatagram,
			Idle:        idle,
			PerSession:  l.udpShare,
			Total:       l.h.udp,
			Dial:        l.h.udpDial,
		}, l.udpSend, l.h.udpTarget, l.h.clock)
	}
}

// closeUDP closes the session's flow tables, and with them the flows' sockets. A table addUDP makes after it is
// not made.
func (l *link) closeUDP() {
	l.udpMu.Lock()
	defer l.udpMu.Unlock()
	l.udpClosed = true
	for _, t := range l.udp {
		t.Close()
	}
}

// Send carries one protocol message on the channel, under its message index. It implements mux.Sender.
func (l *link) Send(msg any) error {
	payload, err := protocol.Encode(msg)
	if err != nil {
		return err
	}
	return l.msgs[protocol.Index(msg)].Send(payload)
}

// watch is the connection as protomux reads it. A read waits until the pairing is set up, and a read that
// fails ends the connection.
type watch struct {
	conn  *hyperdht.Conn
	ready chan struct{}
	end   func(error)
}

func (w *watch) Read(p []byte) (int, error) {
	<-w.ready
	n, err := w.conn.Read(p)
	if err != nil {
		w.end(err)
	}
	return n, err
}

// ReadFrame passes the frame reads of protomux through, so protomux needs no read buffer of its own.
func (w *watch) ReadFrame() ([]byte, error) {
	<-w.ready
	frame, err := w.conn.ReadFrame()
	if err != nil {
		w.end(err)
	}
	return frame, err
}

func (w *watch) Write(p []byte) (int, error) {
	return w.conn.Write(p)
}

func (w *watch) Close() error {
	return w.conn.Close()
}

// forward copies the bytes of an accepted stream to its target and back until both directions end, then
// closes both. The app's end of the stream is a half-close: its EOF ends the copy to the target, and the
// target's EOF ends the copy to the app. An error in either copy closes the target, so the other copy ends
// too. When the stream fails (its session ends, or its grace period does), the target is closed even if it is
// silent, since the copy from the target would otherwise wait for it for good. With an idle time, a stream that
// moves no bytes either way for that long is closed (docs/cli.md, services.<name>.idle).
func forward(st *mux.Stream, target net.Conn, idle time.Duration) {
	quiet := newIdleTimer(idle, func() {
		st.Close()
		target.Close()
	})
	ended := make(chan struct{})
	go func() {
		select {
		case <-st.Failed():
			target.Close()
		case <-ended:
		}
	}()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := io.Copy(target, touched{st, quiet}); err != nil {
			target.Close()
			return
		}
		halfClose(target)
	}()
	go func() {
		defer wg.Done()
		io.Copy(st, touched{target, quiet})
		st.CloseWrite()
	}()
	wg.Wait()
	close(ended)
	quiet.stop()
	target.Close()
	st.Close()
}

// idleTimer runs its function once no bytes have moved for its idle time. Each read that moves bytes restarts
// it. A zero idle time never fires.
type idleTimer struct {
	d time.Duration
	t *time.Timer
}

func newIdleTimer(d time.Duration, f func()) *idleTimer {
	if d <= 0 {
		return &idleTimer{}
	}
	return &idleTimer{d: d, t: time.AfterFunc(d, f)}
}

func (i *idleTimer) touch() {
	if i.t != nil {
		i.t.Reset(i.d)
	}
}

func (i *idleTimer) stop() {
	if i.t != nil {
		i.t.Stop()
	}
}

// touched is a reader whose bytes count as activity on the idle timer. Every byte a copy moves is read from
// one side, so the reads of both sides cover both directions.
type touched struct {
	r    io.Reader
	idle *idleTimer
}

func (t touched) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.idle.touch()
	}
	return n, err
}

// closeWriter is a connection that can end its outgoing side alone, as TCP does.
type closeWriter interface {
	CloseWrite() error
}

// halfClose ends the outgoing side of target when it can, and closes it whole otherwise.
func halfClose(target net.Conn) {
	if cw, ok := target.(closeWriter); ok {
		cw.CloseWrite()
		return
	}
	target.Close()
}
