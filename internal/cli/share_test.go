package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrewloable/HoleBridge/internal/errs"
)

// The share banner starts with the Sharing line, then the key block, the LAN and Internet lines and, since a share
// has no relay, "Relay: none set". The share runs with LAN on and keeps nothing in the config directory: the runner
// gets no directory.
func TestShareBannerHasTheHostLinesAndNoRelay(t *testing.T) {
	interruptedHost(t)
	dir := shortDir(t)
	_, appKey, _ := vectors(t)
	writeAppKey(t, dir, appKey)

	fake := &fakeHostRunner{lan: "192.0.2.10"}
	stdout, stderr, err := callCommand(shareWith(fake), dir, "8080")
	if err != nil {
		t.Fatalf("%v (stderr %q)", err, stderr)
	}
	if !strings.HasPrefix(stdout, `Sharing 127.0.0.1:8080 as "8080" (temporary key)`+"\n") {
		t.Errorf("stdout does not start with the Sharing line: %q", stdout)
	}
	if keyLinkIn(stdout) == "" {
		t.Error("the share banner has no key link")
	}
	if qrBlockLines(stdout) == 0 {
		t.Error("the share banner has no QR block")
	}
	for _, want := range []string{"LAN: listening on 192.0.2.10", "Relay: none set"} {
		if !lineHas(stdout, want) {
			t.Errorf("stdout has no line %q: %q", want, stdout)
		}
	}
	reqs := fake.requests()
	if len(reqs) != 1 {
		t.Fatalf("the runner was started %d times, want 1", len(reqs))
	}
	if reqs[0].Dir != "" || !reqs[0].LAN {
		t.Errorf("the request has Dir %q and LAN %t, want no directory and LAN on", reqs[0].Dir, reqs[0].LAN)
	}
}

// A share with no --name or --kind names its service after the port and leaves the kind to detection.
func TestShareDefaultsNameToPortAndLeavesKindUnset(t *testing.T) {
	interruptedHost(t)
	dir := shortDir(t)
	_, appKey, _ := vectors(t)
	writeAppKey(t, dir, appKey)

	fake := &fakeHostRunner{}
	if _, stderr, err := callCommand(shareWith(fake), dir, "203.0.113.5:445"); err != nil {
		t.Fatalf("%v (stderr %q)", err, stderr)
	}
	reqs := fake.requests()
	if len(reqs) != 1 || reqs[0].Config == nil {
		t.Fatalf("the runner was started %d times, want 1 with a config", len(reqs))
	}
	svc, ok := reqs[0].Config.Services["445"]
	if !ok || svc.Target != "203.0.113.5:445" || svc.Kind != "" {
		t.Errorf("the shared service is %+v, want 445 at 203.0.113.5:445 with no kind", reqs[0].Config.Services)
	}
}

// Arguments that share cannot use are usage errors, and the runner is not started.
func TestShareRefusesBadArgumentsBeforeStarting(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no target", nil},
		{"two targets", []string{"8080", "9090"}},
		{"target is not a port", []string{"notaport"}},
		{"kind not in the list", []string{"--kind", "ftp", "8080"}},
		{"name not a service name", []string{"--name", "Bad_Name", "8080"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			interruptedHost(t)
			dir := shortDir(t)
			_, appKey, _ := vectors(t)
			writeAppKey(t, dir, appKey)

			fake := &fakeHostRunner{}
			_, _, err := callCommand(shareWith(fake), dir, c.args...)
			var e *errs.Error
			if !errors.As(err, &e) || e.Code != "HB-USAGE" {
				t.Errorf("error = %v, want HB-USAGE", err)
			}
			if n := len(fake.requests()); n != 0 {
				t.Errorf("the runner was started %d times, want 0", n)
			}
		})
	}
}

// share never creates host.lock, so a share and a host on one directory do not hold each other's lock.
func TestShareLeavesNoHostLock(t *testing.T) {
	interruptedHost(t)
	dir := shortDir(t)
	_, appKey, _ := vectors(t)
	writeAppKey(t, dir, appKey)

	if _, stderr, err := callCommand(shareWith(&fakeHostRunner{}), dir, "8080"); err != nil {
		t.Fatalf("%v (stderr %q)", err, stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "host.lock")); !os.IsNotExist(err) {
		t.Errorf("share left host.lock behind (stat error %v)", err)
	}
}

// Guard for the HB-LAN-PORT-IN-USE fix text (HoleBridge-trk.16): the fix must name share, which keeps no host.json
// and so cannot be fixed by editing one. A real host may hold the default ports, so the test reads the catalog
// instead of binding them.
func TestShareLANPortInUseNamesShareInTheFix(t *testing.T) {
	fix := errs.Catalog["HB-LAN-PORT-IN-USE"].Fix
	for _, word := range []string{"lan.port", "share"} {
		if !strings.Contains(fix, word) {
			t.Errorf("HB-LAN-PORT-IN-USE fix %q does not contain %q", fix, word)
		}
	}
}
