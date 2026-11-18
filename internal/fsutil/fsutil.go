// Package fsutil writes files so that a power loss leaves either the old
// file or the new one, never a half-written or empty one. A node loses
// power often (batteries, a pulled cable), and an empty identity, profile
// or node configuration would keep it off the mesh, or quietly change who
// it is.
package fsutil

import (
	"os"
	"path/filepath"
	"strings"
)

// WriteFile writes data to path through a temporary file in the same
// directory: the data reaches the disk before the rename, and the rename
// before WriteFile returns. The file gets mode, whatever an earlier file
// had. Readers never see half a file; two writers of the same file use
// temporary files of their own.
func WriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	fail := func(err error) error {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Chmod(mode); err != nil {
		return fail(err)
	}
	if _, err := f.Write(data); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// RemoveStale removes temporary files that WriteFile left in dir when it
// was cut short (a power loss before the rename): names ending ".tmp" and a
// run of digits, and the plain ".tmp" of older releases. Call it only when
// nothing else can be writing to dir. It returns the files removed.
func RemoveStale(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var removed []string
	for _, e := range entries {
		if !e.Type().IsRegular() || !isTemp(e.Name()) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if os.Remove(p) == nil {
			removed = append(removed, p)
		}
	}
	return removed
}

func isTemp(name string) bool {
	i := strings.LastIndex(name, ".tmp")
	if i <= 0 {
		return false
	}
	for _, r := range name[i+len(".tmp"):] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
