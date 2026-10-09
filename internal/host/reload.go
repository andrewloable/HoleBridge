package host

import (
	"crypto/ed25519"
	"errors"

	"github.com/andrewloable/HoleBridge/internal/config"
	"github.com/andrewloable/HoleBridge/internal/keys"
	"github.com/andrewloable/HoleBridge/pears/noise"
)

// Reload re-reads host.json from Options.Dir and applies it to the running host (docs/cli.md, hosting). Added
// services become available. Removed services reject new opens and keep their open streams. Changed targets
// apply to new streams. A changed key re-listens under the new key and closes the old sessions. Every live
// session is then sent a services message (docs/architecture.md, message 8). It runs when service add or rm
// reaches the control socket; SIGHUP runs it too once the host command wires it.
//
// The file is read and checked before anything changes, so a bad host.json leaves the host as it was. The LAN
// route, the limits and the relay key are read at start and do not change here: the DHT node's default key pair is
// set when the node is made, so a relay change takes a restart.
func (h *Host) Reload() error {
	if h.dir == "" {
		return errors.New("host: no config directory to reload from")
	}
	h.lifeMu.Lock()
	defer h.lifeMu.Unlock()

	cfg, err := config.Load(h.dir)
	if err != nil {
		return err
	}
	names, services, err := buildServices(cfg)
	if err != nil {
		return err
	}
	d, err := keys.Derive(cfg.Key, h.appKey)
	if err != nil {
		return err
	}
	var kp noise.KeyPair
	copy(kp.Public[:], d.Host.Public().(ed25519.PublicKey))
	copy(kp.Secret[:], d.Host)
	var clientPub [32]byte
	copy(clientPub[:], d.Client.Public().(ed25519.PublicKey))

	h.mu.Lock()
	running := h.srv != nil
	rekey := kp.Public != h.kp.Public
	h.mu.Unlock()
	if running && rekey {
		if err := h.listen(h.ctx, kp, clientPub); err != nil {
			return err
		}
	} else {
		h.mu.Lock()
		h.kp, h.clientPub = kp, clientPub
		h.mu.Unlock()
	}

	h.mu.Lock()
	h.names, h.services = names, services
	h.mu.Unlock()

	// The list is taken after the services change, so every session gets the new one: a session whose handshake
	// is still to go out takes it from the host, and an open one is pushed it here.
	list := h.servicesList()
	for _, l := range h.liveLinks() {
		l.addUDP()
		l.pushServices(list)
	}
	return nil
}
