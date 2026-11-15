package release

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"

	"meridian/trellis/internal/deb"
)

func TestBuiltinKeys(t *testing.T) {
	keys, err := deb.ParsePublicKeys(builtin)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) == 0 {
		t.Fatal("no built-in release key: nodes would refuse every release")
	}
	seen := map[string]bool{}
	for _, k := range keys {
		if seen[k.ID()] {
			t.Errorf("release key %s listed twice", k.ID())
		}
		seen[k.ID()] = true
	}
}

func TestTrustedKeysAddsOperatorKeys(t *testing.T) {
	builtinKeys, _ := deb.ParsePublicKeys(builtin)
	dir := t.TempDir()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	extra := deb.Public(priv, "test")
	if err := os.WriteFile(filepath.Join(dir, "test.pub"), []byte(extra), 0o644); err != nil {
		t.Fatal(err)
	}
	// Not a key file by name: ignored.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	keys, err := TrustedKeys(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != len(builtinKeys)+1 {
		t.Fatalf("got %d keys, want %d", len(keys), len(builtinKeys)+1)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.pub"), []byte("garbage\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := TrustedKeys(dir); err == nil {
		t.Fatal("a damaged key file must be an error, not fewer keys")
	}
}
