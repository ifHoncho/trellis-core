package wire

import (
	"bytes"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	Write(&buf, Text, []byte(`{"t":"hello"}`))
	Write(&buf, Binary, make([]byte, 640))
	k, b, err := Read(&buf)
	if err != nil || k != Text || string(b) != `{"t":"hello"}` {
		t.Fatalf("%d %q %v", k, b, err)
	}
	k, b, err = Read(&buf)
	if err != nil || k != Binary || len(b) != 640 {
		t.Fatalf("%d %d %v", k, len(b), err)
	}
	if _, _, err := Read(bytes.NewReader([]byte{9, 0, 0, 0, 1, 0})); err == nil {
		t.Error("unknown kind read")
	}
	if _, _, err := Read(bytes.NewReader([]byte{1, 0xff, 0, 0, 0})); err == nil {
		t.Error("oversized read")
	}
}
