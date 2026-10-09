// Package relay is the holebridge relay: a blind relay (pears/blindrelay) on a HyperDHT node, run under one
// relay key (docs/architecture.md, Relay route; docs/security.md, Relay keys).
package relay

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"log/slog"
	"time"

	"github.com/andrewloable/HoleBridge/internal/keys"
	"github.com/andrewloable/HoleBridge/pears/blindrelay"
	"github.com/andrewloable/HoleBridge/pears/dhtrpc"
	"github.com/andrewloable/HoleBridge/pears/hyperdht"
	"github.com/andrewloable/HoleBridge/pears/noise"
	"github.com/andrewloable/HoleBridge/pears/protomux"
)

// reportEvery is how often the relay logs its pairing count.
const reportEvery = 10 * time.Minute

// readyWait bounds the bootstrap of the relay's DHT node, which Run waits for before it listens.
const readyWait = 30 * time.Second

// newTicker starts the report ticker: the channel that ticks every d, and the function that stops it. Tests
// replace it with a fake clock.
var newTicker = func(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}

// Relay is a running relay: it listens under its server key pair and pairs the connections of its members.
type Relay struct {
	pub [32]byte
	dht *hyperdht.DHT
}

// Run derives the server and member key pairs of relayKey (normalized) and appKey, listens under the server
// key pair with a firewall that admits only the member public key, runs the blind relay, and logs the pairing
// count every 10 minutes. It never logs the relay key. The relay runs until ctx is done.
func Run(ctx context.Context, relayKey string, appKey [32]byte, dht *hyperdht.DHT, log *slog.Logger) (*Relay, error) {
	d, err := keys.DeriveRelay(relayKey, appKey)
	if err != nil {
		return nil, err
	}
	var server noise.KeyPair
	copy(server.Public[:], d.Server.Public().(ed25519.PublicKey))
	copy(server.Secret[:], d.Server)
	var member [32]byte
	copy(member[:], d.Member.Public().(ed25519.PublicKey))

	rctx, cancel := context.WithTimeout(ctx, readyWait)
	err = dht.Ready(rctx)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("relay: DHT node not ready: %w", err)
	}

	srv := dht.CreateServer(hyperdht.ServerOptions{Firewall: func(pk [32]byte, _ hyperdht.HandshakePayload) bool {
		return pk != member // refuse every key but the member's
	}})
	if err := srv.Listen(ctx, server); err != nil {
		srv.Close()
		return nil, err
	}
	context.AfterFunc(ctx, func() { srv.Close() })

	bs := blindrelay.NewServer(blindrelay.ServerOptions{Socket: dht.Socket()})
	go func() {
		for {
			c, err := srv.Accept()
			if err != nil {
				return
			}
			pk := c.RemotePublicKey()
			bs.Accept(protomux.New(c), pk[:])
		}
	}()

	ticks, stop := newTicker(reportEvery)
	go func() {
		defer stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticks:
				log.Info(fmt.Sprintf("pairings matched: %d", bs.Pairings()))
			}
		}
	}()

	return &Relay{pub: server.Public, dht: dht}, nil
}

// PublicKey returns the public key of the server key pair, the key members dial.
func (r *Relay) PublicKey() [32]byte {
	return r.pub
}

// NAT returns the NAT state of the relay's DHT node.
func (r *Relay) NAT() dhtrpc.NATInfo {
	return r.dht.NAT()
}
