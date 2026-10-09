package cli

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/andrewloable/HoleBridge/internal/config"
	"github.com/andrewloable/HoleBridge/internal/errs"
	"github.com/andrewloable/HoleBridge/internal/host"
	"github.com/andrewloable/HoleBridge/internal/keys"
	"github.com/andrewloable/HoleBridge/internal/kinds"
	"github.com/andrewloable/HoleBridge/internal/lan"
	"github.com/andrewloable/HoleBridge/internal/log"
	"github.com/andrewloable/HoleBridge/internal/protocol"
	"github.com/andrewloable/HoleBridge/internal/status"
	"github.com/andrewloable/HoleBridge/pears/dhtrpc"
	"github.com/andrewloable/HoleBridge/pears/hyperdht"
	"github.com/andrewloable/HoleBridge/pears/noise"
)

// The host command registers itself here, as the other commands do.
func init() {
	commands["host"] = hostCmd
}

// hostContext returns the context that a host runs under: it ends on an interrupt or a termination signal.
// Tests replace it with a context they cancel.
var hostContext = func() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// hostCmd is holebridge host. It runs the services of host.json until it is interrupted, through the default runner
// (see hostWith).
func hostCmd(args []string, env Env, configDir string) error {
	return hostWith(defaultHostRunner{})(args, env, configDir)
}

// hostWith returns the host command, which runs its host through r (see hostRunner). The command creates app.key and
// the host key on first use, refuses when a live host already serves the config directory, and writes host.lock while
// the host runs. It prints the banner (docs/cli.md, hosting) and waits until the host stops, which an interrupt or a
// termination signal causes. When the runner can report status and reload (the default runner can), the command
// serves the control socket in the config directory, reloads on SIGHUP and removes the socket on the way out.
func hostWith(r hostRunner) Command {
	return func(args []string, env Env, configDir string) error {
		words, _, err := splitFlags(args)
		if err != nil {
			return err
		}
		if len(words) != 0 {
			return usage("host takes no arguments")
		}
		dir, err := requireDir(configDir, env)
		if err != nil {
			return err
		}
		nodes, err := bootstrapFromEnv(env)
		if err != nil {
			return err
		}
		appKey, err := loadOrCreateAppKey(dir)
		if err != nil {
			return err
		}
		cfg, err := loadHost(dir, true)
		if err != nil {
			return err
		}
		if cfg.Key == "" {
			cfg.Key = keys.Generate()
			if err := config.Save(dir, cfg); err != nil {
				return err
			}
		}
		// host.lock is written only when no live host holds it, so a second host does not take over the first one's
		// lock. The control socket is the real guard: its Serve fails when another one already serves dir.
		if hostRunning(dir) {
			return errors.New("a holebridge host is already running in this config directory")
		}
		block, err := keyBlock(cfg.Key, appKey)
		if err != nil {
			return err
		}

		ctx, stop := hostContext()
		defer stop()
		// runCtx ends when the host is interrupted, or when the control socket fails, so that a failed Serve stops the host.
		runCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		logger := log.New(env.Stderr, env.LogLevel)

		lock := filepath.Join(dir, "host.lock")
		if err := os.WriteFile(lock, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
			return err
		}
		defer os.Remove(lock)

		rh, err := r.Start(runCtx, hostRequest{
			Dir:       dir,
			Config:    cfg,
			AppKey:    appKey,
			LAN:       lanEnabled(cfg),
			Bootstrap: nodes,
			Logger:    logger,
		})
		if err != nil {
			return err
		}

		// When the runner can report status and reload, the control socket runs beside the host. The banner waits
		// until the socket answers, so a host that cannot serve prints no banner.
		ctl, hasCtl := rh.(status.Handler)
		var served chan struct{} // closed when Serve has returned
		var serveErr error
		if hasCtl {
			served = make(chan struct{})
			go func() {
				serveErr = status.Serve(runCtx, dir, ctl)
				if serveErr != nil {
					cancel()
				}
				close(served)
			}()
			hup := make(chan os.Signal, 1)
			signal.Notify(hup, syscall.SIGHUP)
			defer signal.Stop(hup)
			go func() {
				for {
					select {
					case <-runCtx.Done():
						return
					case <-hup:
						if err := ctl.Reload(); err != nil {
							logger.Error("reload failed", "error", err)
						}
					}
				}
			}()
			if err := awaitControl(dir, served); err != nil {
				cancel()
				rh.Wait()
				<-served
				if serveErr != nil {
					return serveErr
				}
				if errors.Is(err, errControlEnded) {
					return nil // interrupted before the socket answered
				}
				return err
			}
		}

		state, _ := config.LoadState(dir)
		printBanner(env.Stdout, hostingLine(cfg, state), block, rh, lanEnabled(cfg), cfg.Relay != "")

		err = rh.Wait()
		cancel()
		if hasCtl {
			<-served
			if err == nil {
				err = serveErr
			}
		}
		return err
	}
}

// controlWait bounds how long the host command waits for its control socket to answer before the banner.
const controlWait = 10 * time.Second

// errControlEnded is what awaitControl returns when the control socket's Serve ends before the socket answers.
var errControlEnded = errors.New("the control socket stopped before it answered")

// awaitControl waits until the control socket in dir answers a status request. It returns errControlEnded when
// served closes first, and an error when the socket does not answer within controlWait.
func awaitControl(dir string, served <-chan struct{}) error {
	deadline := time.Now().Add(controlWait)
	for {
		if _, err := status.Query(dir, "status"); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("the control socket does not answer")
		}
		select {
		case <-served:
			return errControlEnded
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// lanEnabled reports whether the LAN route is on for cfg: lan.enabled, which is true unless host.json says false.
func lanEnabled(cfg *config.Config) bool {
	return cfg.LAN.Enabled == nil || *cfg.LAN.Enabled
}

// printBanner prints the banner of a running host or share: its first line (Hosting or Sharing), the key block, and
// the LAN, Internet and Relay lines (docs/cli.md, hosting). The key block is built before the host starts, so a QR
// code that cannot be made stops the command before anything runs.
func printBanner(out io.Writer, first, block string, rh runningHost, lanOn, relaySet bool) {
	fmt.Fprintln(out, first)
	fmt.Fprint(out, block)
	if lanOn {
		addr := rh.LANAddr()
		if addr == "" {
			addr = "no LAN address on this machine"
		}
		fmt.Fprintf(out, "LAN: listening on %s\n", addr)
	} else {
		fmt.Fprintln(out, "LAN: off")
	}
	fmt.Fprintln(out, internetLine(rh.NAT()))
	if relaySet {
		fmt.Fprintln(out, "Relay: set")
	} else {
		fmt.Fprintln(out, "Relay: none set")
	}
}

// internetLine returns the Internet line of the banner from the node's NAT state, worded as the status line is (see
// natLine in status.go), without the address. A state that no peer has reported yet is unknown.
func internetLine(n dhtrpc.NATInfo) string {
	switch {
	case n.Host == "":
		return "Internet: unknown (no peer has reported our address yet)"
	case n.Randomized:
		return "Internet: not reachable (NAT: random, the ports change, so a relay may be needed)"
	case n.Firewalled:
		return "Internet: not reachable (no ping has reached us from outside yet)"
	}
	return "Internet: reachable (NAT: consistent)"
}

// hostingLine returns the first line of a host's banner: the services, sorted by name, each with its kind. A kind is
// the one host.json names, else the one detected into kinds.json, else "detecting".
func hostingLine(cfg *config.Config, state config.State) string {
	names := slices.Sorted(maps.Keys(cfg.Services))
	if len(names) == 0 {
		return "Hosting no services, add one with holebridge service add"
	}
	parts := make([]string, 0, len(names))
	for _, name := range names {
		kind := cfg.Services[name].Kind
		if kind == "" {
			kind = state.Kinds[name]
		}
		if kind == "" {
			kind = "detecting"
		}
		parts = append(parts, name+" ("+kind+")")
	}
	noun := "services"
	if len(names) == 1 {
		noun = "service"
	}
	return fmt.Sprintf("Hosting %d %s: %s", len(names), noun, strings.Join(parts, ", "))
}

// control is the handler of the control socket. Reload re-reads host.json into the host. Status answers from the
// config the host last applied, the DHT node's NAT state, the relay setting the host started with and the host's
// live sessions.
type control struct {
	h     *host.Host
	dir   string
	dht   *hyperdht.DHT
	relay bool // whether host.json had a relay when the host started: set once, so a reload does not change it

	mu  sync.Mutex
	cfg *config.Config // the config the host runs: set at start and after each reload
}

// Reload applies host.json to the running host, then records the config it applied.
func (c *control) Reload() error {
	if err := c.h.Reload(); err != nil {
		return err
	}
	cfg, err := config.Load(c.dir)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.cfg = cfg
	c.mu.Unlock()
	return nil
}

// Status returns the services of the applied config with their kinds, the NAT state, whether a relay was set when the
// host started (not what host.json says now) and the live sessions of the host. A kind is the one host.json names,
// else the one detected and saved in kinds.json, else "detecting". A kinds.json that cannot be read leaves every kind
// as "detecting": the kinds are a display detail, not part of the service.
func (c *control) Status() status.Status {
	c.mu.Lock()
	cfg := c.cfg
	c.mu.Unlock()

	state, _ := config.LoadState(c.dir)
	names := make([]string, 0, len(cfg.Services))
	for name := range cfg.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	st := status.Status{NAT: c.dht.NAT(), Relay: c.relay, Sessions: []status.Session{}}
	for _, name := range names {
		kind := cfg.Services[name].Kind
		if kind == "" {
			kind = state.Kinds[name]
		}
		if kind == "" {
			kind = "detecting"
		}
		st.Services = append(st.Services, status.Service{Name: name, Kind: kind})
	}
	for _, s := range c.h.Sessions() {
		st.Sessions = append(st.Sessions, status.Session{Route: s.Route, Streams: s.Streams, Flows: s.Flows, BytesIn: s.BytesIn, BytesOut: s.BytesOut})
	}
	return st
}

// hostRequest is what holebridge host and share start: the config and the application key, the config directory
// (empty for share, which keeps nothing on disk: no host.json, no control socket, no host.lock), whether the LAN
// responder and listener run, the bootstrap nodes of the DHT node, and the logger.
type hostRequest struct {
	Dir       string
	Config    *config.Config
	AppKey    [32]byte
	LAN       bool
	Bootstrap []string
	Logger    *slog.Logger
}

// runningHost is a host that a hostRunner started, as the banner and the command see it.
type runningHost interface {
	NAT() dhtrpc.NATInfo // the node's NAT state, for the Internet line of the banner
	LANAddr() string     // the LAN addresses the LAN listener serves on, comma-separated; "" when none is found
	Wait() error         // returns once the host has stopped after its context ended, and releases what it holds
}

// hostRunner starts the host of a request and returns once it serves. The host runs until ctx is done. The default
// runner is internal/host with the LAN responder and listener (unless the request says no LAN). Tests replace it
// with a fake, so that no test starts a network node.
type hostRunner interface {
	Start(ctx context.Context, req hostRequest) (runningHost, error)
}

// defaultHostRunner is the hostRunner that the host and share commands of the binary run under: internal/host on a
// DHT node, with the LAN responder and listener when the request turns LAN on.
type defaultHostRunner struct{}

// hostDHTConfig returns the configuration of the DHT node a host runs on: the bootstrap nodes, and with a relay in cfg,
// the member key pair of that relay as the default key pair, so that the relay admits the host's relayed dials
// (docs/architecture.md, Relay route). Without a relay the default key pair is left as the DHT makes it. It does not
// start the node, so the choice can be tested without the network.
func hostDHTConfig(cfg *config.Config, appKey [32]byte, bootstrap []string) (hyperdht.Config, error) {
	kp, err := host.DefaultKeyPair(cfg.Relay, appKey)
	if err != nil {
		return hyperdht.Config{}, err
	}
	return hyperdht.Config{Bootstrap: bootstrap, DefaultKeyPair: kp}, nil
}

// Start starts the DHT node and the host, binds the LAN ports when the request turns LAN on, and runs the host in the
// background until ctx ends or the host fails. A LAN port that another program holds fails with HB-LAN-PORT-IN-USE,
// before anything serves. The kinds watcher checks the services that have no kind; its state is kinds.json in req.Dir,
// and nothing is kept on disk when req.Dir is empty.
func (defaultHostRunner) Start(ctx context.Context, req hostRequest) (runningHost, error) {
	dhtCfg, err := hostDHTConfig(req.Config, req.AppKey, req.Bootstrap)
	if err != nil {
		return nil, err
	}
	d, err := hyperdht.New(dhtCfg)
	if err != nil {
		return nil, err
	}
	w := kinds.NewWatcher(req.Config, req.Dir, func(ctx context.Context, target string) (protocol.Kind, bool) {
		return kinds.Detect(ctx, target, kinds.Options{})
	}, time.Now)
	h, err := host.New(req.Config, req.AppKey, host.Options{DHT: d, Dir: req.Dir, Log: req.Logger, Kinds: w.Kind, Touch: w.Touch})
	if err != nil {
		d.Close()
		return nil, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	lh := &liveHost{
		control: &control{h: h, dir: req.Dir, dht: d, relay: req.Config.Relay != "", cfg: req.Config},
		start:   req.Config,
		appKey:  req.AppKey,
		logger:  req.Logger,
		ctx:     runCtx,
		cancel:  cancel,
		stopped: make(chan struct{}),
		watcher: w,
		watched: make(chan struct{}),
	}
	if req.LAN {
		route, err := startLAN(runCtx, h, req.Config, req.AppKey, req.Logger)
		if err != nil {
			cancel()
			d.Close()
			return nil, err
		}
		lh.lan, lh.key = route, req.Config.Key
	}
	go func() {
		err := h.Run(runCtx)
		lh.runErr = err
		cancel() // a host that failed to listen stops its LAN route too
		close(lh.stopped)
	}()
	go func() {
		w.Run(runCtx) // returns once runCtx ends, after its in-flight probes
		close(lh.watched)
	}()
	return lh, nil
}

// liveHost is a host that defaultHostRunner started: the runningHost of the command, and the handler of the control
// socket. Its Reload also moves the LAN route to a changed key, since the LAN listener is keyed by the host key and
// internal/host does not move it. Its Reload also gives the kinds watcher the applied config, so a service added while
// the host runs is checked at once.
type liveHost struct {
	*control
	start   *config.Config // the config the host started with: the LAN ports and limits are read at start
	appKey  [32]byte
	logger  *slog.Logger
	ctx     context.Context    // the context the host and its LAN route run under
	cancel  context.CancelFunc // ends the host and its LAN route
	stopped chan struct{}      // closed when the host's Run has returned
	runErr  error              // Run's error, read after stopped
	watcher *kinds.Watcher     // the kinds of the services with no kind
	watched chan struct{}      // closed when the watcher's Run has returned

	lanMu sync.Mutex
	lan   *lanRoute // nil while the LAN route is off
	key   string    // the host key the LAN route listens under
}

// NAT returns the node's NAT state.
func (lh *liveHost) NAT() dhtrpc.NATInfo {
	return lh.dht.NAT()
}

// LANAddr returns the host's LAN addresses, or "" while the LAN route is off.
func (lh *liveHost) LANAddr() string {
	lh.lanMu.Lock()
	defer lh.lanMu.Unlock()
	if lh.lan == nil {
		return ""
	}
	return strings.Join(host.LANAddresses(), ", ")
}

// Reload applies host.json to the host. When the host key changed, the LAN route is moved to the new key: it closes
// and listens again under the new key, on the ports the host started with.
func (lh *liveHost) Reload() error {
	if err := lh.control.Reload(); err != nil {
		return err
	}
	lh.control.mu.Lock()
	applied := lh.control.cfg
	lh.control.mu.Unlock()
	lh.watcher.Reload(applied)

	lh.lanMu.Lock()
	defer lh.lanMu.Unlock()
	if lh.lan == nil {
		return nil
	}
	cfg, err := config.Load(lh.dir)
	if err != nil {
		return err
	}
	if cfg.Key == lh.key {
		return nil
	}
	lh.lan.close()
	lh.lan = nil
	lanCfg := *lh.start
	lanCfg.Key = cfg.Key
	route, err := startLAN(lh.ctx, lh.control.h, &lanCfg, lh.appKey, lh.logger)
	if err != nil {
		return err
	}
	lh.lan, lh.key = route, cfg.Key
	return nil
}

// Wait returns once the host and the kinds watcher have stopped, then closes the LAN route and the DHT node. The
// watcher stops with the host's context, and its probes end before its Run returns, so no probe outlives Wait.
func (lh *liveHost) Wait() error {
	<-lh.stopped
	<-lh.watched
	lh.lanMu.Lock()
	if lh.lan != nil {
		lh.lan.close()
		lh.lan = nil
	}
	lh.lanMu.Unlock()
	lh.dht.Close()
	return lh.runErr
}

// lanRoute is the LAN route of a running host: the UDP responder that answers signed probes with the TCP port, and the
// TCP listener whose admitted streams the host serves (docs/architecture.md, LAN route).
type lanRoute struct {
	udp    net.PacketConn
	tcp    net.Listener
	cancel context.CancelFunc
}

// startLAN binds the LAN UDP discovery port and the LAN TCP port of cfg, under the host key that cfg and appKey derive,
// and serves them until ctx ends: probes are answered, and each admitted stream is served by h. A port that another
// program holds fails with HB-LAN-PORT-IN-USE.
func startLAN(ctx context.Context, h *host.Host, cfg *config.Config, appKey [32]byte, logger *slog.Logger) (*lanRoute, error) {
	d, err := keys.Derive(cfg.Key, appKey)
	if err != nil {
		return nil, err
	}
	udp, err := net.ListenPacket("udp", ":"+strconv.Itoa(cfg.LAN.DiscoveryPort))
	if err != nil {
		return nil, lanPortError("udp", cfg.LAN.DiscoveryPort, err)
	}
	tcp, err := net.Listen("tcp", ":"+strconv.Itoa(cfg.LAN.Port))
	if err != nil {
		udp.Close()
		return nil, lanPortError("tcp", cfg.LAN.Port, err)
	}

	var hostKP noise.KeyPair
	copy(hostKP.Public[:], d.Host.Public().(ed25519.PublicKey))
	copy(hostKP.Secret[:], d.Host)
	var clientPub [32]byte
	copy(clientPub[:], d.Client.Public().(ed25519.PublicKey))

	lanCtx, cancel := context.WithCancel(ctx)
	lis := lan.NewListener(tcp, hostKP, func(remote [32]byte) bool { return remote == clientPub }, lan.ListenerConfig{
		HandshakeDeadline: time.Duration(cfg.Limits.LANHandshakeDeadline),
		MaxUnauth:         cfg.Limits.UnauthLANTotal,
		MaxUnauthPerIP:    cfg.Limits.UnauthLANPerIP,
		Log:               logger,
	})
	go func() {
		for {
			s, err := lis.Accept()
			if err != nil {
				return // the TCP listener is closed
			}
			go h.ServeConn(lanCtx, s)
		}
	}()
	responder := lan.NewResponder(d.LAN.Reveal(), uint16(cfg.LAN.Port), udp, time.Now)
	go responder.Serve(lanCtx)
	return &lanRoute{udp: udp, tcp: tcp, cancel: cancel}, nil
}

// close stops the LAN route and frees its ports.
func (l *lanRoute) close() {
	l.cancel()
	l.tcp.Close()
	l.udp.Close()
}

// wsaEADDRINUSE is WSAEADDRINUSE, the bind error on Windows. syscall.EADDRINUSE is not the errno a bind returns there.
const wsaEADDRINUSE = syscall.Errno(10048)

// lanPortError returns the error of a LAN port that a bind could not take: HB-LAN-PORT-IN-USE, which names the port,
// when another program holds it, and the bind error otherwise.
func lanPortError(network string, port int, err error) error {
	if errors.Is(err, syscall.EADDRINUSE) || errors.Is(err, wsaEADDRINUSE) {
		return errs.E("HB-LAN-PORT-IN-USE", network+" "+strconv.Itoa(port), err)
	}
	return err
}
