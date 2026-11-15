package regdb

import (
	"os"
	"testing"
)

// regulatory.db is read from wherever the distribution put it, by a helper
// udev runs as root (trellis crda): a damaged file must be an error.
func FuzzParse(f *testing.F) {
	if b, err := os.ReadFile(testDB); err == nil {
		f.Add(b)
	}
	f.Add([]byte("RGDB"))
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = Parse(b)
	})
}
