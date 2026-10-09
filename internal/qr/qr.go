// Package qr encodes text as a QR Code (ISO/IEC 18004) in byte mode, versions 1 to 40. It is
// written by hand with no dependency; spec/vectors/qr.json holds the symbols it must reproduce.
package qr

import "errors"

// Level is an error correction level.
type Level int

// The error correction levels.
const (
	L Level = iota
	M
	Q
	H
)

// Code is an encoded symbol. Encode sets its version and mask.
type Code struct {
	version int
	mask    int
	size    int
	dark    []bool // row-major: the module at column x, row y is dark[y*size+x]
}

var (
	errLevel   = errors.New("qr: unknown error correction level")
	errTooLong = errors.New("qr: text does not fit in version 40")
)

// Encode encodes text in byte mode at level, with the smallest version that fits and the best
// mask. It returns an error if text does not fit in version 40.
func Encode(text string, level Level) (*Code, error) {
	if level < L || level > H {
		return nil, errLevel
	}
	v := smallestVersion(len(text), level)
	if v == 0 {
		return nil, errTooLong
	}
	base := newBase(v)
	base.placeData(withECC(codewords(text, v, level), v, level))

	// The mask with the lowest penalty wins; on a tie the lowest mask number wins.
	var best *grid
	bestMask, bestScore := 0, 0
	for m := 0; m < 8; m++ {
		g := base.clone()
		g.applyMask(m)
		g.drawFormat(formatBits(level, m))
		if s := g.penalty(); best == nil || s < bestScore {
			best, bestMask, bestScore = g, m, s
		}
	}
	return &Code{version: v, mask: bestMask, size: best.size, dark: best.dark}, nil
}

// Size returns the width and height of the symbol in modules.
func (c *Code) Size() int {
	return c.size
}

// Black reports whether the module at column x and row y is dark. Modules outside the symbol are
// light.
func (c *Code) Black(x, y int) bool {
	if x < 0 || y < 0 || x >= c.size || y >= c.size {
		return false
	}
	return c.dark[y*c.size+x]
}

// eccPerBlock is the number of error correction codewords in one block, by level and version
// (ISO/IEC 18004 Table 9). Index 0 of each row is unused.
var eccPerBlock = [4][41]int{
	{0, 7, 10, 15, 20, 26, 18, 20, 24, 30, 18, 20, 24, 26, 30, 22, 24, 28, 30, 28, 28, 28, 28, 30, 30, 26, 28, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30},
	{0, 10, 16, 26, 18, 24, 16, 18, 22, 22, 26, 30, 22, 22, 24, 24, 28, 28, 26, 26, 26, 26, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28},
	{0, 13, 22, 18, 26, 18, 24, 18, 22, 20, 24, 28, 26, 24, 20, 30, 24, 28, 28, 26, 30, 28, 30, 30, 30, 30, 28, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30},
	{0, 17, 28, 22, 16, 22, 28, 26, 26, 24, 28, 24, 28, 22, 24, 24, 30, 28, 28, 26, 28, 30, 24, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30},
}

// blockCount is the number of error correction blocks, by level and version (ISO/IEC 18004
// Table 9). Index 0 of each row is unused.
var blockCount = [4][41]int{
	{0, 1, 1, 1, 1, 1, 2, 2, 2, 2, 4, 4, 4, 4, 4, 6, 6, 6, 6, 7, 8, 8, 9, 9, 10, 12, 12, 12, 13, 14, 15, 16, 17, 18, 19, 19, 20, 21, 22, 24, 25},
	{0, 1, 1, 1, 2, 2, 4, 4, 4, 5, 5, 5, 8, 9, 9, 10, 10, 11, 13, 14, 16, 17, 17, 18, 20, 21, 23, 25, 26, 28, 29, 31, 33, 35, 37, 38, 40, 43, 45, 47, 49},
	{0, 1, 1, 2, 2, 4, 4, 6, 6, 8, 8, 8, 10, 12, 16, 12, 17, 16, 18, 21, 20, 23, 23, 25, 27, 29, 34, 34, 35, 38, 40, 43, 45, 48, 51, 53, 56, 59, 62, 65, 68},
	{0, 1, 1, 2, 4, 4, 4, 5, 6, 8, 8, 11, 11, 16, 16, 18, 16, 19, 21, 25, 25, 25, 34, 30, 32, 35, 37, 40, 42, 45, 48, 51, 54, 57, 60, 63, 66, 70, 74, 77, 81},
}

// formatECL is the 2-bit error correction indicator in the format bits, by level.
var formatECL = [4]int{1, 0, 3, 2}

// smallestVersion returns the smallest version whose data capacity holds n bytes in byte mode at
// level, or 0 if none does.
func smallestVersion(n int, level Level) int {
	for v := 1; v <= 40; v++ {
		if 4+charCountBits(v)+8*n <= dataCodewords(v, level)*8 {
			return v
		}
	}
	return 0
}

// charCountBits is the width of the byte-mode character count in version v.
func charCountBits(v int) int {
	if v <= 9 {
		return 8
	}
	return 16
}

// rawCodewords is the number of codewords in version v: all modules except the remainder bits,
// divided by 8.
func rawCodewords(v int) int {
	n := (16*v+128)*v + 64
	if v >= 2 {
		align := v/7 + 2
		n -= (25*align-10)*align - 55
		if v >= 7 {
			n -= 36
		}
	}
	return n / 8
}

// dataCodewords is the number of data codewords in version v at level.
func dataCodewords(v int, level Level) int {
	return rawCodewords(v) - blockCount[level][v]*eccPerBlock[level][v]
}

// bitBuffer collects bits, most significant first.
type bitBuffer struct {
	data []byte
	n    int // bits written
}

// put appends the low count bits of val.
func (b *bitBuffer) put(val, count int) {
	for i := count - 1; i >= 0; i-- {
		if b.n%8 == 0 {
			b.data = append(b.data, 0)
		}
		if (val>>i)&1 == 1 {
			b.data[len(b.data)-1] |= 0x80 >> (b.n % 8)
		}
		b.n++
	}
}

// codewords returns the data codewords of text in byte mode for version v at level: mode, count,
// bytes, terminator, byte padding and the 0xEC and 0x11 pad codewords.
func codewords(text string, v int, level Level) []byte {
	capacity := dataCodewords(v, level)
	var b bitBuffer
	b.put(0b0100, 4)
	b.put(len(text), charCountBits(v))
	for i := 0; i < len(text); i++ {
		b.put(int(text[i]), 8)
	}
	b.put(0, min(4, capacity*8-b.n))
	b.put(0, (8-b.n%8)%8)
	for i := 0; len(b.data) < capacity; i++ {
		if i%2 == 0 {
			b.data = append(b.data, 0xEC)
		} else {
			b.data = append(b.data, 0x11)
		}
	}
	return b.data
}

// withECC splits the data codewords into blocks, adds the Reed-Solomon codewords to each block and
// interleaves the result. The short blocks come first.
func withECC(data []byte, v int, level Level) []byte {
	blocks := blockCount[level][v]
	ecc := eccPerBlock[level][v]
	raw := rawCodewords(v)
	short := raw / blocks
	numShort := blocks - raw%blocks
	gen := rsGenerator(ecc)
	dataBlocks := make([][]byte, blocks)
	eccBlocks := make([][]byte, blocks)
	k := 0
	for i := range dataBlocks {
		n := short - ecc
		if i >= numShort {
			n++
		}
		dataBlocks[i] = data[k : k+n]
		k += n
		eccBlocks[i] = rsRemainder(dataBlocks[i], gen)
	}
	out := make([]byte, 0, raw)
	for i := 0; i <= short-ecc; i++ {
		for _, d := range dataBlocks {
			if i < len(d) {
				out = append(out, d[i])
			}
		}
	}
	for i := 0; i < ecc; i++ {
		for _, e := range eccBlocks {
			out = append(out, e[i])
		}
	}
	return out
}

// gfMul multiplies in GF(256) with the reducing polynomial x^8+x^4+x^3+x^2+1 (0x11D).
func gfMul(a, b byte) byte {
	var p byte
	for ; b != 0; b >>= 1 {
		if b&1 == 1 {
			p ^= a
		}
		hi := a & 0x80
		a <<= 1
		if hi != 0 {
			a ^= 0x1D
		}
	}
	return p
}

// rsGenerator returns the coefficients, highest first, of the Reed-Solomon generator polynomial of
// degree n: the product of (x - 2^i) for i from 0 to n-1.
func rsGenerator(n int) []byte {
	g := []byte{1}
	root := byte(1)
	for i := 0; i < n; i++ {
		next := make([]byte, len(g)+1)
		for j, c := range g {
			next[j] ^= c
			next[j+1] ^= gfMul(c, root)
		}
		g = next
		root = gfMul(root, 2)
	}
	return g
}

// rsRemainder returns the Reed-Solomon codewords of data: the remainder of data times x^n divided
// by gen, where n is len(gen)-1.
func rsRemainder(data, gen []byte) []byte {
	rem := make([]byte, len(gen)-1)
	for _, d := range data {
		f := d ^ rem[0]
		copy(rem, rem[1:])
		rem[len(rem)-1] = 0
		for i := range rem {
			rem[i] ^= gfMul(gen[i+1], f)
		}
	}
	return rem
}

// bchCode returns data shifted left by degree, with its BCH remainder modulo poly in the low bits.
func bchCode(data, poly, degree int) int {
	rem := data << degree
	for i := 20; i >= degree; i-- {
		if (rem>>i)&1 == 1 {
			rem ^= poly << (i - degree)
		}
	}
	return data<<degree | rem
}

// formatBits returns the 15 format bits for level and mask.
func formatBits(level Level, mask int) int {
	return bchCode(formatECL[level]<<3|mask, 0x537, 10) ^ 0x5412
}

// grid is a symbol being built: its dark modules, and which modules are function patterns. Function
// modules hold no data and are never masked.
type grid struct {
	size int
	dark []bool
	fn   []bool
}

func (g *grid) clone() *grid {
	return &grid{size: g.size, dark: append([]bool(nil), g.dark...), fn: append([]bool(nil), g.fn...)}
}

// set draws a function module at column x, row y.
func (g *grid) set(x, y int, dark bool) {
	g.dark[y*g.size+x] = dark
	g.fn[y*g.size+x] = true
}

// newBase returns version v with its function patterns drawn and the format and version areas
// reserved, and no data.
func newBase(v int) *grid {
	n := v*4 + 17
	g := &grid{size: n, dark: make([]bool, n*n), fn: make([]bool, n*n)}
	for _, o := range [3][2]int{{0, 0}, {n - 7, 0}, {0, n - 7}} {
		for dy := -1; dy <= 7; dy++ {
			for dx := -1; dx <= 7; dx++ {
				x, y := o[0]+dx, o[1]+dy
				if x < 0 || y < 0 || x >= n || y >= n {
					continue
				}
				inner := dx >= 0 && dx <= 6 && dy >= 0 && dy <= 6
				ring := dx == 0 || dx == 6 || dy == 0 || dy == 6
				core := dx >= 2 && dx <= 4 && dy >= 2 && dy <= 4
				g.set(x, y, inner && (ring || core))
			}
		}
	}
	for i := 8; i < n-8; i++ {
		g.set(6, i, i%2 == 0)
		g.set(i, 6, i%2 == 0)
	}
	pos := alignmentPositions(v)
	last := len(pos) - 1
	for i, cy := range pos {
		for j, cx := range pos {
			if (i == 0 && j == 0) || (i == 0 && j == last) || (i == last && j == 0) {
				continue
			}
			for dy := -2; dy <= 2; dy++ {
				for dx := -2; dx <= 2; dx++ {
					g.set(cx+dx, cy+dy, max(dx, -dx, dy, -dy) != 1)
				}
			}
		}
	}
	g.set(8, n-8, true)
	g.drawFormat(0)
	if v >= 7 {
		g.drawVersion(v)
	}
	return g
}

// alignmentPositions returns the centre coordinates of the alignment patterns in version v, in
// ascending order (none for version 1).
func alignmentPositions(v int) []int {
	if v == 1 {
		return nil
	}
	n := v*4 + 17
	align := v/7 + 2
	step := (v*4 + 4 + align*2 - 3) / (align*2 - 2) * 2
	if v == 32 {
		step = 26
	}
	pos := []int{6}
	for p := n - 7; len(pos) < align; p -= step {
		pos = append(pos[:1], append([]int{p}, pos[1:]...)...)
	}
	return pos
}

// drawFormat writes the 15 format bits. Called with 0 it reserves the format areas.
func (g *grid) drawFormat(bits int) {
	n := g.size
	bit := func(i int) bool { return (bits>>i)&1 == 1 }
	for i := 0; i <= 5; i++ {
		g.set(8, i, bit(i))
	}
	g.set(8, 7, bit(6))
	g.set(8, 8, bit(7))
	g.set(7, 8, bit(8))
	for i := 9; i < 15; i++ {
		g.set(14-i, 8, bit(i))
	}
	for i := 0; i < 8; i++ {
		g.set(n-1-i, 8, bit(i))
	}
	for i := 8; i < 15; i++ {
		g.set(8, n-15+i, bit(i))
	}
}

// drawVersion writes the 18 version bits beside the top-right and bottom-left finders (version 7
// and up).
func (g *grid) drawVersion(v int) {
	n := g.size
	bits := bchCode(v, 0x1F25, 12)
	for i := 0; i < 18; i++ {
		dark := (bits>>i)&1 == 1
		a := n - 11 + i%3
		b := i / 3
		g.set(a, b, dark)
		g.set(b, a, dark)
	}
}

// placeData writes the codeword bits in the zigzag order of ISO/IEC 18004, skipping function
// modules. Modules left over after the data stay light.
func (g *grid) placeData(data []byte) {
	n := g.size
	i := 0
	for right := n - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := 0; vert < n; vert++ {
			for j := 0; j < 2; j++ {
				x := right - j
				y := vert
				if (right+1)&2 == 0 {
					y = n - 1 - vert
				}
				idx := y*n + x
				if !g.fn[idx] && i < len(data)*8 {
					g.dark[idx] = (data[i>>3]>>(7-i&7))&1 == 1
					i++
				}
			}
		}
	}
}

// maskBit reports whether mask m inverts the module at column x, row y.
func maskBit(m, x, y int) bool {
	switch m {
	case 0:
		return (x+y)%2 == 0
	case 1:
		return y%2 == 0
	case 2:
		return x%3 == 0
	case 3:
		return (x+y)%3 == 0
	case 4:
		return (x/3+y/2)%2 == 0
	case 5:
		return x*y%2+x*y%3 == 0
	case 6:
		return (x*y%2+x*y%3)%2 == 0
	default:
		return ((x+y)%2+x*y%3)%2 == 0
	}
}

// applyMask inverts the data modules that mask m selects.
func (g *grid) applyMask(m int) {
	n := g.size
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			if !g.fn[y*n+x] && maskBit(m, x, y) {
				g.dark[y*n+x] = !g.dark[y*n+x]
			}
		}
	}
}

// penalty scores the symbol with the four rules of ISO/IEC 18004 (N1 to N4) that choose the mask.
func (g *grid) penalty() int {
	n := g.size
	at := func(x, y int) bool { return g.dark[y*n+x] }
	score := 0
	for k := 0; k < n; k++ {
		score += linePenalty(n, func(i int) bool { return at(i, k) })
		score += linePenalty(n, func(i int) bool { return at(k, i) })
	}
	for y := 0; y+1 < n; y++ {
		for x := 0; x+1 < n; x++ {
			c := at(x, y)
			if c == at(x+1, y) && c == at(x, y+1) && c == at(x+1, y+1) {
				score += 3
			}
		}
	}
	dark := 0
	for _, d := range g.dark {
		if d {
			dark++
		}
	}
	// N4: 10 points for each 5 percent that the dark share strays from 50 percent.
	dev := 20*dark - 10*n*n
	if dev < 0 {
		dev = -dev
	}
	return score + 10*(dev/(n*n))
}

// linePenalty scores one row or column of n modules: N1 gives 3 points per run of five or more
// modules of one colour, plus 1 per module beyond five; N3 gives 40 points per 1011101 pattern with
// four light modules on one side.
func linePenalty(n int, get func(int) bool) int {
	score := 0
	run := 1
	for i := 1; i <= n; i++ {
		if i < n && get(i) == get(i-1) {
			run++
			continue
		}
		if run >= 5 {
			score += 3 + run - 5
		}
		run = 1
	}
	window := 0
	for i := 0; i < n; i++ {
		window = (window<<1 | b2i(get(i))) & 0x7FF
		if i >= 10 && (window == 0x5D0 || window == 0x05D) {
			score += 40
		}
	}
	return score
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
