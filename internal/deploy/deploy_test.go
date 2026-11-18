package deploy

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"meridian/trellis/internal/deb"
)

func TestParseHosts(t *testing.T) {
	hs, err := ParseHosts(strings.NewReader(`
# the bench
pi@192.0.2.11
pi@192.0.2.12  callsign=alpha-2 id=12   # with settings
192.0.2.13 mesh=10.73.13.1
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(hs) != 3 || hs[0].User != "pi" || hs[0].Addr != "192.0.2.11" || hs[1].Callsign != "alpha-2" || hs[1].NodeID != 12 ||
		hs[2].User != "" || hs[2].Mesh != "10.73.13.1" || hs[1].Line != 4 {
		t.Fatalf("%+v", hs)
	}
	// The same address on two ports is two nodes.
	if hs, err := ParseHosts(strings.NewReader("op@127.0.0.1 port=2201\nop@127.0.0.1 port=2202\n")); err != nil || hs[1].Port != 2202 || hs[1].Key() != "127.0.0.1:2202" {
		t.Errorf("ports: %+v %v", hs, err)
	}
	for _, bad := range []string{
		"a@192.0.2.1 port=0",
		"a@192.0.2.1 port=2201\nb@192.0.2.1 port=2201",
		"pi@192.0.2.1 pi@192.0.2.1",
		"pi@192.0.2.1\nubuntu@192.0.2.1",
		"a@192.0.2.1 callsign=x\nb@192.0.2.2 callsign=x",
		"a@192.0.2.1 id=3\nb@192.0.2.2 id=3",
		"a@192.0.2.1 id=255",
		"a@192.0.2.1 callsign=Bad_Name",
		"a@192.0.2.1 colour=red",
		"a@192.0.2.1 mesh=x@y",
		"a;rm@192.0.2.1",
		"# nothing",
	} {
		if _, err := ParseHosts(strings.NewReader(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestCallsignFrom(t *testing.T) {
	for in, want := range map[string]string{"Pi-Node_1.local": "pi-node-1-local", "--x--": "x", "": "node", "ünï": "n"} {
		if got := CallsignFrom(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestQuote(t *testing.T) {
	for in, want := range map[string]string{"alpha-1": "alpha-1", "a b": "'a b'", "it's": `'it'\''s'`, "": "''", "$(x)": "'$(x)'"} {
		if got := quote(in); got != want {
			t.Errorf("%q: %s, want %s", in, got, want)
		}
	}
}

// release writes a release to dir: trellis for the architectures, the
// batman-adv package, SHA256SUMS; signed with priv unless nil.
func release(t *testing.T, dir string, priv ed25519.PrivateKey, version string, arches ...string) {
	t.Helper()
	write := func(name, arch, ver string, extra ...deb.Field) {
		p := &deb.Package{Control: append([]deb.Field{{Name: "Package", Value: name}, {Name: "Version", Value: ver},
			{Name: "Architecture", Value: arch}, {Name: "Maintainer", Value: "test"}, {Name: "Description", Value: "test"}}, extra...),
			Files: []deb.File{{Path: "/usr/share/doc/" + name + "/x", Mode: 0o644, Body: []byte(arch)}}, Time: time.Unix(1, 0)}
		b, err := p.Bytes()
		if err != nil {
			t.Fatal(err)
		}
		if priv != nil {
			if b, err = deb.Sign(b, priv, p.Time); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, p.Filename()), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range arches {
		write("trellis", a, version, deb.Field{Name: "Trellis-Version", Value: version})
	}
	write("trellis-batman-adv-dkms", "all", "2026.3-1")
	write("trellis-voice", "arm64", version)
	writeSums(t, dir)
}

func writeSums(t *testing.T, dir string) {
	files, _ := filepath.Glob(filepath.Join(dir, "*.deb"))
	var b strings.Builder
	for _, f := range files {
		data, _ := os.ReadFile(f)
		sum := sha256.Sum256(data)
		fmt.Fprintf(&b, "%s  %s\n", hex.EncodeToString(sum[:]), filepath.Base(f))
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func testKey(t *testing.T) (ed25519.PrivateKey, []deb.PublicKey) {
	_, priv, _ := ed25519.GenerateKey(nil)
	keys, err := deb.ParsePublicKeys(deb.Public(priv, "1"))
	if err != nil {
		t.Fatal(err)
	}
	return priv, keys
}

func TestLoadRelease(t *testing.T) {
	priv, keys := testKey(t)
	dir := t.TempDir()
	release(t, dir, priv, "0.2.0", "arm64", "amd64")
	r, err := LoadRelease(dir, keys, true)
	if err != nil {
		t.Fatal(err)
	}
	if r.Version != "0.2.0" || len(r.Arches) != 2 || r.Batman == "" || r.BatmanVersion != "2026.3-1" || r.SignedBy != "1" || !r.Checksums {
		t.Fatalf("%+v", r)
	}

	// A key the program does not trust.
	_, other := testKey(t)
	if _, err := LoadRelease(dir, other, true); err == nil {
		t.Error("a release signed by an unknown key was accepted")
	}
	// A package changed after the checksums.
	f := r.Arches["arm64"]
	b, _ := os.ReadFile(f)
	b[len(b)/2] ^= 1
	os.WriteFile(f, b, 0o644)
	if _, err := LoadRelease(dir, keys, true); err == nil || !strings.Contains(err.Error(), "SHA256SUMS") {
		t.Errorf("a damaged package: %v", err)
	}
	// Unsigned.
	dir = t.TempDir()
	release(t, dir, nil, "0.2.0", "arm64")
	if _, err := LoadRelease(dir, keys, true); err == nil {
		t.Error("an unsigned release was accepted")
	}
	// Two versions, and no checksums where they are required.
	dir = t.TempDir()
	release(t, dir, priv, "0.2.0", "arm64")
	release(t, dir, priv, "0.3.0", "amd64")
	if _, err := LoadRelease(dir, keys, true); err == nil || !strings.Contains(err.Error(), "one release at a time") {
		t.Errorf("mixed: %v", err)
	}
	dir = t.TempDir()
	release(t, dir, priv, "0.2.0", "arm64")
	os.Remove(filepath.Join(dir, "SHA256SUMS"))
	if _, err := LoadRelease(dir, keys, true); err == nil {
		t.Error("no SHA256SUMS where required")
	}
	if _, err := LoadRelease(dir, keys, false); err != nil {
		t.Errorf("no SHA256SUMS, not required: %v", err)
	}
}

func TestFetch(t *testing.T) {
	priv, keys := testKey(t)
	src := t.TempDir()
	release(t, src, priv, "0.2.0", "arm64")
	var assets []ghAsset
	files, _ := os.ReadDir(src)
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	for _, f := range files {
		st, _ := os.Stat(filepath.Join(src, f.Name()))
		assets = append(assets, ghAsset{Name: f.Name(), URL: srv.URL + "/dl/" + f.Name(), Size: st.Size()})
	}
	assets = append(assets, ghAsset{Name: "trellis-deploy_0.2.0_darwin_arm64", URL: srv.URL + "/dl/nope", Size: 10})
	rels := []ghRelease{
		{Tag: "v0.3.0", Draft: true},
		{Tag: "v0.2.0", Prerelease: true, Assets: assets},
		{Tag: "v0.1.0"},
	}
	mux.HandleFunc("/repos/o/r/releases", func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(rels) })
	mux.Handle("/dl/", http.StripPrefix("/dl/", http.FileServer(http.Dir(src))))
	f := Fetcher{API: srv.URL, Repo: "o/r"}
	cache := t.TempDir()
	dir, err := f.Fetch(context.Background(), "", cache, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(dir) != "0.2.0" {
		t.Errorf("the newest that is not a draft: %s", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "trellis-deploy_0.2.0_darwin_arm64")); err == nil {
		t.Error("the deploy tool's own builds are not needed")
	}
	if r, err := LoadRelease(dir, keys, true); err != nil || r.Version != "0.2.0" {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := f.Fetch(context.Background(), "0.9.0", cache, io.Discard); err == nil {
		t.Error("a release that does not exist")
	}
}

func TestPlan(t *testing.T) {
	priv, keys := testKey(t)
	dir := t.TempDir()
	release(t, dir, priv, "0.2.0", "arm64", "armhf")
	rel, err := LoadRelease(dir, keys, true)
	if err != nil {
		t.Fatal(err)
	}
	hosts, _ := ParseHosts(strings.NewReader("a@192.0.2.1\nb@192.0.2.2 callsign=bravo\nc@192.0.2.3\nd@192.0.2.4 mesh=10.73.4.1\ne@192.0.2.5\nf@192.0.2.6\ng@192.0.2.7\n"))
	st := func(s string) State { return ParseState(s) }
	states := []State{
		st("arch=arm64\nos=Debian 13\nhostname=Node_A\napt=yes\nsudo=nopass"), // new, no profile
		st("arch=arm64\nos=Debian 13\nhostname=x\napt=yes\nprofile=yes"),      // new, has a profile
		st("arch=arm64\napt=yes\ntrellis=0.1.0\ncmp=older\nbatman=2026.3-1"),  // upgrade, direct
		st("arch=armhf\napt=yes\ntrellis=0.1.0\ncmp=older"),                   // upgrade, via the mesh
		st("arch=arm64\napt=yes\ntrellis=0.2.0\ncmp=same"),                    // current
		st("arch=amd64\napt=yes"),                                             // no package
		st("arch=arm64\napt=yes\ntrellis=0.3.0~dev\ncmp=newer"),               // newer
	}
	for i := range states {
		states[i].Reachable = true
		states[i].Route = Route{Target: hosts[i].Target}
	}
	states[3].Route = Route{Target: "d@10.73.4.1", Via: "a@192.0.2.1", Alias: "192.0.2.4"}
	steps := Plan(hosts, states, rel, PlanOptions{})
	got := map[string]Step{}
	var order []string
	for _, s := range steps {
		got[s.Host.Addr] = s
		order = append(order, s.Host.Addr)
	}
	want := map[string]Action{"192.0.2.1": Install, "192.0.2.2": Install, "192.0.2.3": Upgrade, "192.0.2.4": Upgrade,
		"192.0.2.5": Current, "192.0.2.6": Skip, "192.0.2.7": Skip}
	for a, act := range want {
		if got[a].Action != act {
			t.Errorf("%s: %v (%s), want %v", a, got[a].Action, got[a].Why, act)
		}
	}
	if got["192.0.2.1"].Callsign != "node-a" || !strings.Contains(got["192.0.2.1"].Why, "setup mode") || got["192.0.2.2"].Callsign != "bravo" {
		t.Errorf("callsigns: %+v %+v", got["192.0.2.1"], got["192.0.2.2"])
	}
	if strings.Join(order[:4], " ") != "192.0.2.1 192.0.2.2 192.0.2.4 192.0.2.3" {
		t.Errorf("order: %v (installs, then the node through the mesh, then the direct one)", order)
	}
	if !strings.Contains(got["192.0.2.6"].Why, "no amd64 package") {
		t.Errorf("why: %s", got["192.0.2.6"].Why)
	}
	steps = Plan(hosts, states, rel, PlanOptions{Profile: true, Force: true})
	for _, s := range steps {
		if s.Host.Addr == "192.0.2.5" && s.Action != Upgrade {
			t.Error("--force reinstalls the current node")
		}
		if s.Host.Addr == "192.0.2.1" && !strings.Contains(s.Why, "callsign node-a") {
			t.Errorf("with a profile: %s", s.Why)
		}
	}
	unreach := []State{{Err: "ssh: connect to host 192.0.2.1 port 22: Connection timed out"}}
	if s := Plan(hosts[:1], unreach, rel, PlanOptions{}); s[0].Action != Skip || !strings.Contains(s[0].Why, "timed out") {
		t.Errorf("unreachable: %+v", s[0])
	}
}

// Nodes that share a hostname are told apart by their address.
func TestLabels(t *testing.T) {
	steps := []Step{
		{Host: Host{Addr: "192.0.2.1"}, State: State{Hostname: "pi"}},
		{Host: Host{Addr: "192.0.2.2"}, State: State{Hostname: "pi"}},
		{Host: Host{Addr: "192.0.2.3", Callsign: "alpha"}, State: State{Hostname: "pi"}},
		{Host: Host{Addr: "192.0.2.4"}},
	}
	label(steps)
	want := []string{"pi@192.0.2.1", "pi@192.0.2.2", "alpha", "192.0.2.4"}
	for i, s := range steps {
		if s.Name() != want[i] {
			t.Errorf("%d: %q, want %q", i, s.Name(), want[i])
		}
	}
}

func TestSudoRule(t *testing.T) {
	if r, err := sudoRule("pi"); err != nil || r != "pi ALL=(ALL) NOPASSWD: ALL" {
		t.Errorf("%q %v", r, err)
	}
	for _, bad := range []string{"", "root ALL", "a;b", "Pi"} {
		if _, err := sudoRule(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestFirstLine(t *testing.T) {
	if got := firstLine("", nil); got != "" {
		t.Errorf("no output, no error: %q", got)
	}
	if got := firstLine("Warning: Permanently added '192.0.2.1' to the list of known hosts.\nop@192.0.2.1: Permission denied (publickey).\n", nil); !strings.Contains(got, "Permission denied") {
		t.Errorf("ssh's warning taken for the answer: %q", got)
	}
}

// The operator's ssh options come first, so they win over the defaults.
func TestSSHOptionsFirst(t *testing.T) {
	s := &SSH{Binary: "ssh", Options: []string{"ConnectTimeout=30"}}
	a := strings.Join(s.args(Route{Target: "a@192.0.2.1"}), " ")
	if i, j := strings.Index(a, "ConnectTimeout=30"), strings.Index(a, "ConnectTimeout=10"); i < 0 || j < 0 || i > j {
		t.Errorf("%s", a)
	}
	if !strings.Contains(a, "StrictHostKeyChecking=accept-new") {
		t.Errorf("a new device's host key: %s", a)
	}
}
