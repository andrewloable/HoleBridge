package kinds

import (
	"context"
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
// is kept. detect probes a target ("host:port") as Detect does. clock is the time source for the
// one-minute re-checks.
func NewWatcher(cfg *config.Config, dir string, detect func(ctx context.Context, target string) (protocol.Kind, bool), clock func() time.Time) *Watcher {
	w := &Watcher{
		tick:   time.Second,
		dir:    dir,
		detect: detect,
		clock:  clock,
		wake:   make(chan struct{}, 1),
		svcs:   map[string]*svcState{},
	}
	// A state file that cannot be read starts the watcher empty; the next save replaces it.
	saved, err := config.LoadState(dir)
	if err != nil {
		saved = config.State{}
	}
	for name, s := range cfg.Services {
		st := &svcState{target: s.Target}
		if k := kindOf(s.Kind); k != protocol.KindUnknown {
			st.explicit, st.kind, st.settled = true, k, true
		} else {
			switch k := kindOf(saved.Kinds[name]); k {
			case protocol.KindHTTPS, protocol.KindHTTP, protocol.KindTCP:
				st.kind = k
			}
			// A loaded tcp kind is settled. A loaded web kind is not: it is checked again at start,
			// on Touch and by the minute timer, until a result classifies it.
			st.settled = st.kind == protocol.KindTCP
			st.due = true
		}
		w.svcs[name] = st
	}
	return w
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
		go func(name, target string) {
			defer wg.Done()
			w.probe(ctx, name, target)
		}(name, s.target)
	}
}

// probe runs one check of a service and applies its result. A change of kind is saved.
func (w *Watcher) probe(ctx context.Context, name, target string) {
	kind, conclusive := w.detect(ctx, target)
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.svcs[name]
	s.inFlight = false
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
