package udx

import (
	"math/rand/v2"
	"net"
	"testing"
	"time"
)

// reorderHold is how long a datagram chosen for reordering is held back, so later ones pass it.
const reorderHold = 3 * time.Millisecond

// linkConfig sets the impairments of an in-process link. Each datagram is dropped with
// probability Loss, duplicated with probability Duplicate and held back with probability Reorder,
// and every datagram is delayed by Delay. Seed makes a run repeatable.
type linkConfig struct {
	Loss      float64
	Duplicate float64
	Reorder   float64
	Delay     time.Duration
	Seed      uint64
}

// relay reads datagrams from in and sends each one to dst out of via, after applying cfg. The
// relay runs in this process; it ends when the test closes in.
func relay(cfg linkConfig, direction uint64, in, via *net.UDPConn, dst *net.UDPAddr) {
	rng := rand.New(rand.NewPCG(cfg.Seed, direction))
	buf := make([]byte, 65536)
	for {
		n, _, err := in.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if rng.Float64() < cfg.Loss {
			continue
		}
		pkt := append([]byte(nil), buf[:n]...)
		copies := 1
		if rng.Float64() < cfg.Duplicate {
			copies = 2
		}
		for i := 0; i < copies; i++ {
			d := cfg.Delay
			if rng.Float64() < cfg.Reorder {
				d += reorderHold
			}
			send := func() { via.WriteToUDP(pkt, dst) }
			if d == 0 {
				send()
			} else {
				time.AfterFunc(d, send)
			}
		}
	}
}

// pairStreams returns two streams, local ids 1001 and 2002, each on its own Socket. The two UDP
// conns reach each other only through relays in this process, which apply cfg to every datagram.
func pairStreams(t *testing.T, cfg linkConfig) (a, b *Stream) {
	t.Helper()
	cA, cB := loopbackUDP(t), loopbackUDP(t)
	rA, rB := loopbackUDP(t), loopbackUDP(t)
	sockA, err := NewSocket(cA)
	if err != nil {
		t.Fatalf("NewSocket: %v", err)
	}
	t.Cleanup(func() { sockA.Close() })
	sockB, err := NewSocket(cB)
	if err != nil {
		t.Fatalf("NewSocket: %v", err)
	}
	t.Cleanup(func() { sockB.Close() })
	go relay(cfg, 1, rA, rB, cB.LocalAddr().(*net.UDPAddr))
	go relay(cfg, 2, rB, rA, cA.LocalAddr().(*net.UDPAddr))

	a = sockA.NewStream(1001)
	b = sockB.NewStream(2002)
	t.Cleanup(func() { a.Close(); b.Close() })
	if err := a.Connect(2002, rA.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := b.Connect(1001, rB.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	return a, b
}

// With 5% loss, 5% reordering and 1% duplication the bytes still arrive intact and in order.
func TestStreamsSurviveLossReorderAndDuplication(t *testing.T) {
	defer failOnPanic(t)
	a, b := pairStreams(t, linkConfig{Loss: 0.05, Reorder: 0.05, Duplicate: 0.01, Delay: time.Millisecond, Seed: 7})
	sendAndCheck(t, a, b, payload(4<<20), time.Minute)
}
