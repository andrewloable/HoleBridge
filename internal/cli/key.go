package cli

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/andrewloable/HoleBridge/internal/config"
	"github.com/andrewloable/HoleBridge/internal/errs"
	"github.com/andrewloable/HoleBridge/internal/keys"
	"github.com/andrewloable/HoleBridge/internal/links"
	"github.com/andrewloable/HoleBridge/internal/qr"
)

// The key and app-key commands register themselves here, as the service commands do.
func init() {
	commands["key"] = keyCmd
	commands["app-key"] = appKeyCmd
}

// keyCmd is holebridge key [--rotate | --set <key>]. With no option it prints the host key, its QR
// code and its link, and creates the host key and app.key on first use.
func keyCmd(args []string, env Env, configDir string) error {
	rotate, rest := takeFlag(args, "--rotate")
	words, opts, err := splitFlags(rest, "set")
	if err != nil {
		return err
	}
	if len(words) != 0 {
		return usage("key takes no arguments")
	}
	value, hasSet := opts["set"]
	if rotate && hasSet {
		return usage("key --rotate and key --set cannot be used together")
	}
	dir, err := requireDir(configDir, env)
	if err != nil {
		return err
	}
	switch {
	case rotate:
		return rotateKey(dir, env)
	case hasSet:
		return setHostKey(dir, value, env)
	}
	return showKey(dir, env)
}

// showKey prints the host key, its QR code and the key link. The link carries the application key,
// so app.key is created here when it is absent.
func showKey(dir string, env Env) error {
	appKey, err := loadOrCreateAppKey(dir)
	if err != nil {
		return err
	}
	c, err := loadHost(dir, true)
	if err != nil {
		return err
	}
	if c.Key == "" {
		c.Key = keys.Generate()
		if err := config.Save(dir, c); err != nil {
			return err
		}
	}
	link := links.KeyLink(links.DefaultBase, c.Key, appKey)
	code, err := qr.Encode(link, qr.M)
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "Key: %s\n", keys.Format(c.Key))
	fmt.Fprint(env.Stdout, code.Terminal(2))
	fmt.Fprintln(env.Stdout, "Scan with your phone, or open:")
	fmt.Fprintln(env.Stdout, link)
	return nil
}

// rotateKey replaces the host key and prints no key; holebridge key shows the new one. On a running
// host the old key stays active until the host restarts.
func rotateKey(dir string, env Env) error {
	c, err := loadHost(dir, true)
	if err != nil {
		return err
	}
	c.Key = keys.Generate()
	if err := config.Save(dir, c); err != nil {
		return err
	}
	if hostRunning(dir) {
		fmt.Fprintln(env.Stdout, "the old key stays active until holebridge host restarts; run holebridge key to see the new one")
	}
	return nil
}

// setHostKey stores the key as typed, in its canonical form. A bad key is refused by keys.Normalize,
// whose error never repeats the input, and host.json is left alone.
func setHostKey(dir, value string, env Env) error {
	k, err := keys.Normalize(value)
	if err != nil {
		return err
	}
	c, err := loadHost(dir, true)
	if err != nil {
		return err
	}
	c.Key = k
	if err := config.Save(dir, c); err != nil {
		return err
	}
	noteRestart(dir, env)
	return nil
}

// appKeyCmd is holebridge app-key [--new | --rotate | --set <hex>]. With no option it prints the
// application key, creating app.key when it is absent, and warns on stderr that it is a secret.
func appKeyCmd(args []string, env Env, configDir string) error {
	isNew, rest := takeFlag(args, "--new")
	rotate, rest := takeFlag(rest, "--rotate")
	words, opts, err := splitFlags(rest, "set")
	if err != nil {
		return err
	}
	if len(words) != 0 {
		return usage("app-key takes no arguments")
	}
	value, hasSet := opts["set"]
	if (isNew && rotate) || (isNew && hasSet) || (rotate && hasSet) {
		return usage("app-key takes one of --new, --rotate and --set")
	}
	dir, err := requireDir(configDir, env)
	if err != nil {
		return err
	}
	switch {
	case isNew:
		return newAppKey(dir)
	case rotate:
		return rotateAppKey(dir, env)
	case hasSet:
		return setAppKey(dir, value, env)
	}
	return showAppKey(dir, env)
}

// showAppKey prints the application key on stdout and the secret warning on stderr, so that a
// redirected stdout holds only the key.
func showAppKey(dir string, env Env) error {
	k, err := loadOrCreateAppKey(dir)
	if err != nil {
		return err
	}
	fmt.Fprintln(env.Stdout, keys.FormatAppKey(k))
	fmt.Fprintln(env.Stderr, "warning: the application key is a secret for this deployment. Do not share or publish it; if it leaks, run holebridge app-key --rotate.")
	return nil
}

// newAppKey creates app.key and refuses, leaving the file alone, when one exists. The error names
// app.key and never shows its content.
func newAppKey(dir string) error {
	if _, err := config.CreateAppKey(dir); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return errors.New("app.key already exists: use app-key --rotate to replace it, or app-key --set <hex> to set it")
		}
		return err
	}
	return nil
}

// rotateAppKey replaces app.key and prints no key; app-key shows the new one.
func rotateAppKey(dir string, env Env) error {
	if err := config.SaveAppKey(dir, keys.NewAppKey()); err != nil {
		return err
	}
	fmt.Fprintln(env.Stdout, "every app must be re-provisioned or rebuilt: the old application key no longer works. Run holebridge app-key to see the new one.")
	noteRestart(dir, env)
	return nil
}

// setAppKey stores a 64-digit lowercase hex key. Capital letters are refused, as LoadAppKey refuses
// them, so the value must round-trip exactly.
func setAppKey(dir, value string, env Env) error {
	k, err := keys.ParseAppKey(value)
	if err != nil || keys.FormatAppKey(k) != value {
		return errs.E("HB-APPKEY-INVALID", "want 64 lowercase hex digits", nil)
	}
	if err := config.SaveAppKey(dir, k); err != nil {
		return err
	}
	noteRestart(dir, env)
	return nil
}

// loadOrCreateAppKey returns the application key in dir, first creating app.key when there is none.
func loadOrCreateAppKey(dir string) ([32]byte, error) {
	k, err := config.LoadAppKey(dir)
	var e *errs.Error
	if errors.As(err, &e) && e.Code == "HB-APPKEY-MISSING" {
		return config.CreateAppKey(dir)
	}
	return k, err
}

// takeFlag removes every occurrence of the boolean option flag from args and reports whether there
// was one. The other words and options are returned in order.
func takeFlag(args []string, flag string) (bool, []string) {
	var rest []string
	found := false
	for _, a := range args {
		if a == flag {
			found = true
			continue
		}
		rest = append(rest, a)
	}
	return found, rest
}
