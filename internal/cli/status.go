package cli

import "errors"

// The status command registers itself here, as the other commands do.
func init() {
	commands["status"] = statusCmd
}

// statusCmd is holebridge status. It asks the control socket in the config directory for the
// snapshot of the running host or relay and prints it. With no socket answering it fails with
// "holebridge is not running".
func statusCmd(args []string, env Env, configDir string) error {
	return errors.ErrUnsupported
}
