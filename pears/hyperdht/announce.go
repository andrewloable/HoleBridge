// Ported from hyperdht 6.34.1 index.js (announce, unannounce, lookup, _requestAnnounce and
// _requestUnannounce) and lib/persistent.js (onlookup, onannounce, onunannounce, the announce signatures
// and the record store), MIT License, Copyright (c) 2018-2019 Mathias Buus, David Mark Clements &
// Contributors.
//
// Announce, unannounce and lookup, and the server side that stores the signed records. A client walks to a
// target with a LOOKUP, then sends its ANNOUNCE or UNANNOUNCE with the token of each closest node. A node
// that stores records checks each signature before it keeps or drops a record. An announce on the hash of
// its own key is a route instead (router.go), and a refresh-only ANNOUNCE renews the record that its token
// names.
package hyperdht

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/blake2b"

	"github.com/andrewloable/HoleBridge/pears/dhtrpc"
	"github.com/andrewloable/HoleBridge/pears/noise"
)

// PeerAddr is a relay address that an announce carries: the wire Address, a host and a port.
type PeerAddr = Address

// LookupResult is the answer of one node to a lookup of a target: the node's address, its token for the
// target, the peer records it holds for the target, and its bump.
type LookupResult struct {
	From  *net.UDPAddr
	Token []byte
	Peers []Peer
	Bump  uint64
}

// Limits of upstream's persistent.js. A record and a bump live 20 minutes, a node keeps 65536 records, a
// lookup reply carries 20 records, a stored announce keeps 3 relay addresses, and a bump may be at most
// 60 s ahead of our clock.
const (
	recordMaxAge      = 20 * time.Minute
	recordMaxCount    = 65536
	lookupMaxPeers    = 20
	announceMaxRelays = 3
	maxBumpDrift      = 60 * time.Second
)

// nsAnnounce and nsUnannounce are the signature namespaces of ANNOUNCE and UNANNOUNCE. As hypercore-crypto's
// namespace derives them, each is the BLAKE2b-256 of the BLAKE2b-256 of "hyperswarm/dht" and the command.
var (
	nsAnnounce   = dhtNamespace(cmdAnnounce)
	nsUnannounce = dhtNamespace(cmdUnannounce)
)

func dhtNamespace(cmd byte) [32]byte {
	base := blake2b.Sum256([]byte("hyperswarm/dht"))
	return blake2b.Sum256(append(base[:], cmd))
}

// Announce stores a signed record for kp, listing relays, on the nodes closest to target. It walks to
// target with a LOOKUP and sends the ANNOUNCE, with the token of each closest node. It returns an error
// when no node answers, as upstream's announce fails when no commit goes through.
func (d *DHT) Announce(ctx context.Context, target [32]byte, kp noise.KeyPair, relays []PeerAddr) error {
	_, err := d.announceRelays(ctx, target, kp, relays)
	return err
}

// announceRelays is Announce, and it also returns the relays the record was stored on: each node that took the
// ANNOUNCE, with its address and our address as that node sees it (upstream announcer relays). A node that
// answers with an error code did not take it.
func (d *DHT) announceRelays(ctx context.Context, target [32]byte, kp noise.KeyPair, relays []PeerAddr) ([]RelayInfo, error) {
	var mu sync.Mutex
	var stored []RelayInfo
	q := d.node.Query(ctx, dhtrpc.QueryOpts{
		Target:  target[:],
		Command: cmdLookup,
		Commit: func(ctx context.Context, r dhtrpc.Reply) error {
			resp, err := d.sendSigned(ctx, r, target, cmdAnnounce, nsAnnounce, Peer{PublicKey: kp.Public[:], RelayAddresses: relays}, kp)
			if err != nil {
				return err
			}
			if resp.Error == 0 {
				mu.Lock()
				stored = append(stored, RelayInfo{RelayAddress: addressOf(r.From), PeerAddress: Address{Host: resp.To.Host, Port: resp.To.Port}})
				mu.Unlock()
			}
			return nil
		},
	})
	err := q.Err()
	mu.Lock()
	defer mu.Unlock()
	return stored, err
}

// Unannounce removes the record that kp announced for target from the nodes that hold it. It walks to
// target with a LOOKUP and sends an UNANNOUNCE, with the token of the reply, to each closest node whose
// reply names kp. The record is also dropped from this node's own store.
func (d *DHT) Unannounce(ctx context.Context, target [32]byte, kp noise.KeyPair) error {
	d.unannounce(target, kp.Public)
	q := d.node.Query(ctx, dhtrpc.QueryOpts{
		Target:  target[:],
		Command: cmdLookup,
		Commit: func(ctx context.Context, r dhtrpc.Reply) error {
			if !holdsRecord(r.Response, kp.Public) {
				return nil
			}
			_, err := d.sendSigned(ctx, r, target, cmdUnannounce, nsUnannounce, Peer{PublicKey: kp.Public[:]}, kp)
			return err
		},
	})
	return q.Err()
}

// Lookup walks towards target and sends the answer of each node that holds records for it on the
// returned channel. The channel closes when the walk ends or ctx ends.
func (d *DHT) Lookup(ctx context.Context, target [32]byte) <-chan LookupResult {
	if d.isClosed() {
		closed := make(chan LookupResult)
		close(closed)
		return closed
	}
	q := d.node.Query(ctx, dhtrpc.QueryOpts{Target: target[:], Command: cmdLookup})
	out := make(chan LookupResult)
	go func() {
		defer close(out)
		for {
			r, ok := q.Next()
			if !ok {
				return
			}
			res, ok := lookupResult(r)
			if !ok {
				continue
			}
			select {
			case out <- res:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// sendSigned signs peer under the namespace ns, with kp, over the target, the token and the id of the
// node that replied r. It sends the result to that node as cmd, with the token, and returns the node's
// reply. An error code in the reply is not an error here: the caller decides what it means.
func (d *DHT) sendSigned(ctx context.Context, r dhtrpc.Reply, target [32]byte, cmd uint64, ns [32]byte, peer Peer, kp noise.KeyPair) (*dhtrpc.Response, error) {
	signable, err := annSignable(target[:], r.Response.Token, r.Response.ID, peer, nil, ns)
	if err != nil {
		return nil, err
	}
	ann := Announce{Peer: &peer, Signature: ed25519.Sign(ed25519.PrivateKey(kp.Secret[:]), signable)}
	value, err := EncodeAnnounce(ann)
	if err != nil {
		return nil, err
	}
	return d.node.Request(ctx, r.From, dhtrpc.Request{
		Command: cmd,
		Target:  target[:],
		Token:   r.Response.Token,
		Value:   value,
	})
}

// holdsRecord reports whether a LOOKUP reply names the key pk. As upstream's lookupAndUnannounce does, a
// reply with 20 records counts too, since the record of pk may be past the first 20.
func holdsRecord(resp *dhtrpc.Response, pk [32]byte) bool {
	raw, err := DecodeLookupRawReply(resp.Value)
	if err != nil {
		return false
	}
	if len(raw.Peers) >= lookupMaxPeers {
		return true
	}
	for _, p := range raw.Peers {
		if bytes.Equal(p.PublicKey, pk[:]) {
			return true
		}
	}
	return false
}

// lookupResult turns a LOOKUP reply into a LookupResult. A reply that carries no records is skipped.
func lookupResult(r dhtrpc.Reply) (LookupResult, bool) {
	raw, err := DecodeLookupRawReply(r.Response.Value)
	if err != nil || len(raw.Peers) == 0 {
		return LookupResult{}, false
	}
	return LookupResult{From: r.From, Token: r.Response.Token, Peers: raw.Peers, Bump: raw.Bump}, true
}

// annSignable returns the bytes that an announce signature covers, as upstream's annSignable builds them:
// the namespace, then the BLAKE2b-256 of the target, the node id, the token, the encoded peer and the
// refresh (nothing when absent).
func annSignable(target, token, id []byte, peer Peer, refresh []byte, ns [32]byte) ([]byte, error) {
	enc, err := EncodePeer(peer)
	if err != nil {
		return nil, err
	}
	h, _ := blake2b.New256(nil)
	h.Write(target)
	h.Write(id)
	h.Write(token)
	h.Write(enc)
	h.Write(refresh)
	return append(ns[:], h.Sum(nil)...), nil
}

// verifySigned checks the signature of the decoded ANNOUNCE or UNANNOUNCE m under ns, over the target and
// the token of req and our id. A message with no peer or no signature does not verify.
func verifySigned(req *dhtrpc.Request, id []byte, ns [32]byte, m Announce) bool {
	if m.Peer == nil || m.Signature == nil {
		return false
	}
	signable, err := annSignable(req.Target, req.Token, id, *m.Peer, m.Refresh, ns)
	return err == nil && ed25519.Verify(m.Peer.PublicKey, signable, m.Signature)
}

// onLookup answers a LOOKUP with the records this node holds for the target, and their bump, and with the
// closest nodes it knows, so the walk goes on. The route of the target, when there is one, counts as a
// record, as upstream's onlookup adds it. An ephemeral node does not answer. A node with no records answers
// with no value, which still carries its token.
func (d *DHT) onLookup(req *dhtrpc.Request) *dhtrpc.Response {
	if d.node.ID() == nil || req.Target == nil {
		return nil
	}
	var target [32]byte
	copy(target[:], req.Target)
	closer := d.node.Closest(req.Target)
	now := time.Now()
	peers, bump := d.records.lookup(target, now)
	if rt, ok := d.routes.get(target, now); ok && len(peers) < lookupMaxPeers {
		peers = append(peers, rt.peer)
	}
	if len(peers) == 0 {
		return &dhtrpc.Response{CloserNodes: closer}
	}
	value, err := EncodeLookupRawReply(LookupRawReply{Peers: peers, Bump: bump})
	if err != nil {
		return nil
	}
	return &dhtrpc.Response{Value: value, CloserNodes: closer}
}

// onAnnounce stores the record of an ANNOUNCE once its signature verifies, and replies with no token and no
// closer nodes, as upstream does. An ANNOUNCE with no peer is a refresh: it renews the record that its token
// names (onRefresh). The record keeps at most 3 relay addresses. An announce on the hash of its own key is a
// route, and any other is a record, where a bump that is newer than the target's and not too far ahead
// raises the target's bump. A refresh token of the announce is remembered, so a later refresh can name it.
func (d *DHT) onAnnounce(req *dhtrpc.Request) *dhtrpc.Response {
	id := d.node.ID()
	if id == nil || req.Target == nil || req.Token == nil {
		return nil
	}
	m, err := DecodeAnnounce(req.Value)
	if err != nil {
		return nil
	}
	if m.Peer == nil {
		if m.Refresh == nil {
			return nil
		}
		return d.onRefresh(req, m.Refresh)
	}
	if !verifySigned(req, id, nsAnnounce, m) {
		return nil
	}
	peer := *m.Peer
	if len(peer.RelayAddresses) > announceMaxRelays {
		peer.RelayAddresses = peer.RelayAddresses[:announceMaxRelays]
	}
	var target [32]byte
	copy(target[:], req.Target)
	now := time.Now()
	selfRoute := isHashOf(target, [32]byte(peer.PublicKey))
	if selfRoute {
		d.routes.set(target, route{peer: peer, relay: req.From, expires: now.Add(recordMaxAge)})
		d.records.remove(target, [32]byte(peer.PublicKey))
	} else {
		d.records.add(target, peer, m.Bump, now)
	}
	if m.Refresh != nil {
		d.records.setRefresh([32]byte(m.Refresh), refreshEntry{target: target, peer: peer, selfRoute: selfRoute, added: now})
	}
	return &dhtrpc.Response{NoToken: true}
}

// onRefresh renews the record that the refresh token names, as upstream's _onrefresh does: a route is set
// again from the sender, and a record is added again. The token is then remembered under the value itself,
// so the next refresh can name it. An unknown token gets no reply.
func (d *DHT) onRefresh(req *dhtrpc.Request, token []byte) *dhtrpc.Response {
	now := time.Now()
	key := blake2b.Sum256(token)
	r, ok := d.records.refresh(key, now)
	if !ok {
		return nil
	}
	if r.selfRoute {
		d.routes.set(r.target, route{peer: r.peer, relay: req.From, expires: now.Add(recordMaxAge)})
		d.records.remove(r.target, [32]byte(r.peer.PublicKey))
	} else {
		d.records.add(r.target, r.peer, 0, now)
	}
	d.records.dropRefresh(key)
	r.added = now
	d.records.setRefresh([32]byte(token), r)
	return &dhtrpc.Response{NoToken: true}
}

// onUnannounce removes the record of the key that an UNANNOUNCE names, once its signature verifies, and
// replies with no token and no closer nodes.
func (d *DHT) onUnannounce(req *dhtrpc.Request) *dhtrpc.Response {
	id := d.node.ID()
	if id == nil || req.Target == nil || req.Token == nil {
		return nil
	}
	un, err := DecodeAnnounce(req.Value)
	if err != nil || !verifySigned(req, id, nsUnannounce, un) {
		return nil
	}
	var target [32]byte
	copy(target[:], req.Target)
	d.unannounce(target, [32]byte(un.Peer.PublicKey))
	return &dhtrpc.Response{NoToken: true}
}

// unannounce removes the record of pk for target, and the route of pk when target is the hash of pk, as
// upstream's unannounce does.
func (d *DHT) unannounce(target [32]byte, pk [32]byte) {
	if isHashOf(target, pk) {
		d.routes.delete(target)
	}
	d.records.remove(target, pk)
}

// recordStore is the set of records that a node keeps: for each target, the record of each key that
// announced to it, and the target's bump. A record expires maxAge after it was stored, and the store holds
// at most recordMaxCount records.
type recordStore struct {
	mu        sync.Mutex
	targets   map[[32]byte]*recordSet
	count     int // records held, over all targets
	refreshes map[[32]byte]refreshEntry
}

// refreshEntry is an ANNOUNCE's refresh token and what a refresh with it renews: the target, the peer, and
// whether the target is the peer's own hash (a route). It lapses recordMaxAge after it was stored.
type refreshEntry struct {
	target    [32]byte
	peer      Peer
	selfRoute bool
	added     time.Time
}

// setRefresh stores r under key. When the cache is full, the lapsed entries are dropped first, and a new
// key is refused if it is still full.
func (s *recordStore) setRefresh(key [32]byte, r refreshEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.refreshes[key]; !ok && len(s.refreshes) >= recordMaxCount {
		now := time.Now()
		for k, e := range s.refreshes {
			if now.Sub(e.added) > recordMaxAge {
				delete(s.refreshes, k)
			}
		}
		if len(s.refreshes) >= recordMaxCount {
			return
		}
	}
	s.refreshes[key] = r
}

// refresh returns the entry stored under key, unless it has lapsed at now.
func (s *recordStore) refresh(key [32]byte, now time.Time) (refreshEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.refreshes[key]
	if !ok || now.Sub(r.added) > recordMaxAge {
		return refreshEntry{}, false
	}
	return r, true
}

// dropRefresh drops the entry stored under key.
func (s *recordStore) dropRefresh(key [32]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.refreshes, key)
}

// recordSet is what the store holds for one target.
type recordSet struct {
	peers  map[[32]byte]storedPeer // by public key
	bump   uint64
	bumpAt time.Time // when bump was set; it expires like a record
}

// storedPeer is a record and the time it was stored.
type storedPeer struct {
	peer  Peer
	added time.Time
}

func newRecordStore() *recordStore {
	return &recordStore{targets: make(map[[32]byte]*recordSet), refreshes: make(map[[32]byte]refreshEntry)}
}

// add stores peer as the record of its key for target. A new key is refused when the store is full, after
// the expired records are dropped. The bump of target rises to bump when bump is newer than the current one
// and at most maxBumpDrift ahead of now.
func (s *recordStore) add(target [32]byte, peer Peer, bump uint64, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := s.targets[target]
	if set == nil {
		set = &recordSet{peers: make(map[[32]byte]storedPeer)}
	}
	pk := [32]byte(peer.PublicKey)
	if _, ok := set.peers[pk]; !ok {
		if s.count >= recordMaxCount {
			s.expire(now)
		}
		if s.count >= recordMaxCount {
			return
		}
		s.count++
	}
	s.targets[target] = set
	set.peers[pk] = storedPeer{peer: peer, added: now}
	if bump > set.currentBump(now) && bump <= uint64(now.Add(maxBumpDrift).UnixMilli()) {
		set.bump = bump
		set.bumpAt = now
	}
}

// lookup returns up to lookupMaxPeers unexpired records of target, and the target's bump.
func (s *recordStore) lookup(target [32]byte, now time.Time) ([]Peer, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := s.targets[target]
	if set == nil {
		return nil, 0
	}
	s.expireSet(set, now)
	var peers []Peer
	for _, sp := range set.peers {
		if len(peers) == lookupMaxPeers {
			break
		}
		peers = append(peers, sp.peer)
	}
	return peers, set.currentBump(now)
}

// remove drops the record of pk for target.
func (s *recordStore) remove(target [32]byte, pk [32]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := s.targets[target]
	if set == nil {
		return
	}
	if _, ok := set.peers[pk]; ok {
		delete(set.peers, pk)
		s.count--
	}
}

// expire drops the expired records and bumps of every target, and the targets that are left empty.
func (s *recordStore) expire(now time.Time) {
	for target, set := range s.targets {
		s.expireSet(set, now)
		if len(set.peers) == 0 && set.currentBump(now) == 0 {
			delete(s.targets, target)
		}
	}
}

// expireSet drops the expired records and the expired bump of one target.
func (s *recordStore) expireSet(set *recordSet, now time.Time) {
	for pk, sp := range set.peers {
		if now.Sub(sp.added) > recordMaxAge {
			delete(set.peers, pk)
			s.count--
		}
	}
	if now.Sub(set.bumpAt) > recordMaxAge {
		set.bump = 0
	}
}

// currentBump returns the bump of the set, or 0 once it has expired.
func (set *recordSet) currentBump(now time.Time) uint64 {
	if now.Sub(set.bumpAt) > recordMaxAge {
		return 0
	}
	return set.bump
}
