package cli

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/internal/errs"
	"github.com/andrewloable/HoleBridge/internal/version"
)

// docsURL is the errors page the More line links to (docs/cli.md, exit codes).
const docsURL = "https://holebridge.app/errors"

// helpCommands are the commands of docs/cli.md#commands, as --help must list them.
var helpCommands = []string{
	"share", "host", "service add", "service rm", "service ls",
	"key", "app-key", "relay", "relay check", "status",
}

// run calls Run with buffers for the two streams and returns the exit code and what was printed.
func run(args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	env := Env{
		Stdout: &out,
		Stderr: &errOut,
		Getenv: func(string) string { return "" },
		Now:    time.Now,
	}
	code = Run(args, env)
	return code, out.String(), errOut.String()
}

// probeCall is one call of a registered test command.
type probeCall struct {
	args      []string
	configDir string
}

// registerProbe registers a command called name that records each call and returns result. The
// command is removed when the test ends.
func registerProbe(t *testing.T, name string, result error) *[]probeCall {
	t.Helper()
	var calls []probeCall
	commands[name] = func(args []string, env Env, configDir string) error {
		calls = append(calls, probeCall{args: args, configDir: configDir})
		return result
	}
	t.Cleanup(func() { delete(commands, name) })
	return &calls
}

// Case 1: --version prints "holebridge <version>" and exits 0.
func TestVersionPrintsVersionAndExitsZero(t *testing.T) {
	code, stdout, stderr := run("--version")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	want := "holebridge " + version.Version + "\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

// Case 2: --help lists every command from docs/cli.md#commands and exits 0. Each command is
// checked as "holebridge <words>", the form the docs use.
func TestHelpListsEveryCommandAndExitsZero(t *testing.T) {
	code, stdout, stderr := run("--help")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	for _, c := range helpCommands {
		if want := "holebridge " + c; !strings.Contains(stdout, want) {
			t.Errorf("help does not list %q; stdout:\n%s", want, stdout)
		}
	}
}

// Case 3: no command, or an unknown command, exits 2 and prints HB-USAGE.
func TestNoCommandOrUnknownCommandExitsTwoWithUsage(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no command", nil},
		{"unknown command", []string{"nope"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := run(tc.args...)
			if code != 2 {
				t.Errorf("exit code = %d, want 2 (stderr %q)", code, stderr)
			}
			if !strings.Contains(stderr, "HB-USAGE") {
				t.Errorf("stderr does not contain HB-USAGE: %q", stderr)
			}
		})
	}
}

// Case 4: --log-level verbose exits 2, before or after the command. The command must not run.
// The valid levels are checked too, so that the rejection is about the value and not the position.
func TestLogLevelVerboseExitsTwo(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"before the command", []string{"--log-level", "verbose", "probe"}},
		{"after the command", []string{"probe", "--log-level", "verbose"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := registerProbe(t, "probe", nil)
			code, _, stderr := run(tc.args...)
			if code != 2 {
				t.Errorf("exit code = %d, want 2 (stderr %q)", code, stderr)
			}
			if !strings.Contains(stderr, "HB-USAGE") {
				t.Errorf("stderr does not contain HB-USAGE: %q", stderr)
			}
			if len(*calls) != 0 {
				t.Errorf("command ran %d times, want 0", len(*calls))
			}
		})
	}

	for _, level := range []string{"error", "warn", "info", "debug"} {
		t.Run("valid level "+level, func(t *testing.T) {
			calls := registerProbe(t, "probe", nil)
			code, _, stderr := run("--log-level", level, "probe")
			if code != 0 {
				t.Errorf("exit code = %d, want 0 (stderr %q)", code, stderr)
			}
			if len(*calls) != 1 {
				t.Errorf("command ran %d times, want 1", len(*calls))
			}
		})
	}
}

// Case 5: --config dir is passed to commands, before or after the command. The global option and
// its value are not left in the command's arguments.
func TestConfigDirIsPassedToCommands(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		args []string
	}{
		{"before the command", []string{"--config", dir, "probe", "a", "b"}},
		{"after the command", []string{"probe", "a", "b", "--config", dir}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := registerProbe(t, "probe", nil)
			code, _, stderr := run(tc.args...)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
			}
			if len(*calls) != 1 {
				t.Fatalf("command ran %d times, want 1", len(*calls))
			}
			got := (*calls)[0]
			if got.configDir != dir {
				t.Errorf("configDir = %q, want %q", got.configDir, dir)
			}
			if want := []string{"a", "b"}; !reflect.DeepEqual(got.args, want) {
				t.Errorf("args = %q, want %q", got.args, want)
			}
		})
	}
}

// Case 6: a command returning an *errs.Error prints the four-line format to stderr and exits 1.
// The expected text is errs.Format's own output, so this test does not decide how the detail is
// placed (that is errs's job, HoleBridge-hb5.12.6). The line checks use the catalog entry.
func TestCommandErrorPrintsFourLinesAndExitsOne(t *testing.T) {
	e := errs.E("HB-CONFIG-PERMS", "mode 0644", nil)
	registerProbe(t, "probe", e)

	code, stdout, stderr := run("probe")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (stderr %q)", code, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty: errors go to stderr", stdout)
	}
	if want := errs.Format(e, docsURL) + "\n"; stderr != want {
		t.Fatalf("stderr = %q, want %q", stderr, want)
	}

	lines := strings.Split(strings.TrimSuffix(stderr, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("stderr has %d lines, want 4: %q", len(lines), stderr)
	}
	c := errs.Catalog["HB-CONFIG-PERMS"]
	if wantPrefix := "error HB-CONFIG-PERMS: " + c.Problem; !strings.HasPrefix(lines[0], wantPrefix) {
		t.Errorf("error line = %q, want it to start with %q", lines[0], wantPrefix)
	}
	if want := "  " + c.Cause; lines[1] != want {
		t.Errorf("cause line = %q, want %q", lines[1], want)
	}
	if want := "  Fix: " + c.Fix; lines[2] != want {
		t.Errorf("fix line = %q, want %q", lines[2], want)
	}
	if want := "  More: " + docsURL + "#hb-config-perms"; lines[3] != want {
		t.Errorf("more line = %q, want %q", lines[3], want)
	}
}

// Usage errors never print an argument value. A key or an application key typed with a wrong
// option, pasted where the command goes, or given to a valued option with a bad value must not reach
// stdout or stderr. The non-usage rows (exit 1) show the same for the key and app-key commands.
func TestUsageErrorsNeverEchoASecret(t *testing.T) {
	const appKey = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	const hostKey = "7KQ-M4X-9TR"
	const hostKeyPlain = "7KQM4X9TR"
	cases := []struct {
		name    string
		args    []string // after --config <dir>, which every row passes
		code    int
		catalog string   // catalog code that must appear on stderr
		option  string   // text that must appear on stderr, the option name; "" for none
		secrets []string // values that must appear on neither stream
	}{
		{"app key with a mistyped option", []string{"app-key", "--sett=" + appKey}, 2, "HB-USAGE", "--sett", []string{appKey}},
		{"app key with a single dash", []string{"app-key", "-set=" + appKey}, 2, "HB-USAGE", "-set", []string{appKey}},
		{"host key with a mistyped option", []string{"key", "--sett=" + hostKey}, 2, "HB-USAGE", "--sett", []string{hostKey, hostKeyPlain}},
		{"host key in a made-up option", []string{"key", "--key=" + hostKey}, 2, "HB-USAGE", "--key", []string{hostKey, hostKeyPlain}},
		{"host key as a bare option", []string{"key", "--" + hostKeyPlain}, 2, "HB-USAGE", "", []string{hostKeyPlain}},
		{"app key as the command word", []string{appKey}, 2, "HB-USAGE", "", []string{appKey}},
		{"host key as the command word", []string{hostKey}, 2, "HB-USAGE", "", []string{hostKey, hostKeyPlain}},
		{"app key after a global option", []string{"--log-level", appKey, "key"}, 2, "HB-USAGE", "", []string{appKey}},
		{"app key as the service target", []string{"service", "add", "web", appKey}, 2, "HB-USAGE", "", []string{appKey}},
		{"app key as the --kind value", []string{"service", "add", "web", "8080", "--kind=" + appKey}, 2, "HB-USAGE", "", []string{appKey}},
		{"app key as an extra key argument", []string{"key", appKey}, 2, "HB-USAGE", "", []string{appKey}},
		{"bad app key for app-key --set", []string{"app-key", "--set", hostKey}, 1, "HB-APPKEY-INVALID", "", []string{hostKey, hostKeyPlain}},
		{"bad host key for key --set", []string{"key", "--set", appKey}, 1, "HB-KEY-INVALID", "", []string{appKey}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			args := append([]string{"--config", dir}, tc.args...)
			code, stdout, stderr := run(args...)
			if code != tc.code {
				t.Errorf("exit code = %d, want %d (stderr %q)", code, tc.code, stderr)
			}
			if !strings.Contains(stderr, tc.catalog) {
				t.Errorf("stderr does not contain %s: %q", tc.catalog, stderr)
			}
			if tc.option != "" && !strings.Contains(stderr, tc.option) {
				t.Errorf("stderr does not name the option %s: %q", tc.option, stderr)
			}
			assertNoSecret(t, tc.name, stdout+stderr, tc.secrets...)
		})
	}
}

// An empty --config value is a usage error, in both forms and wherever it appears. It must not
// fall back to the default config directory, where key --rotate would replace the host key.
func TestEmptyConfigValueIsUsageError(t *testing.T) {
	defaultDir := t.TempDir()
	writeHostJSON(t, defaultDir, keyHostJSON)
	before := readHostJSON(t, defaultDir)
	getenv := func(k string) string {
		if k == "HOLEBRIDGE_CONFIG" {
			return defaultDir
		}
		return ""
	}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"separate value before the command", []string{"--config", "", "key", "--rotate"}},
		{"separate value after the command", []string{"key", "--rotate", "--config", ""}},
		{"equals form before the command", []string{"--config=", "key", "--rotate"}},
		{"equals form after the command", []string{"key", "--rotate", "--config="}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			env := Env{Stdout: &out, Stderr: &errOut, Getenv: getenv, Now: time.Now}
			if code := Run(tc.args, env); code != 2 {
				t.Errorf("exit code = %d, want 2 (stderr %q)", code, errOut.String())
			}
			if !strings.Contains(errOut.String(), "HB-USAGE") {
				t.Errorf("stderr does not contain HB-USAGE: %q", errOut.String())
			}
			if after := readHostJSON(t, defaultDir); !bytes.Equal(after, before) {
				t.Error("host.json in the default directory changed")
			}
		})
	}
}
