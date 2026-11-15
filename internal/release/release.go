// Package release knows which release keys a node trusts.
package release

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"meridian/trellis/internal/deb"
)

//go:embed trusted.keys
var builtin string

// KeyDir holds keys the node's operator trusts besides the built-in ones:
// files named *.pub, each with one or more public key lines.
const KeyDir = "/etc/trellis/release-keys"

// TrustedKeys returns the built-in keys and those in dir. A file that
// cannot be read as keys is an error: a node must not quietly trust fewer
// keys than its operator installed.
func TrustedKeys(dir string) ([]deb.PublicKey, error) {
	keys, err := deb.ParsePublicKeys(builtin)
	if err != nil {
		return nil, fmt.Errorf("built-in release keys: %w", err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.pub"))
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		more, err := deb.ParsePublicKeys(string(b))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		keys = append(keys, more...)
	}
	return keys, nil
}
