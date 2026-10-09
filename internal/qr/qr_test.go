package qr

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/andrewloable/HoleBridge/internal/testvec"
)

// qrSymbol is one symbol in spec/vectors/qr.json: its version, mask and dark modules, one row per
// string with '1' for a dark module.
type qrSymbol struct {
	Version int      `json:"version"`
	Mask    int      `json:"mask"`
	Modules []string `json:"modules"`
}

// qrVectors is spec/vectors/qr.json, written by spec/gen/qr.js from the npm qrcode package.
type qrVectors struct {
	Cases []struct {
		Text string `json:"text"`
		qrSymbol
	} `json:"cases"`
	Levels struct {
		Text string   `json:"text"`
		L    qrSymbol `json:"L"`
		Q    qrSymbol `json:"Q"`
		H    qrSymbol `json:"H"`
	} `json:"levels"`
	ByteCapacityV40 map[string]int `json:"byte_capacity_v40"`
}

// checkSymbol fails the test unless c has the version, mask and dark modules of want.
func checkSymbol(t *testing.T, c *Code, want qrSymbol) {
	t.Helper()
	if c.version != want.Version || c.mask != want.Mask {
		t.Fatalf("version %d mask %d, want version %d mask %d", c.version, c.mask, want.Version, want.Mask)
	}
	if c.Size() != len(want.Modules) {
		t.Fatalf("size %d, want %d", c.Size(), len(want.Modules))
	}
	for y, row := range want.Modules {
		for x := range row {
			if c.Black(x, y) != (row[x] == '1') {
				t.Fatalf("module at x=%d y=%d differs from the vector", x, y)
			}
		}
	}
}

// Case 1: the 10 texts at level M, one of them a 99-character key link, each encode to the vector's
// version, mask and matrix.
func TestEncodeMatchesVectorsAtM(t *testing.T) {
	var v qrVectors
	testvec.Load(t, "qr.json", &v)
	for i, tc := range v.Cases {
		c, err := Encode(tc.Text, M)
		if err != nil {
			t.Fatalf("case %d: Encode: %v", i, err)
		}
		checkSymbol(t, c, tc.qrSymbol)
	}
}

// Case 2: levels L, Q and H for one text match the vectors.
func TestEncodeLevelsMatchVectors(t *testing.T) {
	var v qrVectors
	testvec.Load(t, "qr.json", &v)
	for _, tc := range []struct {
		name  string
		level Level
		want  qrSymbol
	}{
		{"L", L, v.Levels.L},
		{"Q", Q, v.Levels.Q},
		{"H", H, v.Levels.H},
	} {
		c, err := Encode(v.Levels.Text, tc.level)
		if err != nil {
			t.Fatalf("level %s: Encode: %v", tc.name, err)
		}
		checkSymbol(t, c, tc.want)
	}
}

// Case 3: a text longer than the version 40 capacity returns an error. The capacity at each level
// is the vector's byte_capacity_v40. A text of exactly that length must still encode, so the error
// comes from the length and not from an encoder that rejects everything.
func TestEncodeTooLong(t *testing.T) {
	var v qrVectors
	testvec.Load(t, "qr.json", &v)
	for _, tc := range []struct {
		name  string
		level Level
	}{{"L", L}, {"M", M}, {"Q", Q}, {"H", H}} {
		limit := v.ByteCapacityV40[tc.name]
		if _, err := Encode(strings.Repeat("A", limit), tc.level); err != nil {
			t.Fatalf("level %s: %d bytes (the capacity) should encode: %v", tc.name, limit, err)
		}
		if _, err := Encode(strings.Repeat("A", limit+1), tc.level); err == nil {
			t.Fatalf("level %s: %d bytes should not encode", tc.name, limit+1)
		}
	}
}

// Case 4: Terminal(2) of a version 1 code has (21+2*2)/2, rounded up, lines: 13. Each line is 25
// characters wide: 21 modules plus 2 quiet modules on each side. The text is 11 bytes, which fits
// version 1 at level M (see the vector's cases).
func TestTerminalVersion1Shape(t *testing.T) {
	c, err := Encode("7KQ-M4X-9TR", M)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if c.version != 1 {
		t.Fatalf("version %d, want 1", c.version)
	}
	const width = 21 + 2*2
	lines := strings.Split(strings.TrimSuffix(c.Terminal(2), "\n"), "\n")
	if want := (width + 1) / 2; len(lines) != want {
		t.Fatalf("%d lines, want %d", len(lines), want)
	}
	for i, line := range lines {
		if n := utf8.RuneCountInString(line); n != width {
			t.Fatalf("line %d is %d characters wide, want %d", i, n, width)
		}
	}
}
