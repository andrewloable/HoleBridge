package kinds

import (
	"context"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/andrewloable/HoleBridge/internal/config"
	"github.com/andrewloable/HoleBridge/internal/protocol"
)

// Watcher keeps the kind of each service of the host config, from Detect, under the rules of
// docs/architecture.md, "Service kinds". Explicit kinds are never probed. A known https or http kind
// becomes tcp only after two consecutive tcp results. Inconclusive probes keep the previous kind. The
// detected kinds are saved in kinds.json in the config directory and loaded at start.
type Watcher struct {
	// tick is how often Run checks its timers. The design value is one second; tests shorten it.
	tick time.Duration

	dir    string
	detect func(ctx context.Context, target string) (protocol.Kind, bool)
	clock  func() time.Time
	wake   chan struct{} // Touch signals here, without blocking, so Run checks at once

	mu   sync.Mutex
	svcs map[string]*svcState
}

// svcState is what the watcher knows about one service.
type svcState struct {
	target   string
	explicit bool          // the kind is set in host.json: never probed
	kind     protocol.Kind // what Kind reports
	settled  bool          // classified: no re-check on Touch or by the minute timer
	streak   int           // consecutive tcp results since the last other result
	due      bool          // a check was asked for (at start, or by Touch) and not started yet
	inFlight bool          // a probe is running
	last     time.Time     // when the last probe started
}

// NewWatcher returns a watcher for the services of cfg. dir is the config directory, where kinds.json
// is kept; "" keeps nothing on disk (share). detect probes a target ("host:port") as Detect does. clock is
// the time source for the one-minute re-checks.
func NewWatcher(cfg *config.Config, dir string, detect func(ctx context.Context, target string) (protocol.Kind, bool), clock func() time.Time) *Watcher {
	w := &Watcher{
		tick:   time.Second,
		dir:    dir,
		detect: detect,
		clock:  clock,
		wake:   make(chan struct{}, 1),
		svcs:   map[string]*svcState{},
	}
	saved := loadSaved(dir)
	for name, s := range cfg.Services {
		w.svcs[name] = newSvcState(name, s, saved)
	}
	return w
}

// loadSaved returns the kinds saved in dir. A state file that cannot be read, and dir "", give an empty State;
// the next save replaces the file.
func loadSaved(dir string) config.State {
	if dir == "" {
		return config.State{}
	}
	saved, err := config.LoadState(dir)
	if err != nil {
		return config.State{}
	}
	return saved
}

// newSvcState returns the state that a service of a config starts with, as NewWatcher and Reload build it. An
// explicit kind is settled and never probed. Otherwise the kind saved for the service is the starting kind: a saved
// tcp kind is settled, and a saved web kind, or no kind, is checked again at start, on Touch and by the minute timer.
func newSvcState(name string, s config.Service, saved config.State) *svcState {
	st := &svcState{target: s.Target}
	if k := kindOf(s.Kind); k != protocol.KindUnknown {
		st.explicit, st.kind, st.settled = true, k, true
		return st
	}
	switch k := kindOf(saved.Kinds[name]); k {
	case protocol.KindHTTPS, protocol.KindHTTP, protocol.KindTCP:
		st.kind = k
	}
	st.settled = st.kind == protocol.KindTCP
	st.due = true
	return st
}

// changed reports whether the config entry e differs from the one s was built from, in its target or its explicit
// kind. A detected service with no explicit kind in e is unchanged while its target is.
func (s *svcState) changed(e config.Service) bool {
	k := kindOf(e.Kind)
	return s.target != e.Target || s.explicit != (k != protocol.KindUnknown) || (s.explicit && s.kind != k)
}

// Reload applies cfg to the watcher, as the host applies a reload of host.json. A service the watcher does not know,
// and one whose target or explicit kind changed, gets the state NewWatcher gives a new service. A service that did
// not change keeps its state, and is not probed again. A service that cfg no longer names is dropped. The kinds are
// saved, and Run is woken so that the new services are checked at once.
func (w *Watcher) Reload(cfg *config.Config) {
	w.mu.Lock()
	defer w.mu.Unlock()
	saved := loadSaved(w.dir)
	for name := range w.svcs {
		if _, ok := cfg.Services[name]; !ok {
			delete(w.svcs, name)
		}
	}
	for name, s := range cfg.Services {
		if old, ok := w.svcs[name]; ok && !old.changed(s) {
			continue
		}
		w.svcs[name] = newSvcState(name, s, saved)
	}
	w.save()
	select {
	case w.wake <- struct{}{}:
	default: // a wake is already pending
	}
}

// Kind returns the kind of service: its explicit kind, else its detected kind, else unknown.
func (w *Watcher) Kind(service string) protocol.Kind {
	w.mu.Lock()
	defer w.mu.Unlock()
	if s, ok := w.svcs[service]; ok {
		return s.kind
	}
	return protocol.KindUnknown
}

// Touch is called when a stream to service opens. An unclassified service (unknown, or one tcp result
// after a web kind) is re-checked at once.
func (w *Watcher) Touch(service string) {
	w.mu.Lock()
	s, ok := w.svcs[service]
	asked := ok && !s.explicit && !s.settled
	if asked {
		s.due = true
	}
	w.mu.Unlock()
	if asked {
		select {
		case w.wake <- struct{}{}:
		default: // a wake is already pending
		}
	}
}

// Run checks each service without an explicit kind once when it starts, then re-checks the unclassified
// ones on Touch and once a minute, until ctx is done. It returns nil when ctx is done.
func (w *Watcher) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	defer wg.Wait() // probes end with ctx, and their results are saved before Run returns
	ticker := time.NewTicker(w.tick)
	defer ticker.Stop()
	w.dispatch(ctx, &wg)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-w.wake:
		}
		w.dispatch(ctx, &wg)
	}
}

// dispatch starts a probe for each service that is due, and for each unsettled one whose minute is up.
// A service with a probe in flight waits for it.
func (w *Watcher) dispatch(ctx context.Context, wg *sync.WaitGroup) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.clock()
	for name, s := range w.svcs {
		if s.explicit || s.inFlight {
			continue
		}
		if !s.due && (s.settled || now.Sub(s.last) < time.Minute) {
			continue
		}
		s.due, s.inFlight, s.last = false, true, now
		wg.Add(1)
		go func(name string, s *svcState, target string) {
			defer wg.Done()
			w.probe(ctx, name, s, target)
		}(name, s, s.target)
	}
}

// probe runs one check of a service and applies its result to s, the state the check was started for. A change of
// kind is saved. When Reload replaced or removed the service meanwhile, the result is dropped.
func (w *Watcher) probe(ctx context.Context, name string, s *svcState, target string) {
	kind, conclusive := w.detect(ctx, dialAddr(target))
	w.mu.Lock()
	defer w.mu.Unlock()
	s.inFlight = false
	if w.svcs[name] != s {
		return
	}
	before := s.kind
	s.apply(kind, conclusive)
	if s.settled {
		s.due = false // a Touch that came during the probe is answered by this result
	}
	if s.kind != before {
		w.save()
	}
}

// apply folds one probe result into the service. An inconclusive result keeps the kind and breaks the tcp
// streak. A web kind becomes tcp only after two consecutive tcp results, and one tcp result leaves it
// unsettled. A conclusive web result settles the service.
func (s *svcState) apply(kind protocol.Kind, conclusive bool) {
	switch {
	case !conclusive:
		s.streak = 0
	case kind == protocol.KindHTTPS || kind == protocol.KindHTTP:
		s.kind, s.streak, s.settled = kind, 0, true
	case kind == protocol.KindTCP:
		s.streak++
		if (s.kind == protocol.KindHTTPS || s.kind == protocol.KindHTTP) && s.streak < 2 {
			s.settled = false
			return
		}
		s.kind, s.settled = protocol.KindTCP, true
	}
}

// save writes the detected kinds of the services without an explicit kind to kinds.json. Called with mu
// held. A failed save only loses the restart hint, so the watcher does not stop; the next change saves again.
func (w *Watcher) save() {
	if w.dir == "" {
		return // share keeps nothing on disk
	}
	st := config.State{Kinds: map[string]string{}}
	for name, s := range w.svcs {
		if n := kindName(s.kind); !s.explicit && n != "" {
			st.Kinds[name] = n
		}
	}
	_ = config.SaveState(w.dir, st)
}

// kindOf maps a kind name, as host.json and kinds.json spell it, to its wire kind. An empty or unknown
// name is KindUnknown.
func kindOf(name string) protocol.Kind {
	switch name {
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

// dialAddr returns the host:port that the host dials for a target as host.json writes it. A bare port means
// 127.0.0.1:port, as config.ParseTarget reads it and internal/host dials it. A target that ParseTarget refuses
// is returned as written.
func dialAddr(target string) string {
	host, port, err := config.ParseTarget(target)
	if err != nil {
		return target
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// kindName is the name of a detected kind in kinds.json, or "" for a kind that is never detected.
func kindName(k protocol.Kind) string {
	switch k {
	case protocol.KindHTTPS:
		return "https"
	case protocol.KindHTTP:
		return "http"
	case protocol.KindTCP:
		return "tcp"
	}
	return ""
}
