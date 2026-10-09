// Package hyperdht is the pears-go port of HyperDHT, the Holepunch DHT that runs on dht-rpc. This
// file holds its wire messages: the Go structs and the Encode and Decode functions for each
// encoding of lib/messages.js.
//
// Ported from hyperdht 6.34.1 lib/messages.js, MIT License, Copyright (c) 2018-2019 Mathias Buus,
// David Mark Clements & Contributors. Binary fields are []byte; keys and tokens are 32 bytes and
// signatures 64 bytes. An absent optional field is a nil pointer or a nil slice.
package hyperdht

import (
	"bytes"
	"errors"
	"fmt"
	"net/netip"

	"github.com/andrewloable/HoleBridge/pears/compact"
)

// Address is a peer address: a host and a port. The address encoding is IPv4 only; IPv6 hosts
// appear in the addresses6 list of a NoisePayload.
type Address struct {
	Host netip.Addr
	Port uint16
}

// Handshake is the handshake message. PeerAddress and RelayAddress are nil when absent.
type Handshake struct {
	Mode         uint64
	Noise        []byte
	PeerAddress  *Address
	RelayAddress *Address
}

// Holepunch is the holepunch message. PeerAddress is nil when absent.
type Holepunch struct {
	Mode        uint64
	ID          uint64
	Payload     []byte
	PeerAddress *Address
}

// HolepunchPayload is the payload of a holepunch. Addresses is nil when absent. Token, RemoteToken
// and RemoteAddress are nil when absent.
type HolepunchPayload struct {
	Error         uint64
	Firewall      uint64
	Round         uint64
	Connected     bool
	Punching      bool
	Addresses     []Address
	RemoteAddress *Address
	Token         []byte
	RemoteToken   []byte
}

// Peer is a lookup record: a 32-byte public key and the relay addresses of that peer.
type Peer struct {
	PublicKey      []byte
	RelayAddresses []Address
}

// LookupRawReply is the reply to a lookup: the peers it found and the bump.
type LookupRawReply struct {
	Peers []Peer
	Bump  uint64
}

// Announce is the announce message. Peer, Refresh and Signature are nil when absent.
type Announce struct {
	Peer      *Peer
	Refresh   []byte
	Signature []byte
	Bump      uint64
}

// RelayInfo is one relay of a HolepunchInfo.
type RelayInfo struct {
	RelayAddress Address
	PeerAddress  Address
}

// HolepunchInfo is the holepunch part of a NoisePayload.
type HolepunchInfo struct {
	ID     uint64
	Relays []RelayInfo
}

// UDXInfo is the UDX part of a NoisePayload.
type UDXInfo struct {
	Version        uint64
	ReusableSocket bool
	ID             uint64
	Seq            uint64
}

// SecretStreamInfo is the secret stream part of a NoisePayload.
type SecretStreamInfo struct {
	Version uint64
}

// RelayThroughInfo is the relay-through part of a NoisePayload.
type RelayThroughInfo struct {
	Version   uint64
	PublicKey []byte
	Token     []byte
}

// NoisePayload is the payload carried inside the Noise handshake. Each optional part is nil, or
// empty for a list, when absent.
type NoisePayload struct {
	Version        uint64
	Error          uint64
	Firewall       uint64
	Holepunch      *HolepunchInfo
	Addresses4     []Address
	Addresses6     []Address
	UDX            *UDXInfo
	SecretStream   *SecretStreamInfo
	RelayThrough   *RelayThroughInfo
	RelayAddresses []Address
}

// maxListLen is the most entries a list may hold. The reference refuses a longer list.
const maxListLen = 0x100000

// Errors for an address host of the wrong family.
var (
	errIPv4 = errors.New("hyperdht: address host is not IPv4")
	errIPv6 = errors.New("hyperdht: address host is not IPv6 without a zone")
)

// writer encodes compact-encoding fields in order. It keeps the first error a field reports, so an
// encoder reads as a list of writes and checks the error once, in result.
type writer struct {
	compact.Encoder
	err error
}

func (w *writer) fail(err error) {
	if w.err == nil {
		w.err = err
	}
}

// result returns the bytes written, or the first error.
func (w *writer) result() ([]byte, error) {
	if w.err != nil {
		return nil, w.err
	}
	return w.Bytes(), nil
}

// bit returns v when set is true, and 0 otherwise: one flag of a flags field.
func bit(set bool, v uint64) uint64 {
	if set {
		return v
	}
	return 0
}

// addr4 writes an IPv4 address: 4 bytes, then the port as uint16 LE.
func (w *writer) addr4(a Address) {
	if !a.Host.Is4() {
		w.fail(errIPv4)
		return
	}
	ip := a.Host.As4()
	w.Raw(ip[:])
	w.Uint16(a.Port)
}

// addr6 writes an IPv6 address: 16 bytes, then the port as uint16 LE. A zone cannot be written.
func (w *writer) addr6(a Address) {
	if !a.Host.Is6() || a.Host.Zone() != "" {
		w.fail(errIPv6)
		return
	}
	ip := a.Host.As16()
	w.Raw(ip[:])
	w.Uint16(a.Port)
}

// addrs4 writes a list of IPv4 addresses: a count, then each address.
func (w *writer) addrs4(as []Address) {
	w.Len(len(as))
	for _, a := range as {
		w.addr4(a)
	}
}

// addrs6 writes a list of IPv6 addresses: a count, then each address.
func (w *writer) addrs6(as []Address) {
	w.Len(len(as))
	for _, a := range as {
		w.addr6(a)
	}
}

// fixed writes b, a field the schema fixes at n bytes.
func (w *writer) fixed(b []byte, n int) {
	if len(b) != n {
		w.fail(fmt.Errorf("hyperdht: field is %d bytes, want %d", len(b), n))
		return
	}
	w.Raw(b)
}

// peer writes a lookup record: the public key, then its relay addresses.
func (w *writer) peer(p Peer) {
	w.fixed(p.PublicKey, 32)
	w.addrs4(p.RelayAddresses)
}

// peers writes a list of lookup records: a count, then each record.
func (w *writer) peers(ps []Peer) {
	w.Len(len(ps))
	for _, p := range ps {
		w.peer(p)
	}
}

// holepunchInfo writes the id, then the relays, each as its relay address and then its peer address.
func (w *writer) holepunchInfo(h HolepunchInfo) {
	w.Uint(h.ID)
	w.Len(len(h.Relays))
	for _, r := range h.Relays {
		w.addr4(r.RelayAddress)
		w.addr4(r.PeerAddress)
	}
}

// udxInfo writes version 1, as the reference does, whatever Version holds.
func (w *writer) udxInfo(u UDXInfo) {
	w.Uint(1) // version
	w.Uint(bit(u.ReusableSocket, 1))
	w.Uint(u.ID)
	w.Uint(u.Seq)
}

// relayThroughInfo writes version 1 and flags 0, as the reference does, whatever Version holds.
func (w *writer) relayThroughInfo(t RelayThroughInfo) {
	w.Uint(1) // version
	w.Uint(0) // flags
	w.fixed(t.PublicKey, 32)
	w.fixed(t.Token, 32)
}

// EncodeAddress encodes an IPv4 address as the ipv4Address layout: 4 bytes, then a little-endian
// port. It returns an error for a host that is not IPv4.
func EncodeAddress(a Address) ([]byte, error) {
	var w writer
	w.addr4(a)
	return w.result()
}

// DecodeAddress decodes an IPv4 address from b.
func DecodeAddress(b []byte) (Address, error) {
	r := newReader(b)
	a := r.addr4()
	return a, r.err
}

// EncodeHandshake encodes a handshake message.
func EncodeHandshake(m Handshake) ([]byte, error) {
	var w writer
	w.Uint(bit(m.PeerAddress != nil, 1) | bit(m.RelayAddress != nil, 2))
	w.Uint(m.Mode)
	w.Buffer(m.Noise)
	if m.PeerAddress != nil {
		w.addr4(*m.PeerAddress)
	}
	if m.RelayAddress != nil {
		w.addr4(*m.RelayAddress)
	}
	return w.result()
}

// DecodeHandshake decodes a handshake message from b.
func DecodeHandshake(b []byte) (Handshake, error) {
	r := newReader(b)
	flags := r.uint()
	var m Handshake
	m.Mode = r.uint()
	m.Noise = r.buf()
	if flags&1 != 0 {
		m.PeerAddress = ptr(r.addr4())
	}
	if flags&2 != 0 {
		m.RelayAddress = ptr(r.addr4())
	}
	return m, r.err
}

// EncodeHolepunch encodes a holepunch message.
func EncodeHolepunch(m Holepunch) ([]byte, error) {
	var w writer
	w.Uint(bit(m.PeerAddress != nil, 1))
	w.Uint(m.Mode)
	w.Uint(m.ID)
	w.Buffer(m.Payload)
	if m.PeerAddress != nil {
		w.addr4(*m.PeerAddress)
	}
	return w.result()
}

// DecodeHolepunch decodes a holepunch message from b.
func DecodeHolepunch(b []byte) (Holepunch, error) {
	r := newReader(b)
	flags := r.uint()
	var m Holepunch
	m.Mode = r.uint()
	m.ID = r.uint()
	m.Payload = r.buf()
	if flags&1 != 0 {
		m.PeerAddress = ptr(r.addr4())
	}
	return m, r.err
}

// EncodeHolepunchPayload encodes a holepunch payload. A list or a byte field is written when it is
// not nil, so an empty Addresses list is still written.
func EncodeHolepunchPayload(m HolepunchPayload) ([]byte, error) {
	var w writer
	w.Uint(bit(m.Connected, 1) | bit(m.Punching, 2) | bit(m.Addresses != nil, 4) |
		bit(m.RemoteAddress != nil, 8) | bit(m.Token != nil, 16) | bit(m.RemoteToken != nil, 32))
	w.Uint(m.Error)
	w.Uint(m.Firewall)
	w.Uint(m.Round)
	if m.Addresses != nil {
		w.addrs4(m.Addresses)
	}
	if m.RemoteAddress != nil {
		w.addr4(*m.RemoteAddress)
	}
	if m.Token != nil {
		w.fixed(m.Token, 32)
	}
	if m.RemoteToken != nil {
		w.fixed(m.RemoteToken, 32)
	}
	return w.result()
}

// DecodeHolepunchPayload decodes a holepunch payload from b.
func DecodeHolepunchPayload(b []byte) (HolepunchPayload, error) {
	r := newReader(b)
	flags := r.uint()
	var m HolepunchPayload
	m.Error = r.uint()
	m.Firewall = r.uint()
	m.Round = r.uint()
	m.Connected = flags&1 != 0
	m.Punching = flags&2 != 0
	if flags&4 != 0 {
		m.Addresses = list(r, r.addr4)
	}
	if flags&8 != 0 {
		m.RemoteAddress = ptr(r.addr4())
	}
	if flags&16 != 0 {
		m.Token = r.fixed(32)
	}
	if flags&32 != 0 {
		m.RemoteToken = r.fixed(32)
	}
	return m, r.err
}

// EncodeNoisePayload encodes a noise payload. It writes version 1, the only version the reference
// encodes. A list of addresses is flagged when it is not empty, and the relay addresses when they
// are not nil.
func EncodeNoisePayload(m NoisePayload) ([]byte, error) {
	var w writer
	w.Uint(1) // version
	w.Uint(bit(m.Holepunch != nil, 1) | bit(len(m.Addresses4) > 0, 2) | bit(len(m.Addresses6) > 0, 4) |
		bit(m.UDX != nil, 8) | bit(m.SecretStream != nil, 16) | bit(m.RelayThrough != nil, 32) |
		bit(m.RelayAddresses != nil, 64))
	w.Uint(m.Error)
	w.Uint(m.Firewall)
	if m.Holepunch != nil {
		w.holepunchInfo(*m.Holepunch)
	}
	if len(m.Addresses4) > 0 {
		w.addrs4(m.Addresses4)
	}
	if len(m.Addresses6) > 0 {
		w.addrs6(m.Addresses6)
	}
	if m.UDX != nil {
		w.udxInfo(*m.UDX)
	}
	if m.SecretStream != nil {
		w.Uint(1) // version
	}
	if m.RelayThrough != nil {
		w.relayThroughInfo(*m.RelayThrough)
	}
	if m.RelayAddresses != nil {
		w.addrs4(m.RelayAddresses)
	}
	return w.result()
}

// DecodeNoisePayload decodes a noise payload from b. A version other than 1 is returned with only
// Version set: the reference does not read the rest of b.
func DecodeNoisePayload(b []byte) (NoisePayload, error) {
	r := newReader(b)
	var m NoisePayload
	m.Version = r.uint()
	if r.err != nil || m.Version != 1 {
		return m, r.err
	}
	flags := r.uint()
	m.Error = r.uint()
	m.Firewall = r.uint()
	if flags&1 != 0 {
		m.Holepunch = ptr(r.holepunchInfo())
	}
	if flags&2 != 0 {
		m.Addresses4 = list(r, r.addr4)
	}
	if flags&4 != 0 {
		m.Addresses6 = list(r, r.addr6)
	}
	if flags&8 != 0 {
		m.UDX = ptr(r.udxInfo())
	}
	if flags&16 != 0 {
		m.SecretStream = ptr(r.secretStreamInfo())
	}
	if flags&32 != 0 {
		m.RelayThrough = ptr(r.relayThroughInfo())
	}
	if flags&64 != 0 {
		m.RelayAddresses = list(r, r.addr4)
	}
	return m, r.err
}

// EncodePeer encodes a lookup record.
func EncodePeer(p Peer) ([]byte, error) {
	var w writer
	w.peer(p)
	return w.result()
}

// DecodePeer decodes a lookup record from b.
func DecodePeer(b []byte) (Peer, error) {
	r := newReader(b)
	p := r.peer()
	return p, r.err
}

// EncodePeers encodes a list of lookup records: a count, then each record.
func EncodePeers(ps []Peer) ([]byte, error) {
	var w writer
	w.peers(ps)
	return w.result()
}

// DecodePeers decodes a list of lookup records from b.
func DecodePeers(b []byte) ([]Peer, error) {
	r := newReader(b)
	ps := list(r, r.peer)
	return ps, r.err
}

// EncodeLookupRawReply encodes a lookup reply: its peers, then its bump.
func EncodeLookupRawReply(m LookupRawReply) ([]byte, error) {
	var w writer
	w.peers(m.Peers)
	w.Uint(m.Bump)
	return w.result()
}

// DecodeLookupRawReply decodes a lookup reply from b. A reply with no bump after its peers decodes
// with Bump 0, as the reference does.
func DecodeLookupRawReply(b []byte) (LookupRawReply, error) {
	r := newReader(b)
	var m LookupRawReply
	m.Peers = list(r, r.peer)
	// Done returns ErrTrailing while bytes remain, so a non-nil result means a bump follows.
	if r.d.Done() != nil {
		m.Bump = r.uint()
	}
	return m, r.err
}

// EncodeAnnounce encodes an announce message.
func EncodeAnnounce(m Announce) ([]byte, error) {
	var w writer
	w.Uint(bit(m.Peer != nil, 1) | bit(m.Refresh != nil, 2) | bit(m.Signature != nil, 4) | bit(m.Bump != 0, 8))
	if m.Peer != nil {
		w.peer(*m.Peer)
	}
	if m.Refresh != nil {
		w.fixed(m.Refresh, 32)
	}
	if m.Signature != nil {
		w.fixed(m.Signature, 64)
	}
	if m.Bump != 0 {
		w.Uint(m.Bump)
	}
	return w.result()
}

// DecodeAnnounce decodes an announce message from b.
func DecodeAnnounce(b []byte) (Announce, error) {
	r := newReader(b)
	flags := r.uint()
	var m Announce
	if flags&1 != 0 {
		m.Peer = ptr(r.peer())
	}
	if flags&2 != 0 {
		m.Refresh = r.fixed(32)
	}
	if flags&4 != 0 {
		m.Signature = r.fixed(64)
	}
	if flags&8 != 0 {
		m.Bump = r.uint()
	}
	return m, r.err
}

// reader decodes compact-encoding fields in order. It keeps the first error: once a read fails,
// every later read returns a zero value, so a decoder reads as a list of fields and checks the
// error once, at its return.
type reader struct {
	d   *compact.Decoder
	err error
}

// newReader reads b. The values it returns do not share memory with b, as in pears/dhtrpc.
func newReader(b []byte) *reader {
	return &reader{d: compact.NewDecoder(bytes.Clone(b))}
}

// read returns the next field, which f reads, or the zero value once an earlier read has failed.
func read[T any](r *reader, f func() (T, error)) T {
	var v T
	if r.err == nil {
		v, r.err = f()
	}
	return v
}

// list reads a count, then that many items with one. The list is never nil, so a present but empty
// list stays distinct from an absent one.
func list[T any](r *reader, one func() T) []T {
	n := read(r, func() (int, error) { return r.d.Len(maxListLen) })
	out := []T{}
	for i := 0; i < n && r.err == nil; i++ {
		out = append(out, one())
	}
	return out
}

// ptr returns a pointer to v, for an optional field.
func ptr[T any](v T) *T { return &v }

func (r *reader) uint() uint64 { return read(r, r.d.Uint) }

func (r *reader) buf() []byte { return read(r, r.d.Buffer) }

func (r *reader) fixed(n int) []byte {
	return read(r, func() ([]byte, error) { return r.d.Fixed(n) })
}

// addr4 reads an IPv4 address: 4 bytes, then the port as uint16 LE.
func (r *reader) addr4() Address {
	b := r.fixed(4)
	port := read(r, r.d.Uint16)
	if r.err != nil {
		return Address{}
	}
	return Address{Host: netip.AddrFrom4([4]byte(b)), Port: port}
}

// addr6 reads an IPv6 address: 16 bytes, then the port as uint16 LE.
func (r *reader) addr6() Address {
	b := r.fixed(16)
	port := read(r, r.d.Uint16)
	if r.err != nil {
		return Address{}
	}
	return Address{Host: netip.AddrFrom16([16]byte(b)), Port: port}
}

// peer reads a lookup record: the public key, then its relay addresses.
func (r *reader) peer() Peer {
	key := r.fixed(32)
	return Peer{PublicKey: key, RelayAddresses: list(r, r.addr4)}
}

// holepunchInfo reads the id, then the relays, as holepunchInfo writes them.
func (r *reader) holepunchInfo() HolepunchInfo {
	id := r.uint()
	return HolepunchInfo{ID: id, Relays: list(r, r.relayInfo)}
}

// relayInfo reads one relay: its relay address, then its peer address.
func (r *reader) relayInfo() RelayInfo {
	return RelayInfo{RelayAddress: r.addr4(), PeerAddress: r.addr4()}
}

// udxInfo reads the UDX part: version, features (bit 0 is reusable socket), id and seq.
func (r *reader) udxInfo() UDXInfo {
	var u UDXInfo
	u.Version = r.uint()
	features := r.uint()
	u.ReusableSocket = features&1 != 0
	u.ID = r.uint()
	u.Seq = r.uint()
	return u
}

// secretStreamInfo reads the secret stream part: its version.
func (r *reader) secretStreamInfo() SecretStreamInfo {
	return SecretStreamInfo{Version: r.uint()}
}

// relayThroughInfo reads the relay-through part: version, flags (ignored), public key and token.
func (r *reader) relayThroughInfo() RelayThroughInfo {
	var t RelayThroughInfo
	t.Version = r.uint()
	r.uint()
	t.PublicKey = r.fixed(32)
	t.Token = r.fixed(32)
	return t
}
