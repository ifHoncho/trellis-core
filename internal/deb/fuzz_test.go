package deb

import (
	"testing"
	"time"
)

// A package arrives from a browser (the web UI's upgrade): read it, and
// check its signature, without panicking whatever it holds.
func FuzzRead(f *testing.F) {
	secret, public, _ := NewKey("fuzz")
	priv, _ := ParseSecret(secret)
	keys, _ := ParsePublicKeys(public)
	if b, err := sample().Bytes(); err == nil {
		f.Add(b)
		if s, err := Sign(b, priv, time.Unix(1_800_000_000, 0)); err == nil {
			f.Add(s)
		}
	}
	f.Add([]byte(arMagic))
	f.Add([]byte(arMagic + "debian-binary   0           0     0     100644  4         `\n2.0\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		info, err := Read(b)
		if err != nil {
			return
		}
		if info.Get("Package") == "" || info.Get("Version") == "" {
			t.Fatal("Read accepted a package without a name or version")
		}
		if _, err := info.Verify(keys); err == nil && !info.Signed() {
			t.Fatal("an unsigned package verified")
		}
		_, _ = Payload(b)
	})
}

func FuzzParseKeys(f *testing.F) {
	_, public, _ := NewKey("fuzz")
	f.Add(public)
	f.Add("trellis-release-key v1 !!!! comment")
	f.Fuzz(func(t *testing.T, s string) {
		keys, err := ParsePublicKeys(s)
		if err != nil {
			return
		}
		for _, k := range keys {
			if len(k.Key) != 32 {
				t.Fatalf("a key of %d bytes was accepted", len(k.Key))
			}
		}
		_, _ = ParseSecret(s)
	})
}
