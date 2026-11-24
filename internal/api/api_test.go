package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"meridian/trellis/internal/account"
	"meridian/trellis/internal/config"
	"meridian/trellis/internal/osupdate"
	"meridian/trellis/internal/status"
)

type fakeProvider struct {
	kicks      int
	node       []byte
	dry        bool
	privileged *bool
	identified string
	pkg        []byte
	pkgDry     bool
	rollout    RolloutRequest
	from       string
}

func (f *fakeProvider) UpgradeState(ctx context.Context) (*UpgradeState, error) {
	return &UpgradeState{Version: "test", Package: "0.3.0"}, nil
}
func (f *fakeProvider) Upgrade(ctx context.Context, in UpgradeRequest) (*UpgradeCheck, error) {
	f.pkg, f.pkgDry = in.Package, in.DryRun
	return &UpgradeCheck{From: "0.3.0", To: "0.3.1", Started: !in.DryRun, SHA256: in.SHA256}, nil
}
func (f *fakeProvider) UpgradeRollback(ctx context.Context) error {
	return Errorf(http.StatusConflict, "there is no upgrade to go back from")
}
func (f *fakeProvider) RolloutPackage(ctx context.Context, pkg []byte) (*RolloutPackage, error) {
	f.pkg = pkg
	return &RolloutPackage{SHA256: "ab", Arch: "arm64", Version: "0.3.1", Size: int64(len(pkg))}, nil
}
func (f *fakeProvider) Rollout(ctx context.Context, in RolloutRequest) (*RolloutPlan, error) {
	f.rollout = in
	return &RolloutPlan{Version: "0.3.1", Started: !in.DryRun}, nil
}
func (f *fakeProvider) CancelRollout(ctx context.Context) error { return nil }

func (f *fakeProvider) Status(ctx context.Context) (*status.Status, error) {
	return &status.Status{Version: "test", Node: status.Node{Callsign: "n1", NodeID: 7}}, nil
}
func (f *fakeProvider) Config() ConfigResult {
	return ConfigResult{Node: config.Node{Callsign: "n1"}, Profile: config.Profile{Name: "p", SAEPassphrase: config.Redacted}}
}
func (f *fakeProvider) Kick(string)   { f.kicks++ }
func (f *fakeProvider) Reload() error { return errors.New("bad config: callsign: required") }
func (f *fakeProvider) APJoin() (*status.APJoin, error) {
	return &status.APJoin{Enabled: true, SSID: "node-1", Passphrase: "secret12", WiFiQR: "WIFI:T:WPA;S:node-1;P:secret12;;"}, nil
}
func (f *fakeProvider) SetNode(ctx context.Context, raw []byte, base string, dryRun bool) (*ApplyResult, error) {
	f.node, f.dry = raw, dryRun
	n, err := config.ParseNodeJSON(raw)
	if err != nil {
		return nil, err
	}
	return &ApplyResult{Applied: !dryRun, Changed: true, Effects: []status.Warning{{Level: "info", Code: "renamed", Message: n.Callsign}}}, nil
}
func (f *fakeProvider) ProfileExport() (*ProfileExport, error) {
	return &ProfileExport{Code: "TRP1:abc", YAML: "name: p\n"}, nil
}
func (f *fakeProvider) Survey() *status.Survey { return nil }
func (f *fakeProvider) StartSurvey(ctx context.Context) (*status.Survey, error) {
	return &status.Survey{}, nil
}
func (f *fakeProvider) SetMeshProfile(ctx context.Context, in MeshProfileInput, dryRun bool) (*ApplyResult, error) {
	return &ApplyResult{Applied: !dryRun, Changed: true, Revision: 2, Nodes: []string{"node-2"}}, nil
}

func (f *fakeProvider) SetProfile(ctx context.Context, in ProfileInput, dryRun bool) (*ApplyResult, error) {
	if in.Code == "" {
		return nil, Invalid("give the profile in exactly one form")
	}
	return &ApplyResult{Applied: !dryRun, Changed: true}, nil
}
func (f *fakeProvider) Setup(ctx context.Context, in SetupInput, dryRun bool) (*ApplyResult, error) {
	return &ApplyResult{Applied: !dryRun, Changed: true, AP: &status.APJoin{SSID: in.Callsign}}, nil
}
func (f *fakeProvider) OSUpdates(ctx context.Context) (*osupdate.State, error) {
	return &osupdate.State{}, nil
}
func (f *fakeProvider) StartOSUpdate(ctx context.Context, op string) (*osupdate.State, error) {
	return &osupdate.State{Phase: osupdate.PhaseChecking}, nil
}
func (f *fakeProvider) Account(ctx context.Context) (*account.State, error) {
	return &account.State{Name: account.Name}, nil
}
func (f *fakeProvider) SetAccount(ctx context.Context, in AccountInput, dryRun bool) (*account.State, error) {
	return &account.State{Name: account.Name, Exists: !dryRun}, nil
}
func (f *fakeProvider) GitHubKeys(ctx context.Context, user string) ([]account.Key, error) {
	return []account.Key{{Type: "ssh-ed25519", Comment: "github:" + user}}, nil
}
func (f *fakeProvider) Topology(ctx context.Context) (*status.Topology, error) {
	return &status.Topology{Nodes: []status.TopoNode{{ID: "02:00:00:00:00:01", Self: true}}, Links: []status.TopoLink{}}, nil
}
func (f *fakeProvider) SiteLinkNewKey(force bool) (*SiteKey, error) {
	return &SiteKey{PublicKey: "new", Was: "old"}, nil
}

func (f *fakeProvider) IdentifyRadio(fp string, seconds int) error {
	if fp == "" {
		return Errorf(http.StatusNotFound, "no radio with fingerprint %q is present", fp)
	}
	f.identified = fp
	return nil
}
func (f *fakeProvider) Doctor(ctx context.Context) (string, error) { return "report", nil }
func (f *fakeProvider) WebInfo() WebInfo {
	return WebInfo{Callsign: "n1", AdminSet: true, Prefix: "10.73.0.0/16"}
}
func (f *fakeProvider) WebLogin(from, password string) error {
	f.from = from
	switch password {
	case "right-password":
		return nil
	case "locked":
		return TooMany(7*time.Second, "too many wrong passwords")
	}
	return ErrBadPassword
}
func (f *fakeProvider) WebSetPassword(from, current, new string, privileged bool) error {
	f.privileged = &privileged
	return nil
}

func startServer(t *testing.T) (*fakeProvider, *Client, context.Context) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "m.sock")
	p := &fakeProvider{}
	srv := &Server{Provider: p, Socket: sock}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go srv.Serve(ctx)
	c := NewClient(sock)
	deadline := time.Now().Add(3 * time.Second)
	for c.Health(ctx) != nil {
		if time.Now().After(deadline) {
			t.Fatal("server did not come up")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return p, c, ctx
}

func TestServerAndClient(t *testing.T) {
	p, c, ctx := startServer(t)
	st, err := c.Status(ctx)
	if err != nil || st.Node.NodeID != 7 {
		t.Fatalf("status: %+v %v", st, err)
	}
	if err := c.Reconcile(ctx); err != nil || p.kicks != 1 {
		t.Fatalf("reconcile: %v kicks=%d", err, p.kicks)
	}
	err = c.Reload(ctx)
	var ae *Error
	if !errors.As(err, &ae) || ae.Status != http.StatusBadRequest || ae.Message != "bad config: callsign: required" {
		t.Errorf("reload error not propagated: %v", err)
	}
	cfg, err := c.Config(ctx)
	if err != nil || cfg.Profile.SAEPassphrase != config.Redacted || cfg.Node.Callsign != "n1" {
		t.Errorf("config: %+v %v", cfg, err)
	}
	topo, err := c.Topology(ctx)
	if err != nil || len(topo.Nodes) != 1 || !topo.Nodes[0].Self {
		t.Errorf("topology: %+v %v", topo, err)
	}
	if ex, err := c.ProfileExport(ctx); err != nil || ex.Code != "TRP1:abc" {
		t.Errorf("profile export: %+v %v", ex, err)
	}
	if wi, err := c.WebInfo(ctx); err != nil || !wi.AdminSet || wi.Prefix != "10.73.0.0/16" {
		t.Errorf("web info: %+v %v", wi, err)
	}
	var ch Channels
	if err := c.do(ctx, http.MethodGet, "/channels", nil, &ch); err != nil {
		t.Fatalf("channels: %v", err)
	}
	if !slices.Equal(ch.Widths["5g"], []int{20, 40, 80, 160}) || !slices.Equal(ch.Widths["2g"], []int{20, 40}) || ch.MaxChannel["5g"] != 196 ||
		!slices.Equal(ch.S1G["US"][8], []int{12, 28, 44}) || len(ch.S1G["DE"][2]) != 0 || len(ch.S1G["DE"][1]) == 0 {
		t.Errorf("channels: %+v", ch)
	}
}

func TestConfigChanges(t *testing.T) {
	p, c, ctx := startServer(t)
	n := config.Node{Callsign: "node-9"}
	n.ApplyDefaults()
	res, err := c.SetNode(ctx, n, true)
	if err != nil || res.Applied || !p.dry || !strings.Contains(string(p.node), `"callsign":"node-9"`) {
		t.Fatalf("dry run: %+v %v dry=%v body=%s", res, err, p.dry, p.node)
	}
	res, err = c.SetNode(ctx, n, false)
	if err != nil || !res.Applied || p.dry || len(res.Effects) != 1 || res.Effects[0].Message != "node-9" {
		t.Fatalf("apply: %+v %v", res, err)
	}
	// A rejected configuration is a 422 that names each field.
	n.Callsign, n.NodeID = "Not Valid", 999
	_, err = c.SetNode(ctx, n, false)
	var ae *Error
	if !errors.As(err, &ae) || ae.Status != http.StatusUnprocessableEntity || len(ae.Fields) != 2 || ae.Fields[0].Field != "callsign" || ae.Fields[1].Field != "node_id" {
		t.Fatalf("invalid node: %+v", err)
	}
	if _, err := c.SetProfile(ctx, ProfileInput{}); !errors.As(err, &ae) || ae.Status != http.StatusBadRequest {
		t.Errorf("empty profile input: %v", err)
	}
	if res, err := c.SetProfile(ctx, ProfileInput{Code: "TRP1:x"}); err != nil || !res.Applied {
		t.Errorf("profile: %+v %v", res, err)
	}

	// Unknown fields in a request body are refused, not ignored.
	code, _, body, err := c.Raw(ctx, http.MethodPost, "/setup", "dry_run=1", []byte(`{"callsign":"n2","code":"TRP1:x","colour":"red"}`))
	if err != nil || code != http.StatusBadRequest || !strings.Contains(string(body), "colour") {
		t.Errorf("unknown field: %d %s %v", code, body, err)
	}
	code, _, body, err = c.Raw(ctx, http.MethodPost, "/setup", "dry_run=1", []byte(`{"callsign":"n2","code":"TRP1:x"}`))
	if err != nil || code != http.StatusOK || !strings.Contains(string(body), `"ssid": "n2"`) || strings.Contains(string(body), `"applied": true`) {
		t.Errorf("setup dry run: %d %s %v", code, body, err)
	}
	code, _, body, _ = c.Raw(ctx, http.MethodPost, "/radios/identify", "", []byte(`{"fingerprint":"mac:00:11:22:33:44:55","seconds":5}`))
	if code != http.StatusOK || p.identified != "mac:00:11:22:33:44:55" {
		t.Errorf("identify: %d %s", code, body)
	}
	code, _, body, _ = c.Raw(ctx, http.MethodPost, "/radios/identify", "", []byte(`{}`))
	if code != http.StatusNotFound || !strings.Contains(string(body), `"error"`) {
		t.Errorf("identify unknown radio: %d %s", code, body)
	}
	// Anything else is a JSON 404 that says which API this is.
	code, ctype, body, _ := c.Raw(ctx, http.MethodGet, "/nope", "", nil)
	if code != http.StatusNotFound || ctype != "application/json" || !strings.Contains(string(body), "v1") {
		t.Errorf("unknown path: %d %s %s", code, ctype, body)
	}
}

func TestWebAuthEndpoints(t *testing.T) {
	p, c, ctx := startServer(t)
	if err := c.WebLogin(ctx, "10.73.5.9", "right-password"); err != nil || p.from != "10.73.5.9" {
		t.Errorf("login: %v, from %q", err, p.from)
	}
	var ae *Error
	if err := c.WebLogin(ctx, "", "wrong"); !errors.As(err, &ae) || ae.Status != http.StatusUnauthorized {
		t.Errorf("wrong password: %v", err)
	}
	if err := c.WebLogin(ctx, "", "locked"); !errors.As(err, &ae) || ae.Status != http.StatusTooManyRequests || ae.RetryAfter != 8 {
		t.Errorf("lockout: %+v", err)
	}
	// Only root on the socket is privileged: the server reads the peer's
	// uid from the connection, the request cannot claim it.
	if err := c.WebSetPassword(ctx, "", "", "new-password"); err != nil || p.privileged == nil {
		t.Fatalf("set password: %v", err)
	}
	if want := os.Getuid() == 0; *p.privileged != want {
		t.Errorf("privileged = %v for uid %d", *p.privileged, os.Getuid())
	}
}

// A release package is far larger than any other request, and reaches the
// provider byte for byte.
func TestUpgradeEndpoints(t *testing.T) {
	p, c, ctx := startServer(t)
	pkg := make([]byte, 3<<20)
	for i := range pkg {
		pkg[i] = byte(i * 7)
	}
	code, _, body, err := c.Raw(ctx, "POST", "/upgrade", "dry_run=1", pkg)
	if err != nil || code != 200 || !strings.Contains(string(body), `"to": "0.3.1"`) {
		t.Fatalf("upgrade dry run: %d %s %v", code, body, err)
	}
	if !p.pkgDry || !bytes.Equal(p.pkg, pkg) {
		t.Fatalf("the package did not arrive intact (dry %v, %d bytes)", p.pkgDry, len(p.pkg))
	}
	// Starting by the hash of the package checked before: no package is sent.
	if code, _, body, _ := c.Raw(ctx, "POST", "/upgrade", "sha256=abc123", []byte("{}")); code != 200 || p.pkg != nil || !strings.Contains(string(body), `"sha256": "abc123"`) {
		t.Errorf("start by hash: %d %s, package %d bytes", code, body, len(p.pkg))
	}
	if code, _, body, _ := c.Raw(ctx, "GET", "/upgrade", "", nil); code != 200 || !strings.Contains(string(body), `"package": "0.3.0"`) {
		t.Errorf("upgrade state: %d %s", code, body)
	}
	if code, _, _, _ := c.Raw(ctx, "POST", "/upgrade/rollback", "", []byte("{}")); code != http.StatusConflict {
		t.Errorf("rollback refusal: %d", code)
	}
	// Other endpoints keep their small limit.
	if code, _, _, _ := c.Raw(ctx, "PUT", "/config/node", "", pkg); code != http.StatusBadRequest {
		t.Errorf("a 3 MB node configuration was taken: %d", code)
	}
}

func TestClientNotRunning(t *testing.T) {
	c := NewClient(filepath.Join(t.TempDir(), "none.sock"))
	err := c.Health(context.Background())
	if !errors.Is(err, ErrNotRunning) {
		t.Errorf("expected ErrNotRunning, got %v", err)
	}
}

func (p *fakeProvider) VoiceConfig() VoiceConfig { return VoiceConfig{Config: []byte("{}")} }
func (p *fakeProvider) Voice(context.Context) (*VoiceState, error) {
	return &VoiceState{}, nil
}
func (p *fakeProvider) VoiceTest(context.Context, VoiceTestInput) error { return nil }
