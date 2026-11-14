package meshctl

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"meridian/trellis/internal/config"
)

func TestProfileAndNodeToFile(t *testing.T) {
	dir := t.TempDir()
	prof := filepath.Join(dir, "etc", "profile.yaml")
	node := filepath.Join(dir, "etc", "node.yaml")
	var out, errb bytes.Buffer

	if rc := Main("test", []string{"profile", "new", "--name", "alpha", "-o", prof}, &out, &errb); rc != 0 {
		t.Fatalf("profile new rc=%d: %s", rc, errb.String())
	}
	st, err := os.Stat(prof)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("profile mode: %v %v", st, err)
	}
	if _, err := config.LoadProfile(prof); err != nil {
		t.Fatalf("generated profile invalid: %v", err)
	}
	before, _ := os.ReadFile(prof)

	errb.Reset()
	if rc := Main("test", []string{"profile", "new", "--name", "beta", "-o", prof}, &out, &errb); rc == 0 || !strings.Contains(errb.String(), "already exists") {
		t.Fatalf("overwrite without --force must fail: rc=%d %s", rc, errb.String())
	}
	if after, _ := os.ReadFile(prof); !bytes.Equal(before, after) {
		t.Fatal("existing profile was modified")
	}
	if rc := Main("test", []string{"profile", "new", "--name", "beta", "-o", prof, "--force"}, &out, &errb); rc != 0 {
		t.Fatalf("--force rc=%d", rc)
	}

	out.Reset()
	if rc := Main("test", []string{"profile", "show", prof}, &out, &errb); rc != 0 || !strings.Contains(out.String(), "sae_passphrase:") {
		t.Fatalf("profile show rc=%d: %s", rc, out.String())
	}
	if rc := Main("test", []string{"node", "new", "--callsign", "relay-1", "--id", "1", "-o", node}, &out, &errb); rc != 0 {
		t.Fatalf("node new rc=%d: %s", rc, errb.String())
	}
	n, err := config.LoadNode(node)
	if err != nil || n.Callsign != "relay-1" || n.NodeID != 1 {
		t.Fatalf("generated node: %+v %v", n, err)
	}
}

func TestProfileBandSwitch(t *testing.T) {
	dir := t.TempDir()
	prof := filepath.Join(dir, "mesh-profile.yaml")
	var out, errb bytes.Buffer
	if rc := Main("test", []string{"profile", "new", "--name", "alpha", "--width", "80", "-o", prof}, &out, &errb); rc != 0 {
		t.Fatalf("new: %s", errb.String())
	}
	before, _ := config.LoadProfile(prof)

	out.Reset()
	if rc := Main("test", []string{"profile", "band", "2g", prof}, &out, &errb); rc != 0 {
		t.Fatalf("band 2g rc=%d: %s", rc, errb.String())
	}
	p, err := config.LoadProfile(prof)
	if err != nil {
		t.Fatal(err)
	}
	if p.Radio.Band != "2g" || p.Radio.Channel != 6 || p.Radio.Width != 20 {
		t.Errorf("after band 2g: %+v", p.Radio)
	}
	if p.SAEPassphrase != before.SAEPassphrase || p.MeshID != before.MeshID {
		t.Error("band switch must keep the mesh identity and secret")
	}
	if st, _ := os.Stat(prof); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", st.Mode().Perm())
	}
	if !strings.Contains(out.String(), "5g ch 149 (80 MHz) -> 2g ch 6 (20 MHz)") {
		t.Errorf("summary: %q", out.String())
	}

	if rc := Main("test", []string{"profile", "band", "5g", "--channel", "36", prof}, &out, &errb); rc != 0 {
		t.Fatalf("band 5g: %s", errb.String())
	}
	p, _ = config.LoadProfile(prof)
	if p.Radio.Band != "5g" || p.Radio.Channel != 36 {
		t.Errorf("after band 5g ch 36: %+v", p.Radio)
	}

	errb.Reset()
	if rc := Main("test", []string{"profile", "band", "2g", "--channel", "149", prof}, &out, &errb); rc == 0 {
		t.Error("2.4 GHz channel 149 must be rejected")
	}
	if p2, _ := config.LoadProfile(prof); p2.Radio != p.Radio {
		t.Error("a rejected switch must leave the file untouched")
	}

	// A second link in the file survives a band switch of the first: the
	// file is rewritten, and used to lose it.
	data, _ := os.ReadFile(prof)
	if err := os.WriteFile(prof, append(data, []byte("radio2: {band: s1g, channel: 26}\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if rc := Main("test", []string{"profile", "band", "2g", prof}, &out, &errb); rc != 0 {
		t.Fatalf("band 2g with radio2: %s", errb.String())
	}
	p, err = config.LoadProfile(prof)
	if err != nil || p.Radio.Band != "2g" || len(p.Links) != 1 || p.Links[0] != (config.RadioPlan{Band: "s1g", Channel: 26, Width: 2}) {
		t.Errorf("after a band switch with a second link: %+v links %+v, %v", p.Radio, p.Links, err)
	}
	// Three links survive it too.
	data, _ = os.ReadFile(prof)
	if err := os.WriteFile(prof, []byte(strings.Replace(string(data), "  - {band: s1g, channel: 26, width: 2}\n", "  - {band: s1g, channel: 26, width: 2}\n  - {band: 5g, channel: 36, width: 40}\n", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if rc := Main("test", []string{"profile", "band", "2g", "--channel", "11", prof}, &out, &errb); rc != 0 {
		t.Fatalf("band 2g with three links: %s", errb.String())
	}
	if p, err = config.LoadProfile(prof); err != nil || p.Radio.Channel != 11 || len(p.Links) != 2 || p.Links[1] != (config.RadioPlan{Band: "5g", Channel: 36, Width: 40}) {
		t.Errorf("after a band switch with three links: %+v links %+v, %v", p.Radio, p.Links, err)
	}

	// HaLow as the first link: its own default channel and width, both ways.
	if rc := Main("test", []string{"profile", "band", "s1g", "--channel", "10", prof}, &out, &errb); rc != 0 {
		t.Fatalf("band s1g: %s", errb.String())
	}
	if p, _ = config.LoadProfile(prof); p.Radio != (config.RadioPlan{Band: "s1g", Channel: 10, Width: 2}) {
		t.Errorf("after band s1g: %+v", p.Radio)
	}
	if rc := Main("test", []string{"profile", "band", "5g", prof}, &out, &errb); rc != 0 {
		t.Fatalf("band 5g from s1g: %s", errb.String())
	}
	if p, err = config.LoadProfile(prof); err != nil || p.Radio != (config.RadioPlan{Band: "5g", Channel: 149, Width: 20}) {
		t.Errorf("after band 5g from s1g: %+v %v", p, err)
	}
	out.Reset()
	halow := filepath.Join(dir, "halow.yaml")
	if rc := Main("test", []string{"profile", "new", "--name", "h", "--band", "s1g", "--width", "8", "--channel", "28", "-o", halow}, &out, &errb); rc != 0 {
		t.Fatalf("profile new --band s1g: %s", errb.String())
	}
	if p, err = config.LoadProfile(halow); err != nil || p.Radio != (config.RadioPlan{Band: "s1g", Channel: 28, Width: 8}) {
		t.Errorf("new HaLow profile: %+v %v", p, err)
	}
}

func TestProfileInstall(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "mesh-profile.yaml")
	dst := filepath.Join(dir, "etc", "profile.yaml")
	var out, errb bytes.Buffer
	Main("test", []string{"profile", "new", "--name", "alpha", "-o", src}, &out, &errb)

	out.Reset()
	if rc := Main("test", []string{"profile", "install", "--target", dst, "--no-reload", src}, &out, &errb); rc != 0 {
		t.Fatalf("install rc=%d: %s", rc, errb.String())
	}
	a, _ := os.ReadFile(src)
	b, _ := os.ReadFile(dst)
	if !bytes.Equal(a, b) {
		t.Fatal("installed profile differs from source")
	}
	out.Reset()
	Main("test", []string{"profile", "install", "--target", dst, "--no-reload", src}, &out, &errb)
	if !strings.Contains(out.String(), "unchanged") {
		t.Errorf("second install should be a no-op: %q", out.String())
	}

	bad := filepath.Join(dir, "bad.yaml")
	os.WriteFile(bad, []byte("schema: 0\nname: x\nsae_passphrase: short\n"), 0o600)
	errb.Reset()
	if rc := Main("test", []string{"profile", "install", "--target", dst, "--no-reload", bad}, &out, &errb); rc == 0 || !strings.Contains(errb.String(), "nothing installed") {
		t.Fatalf("invalid profile must be refused: rc=%d %s", rc, errb.String())
	}
	if c, _ := os.ReadFile(dst); !bytes.Equal(c, a) {
		t.Error("a refused install must leave the existing profile untouched")
	}
}

// profile same compares what two profiles say: node-update.sh uses it, so
// a profile the API rewrote (comments gone, defaults written out, radio2
// spelled links) is not reported as differing from the deploy's.
func TestProfileSame(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	hand := write("hand.yaml", `# the deploy's copy, written by hand
schema: 0
name: alpha
sae_passphrase: "correct-horse-battery"
regulatory: {mode: country, country: us}
radio: {band: 2g, channel: 6}
radio2: {band: 5g, channel: 149, width: 80}
`)
	apiBody := `schema: 0
name: alpha
mesh_id: alpha
sae_passphrase: correct-horse-battery
regulatory:
  country: US
  mode: country
radio:
  band: 2g
  channel: 6
  width: 20
links:
  - band: 5g
    channel: 149
    width: 80
prefix: 10.73.0.0/16
batman:
  routing_algo: BATMAN_V
  orig_interval_ms: 1000
`
	api := write("api.yaml", apiBody)
	other := write("other.yaml", strings.Replace(strings.Replace(apiBody, "channel: 149", "channel: 36", 1), "battery", "staple", 1))
	var out, errb bytes.Buffer
	if rc := Main("test", []string{"profile", "same", hand, api}, &out, &errb); rc != 0 {
		t.Fatalf("same profiles reported as different: rc=%d %s", rc, errb.String())
	}
	out.Reset()
	errb.Reset()
	if rc := Main("test", []string{"profile", "same", hand, other}, &out, &errb); rc == 0 ||
		!strings.Contains(errb.String(), "links") || !strings.Contains(errb.String(), "sae_passphrase") || strings.Contains(errb.String(), "staple") {
		t.Errorf("different profiles: rc=%d %q; want an error naming links and sae_passphrase, without the secret", rc, errb.String())
	}
}
