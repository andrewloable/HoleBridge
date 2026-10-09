package host

import (
	"testing"

	"github.com/andrewloable/HoleBridge/internal/keys"
)

// The host gives its server a relayThrough policy only when host.json has a relay (docs/architecture.md, Relay route).
// serverOptions is what listen passes to CreateServer, so the test reads the options the server gets, without a network.
// The testnet node is not randomized, so the policy declines when it is not forced; the forced offer is the relay key.
func TestHostServerRelayThroughIsSetOnlyWithARelay(t *testing.T) {
	r := newRig(t)

	t.Run("with a relay", func(t *testing.T) {
		relayKey := keys.Generate()
		cfg := r.hostConfig(t, echoServices(t), nil)
		cfg.Relay = relayKey
		h := r.newWireHost(t, cfg, nil)

		opts := h.serverOptions(h.clientKey())
		if opts.RelayThrough == nil {
			t.Fatal("the server of a host with a relay has no relayThrough policy")
		}
		if opts.Firewall == nil || opts.Keepalive != keepalive {
			t.Fatal("serverOptions dropped the firewall or the keepalive of the server")
		}
		want, err := RelayServerPublicKey(relayKey, r.appKey)
		if err != nil {
			t.Fatalf("RelayServerPublicKey: %v", err)
		}
		got := opts.RelayThrough(true)
		if got == nil || *got != want {
			t.Fatal("the policy does not offer the relay server's public key when the dial is forced")
		}
		if got := opts.RelayThrough(false); got != nil {
			t.Fatal("the policy offers the relay on a consistent NAT when the dial is not forced")
		}
	})

	t.Run("without a relay", func(t *testing.T) {
		cfg := r.hostConfig(t, echoServices(t), nil)
		h := r.newWireHost(t, cfg, nil)
		if opts := h.serverOptions(h.clientKey()); opts.RelayThrough != nil {
			t.Fatal("a host with no relay has a relayThrough policy on its server")
		}
	})
}
