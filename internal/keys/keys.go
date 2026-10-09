// Package keys holds the host key (9 symbols of Crockford Base32, docs/security.md, The key) and
// the 32-byte application key that every deployment has.
package keys

import (
	"crypto/rand"
	hexenc "encoding/hex"
	"strings"

	"github.com/andrewloable/HoleBridge/internal/errs"
)

// Alphabet is the Crockford Base32 alphabet a key is written in. It has no I, L, O or U.
const Alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// Normalize turns user input into the canonical 9-symbol key. It ignores case, dashes and spaces,
// reads O as 0 and I and L as 1. Only ASCII letters fold to upper case, so the dotless i and the
// long s stay characters. Length is checked before character. On failure it returns an errs.Error
// with code HB-KEY-INVALID and reason "length" or "character".
func Normalize(s string) (string, error) {
	var k []rune
	for _, r := range s {
		switch {
		case r == '-' || r == ' ':
			continue
		case 'a' <= r && r <= 'z':
			r -= 'a' - 'A'
		}
		switch r {
		case 'O':
			r = '0'
		case 'I', 'L':
			r = '1'
		}
		k = append(k, r)
	}
	if len(k) != 9 {
		return "", errs.E("HB-KEY-INVALID", "length", nil)
	}
	for _, r := range k {
		if !strings.ContainsRune(Alphabet, r) {
			return "", errs.E("HB-KEY-INVALID", "character", nil)
		}
	}
	return string(k), nil
}

// Format groups a normalized key in threes: "XXX-XXX-XXX".
func Format(normalized string) string {
	var b strings.Builder
	for i := 0; i < len(normalized); i++ {
		if i > 0 && i%3 == 0 {
			b.WriteByte('-')
		}
		b.WriteByte(normalized[i])
	}
	return b.String()
}

// Generate returns a new random key: 9 symbols from crypto/rand, uniform over Alphabet.
// Alphabet has 32 symbols and 256 is a multiple of 32, so the low five bits of a byte are uniform.
func Generate() string {
	var b [9]byte
	rand.Read(b[:]) // crypto/rand.Read never returns an error since Go 1.24.
	for i := range b {
		b[i] = Alphabet[b[i]&31]
	}
	return string(b[:])
}

// NewAppKey returns 32 random bytes from crypto/rand.
func NewAppKey() [32]byte {
	var k [32]byte
	rand.Read(k[:]) // crypto/rand.Read never returns an error since Go 1.24.
	return k
}

// ParseAppKey reads 64 hex digits, lowercase or uppercase. Any other input returns an errs.Error
// with code HB-APPKEY-INVALID.
func ParseAppKey(hex string) ([32]byte, error) {
	var k [32]byte
	if len(hex) != 64 {
		return k, errs.E("HB-APPKEY-INVALID", "length", nil)
	}
	if _, err := hexenc.Decode(k[:], []byte(hex)); err != nil {
		return [32]byte{}, errs.E("HB-APPKEY-INVALID", "character", nil)
	}
	return k, nil
}

// FormatAppKey writes k as 64 lowercase hex digits.
func FormatAppKey(k [32]byte) string {
	return hexenc.EncodeToString(k[:])
}
