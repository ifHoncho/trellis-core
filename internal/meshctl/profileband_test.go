package meshctl

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProfileBandKeepsSettings: switching a profile's band and back leaves
// every other setting as it was (talk groups, positions, batman options).
func TestProfileBandKeepsSettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "p.yaml")
	if rc := Main("test", []string{"profile", "new", "--name", "bandtest", "-o", path}, io.Discard, io.Discard); rc != 0 {
		t.Fatal("profile new failed")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Replace(string(data), "batman:\n", "batman:\n  bridge_loop_avoidance: true\n  multicast_fanout: 4\n", 1)
	s += "positions:\n  share: true\nvoice:\n  groups:\n    - {number: 1, name: ops}\n"
	before := filepath.Join(dir, "before.yaml")
	for _, p := range []string{path, before} {
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var errb bytes.Buffer
	for _, args := range [][]string{{"profile", "band", "2g", path}, {"profile", "band", "5g", "--channel", "149", path}} {
		if rc := Main("test", args, io.Discard, &errb); rc != 0 {
			t.Fatalf("%v: %s", args, errb.String())
		}
	}
	var out bytes.Buffer
	if rc := Main("test", []string{"profile", "same", before, path}, &out, &errb); rc != 0 {
		t.Fatalf("after 2g and back: %s", errb.String())
	}
}
