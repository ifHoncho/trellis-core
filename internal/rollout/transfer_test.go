package rollout

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// serve starts Handler on a test server, returning its address (host only:
// Fetch adds Port, so the test server's port is patched in by a transport).
func serve(t *testing.T, dir string, wanted func(string) bool, allowed func(net.IP) bool) *http.Client {
	srv := httptest.NewServer(Handler(dir, wanted, allowed))
	t.Cleanup(srv.Close)
	// Fetch dials addr:Port; send it to the test server instead.
	tr := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
	}}
	return &http.Client{Transport: tr}
}

func TestTransfer(t *testing.T) {
	origin, node := t.TempDir(), t.TempDir()
	pkg := []byte(strings.Repeat("package bytes ", 1000))
	p, err := Keep(origin, "arm64", pkg)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := Keep(origin, "armhf", []byte("not in the rollout"))
	inRollout := func(sha string) bool { return sha == p.SHA256 }
	allow := true
	client := serve(t, origin, inRollout, func(net.IP) bool { return allow })

	path, err := Fetch(context.Background(), client, "10.73.1.1", p, node)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != string(pkg) {
		t.Fatal("fetched bytes differ")
	}
	// Already there: kept without asking again.
	if _, err := Fetch(context.Background(), &http.Client{Transport: failing{}}, "10.73.1.1", p, node); err != nil {
		t.Errorf("a package already kept was fetched again: %v", err)
	}
	// Not part of the rollout: not served.
	if _, err := Fetch(context.Background(), client, "10.73.1.1", other, t.TempDir()); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("a package outside the rollout: %v", err)
	}
	// Not a node: refused.
	allow = false
	if _, err := Fetch(context.Background(), client, "10.73.1.1", p, t.TempDir()); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("from a stranger: %v", err)
	}
	allow = true
	// A hash that the bytes do not match.
	bad := p
	bad.SHA256 = other.SHA256
	bad.Size = p.Size
	client2 := serve(t, origin, func(string) bool { return true }, func(net.IP) bool { return true })
	if err := os.WriteFile(File(origin, other.SHA256), pkg, 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, err := Fetch(context.Background(), client2, "10.73.1.1", bad, dir); !errors.Is(err, ErrHash) {
		t.Errorf("a mismatched package: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*")); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
	Prune(origin, inRollout)
	if left, _ := filepath.Glob(filepath.Join(origin, "*.deb")); len(left) != 1 {
		t.Errorf("after prune: %v", left)
	}
}

type failing struct{}

func (failing) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("no request expected")
}

// TestPruneLeftovers: a fetch or keep cut short by a power loss leaves its
// temporary file; Prune removes it once it is old, not while a transfer
// may still be writing it.
func TestPruneLeftovers(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, ".fetch-123")
	fresh := filepath.Join(dir, ".fetch-456")
	keep := filepath.Join(dir, ".keep-789")
	for _, p := range []string{old, fresh, keep} {
		if err := os.WriteFile(p, []byte("part of a package"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(keep, past, past); err != nil {
		t.Fatal(err)
	}
	Prune(dir, func(string) bool { return true })
	for p, want := range map[string]bool{old: false, keep: false, fresh: true} {
		if _, err := os.Stat(p); (err == nil) != want {
			t.Errorf("%s: present %v, want %v", filepath.Base(p), err == nil, want)
		}
	}
}
