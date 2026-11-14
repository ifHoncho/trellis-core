// Package qr encodes text as a QR code (ISO/IEC 18004, model 2): byte mode,
// versions 1–40, all four error correction levels, automatic mask choice.
//
// Trellis shows two things as QR codes: a node's client-AP credentials, so
// a phone joins by scanning (architecture §10), and the mesh profile, so a
// new node is provisioned by scanning (§12.6). Nodes work with no internet,
// so the encoder is built in.
package qr

import (
	"errors"
	"fmt"
	"strings"
)

// Level is the error correction level.
type Level int

// Error correction levels, by the share of codewords that can be restored.
const (
	L Level = iota // about 7%
	M              // about 15%
	Q              // about 25%
	H              // about 30%
)

// Code is a QR code symbol.
type Code struct {
	Version int // 1–40
	Level   Level
	Mask    int // 0–7
	Size    int // modules per side, without the quiet zone

	modules  [][]bool // [y][x], true = dark
	function [][]bool // modules that are not data: finders, timing, format…
}

// ErrTooLong means the text does not fit the largest symbol at this level.
var ErrTooLong = errors.New("too long for a QR code")

// Tables indexed [level][version]; index 0 is unused.
var (
	eccPerBlock = [4][41]int8{
		{-1, 7, 10, 15, 20, 26, 18, 20, 24, 30, 18, 20, 24, 26, 30, 22, 24, 28, 30, 28, 28, 28, 28, 30, 30, 26, 28, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30},
		{-1, 10, 16, 26, 18, 24, 16, 18, 22, 22, 26, 30, 22, 22, 24, 24, 28, 28, 26, 26, 26, 26, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28},
		{-1, 13, 22, 18, 26, 18, 24, 18, 22, 20, 24, 28, 26, 24, 20, 30, 24, 28, 28, 26, 30, 28, 30, 30, 30, 30, 28, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30},
		{-1, 17, 28, 22, 16, 22, 28, 26, 26, 24, 28, 24, 28, 22, 24, 24, 30, 28, 28, 26, 28, 30, 24, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30},
	}
	numBlocks = [4][41]int8{
		{-1, 1, 1, 1, 1, 1, 2, 2, 2, 2, 4, 4, 4, 4, 4, 6, 6, 6, 6, 7, 8, 8, 9, 9, 10, 12, 12, 12, 13, 14, 15, 16, 17, 18, 19, 19, 20, 21, 22, 24, 25},
		{-1, 1, 1, 1, 2, 2, 4, 4, 4, 5, 5, 5, 8, 9, 9, 10, 10, 11, 13, 14, 16, 17, 17, 18, 20, 21, 23, 25, 26, 28, 29, 31, 33, 35, 37, 38, 40, 43, 45, 47, 49},
		{-1, 1, 1, 2, 2, 4, 4, 6, 6, 8, 8, 8, 10, 12, 16, 12, 17, 16, 18, 21, 20, 23, 23, 25, 27, 29, 34, 34, 35, 38, 40, 43, 45, 48, 51, 53, 56, 59, 62, 65, 68},
		{-1, 1, 1, 2, 4, 4, 4, 5, 6, 8, 8, 11, 11, 16, 16, 18, 16, 19, 21, 25, 25, 25, 34, 30, 32, 35, 37, 40, 42, 45, 48, 51, 54, 57, 60, 63, 66, 70, 74, 77, 81},
	}
	// formatBits is each level's two-bit code in the format information.
	formatBits = [4]int{L: 1, M: 0, Q: 3, H: 2}
)

// rawModules is the number of data modules in a symbol of this version:
// everything that is not a function pattern.
func rawModules(ver int) int {
	n := (16*ver+128)*ver + 64
	if ver >= 2 {
		align := ver/7 + 2
		n -= (25*align-10)*align - 55
		if ver >= 7 {
			n -= 36
		}
	}
	return n
}

// dataCodewords is the number of 8-bit data codewords a version holds at a
// level, after error correction is taken out.
func dataCodewords(ver int, lvl Level) int {
	return rawModules(ver)/8 - int(eccPerBlock[lvl][ver])*int(numBlocks[lvl][ver])
}

// Capacity is the longest text, in bytes, that fits a version at a level.
func Capacity(ver int, lvl Level) int {
	bits := dataCodewords(ver, lvl)*8 - 4 - countBits(ver)
	return bits / 8
}

func countBits(ver int) int {
	if ver <= 9 {
		return 8
	}
	return 16
}

// Encode makes the smallest symbol that holds text at the given level,
// with the mask that scores best.
func Encode(text string, lvl Level) (*Code, error) {
	return encode([]byte(text), lvl, -1)
}

func encode(data []byte, lvl Level, mask int) (*Code, error) {
	if lvl < L || lvl > H {
		return nil, fmt.Errorf("qr: bad error correction level %d", lvl)
	}
	ver := 0
	for v := 1; v <= 40; v++ {
		if len(data) <= Capacity(v, lvl) {
			ver = v
			break
		}
	}
	if ver == 0 {
		return nil, fmt.Errorf("qr: %d bytes: %w", len(data), ErrTooLong)
	}

	// Data bit stream: mode, length, bytes, terminator, padding.
	var bb bitBuffer
	bb.put(0b0100, 4)
	bb.put(len(data), countBits(ver))
	for _, b := range data {
		bb.put(int(b), 8)
	}
	capacity := dataCodewords(ver, lvl) * 8
	bb.put(0, min(4, capacity-bb.n))
	bb.put(0, (8-bb.n%8)%8)
	for pad := 0xEC; bb.n < capacity; pad ^= 0xEC ^ 0x11 {
		bb.put(pad, 8)
	}

	c := &Code{Version: ver, Level: lvl, Size: ver*4 + 17}
	c.modules = make([][]bool, c.Size)
	c.function = make([][]bool, c.Size)
	for i := range c.modules {
		c.modules[i] = make([]bool, c.Size)
		c.function[i] = make([]bool, c.Size)
	}
	c.drawFunctionPatterns()
	c.drawCodewords(c.interleave(bb.bytes))

	if mask < 0 {
		best := -1
		for m := 0; m < 8; m++ {
			c.applyMask(m)
			c.drawFormat(m)
			if p := c.penalty(); best < 0 || p < best {
				best, mask = p, m
			}
			c.applyMask(m) // XOR again: undo
		}
	}
	c.Mask = mask
	c.applyMask(mask)
	c.drawFormat(mask)
	return c, nil
}

// Dark reports whether the module at column x, row y is dark. Outside the
// symbol (the quiet zone) everything is light.
func (c *Code) Dark(x, y int) bool {
	return x >= 0 && y >= 0 && x < c.Size && y < c.Size && c.modules[y][x]
}

// Rows returns the symbol as one string per row, '1' for dark and '0' for
// light, without a quiet zone: the form the API hands to the web UI.
func (c *Code) Rows() []string {
	out := make([]string, c.Size)
	var b strings.Builder
	for y := 0; y < c.Size; y++ {
		b.Reset()
		for x := 0; x < c.Size; x++ {
			if c.modules[y][x] {
				b.WriteByte('1')
			} else {
				b.WriteByte('0')
			}
		}
		out[y] = b.String()
	}
	return out
}

// Terminal draws the symbol with half-block characters, two rows per line,
// with a quiet zone. With invert it draws light modules as blocks, for the
// usual terminal with light text on a dark background.
func (c *Code) Terminal(invert bool) string {
	const quiet = 2
	on := func(x, y int) bool { return c.Dark(x, y) != invert }
	var b strings.Builder
	for y := -quiet; y < c.Size+quiet; y += 2 {
		for x := -quiet; x < c.Size+quiet; x++ {
			top, bottom := on(x, y), on(x, y+1)
			if y+1 >= c.Size+quiet {
				bottom = invert
			}
			switch {
			case top && bottom:
				b.WriteRune('█')
			case top:
				b.WriteRune('▀')
			case bottom:
				b.WriteRune('▄')
			default:
				b.WriteByte(' ')
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// TerminalANSI is Terminal with the colours set explicitly (black on
// white), so the symbol scans from a terminal of any colour scheme.
func (c *Code) TerminalANSI() string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(c.Terminal(false), "\n"), "\n") {
		b.WriteString("\x1b[30;47m" + line + "\x1b[0m\n")
	}
	return b.String()
}

// SVG renders the symbol as a standalone SVG image, dark on white, with the
// four-module quiet zone scanners expect.
func (c *Code) SVG() string {
	const quiet = 4
	var b strings.Builder
	n := c.Size + 2*quiet
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges">`, n, n)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="`, n, n)
	for y := 0; y < c.Size; y++ {
		for x := 0; x < c.Size; x++ {
			if !c.modules[y][x] {
				continue
			}
			run := 1
			for x+run < c.Size && c.modules[y][x+run] {
				run++
			}
			fmt.Fprintf(&b, "M%d %dh%dv1h-%dz", x+quiet, y+quiet, run, run)
			x += run - 1
		}
	}
	b.WriteString(`"/></svg>`)
	return b.String()
}

// Bits ------------------------------------------------------------------------

type bitBuffer struct {
	bytes []byte
	n     int // bits written
}

// put appends the low count bits of v, most significant first.
func (b *bitBuffer) put(v, count int) {
	for i := count - 1; i >= 0; i-- {
		if b.n%8 == 0 {
			b.bytes = append(b.bytes, 0)
		}
		if v>>i&1 != 0 {
			b.bytes[b.n/8] |= 0x80 >> (b.n % 8)
		}
		b.n++
	}
}

// Error correction --------------------------------------------------------------

// gfMul multiplies in GF(2^8) modulo x^8+x^4+x^3+x^2+1 (0x11D).
func gfMul(a, b byte) byte {
	var z int
	for i := 7; i >= 0; i-- {
		z = z<<1 ^ (z>>7)*0x11D
		z ^= int(b>>i&1) * int(a)
	}
	return byte(z)
}

// rsDivisor is the Reed–Solomon generator polynomial of the given degree,
// without its leading coefficient (always 1).
func rsDivisor(degree int) []byte {
	out := make([]byte, degree)
	out[degree-1] = 1
	root := byte(1)
	for i := 0; i < degree; i++ {
		for j := range out {
			out[j] = gfMul(out[j], root)
			if j+1 < len(out) {
				out[j] ^= out[j+1]
			}
		}
		root = gfMul(root, 2)
	}
	return out
}

// rsRemainder is the error correction codewords for one block of data.
func rsRemainder(data, divisor []byte) []byte {
	out := make([]byte, len(divisor))
	for _, b := range data {
		factor := b ^ out[0]
		copy(out, out[1:])
		out[len(out)-1] = 0
		for i := range out {
			out[i] ^= gfMul(divisor[i], factor)
		}
	}
	return out
}

// interleave splits the data codewords into blocks, appends each block's
// error correction, and interleaves them into the final codeword sequence.
func (c *Code) interleave(data []byte) []byte {
	blocks := int(numBlocks[c.Level][c.Version])
	eccLen := int(eccPerBlock[c.Level][c.Version])
	raw := rawModules(c.Version) / 8
	short := blocks - raw%blocks // number of blocks one data codeword shorter
	shortLen := raw / blocks

	divisor := rsDivisor(eccLen)
	all := make([][]byte, blocks)
	for i, k := 0, 0; i < blocks; i++ {
		n := shortLen - eccLen
		if i >= short {
			n++
		}
		dat := data[k : k+n]
		k += n
		block := make([]byte, 0, shortLen+1)
		block = append(block, dat...)
		if i < short {
			block = append(block, 0) // placeholder, skipped below
		}
		all[i] = append(block, rsRemainder(dat, divisor)...)
	}
	out := make([]byte, 0, raw)
	for i := 0; i < len(all[0]); i++ {
		for j, block := range all {
			if i != shortLen-eccLen || j >= short {
				out = append(out, block[i])
			}
		}
	}
	return out
}

// Drawing -----------------------------------------------------------------------

func (c *Code) setFunction(x, y int, dark bool) {
	c.modules[y][x] = dark
	c.function[y][x] = true
}

func (c *Code) drawFunctionPatterns() {
	for i := 0; i < c.Size; i++ {
		c.setFunction(6, i, i%2 == 0)
		c.setFunction(i, 6, i%2 == 0)
	}
	c.drawFinder(3, 3)
	c.drawFinder(c.Size-4, 3)
	c.drawFinder(3, c.Size-4)

	pos := alignmentPositions(c.Version)
	for i, y := range pos {
		for j, x := range pos {
			last := len(pos) - 1
			if (i == 0 && j == 0) || (i == 0 && j == last) || (i == last && j == 0) {
				continue // a finder pattern is there
			}
			for dy := -2; dy <= 2; dy++ {
				for dx := -2; dx <= 2; dx++ {
					c.setFunction(x+dx, y+dy, max(abs(dx), abs(dy)) != 1)
				}
			}
		}
	}
	c.drawFormat(0) // reserves the modules; redrawn with the real mask
	c.drawVersion()
}

// drawFinder draws a finder pattern centred on x, y with its separator.
func (c *Code) drawFinder(cx, cy int) {
	for dy := -4; dy <= 4; dy++ {
		for dx := -4; dx <= 4; dx++ {
			x, y := cx+dx, cy+dy
			if x < 0 || y < 0 || x >= c.Size || y >= c.Size {
				continue
			}
			d := max(abs(dx), abs(dy))
			c.setFunction(x, y, d != 2 && d != 4)
		}
	}
}

// alignmentPositions are the row and column coordinates of the alignment
// pattern centres.
func alignmentPositions(ver int) []int {
	if ver == 1 {
		return nil
	}
	n := ver/7 + 2
	step := 26
	if ver != 32 {
		step = (ver*4 + n*2 + 1) / (n*2 - 2) * 2
	}
	out := make([]int, n)
	out[0] = 6
	for i, pos := n-1, ver*4+17-7; i >= 1; i, pos = i-1, pos-step {
		out[i] = pos
	}
	return out
}

// drawFormat writes both copies of the format information: the level and
// the mask, protected by a (15,5) BCH code.
func (c *Code) drawFormat(mask int) {
	data := formatBits[c.Level]<<3 | mask
	rem := data
	for i := 0; i < 10; i++ {
		rem = rem<<1 ^ (rem>>9)*0x537
	}
	bits := (data<<10 | rem) ^ 0x5412
	bit := func(i int) bool { return bits>>i&1 != 0 }

	for i := 0; i <= 5; i++ {
		c.setFunction(8, i, bit(i))
	}
	c.setFunction(8, 7, bit(6))
	c.setFunction(8, 8, bit(7))
	c.setFunction(7, 8, bit(8))
	for i := 9; i < 15; i++ {
		c.setFunction(14-i, 8, bit(i))
	}
	for i := 0; i < 8; i++ {
		c.setFunction(c.Size-1-i, 8, bit(i))
	}
	for i := 8; i < 15; i++ {
		c.setFunction(8, c.Size-15+i, bit(i))
	}
	c.setFunction(8, c.Size-8, true) // always dark
}

// drawVersion writes both copies of the version information (versions 7
// and up), protected by an (18,6) BCH code.
func (c *Code) drawVersion() {
	if c.Version < 7 {
		return
	}
	rem := c.Version
	for i := 0; i < 12; i++ {
		rem = rem<<1 ^ (rem>>11)*0x1F25
	}
	bits := c.Version<<12 | rem
	for i := 0; i < 18; i++ {
		dark := bits>>i&1 != 0
		a, b := c.Size-11+i%3, i/3
		c.setFunction(a, b, dark)
		c.setFunction(b, a, dark)
	}
}

// drawCodewords places the codeword bits in the zigzag order of the
// standard: two-module columns from the right, alternately up and down,
// skipping function modules and the vertical timing column.
func (c *Code) drawCodewords(data []byte) {
	i := 0
	for right := c.Size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := 0; vert < c.Size; vert++ {
			for j := 0; j < 2; j++ {
				x := right - j
				y := vert
				if (right+1)&2 == 0 {
					y = c.Size - 1 - vert
				}
				if !c.function[y][x] && i < len(data)*8 {
					c.modules[y][x] = data[i>>3]>>(7-i&7)&1 != 0
					i++
				}
			}
		}
	}
}

// maskBit reports whether mask pattern m inverts the module at x, y.
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

// applyMask XORs the data modules with a mask pattern. Applying the same
// mask twice restores them.
func (c *Code) applyMask(m int) {
	for y := 0; y < c.Size; y++ {
		for x := 0; x < c.Size; x++ {
			if !c.function[y][x] && maskBit(m, x, y) {
				c.modules[y][x] = !c.modules[y][x]
			}
		}
	}
}

// penalty scores the symbol by the standard's four rules; the mask with
// the lowest score is the easiest to scan.
func (c *Code) penalty() int {
	const n1, n2, n3, n4 = 3, 3, 40, 10
	score := 0
	at := func(i, j int, rows bool) bool {
		if rows {
			return c.modules[i][j]
		}
		return c.modules[j][i]
	}
	// Rule 1: runs of five or more of one colour. Rule 3: the 1:1:3:1:1
	// finder ratio with four light modules on either side.
	finder := [2][11]bool{
		{true, false, true, true, true, false, true, false, false, false, false},
		{false, false, false, false, true, false, true, true, true, false, true},
	}
	for _, rows := range []bool{true, false} {
		for i := 0; i < c.Size; i++ {
			run := 1
			for j := 1; j <= c.Size; j++ {
				if j < c.Size && at(i, j, rows) == at(i, j-1, rows) {
					run++
					continue
				}
				if run >= 5 {
					score += n1 + run - 5
				}
				run = 1
			}
			for j := 0; j+11 <= c.Size; j++ {
				for _, pat := range finder {
					match := true
					for k := 0; k < 11 && match; k++ {
						match = at(i, j+k, rows) == pat[k]
					}
					if match {
						score += n3
					}
				}
			}
		}
	}
	// Rule 2: 2×2 blocks of one colour.
	dark := 0
	for y := 0; y < c.Size; y++ {
		for x := 0; x < c.Size; x++ {
			m := c.modules[y][x]
			if m {
				dark++
			}
			if x+1 < c.Size && y+1 < c.Size && m == c.modules[y][x+1] && m == c.modules[y+1][x] && m == c.modules[y+1][x+1] {
				score += n2
			}
		}
	}
	// Rule 4: how far the share of dark modules is from half, in 5% steps.
	total := c.Size * c.Size
	score += (abs(dark*20-total*10) / total) * n4
	return score
}

func abs(a int) int {
	if a < 0 {
		return -a
	}
	return a
}
