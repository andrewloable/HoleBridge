package noise

import "testing"

// dh refuses a remote point that is not canonical or not of prime order. Each point here is one of
// those: the identity, the order-2 point (0, -1), and the identity written with y = p+1.
func TestDHRefusesBadRemotePoints(t *testing.T) {
	var order2, nonCanonical [32]byte
	for i := 1; i < 31; i++ {
		order2[i], nonCanonical[i] = 0xff, 0xff
	}
	order2[0], order2[31] = 0xec, 0x7f
	nonCanonical[0], nonCanonical[31] = 0xee, 0x7f
	points := map[string][32]byte{
		"identity":        {1},
		"order-2":         order2,
		"non-canonical 1": nonCanonical,
	}
	var local KeyPair // dh reads only the seed, so the zero key is a valid input
	for name, p := range points {
		if _, err := dh(p, local); err == nil {
			t.Errorf("dh accepted the %s point", name)
		}
	}
}
