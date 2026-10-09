package table

import (
	"bytes"
	"math/rand/v2"
	"slices"
	"testing"
)

// withFirst returns an id whose first byte is b and whose other bytes are zero.
func withFirst(b byte) [32]byte {
	var id [32]byte
	id[0] = b
	return id
}

// randomID returns a random id drawn from r.
func randomID(r *rand.Rand) [32]byte {
	var id [32]byte
	for i := range id {
		id[i] = byte(r.UintN(256))
	}
	return id
}

// xorDistance returns a XOR b. Compared bytewise, it orders ids by distance from the same target.
func xorDistance(a, b [32]byte) [32]byte {
	var d [32]byte
	for i := range d {
		d[i] = a[i] ^ b[i]
	}
	return d
}

func TestAddGetRemove(t *testing.T) {
	tb := New(withFirst(0x01), 20)
	n := Node{ID: withFirst(0x40), Host: "192.0.2.7", Port: 4000}
	if !tb.Add(n) {
		t.Fatal("Add of a new node returned false")
	}
	if got, ok := tb.Get(n.ID); !ok || got != n {
		t.Fatalf("Get after Add = %+v, %v; want %+v, true", got, ok, n)
	}
	if !tb.Remove(n.ID) {
		t.Fatal("Remove of a stored node returned false")
	}
	if _, ok := tb.Get(n.ID); ok {
		t.Error("Get after Remove still finds the node")
	}
}

// Upstream kademlia-routing-table accepts its own id. This rule comes from the design.
func TestOwnIDRefused(t *testing.T) {
	self := withFirst(0x40)
	tb := New(self, 20)
	if tb.Add(Node{ID: self, Host: "192.0.2.7", Port: 4000}) {
		t.Error("Add of our own id returned true, want false")
	}
	if _, ok := tb.Get(self); ok {
		t.Error("Get of our own id found a node after a refused Add")
	}
}

// A bucket holds the nodes that share the same number of leading bits with our id, as in
// kademlia-routing-table (_diff). With our id all zeros, every id whose first byte is 0x80 or more
// is in bucket 0, and an id whose first byte is 0x40 is in bucket 1.
func TestFullBucketRefusesNextNode(t *testing.T) {
	const k = 20
	tb := New([32]byte{}, k)
	r := rand.New(rand.NewPCG(3, 3))
	for i := 0; i < k; i++ {
		id := randomID(r)
		id[0] |= 0x80
		if !tb.Add(Node{ID: id, Port: i}) {
			t.Fatalf("Add %d into a bucket with room returned false", i)
		}
	}
	id := randomID(r)
	id[0] |= 0x80
	if tb.Add(Node{ID: id, Port: k}) {
		t.Errorf("Add of node %d into a full bucket returned true, want false", k+1)
	}
	if !tb.Add(Node{ID: withFirst(0x40), Port: k + 1}) {
		t.Error("Add into an empty bucket returned false; a full bucket must not block other buckets")
	}
}

func TestClosestMatchesBruteForceSort(t *testing.T) {
	const k = 20
	r := rand.New(rand.NewPCG(4, 4))
	tb := New(randomID(r), k)
	var stored []Node
	for i := 0; i < 500; i++ {
		n := Node{ID: randomID(r), Port: i}
		if tb.Add(n) {
			stored = append(stored, n)
		}
	}
	if len(stored) < k {
		t.Fatalf("only %d of 500 random nodes were stored, need at least %d", len(stored), k)
	}
	target := randomID(r)
	want := slices.Clone(stored)
	slices.SortFunc(want, func(a, b Node) int {
		da, db := xorDistance(a.ID, target), xorDistance(b.ID, target)
		return bytes.Compare(da[:], db[:])
	})
	want = want[:k]
	got := tb.Closest(target, k)
	if len(got) != k {
		t.Fatalf("Closest(target, %d) returned %d nodes, want %d", k, len(got), k)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Closest[%d] is node %d, want node %d (XOR order)", i, got[i].Port, want[i].Port)
		}
	}
}

func TestClosestReturnsAtMostN(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 5))
	tb := New(randomID(r), 20)
	for i := 0; i < 100; i++ {
		tb.Add(Node{ID: randomID(r), Port: i})
	}
	target := randomID(r)
	for _, n := range []int{5, 1000} {
		got := tb.Closest(target, n)
		if want := min(n, tb.Len()); len(got) != want {
			t.Errorf("Closest(target, %d) returned %d nodes, want %d", n, len(got), want)
		}
	}
}

func TestEdgeCases(t *testing.T) {
	tb := New(withFirst(0x01), 20)
	a := Node{ID: withFirst(0x40), Host: "192.0.2.7", Port: 4000}
	if !tb.Add(a) {
		t.Fatal("Add of a new node returned false")
	}
	if !tb.Add(Node{ID: a.ID, Host: "192.0.2.8", Port: 4001}) {
		t.Error("Add of an id already stored returned false")
	}
	if tb.Len() != 1 {
		t.Errorf("Len after adding one id twice = %d, want 1", tb.Len())
	}
	if got, _ := tb.Get(a.ID); got.Port != 4001 {
		t.Errorf("Get after re-adding = port %d, want 4001", got.Port)
	}
	if tb.Remove(withFirst(0x80)) {
		t.Error("Remove of an unknown id returned true")
	}
	if got := tb.Closest(a.ID, 0); len(got) != 0 {
		t.Errorf("Closest(target, 0) returned %d nodes, want 0", len(got))
	}
	if _, ok := New(a.ID, 20).Random(); ok {
		t.Error("Random of an empty table returned true")
	}
	if got, ok := tb.Random(); !ok || got.ID != a.ID {
		t.Errorf("Random = %+v, %v; want the only stored node", got, ok)
	}
}
