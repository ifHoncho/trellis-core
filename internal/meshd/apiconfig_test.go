package meshd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"meridian/trellis/internal/api"
	"meridian/trellis/internal/config"
	"meridian/trellis/internal/gossip"
	"meridian/trellis/internal/nodestate"
	"meridian/trellis/internal/status"
)

// testDaemon is a daemon that never touches the kernel: configuration in
// a temporary directory, no netlink, no loop. files maps names under the
// configuration directory to contents.
func testDaemon(t *testing.T, files map[string]string) (*Daemon, string) {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		mode := os.FileMode(0o644)
		if name == "profile.yaml" {
			mode = 0o600
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	d, err := New(Options{
		NodePath: filepath.Join(dir, "node.yaml"), ProfilePath: filepath.Join(dir, "profile.yaml"),
		StateDir: filepath.Join(dir, "state"), RunDir: filepath.Join(dir, "run"), SysRoot: filepath.Join(dir, "sys"),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Version: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	d.ctx = context.Background()
	return d, dir
}

const testProfile = `schema: 0
name: alpha
mesh_id: trellis-alpha
sae_passphrase: test-passphrase-not-secret
regulatory: {mode: country, country: US}
radio: {band: 5g, channel: 149, width: 20}
`

func hasEffect(es []status.Warning, level, code string) bool {
	for _, e := range es {
		if e.Level == level && e.Code == code {
			return true
		}
	}
	return false
}

func TestSetupModeAndProvisioning(t *testing.T) {
	d, dir := testDaemon(t, nil)
	ctx := context.Background()
	st := d.setupState()
	if !st.Needed || st.NodeConfig || st.Profile {
		t.Fatalf("a node with no files: %+v", st)
	}
	node, profile := d.conf()
	if node.Callsign == "" || !strings.HasPrefix(profile.MeshID, "trellis-setup-") || profile.Radio.Band != config.Band2G {
		t.Fatalf("setup config: callsign %q, mesh %q on %s", node.Callsign, profile.MeshID, profile.Radio.Band)
	}
	// The private profile survives a restart: same mesh, same secret.
	again, err := setupProfile(filepath.Join(dir, "state"))
	if err != nil || again.MeshID != profile.MeshID || again.SAEPassphrase != profile.SAEPassphrase {
		t.Errorf("setup profile changed across loads: %v", err)
	}
	// The setup AP has documented credentials under a name that says so.
	d.ident = &nodestate.Identity{MAC: "02:aa:bb:cc:3f:2a", NodeID: 9, APPassphrase: "generated1234"}
	j, _ := d.APJoin()
	if j.SSID != "trellis-setup-3f2a" || j.Passphrase != setupAPPassphrase || j.QR == nil || j.QR.Size < 21 {
		t.Errorf("setup AP: %+v", j)
	}
	if _, err := d.ProfileExport(); err == nil {
		t.Error("a setup profile was offered for sharing")
	}

	// A dry run says what provisioning will do and how to join afterwards.
	in := api.SetupInput{Callsign: "alpha-1", ProfileInput: api.ProfileInput{New: &api.NewProfile{Name: "alpha", Band: "5g"}}}
	res, err := d.Setup(ctx, in, true)
	if err != nil || res.Applied {
		t.Fatalf("dry run: %+v %v", res, err)
	}
	if !hasEffect(res.Effects, "info", "joins-mesh") || !hasEffect(res.Effects, "warning", "ap-credentials") {
		t.Errorf("effects: %+v", res.Effects)
	}
	if res.AP == nil || res.AP.SSID != "alpha-1" || res.AP.Passphrase != "generated1234" {
		t.Errorf("AP after setup: %+v", res.AP)
	}
	if _, err := os.Stat(filepath.Join(dir, "node.yaml")); err == nil {
		t.Fatal("a dry run wrote node.yaml")
	}
	// Bad input names its field and writes nothing.
	bad := in
	bad.Callsign = "Alpha One"
	var es config.Errors
	if _, err := d.Setup(ctx, bad, false); !errors.As(err, &es) || es[0].Field != "callsign" {
		t.Errorf("bad callsign: %v", err)
	}
	if _, err := d.Setup(ctx, api.SetupInput{Callsign: "x", ProfileInput: api.ProfileInput{Code: "TRP1:garbage"}}, false); err == nil {
		t.Error("a damaged profile code was accepted")
	}

	res, err = d.Setup(ctx, in, false)
	if err != nil || !res.Applied {
		t.Fatalf("setup: %+v %v", res, err)
	}
	p, err := config.LoadProfile(filepath.Join(dir, "profile.yaml"))
	if err != nil || p.Name != "alpha" || p.Radio.Channel != 149 || len(p.SAEPassphrase) != 27 || p.Regulatory.Mode != config.RegModeCountry {
		t.Fatalf("written profile: %+v %v", p, err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "profile.yaml")); fi.Mode().Perm() != 0o600 {
		t.Errorf("profile mode %v", fi.Mode().Perm())
	}
	if n, err := config.LoadNode(filepath.Join(dir, "node.yaml")); err != nil || n.Callsign != "alpha-1" {
		t.Fatalf("written node config: %+v %v", n, err)
	}
	if !d.applyPending() {
		t.Fatal("no pending configuration after setup")
	}
	if st := d.setupState(); st.Needed || !st.NodeConfig || !st.Profile {
		t.Errorf("after setup: %+v", st)
	}
	if _, err := os.Stat(filepath.Join(dir, "state", setupProfileFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the setup profile was left behind: %v", err)
	}
	if j, _ := d.APJoin(); j.SSID != "alpha-1" || j.Passphrase != "generated1234" {
		t.Errorf("AP after setup: %+v", j)
	}
	// Now it can be shared, and the code another node gets is the profile.
	ex, err := d.ProfileExport()
	if err != nil || ex.QR == nil || !strings.Contains(ex.YAML, "sae_passphrase: "+p.SAEPassphrase) {
		t.Fatalf("export: %v", err)
	}
	got, err := config.DecodeProfileCode(ex.Code)
	if err != nil || got.SAEPassphrase != p.SAEPassphrase || got.MeshID != p.MeshID {
		t.Errorf("profile code: %+v %v", got, err)
	}
	t.Logf("profile code: %d characters, QR %d modules", len(ex.Code), ex.QR.Size)
}

func TestSetNode(t *testing.T) {
	d, dir := testDaemon(t, map[string]string{
		"node.yaml":    "schema: 0\ncallsign: node-1\nap: {passphrase: \"my wifi secret\"}\n",
		"profile.yaml": testProfile,
	})
	ctx := context.Background()
	cfg := d.Config()
	if cfg.Node.AP.Passphrase != config.Redacted || cfg.Profile.SAEPassphrase != config.Redacted || cfg.Setup.Needed {
		t.Fatalf("config: %+v", cfg)
	}
	put := func(n config.Node, dry bool) (*api.ApplyResult, error) {
		raw, _ := json.Marshal(n)
		return d.SetNode(ctx, raw, "", dry)
	}
	// Sent back as it came: nothing changes, nothing is written.
	before, _ := os.ReadFile(filepath.Join(dir, "node.yaml"))
	res, err := put(cfg.Node, false)
	if err != nil || res.Changed || res.Applied || len(res.Effects) != 0 {
		t.Fatalf("unchanged: %+v %v", res, err)
	}
	// "No effects" is an empty list in JSON, never null: clients iterate it.
	if js, _ := json.Marshal(res); !strings.Contains(string(js), `"effects":[]`) {
		t.Errorf("an answer with no effects: %s", js)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "node.yaml")); string(after) != string(before) {
		t.Error("an unchanged configuration rewrote the file")
	}

	n := cfg.Node
	n.Callsign = "relay-7"
	n.Gateway.Mode = config.GatewayOff
	n.Web.WANAccess = true
	n.Ports = []config.Port{{Name: "eth1", Role: config.PortRoleClient}}
	n.ProfilePath = "/somewhere/else.yaml" // not the API's to change
	res, err = put(n, true)
	if err != nil || !res.Changed || res.Applied {
		t.Fatalf("dry run: %+v %v", res, err)
	}
	for _, want := range [][2]string{{"info", "renamed"}, {"warning", "gateway-off"}, {"warning", "web-wan-access"}, {"info", "port-role"}, {"warning", "ap-credentials"}} {
		if !hasEffect(res.Effects, want[0], want[1]) {
			t.Errorf("no %s effect %q in %+v", want[0], want[1], res.Effects)
		}
	}
	if res.Effects[0].Level != "warning" || res.Effects[len(res.Effects)-1].Level != "info" {
		t.Errorf("effects are not sorted by severity: %+v", res.Effects)
	}
	if res.AP == nil || res.AP.SSID != "relay-7" || res.AP.Passphrase != "my wifi secret" {
		t.Errorf("AP: %+v", res.AP)
	}
	if res, err = put(n, false); err != nil || !res.Applied {
		t.Fatalf("apply: %+v %v", res, err)
	}
	written, _ := os.ReadFile(filepath.Join(dir, "node.yaml"))
	for _, want := range []string{"# Trellis node configuration", "callsign: relay-7", "passphrase: my wifi secret", "wan_access: true", "mode: \"off\""} {
		if !strings.Contains(string(written), want) {
			t.Errorf("written file lacks %q:\n%s", want, written)
		}
	}
	if strings.Contains(string(written), "else.yaml") || strings.Contains(string(written), config.Redacted) {
		t.Errorf("written file:\n%s", written)
	}
	d.applyPending()
	if node, _ := d.conf(); node.Callsign != "relay-7" || node.AP.Passphrase != "my wifi secret" || !node.Web.WANAccess {
		t.Errorf("running config: %+v", node)
	}

	// Rejected documents name the field and leave the file alone.
	n.Callsign = "-bad-"
	var es config.Errors
	if _, err := put(n, false); !errors.As(err, &es) || es[0].Field != "callsign" {
		t.Errorf("invalid callsign: %v", err)
	}
	if _, err := d.SetNode(ctx, []byte(`{"callsign":"x","nonsense":1}`), "", false); !errors.As(err, &es) {
		t.Errorf("unknown field: %v", err)
	}
	if now, _ := os.ReadFile(filepath.Join(dir, "node.yaml")); string(now) != string(written) {
		t.Error("a rejected configuration changed the file")
	}
}

func TestSetProfile(t *testing.T) {
	d, dir := testDaemon(t, map[string]string{"node.yaml": "schema: 0\ncallsign: node-1\n", "profile.yaml": testProfile})
	ctx := context.Background()
	ex, err := d.ProfileExport()
	if err != nil {
		t.Fatal(err)
	}
	if res, err := d.SetProfile(ctx, api.ProfileInput{Code: ex.Code}, false); err != nil || res.Changed || res.Applied {
		t.Fatalf("the node's own profile: %+v %v", res, err)
	}
	// Edited through the API with the secret redacted: the secret is kept.
	p := d.Config().Profile
	p.Radio.Channel = 157
	res, err := d.SetProfile(ctx, api.ProfileInput{Profile: &p}, true)
	if err != nil || !res.Changed || !hasEffect(res.Effects, "error", "leaves-mesh") {
		t.Fatalf("channel change: %+v %v", res, err)
	}
	if res, err = d.SetProfile(ctx, api.ProfileInput{Profile: &p}, false); err != nil || !res.Applied {
		t.Fatalf("apply: %+v %v", res, err)
	}
	got, err := config.LoadProfile(filepath.Join(dir, "profile.yaml"))
	if err != nil || got.Radio.Channel != 157 || got.SAEPassphrase != "test-passphrase-not-secret" {
		t.Fatalf("written profile: %+v %v", got, err)
	}
	d.applyPending()

	for name, in := range map[string]api.ProfileInput{
		"none":                           {},
		"two":                            {Code: ex.Code, YAML: testProfile},
		"halow at a Wi-Fi width":         {YAML: strings.Replace(testProfile, "band: 5g, channel: 149", "band: s1g, channel: 26", 1)},
		"halow channel of another width": {YAML: strings.Replace(testProfile, "band: 5g, channel: 149, width: 20", "band: s1g, channel: 27, width: 2", 1)},
		"bad yaml":                       {YAML: "name: [\n"},
		"no name":                        {New: &api.NewProfile{}},
	} {
		if _, err := d.SetProfile(ctx, in, false); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if now, _ := config.LoadProfile(filepath.Join(dir, "profile.yaml")); now.Radio.Channel != 157 {
		t.Error("a rejected profile changed the file")
	}

	// A HaLow second link is a profile this build runs: checked without
	// applying.
	withHaLow := testProfile + "radio2: {band: s1g, channel: 26}\n"
	res, err = d.SetProfile(ctx, api.ProfileInput{YAML: withHaLow}, true)
	if err != nil || !res.Changed {
		t.Errorf("profile with a HaLow second link: %+v %v", res, err)
	}
	// A new mesh may start with several links (the setup wizard); a link
	// without a channel gets its band's usual one.
	np, err := newProfile(api.NewProfile{Name: "beta", Band: "5g", Width: 160, Country: "us",
		Links: []config.RadioPlan{{Band: "s1g"}, {Band: "2g", Channel: 11, Width: 40}}})
	if err != nil || np.Radio != (config.RadioPlan{Band: "5g", Channel: 149, Width: 160}) || len(np.Links) != 2 ||
		np.Links[0] != (config.RadioPlan{Band: "s1g", Channel: 26, Width: 2}) || np.Links[1] != (config.RadioPlan{Band: "2g", Channel: 11, Width: 40}) {
		t.Errorf("new mesh with links: %+v %v", np, err)
	}
	if _, err := newProfile(api.NewProfile{Name: "beta", Links: []config.RadioPlan{{Band: "2g", Width: 80}}}); err == nil || !strings.Contains(err.Error(), "links[0].width") {
		t.Errorf("new mesh with a 2.4 GHz link at 80 MHz: %v, want an error naming links[0].width", err)
	}
	if got := linkFreqMHz(config.RadioPlan{Band: "s1g", Channel: 27, Width: 1}, "US", 5560); got != 915.5 {
		t.Errorf("HaLow link frequency = %v, want 915.5 (not the supplicant's mapped 5560)", got)
	}
	if got := linkFreqMHz(config.RadioPlan{Band: "5g", Channel: 149, Width: 20}, "US", 5745); got != 5745 {
		t.Errorf("Wi-Fi link frequency = %v, want 5745", got)
	}
	if got := linkFreqMHz(config.RadioPlan{Band: "s1g", Channel: 27, Width: 1}, "US", 0); got != 0 {
		t.Errorf("a link that has not started must report no frequency, got %v", got)
	}
	// Regulatory mode changes are spelled out (architecture §5.1).
	p = d.Config().Profile
	p.Regulatory.Mode = config.RegModeUnrestricted
	if res, err := d.SetProfile(ctx, api.ProfileInput{Profile: &p}, true); err != nil || !hasEffect(res.Effects, "warning", "regulatory") || hasEffect(res.Effects, "error", "leaves-mesh") {
		t.Errorf("regulatory change: %+v %v", res, err)
	}
}

func TestWriteFilesRollsBack(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.yaml"), filepath.Join(dir, "missing", "sub", "b.yaml")
	os.WriteFile(a, []byte("old\n"), 0o644)
	// The second file cannot be written: its directory is a file.
	os.WriteFile(filepath.Join(dir, "missing"), []byte("x"), 0o644)
	err := writeFiles([]*fileChange{{path: a, data: []byte("new\n"), mode: 0o644}, {path: b, data: []byte("new\n"), mode: 0o600}})
	if err == nil {
		t.Fatal("expected an error")
	}
	if got, _ := os.ReadFile(a); string(got) != "old\n" {
		t.Errorf("first file not restored: %q", got)
	}
}

func TestWebPassword(t *testing.T) {
	// A provisioned node with no password takes one only from root.
	d, dir := testDaemon(t, map[string]string{"node.yaml": "schema: 0\ncallsign: node-1\n", "profile.yaml": testProfile})
	if d.WebInfo().AdminSet {
		t.Fatal("a fresh node has an admin password")
	}
	var ae *api.Error
	if err := d.WebSetPassword("", "", "claimed-by-a-stranger", false); !errors.As(err, &ae) || ae.Status != 403 {
		t.Fatalf("unprivileged first password on a provisioned node: %v", err)
	}
	if err := d.WebLogin("", "anything"); !errors.As(err, &ae) || ae.Status != 403 {
		t.Errorf("login with no password set: %v", err)
	}
	if err := d.WebSetPassword("", "", "short", true); !errors.As(err, &ae) || ae.Status != 400 {
		t.Errorf("short password: %v", err)
	}
	if err := d.WebSetPassword("", "", "first-password", true); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "state", webAdminFile))
	if strings.Contains(string(raw), "first-password") || !strings.Contains(string(raw), "pbkdf2-sha256") {
		t.Errorf("credential file:\n%s", raw)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "state", webAdminFile)); fi.Mode().Perm() != 0o600 {
		t.Errorf("credential file mode %v", fi.Mode().Perm())
	}
	if err := d.WebLogin("", "first-password"); err != nil {
		t.Errorf("login: %v", err)
	}
	if err := d.WebLogin("", "wrong"); !errors.Is(err, api.ErrBadPassword) {
		t.Errorf("wrong password: %v", err)
	}
	// Changing it takes the current one.
	if err := d.WebSetPassword("", "wrong", "second-password", false); !errors.As(err, &ae) || ae.Status != 401 {
		t.Errorf("change with a wrong current password: %v", err)
	}
	if err := d.WebSetPassword("", "first-password", "second-password", false); err != nil {
		t.Fatal(err)
	}
	if d.WebLogin("", "first-password") == nil || d.WebLogin("", "second-password") != nil {
		t.Error("the password did not change")
	}
	// It survives a restart.
	d2, err := New(d.opts)
	if err != nil || !d2.WebInfo().AdminSet || d2.WebLogin("", "second-password") != nil {
		t.Errorf("after restart: %v", err)
	}
	// Wrong passwords in a row lock that source out for a while, even with
	// the right one; other sources are not held up.
	const guesser, admin = "10.73.9.9", "10.73.1.2"
	for i := 0; i < webFreeFailures; i++ {
		if err := d.WebLogin(guesser, "guess"); !errors.Is(err, api.ErrBadPassword) {
			t.Fatalf("guess %d: %v", i, err)
		}
	}
	if err := d.WebLogin(guesser, "second-password"); !errors.As(err, &ae) || ae.Status != 429 || ae.RetryAfter < 1 {
		t.Errorf("after %d wrong passwords: %v", webFreeFailures, err)
	}
	if err := d.WebLogin(admin, "second-password"); err != nil {
		t.Errorf("another source during the guesser's lockout: %v", err)
	}
	if err := d.WebSetPassword(guesser, "second-password", "third-password", false); !errors.As(err, &ae) || ae.Status != 429 {
		t.Errorf("password change from a locked-out source: %v", err)
	}
	d.webAuth.sources[guesser].lockedUntil = time.Now().Add(-time.Second)
	if err := d.WebLogin(guesser, "second-password"); err != nil {
		t.Errorf("after the lockout: %v", err)
	}
	if _, ok := d.webAuth.sources[guesser]; ok {
		t.Error("a right password did not clear the source's failures")
	}
	// The table of sources stays bounded.
	for i := 0; i < webSourcesMax+10; i++ {
		d.webAuth.source(fmt.Sprintf("10.73.%d.%d", i/250, i%250), time.Now())
	}
	if n := len(d.webAuth.sources); n > webSourcesMax {
		t.Errorf("%d sources tracked, over %d", n, webSourcesMax)
	}
	// Root can remove it (meshctl web password --clear).
	if err := d.WebSetPassword("", "", "", true); err != nil || d.WebInfo().AdminSet {
		t.Errorf("clear: %v", err)
	}

	// In setup mode, whoever sets the node up gives it its password.
	s, _ := testDaemon(t, nil)
	if err := s.WebSetPassword("", "", "set-during-setup", false); err != nil {
		t.Fatalf("first password in setup mode: %v", err)
	}
	if err := s.WebSetPassword("", "", "taken-over", false); err == nil {
		t.Error("a second unauthenticated password was accepted in setup mode")
	}
}

// Parallel guesses from one source are counted one by one: no more than
// webFreeFailures of them reach the hash before the lockout.
func TestWebPasswordParallelGuesses(t *testing.T) {
	d, _ := testDaemon(t, map[string]string{"node.yaml": "schema: 0\ncallsign: node-1\n", "profile.yaml": testProfile})
	if err := d.WebSetPassword("", "", "the-password", true); err != nil {
		t.Fatal(err)
	}
	const n = 32
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() { errs <- d.WebLogin("10.73.9.9", "guess") }()
	}
	wrong, locked := 0, 0
	for i := 0; i < n; i++ {
		var ae *api.Error
		switch err := <-errs; {
		case errors.Is(err, api.ErrBadPassword):
			wrong++
		case errors.As(err, &ae) && ae.Status == 429:
			locked++
		default:
			t.Errorf("guess: %v", err)
		}
	}
	if wrong != webFreeFailures || locked != n-webFreeFailures {
		t.Errorf("%d guesses checked, %d locked out; want %d checked", wrong, locked, webFreeFailures)
	}
}

// IPv6 sources are locked out by /64: a client cannot take a fresh address
// for each guess. Filling the table does not push a lockout out.
func TestWebPasswordSources(t *testing.T) {
	for from, want := range map[string]string{
		"10.73.1.2":            "10.73.1.2",
		"::ffff:10.73.1.2":     "10.73.1.2",
		"2001:db8:1:2:aaaa::1": "2001:db8:1:2::/64",
		"2001:db8:1:2:bbbb::2": "2001:db8:1:2::/64",
		"fe80::1%br-mesh":      "fe80::/64",
		"":                     "",
	} {
		if got := sourceKey(from); got != want {
			t.Errorf("sourceKey(%q) = %q, want %q", from, got, want)
		}
	}
	d, _ := testDaemon(t, map[string]string{"node.yaml": "schema: 0\ncallsign: node-1\n", "profile.yaml": testProfile})
	if err := d.WebSetPassword("", "", "the-password", true); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < webFreeFailures; i++ {
		d.WebLogin(fmt.Sprintf("2001:db8:1:2::%x", i+1), "guess")
	}
	var ae *api.Error
	if err := d.WebLogin("2001:db8:1:2::ffff", "the-password"); !errors.As(err, &ae) || ae.Status != 429 {
		t.Errorf("a new address in a locked-out /64: %v", err)
	}
	d.webAuth.mu.Lock()
	for i := 0; i < 2*webSourcesMax; i++ {
		d.webAuth.source(fmt.Sprintf("10.73.%d.%d", i/250, i%250), time.Now())
	}
	_, kept := d.webAuth.sources["2001:db8:1:2::/64"]
	d.webAuth.mu.Unlock()
	if !kept {
		t.Error("filling the table pushed out a locked-out source")
	}
}

func TestIdentifyLEDs(t *testing.T) {
	d, dir := testDaemon(t, map[string]string{"node.yaml": "schema: 0\ncallsign: node-1\n", "profile.yaml": testProfile})
	leds := filepath.Join(dir, "sys", "class", "leds")
	for _, name := range []string{"mt76-phy1", "mt76-phy10", "ath9k_htc-phy5", "phy0-led", "rt2800usb-phy2::radio", "rt2800usb-phy2::assoc", "lan1_led", "mmc0::"} {
		os.MkdirAll(filepath.Join(leds, name), 0o755)
	}
	for phy, want := range map[string]int{"phy1": 1, "phy10": 1, "phy5": 1, "phy0": 1, "phy2": 2, "phy3": 0, "": 0} {
		if got := radioLEDs(filepath.Join(dir, "sys"), phy); len(got) != want {
			t.Errorf("%s: %d LEDs %v, want %d", phy, len(got), got, want)
		}
	}
	led := filepath.Join(leds, "mt76-phy1")
	os.WriteFile(filepath.Join(led, "trigger"), []byte("none kbd-scrolllock [phy1tpt] phy1radio\n"), 0o644)
	os.WriteFile(filepath.Join(led, "brightness"), []byte("0\n"), 0o644)
	os.WriteFile(filepath.Join(led, "max_brightness"), []byte("255\n"), 0o644)
	if err := blinkLED(led, 200*time.Millisecond, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(led, "trigger")); string(got) != "phy1tpt" {
		t.Errorf("trigger after the blink: %q", got)
	}
	var ae *api.Error
	if err := d.IdentifyRadio("mac:00:00:00:00:00:00", 1); !errors.As(err, &ae) || ae.Status != 404 {
		t.Errorf("unknown radio: %v", err)
	}
}

func TestTopologyAssembly(t *testing.T) {
	d, _ := testDaemon(t, map[string]string{"node.yaml": "schema: 0\ncallsign: node-1\nnode_id: 1\n", "profile.yaml": testProfile})
	const self, n2, n3 = "02:00:00:00:00:01", "02:00:00:00:00:02", "02:00:00:00:00:03"
	d.ident = &nodestate.Identity{MAC: self, NodeID: 1}
	d.peers = gossip.NewTable(self)
	d.peers.Update(&gossip.Record{V: 1, MAC: n2, Orig: []string{"9c:00:00:00:00:02"}, ID: 2, Callsign: "node-2", Addr: "10.73.2.1", Boot: 1, Seq: 1, GW: &gossip.GW{Healthy: true}}, "10.73.2.1")
	d.peers.Update(&gossip.Record{V: 1, MAC: n3, Orig: []string{"9c:00:00:00:00:03", "9c:00:00:00:01:03"}, ID: 3, Callsign: "node-3", Addr: "10.73.3.1", Boot: 1, Seq: 1}, "10.73.3.1")
	now := time.Now()
	d.topo.lastAsk = now // no round: this daemon has no socket
	d.topo.reports = map[string]linkReport{
		n2: {at: now.Add(-2 * time.Second), links: &gossip.Links{V: 1, MAC: n2, Clients: 2, Selected: 2, Links: []gossip.Link{
			{Local: "9c:00:00:00:00:02", Remote: "9c:00:00:00:00:03", Kbps: 40000, Signal: -60},
			{Local: "9c:00:00:00:00:02", Remote: "9c:00:00:00:00:99", Kbps: 1000, Signal: -88}, // not in gossip yet
		}}},
		n3: {at: now.Add(-3 * time.Second), links: &gossip.Links{V: 1, MAC: n3, Selected: 2, Links: []gossip.Link{
			{Local: "9c:00:00:00:00:03", Remote: "9c:00:00:00:00:02", Kbps: 30000, Signal: -64},
		}}},
		"02:00:00:00:00:77": {at: now, links: &gossip.Links{V: 1, MAC: "02:00:00:00:00:77"}}, // left the mesh
	}
	topo, err := d.Topology(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]status.TopoNode{}
	for _, n := range topo.Nodes {
		byID[n.ID] = n
	}
	if len(topo.Nodes) != 4 || !byID[self].Self || byID[self].Callsign != "node-1" {
		t.Fatalf("nodes: %+v", topo.Nodes)
	}
	if n := byID[n2]; !n.Reported || n.Clients != 2 || n.SelectedGateway != 2 || !n.Gateway || !n.WANHealthy || n.ReportAgeMS < 1900 {
		t.Errorf("node-2: %+v", n)
	}
	if n := byID["9c:00:00:00:00:99"]; n.Callsign != "" || n.Reported {
		t.Errorf("unnamed neighbour: %+v", n)
	}
	if len(topo.Links) != 2 {
		t.Fatalf("links: %+v", topo.Links)
	}
	var both *status.TopoLink
	for i, l := range topo.Links {
		if (l.A == n2 && l.B == n3) || (l.A == n3 && l.B == n2) {
			both = &topo.Links[i]
		}
	}
	if both == nil || both.ABMbps+both.BAMbps != 70 || both.ASignalDBm+both.BSignalDBm != -124 || both.Medium != "wifi" {
		t.Errorf("the link both ends reported: %+v", both)
	}
	if _, ok := d.topo.reports["02:00:00:00:00:77"]; ok {
		t.Error("a report from a node that left was kept")
	}
	// A report older than three rounds no longer counts.
	d.topo.reports[n3] = linkReport{at: now.Add(-time.Minute), links: d.topo.reports[n3].links}
	topo, _ = d.Topology(context.Background())
	for _, n := range topo.Nodes {
		if n.ID == n3 && n.Reported {
			t.Errorf("a stale report was used: %+v", n)
		}
	}
	if topoInterval(4) != topoMinInterval || topoInterval(50) != 25*time.Second {
		t.Errorf("intervals: %v %v", topoInterval(4), topoInterval(50))
	}
}

func TestDefaultCallsign(t *testing.T) {
	for in, want := range map[string]string{
		"Alpha-Unit5": "alpha-unit5", "pi_zero.local": "pi-zero", "": "trellis-node", "localhost": "trellis-node",
		"--weird$$name--": "weirdname", strings.Repeat("a", 80): strings.Repeat("a", 63),
	} {
		if got := defaultCallsign(in); got != want {
			t.Errorf("defaultCallsign(%q) = %q, want %q", in, got, want)
		}
	}
}

// effectText returns the message of the first effect with a code, or "".
func effectText(es []status.Warning, code string) string {
	for _, e := range es {
		if e.Code == code {
			return e.Message
		}
	}
	return ""
}

// A profile is installed on one node at a time. The check before it says
// what that does to this node's links to the others, which keep theirs, and
// which nodes could not follow at all.
func TestProfileEffectsLinks(t *testing.T) {
	base, err := config.ParseProfile([]byte(strings.Replace(testProfile, "band: 5g, channel: 149", "band: 2g, channel: 6", 1)))
	if err != nil {
		t.Fatal(err)
	}
	with := func(edit func(p *config.Profile)) *config.Profile {
		p := *base
		p.Links = append([]config.RadioPlan{}, base.Links...)
		edit(&p)
		return &p
	}
	five := config.RadioPlan{Band: "5g", Channel: 149, Width: 80}
	// This node next to two dual-band nodes and two that only have 2.4 GHz,
	// all four direct neighbours on the one link.
	nodes := []status.MeshNode{
		{Callsign: "node-1", MeshRadios: [][]string{{"2g", "5g"}}},
		{Callsign: "node-2", MeshRadios: [][]string{{"2g", "5g"}}},
		{Callsign: "alpha-5", MeshRadios: [][]string{{"2g"}}},
		{Callsign: "zulu-3", MeshRadios: [][]string{{"2g"}}},
	}
	view := ProfileView{Peers: [][]string{{"alpha-5", "node-1", "node-2", "zulu-3"}}, Nodes: nodes}

	// The whole mesh link to 5 GHz, on this node alone.
	es := profileEffects(base, with(func(p *config.Profile) { p.Radio = config.RadioPlan{Band: "5g", Channel: 149, Width: 20} }), false, view)
	if !hasEffect(es, "error", "leaves-mesh") {
		t.Fatalf("band change: %+v", es)
	}
	if msg := effectText(es, "leaves-mesh"); !strings.Contains(msg, "Its 4 direct links (alpha-5, node-1, node-2 and zulu-3) drop") ||
		!strings.Contains(msg, "off 2.4 GHz channel 6 (20 MHz) and puts it on 5 GHz channel 149 (20 MHz)") {
		t.Errorf("band change does not count the links it drops: %s", msg)
	}
	if msg := effectText(es, "nodes-cannot-follow"); !hasEffect(es, "warning", "nodes-cannot-follow") || !strings.Contains(msg, "alpha-5 and zulu-3 have no mesh radio for 5 GHz") {
		t.Errorf("nodes without a 5 GHz radio are not named: %+v", es)
	}
	if msg := effectText(es, "this-node-only"); !strings.Contains(msg, "Only this node changes. The 4 other nodes of this mesh") {
		t.Errorf("the scope is not stated: %+v", es)
	}

	// 5 GHz as a second link: nothing is lost, and nobody is left out.
	es = profileEffects(base, with(func(p *config.Profile) { p.Links = []config.RadioPlan{five} }), false, view)
	for _, e := range es {
		if e.Level != "info" {
			t.Errorf("adding a link should cost nothing: %+v", e)
		}
	}
	if msg := effectText(es, "link-added"); !strings.Contains(msg, "New link: 5 GHz channel 149 (80 MHz)") ||
		!strings.Contains(msg, "No 5 GHz mesh radio on alpha-5 and zulu-3: they stay without this link") {
		t.Errorf("added link: %+v", es)
	}
	if !hasEffect(es, "info", "this-node-only") {
		t.Errorf("the scope is not stated: %+v", es)
	}
	// On a node with no radio for the new link.
	none := view
	none.Carries = func(i int) bool { return i == 0 }
	es = profileEffects(base, with(func(p *config.Profile) { p.Links = []config.RadioPlan{five} }), false, none)
	if msg := effectText(es, "link-added"); !strings.Contains(msg, "This node has no radio free for it") {
		t.Errorf("added link without a radio: %+v", es)
	}

	// 5 GHz first and 2.4 GHz second. A node with a radio for each keeps
	// its 2.4 GHz neighbours (the link only restarts under its new number).
	swapped := with(func(p *config.Profile) { p.Radio, p.Links = five, []config.RadioPlan{base.Radio} })
	es = profileEffects(base, swapped, false, view)
	if hasEffect(es, "error", "leaves-mesh") || !hasEffect(es, "warning", "link-restarts") || !hasEffect(es, "info", "link-added") {
		t.Errorf("reordered with both radios: %+v", es)
	}
	// A node whose one radio goes to the new first link loses them all.
	one := view
	one.Carries = func(i int) bool { return i == 0 }
	es = profileEffects(base, swapped, false, one)
	if !hasEffect(es, "error", "leaves-mesh") || !hasEffect(es, "info", "link-no-radio") || !strings.Contains(effectText(es, "leaves-mesh"), "Its 4 direct links") {
		t.Errorf("reordered with one radio: %+v", es)
	}

	// Dropping the second of two links: the node stays in over the first.
	two := with(func(p *config.Profile) { p.Links = []config.RadioPlan{five} })
	both := ProfileView{Peers: [][]string{{"alpha-5", "zulu-3"}, {"node-1"}}, Nodes: nodes}
	es = profileEffects(two, base, false, both)
	if hasEffect(es, "error", "leaves-mesh") || !hasEffect(es, "warning", "links-drop") {
		t.Fatalf("second link removed: %+v", es)
	}
	if msg := effectText(es, "links-drop"); !strings.Contains(msg, "Its direct link to node-1 drops") || !strings.Contains(msg, "stays in the mesh over 2.4 GHz channel 6 (20 MHz), where it has 2 direct links (alpha-5 and zulu-3)") {
		t.Errorf("second link removed: %s", msg)
	}
	// The same when the link that stays has nobody on it: cut off.
	both.Peers = [][]string{nil, {"node-1"}}
	if es = profileEffects(two, base, false, both); !hasEffect(es, "error", "leaves-mesh") {
		t.Errorf("the only neighbour is on the link that goes: %+v", es)
	}
	// With wired mesh ports the node is never cut off by a channel.
	both.Wired = true
	if es = profileEffects(two, base, false, both); hasEffect(es, "error", "leaves-mesh") {
		t.Errorf("wired mesh ports: %+v", es)
	}

	// A link the node has no radio for is not one it "goes off": moving the
	// first link names that one alone.
	held := view
	held.Has = func(i int) bool { return i == 0 }
	es = profileEffects(two, with(func(p *config.Profile) { p.Radio = config.RadioPlan{Band: "5g", Channel: 149, Width: 20} }), false, held)
	if msg := effectText(es, "leaves-mesh"); !strings.Contains(msg, "off 2.4 GHz channel 6 (20 MHz) and puts it on 5 GHz channel 149 (20 MHz)") {
		t.Errorf("a link without a radio is listed as left: %s", msg)
	}

	// A new secret separates the node whatever the channels.
	es = profileEffects(base, with(func(p *config.Profile) { p.SAEPassphrase = "another-secret-entirely" }), false, view)
	if msg := effectText(es, "leaves-mesh"); !hasEffect(es, "error", "leaves-mesh") || !strings.Contains(msg, "the mesh secret") || !strings.Contains(msg, "Its 4 direct links") {
		t.Errorf("secret change: %+v", es)
	}
	// A node that does not report its radios is said to be unknown, not fine.
	old := view
	old.Nodes = append([]status.MeshNode{{Callsign: "pizero"}}, nodes...)
	es = profileEffects(base, with(func(p *config.Profile) { p.Radio = config.RadioPlan{Band: "5g", Channel: 149, Width: 20} }), false, old)
	if msg := effectText(es, "nodes-unknown-radios"); !strings.Contains(msg, "pizero") {
		t.Errorf("node with unknown radios: %+v", es)
	}
	// The client Wi-Fi on the mesh radio moves band with it.
	ap := view
	ap.APBand, ap.APBandAfter = "2g", "5g"
	es = profileEffects(base, with(func(p *config.Profile) { p.Radio = config.RadioPlan{Band: "5g", Channel: 149, Width: 20} }), false, ap)
	if !hasEffect(es, "warning", "ap-band") {
		t.Errorf("shared AP band change: %+v", es)
	}
	// A rename alone concerns nobody else.
	es = profileEffects(base, with(func(p *config.Profile) { p.Name = "bravo" }), false, view)
	if len(es) != 1 || es[0].Code != "mesh-name" {
		t.Errorf("rename: %+v", es)
	}
}

// A change read back at once is the change, not the configuration it
// replaced: the reload is accepted when the files are written, and swapped
// in on the next pass, which may not have run yet.
func TestConfigReadBackBeforeNextPass(t *testing.T) {
	d, _ := testDaemon(t, map[string]string{
		"node.yaml":    "schema: 0\ncallsign: node-1\n",
		"profile.yaml": testProfile,
	})
	n := d.Config().Node
	n.Callsign = "relay-7"
	raw, _ := json.Marshal(n)
	if res, err := d.SetNode(context.Background(), raw, "", false); err != nil || !res.Applied {
		t.Fatalf("save: %+v %v", res, err)
	}
	if got, _ := d.conf(); got.Callsign != "node-1" {
		t.Fatalf("the running configuration changed before a pass: %s", got.Callsign)
	}
	if got := d.Config().Node.Callsign; got != "relay-7" {
		t.Errorf("read back right after saving: callsign %q, want relay-7", got)
	}
	d.applyPending()
	if got := d.Config().Node.Callsign; got != "relay-7" {
		t.Errorf("after the pass: callsign %q, want relay-7", got)
	}
}

// A node with one radio on its second link: a change to a third link it has
// no radio for, now or after, does nothing to it (it once read as leaving
// the first link for the third).
func TestProfileEffectsLinkNotCarried(t *testing.T) {
	cur, err := config.ParseProfile([]byte(testProfile))
	if err != nil {
		t.Fatal(err)
	}
	cur.Radio = config.RadioPlan{Band: "2g", Channel: 6, Width: 20}
	cur.Links = []config.RadioPlan{{Band: "5g", Channel: 149, Width: 80}, {Band: "s1g", Channel: 26, Width: 2}}
	p := *cur
	p.Links = []config.RadioPlan{{Band: "5g", Channel: 149, Width: 80}, {Band: "s1g", Channel: 30, Width: 2}}
	on5 := func(i int) bool { return i == 1 } // the one radio, on 5 GHz, both before and after
	v := ProfileView{Peers: [][]string{nil, {"relay-2"}, nil}, Has: on5, Carries: on5}
	es := profileEffects(cur, &p, false, v)
	for _, e := range es {
		if e.Level != "info" {
			t.Errorf("unexpected %s %s: %s", e.Level, e.Code, e.Message)
		}
		if strings.Contains(e.Message, "takes the node off") || strings.Contains(e.Message, "no radio left") {
			t.Errorf("claims a change for this node: %s", e.Message)
		}
	}
	if !hasEffect(es, "info", "link-added") {
		t.Errorf("the new HaLow link is not mentioned: %+v", es)
	}
}

// A profile code holds neither talk groups nor the site link. Installed
// or sent on the mesh it is the code of, it keeps the node's; for another
// mesh it brings none.
func TestProfileCodeKeepsWhatItCannotCarry(t *testing.T) {
	const more = `voice:
  groups:
    - {number: 1, name: Command}
site_link:
  members:
    - {name: hq-1, site: hq, public_key: "q2Lw8mJ4c1vN0xTfR7yH3kPzS5aG9dE6uB4iO2nM1lY="}
`
	d, _ := testDaemon(t, map[string]string{"node.yaml": "schema: 0\ncallsign: node-1\n", "profile.yaml": testProfile + more})
	ctx := context.Background()
	if cur := d.Config().Profile; len(cur.Voice.Groups) != 1 || len(cur.SiteLink.Members) != 1 {
		t.Fatalf("the test profile: %d groups, %d members", len(cur.Voice.Groups), len(cur.SiteLink.Members))
	}
	ex, err := d.ProfileExport()
	if err != nil {
		t.Fatal(err)
	}
	// The mesh's own code changes nothing: not "the members leave".
	res, err := d.SetProfile(ctx, api.ProfileInput{Code: ex.Code}, true)
	if err != nil || res.Changed {
		t.Fatalf("the mesh's own code, this node: %+v %v", res, err)
	}
	// Sent to every node it is read the same way (profileFrom).
	_, cur := d.conf()
	p, err := d.profileFrom(api.ProfileInput{Code: ex.Code}, cur, false)
	if err != nil || len(p.Voice.Groups) != 1 || len(p.SiteLink.Members) != 1 {
		t.Fatalf("the mesh's own code: %+v %v", p, err)
	}
	// Another mesh's code brings neither.
	other, err := config.ParseProfile([]byte(testProfile))
	if err != nil {
		t.Fatal(err)
	}
	other.MeshID = "another-mesh"
	code, err := other.Code()
	if err != nil {
		t.Fatal(err)
	}
	p, err = d.profileFrom(api.ProfileInput{Code: code}, cur, false)
	if err != nil || len(p.Voice.Groups) != 0 || len(p.SiteLink.Members) != 0 {
		t.Fatalf("another mesh's code: %+v %v", p, err)
	}
}
