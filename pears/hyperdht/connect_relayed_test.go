package hyperdht

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/pears/dhtrpc"
	"github.com/andrewloable/HoleBridge/pears/noise"
)

// claimWait bounds takeReply in these tests. A direct claim waits for the secret stream's header exchange, which no
// peer answers here, so takeReply gives up after this long; the claim itself is made before the wait.
const claimWait = 2 * time.Second

// replyFrom returns the value of the server's handshake reply to msg1. peer is the server address the reply names
// (PeerAddress, as a relayed reply carries it), or nil for a reply that came straight from the server. hp is the
// holepunch part of the reply, or nil for none.
func replyFrom(t *testing.T, server noise.KeyPair, msg1 []byte, peer *Address, hp *HolepunchInfo) []byte {
	t.Helper()
	resp := noise.NewResponder(server, nsPeerHandshake[:])
	_, err := resp.Recv(msg1)
	must(t, err)
	body, err := EncodeNoisePayload(NoisePayload{
		UDX:          &UDXInfo{Version: 1, ID: 2},
		SecretStream: &SecretStreamInfo{Version: 1},
		Holepunch:    hp,
	})
	must(t, err)
	msg2, err := resp.Send(body)
	must(t, err)
	value, err := EncodeHandshake(Handshake{Mode: handshakeReply, Noise: msg2, PeerAddress: peer})
	must(t, err)
	return value
}

// newConnectAttempt returns a connect attempt of d for the server key pair, with its client handshake hs and a
// stream of its own. The attempt's context ends with the test, and its punch, if one starts, is stopped then.
func newConnectAttempt(t *testing.T, d *DHT, server noise.KeyPair, hs *noise.Handshake) *attempt {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	a := &attempt{d: d, ctx: ctx, pk: server.Public, st: d.newStream(), hs: hs, relayed: make(chan claimResult, 2)}
	t.Cleanup(func() {
		cancel()
		if a.punch != nil {
			a.punch.destroy()
		}
		a.st.Destroy()
	})
	return a
}

// Test case 1: a verified reply that came through another node (it names the server address, which is not the node
// asked) is relayed, so the connect holepunches (upstream connectThroughNode: a relayed reply is not claimed
// directly, holepunch runs). The reply names a holepunch relay, so the punch starts, and the stream is not claimed
// at the server address. Upstream reaches the punch with the probe round through the relay.
func TestRelayedReplyStartsPunchAndDoesNotClaim(t *testing.T) {
	tn := NewTestnet(t, 1)
	server, client := testKeyPair(7), testKeyPair(3)
	hs, msg1 := clientHandshake(t, client, server)
	asked := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 7001}
	serverAddr := Address{Host: netip.MustParseAddr("127.0.0.1"), Port: 7100}
	hp := &HolepunchInfo{ID: 1, Relays: []RelayInfo{{RelayAddress: addressOf(asked), PeerAddress: serverAddr}}}
	value := replyFrom(t, server, msg1, &serverAddr, hp)

	a := newConnectAttempt(t, tn.Nodes[0], server, hs)
	ctx, cancel := context.WithTimeout(context.Background(), claimWait)
	defer cancel()
	c, _, err := a.takeReply(ctx, asked, &dhtrpc.Response{Value: value, From: asked}, nil)
	if c != nil {
		c.Close()
		t.Fatal("a relayed reply gave a connection at once, want the hole punch to make it")
	}
	if !a.started {
		t.Fatal("a verifying relayed reply did not start the attempt")
	}
	if a.claim.taken.Load() {
		t.Error("a relayed reply claimed the stream at the server address, want the hole punch to claim it")
	}
	if a.punch == nil {
		t.Errorf("a relayed reply with holepunch relays did not start the hole punch (takeReply err %v)", err)
	}
}

// Test case 2: a verified reply that the node asked sent itself, with no relay in between, claims the stream
// directly, even when the server names holepunch relays. No punch starts.
func TestDirectReplyClaimsStreamWithoutPunch(t *testing.T) {
	tn := NewTestnet(t, 1)
	server, client := testKeyPair(7), testKeyPair(3)
	hs, msg1 := clientHandshake(t, client, server)
	asked := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 7001}
	hp := &HolepunchInfo{ID: 1, Relays: []RelayInfo{{RelayAddress: addressOf(asked), PeerAddress: addr("127.0.0.1", 7100)}}}
	value := replyFrom(t, server, msg1, nil, hp)

	a := newConnectAttempt(t, tn.Nodes[0], server, hs)
	ctx, cancel := context.WithTimeout(context.Background(), claimWait)
	defer cancel()
	_, _, _ = a.takeReply(ctx, asked, &dhtrpc.Response{Value: value, From: asked}, nil)
	if !a.claim.taken.Load() {
		t.Error("a direct reply did not claim the stream")
	}
	if a.punch != nil {
		t.Error("a direct reply started a hole punch, want the direct claim only")
	}
}

// Test case 3: a relayed reply from a server that names no holepunch relays cannot be punched. Upstream claims it
// directly at the server address (the relayed and not holepunchable fallback of holepunch), so no punch starts.
func TestRelayedReplyWithoutRelaysClaimsServerAddress(t *testing.T) {
	tn := NewTestnet(t, 1)
	server, client := testKeyPair(7), testKeyPair(3)
	hs, msg1 := clientHandshake(t, client, server)
	asked := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 7001}
	serverAddr := addr("127.0.0.1", 7100)
	value := replyFrom(t, server, msg1, &serverAddr, nil)

	a := newConnectAttempt(t, tn.Nodes[0], server, hs)
	ctx, cancel := context.WithTimeout(context.Background(), claimWait)
	defer cancel()
	_, _, _ = a.takeReply(ctx, asked, &dhtrpc.Response{Value: value, From: asked}, nil)
	if !a.claim.taken.Load() {
		t.Error("a relayed reply that names no holepunch relays did not claim the stream at the server address")
	}
	if a.punch != nil {
		t.Error("a relayed reply that names no holepunch relays started a hole punch")
	}
}
