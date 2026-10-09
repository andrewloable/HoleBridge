// Ported from hyperdht 6.34.1 index.js (the constructor and the default key pair), MIT License, Copyright
// (c) 2018-2019 Mathias Buus, David Mark Clements & Contributors.
//
// The HyperDHT node: a dht-rpc node (pears/dhtrpc) that also stores the signed announce records of the
// keys that announce to it. Announce, unannounce and lookup are in announce.go. The node's UDP port is
// shared with the UDX streams of the connections (newStream), as upstream shares its socket.
package hyperdht

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/pears/dhtrpc"
	"github.com/andrewloable/HoleBridge/pears/noise"
	"github.com/andrewloable/HoleBridge/pears/udx"
)

// Config sets up a HyperDHT node. Bootstrap lists the host:port of the bootstrap nodes. DefaultKeyPair
// is the node's own key pair, or nil for a random one. Port is the UDP port to listen on. Ephemeral keeps the
// node ephemeral for good, as upstream's ephemeral option does: it has no id, so it answers no LOOKUP,
// FIND_PEER or ANNOUNCE, and the routes of its servers are held by the other nodes. Its handshakes still
// reach its servers. The zero value asks for a persistent node, which becomes persistent once NAT sampling
// allows.
type Config struct {
	Bootstrap      []string
	DefaultKeyPair *noise.KeyPair
	Port           int
	Ephemeral      bool
}

// DHT is a HyperDHT node: a dht-rpc node (pears/dhtrpc) that also stores the signed announce records of
// the keys that announce to it, and the routes of the keys that announce on the hash of their own public
// key. Its announce, unannounce and lookup methods are in announce.go, and its server in server.go.
type DHT struct {
	node    *dhtrpc.Node
	keyPair noise.KeyPair
	records *recordStore
	routes  *routeTable
	decide  chan struct{} // a slot for each handshake decision made off the read loop (router.go)

	mu      sync.Mutex           // guards closed, servers and keys
	closed  bool                 // set by Close
	servers map[*Server]struct{} // the servers made by CreateServer that are not closed yet
	keys    map[[32]byte]*Server // the server listening on each key pair of this DHT (claimKey)

	// forceRelay is a test seam, set only by tests of this package: a connect or a server claims its streams
	// only through a relay pairing, never on the direct path. Loopback always has a direct path, so this is
	// how the relay tests make the bytes cross the relay. Relay dials of the node are not forced.
	forceRelay bool

	// forcePunch is a test seam, set only by tests of this package, like forceRelay: a connect or a server claims
	// its streams only through a hole punch, never on the direct path or through a relay. Loopback always has a
	// direct path, so this is how the punch tests make the bytes cross a punched path.
	forcePunch bool

	// hideAddress is a test seam, set only by tests of this package: the node never names its own address, so a server on
	// it answers with a holepunch, as a server that does not know its address does (remoteAddress).
	hideAddress bool

	punch   punchHub    // the live punch handles, which the DHT's holepunch datagrams go to (punch_connect.go)
	randoms *randomGate // the limit on randomized punches of this DHT (gate)
}

// gate returns the DHT's gate on randomized punches (dht._randomPunchLimit and _randomPunchInterval).
func (d *DHT) gate() *randomGate {
	return d.randoms
}

// ForceRelayForTest is the relay test seam (dht.forceRelay) for packages outside this one. After the call, every
// connect and server of d claims its streams only through a relay pairing, never on the direct path. Call it
// before d connects or listens. The testing.TB argument marks it as test-only, as NewTestnet does.
func ForceRelayForTest(t testing.TB, d *DHT) {
	t.Helper()
	d.forceRelay = true
}

// ErrDHTClosed is the error of a Connect on a DHT that Close has stopped.
var ErrDHTClosed = errors.New("hyperdht: DHT closed")

// Close stops the node. Every server made on it is closed first, which unannounces its key pair on a best
// effort basis and stops its reannounce loop. Then the node's dht-rpc socket is closed, so its UDP port is free
// again. Connect fails with ErrDHTClosed and Lookup returns a channel that is already closed. A second Close
// returns nil.
func (d *DHT) Close() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	servers := make([]*Server, 0, len(d.servers))
	for s := range d.servers {
		servers = append(servers, s)
	}
	d.mu.Unlock()

	for _, s := range servers {
		s.Close()
	}
	return d.node.Close()
}

// isClosed reports whether Close has been called.
func (d *DHT) isClosed() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closed
}

// forgetServer drops s from the servers that Close closes, once s is closed.
func (d *DHT) forgetServer(s *Server) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.servers, s)
}

// claimKey records s as the server listening on the key pair pk of this DHT. It returns false when another server
// on this DHT listens on pk already. The check and the record are one step, so two Listen calls cannot both win.
func (d *DHT) claimKey(pk [32]byte, s *Server) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if owner, ok := d.keys[pk]; ok && owner != s {
		return false
	}
	if d.keys == nil {
		d.keys = make(map[[32]byte]*Server)
	}
	d.keys[pk] = s
	return true
}

// releaseKey drops the record that s listens on pk, when s is the server that holds it.
func (d *DHT) releaseKey(pk [32]byte, s *Server) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.keys[pk] == s {
		delete(d.keys, pk)
	}
}

// Command numbers of hyperdht's COMMANDS (lib/constants.js) that this node answers or sends.
const (
	cmdLookup     = 3 // LOOKUP: the records a node holds for a target, with a token
	cmdAnnounce   = 4 // ANNOUNCE: a signed peer record for a target, sent with the token of a LOOKUP
	cmdUnannounce = 5 // UNANNOUNCE: removes a key's record for a target, sent with the token of a LOOKUP
)

// New starts a HyperDHT node. Unless cfg.Ephemeral is set, the node asks for persistence, so it stores records
// once NAT sampling finds it reachable, as upstream's default node does.
func New(cfg Config) (*DHT, error) {
	kp, err := keyPair(cfg.DefaultKeyPair)
	if err != nil {
		return nil, err
	}
	ephemeral := cfg.Ephemeral
	n, err := dhtrpc.New(dhtrpc.Config{Bootstrap: cfg.Bootstrap, Port: cfg.Port, Ephemeral: &ephemeral})
	if err != nil {
		return nil, err
	}
	return newDHT(n, kp), nil
}

// keyPair returns the key pair kp points to, or a random one when kp is nil.
func keyPair(kp *noise.KeyPair) (noise.KeyPair, error) {
	if kp != nil {
		return *kp, nil
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return noise.KeyPair{}, err
	}
	var out noise.KeyPair
	copy(out.Public[:], pub)
	copy(out.Secret[:], priv)
	return out, nil
}

// newDHT wraps the dht-rpc node n and answers the lookup, announce, unannounce, find-peer and handshake
// commands on it.
func newDHT(n *dhtrpc.Node, kp noise.KeyPair) *DHT {
	d := &DHT{
		node:    n,
		keyPair: kp,
		records: newRecordStore(),
		routes:  newRouteTable(),
		decide:  make(chan struct{}, maxHandshakes),
		randoms: &randomGate{limit: 1, interval: 20 * time.Second},
	}
	n.Handle(cmdLookup, d.onLookup)
	n.Handle(cmdAnnounce, d.onAnnounce)
	n.Handle(cmdUnannounce, d.onUnannounce)
	n.Handle(cmdFindPeer, d.onFindPeer)
	n.Handle(cmdPeerHandshake, d.onPeerHandshake)
	n.Handle(cmdPeerHolepunch, d.onPeerHolepunch)
	n.OnPunch(d.punch.deliver)
	return d
}

// addr returns the UDP address that d listens on.
func (d *DHT) addr() (*net.UDPAddr, error) {
	return d.node.Addr()
}

// remoteAddress returns the address this node knows as its own, as upstream's dht.remoteAddress() does: the host and
// port that its peers report for it, when that port is the one it listens on and the node is not firewalled. It is nil
// until the NAT samples name one.
func (d *DHT) remoteAddress() *net.UDPAddr {
	if d.hideAddress {
		return nil
	}
	nat := d.node.NAT()
	bound, err := d.addr()
	if err != nil || bound == nil || nat.Host == "" || nat.Port == 0 || nat.Firewalled || nat.Port != bound.Port {
		return nil
	}
	ip := net.ParseIP(nat.Host).To4()
	if ip == nil {
		return nil
	}
	return &net.UDPAddr{IP: ip, Port: nat.Port}
}

// newStream returns a UDX stream on the node's socket, with a random local id that is not zero, since zero
// means no stream in a handshake reply. The stream is not connected.
func (d *DHT) newStream() *udx.Stream {
	var b [4]byte
	for {
		rand.Read(b[:])
		if id := binary.LittleEndian.Uint32(b[:]); id != 0 {
			return d.node.Socket().NewStream(id)
		}
	}
}
