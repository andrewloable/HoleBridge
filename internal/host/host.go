// Package host is the host role of HoleBridge protocol v1: the DHT server under the host key pair with
// the client-key firewall, one protomux channel per session with the services handshake, one mux session
// per connection whose streams are connected to the configured targets (docs/architecture.md, "Direct
// route (connection flow)", "Wire protocol v1" and "Limits"; docs/decisions.md, D8).
//
// The host never connects anywhere an app names: an app names a service, and only the host's configuration
// maps the name to a target.
package host

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/andrewloable/HoleBridge/internal/config"
	"github.com/andrewloable/HoleBridge/internal/keys"
	"github.com/andrewloable/HoleBridge/internal/mux"
	"github.com/andrewloable/HoleBridge/internal/protocol"
	"github.com/andrewloable/HoleBridge/internal/udpflow"
	"github.com/andrewloable/HoleBridge/pears/hyperdht"
	"github.com/andrewloable/HoleBridge/pears/noise"
	"github.com/andrewloable/HoleBridge/pears/secretstream"
)

// Reject codes of docs/architecture.md, Wire protocol v1. The limit code (2) is the mux's.
const (
	rejectUnknownService = 1
	rejectRefused        = 3
	rejectTimeout        = 4
)

// Options sets up a host. DHT is the HyperDHT node the server listens on. Dial connects to a service's
// target, or nil for a net.Dialer. Clock is the time source, or nil for time.Now. Log receives the host's
// log lines; they never carry a key, an application key, a derived secret or payload bytes. Kinds gives the
// kind of a service as the kinds watcher knows it (docs/architecture.md, Service kinds). It is asked only for
// a service with no kind in host.json, since an explicit kind is never re-probed; nil means unknown. Touch is called
// when a stream to a known service that is not udp opens, before its target is dialed, so that the kinds watcher
// re-checks a service that is still unknown (docs/architecture.md, Service kinds). It must not block; nil means no call.
type Options struct {
	DHT   *hyperdht.DHT
	Dial  func(ctx context.Context, network, addr string) (net.Conn, error)
	Clock func() time.Time
	Log   *slog.Logger
	Kinds func(service string) protocol.Kind
	Touch func(service string)

	// Dir is the config directory whose host.json Reload re-reads.
	Dir string

	// streamOptions is a test hook. When set, the host calls it with the secret-stream options each session's
	// stream is set up with. Nil in production.
	streamOptions func(secretstream.Options)
	// udpDial is a test hook: the dialer of each UDP flow's socket, where a host name's lookup happens. Nil means
	// net.Dial. Nil in production.
	udpDial func(network, address string) (net.Conn, error)
}

// keepalive is the secret-stream keepalive interval of every session: upstream's connectionKeepAlive, 5 s
// (app/engine/node_modules/hyperdht/index.js). The LAN listener gives its streams the same interval.
const keepalive = 5 * time.Second

// hostFlags are the handshake flags the host supports. A feature is on for a session when the app sets it too.
const hostFlags = protocol.FlagResume | protocol.FlagDatagrams

// Stream resume limits (docs/architecture.md, "Sessions and reconnects"): the bytes a stream keeps for a
// resend, per stream and in total, and how long a dropped stream waits for its reattach.
const (
	resumePerStream = 4 << 20
	resumeTotal     = 32 << 20
	resumeGrace     = 60 * time.Second
)

// service is one configured service: the target the host dials, and whether it is a UDP service, which
// uses flows and no streams. kind, port and origins are its handshake entry; idle is host.json's idle time,
// zero for none.
type service struct {
	addr    string        // host:port of the target
	udp     bool          // a udp service: flows, not streams
	kind    protocol.Kind // the kind host.json names, or unknown
	port    uint64        // the target's port, the port hint
	origins []string
	idle    time.Duration
}

// Host serves the services of one configuration under one key on one DHT node.
type Host struct {
	log           *slog.Logger
	dht           *hyperdht.DHT
	dial          func(ctx context.Context, network, addr string) (net.Conn, error)
	clock         func() time.Time
	kinds         func(service string) protocol.Kind              // the kinds watcher, or nil
	touch         func(service string)                            // the kinds watcher's Touch, or nil
	streamOptions func(secretstream.Options)                      // test hook: the options each DHT session is set up with
	udpDial       func(network, address string) (net.Conn, error) // test hook: the dialer of each UDP flow, nil for net.Dial
	limits        config.Limits
	dir           string           // the config directory Reload re-reads host.json from; empty means none
	appKey        [32]byte         // the application key Reload derives the key pairs with: a secret, never logged
	relayServer   *[32]byte        // the relay server's public key when host.json has a relay, else nil: read at start
	lan           bool             // the LAN route is on: each handshake carries the LAN block
	lanPort       uint64           // the LAN TCP port, from host.json
	budget        *mux.Budget      // the receive budget, shared by the process's sessions
	streams       *mux.Counter     // the streams in total, shared by the process's sessions
	udp           *udpflow.Counter // the UDP flows in total, shared by the process's sessions
	resume        *mux.ResumeTable // the streams of every session; a dropped session's streams wait here for a reattach

	// lifeMu serialises Run's start and stop with Reload, which may listen again under a new key. ctx is the
	// context Run was given; a reload that listens again uses it.
	lifeMu sync.Mutex
	ctx    context.Context

	mu        sync.Mutex
	kp        noise.KeyPair               // the host key pair the server listens under
	clientPub [32]byte                    // the one key the firewall admits
	srv       *hyperdht.Server            // the server that serves now; nil while Run does not listen
	names     []string                    // the service names, sorted: the handshake lists them in this order
	services  map[string]service          // by name: what an open is dialed to
	conns     map[*hyperdht.Conn]struct{} // the live connections: one session each, for the one key
	links     map[*link]struct{}          // the sessions, each with a channel a services list can be pushed on
	// streamMeters are the streams whose forward runs, each with the meter it counts on (streamMeter). A reattach
	// moves a stream's count with it, so the set is read and changed under mu.
	streamMeters map[*streamMeter]struct{}
}

// New returns a host for cfg. The host key pair and the client key pair come from cfg.Key and appKey
// (internal/keys). New does not listen: Run does.
func New(cfg *config.Config, appKey [32]byte, opts Options) (*Host, error) {
	d, err := keys.Derive(cfg.Key, appKey)
	if err != nil {
		return nil, err
	}
	if opts.DHT == nil {
		return nil, errors.New("host: no DHT node")
	}
	names, services, err := buildServices(cfg)
	if err != nil {
		return nil, err
	}
	var relayServer *[32]byte
	if cfg.Relay != "" {
		pub, err := RelayServerPublicKey(cfg.Relay, appKey)
		if err != nil {
			return nil, err
		}
		relayServer = &pub
	}
	h := &Host{
		log:           opts.Log,
		dht:           opts.DHT,
		dial:          opts.Dial,
		clock:         opts.Clock,
		kinds:         opts.Kinds,
		touch:         opts.Touch,
		streamOptions: opts.streamOptions,
		udpDial:       opts.udpDial,
		limits:        cfg.Limits,
		dir:           opts.Dir,
		appKey:        appKey,
		relayServer:   relayServer,
		lan:           cfg.LAN.Enabled == nil || *cfg.LAN.Enabled,
		lanPort:       uint64(cfg.LAN.Port),
		names:         names,
		services:      services,
		budget:        mux.NewBudget(uint64(cfg.Limits.ReceiveBudget)),
		streams:       mux.NewCounter(cfg.Limits.StreamsTotal),
		udp:           udpflow.NewCounter(cfg.Limits.UDPFlowsTotal),
		conns:         map[*hyperdht.Conn]struct{}{},
		links:         map[*link]struct{}{},
		streamMeters:  map[*streamMeter]struct{}{},
	}
	if h.log == nil {
		h.log = slog.New(slog.DiscardHandler)
	}
	if h.clock == nil {
		h.clock = time.Now
	}
	if h.dial == nil {
		var nd net.Dialer
		h.dial = nd.DialContext
	}
	h.resume = mux.NewResumeTable(mux.ResumeConfig{PerStream: resumePerStream, Total: resumeTotal, Grace: resumeGrace}, h.clock)
	copy(h.kp.Public[:], d.Host.Public().(ed25519.PublicKey))
	copy(h.kp.Secret[:], d.Host)
	copy(h.clientPub[:], d.Client.Public().(ed25519.PublicKey))
	return h, nil
}

// buildServices returns the service names of cfg, sorted, and the entry of each: the target the host dials and
// its handshake entry. The names are sorted, so the handshake lists the services in the same order on every
// connection.
func buildServices(cfg *config.Config) ([]string, map[string]service, error) {
	var names []string
	for name := range cfg.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	services := make(map[string]service, len(names))
	for _, name := range names {
		s := cfg.Services[name]
		target, port, err := config.ParseTarget(s.Target)
		if err != nil {
			return nil, nil, err
		}
		kind := kindOf(s.Kind)
		services[name] = service{
			addr:    net.JoinHostPort(target, strconv.Itoa(port)),
			udp:     kind == protocol.KindUDP,
			kind:    kind,
			port:    uint64(port),
			origins: s.Origins,
			idle:    time.Duration(s.Idle),
		}
	}
	return names, services, nil
}

// handshake returns the services handshake a session's app is sent when its channel opens. The services are
// servicesList; the LAN block is there while the LAN route is on. It is built per session, so a kind learned
// mid-session shows at the next session start.
func (h *Host) handshake() protocol.Handshake {
	hs := protocol.Handshake{Version: protocolVersion, Flags: hostFlags, Services: h.servicesList()}
	if h.lan {
		hs.Flags |= protocol.FlagLAN
		hs.LAN = &protocol.LAN{Addresses: lanAddresses(), Port: h.lanPort}
	}
	return hs
}

// servicesList returns the services in handshake entries, in name order. A service with no kind takes the kind
// of the Kinds option, or unknown (docs/architecture.md, Service kinds). A reload pushes this list to the open
// sessions as message 8.
func (h *Host) servicesList() []protocol.Service {
	h.mu.Lock()
	defer h.mu.Unlock()
	var list []protocol.Service
	for _, name := range h.names {
		s := h.services[name]
		kind := s.kind
		if kind == protocol.KindUnknown && h.kinds != nil {
			kind = h.kinds(name)
		}
		list = append(list, protocol.Service{Name: name, Kind: kind, Port: s.port, Origins: s.origins})
	}
	return list
}

// udpServices returns the udp services, by name.
func (h *Host) udpServices() map[string]service {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]service{}
	for name, s := range h.services {
		if s.udp {
			out[name] = s
		}
	}
	return out
}

// udpTarget resolves the target of a udp service for a new flow. A service that a reload removed, or that is no
// longer udp, has none, so its new flows are dropped.
func (h *Host) udpTarget(name string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.services[name]
	return s.addr, ok && s.udp
}

// clientKey returns the one key the firewall admits now.
func (h *Host) clientKey() [32]byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.clientPub
}

// addLink and removeLink register a session, so that a reload can push to it; liveLinks returns them all.
func (h *Host) addLink(l *link) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.links[l] = struct{}{}
}

func (h *Host) removeLink(l *link) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.links, l)
}

func (h *Host) liveLinks() []*link {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*link, 0, len(h.links))
	for l := range h.links {
		out = append(out, l)
	}
	return out
}

// kindOf maps a service kind of host.json to its wire kind. A service with no kind is unknown until kind
// detection (docs/architecture.md, Service kinds) sets one.
func kindOf(s string) protocol.Kind {
	switch s {
	case "https":
		return protocol.KindHTTPS
	case "http":
		return protocol.KindHTTP
	case "tcp":
		return protocol.KindTCP
	case "udp":
		return protocol.KindUDP
	}
	return protocol.KindUnknown
}

// Run listens on the DHT under the host key pair and serves sessions until ctx is done. It returns nil
// when ctx is done and an error when the server cannot listen. A reload that changes the key listens again
// under the new one.
func (h *Host) Run(ctx context.Context) error {
	h.lifeMu.Lock()
	h.ctx = ctx
	h.mu.Lock()
	kp, clientPub := h.kp, h.clientPub
	h.mu.Unlock()
	err := h.listen(ctx, kp, clientPub)
	h.lifeMu.Unlock()
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("host: listen: %w", err)
	}
	h.log.Info("listening")

	<-ctx.Done()
	h.lifeMu.Lock()
	h.stop()
	h.lifeMu.Unlock()
	return nil
}

// listen listens under kp on a new server that admits clientPub, and makes it the server that serves. The server
// it replaces is closed, and so are the sessions, which were admitted under the old key. When listen fails the
// old server keeps serving. The caller holds lifeMu.
func (h *Host) listen(ctx context.Context, kp noise.KeyPair, clientPub [32]byte) error {
	srv := h.dht.CreateServer(h.serverOptions(clientPub))
	if err := srv.Listen(ctx, kp); err != nil {
		srv.Close()
		return err
	}
	h.mu.Lock()
	old := h.srv
	h.srv, h.kp, h.clientPub = srv, kp, clientPub
	h.mu.Unlock()
	if old != nil {
		old.Close()
	}
	h.closeAll()
	go h.acceptLoop(srv)
	return nil
}

// serverOptions returns the options of the server that listens under the host key and admits clientPub. The server has
// a relayThrough policy only when host.json has a relay (docs/architecture.md, Relay route): the policy offers the relay
// when a dial is forced or this host's own NAT is randomized, and reads that NAT from the DHT at each call.
func (h *Host) serverOptions(clientPub [32]byte) hyperdht.ServerOptions {
	opts := hyperdht.ServerOptions{Firewall: h.firewall(clientPub), Keepalive: keepalive}
	if h.relayServer != nil {
		opts.RelayThrough = RelayThrough(h.relayServer, h.dht.NAT)
	}
	return opts
}

// stop ends the listening: the server closes, and so do the sessions. Run calls it when its context is done.
// The caller holds lifeMu.
func (h *Host) stop() {
	h.mu.Lock()
	srv := h.srv
	h.srv = nil
	h.mu.Unlock()
	if srv != nil {
		srv.Close()
	}
	h.closeAll()
}

// acceptLoop serves the connections srv accepts, until srv closes. A connection that arrives once srv no longer
// serves is destroyed: its key is no longer the one the host listens under.
func (h *Host) acceptLoop(srv *hyperdht.Server) {
	for {
		accepted, err := srv.Accept()
		if err != nil {
			return // srv closed: Run stopped, or a reload replaced it
		}
		conn := accepted.Conn
		if !h.trackServing(srv, conn) {
			conn.Destroy()
			return
		}
		if h.streamOptions != nil {
			h.streamOptions(secretstream.Options{Keepalive: conn.Keepalive()})
		}
		relayed := accepted.Relayed()
		go func() {
			if err := h.serve(conn, false, relayed); err != nil {
				h.log.Debug("session ended", "err", err)
			}
		}()
	}
}

// firewall returns the firewall of a server that admits clientPub.
func (h *Host) firewall(clientPub [32]byte) func(remote [32]byte, _ hyperdht.HandshakePayload) bool {
	return func(remote [32]byte, _ hyperdht.HandshakePayload) bool {
		return h.refuse(clientPub, remote)
	}
}

// refuse is the firewall for clientPub. It refuses every key but clientPub, and that key once it has
// SessionsPerKey live sessions (docs/architecture.md, Limits). A refused handshake gets no reply.
func (h *Host) refuse(clientPub, remote [32]byte) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if remote != clientPub {
		return true
	}
	if len(h.conns) >= h.limits.SessionsPerKey {
		h.log.Warn("HB-LIMIT-REACHED", "limit", "sessionsPerKey")
		return true
	}
	return false
}

// accept decides an open of the service name. An unknown service, or a UDP service, which uses flows,
// is rejected. Otherwise accept dials the target within the target connect timeout, and rejects with
// code 3 when the target refuses and code 4 when it does not answer in time. Reasons name no address and
// no service (spec/ipc.md, rule 9). An accepted stream counts on m, the meter of the link that accepted it, while
// it runs; a reattach moves it to the meter of the link that owns it (streamMeter), and its bytes count as they move.
func (h *Host) accept(name string, m *meter) mux.AcceptResult {
	h.mu.Lock()
	svc, ok := h.services[name]
	h.mu.Unlock()
	if !ok {
		return mux.AcceptResult{Code: rejectUnknownService, Reason: "unknown service"}
	}
	if svc.udp {
		return mux.AcceptResult{Code: rejectUnknownService, Reason: "udp service: use a flow"}
	}
	if h.touch != nil {
		h.touch(name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(h.limits.TargetConnectTimeout))
	defer cancel()
	conn, err := h.dial(ctx, "tcp", svc.addr)
	if err != nil {
		if ctx.Err() != nil {
			h.log.Warn("target timed out", "service", name)
			return mux.AcceptResult{Code: rejectTimeout, Reason: "target did not answer in time"}
		}
		h.log.Warn("target refused", "service", name)
		return mux.AcceptResult{Code: rejectRefused, Reason: "target refused the connection"}
	}
	return mux.AcceptResult{Target: func(st *mux.Stream) {
		sm := &streamMeter{st: st}
		h.beginStream(sm, m)
		defer h.endStream(sm)
		forward(st, conn, svc.idle, sm)
	}}
}

// trackServing records c as a live connection when srv is still the server that serves. It reports false when
// srv is no longer serving, and then records nothing.
func (h *Host) trackServing(srv *hyperdht.Server, c *hyperdht.Conn) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.srv != srv {
		return false
	}
	h.conns[c] = struct{}{}
	return true
}

// trackAdmitted records s, a LAN stream, as a live connection when its key is the one the host admits now, in the
// same step as the check, so that a reload which changes the key cannot miss it. It reports false otherwise.
func (h *Host) trackAdmitted(s *hyperdht.Conn) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s.RemotePublicKey() != h.clientPub {
		return false
	}
	h.conns[s] = struct{}{}
	return true
}

// untrack forgets a connection that ended. closeAll ends the live connections when the host stops listening
// under a key.
func (h *Host) untrack(c *hyperdht.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.conns, c)
}

func (h *Host) closeAll() {
	h.mu.Lock()
	conns := make([]*hyperdht.Conn, 0, len(h.conns))
	for c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.Unlock()
	for _, c := range conns {
		c.Destroy()
	}
}

// errors of a session that ends on the host's own decision.
var (
	errVersion   = errors.New("host: the app speaks another protocol version")
	errMalformed = errors.New("host: malformed handshake")
)
