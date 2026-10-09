package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/andrewloable/HoleBridge/internal/config"
	"github.com/andrewloable/HoleBridge/internal/errs"
)

// The service commands register themselves here, so cli.go does not change when they are built.
func init() {
	commands["service add"] = serviceAdd
	commands["service rm"] = serviceRm
	commands["service ls"] = serviceLs
}

// serviceKinds are the values --kind accepts.
var serviceKinds = []string{"https", "http", "tcp", "udp"}

// serviceAdd is holebridge service add <name> <target> [--kind https|http|tcp|udp] [--idle <duration>].
func serviceAdd(args []string, env Env, configDir string) error {
	words, opts, err := splitFlags(args, "kind", "idle")
	if err != nil {
		return err
	}
	if len(words) != 2 {
		return usage("service add wants a name and a target")
	}
	name := words[0]
	if !config.ValidServiceName(name) {
		return usage("a service name is 1 to 32 characters: a-z, 0-9 and dash, starting with a letter or digit")
	}
	host, port, err := config.ParseTarget(words[1])
	if err != nil {
		return usage("a service target is a port or host:port, such as 8080 or 203.0.113.20:445")
	}
	kind, hasKind := opts["kind"]
	if hasKind && !slices.Contains(serviceKinds, kind) {
		return usage("--kind must be https, http, tcp or udp")
	}
	var idle config.Duration
	if v, ok := opts["idle"]; ok {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return usage("--idle must be a positive duration such as 8h")
		}
		idle = config.Duration(d)
	}

	dir, err := requireDir(configDir, env)
	if err != nil {
		return err
	}
	c, err := loadHost(dir, true)
	if err != nil {
		return err
	}
	if _, ok := c.Services[name]; ok {
		return fmt.Errorf("service %s already exists: run service rm %s first, or pick another name", name, name)
	}
	c.Services[name] = config.Service{
		Target: net.JoinHostPort(host, strconv.Itoa(port)),
		Kind:   kind,
		Idle:   idle,
	}
	if err := config.Save(dir, c); err != nil {
		return err
	}
	noteRestart(dir, env)
	return nil
}

// serviceRm is holebridge service rm <name>.
func serviceRm(args []string, env Env, configDir string) error {
	words, _, err := splitFlags(args)
	if err != nil {
		return err
	}
	if len(words) != 1 {
		return usage("service rm wants a name")
	}
	name := words[0]
	dir, err := requireDir(configDir, env)
	if err != nil {
		return err
	}
	c, err := loadHost(dir, false)
	if err != nil {
		return err
	}
	if _, ok := c.Services[name]; !ok {
		// The name is printed only when it is name-shaped (see nameOf); otherwise the error says the service is
		// unknown and leaves the word out, so a key typed in this slot never reaches stderr.
		return errs.E("HB-UNKNOWN-SERVICE", nameOf(name), nil)
	}
	// The state is read before host.json changes, so a bad state file fails the command early.
	st, err := config.LoadState(dir)
	if err != nil {
		return err
	}
	delete(c.Services, name)
	if err := config.Save(dir, c); err != nil {
		return err
	}
	// A service added later under the same name must not inherit this one's detected kind.
	if _, ok := st.Kinds[name]; ok {
		delete(st.Kinds, name)
		if err := config.SaveState(dir, st); err != nil {
			return err
		}
	}
	noteRestart(dir, env)
	return nil
}

// serviceLs is holebridge service ls: one line per service, name, kind (or detecting) and target.
// The kind is the one in host.json, else the one detected into the state file, else detecting.
func serviceLs(args []string, env Env, configDir string) error {
	words, _, err := splitFlags(args)
	if err != nil {
		return err
	}
	if len(words) != 0 {
		return usage("service ls takes no arguments")
	}
	dir, err := requireDir(configDir, env)
	if err != nil {
		return err
	}
	c, err := loadHost(dir, false)
	if err != nil {
		return err
	}
	st, err := config.LoadState(dir)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	for _, name := range slices.Sorted(maps.Keys(c.Services)) {
		s := c.Services[name]
		kind := s.Kind
		if kind == "" {
			kind = st.Kinds[name]
		}
		if kind == "" {
			kind = "detecting"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", name, kind, s.Target)
	}
	return w.Flush()
}

// usage returns an HB-USAGE error, which exits 2.
func usage(detail string) error {
	return errs.E("HB-USAGE", detail, nil)
}

// splitFlags separates the words of args from the options named in valued, which take a value.
// An option may come before or after the words, as --kind udp or --kind=udp; a repeated option
// keeps its last value. Any other option is a usage error.
func splitFlags(args []string, valued ...string) ([]string, map[string]string, error) {
	var words []string
	opts := map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			words = append(words, a)
			continue
		}
		name, value, hasValue := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		if !slices.Contains(valued, name) {
			// Only the option name is printed, never its value: name is the part before "=".
			opt, _, _ := strings.Cut(a, "=")
			return nil, nil, usageNamed("unknown option", opt)
		}
		if !hasValue {
			if i+1 == len(args) {
				return nil, nil, usage(a + " needs a value, see holebridge --help")
			}
			i++
			value = args[i]
		}
		opts[name] = value
	}
	return words, opts, nil
}

// requireDir returns configDir, or the error config.Dir gives when no config directory can be found.
func requireDir(configDir string, env Env) (string, error) {
	if configDir == "" {
		return config.Dir("", env.Getenv)
	}
	return configDir, nil
}

// loadHost returns the config in dir. A missing host.json is an empty config when create is false.
// When create is true, the file is first written as {} so that config.Load fills in the defaults;
// the caller saves it straight after, so no empty placeholder is left behind.
func loadHost(dir string, create bool) (*config.Config, error) {
	c, err := config.Load(dir)
	if !errors.Is(err, fs.ErrNotExist) {
		return c, err
	}
	if !create {
		return &config.Config{Services: map[string]config.Service{}}, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "host.json"), []byte("{}\n"), 0o600); err != nil {
		return nil, err
	}
	return config.Load(dir)
}

// noteRestart prints the restart note on stdout when a running host has read host.json. The host
// reads host.json only at start; M3 replaces the note with a reload (docs/cli.md, hosting).
func noteRestart(dir string, env Env) {
	if hostRunning(dir) {
		fmt.Fprintln(env.Stdout, "restart holebridge host to apply")
	}
}

// hostRunning reports whether host.lock in dir names a live process. The host writes its process
// ID in decimal and a newline. A missing or unreadable lock means no host is running.
func hostRunning(dir string) bool {
	b, err := os.ReadFile(filepath.Join(dir, "host.lock"))
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return false
	}
	return processAlive(pid)
}

// processAlive reports whether a process with this ID exists. On POSIX, signal 0 checks without
// signalling, and EPERM means the process exists but belongs to another user. Windows has no
// signal 0, and FindProcess fails there when the process does not exist.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
