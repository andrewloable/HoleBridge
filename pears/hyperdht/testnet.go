package hyperdht

import (
	"testing"

	"github.com/andrewloable/HoleBridge/pears/dhtrpc"
)

// Testnet is a set of HyperDHT nodes on 127.0.0.1 in one process, as hyperdht's testnet.js builds them.
// Bootstrap is the host:port of the first node, and Nodes lists every node, the first one first.
type Testnet struct {
	Bootstrap []string
	Nodes     []*DHT
}

// NewTestnet starts size nodes on 127.0.0.1 and waits until each is ready. t.Cleanup stops them. The nodes
// are the dhtrpc testnet's nodes, each with a random key pair and the HyperDHT commands answered.
func NewTestnet(t testing.TB, size int) *Testnet {
	t.Helper()
	dt := dhtrpc.NewTestnet(t, size)
	tn := &Testnet{Bootstrap: dt.Bootstrap}
	for _, n := range dt.Nodes {
		kp, err := keyPair(nil)
		if err != nil {
			t.Fatalf("testnet key pair: %v", err)
		}
		tn.Nodes = append(tn.Nodes, newDHT(n, kp))
	}
	return tn
}
