package halowfiles

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sum(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func writePins(t *testing.T, fw, bcf string) string {
	p := filepath.Join(t.TempDir(), "firmware.pins")
	os.WriteFile(p, []byte("# pins\nFW_REPO=https://github.com/MorseMicro/morse-firmware.git\nFW_REF=abc\n"+
		"FW_FILE=firmware/mm6108.bin\nFW_SHA256="+sum(fw)+"\nBCF_FILE=bcf/quectel/bcf_x.bin\nBCF_SHA256="+sum(bcf)+"\nLICENSE_FILE=firmware/LICENSE\n"), 0o644)
	return p
}

// The repository's own pins parse.
func TestRealPins(t *testing.T) {
	p, err := LoadPins("../../packaging/dkms-morse/firmware.pins")
	if err != nil || len(p.Files) != 2 || p.Files[0].Name != "mm6108.bin" || p.Files[1].Name != "bcf_fgh100mhaamd.bin" {
		t.Fatalf("%+v %v", p, err)
	}
	if u := p.UpstreamURL(p.Files[0].Path); !strings.HasPrefix(u, "https://raw.githubusercontent.com/MorseMicro/morse-firmware/"+p.Ref+"/firmware/mm6108.bin") {
		t.Fatal(u)
	}
}

func TestMissingAndModule(t *testing.T) {
	p, err := LoadPins(writePins(t, "fw", "bcf"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if len(p.Missing(dir)) != 2 {
		t.Fatal("empty dir")
	}
	os.WriteFile(filepath.Join(dir, "mm6108.bin"), []byte("fw"), 0o644)
	os.WriteFile(filepath.Join(dir, "bcf_x.bin"), []byte("tampered"), 0o644)
	if m := p.Missing(dir); len(m) != 1 || m[0].Name != "bcf_x.bin" {
		t.Fatalf("missing %+v", m)
	}
	// A file with another hash is there: missing from the pinned set, not
	// absent, so never fetched over.
	if a := p.Absent(dir); len(a) != 0 {
		t.Fatalf("absent %+v", a)
	}
	os.Remove(filepath.Join(dir, "mm6108.bin"))
	if a := p.Absent(dir); len(a) != 1 || a[0].Name != "mm6108.bin" {
		t.Fatalf("absent %+v", a)
	}
	root := t.TempDir()
	if ModulePresent(root) {
		t.Fatal("no module")
	}
	os.MkdirAll(filepath.Join(root, "proc/device-tree/soc/spi@7e204000/mm6108@0"), 0o755)
	if !ModulePresent(root) {
		t.Fatal("module not seen")
	}
}

func TestHandlerAndGet(t *testing.T) {
	p, _ := LoadPins(writePins(t, "fw", "bcf"))
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "mm6108.bin"), []byte("fw"), 0o644)
	os.WriteFile(filepath.Join(dir, "bcf_x.bin"), []byte("tampered"), 0o644)
	os.WriteFile(filepath.Join(dir, "LICENSE"), []byte("licence"), 0o644)
	os.WriteFile(filepath.Join(dir, "other"), []byte("x"), 0o644)
	allow := true
	srv := httptest.NewServer(Handler(func() *Pins { return p }, dir, func(net.IP) bool { return allow }))
	defer srv.Close()
	ctx := context.Background()
	out := t.TempDir()
	if path, err := Get(ctx, srv.Client(), srv.URL+"/halow/"+sum("fw"), out, "mm6108.bin", sum("fw")); err != nil || path == "" {
		t.Fatalf("firmware: %v", err)
	}
	// A file that is not the pinned one is never passed on.
	if _, err := Get(ctx, srv.Client(), srv.URL+"/halow/"+sum("bcf"), out, "bcf_x.bin", sum("bcf")); err == nil {
		t.Fatal("a tampered BCF was served")
	}
	if _, err := Get(ctx, srv.Client(), srv.URL+"/halow/LICENSE", out, "LICENSE", ""); err != nil {
		t.Fatalf("licence: %v", err)
	}
	for _, path := range []string{"/halow/other", "/halow/../other", "/halow/" + sum("x"), "/release/" + sum("fw")} {
		resp, err := srv.Client().Get(srv.URL + path)
		if err != nil || resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %v %v", path, resp.Status, err)
		}
	}
	allow = false
	if resp, _ := srv.Client().Get(srv.URL + "/halow/" + sum("fw")); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("not a mesh node: %s", resp.Status)
	}
	// What arrives is checked on this side too.
	allow = true
	if _, err := Get(ctx, srv.Client(), srv.URL+"/halow/"+sum("fw"), out, "mm6108.bin", sum("other")); err == nil {
		t.Fatal("a file with the wrong hash was kept")
	}
}

// The install puts in only what the node has no file for: a file under a
// pinned name stays, whatever it holds.
func TestInstallKeepsWhatIsThere(t *testing.T) {
	p, err := LoadPins(writePins(t, "fw", "bcf"))
	if err != nil {
		t.Fatal(err)
	}
	from, dir := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(from, "mm6108.bin"), []byte("fw"), 0o644)
	os.WriteFile(filepath.Join(from, "bcf_x.bin"), []byte("bcf"), 0o644)
	os.WriteFile(filepath.Join(dir, "bcf_x.bin"), []byte("the operator's own"), 0o644)
	if err := installFiles(p, from, dir, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "bcf_x.bin")); string(got) != "the operator's own" {
		t.Fatalf("the BCF is now %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "mm6108.bin")); string(got) != "fw" {
		t.Fatalf("firmware %q", got)
	}
	// A fetched file that is not the pinned one is refused.
	os.WriteFile(filepath.Join(from, "mm6108.bin"), []byte("other"), 0o644)
	os.Remove(filepath.Join(dir, "mm6108.bin"))
	if err := installFiles(p, from, dir, io.Discard); err == nil {
		t.Fatal("installed a file that is not the pinned one")
	}
}
