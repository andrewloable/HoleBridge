package hyperdht

import (
	"context"
	"net"
	"testing"

	"github.com/andrewloable/HoleBridge/pears/dhtrpc"
	"github.com/andrewloable/HoleBridge/pears/noise"
)

// clientHandshake starts the client's handshake to the server key pair and returns it with its first message.
func clientHandshake(t *testing.T, client, server noise.KeyPair) (*noise.Handshake, []byte) {
	t.Helper()
	hs := noise.NewInitiator(client, server.Public, nsPeerHandshake[:])
	payload, err := EncodeNoisePayload(NoisePayload{
		UDX:          &UDXInfo{Version: 1, ID: 1},
		SecretStream: &SecretStreamInfo{Version: 1},
	})
	must(t, err)
	msg1, err := hs.Send(payload)
	must(t, err)
	return hs, msg1
}

// handshakeReplyTo returns the value of the handshake reply that the server key pair gives to the client's
// first message msg1. The reply verifies for the client that sent msg1.
func handshakeReplyTo(t *testing.T, server noise.KeyPair, msg1 []byte) []byte {
	t.Helper()
	resp := noise.NewResponder(server, nsPeerHandshake[:])
	_, err := resp.Recv(msg1)
	must(t, err)
	body, err := EncodeNoisePayload(NoisePayload{
		UDX:          &UDXInfo{Version: 1, ID: 2},
		SecretStream: &SecretStreamInfo{Version: 1},
	})
	must(t, err)
	msg2, err := resp.Send(body)
	must(t, err)
	value, err := EncodeHandshake(Handshake{Mode: handshakeReply, Noise: msg2})
	must(t, err)
	return value
}

// Test case 1: a handshake reply that verifies but comes from another address than the node asked is ignored.
// Upstream's peerHandshake rejects such a reply (BAD_HANDSHAKE_REPLY). The attempt does not start, so a reply
// from another node cannot start the relay path or claim the stream.
func TestHandshakeReplyFromOtherNodeIgnored(t *testing.T) {
	server, client := testKeyPair(7), testKeyPair(3)
	hs, msg1 := clientHandshake(t, client, server)
	value := handshakeReplyTo(t, server, msg1)
	asked := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 7001}
	other := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 7002}
	a := &attempt{hs: hs, forced: true, relayed: make(chan claimResult, 1)}
	c, from, err := a.takeReply(context.Background(), asked, &dhtrpc.Response{Value: value, From: other}, nil)
	if c != nil || from != nil || err != nil {
		t.Fatalf("takeReply = (%v, %v, %v), want no connection and no error", c, from, err)
	}
	if a.started {
		t.Error("a verifying reply from a node other than the one asked started the attempt")
	}
}

// Test case 2: the same verifying reply, sent by the node the client asked, is accepted. It starts the attempt.
func TestHandshakeReplyFromAskedNodeAccepted(t *testing.T) {
	server, client := testKeyPair(7), testKeyPair(3)
	hs, msg1 := clientHandshake(t, client, server)
	value := handshakeReplyTo(t, server, msg1)
	asked := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 7001}
	a := &attempt{hs: hs, forced: true, relayed: make(chan claimResult, 1)}
	if _, _, err := a.takeReply(context.Background(), asked, &dhtrpc.Response{Value: value, From: asked}, nil); err != nil {
		t.Fatalf("takeReply: %v", err)
	}
	if !a.started {
		t.Error("a verifying reply from the node asked did not start the attempt")
	}
}
