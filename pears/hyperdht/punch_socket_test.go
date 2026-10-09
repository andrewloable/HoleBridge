package hyperdht

import (
	"context"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/pears/dhtrpc"
)

// A punch socket sends its dht-rpc requests and its NAT samples from itself. A birthday socket has a NAT mapping of its
// own, so a ping from it names that socket's address, and a request from it gets its reply back on it. The handle of
// the node's socket names the node's address (upstream nat.autoSample pings and updateHolepunch's socket option).

// dhtPingCommand is PING, the built-in dht-rpc command 0 (dht-rpc lib/commands.js).
const dhtPingCommand = 0

func TestPunchSocketObserveAndRequestUseTheirOwnSocket(t *testing.T) {
	tn := startTestnet(t, 3)
	d := tn.Nodes[0]
	peer := nodeAddr(t, tn.Nodes[1])
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	birthday := acquireBirthday(t, d)
	own := addressOf(birthday.Local())
	var seen Address
	var err error
	callStub(t, "Observe", func() { seen, err = birthday.Observe(ctx, peer) })
	failIfStub(t, err)
	must(t, err)
	if seen != own {
		t.Errorf("Observe from a birthday socket names %v, want the socket's own address %v", seen, own)
	}

	var resp *dhtrpc.Response
	callStub(t, "Request", func() {
		resp, err = birthday.Request(ctx, peer, dhtrpc.Request{Internal: true, Command: dhtPingCommand})
	})
	failIfStub(t, err)
	must(t, err)
	if resp.Error != 0 {
		t.Fatalf("the request from a birthday socket got error %d", resp.Error)
	}
	if got := (Address{Host: resp.To.Host, Port: resp.To.Port}); got != own {
		t.Errorf("the reply to a request from a birthday socket names %v, want the socket's own address %v", got, own)
	}

	node := punchSocketOf(t, d)
	callStub(t, "Observe", func() { seen, err = node.Observe(ctx, peer) })
	failIfStub(t, err)
	must(t, err)
	if want := addressOf(nodeAddr(t, d)); seen != want {
		t.Errorf("Observe from the node's handle names %v, want the node's own address %v", seen, want)
	}
}
