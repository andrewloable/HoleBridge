// Package table is the Kademlia routing table of pears-go's dht-rpc. Nodes are kept in buckets by
// the number of leading id bits they share with our own id, at most k per bucket.
//
// Ported from kademlia-routing-table index.js (1.0.6), MIT License, Copyright (c) 2019 Mathias Buus.
// Two rules differ from upstream: Add refuses our own id, and Closest sorts every stored node by
// XOR distance rather than walking the rows in order.
package table

import (
	"bytes"
	"math/bits"
	"math/rand/v2"
	"slices"
)

// Node is one entry in the table. ID is the node's 32-byte id.
type Node struct {
	ID   [32]byte
	Host string
	Port int
}

// Table is a Kademlia routing table for one local id.
type Table struct {
	self    [32]byte
	k       int
	buckets [256][]Node
}

// New returns an empty table for our own id, with k nodes per bucket (dht-rpc uses 20).
func New(id [32]byte, k int) *Table {
	return &Table{self: id, k: k}
}

// bucket returns the index of the bucket for id: the number of leading bits it shares with our id.
func (t *Table) bucket(id [32]byte) int {
	for i := range id {
		if x := id[i] ^ t.self[i]; x != 0 {
			return i*8 + bits.LeadingZeros8(x)
		}
	}
	return len(t.buckets) - 1 // our own id; upstream puts it in the last row too
}

// find returns the bucket and position of the node with the given id, and false if there is none.
func (t *Table) find(id [32]byte) (b, i int, ok bool) {
	b = t.bucket(id)
	i = slices.IndexFunc(t.buckets[b], func(n Node) bool { return n.ID == id })
	return b, i, i >= 0
}

// Add stores n and reports whether it was stored. It refuses our own id and nodes whose bucket is
// full.
func (t *Table) Add(n Node) bool {
	if n.ID == t.self {
		return false
	}
	b, i, ok := t.find(n.ID)
	if ok {
		t.buckets[b][i] = n
		return true
	}
	if len(t.buckets[b]) >= t.k {
		return false
	}
	t.buckets[b] = append(t.buckets[b], n)
	return true
}

// Remove deletes the node with the given id and reports whether one was stored.
func (t *Table) Remove(id [32]byte) bool {
	b, i, ok := t.find(id)
	if !ok {
		return false
	}
	t.buckets[b] = slices.Delete(t.buckets[b], i, i+1)
	return true
}

// Get returns the stored node with the given id, and false if there is none.
func (t *Table) Get(id [32]byte) (Node, bool) {
	b, i, ok := t.find(id)
	if !ok {
		return Node{}, false
	}
	return t.buckets[b][i], true
}

// all returns every stored node, in no particular order.
func (t *Table) all() []Node {
	out := make([]Node, 0, t.Len())
	for _, b := range t.buckets {
		out = append(out, b...)
	}
	return out
}

// Closest returns at most n stored nodes, nearest first by XOR distance to target.
func (t *Table) Closest(target [32]byte, n int) []Node {
	nodes := t.all()
	slices.SortFunc(nodes, func(a, b Node) int {
		da, db := xor(a.ID, target), xor(b.ID, target)
		return bytes.Compare(da[:], db[:])
	})
	return nodes[:max(0, min(n, len(nodes)))]
}

// Len returns the number of stored nodes.
func (t *Table) Len() int {
	n := 0
	for _, b := range t.buckets {
		n += len(b)
	}
	return n
}

// Random returns a random stored node, and false if the table is empty.
func (t *Table) Random() (Node, bool) {
	nodes := t.all()
	if len(nodes) == 0 {
		return Node{}, false
	}
	return nodes[rand.IntN(len(nodes))], true
}

// xor returns a XOR b.
func xor(a, b [32]byte) (d [32]byte) {
	for i := range d {
		d[i] = a[i] ^ b[i]
	}
	return d
}
