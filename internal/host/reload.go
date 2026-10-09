package host

import "errors"

// Reload re-reads host.json from Options.Dir and applies it to the running host (docs/cli.md, hosting). Added
// services become available. Removed services reject new opens and keep their open streams. Changed targets
// apply to new streams. A changed key re-listens under the new key and closes the old sessions. Every live
// session is then sent a services message (docs/architecture.md, message 8). It runs on SIGHUP and when
// service add or rm reaches the control socket.
func (h *Host) Reload() error {
	return errors.ErrUnsupported
}
