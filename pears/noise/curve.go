// Ported from noise-curve-ed 2.1.0 (index.js), ISC license. The npm package has no copyright line of
// its own; its repository is chm-diederichs/noise-curve-ed.

package noise

import (
	"bytes"
	"crypto/sha512"
	"errors"

	"filippo.io/edwards25519"
)

var errBadPoint = errors.New("noise: remote Ed25519 key is not a canonical point of prime order")

// dh is the Ed25519 Diffie-Hellman that noise-curve-ed computes: the scalar of local's secret applied
// to the point remote, as a compressed 32-byte point. The scalar is the clamped SHA-512 of the seed.
//
// libsodium's noclamp multiplication refuses a remote point that is not canonical, has small order or
// lies outside the prime-order subgroup, so dh refuses it too. Every honest key has prime order, and
// the scalar reduced mod l then gives the same point as the unreduced one.
func dh(remote [32]byte, local KeyPair) ([32]byte, error) {
	p, err := edwards25519.NewIdentityPoint().SetBytes(remote[:])
	if err != nil {
		return [32]byte{}, err
	}
	if !bytes.Equal(p.Bytes(), remote[:]) || !primeOrder(p) {
		return [32]byte{}, errBadPoint
	}
	digest := sha512.Sum512(local.Secret[:32])
	s, err := edwards25519.NewScalar().SetBytesWithClamping(digest[:32])
	if err != nil {
		return [32]byte{}, err
	}
	var r [32]byte
	copy(r[:], edwards25519.NewIdentityPoint().ScalarMult(s, p).Bytes())
	return r, nil
}

// primeOrder reports whether p is a point of prime order l other than the identity. The scalar l-1 is
// -1 mod l, so (l-1)p + p is l·p, which is the identity exactly when p lies in the prime-order subgroup.
func primeOrder(p *edwards25519.Point) bool {
	one := make([]byte, 32)
	one[0] = 1
	s, _ := edwards25519.NewScalar().SetCanonicalBytes(one)
	lp := edwards25519.NewIdentityPoint().ScalarMult(edwards25519.NewScalar().Negate(s), p)
	lp.Add(lp, p)
	id := edwards25519.NewIdentityPoint()
	return lp.Equal(id) == 1 && p.Equal(id) == 0
}
