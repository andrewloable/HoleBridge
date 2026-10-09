package hyperdht

import (
	"bytes"
	"errors"
	"net/netip"
	"testing"

	"github.com/andrewloable/HoleBridge/pears/compact"
)

// The encoders refuse a host of the wrong family and a fixed-size field of the wrong length, as the
// reference does by throwing.
func TestEncodeRejectsBadFields(t *testing.T) {
	v6 := Address{Host: netip.MustParseAddr("2001:db8::1"), Port: 1}
	v4 := Address{Host: netip.MustParseAddr("192.0.2.1"), Port: 1}
	if _, err := EncodeAddress(v6); err == nil {
		t.Error("EncodeAddress accepted an IPv6 host")
	}
	if _, err := EncodeHandshake(Handshake{PeerAddress: &v6}); err == nil {
		t.Error("EncodeHandshake accepted an IPv6 peer address")
	}
	if _, err := EncodeNoisePayload(NoisePayload{Addresses6: []Address{v4}}); err == nil {
		t.Error("EncodeNoisePayload accepted an IPv4 host in addresses6")
	}
	if _, err := EncodeAnnounce(Announce{Refresh: make([]byte, 31)}); err == nil {
		t.Error("EncodeAnnounce accepted a 31-byte refresh")
	}
}

// A list longer than the reference allows fails with ErrTooLong, not with a huge allocation.
func TestDecodePeersRejectsLongList(t *testing.T) {
	var e compact.Encoder
	e.Uint(maxListLen + 1)
	if _, err := DecodePeers(e.Bytes()); !errors.Is(err, compact.ErrTooLong) {
		t.Errorf("err = %v, want ErrTooLong", err)
	}
}

// A noise payload of another version decodes with only its version set. The reference does not read
// on, so bytes after the version are not an error.
func TestDecodeNoisePayloadOtherVersion(t *testing.T) {
	m, err := DecodeNoisePayload([]byte{2, 0xff, 0xff})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if m.Version != 2 || m.Error != 0 || m.Firewall != 0 || m.Holepunch != nil || m.Addresses4 != nil ||
		m.Addresses6 != nil || m.UDX != nil || m.SecretStream != nil || m.RelayThrough != nil ||
		m.RelayAddresses != nil {
		t.Errorf("decoded %+v, want only Version 2 set", m)
	}
}

// An empty list that is present stays present: it decodes as a non-nil list and encodes to the same
// bytes, so a decode and re-encode does not drop the flag.
func TestEmptyListStaysPresent(t *testing.T) {
	in, err := EncodeHolepunchPayload(HolepunchPayload{Addresses: []Address{}})
	must(t, err)
	m, err := DecodeHolepunchPayload(in)
	must(t, err)
	if m.Addresses == nil {
		t.Fatal("present empty addresses decoded as absent")
	}
	again, err := EncodeHolepunchPayload(m)
	must(t, err)
	if !bytes.Equal(again, in) {
		t.Errorf("re-encode = %x, want %x", again, in)
	}
}

// Decoded byte fields are copies: changing the input afterwards does not change the value.
func TestDecodeDoesNotShareInput(t *testing.T) {
	in, err := EncodePeer(Peer{PublicKey: bytes.Repeat([]byte{7}, 32)})
	must(t, err)
	p, err := DecodePeer(in)
	must(t, err)
	in[0] = 0
	if p.PublicKey[0] != 7 {
		t.Error("decoded public key shares memory with the input")
	}
}
