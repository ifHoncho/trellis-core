package webd

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// frame builds a client frame: masked, as browsers send them.
func frame(fin bool, op byte, p []byte) []byte {
	b := []byte{op}
	if fin {
		b[0] |= 0x80
	}
	switch n := len(p); {
	case n < 126:
		b = append(b, 0x80|byte(n))
	case n <= 0xffff:
		b = append(b, 0x80|126)
		b = binary.BigEndian.AppendUint16(b, uint16(n))
	default:
		b = append(b, 0x80|127)
		b = binary.BigEndian.AppendUint64(b, uint64(n))
	}
	mask := [4]byte{1, 2, 3, 4}
	b = append(b, mask[:]...)
	for i, v := range p {
		b = append(b, v^mask[i%4])
	}
	return b
}

func TestReadFrames(t *testing.T) {
	big := bytes.Repeat([]byte{7}, 1000)
	in := append(frame(true, opText, []byte(`{"t":"hello"}`)), frame(false, opBinary, big[:600])...)
	in = append(in, frame(true, opCont, big[600:])...)
	c := &wsConn{r: newReader(in), c: nopConn{}}
	op, m, err := c.ReadMessage()
	if err != nil || op != opText || string(m) != `{"t":"hello"}` {
		t.Fatalf("%d %q %v", op, m, err)
	}
	op, m, err = c.ReadMessage()
	if err != nil || op != opBinary || !bytes.Equal(m, big) {
		t.Fatalf("fragmented: %d %d %v", op, len(m), err)
	}
	for name, bad := range map[string][]byte{
		"unmasked":       {0x81, 0x01, 'x'},
		"reserved bits":  append([]byte{0xc1}, frame(true, opText, []byte("x"))[1:]...),
		"too big":        frame(true, opBinary, make([]byte, wsMaxMessage+1)),
		"bad control":    frame(false, opPing, nil),
		"orphan cont":    frame(true, opCont, []byte("x")),
		"unknown opcode": frame(true, 0x3, nil),
		"nested message": append(frame(false, opText, []byte("a")), frame(true, opText, []byte("b"))...),
	} {
		c := &wsConn{r: newReader(bad), c: nopConn{}}
		if _, _, err := c.ReadMessage(); err == nil {
			t.Errorf("%s: read", name)
		}
	}
}

// FuzzWSRead feeds frames from a browser (or anything that reaches the
// UI) to the reader: nothing panics, no message is larger than the limit.
func FuzzWSRead(f *testing.F) {
	f.Add(frame(true, opText, []byte(`{"t":"hello"}`)))
	f.Add(append(frame(false, opBinary, []byte("ab")), frame(true, opCont, []byte("cd"))...))
	f.Add(frame(true, opPing, []byte("p")))
	f.Fuzz(func(t *testing.T, b []byte) {
		c := &wsConn{r: newReader(b), c: nopConn{}}
		for i := 0; i < 8; i++ {
			_, m, err := c.ReadMessage()
			if err != nil {
				return
			}
			if len(m) > wsMaxMessage {
				t.Fatalf("a %d-byte message", len(m))
			}
		}
	})
}
