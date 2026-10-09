package kinds

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/internal/config"
	"github.com/andrewloable/HoleBridge/internal/protocol"
)

// answer is one scripted result of a detect call.
type answer struct {
	kind       protocol.Kind
	conclusive bool
}

var (
	inconclusive = answer{protocol.KindUnknown, false}
	tcpAnswer    = answer{protocol.KindTCP, true}
	httpAnswer   = answer{protocol.KindHTTP, true}
	httpsAnswer  = answer{protocol.KindHTTPS, true}
)

// fakeDetect stands in for Detect. Each target has a script of answers: a call takes the next one, and
// the last one repeats. A target with no script is inconclusive. It counts the calls per target.
type fakeDetect struct {
	mu     sync.Mutex
	script map[string][]answer
	calls  map[string]int
}

func newFakeDetect(script map[string][]answer) *fakeDetect {
	return &fakeDetect{script: script, calls: map[string]int{}}
}

func (f *fakeDetect) detect(_ context.Context, target string) (protocol.Kind, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[target]++
	s := f.script[target]
	if len(s) == 0 {
		return protocol.KindUnknown, false
	}
	a := s[0]
	if len(s) > 1 {
		f.script[target] = s[1:]
	}
	return a.kind, a.conclusive
}

// count returns how many times target was probed.
func (f *fakeDetect) count(target string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[target]
}

// fakeClock is a clock the test moves by hand. Run reads it from its own goroutine.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// failOnPanic turns a stub's panic into a failure of the calling test, so each test reports on its own.
// Defer it in the test function itself.
func failOnPanic(t *testing.T) {
	t.Helper()
	if r := recover(); r != nil {
		t.Fatalf("panic: %v", r)
	}
}

// newTestWatcher builds a watcher whose timers check every 5 ms, so a test does not wait a second per tick.
func newTestWatcher(cfg *config.Config, dir string, f *fakeDetect, clk *fakeClock) *Watcher {
	w := NewWatcher(cfg, dir, f.detect, clk.Now)
	w.tick = 5 * time.Millisecond
	return w
}

// start runs w until the test ends or the returned stop is called, whichever is first. It fails the test
// if Run returns an error or does not return within 5 s of cancel.
func start(t *testing.T, w *Watcher) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Run: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Errorf("Run did not return after its context was cancelled")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// waitFor polls cond until it holds. It fails the test with what if that takes more than 5 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// quiet gives the watcher about ten ticks to act before a test asserts that nothing happened, or that a
// result was not applied yet.
func quiet() {
	time.Sleep(50 * time.Millisecond)
}

// saveKinds writes the kinds.json a previous run would have left in dir.
func saveKinds(t *testing.T, dir string, kinds map[string]string) {
	t.Helper()
	if err := config.SaveState(dir, config.State{Kinds: kinds}); err != nil {
		t.Fatal(err)
	}
}

// savedKind returns the kind kinds.json holds for service, or "" when there is none.
func savedKind(t *testing.T, dir, service string) string {
	t.Helper()
	st, err := config.LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	return st.Kinds[service]
}

// Case 1: a known https service that answers one tcp probe stays https. A second consecutive tcp probe
// makes it tcp. The first probe is the check at host start; the second comes on Touch.
func TestWatcherNoDowngradeOnOneTCPProbe(t *testing.T) {
	defer failOnPanic(t)
	const target = "127.0.0.1:8443"
	dir := t.TempDir()
	saveKinds(t, dir, map[string]string{"web": "https"})
	cfg := &config.Config{Services: map[string]config.Service{"web": {Target: target}}}
	f := newFakeDetect(map[string][]answer{target: {tcpAnswer, tcpAnswer}})
	w := newTestWatcher(cfg, dir, f, newFakeClock())
	start(t, w)

	waitFor(t, "the probe at host start", func() bool { return f.count(target) == 1 })
	quiet()
	if k := w.Kind("web"); k != protocol.KindHTTPS {
		t.Fatalf("after one tcp probe: kind %d, want %d (https): one probe must not downgrade", k, protocol.KindHTTPS)
	}

	w.Touch("web")
	waitFor(t, "the second probe on Touch", func() bool { return f.count(target) == 2 })
	quiet()
	if k := w.Kind("web"); k != protocol.KindTCP {
		t.Fatalf("after two consecutive tcp probes: kind %d, want %d (tcp)", k, protocol.KindTCP)
	}
	if n := f.count(target); n != 2 {
		t.Fatalf("probes = %d, want 2: a tcp kind is settled and must not be probed again", n)
	}
}

// Case 2: an explicit kind is never probed, whatever the kind. Its own kind is what Kind reports, even
// after Touch and after the clock has run for minutes. An unlabeled service is probed, so the test shows
// the watcher is running.
func TestWatcherExplicitKindsNeverProbed(t *testing.T) {
	defer failOnPanic(t)
	const unlabeled = "127.0.0.1:9000"
	explicit := map[string]config.Service{
		"db":   {Target: "127.0.0.1:5432", Kind: "tcp"},
		"dns":  {Target: "127.0.0.1:53", Kind: "udp"},
		"site": {Target: "127.0.0.1:8443", Kind: "https"},
		"api":  {Target: "127.0.0.1:8080", Kind: "http"},
		"auto": {Target: unlabeled},
	}
	cfg := &config.Config{Services: explicit}
	dir := t.TempDir()
	clk := newFakeClock()
	f := newFakeDetect(map[string][]answer{})
	w := newTestWatcher(cfg, dir, f, clk)
	start(t, w)

	waitFor(t, "the probe of the unlabeled service", func() bool { return f.count(unlabeled) == 1 })
	for name := range explicit {
		w.Touch(name)
	}
	for range 10 {
		clk.Advance(time.Minute)
		quiet()
	}

	for _, name := range []string{"db", "dns", "site", "api"} {
		if n := f.count(explicit[name].Target); n != 0 {
			t.Errorf("service %s with an explicit kind was probed %d times, want 0", name, n)
		}
	}
	want := map[string]protocol.Kind{
		"db":   protocol.KindTCP,
		"dns":  protocol.KindUDP,
		"site": protocol.KindHTTPS,
		"api":  protocol.KindHTTP,
	}
	for name, k := range want {
		if got := w.Kind(name); got != k {
			t.Errorf("Kind(%s) = %d, want %d (its explicit kind)", name, got, k)
		}
	}
}

// Case 3: an unknown service is probed at host start, again on Touch, and then once a minute by the fake
// clock. Its timer stops once it is classified.
func TestWatcherRechecksUnknownOnTouchAndEveryMinute(t *testing.T) {
	defer failOnPanic(t)
	const target = "127.0.0.1:9100"
	cfg := &config.Config{Services: map[string]config.Service{"svc": {Target: target}}}
	f := newFakeDetect(map[string][]answer{target: {inconclusive, inconclusive, httpAnswer}})
	clk := newFakeClock()
	w := newTestWatcher(cfg, t.TempDir(), f, clk)
	start(t, w)

	waitFor(t, "the probe at host start", func() bool { return f.count(target) == 1 })
	w.Touch("svc") // at the same fake time as the first probe, so the minute is counted from here
	waitFor(t, "the re-check on Touch", func() bool { return f.count(target) == 2 })
	quiet()

	clk.Advance(59 * time.Second)
	quiet()
	if n := f.count(target); n != 2 {
		t.Fatalf("probes 59 s after the last check = %d, want 2: no re-check before a minute", n)
	}

	clk.Advance(time.Second)
	waitFor(t, "the re-check a minute after the last check", func() bool { return f.count(target) == 3 })
	quiet()
	if k := w.Kind("svc"); k != protocol.KindHTTP {
		t.Fatalf("Kind(svc) = %d, want %d (http) after the third probe", k, protocol.KindHTTP)
	}

	clk.Advance(10 * time.Minute)
	quiet()
	w.Touch("svc")
	quiet()
	if n := f.count(target); n != 3 {
		t.Fatalf("probes after classification = %d, want 3: the timer and Touch must stop once classified", n)
	}
}

// Case 4: an inconclusive probe keeps the previous kind. A known https service stays https, and a service
// with no kind stays unknown.
func TestWatcherInconclusiveKeepsPreviousKind(t *testing.T) {
	defer failOnPanic(t)
	const web, fresh = "127.0.0.1:8443", "127.0.0.1:8080"
	dir := t.TempDir()
	saveKinds(t, dir, map[string]string{"web": "https"})
	cfg := &config.Config{Services: map[string]config.Service{
		"web": {Target: web},
		"new": {Target: fresh},
	}}
	f := newFakeDetect(map[string][]answer{web: {inconclusive}, fresh: {inconclusive}})
	w := newTestWatcher(cfg, dir, f, newFakeClock())
	start(t, w)

	waitFor(t, "the probes at host start", func() bool { return f.count(web) == 1 && f.count(fresh) == 1 })
	quiet()
	if k := w.Kind("web"); k != protocol.KindHTTPS {
		t.Fatalf("Kind(web) = %d after an inconclusive probe, want %d (https) kept", k, protocol.KindHTTPS)
	}
	if k := w.Kind("new"); k != protocol.KindUnknown {
		t.Fatalf("Kind(new) = %d after an inconclusive probe, want %d (unknown) kept", k, protocol.KindUnknown)
	}

	w.Touch("web")
	w.Touch("new")
	waitFor(t, "the re-checks on Touch", func() bool { return f.count(web) == 2 && f.count(fresh) == 2 })
	quiet()
	if k := w.Kind("web"); k != protocol.KindHTTPS {
		t.Fatalf("Kind(web) = %d after a second inconclusive probe, want %d (https) kept", k, protocol.KindHTTPS)
	}
	if k := w.Kind("new"); k != protocol.KindUnknown {
		t.Fatalf("Kind(new) = %d after a second inconclusive probe, want %d (unknown) kept", k, protocol.KindUnknown)
	}
}

// Case 5: a detected kind survives a restart. The first run saves https to kinds.json. A second watcher on
// the same directory starts from https, and its inconclusive probe does not lose it.
func TestWatcherDetectedKindSurvivesRestart(t *testing.T) {
	defer failOnPanic(t)
	const target = "127.0.0.1:8443"
	dir := t.TempDir()
	cfg := &config.Config{Services: map[string]config.Service{"web": {Target: target}}}

	f1 := newFakeDetect(map[string][]answer{target: {httpsAnswer}})
	w1 := newTestWatcher(cfg, dir, f1, newFakeClock())
	stop1 := start(t, w1)
	waitFor(t, "https to be detected", func() bool { return w1.Kind("web") == protocol.KindHTTPS })
	waitFor(t, "https to be saved in kinds.json", func() bool { return savedKind(t, dir, "web") == "https" })
	stop1()

	f2 := newFakeDetect(map[string][]answer{})
	w2 := newTestWatcher(cfg, dir, f2, newFakeClock())
	start(t, w2)
	waitFor(t, "the probe at host start", func() bool { return f2.count(target) == 1 })
	quiet()
	if k := w2.Kind("web"); k != protocol.KindHTTPS {
		t.Fatalf("after restart: Kind(web) = %d, want %d (https) from kinds.json", k, protocol.KindHTTPS)
	}
}

// An inconclusive probe between two tcp results breaks the streak, because two consecutive tcp results
// means no other result comes between them. The service stays https until two tcp results come back to back.
func TestWatcherInconclusiveBreaksTCPStreak(t *testing.T) {
	defer failOnPanic(t)
	const target = "127.0.0.1:8443"
	dir := t.TempDir()
	saveKinds(t, dir, map[string]string{"web": "https"})
	cfg := &config.Config{Services: map[string]config.Service{"web": {Target: target}}}
	f := newFakeDetect(map[string][]answer{target: {tcpAnswer, inconclusive, tcpAnswer, tcpAnswer}})
	w := newTestWatcher(cfg, dir, f, newFakeClock())
	start(t, w)

	// Probes: the check at host start, then three Touches.
	waitFor(t, "probe 1 (tcp)", func() bool { return f.count(target) == 1 })
	quiet()
	if k := w.Kind("web"); k != protocol.KindHTTPS {
		t.Fatalf("after probe 1 (tcp): kind %d, want %d (https)", k, protocol.KindHTTPS)
	}

	w.Touch("web")
	waitFor(t, "probe 2 (inconclusive)", func() bool { return f.count(target) == 2 })
	quiet()
	if k := w.Kind("web"); k != protocol.KindHTTPS {
		t.Fatalf("after probe 2 (inconclusive): kind %d, want %d (https)", k, protocol.KindHTTPS)
	}

	w.Touch("web")
	waitFor(t, "probe 3 (tcp)", func() bool { return f.count(target) == 3 })
	quiet()
	if k := w.Kind("web"); k != protocol.KindHTTPS {
		t.Fatalf("after probe 3 (tcp, one after an inconclusive): kind %d, want %d (https)", k, protocol.KindHTTPS)
	}

	w.Touch("web")
	waitFor(t, "probe 4 (tcp)", func() bool { return f.count(target) == 4 })
	quiet()
	if k := w.Kind("web"); k != protocol.KindTCP {
		t.Fatalf("after probe 4 (second consecutive tcp): kind %d, want %d (tcp)", k, protocol.KindTCP)
	}
}
