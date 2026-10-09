package qr

import (
	"strings"
	"testing"
)

// An empty text encodes in version 1 at level L.
func TestEncodeEmptyText(t *testing.T) {
	c, err := Encode("", L)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if c.version != 1 || c.Size() != 21 {
		t.Fatalf("version %d size %d, want version 1 size 21", c.version, c.Size())
	}
}

// Version 1 at level M holds 14 bytes in byte mode; the 15th byte needs version 2.
func TestEncodeVersionBoundary(t *testing.T) {
	for _, tc := range []struct{ n, version int }{{14, 1}, {15, 2}} {
		c, err := Encode(strings.Repeat("A", tc.n), M)
		if err != nil {
			t.Fatalf("%d bytes: Encode: %v", tc.n, err)
		}
		if c.version != tc.version {
			t.Fatalf("%d bytes: version %d, want %d", tc.n, c.version, tc.version)
		}
	}
}

// A level outside L to H is an error, not a panic.
func TestEncodeUnknownLevel(t *testing.T) {
	for _, level := range []Level{-1, 4} {
		if _, err := Encode("HB", level); err == nil {
			t.Fatalf("level %d: want an error", level)
		}
	}
}

// Modules outside the symbol, which the quiet zone of Terminal reads, are light.
func TestBlackOutsideSymbolIsLight(t *testing.T) {
	c, err := Encode("HB", M)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	n := c.Size()
	for _, p := range [][2]int{{-1, 0}, {0, -1}, {n, 0}, {0, n}} {
		if c.Black(p[0], p[1]) {
			t.Fatalf("module at x=%d y=%d is dark outside the symbol", p[0], p[1])
		}
	}
}
