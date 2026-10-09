package cli

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strconv"

	"github.com/andrewloable/HoleBridge/internal/config"
	"github.com/andrewloable/HoleBridge/internal/keys"
	"github.com/andrewloable/HoleBridge/internal/log"
)

// The share command registers itself here, as the host command does.
func init() {
	commands["share"] = shareCmd
}

// shareCmd is holebridge share <target> [--name <n>] [--kind <k>], run through the default runner (see shareWith).
func shareCmd(args []string, env Env, configDir string) error {
	return shareWith(defaultHostRunner{})(args, env, configDir)
}

// shareWith returns the share command, which runs its host through r (see hostRunner). The host serves one service
// on a new temporary key, which lives as long as the process; the command prints the Sharing line and the banner with
// the key block, and never writes host.json. The application key comes from app.key in the config directory, as it
// does for host.
func shareWith(r hostRunner) Command {
	return func(args []string, env Env, configDir string) error {
		words, opts, err := splitFlags(args, "name", "kind")
		if err != nil {
			return err
		}
		if len(words) != 1 {
			return usage("share wants one target, such as 8080 or 192.168.1.20:445")
		}
		host, port, err := config.ParseTarget(words[0])
		if err != nil {
			return usage("a share target is a port or host:port, such as 8080 or 203.0.113.20:445")
		}
		name := strconv.Itoa(port)
		if v, ok := opts["name"]; ok {
			name = v
		}
		if !config.ValidServiceName(name) {
			return usage("a service name is 1 to 32 characters: a-z, 0-9 and dash, starting with a letter or digit")
		}
		kind, hasKind := opts["kind"]
		if hasKind && !slices.Contains(serviceKinds, kind) {
			return usage("--kind must be https, http, tcp or udp")
		}
		dir, err := requireDir(configDir, env)
		if err != nil {
			return err
		}
		nodes, err := bootstrapFromEnv(env)
		if err != nil {
			return err
		}
		appKey, err := loadOrCreateAppKey(dir)
		if err != nil {
			return err
		}

		target := net.JoinHostPort(host, strconv.Itoa(port))
		cfg := config.Defaults()
		cfg.Key = keys.Generate()
		cfg.Services[name] = config.Service{Target: target, Kind: kind}
		block, err := keyBlock(cfg.Key, appKey)
		if err != nil {
			return err
		}

		ctx, stop := hostContext()
		defer stop()
		runCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		logger := log.New(env.Stderr, env.LogLevel)

		rh, err := r.Start(runCtx, hostRequest{
			Config:    cfg,
			AppKey:    appKey,
			LAN:       true,
			Bootstrap: nodes,
			Logger:    logger,
		})
		if err != nil {
			return err
		}
		printBanner(env.Stdout, fmt.Sprintf("Sharing %s as %q (temporary key)", target, name), block, rh, true, false)
		return rh.Wait()
	}
}
