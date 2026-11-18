package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "f.yaml")
	if err := WriteFile(path, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A leftover wider file of the same name must not lend its mode.
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	fi, _ := os.Stat(path)
	if string(b) != "two" || fi.Mode().Perm() != 0o600 {
		t.Errorf("got %q mode %v", b, fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("temporary files left: %v", entries)
	}
}

func TestRemoveStale(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"new.deb.tmp123456", "identity.yaml.tmp", "identity.yaml", "notes.tmpl", "a.tmp9x"} {
		os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600)
	}
	if err := WriteFile(filepath.Join(dir, "identity.yaml"), []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := RemoveStale(dir); len(got) != 2 {
		t.Errorf("removed %v", got)
	}
	left, _ := os.ReadDir(dir)
	var names []string
	for _, e := range left {
		names = append(names, e.Name())
	}
	if len(names) != 3 {
		t.Errorf("left %v", names)
	}
}
