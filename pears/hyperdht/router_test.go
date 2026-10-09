package hyperdht

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/pears/dhtrpc"
	"github.com/andrewloable/HoleBridge/pears/noise"
)

// Handshake routing through a relay. The server runs on node 0, which answers the FIND_PEER of a client
// first, so the other nodes that hold the server's record are the relays. A handshake sent to one of them
// is relayed to the server, and the server's reply comes back through it to the client.
func TestHandshakeRelayedThroughHolder(t *testing.T) {
	tn := startTestnet(t, 10)
	host := testKeyPair(3)
	srv := newServer(t, tn.Nodes[0], ServerOptions{})
	listenOn(t, srv, host)
	serverAddr := nodeAddr(t, tn.Nodes[0])
	var holder *net.UDPAddr
	for _, a := range findPeerAll(t, tn, hashKey(host.Public)) {
		if a.from.Port != serverAddr.Port && bytes.Equal(a.peer.PublicKey, host.Public[:]) {
			holder = a.from
			break
		}
	}
	if holder == nil {
		t.Fatal("no node other than the server holds its record, so there is no relay to test")
	}
	ans, ok := handshakeAt(t, holder, host.Public, testKeyPair(5))
	if !ok {
		t.Fatal("the handshake relayed through a holder got no answer")
	}
	if ans.serverKey != host.Public {
		t.Error("the relayed answer does not prove the server's key")
	}
	if ans.payload.Error != 0 {
		t.Errorf("relayed answer error = %d, want 0", ans.payload.Error)
	}
}

// A handshake for a key that no node routes gets no value, and the closest nodes to the target, so the
// client can route on, as upstream's reply does when it has no relay for the target.
func TestHandshakeWithoutRouteNamesCloserNodes(t *testing.T) {
	tn := startTestnet(t, 10)
	host := testKeyPair(3)
	target := hashKey(host.Public)
	kp := testKeyPair(5)
	prologue := dhtNamespace(cmdPeerHandshake)
	payload, err := EncodeNoisePayload(NoisePayload{
		UDX:          &UDXInfo{Version: 1, ID: 1},
		SecretStream: &SecretStreamInfo{Version: 1},
	})
	must(t, err)
	hs := noise.NewInitiator(kp, host.Public, prologue[:])
	msg1, err := hs.Send(payload)
	must(t, err)
	value, err := EncodeHandshake(Handshake{Mode: handshakeFromClient, Noise: msg1})
	must(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := newClient(t).Request(ctx, nodeAddr(t, tn.Nodes[4]), dhtrpc.Request{Command: cmdPeerHandshake, Target: target[:], Value: value})
	if err != nil {
		t.Fatalf("handshake with no route got no reply: %v", err)
	}
	if len(resp.Value) != 0 {
		t.Error("handshake with no route carries a value, want none")
	}
	if len(resp.CloserNodes) == 0 {
		t.Error("handshake with no route names no closer nodes")
	}
}
