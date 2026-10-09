package hyperdht

// Edge cases of the holepunch code that the paired tests do not reach: the payload coder's rejections, and the
// server's answer to a probe that asks to punch, to a probe with no admitted handshake, and to a relayed probe.

import (
	"bytes"
	"net"
	"reflect"
	"testing"
)

// TestSecurePayloadRejectsTamperedAndForeignPayloads checks that a payload round-trips under its secret, and that
// a changed byte, another secret, or a message with no ciphertext does not decrypt.
func TestSecurePayloadRejectsTamperedAndForeignPayloads(t *testing.T) {
	sp := newSecurePayload([32]byte{1, 2, 3})
	in := HolepunchPayload{Error: 0, Firewall: firewallConsistent, Round: 7, Token: bytes.Repeat([]byte{9}, 32)}
	enc, err := sp.encrypt(in)
	must(t, err)
	got, ok := sp.decrypt(enc)
	if !ok || !reflect.DeepEqual(got, in) {
		t.Fatalf("round trip = %+v, %v, want %+v", got, ok, in)
	}

	tampered := append([]byte(nil), enc...)
	tampered[len(tampered)-1] ^= 1
	if _, ok := sp.decrypt(tampered); ok {
		t.Error("a tampered payload decrypts")
	}
	if _, ok := newSecurePayload([32]byte{4}).decrypt(enc); ok {
		t.Error("a payload decrypts under another holepunch secret")
	}
	if _, ok := sp.decrypt(enc[:40]); ok {
		t.Error("a payload with no ciphertext decrypts")
	}
}

// TestAnswerHolepunchAbortsAndForgetsUnknownIDs checks the server's answer without a network, for a handshake that
// has no puncher (its puncher is made by setupHolepuncher, which this test does not call): a probe that asks to
// punch gets an abort, since there is no puncher to punch with; a relayed probe gets the token echo; an id no
// admitted handshake holds, and a payload that does not decrypt, get no reply. A handshake with a puncher answers
// from it (answerWithPuncher), and its tests are the punch tests in punch_connect_test.go.
func TestAnswerHolepunchAbortsAndForgetsUnknownIDs(t *testing.T) {
	srv := (&DHT{}).CreateServer(ServerOptions{})
	secret := [32]byte{5}
	id := srv.reserveHolepunch()
	srv.keepHolepunch(id, secret)
	client := newSecurePayload(secret)
	peer := &net.UDPAddr{IP: net.IPv4(192, 0, 2, 7), Port: 4000}
	relay := &net.UDPAddr{IP: net.IPv4(198, 51, 100, 3), Port: 5000}

	punching, err := client.encrypt(HolepunchPayload{Punching: true})
	must(t, err)
	got, ok := client.decrypt(srv.answerHolepunch(id, punching, peer, relay))
	if !ok || got.Error != handshakeAborted {
		t.Errorf("answer to a punching probe = %+v, %v, want an abort (%d)", got, ok, handshakeAborted)
	}

	srv.mu.Lock()
	srv.relays = []RelayInfo{{RelayAddress: addressOf(relay), PeerAddress: addressOf(peer)}}
	srv.mu.Unlock()
	echo := bytes.Repeat([]byte{7}, 32)
	relayed, err := client.encrypt(HolepunchPayload{Token: echo})
	must(t, err)
	got, ok = client.decrypt(srv.answerHolepunch(id, relayed, peer, relay))
	if !ok || got.Error != 0 || !bytes.Equal(got.RemoteToken, echo) || len(got.Token) != 32 {
		t.Errorf("answer to a relayed probe = %+v, %v, want no error, the client's token echoed and a token of ours", got, ok)
	}

	if reply := srv.answerHolepunch(id+1, punching, peer, relay); reply != nil {
		t.Error("an id no admitted handshake holds got a reply")
	}
	if reply := srv.answerHolepunch(id, []byte{1, 2, 3}, peer, relay); reply != nil {
		t.Error("a payload that does not decrypt got a reply")
	}
}
