package host

import (
	"crypto/ed25519"

	"github.com/andrewloable/HoleBridge/internal/keys"
	"github.com/andrewloable/HoleBridge/pears/dhtrpc"
	"github.com/andrewloable/HoleBridge/pears/noise"
)

// DefaultKeyPair returns the DHT default key pair of a host or app with relayKey under appKey: the member key pair of
// the relay key (docs/security.md, Relay keys). It returns nil and no error when relayKey is empty, since without a
// relay the DHT keeps its random default key pair. An invalid relay key is an error (HB-KEY-INVALID).
func DefaultKeyPair(relayKey string, appKey [32]byte) (*noise.KeyPair, error) {
	if relayKey == "" {
		return nil, nil
	}
	d, err := keys.DeriveRelay(relayKey, appKey)
	if err != nil {
		return nil, err
	}
	kp := kpFromPriv(d.Member)
	return &kp, nil
}

// RelayServerPublicKey returns the public key of the relay server that relayKey names under appKey, the key the relay
// listens under and the key a relayThrough policy offers (docs/architecture.md, Relay route). An invalid relay key is
// an error (HB-KEY-INVALID).
func RelayServerPublicKey(relayKey string, appKey [32]byte) ([32]byte, error) {
	d, err := keys.DeriveRelay(relayKey, appKey)
	if err != nil {
		return [32]byte{}, err
	}
	return kpFromPriv(d.Server).Public, nil
}

// RelayThrough returns the relayThrough policy of a server (hyperdht.ServerOptions.RelayThrough) for the relay server
// relayServer. The policy returns &relayServer when force is true or when nat reports a randomized NAT
// (dhtrpc.NATInfo.Randomized), and nil otherwise. nat is read at each call. A nil relayServer, a host with no relay
// key, gives a nil policy.
func RelayThrough(relayServer *[32]byte, nat func() dhtrpc.NATInfo) func(force bool) *[32]byte {
	if relayServer == nil {
		return nil
	}
	return func(force bool) *[32]byte {
		if force || nat().Randomized {
			k := *relayServer
			return &k
		}
		return nil
	}
}

// kpFromPriv returns the key pair of an ed25519 private key: its public key and the 64-byte secret, as the DHT takes it.
func kpFromPriv(priv ed25519.PrivateKey) noise.KeyPair {
	var kp noise.KeyPair
	copy(kp.Public[:], priv.Public().(ed25519.PublicKey))
	copy(kp.Secret[:], priv)
	return kp
}
