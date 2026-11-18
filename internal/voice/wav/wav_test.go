package wav

import (
	"path/filepath"
	"testing"
)

func TestRecordAndRead(t *testing.T) {
	p := filepath.Join(t.TempDir(), "out.wav")
	r, err := NewRecorder(p)
	if err != nil {
		t.Fatal(err)
	}
	r.Write([]int16{1, -1, 2, -2})
	r.Write([]int16{3, -3})
	// Readable before Close: the header is kept current.
	ch, rate, data, err := Read(p)
	if err != nil || ch != 2 || rate != 48000 || len(data) != 6 || data[4] != 3 || data[5] != -3 {
		t.Fatalf("%d %d %v %v", ch, rate, data, err)
	}
	r.Close()
}
