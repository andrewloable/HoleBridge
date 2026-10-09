package cli

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"syscall"

	"github.com/andrewloable/HoleBridge/internal/config"
	"github.com/andrewloable/HoleBridge/internal/host"
	"github.com/andrewloable/HoleBridge/internal/keys"
	"github.com/andrewloable/HoleBridge/internal/log"
	"github.com/andrewloable/HoleBridge/internal/status"
	"github.com/andrewloable/HoleBridge/pears/hyperdht"
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

// hostCmd is holebridge host. It runs the services of host.json until it is interrupted. It serves the control
// socket in the config directory, reloads host.json on SIGHUP and when service add or rm asks over the socket, and
// removes host.lock on the way out. It creates app.key and the host key on first use.
func hostCmd(args []string, env Env, configDir string) error {
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

	ctx, stop := hostContext()
	defer stop()
	logger := log.New(env.Stderr, slog.LevelInfo)
	d, err := hyperdht.New(hyperdht.Config{Bootstrap: bootstrap})
	if err != nil {
		return err
	}
	defer d.Close()
	h, err := host.New(cfg, appKey, host.Options{DHT: d, Dir: dir, Log: logger})
	if err != nil {
		return err
	}
	ctl := &control{h: h, dir: dir, dht: d, cfg: cfg}

	lock := filepath.Join(dir, "host.lock")
	if err := os.WriteFile(lock, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		return err
	}
	defer os.Remove(lock)

	// runCtx ends when the host is interrupted or when the control socket fails, so that a failed Serve stops the host.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	served := make(chan error, 1)
	go func() {
		err := status.Serve(runCtx, dir, ctl)
		if err != nil {
			cancel()
		}
		served <- err
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

	if err := h.Run(runCtx); err != nil {
		cancel()
		<-served
		return err
	}
	cancel()
	return <-served
}

// control is the handler of the control socket. Reload re-reads host.json into the host. Status answers from the
// config the host last applied, the DHT node's NAT state and the relay setting. The host does not yet report its
// sessions, so Status lists none.
type control struct {
	h   *host.Host
	dir string
	dht *hyperdht.DHT

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

// Status returns the services of the applied config with their kinds, the NAT state and whether a relay is set.
// A kind is the one host.json names, else the one detected and saved in kinds.json, else "detecting". A kinds.json
// that cannot be read leaves every kind as "detecting": the kinds are a display detail, not part of the service.
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
	st := status.Status{NAT: c.dht.NAT(), Relay: cfg.Relay != ""}
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
	return st
}
