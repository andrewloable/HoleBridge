package qr

import "strings"

// Terminal draws the symbol as text for a terminal, with quiet modules on each side. Each line
// holds two module rows, drawn with the half-block characters.
func (c *Code) Terminal(quiet int) string {
	var b strings.Builder
	n := c.size + 2*quiet
	for y := 0; y < n; y += 2 {
		for x := 0; x < n; x++ {
			top := c.Black(x-quiet, y-quiet)
			bottom := c.Black(x-quiet, y+1-quiet)
			switch {
			case top && bottom:
				b.WriteString("█")
			case top:
				b.WriteString("▀")
			case bottom:
				b.WriteString("▄")
			default:
				b.WriteByte(' ')
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}
