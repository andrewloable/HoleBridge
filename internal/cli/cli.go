// Package cli is the holebridge command line: global options, the command table, exit codes and
// error printing. The commands themselves are added by later tasks.
package cli

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/andrewloable/HoleBridge/internal/config"
	"github.com/andrewloable/HoleBridge/internal/errs"
	"github.com/andrewloable/HoleBridge/internal/log"
	"github.com/andrewloable/HoleBridge/internal/version"
)

// errorsURL is the errors page each error's More line links to (docs/cli.md, exit codes).
const errorsURL = "https://holebridge.app/errors"

// helpText is printed by --help. It lists the commands of docs/cli.md#commands.
const helpText = `Usage: holebridge [global options] <command> [arguments]

Commands:
  holebridge share <target> [--name <n>] [--kind <k>]
  holebridge host
  holebridge service add <name> <target> [--kind https|http|tcp|udp] [--idle <duration>]
  holebridge service rm <name>
  holebridge service ls
  holebridge key [--rotate | --set <key>]
  holebridge app-key [--new | --rotate | --set <hex>]
  holebridge relay [--new-key | --bootstrap <host:port,...>]
  holebridge relay check <relay-key-file> [--bootstrap <host:port,...>]
  holebridge status

Global options, before or after the command:
  --config <dir>       config directory (HOLEBRIDGE_CONFIG, else the platform default)
  --log-level <level>  error, warn, info or debug
  --version            print the version and exit
  --help               print this help and exit
`

// Env is the process environment a command line runs in. main passes the real streams, os.Getenv
// and time.Now; tests pass buffers and fakes.
type Env struct {
	Stdout, Stderr io.Writer
	Getenv         func(string) string
	Now            func() time.Time
	// LogLevel is the level of the log a command writes to Stderr. Run sets it from --log-level. Its zero value is
	// slog.LevelInfo, so an Env that sets nothing logs at info.
	LogLevel slog.Level
}

// Command runs one subcommand. args are the words after the command name, with the global options
// removed. configDir is the config directory from --config, HOLEBRIDGE_CONFIG or the platform
// default; it is "" when none can be found, and a command that needs one returns the error from
// config.Dir. A returned *errs.Error is printed by Run with errs.Format and exits 1, or 2 when its
// code is HB-USAGE.
type Command func(args []string, env Env, configDir string) error

// commands maps a command's words ("share", "service add") to its function. The tasks that add
// commands register them here.
var commands = map[string]Command{}

// globals holds the global options found on the command line.
type globals struct {
	configDir     string
	logLevel      slog.Level // info when no --log-level is given
	version, help bool
}

// Run runs the command line args (os.Args[1:]) and returns the exit code: 0 success, 1 failure,
// 2 bad usage. Errors go to env.Stderr.
func Run(args []string, env Env) int {
	g, words, err := parseGlobals(args)
	if err != nil {
		return fail(env, err)
	}
	switch {
	case g.help:
		fmt.Fprint(env.Stdout, helpText)
		return 0
	case g.version:
		fmt.Fprintf(env.Stdout, "holebridge %s\n", version.Version)
		return 0
	case len(words) == 0:
		return fail(env, errs.E("HB-USAGE", "no command given, see holebridge --help", nil))
	}

	c, cargs, ok := lookup(words)
	if !ok {
		return fail(env, usageNamed("unknown command", words[0]))
	}
	// config.Dir returns "" with its error when no directory can be found. Run does not stop here,
	// so that a command which needs no directory still runs; see Command.
	configDir, _ := config.Dir(g.configDir, env.Getenv)
	// env is a copy, so setting its level here does not change the caller's Env.
	env.LogLevel = g.logLevel
	if err := c(cargs, env, configDir); err != nil {
		return fail(env, err)
	}
	return 0
}

// parseGlobals takes the global options out of args, before or after the command, and returns them
// with the rest of the words. Other options pass through, so a command keeps its own flags. The
// log level is checked and stored in the globals; Run passes it on in Env.LogLevel.
func parseGlobals(args []string) (globals, []string, error) {
	var g globals
	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--version":
			g.version = true
			continue
		case "--help":
			g.help = true
			continue
		}
		name, value, hasValue := strings.Cut(args[i], "=")
		if name != "--config" && name != "--log-level" {
			rest = append(rest, args[i])
			continue
		}
		if !hasValue {
			if i+1 == len(args) {
				return g, nil, errs.E("HB-USAGE", name+" needs a value, see holebridge --help", nil)
			}
			i++
			value = args[i]
		}
		if name == "--config" {
			// An empty value must not fall back to the default directory: a script that passes an
			// unset variable would otherwise change the default host.json.
			if value == "" {
				return g, nil, errs.E("HB-USAGE", "--config needs a directory, see holebridge --help", nil)
			}
			g.configDir = value
		} else {
			lvl, err := log.ParseLevel(value)
			if err != nil {
				return g, nil, errs.E("HB-USAGE", "--log-level must be error, warn, info or debug", nil)
			}
			g.logLevel = lvl
		}
	}
	return g, rest, nil
}

// lookup finds the command that words start with, preferring a two-word name such as "service add".
// It returns the command and the words after its name.
func lookup(words []string) (Command, []string, bool) {
	if len(words) >= 2 {
		if c, ok := commands[words[0]+" "+words[1]]; ok {
			return c, words[2:], true
		}
	}
	c, ok := commands[words[0]]
	return c, words[1:], ok
}

// usageNamed returns an HB-USAGE error that says what was wrong and names the word only when it is
// shaped like a command or option name (see nameOf). Usage errors never print a value: a key or an
// application key typed in the wrong place must not reach stderr.
func usageNamed(what, word string) error {
	if n := nameOf(word); n != "" {
		what += " " + n
	}
	return errs.E("HB-USAGE", what+", see holebridge --help", nil)
}

// nameOf returns s when it is shaped like a command or option name: at most two leading dashes,
// then lowercase letters and dashes, at most 32 characters in all. Otherwise it returns "". A key
// typed in capitals or with digits, and an application key (64 characters), fail the test. It is a
// filter, not a proof: a key typed in lowercase letters only, with a leading dash, would pass.
func nameOf(s string) string {
	if len(s) > 32 {
		return ""
	}
	body := strings.TrimLeft(s, "-")
	if len(s)-len(body) > 2 || body == "" || body[0] < 'a' || body[0] > 'z' {
		return ""
	}
	for i := 0; i < len(body); i++ {
		if c := body[i]; !('a' <= c && c <= 'z' || c == '-') {
			return ""
		}
	}
	return s
}

// fail prints err to env.Stderr and returns its exit code. A *errs.Error prints as errs.Format
// lays it out; any other error prints as one plain line. HB-USAGE exits 2, everything else 1.
func fail(env Env, err error) int {
	var e *errs.Error
	if !errors.As(err, &e) {
		fmt.Fprintf(env.Stderr, "error: %v\n", err)
		return 1
	}
	fmt.Fprintln(env.Stderr, errs.Format(e, errorsURL))
	if e.Code == "HB-USAGE" {
		return 2
	}
	return 1
}
