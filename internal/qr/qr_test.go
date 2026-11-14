package qr

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// Known capacities from the standard's tables (data codewords, and bytes
// in byte mode).
func TestCapacityTables(t *testing.T) {
	codewords := []struct {
		ver  int
		lvl  Level
		want int
	}{
		{1, L, 19}, {1, M, 16}, {1, Q, 13}, {1, H, 9}, {2, M, 28}, {5, M, 86}, {7, M, 124}, {10, M, 216},
		{10, L, 274}, {14, Q, 261}, {27, H, 628}, {40, L, 2956}, {40, M, 2334}, {40, Q, 1666}, {40, H, 1276},
	}
	for _, c := range codewords {
		if got := dataCodewords(c.ver, c.lvl); got != c.want {
			t.Errorf("version %d level %d: %d data codewords, want %d", c.ver, c.lvl, got, c.want)
		}
	}
	for _, c := range []struct {
		ver  int
		lvl  Level
		want int
	}{{1, L, 17}, {1, H, 7}, {9, L, 230}, {10, L, 271}, {10, M, 213}, {40, L, 2953}, {40, H, 1273}} {
		if got := Capacity(c.ver, c.lvl); got != c.want {
			t.Errorf("version %d level %d: capacity %d bytes, want %d", c.ver, c.lvl, got, c.want)
		}
	}
	// Every version: the blocks divide the codewords as the standard says.
	for lvl := L; lvl <= H; lvl++ {
		for ver := 1; ver <= 40; ver++ {
			raw, blocks, ecc := rawModules(ver)/8, int(numBlocks[lvl][ver]), int(eccPerBlock[lvl][ver])
			if raw/blocks <= ecc || blocks*ecc >= raw {
				t.Errorf("version %d level %d: %d codewords cannot hold %d blocks of %d", ver, lvl, raw, blocks, ecc)
			}
		}
	}
}

func TestTooLong(t *testing.T) {
	if _, err := Encode(strings.Repeat("x", 2954), L); !errors.Is(err, ErrTooLong) {
		t.Errorf("2954 bytes at L: %v", err)
	}
	if c, err := Encode(strings.Repeat("x", 2953), L); err != nil || c.Version != 40 {
		t.Errorf("2953 bytes at L: %v", err)
	}
}

// decode reads a symbol back the way a scanner does after sampling: format
// information, unmasking, the codeword zigzag, de-interleaving, a
// Reed–Solomon syndrome check on every block, then the byte segment. It
// shares only the tables with the encoder.
func decode(c *Code) (string, error) {
	size := c.Size
	if size != c.Version*4+17 {
		return "", fmt.Errorf("size %d for version %d", size, c.Version)
	}
	// Format information, first copy, read in the standard's bit order.
	var fb int
	coords := [15][2]int{{8, 0}, {8, 1}, {8, 2}, {8, 3}, {8, 4}, {8, 5}, {8, 7}, {8, 8}, {7, 8}, {5, 8}, {4, 8}, {3, 8}, {2, 8}, {1, 8}, {0, 8}}
	for i, xy := range coords {
		if c.Dark(xy[0], xy[1]) {
			fb |= 1 << i
		}
	}
	// Second copy must agree.
	for i := 0; i < 15; i++ {
		x, y := size-1-i, 8
		if i >= 8 {
			x, y = 8, size-15+i
		}
		if c.Dark(x, y) != (fb>>i&1 != 0) {
			return "", fmt.Errorf("format copies differ at bit %d", i)
		}
	}
	fb ^= 0x5412
	// A valid (15,5) BCH codeword leaves no remainder modulo 0x537.
	rem := fb
	for i := 14; i >= 10; i-- {
		if rem>>i&1 != 0 {
			rem ^= 0x537 << (i - 10)
		}
	}
	if rem != 0 {
		return "", fmt.Errorf("format information fails its BCH check (remainder %#x)", rem)
	}
	lvl := map[int]Level{1: L, 0: M, 3: Q, 2: H}[fb>>13]
	mask := fb >> 10 & 7
	if lvl != c.Level || mask != c.Mask {
		return "", fmt.Errorf("format says level %d mask %d, symbol was made with %d and %d", lvl, mask, c.Level, c.Mask)
	}
	if !c.Dark(8, size-8) {
		return "", errors.New("the dark module is light")
	}

	// Which modules are function patterns, worked out from the version.
	fn := make([][]bool, size)
	for i := range fn {
		fn[i] = make([]bool, size)
	}
	mark := func(x0, y0, x1, y1 int) {
		for y := max(y0, 0); y <= min(y1, size-1); y++ {
			for x := max(x0, 0); x <= min(x1, size-1); x++ {
				fn[y][x] = true
			}
		}
	}
	mark(0, 0, 8, 8)
	mark(size-8, 0, size-1, 8)
	mark(0, size-8, 8, size-1)
	mark(6, 0, 6, size-1)
	mark(0, 6, size-1, 6)
	pos := alignmentPositions(c.Version)
	for i, y := range pos {
		for j, x := range pos {
			if (i == 0 && j == 0) || (i == 0 && j == len(pos)-1) || (i == len(pos)-1 && j == 0) {
				continue
			}
			mark(x-2, y-2, x+2, y+2)
		}
	}
	if c.Version >= 7 {
		mark(size-11, 0, size-9, 5)
		mark(0, size-11, 5, size-9)
	}
	free := 0
	for y := range fn {
		for x := range fn[y] {
			if !fn[y][x] {
				free++
			}
		}
	}
	if free != rawModules(c.Version) {
		return "", fmt.Errorf("%d data modules, the standard says %d", free, rawModules(c.Version))
	}

	// Codewords in zigzag order, unmasked.
	raw := rawModules(c.Version) / 8
	cw := make([]byte, raw)
	n := 0
	up := true
	for right := size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right--
		}
		for k := 0; k < size; k++ {
			y := k
			if up {
				y = size - 1 - k
			}
			for _, x := range []int{right, right - 1} {
				if fn[y][x] || n >= raw*8 {
					continue
				}
				if c.Dark(x, y) != maskBit(mask, x, y) {
					cw[n/8] |= 0x80 >> (n % 8)
				}
				n++
			}
		}
		up = !up
	}

	// De-interleave into blocks and check each block's syndromes.
	blocks, ecc := int(numBlocks[lvl][c.Version]), int(eccPerBlock[lvl][c.Version])
	short, shortData := blocks-raw%blocks, raw/blocks-ecc
	data := make([][]byte, blocks)
	check := make([][]byte, blocks)
	k := 0
	for i := 0; i <= shortData; i++ {
		for b := 0; b < blocks; b++ {
			if i == shortData && b < short {
				continue
			}
			data[b] = append(data[b], cw[k])
			k++
		}
	}
	for i := 0; i < ecc; i++ {
		for b := 0; b < blocks; b++ {
			check[b] = append(check[b], cw[k])
			k++
		}
	}
	var stream []byte
	for b := 0; b < blocks; b++ {
		word := append(append([]byte{}, data[b]...), check[b]...)
		root := byte(1)
		for s := 0; s < ecc; s++ {
			var v byte
			for _, x := range word {
				v = gfMul(v, root) ^ x
			}
			if v != 0 {
				return "", fmt.Errorf("block %d: syndrome %d is %#x, not zero", b, s, v)
			}
			root = gfMul(root, 2)
		}
		stream = append(stream, data[b]...)
	}

	// One byte-mode segment.
	bit := 0
	read := func(count int) int {
		v := 0
		for i := 0; i < count; i++ {
			v = v<<1 | int(stream[bit/8]>>(7-bit%8)&1)
			bit++
		}
		return v
	}
	if mode := read(4); mode != 4 {
		return "", fmt.Errorf("mode %d, want byte mode", mode)
	}
	length := read(countBits(c.Version))
	out := make([]byte, length)
	for i := range out {
		out[i] = byte(read(8))
	}
	return string(out), nil
}

func TestRoundTrip(t *testing.T) {
	texts := []string{
		"", "A", "HELLO WORLD", "WIFI:T:WPA;S:node-1;P:abcd2345efgh;;",
		"TRP1:" + strings.Repeat("q7Zk-_2Bx", 34),
		strings.Repeat("The quick brown fox jumps over the lazy dog. ", 20),
		strings.Repeat("\x00\xff\x55\xaa", 300),
		strings.Repeat("z", 2331), // version 40 at M
	}
	for _, text := range texts {
		for lvl := L; lvl <= H; lvl++ {
			c, err := Encode(text, lvl)
			if err != nil {
				if errors.Is(err, ErrTooLong) && len(text) > Capacity(40, lvl) {
					continue
				}
				t.Errorf("%d bytes at level %d: %v", len(text), lvl, err)
				continue
			}
			if len(text) > Capacity(c.Version, lvl) || (c.Version > 1 && len(text) <= Capacity(c.Version-1, lvl)) {
				t.Errorf("%d bytes at level %d: version %d is not the smallest that fits", len(text), lvl, c.Version)
			}
			got, err := decode(c)
			if err != nil {
				t.Errorf("%d bytes at level %d (version %d, mask %d): %v", len(text), lvl, c.Version, c.Mask, err)
			} else if got != text {
				t.Errorf("%d bytes at level %d: decoded %q", len(text), lvl, got)
			}
		}
	}
	// Every version once, every mask once.
	for ver := 1; ver <= 40; ver++ {
		text := strings.Repeat("m", Capacity(ver, M))
		c, err := encode([]byte(text), M, ver%8)
		if err != nil || c.Version != ver {
			t.Fatalf("version %d: %v", ver, err)
		}
		if got, err := decode(c); err != nil || got != text {
			t.Errorf("version %d mask %d: %v", ver, ver%8, err)
		}
	}
}

// TestAgainstLibqrencode compares whole symbols with libqrencode, an
// independent implementation, where that library is installed (it is
// loaded through Python's ctypes). The two may choose different masks,
// so the comparison is with this encoder forced to libqrencode's choice.
func TestAgainstLibqrencode(t *testing.T) {
	const script = `
import ctypes, ctypes.util, sys
name = ctypes.util.find_library("qrencode") or "libqrencode.so.4"
try:
    lib = ctypes.CDLL(name)
except OSError:
    sys.exit(3)
class QR(ctypes.Structure):
    _fields_ = [("version", ctypes.c_int), ("width", ctypes.c_int), ("data", ctypes.POINTER(ctypes.c_ubyte))]
lib.QRcode_encodeData.restype = ctypes.POINTER(QR)
lib.QRcode_encodeData.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_int]
data = bytes.fromhex(sys.argv[1])
q = lib.QRcode_encodeData(len(data), data, 0, int(sys.argv[2]))
if not q:
    sys.exit(4)
w = q.contents.width
print(q.contents.version)
for y in range(w):
    print("".join("1" if q.contents.data[y * w + x] & 1 else "0" for x in range(w)))
`
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	texts := []string{
		"A", "HELLO WORLD", "WIFI:T:WPA;S:alpha-5;P:k3m9w2xp7fhq;;",
		"TRP1:" + strings.Repeat("q7Zk-_2Bx", 34),
		strings.Repeat("Trellis mesh node. ", 60),
		strings.Repeat("\x01\xfe", 700),
	}
	compared := 0
	for _, text := range texts {
		for lvl := L; lvl <= H; lvl++ {
			out, err := exec.Command(py, "-I", "-c", script, fmt.Sprintf("%x", text), fmt.Sprint(int(lvl))).Output()
			var ee *exec.ExitError
			if errors.As(err, &ee) && ee.ExitCode() == 3 {
				t.Skip("libqrencode not installed")
			}
			if errors.As(err, &ee) && ee.ExitCode() == 4 && len(text) > Capacity(40, lvl) {
				continue
			}
			if err != nil {
				t.Fatalf("libqrencode helper: %v", err)
			}
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			want := lines[1:]
			match := false
			var ours *Code
			for mask := 0; mask < 8 && !match; mask++ {
				ours, err = encode([]byte(text), lvl, mask)
				if err != nil {
					t.Fatal(err)
				}
				match = bytes.Equal([]byte(strings.Join(ours.Rows(), "\n")), []byte(strings.Join(want, "\n")))
			}
			if !match {
				t.Errorf("%d bytes at level %d: no mask gives libqrencode's symbol (its version %s, ours %d)", len(text), lvl, lines[0], ours.Version)
			}
			compared++
		}
	}
	t.Logf("%d symbols identical to libqrencode's", compared)
}

func TestRenderings(t *testing.T) {
	c, err := Encode("WIFI:T:WPA;S:n;P:12345678;;", M)
	if err != nil {
		t.Fatal(err)
	}
	rows := c.Rows()
	if len(rows) != c.Size || len(rows[0]) != c.Size || !strings.HasPrefix(rows[0], "1111111") {
		t.Errorf("rows: %d of %d, first %q", len(rows), c.Size, rows[0])
	}
	svg := c.SVG()
	if !strings.HasPrefix(svg, "<svg ") || !strings.Contains(svg, fmt.Sprintf(`viewBox="0 0 %d %d"`, c.Size+8, c.Size+8)) {
		t.Errorf("svg: %.120s", svg)
	}
	term := c.Terminal(false)
	if n := strings.Count(term, "\n"); n != (c.Size+4+1)/2 {
		t.Errorf("terminal rendering has %d lines for size %d", n, c.Size)
	}
}
