// Ported from hyperdht 6.34.1 lib/constants.js (BOOTSTRAP_NODES), MIT License, Copyright (c) 2018-2019
// Mathias Buus, David Mark Clements & Contributors.

package cli

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/andrewloable/HoleBridge/internal/config"
	"github.com/andrewloable/HoleBridge/internal/errs"
	"github.com/andrewloable/HoleBridge/internal/keys"
	"github.com/andrewloable/HoleBridge/internal/log"
	"github.com/andrewloable/HoleBridge/internal/relay"
	"github.com/andrewloable/HoleBridge/pears/dhtrpc"
	"github.com/andrewloable/HoleBridge/pears/hyperdht"
	"github.com/andrewloable/HoleBridge/pears/noise"
)

// The relay commands register themselves here, as the key and service commands do.
func init() {
	commands["relay"] = relayCmd
	commands["relay check"] = relayCheckCmd
}

// dhtWait bounds the bootstrap of the DHT node that a relay or a relay check starts. connectWait bounds one
// connect of relay check.
const (
	dhtWait     = 30 * time.Second
	connectWait = 30 * time.Second
)

// bootstrap lists the bootstrap nodes of the DHT nodes that the relay and relay check commands start, as
// host:port. It is the public set that hyperdht 6.34.1 uses by default (BOOTSTRAP_NODES, which gives each
// node a suggested IP and then its host name). dhtrpc takes host:port and resolves names, so the host names
// are listed. --bootstrap replaces the list; tests replace it with the nodes of a testnet.
var bootstrap = []string{
	"node1.hyperdht.org:49737", // 88.99.3.86
	"node2.hyperdht.org:49737", // 142.93.90.113
	"node3.hyperdht.org:49737", // 138.68.147.8
}

// bootstrapKey is the context key of the bootstrap nodes that a relay command was given.
type bootstrapKey struct{}

// bootstrapOf returns the bootstrap nodes that ctx carries, or the default list when it carries none.
func bootstrapOf(ctx context.Context) []string {
	if nodes, ok := ctx.Value(bootstrapKey{}).([]string); ok {
		return nodes
	}
	return bootstrap
}

// relayStatus is what the relay command prints about a running relay. *relay.Relay has both methods.
type relayStatus interface {
	PublicKey() [32]byte
	NAT() dhtrpc.NATInfo
}

// startRelay starts the relay under relayKey and appKey and returns it; the relay runs until ctx is
// done. The default makes the DHT node on the bootstrap nodes that ctx carries, runs internal/relay on it and
// closes the node when ctx is done. Tests replace it with a fake, so that no test touches the network.
var startRelay = func(ctx context.Context, relayKey string, appKey [32]byte, logger *slog.Logger) (relayStatus, error) {
	d, err := hyperdht.New(hyperdht.Config{Bootstrap: bootstrapOf(ctx)})
	if err != nil {
		return nil, err
	}
	r, err := relay.Run(ctx, relayKey, appKey, d, logger)
	if err != nil {
		d.Close()
		return nil, err
	}
	// Close stops the servers of the node first, so the relay unannounces before its node goes.
	context.AfterFunc(ctx, func() { d.Close() })
	return r, nil
}

// relayContext returns the context that a foreground relay runs under: it ends on an interrupt or a
// termination signal. Tests replace it with a context that is already done.
var relayContext = func() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// relayCmd is holebridge relay [--new-key] [--bootstrap <host:port,...>]. With --new-key it creates relay.key
// in the config directory. Otherwise it runs the relay with the key in relay.key until it is interrupted, and
// prints its public key and its NAT state.
func relayCmd(args []string, env Env, configDir string) error {
	isNew, rest := takeFlag(args, "--new-key")
	words, opts, err := splitFlags(rest, "bootstrap")
	if err != nil {
		return err
	}
	if len(words) != 0 {
		return usage("relay takes no arguments other than --new-key and --bootstrap")
	}
	if _, ok := opts["bootstrap"]; ok && isNew {
		return usage("relay --new-key takes no --bootstrap")
	}
	nodes, err := bootstrapFlag(opts, env)
	if err != nil {
		return err
	}
	dir, err := requireDir(configDir, env)
	if err != nil {
		return err
	}
	if isNew {
		return newRelayKey(dir, env)
	}
	return runRelay(dir, nodes, env)
}

// relayCheckCmd is holebridge relay check <relay-key-file> [--bootstrap <host:port,...>]. It connects to the
// relay with this machine's app.key as a member and with a random key, and prints both results. It exits 0
// only when the member is admitted and the stranger is refused.
func relayCheckCmd(args []string, env Env, configDir string) error {
	words, opts, err := splitFlags(args, "bootstrap")
	if err != nil {
		return err
	}
	if len(words) != 1 {
		return usage("relay check wants a relay key file")
	}
	nodes, err := bootstrapFlag(opts, env)
	if err != nil {
		return err
	}
	dir, err := requireDir(configDir, env)
	if err != nil {
		return err
	}
	relayKey, err := readRelayKey(words[0])
	if err != nil {
		return err
	}
	appKey, err := config.LoadAppKey(dir)
	if err != nil {
		return err
	}
	d, err := keys.DeriveRelay(relayKey, appKey)
	if err != nil {
		return err
	}
	var server [32]byte
	copy(server[:], d.Server.Public().(ed25519.PublicKey))
	member := keyPairOf(d.Member)

	node, err := hyperdht.New(hyperdht.Config{Bootstrap: nodes})
	if err != nil {
		return err
	}
	defer node.Close()
	ready, cancel := context.WithTimeout(context.Background(), dhtWait)
	defer cancel()
	if err := node.Ready(ready); err != nil {
		return fmt.Errorf("DHT node not ready: %w", err)
	}

	memberErr := dialRelay(node, server, &member)
	strangerErr := dialRelay(node, server, nil)
	if memberErr == nil {
		fmt.Fprintln(env.Stdout, "member: admitted")
	} else {
		fmt.Fprintf(env.Stdout, "member: not admitted (%v)\n", memberErr)
	}
	switch {
	case strangerErr == nil:
		fmt.Fprintln(env.Stdout, "stranger: admitted")
	case errors.Is(strangerErr, hyperdht.ErrPeerConnectionFailed):
		fmt.Fprintln(env.Stdout, "stranger: refused")
	default:
		fmt.Fprintf(env.Stdout, "stranger: no answer (%v)\n", strangerErr)
	}

	switch {
	case memberErr != nil:
		return errs.E("HB-RELAY-REFUSED", "this machine's app.key is not admitted", nil)
	case strangerErr == nil:
		return errors.New("the relay admitted a key that is not a member: do not use this relay")
	case !errors.Is(strangerErr, hyperdht.ErrPeerConnectionFailed):
		return errors.New("the stranger got no refusal from the relay: check the relay key file and the relay's address")
	}
	return nil
}

// dialRelay connects to the relay under server with the key pair kp, or with the node's own key pair when kp
// is nil. It returns nil when the relay admitted the connection, and closes it.
func dialRelay(node *hyperdht.DHT, server [32]byte, kp *noise.KeyPair) error {
	ctx, cancel := context.WithTimeout(context.Background(), connectWait)
	defer cancel()
	c, err := node.Connect(ctx, server, hyperdht.ConnectOptions{KeyPair: kp})
	if err != nil {
		return err
	}
	c.Close()
	return nil
}

// keyPairOf returns the noise key pair of an ed25519 private key.
func keyPairOf(priv ed25519.PrivateKey) noise.KeyPair {
	var kp noise.KeyPair
	copy(kp.Public[:], priv.Public().(ed25519.PublicKey))
	copy(kp.Secret[:], priv)
	return kp
}

// bootstrapFlag returns the nodes of --bootstrap when the option is given, else the nodes of bootstrapFromEnv.
func bootstrapFlag(opts map[string]string, env Env) ([]string, error) {
	v, ok := opts["bootstrap"]
	if !ok {
		return bootstrapFromEnv(env)
	}
	return parseBootstrap(v)
}

// bootstrapFromEnv returns the bootstrap nodes that a host, a share, a relay or a relay check uses when no
// --bootstrap is given: the comma-separated host:port list of HOLEBRIDGE_BOOTSTRAP, else the default list. The
// variable is a testing hook for a private network (docs/cli.md, configuration).
func bootstrapFromEnv(env Env) ([]string, error) {
	v := env.Getenv("HOLEBRIDGE_BOOTSTRAP")
	if v == "" {
		return bootstrap, nil
	}
	nodes, err := parseBootstrap(v)
	if err != nil {
		return nil, usage("HOLEBRIDGE_BOOTSTRAP wants host:port nodes separated by commas, such as 192.0.2.1:49737")
	}
	return nodes, nil
}

// parseBootstrap reads a --bootstrap value: host:port nodes separated by commas. Each needs a host and a
// port from 1 to 65535.
func parseBootstrap(v string) ([]string, error) {
	nodes := strings.Split(v, ",")
	for _, n := range nodes {
		host, port, err := net.SplitHostPort(n)
		p, perr := strconv.Atoi(port)
		if err != nil || perr != nil || host == "" || p < 1 || p > 65535 {
			return nil, usage("--bootstrap wants host:port nodes separated by commas, such as 192.0.2.1:49737")
		}
	}
	return nodes, nil
}

// newRelayKey creates relay.key with a new relay key and prints the key once, with the path it was saved
// to. It refuses, and leaves the file alone, when relay.key exists. Its error never shows the file's content.
func newRelayKey(dir string, env Env) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, "relay.key")
	// O_EXCL makes the create fail, and not overwrite, when the file exists.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return errors.New("relay.key already exists: keep it, or move it aside and run relay --new-key again")
		}
		return err
	}
	k := keys.Format(keys.Generate())
	_, err = f.WriteString(k + "\n")
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path) // a partial file would block the next create
		return err
	}
	fmt.Fprintf(env.Stdout, "Relay key: %s (saved to %s, mode 0600)\n", k, path)
	return nil
}

// runRelay runs the relay with the key in dir/relay.key and the application key in dir/app.key until it is
// interrupted. It prints the relay's public key and its NAT state, then waits.
func runRelay(dir string, nodes []string, env Env) error {
	relayKey, err := readRelayKey(filepath.Join(dir, "relay.key"))
	if errors.Is(err, fs.ErrNotExist) {
		return errs.E("HB-RELAY-KEY-MISSING", "", err)
	}
	if err != nil {
		return err
	}
	appKey, err := config.LoadAppKey(dir)
	if err != nil {
		return err
	}
	ctx, stop := relayContext()
	defer stop()
	ctx = context.WithValue(ctx, bootstrapKey{}, nodes)
	r, err := startRelay(ctx, relayKey, appKey, log.New(env.Stderr, env.LogLevel))
	if err != nil {
		return err
	}
	pub := r.PublicKey()
	fmt.Fprintf(env.Stdout, "Relay public key %s\n", hex.EncodeToString(pub[:]))
	nat := r.NAT()
	fmt.Fprintf(env.Stdout, "Public UDP %s firewalled=%t randomized=%t\n",
		net.JoinHostPort(nat.Host, strconv.Itoa(nat.Port)), nat.Firewalled, nat.Randomized)
	<-ctx.Done()
	return nil
}

// readRelayKey reads a relay key file: the key as XXX-XXX-XXX and a newline, in any form keys.Normalize
// accepts. On POSIX systems a file that group or others can read returns HB-RELAY-KEY-PERMS, as
// LoadAppKey does for app.key. A malformed key fails with HB-KEY-INVALID, which never shows the key.
func readRelayKey(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if runtime.GOOS != "windows" {
		info, err := f.Stat()
		if err != nil {
			return "", err
		}
		if info.Mode().Perm()&0o077 != 0 {
			return "", errs.E("HB-RELAY-KEY-PERMS", "", nil)
		}
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	return keys.Normalize(strings.TrimSpace(string(b)))
}
