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
// a service with no kind in host.json, since an explicit kind is never re-probed; nil means unknown.
type Options struct {
	DHT   *hyperdht.DHT
	Dial  func(ctx context.Context, network, addr string) (net.Conn, error)
	Clock func() time.Time
	Log   *slog.Logger
	Kinds func(service string) protocol.Kind

	// Dir is the config directory whose host.json Reload re-reads.
	Dir string

	// streamOptions is a test hook. When set, the host calls it with the secret-stream options each session's
	// stream is set up with. Nil in production.
	streamOptions func(secretstream.Options)
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
	kinds         func(service string) protocol.Kind // the kinds watcher, or nil
	streamOptions func(secretstream.Options)         // test hook: the options each DHT session is set up with
	limits        config.Limits
	kp            noise.KeyPair      // the host key pair the server listens under
	clientPub     [32]byte           // the one key the firewall admits
	lan           bool               // the LAN route is on: each handshake carries the LAN block
	lanPort       uint64             // the LAN TCP port, from host.json
	names         []string           // the service names, sorted: the handshake lists them in this order
	services      map[string]service // by name: what an open is dialed to
	budget        *mux.Budget        // the receive budget, shared by the process's sessions
	streams       *mux.Counter       // the streams in total, shared by the process's sessions
	udp           *udpflow.Counter   // the UDP flows in total, shared by the process's sessions
	resume        *mux.ResumeTable   // the streams of every session; a dropped session's streams wait here for a reattach

	mu    sync.Mutex
	conns map[*hyperdht.Conn]struct{} // the live connections: one session each, for the one key
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
	h := &Host{
		log:           opts.Log,
		dht:           opts.DHT,
		dial:          opts.Dial,
		clock:         opts.Clock,
		kinds:         opts.Kinds,
		streamOptions: opts.streamOptions,
		limits:        cfg.Limits,
		lan:           cfg.LAN.Enabled == nil || *cfg.LAN.Enabled,
		lanPort:       uint64(cfg.LAN.Port),
		services:      map[string]service{},
		budget:        mux.NewBudget(uint64(cfg.Limits.ReceiveBudget)),
		streams:       mux.NewCounter(cfg.Limits.StreamsTotal),
		udp:           udpflow.NewCounter(cfg.Limits.UDPFlowsTotal),
		conns:         map[*hyperdht.Conn]struct{}{},
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

	// The names are sorted, so the handshake lists the services in the same order on every connection.
	for name := range cfg.Services {
		h.names = append(h.names, name)
	}
	sort.Strings(h.names)
	for _, name := range h.names {
		s := cfg.Services[name]
		target, port, err := config.ParseTarget(s.Target)
		if err != nil {
			return nil, err
		}
		kind := kindOf(s.Kind)
		h.services[name] = service{
			addr:    net.JoinHostPort(target, strconv.Itoa(port)),
			udp:     kind == protocol.KindUDP,
			kind:    kind,
			port:    uint64(port),
			origins: s.Origins,
			idle:    time.Duration(s.Idle),
		}
	}
	return h, nil
}

// handshake returns the services handshake a session's app is sent when its channel opens. A service's kind
// is the one host.json names; a service with none takes the kind of the Kinds option, or unknown
// (docs/architecture.md, Service kinds). The LAN block is there while the LAN route is on. It is built per
// session, so a kind learned mid-session shows at the next session start.
func (h *Host) handshake() protocol.Handshake {
	hs := protocol.Handshake{Version: protocolVersion, Flags: hostFlags}
	for _, name := range h.names {
		s := h.services[name]
		kind := s.kind
		if kind == protocol.KindUnknown && h.kinds != nil {
			kind = h.kinds(name)
		}
		hs.Services = append(hs.Services, protocol.Service{Name: name, Kind: kind, Port: s.port, Origins: s.origins})
	}
	if h.lan {
		hs.Flags |= protocol.FlagLAN
		hs.LAN = &protocol.LAN{Addresses: lanAddresses(), Port: h.lanPort}
	}
	return hs
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
// when ctx is done and an error when the server cannot listen.
func (h *Host) Run(ctx context.Context) error {
	srv := h.dht.CreateServer(hyperdht.ServerOptions{Firewall: h.refuse, Keepalive: keepalive})
	defer srv.Close()
	if err := srv.Listen(ctx, h.kp); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("host: listen: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { srv.Close() })
	defer stop()
	h.log.Info("listening")

	for {
		conn, err := srv.Accept()
		if err != nil {
			h.closeAll()
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("host: accept: %w", err)
		}
		h.track(conn)
		if h.streamOptions != nil {
			h.streamOptions(secretstream.Options{Keepalive: conn.Keepalive()})
		}
		go func() {
			if err := h.serve(conn, false); err != nil {
				h.log.Debug("session ended", "err", err)
			}
		}()
	}
}

// refuse is the server's firewall. It refuses every key but the client key pair, and refuses that key once
// it has SessionsPerKey live sessions (docs/architecture.md, Limits). A refused handshake gets no reply.
func (h *Host) refuse(remote [32]byte, _ hyperdht.HandshakePayload) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if remote != h.clientPub {
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
// no service (spec/ipc.md, rule 9).
func (h *Host) accept(name string) mux.AcceptResult {
	svc, ok := h.services[name]
	if !ok {
		return mux.AcceptResult{Code: rejectUnknownService, Reason: "unknown service"}
	}
	if svc.udp {
		return mux.AcceptResult{Code: rejectUnknownService, Reason: "udp service: use a flow"}
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
	return mux.AcceptResult{Target: func(st *mux.Stream) { forward(st, conn, svc.idle) }}
}

// track and untrack record the live connections; closeAll ends them when Run returns.
func (h *Host) track(c *hyperdht.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.conns[c] = struct{}{}
}

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
