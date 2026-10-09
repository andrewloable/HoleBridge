package dhtrpc

import (
	"context"
	"net"
	"testing"
	"time"
)

// Testnet is a set of DHT nodes on 127.0.0.1 in one process. The first node is the bootstrap node, and
// the others bootstrap from it.
type Testnet struct {
	Bootstrap []string // host:port of the bootstrap node
	Nodes     []*Node
}

// NewTestnet starts size nodes and waits until each is ready. t.Cleanup closes them. Each node is
// persistent and starts after the one before it is ready, so every node's table holds the others.
func NewTestnet(t testing.TB, size int) *Testnet {
	t.Helper()
	tn := &Testnet{}
	ephemeral := false
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := range size {
		n, err := open(Config{Ephemeral: &ephemeral, Bootstrap: tn.Bootstrap}, net.IPv4(127, 0, 0, 1))
		if err != nil {
			t.Fatalf("testnet node %d: %v", i, err)
		}
		t.Cleanup(func() { n.Close() })
		tn.Nodes = append(tn.Nodes, n)
		if i == 0 {
			tn.Bootstrap = []string{n.local.String()}
		}
		if err := n.Ready(ctx); err != nil {
			t.Fatalf("testnet node %d not ready: %v", i, err)
		}
	}
	return tn
}
