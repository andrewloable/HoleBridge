package cli

import (
	"context"
	"errors"
	"log/slog"

	"github.com/andrewloable/HoleBridge/pears/dhtrpc"
)

// The relay commands register themselves here, as the key and service commands do.
func init() {
	commands["relay"] = relayCmd
	commands["relay check"] = relayCheckCmd
}

// bootstrap lists the bootstrap nodes of the DHT nodes that the relay and relay check commands start.
// Tests replace it with the nodes of a testnet.
var bootstrap []string

// relayStatus is what the relay command prints about a running relay. *relay.Relay has both methods.
type relayStatus interface {
	PublicKey() [32]byte
	NAT() dhtrpc.NATInfo
}

// startRelay starts the relay under relayKey and appKey and returns it; the relay runs until ctx is
// done. The default makes the DHT node on bootstrap, runs internal/relay on it and closes the node
// when ctx is done. Tests replace it with a fake, so that no test touches the network.
var startRelay = func(ctx context.Context, relayKey string, appKey [32]byte, logger *slog.Logger) (relayStatus, error) {
	return nil, errors.ErrUnsupported
}

// relayContext returns the context that a foreground relay runs under: it ends on an interrupt or a
// termination signal. Tests replace it with a context that is already done.
var relayContext = func() (context.Context, context.CancelFunc) {
	panic("not implemented")
}

// relayCmd is holebridge relay [--new-key]. With --new-key it creates relay.key in the config
// directory. Otherwise it runs the relay with the key in relay.key until it is interrupted, and
// prints its public key and its NAT state.
func relayCmd(args []string, env Env, configDir string) error {
	return errors.ErrUnsupported
}

// relayCheckCmd is holebridge relay check <relay-key-file>. It connects to the relay with this
// machine's app.key as a member and with a random key, and prints both results.
func relayCheckCmd(args []string, env Env, configDir string) error {
	return errors.ErrUnsupported
}
