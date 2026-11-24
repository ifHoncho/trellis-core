package webd

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/x509"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"meridian/trellis/internal/account"
	"meridian/trellis/internal/api"
	"meridian/trellis/internal/osupdate"
	"meridian/trellis/internal/status"
)

// node is a stand-in for meshd behind its real API server.
type node struct {
	mu       sync.Mutex
	since    time.Time
	password string
	setup    bool
	wan      bool
	changes  int
	pkgLen   int
	from     string // the source of the last password check
}

func (n *node) UpgradeState(ctx context.Context) (*api.UpgradeState, error) {
	return &api.UpgradeState{Version: "test"}, nil
}
func (n *node) Upgrade(ctx context.Context, in api.UpgradeRequest) (*api.UpgradeCheck, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.pkgLen = len(in.Package)
	return &api.UpgradeCheck{From: "0.3.0", To: "0.3.1"}, nil
}
func (n *node) UpgradeRollback(ctx context.Context) error { return nil }
func (n *node) RolloutPackage(ctx context.Context, pkg []byte) (*api.RolloutPackage, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.pkgLen = len(pkg)
	return &api.RolloutPackage{SHA256: "ab", Arch: "arm64", Version: "0.3.1"}, nil
}
func (n *node) Rollout(ctx context.Context, in api.RolloutRequest) (*api.RolloutPlan, error) {
	return &api.RolloutPlan{Version: "0.3.1"}, nil
}
func (n *node) CancelRollout(ctx context.Context) error { return nil }

func (n *node) Status(ctx context.Context) (*status.Status, error) {
	return &status.Status{Version: "test", Node: status.Node{Callsign: "n1", NodeID: 7}}, nil
}
func (n *node) Config() api.ConfigResult { return api.ConfigResult{} }
func (n *node) Kick(string)              {}
func (n *node) Reload() error            { return nil }
func (n *node) APJoin() (*status.APJoin, error) {
	return &status.APJoin{SSID: "n1", Passphrase: "wifi-secret"}, nil
}
func (n *node) SetNode(ctx context.Context, raw []byte, base string, dry bool) (*api.ApplyResult, error) {
	n.mu.Lock()
	n.changes++
	n.mu.Unlock()
	return &api.ApplyResult{Applied: !dry, Changed: true}, nil
}
func (n *node) ProfileExport() (*api.ProfileExport, error) {
	return &api.ProfileExport{Code: "TRP1:secret"}, nil
}
func (n *node) SetProfile(ctx context.Context, in api.ProfileInput, dry bool) (*api.ApplyResult, error) {
	return &api.ApplyResult{}, nil
}
func (n *node) Survey() *status.Survey { return nil }
func (n *node) StartSurvey(ctx context.Context) (*status.Survey, error) {
	return &status.Survey{}, nil
}
func (n *node) SetMeshProfile(ctx context.Context, in api.MeshProfileInput, dry bool) (*api.ApplyResult, error) {
	return &api.ApplyResult{}, nil
}
func (n *node) Setup(ctx context.Context, in api.SetupInput, dry bool) (*api.ApplyResult, error) {
	return &api.ApplyResult{}, nil
}
func (n *node) OSUpdates(ctx context.Context) (*osupdate.State, error) { return &osupdate.State{}, nil }
func (n *node) StartOSUpdate(ctx context.Context, op string) (*osupdate.State, error) {
	return &osupdate.State{}, nil
}
func (n *node) Account(ctx context.Context) (*account.State, error) {
	return &account.State{Name: account.Name}, nil
}
func (n *node) SetAccount(ctx context.Context, in api.AccountInput, dry bool) (*account.State, error) {
	return &account.State{Name: account.Name}, nil
}
func (n *node) GitHubKeys(ctx context.Context, user string) ([]account.Key, error) { return nil, nil }
func (n *node) Topology(ctx context.Context) (*status.Topology, error) {
	return &status.Topology{}, nil
}
func (n *node) IdentifyRadio(string, int) error           { return nil }
func (n *node) SiteLinkNewKey(bool) (*api.SiteKey, error) { return nil, nil }
func (n *node) Doctor(ctx context.Context) (string, error) {
	return strings.Repeat("report line\n", 200), nil
}
func (n *node) WebInfo() api.WebInfo {
	n.mu.Lock()
	defer n.mu.Unlock()
	return api.WebInfo{Callsign: "n1", AdminSet: n.password != "", AdminSince: n.since, Setup: n.setup, WANAccess: n.wan, Prefix: "10.73.0.0/16"}
}
func (n *node) WebLogin(from, pw string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.from = from
	if n.password == "" {
		return api.Forbidden("no admin password is set on this node")
	}
	if pw != n.password {
		return api.ErrBadPassword
	}
	return nil
}
func (n *node) WebSetPassword(from, current, new string, privileged bool) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.from = from
	if n.password != "" && current != n.password {
		return api.Errorf(401, "the current password is wrong")
	}
	if n.password == "" && !n.setup {
		return api.Forbidden("set one on the node itself")
	}
	n.password, n.since = new, time.Now()
	return nil
}

// start runs a node's API on a socket and a webd in front of it.
func start(t *testing.T, n *node) (*httptest.Server, *Server) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "m.sock")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if n != nil {
		go (&api.Server{Provider: n, Socket: sock}).Serve(ctx)
		c := api.NewClient(sock)
		for deadline := time.Now().Add(3 * time.Second); c.Health(ctx) != nil; time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("the API server did not come up")
			}
		}
	}
	s, err := New(Options{Version: "test", Socket: sock, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	go s.hub.run(ctx)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, s
}

// call makes one request; cookie is a session cookie value or "".
func call(t *testing.T, ts *httptest.Server, method, p, body, cookie string, hdr ...string) (*http.Response, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, ts.URL+p, rd)
	if method != http.MethodGet {
		req.Header.Set(requestHeader, "1")
	}
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i+1] == "" {
			req.Header.Del(hdr[i])
		} else {
			req.Header.Set(hdr[i], hdr[i+1])
		}
	}
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	return res, string(data)
}

func sessionOf(res *http.Response) *http.Cookie {
	for _, c := range res.Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	return nil
}

func login(t *testing.T, ts *httptest.Server, pw string) string {
	t.Helper()
	res, body := call(t, ts, "POST", "/api/v1/login", `{"password":"`+pw+`"}`, "")
	c := sessionOf(res)
	if res.StatusCode != 200 || c == nil {
		t.Fatalf("login: %d %s", res.StatusCode, body)
	}
	return c.Value
}

func TestLoginAndProxy(t *testing.T) {
	n := &node{password: "correct-horse"}
	ts, _ := start(t, n)

	// Nothing of the node is readable before signing in.
	for _, p := range []string{"/api/v1/status", "/api/v1/config", "/api/v1/profile/export", "/api/v1/ap", "/api/v1/events", "/api/v1/topology"} {
		if res, body := call(t, ts, "GET", p, "", ""); res.StatusCode != 401 || strings.Contains(body, "secret") {
			t.Errorf("%s without a session: %d %s", p, res.StatusCode, body)
		}
	}
	res, body := call(t, ts, "GET", "/api/v1/session", "", "")
	var si SessionInfo
	json.Unmarshal([]byte(body), &si)
	if res.StatusCode != 200 || si.Authenticated || !si.AdminSet || si.Setup || !si.MeshdUp || si.Callsign != "n1" {
		t.Errorf("session before login: %d %+v", res.StatusCode, si)
	}

	// meshd's lockout is per source: webd names the connection's address,
	// whatever the browser claims.
	if res, _ := call(t, ts, "POST", "/api/v1/login", `{"password":"wrong","from":"10.73.1.1"}`, ""); res.StatusCode != 401 || sessionOf(res) != nil {
		t.Errorf("wrong password: %d", res.StatusCode)
	}
	if n.from != "127.0.0.1" {
		t.Errorf("meshd was told the attempt came from %q, not the connection's address", n.from)
	}
	res, _ = call(t, ts, "POST", "/api/v1/login", `{"password":"correct-horse"}`, "")
	c := sessionOf(res)
	if res.StatusCode != 200 || c == nil || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || len(c.Value) < 40 {
		t.Fatalf("login: %d cookie %+v", res.StatusCode, c)
	}
	if res, body := call(t, ts, "GET", "/api/v1/status", "", c.Value); res.StatusCode != 200 || !strings.Contains(body, `"callsign": "n1"`) {
		t.Errorf("status with a session: %d %s", res.StatusCode, body)
	}
	if res, body := call(t, ts, "GET", "/api/v1/session", "", c.Value); !strings.Contains(body, `"authenticated":true`) {
		t.Errorf("session after login: %d %s", res.StatusCode, body)
	}
	// A change reaches meshd with its query, and large answers are compressed.
	if res, body := call(t, ts, "PUT", "/api/v1/config/node?dry_run=1", `{"callsign":"n2"}`, c.Value); res.StatusCode != 200 || !strings.Contains(body, `"applied": false`) || n.changes != 1 {
		t.Errorf("proxied change: %d %s", res.StatusCode, body)
	}
	res, _ = call(t, ts, "GET", "/api/v1/doctor", "", c.Value, "Accept-Encoding", "gzip")
	if res.Header.Get("Content-Encoding") != "gzip" {
		t.Errorf("a large answer was not compressed: %v", res.Header)
	}
	// meshd's password endpoints are not reachable around the login handler.
	for _, p := range []string{"/api/v1/web", "/api/v1/web/login", "/api/v1/web/password"} {
		if res, _ := call(t, ts, "POST", p, `{"password":"x"}`, c.Value); res.StatusCode != 404 {
			t.Errorf("%s through the proxy: %d", p, res.StatusCode)
		}
	}
	// Nor written another way: escapes are decoded once by webd, and must
	// not be decoded again into a /web path on meshd's side.
	n.mu.Lock()
	n.from = "untouched"
	n.mu.Unlock()
	for _, p := range []string{"/api/v1/web%2Flogin", "/api/v1/web%252Flogin", "/api/v1/%77eb/login", "/api/v1/x/..%2Fweb/login",
		"/api/v1/web%252fpassword", "/api/v1/status%3Fx", "/api/v1/.%2Fweb/login", "/api/v1/%2Fweb/login"} {
		for _, m := range []string{"POST", "PUT"} {
			if res, body := call(t, ts, m, p, `{"password":"x","new":"xxxxxxxxxx"}`, c.Value); res.StatusCode < 300 {
				t.Errorf("%s %s through the proxy: %d %s", m, p, res.StatusCode, body)
			}
		}
	}
	n.mu.Lock()
	if n.from != "untouched" {
		t.Errorf("a password endpoint was reached through the proxy (from %q)", n.from)
	}
	n.mu.Unlock()
	// A release package passes the proxy whole; nothing else may be large.
	big := strings.Repeat("x", 3<<20)
	if res, body := call(t, ts, "POST", "/api/v1/upgrade?dry_run=1", big, c.Value); res.StatusCode != 200 || n.pkgLen != len(big) {
		t.Errorf("package upload: %d %s, %d bytes reached meshd", res.StatusCode, body, n.pkgLen)
	}
	if res, _ := call(t, ts, "PUT", "/api/v1/config/node", big, c.Value); res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("a 3 MB configuration passed the proxy: %d", res.StatusCode)
	}
	// Signing out ends the session.
	call(t, ts, "POST", "/api/v1/logout", "", c.Value)
	if res, _ := call(t, ts, "GET", "/api/v1/status", "", c.Value); res.StatusCode != 401 {
		t.Errorf("after logout: %d", res.StatusCode)
	}
}

func TestCrossSiteRequestsRefused(t *testing.T) {
	n := &node{password: "correct-horse"}
	ts, _ := start(t, n)
	cookie := login(t, ts, "correct-horse")
	// A form on another site can send the cookie, but not the header.
	if res, _ := call(t, ts, "PUT", "/api/v1/config/node", `{"callsign":"evil"}`, cookie, requestHeader, ""); res.StatusCode != 403 || n.changes != 0 {
		t.Errorf("no request header: %d, %d changes", res.StatusCode, n.changes)
	}
	if res, _ := call(t, ts, "PUT", "/api/v1/config/node", `{"callsign":"evil"}`, cookie, "Origin", "https://evil.example"); res.StatusCode != 403 || n.changes != 0 {
		t.Errorf("foreign origin: %d, %d changes", res.StatusCode, n.changes)
	}
	host := strings.TrimPrefix(ts.URL, "http://")
	if res, _ := call(t, ts, "PUT", "/api/v1/config/node", `{"callsign":"ok"}`, cookie, "Origin", "http://"+host); res.StatusCode != 200 {
		t.Errorf("own origin: %d", res.StatusCode)
	}
	if res, _ := call(t, ts, "POST", "/api/v1/login", `{"password":"correct-horse"}`, "", requestHeader, ""); res.StatusCode != 403 {
		t.Errorf("login without the request header: %d", res.StatusCode)
	}
}

func TestPasswordRules(t *testing.T) {
	// During setup anyone may set the first password, and is signed in.
	n := &node{setup: true}
	ts, s := start(t, n)
	res, body := call(t, ts, "PUT", "/api/v1/password", `{"new":"first-password"}`, "")
	first := sessionOf(res)
	if res.StatusCode != 200 || first == nil || n.password != "first-password" {
		t.Fatalf("first password: %d %s", res.StatusCode, body)
	}
	// From then on it takes a session and the current password.
	if res, _ := call(t, ts, "PUT", "/api/v1/password", `{"current":"first-password","new":"taken-over"}`, ""); res.StatusCode != 401 {
		t.Errorf("change without a session: %d", res.StatusCode)
	}
	if res, _ := call(t, ts, "PUT", "/api/v1/password", `{"current":"nope","new":"second-password"}`, first.Value); res.StatusCode != 401 {
		t.Errorf("change with a wrong current password: %d", res.StatusCode)
	}
	other := login(t, ts, "first-password")
	res, _ = call(t, ts, "PUT", "/api/v1/password", `{"current":"first-password","new":"second-password"}`, first.Value)
	fresh := sessionOf(res)
	if res.StatusCode != 200 || fresh == nil || n.password != "second-password" {
		t.Fatalf("change: %d", res.StatusCode)
	}
	// Every older session ends; the browser that changed it has a new one.
	for name, c := range map[string]string{"the changing browser's old session": first.Value, "another browser": other} {
		if res, _ := call(t, ts, "GET", "/api/v1/status", "", c); res.StatusCode != 401 {
			t.Errorf("%s still works: %d", name, res.StatusCode)
		}
	}
	if res, _ := call(t, ts, "GET", "/api/v1/status", "", fresh.Value); res.StatusCode != 200 {
		t.Errorf("the new session: %d", res.StatusCode)
	}

	// A password set on the node itself (meshctl web password) ends the
	// browser sessions too, once webd next asks meshd.
	n.mu.Lock()
	n.password, n.since = "set-with-meshctl", time.Now().Add(time.Second)
	n.mu.Unlock()
	s.forgetInfo()
	if res, _ := call(t, ts, "GET", "/api/v1/status", "", fresh.Value); res.StatusCode != 401 {
		t.Errorf("a session survived a password set behind webd's back: %d", res.StatusCode)
	}

	// Signing in with it straight away works: the new session is not
	// mistaken for one from before the change.
	n.mu.Lock()
	n.password, n.since = "set-again", time.Now().Add(2*time.Second)
	n.mu.Unlock()
	if res, _ := call(t, ts, "GET", "/api/v1/status", "", login(t, ts, "set-again")); res.StatusCode != 200 {
		t.Errorf("a session opened right after a password change was dropped: %d", res.StatusCode)
	}

	// A provisioned node with no password takes none from the network.
	ts2, _ := start(t, &node{})
	if res, _ := call(t, ts2, "PUT", "/api/v1/password", `{"new":"claimed-by-a-stranger"}`, ""); res.StatusCode != 403 {
		t.Errorf("first password on a provisioned node: %d", res.StatusCode)
	}
}

func TestEvents(t *testing.T) {
	ts, _ := start(t, &node{password: "correct-horse"})
	cookie := login(t, ts, "correct-horse")
	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/events", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	req.Header.Set("Accept-Encoding", "gzip")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := http.DefaultTransport.RoundTrip(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "text/event-stream" || res.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("events: %d %v", res.StatusCode, res.Header)
	}
	zr, err := gzip.NewReader(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var name string
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event: ") {
			name = strings.TrimPrefix(line, "event: ")
		}
		if strings.HasPrefix(line, "data: ") && name == "status" {
			var st status.Status
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &st); err != nil || st.Node.Callsign != "n1" {
				t.Errorf("status event: %v %s", err, line)
			}
			return
		}
	}
	t.Fatalf("no status event arrived: %v", sc.Err())
}

// The server's read and write timeouts bound ordinary requests; an event
// stream must outlive them.
func TestEventsOutliveServerTimeouts(t *testing.T) {
	_, s := start(t, &node{password: "correct-horse"})
	ts := httptest.NewUnstartedServer(s.Handler())
	ts.Config.ReadTimeout, ts.Config.WriteTimeout = 300*time.Millisecond, 300*time.Millisecond
	ts.Start()
	defer ts.Close()
	cookie := login(t, ts, "correct-horse")
	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/events", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := http.DefaultTransport.RoundTrip(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	began, events := time.Now(), 0
	for sc.Scan() {
		if sc.Text() == "event: status" {
			events++
		}
		if events >= 2 && time.Since(began) > time.Second {
			return
		}
	}
	t.Fatalf("the stream ended after %s with %d status events: %v", time.Since(began).Round(time.Millisecond), events, sc.Err())
}

// A release package may arrive slowly over the mesh: its upload outlives
// the server's read timeout, which still bounds every other request.
func TestSlowUploadOutlivesReadTimeout(t *testing.T) {
	n := &node{password: "correct-horse"}
	_, s := start(t, n)
	ts := httptest.NewUnstartedServer(s.Handler())
	ts.Config.ReadTimeout, ts.Config.WriteTimeout = 300*time.Millisecond, 300*time.Millisecond
	ts.Start()
	defer ts.Close()
	cookie := login(t, ts, "correct-horse")
	slow := func(path string) (*http.Response, error) {
		pr, pw := io.Pipe()
		go func() {
			for i := 0; i < 8; i++ {
				time.Sleep(100 * time.Millisecond)
				pw.Write(make([]byte, 1000))
			}
			pw.Close()
		}()
		req, _ := http.NewRequest("POST", ts.URL+path, pr)
		req.Header.Set(requestHeader, "1")
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
		return http.DefaultClient.Do(req)
	}
	res, err := slow("/api/v1/upgrade?dry_run=1")
	if err != nil || res.StatusCode != 200 || n.pkgLen != 8000 {
		t.Fatalf("slow upload: %v %v, %d bytes arrived", err, res, n.pkgLen)
	}
	res.Body.Close()
	if res, err := slow("/api/v1/reconcile"); err == nil && res.StatusCode == 200 {
		t.Error("a slow ordinary request outlived the read timeout")
	}
}

func TestMeshdDown(t *testing.T) {
	ts, s := start(t, nil)
	res, body := call(t, ts, "GET", "/api/v1/session", "", "")
	if res.StatusCode != 200 || !strings.Contains(body, `"meshd_up":false`) {
		t.Errorf("session with meshd down: %d %s", res.StatusCode, body)
	}
	if res, _ := call(t, ts, "POST", "/api/v1/login", `{"password":"x"}`, ""); res.StatusCode != 503 {
		t.Errorf("login with meshd down: %d", res.StatusCode)
	}
	token, _ := s.sessions.create()
	if res, _ := call(t, ts, "GET", "/api/v1/status", "", token); res.StatusCode != 503 {
		t.Errorf("status with meshd down: %d", res.StatusCode)
	}
	if ev := s.hub.poll(context.Background()); ev.name != "problem" || !strings.Contains(string(ev.data), "not running") {
		t.Errorf("hub with meshd down: %s %s", ev.name, ev.data)
	}
	// The UI itself still loads, to say so.
	if res, _ := call(t, ts, "GET", "/", "", ""); res.StatusCode != 200 {
		t.Errorf("index with meshd down: %d", res.StatusCode)
	}
}

func TestAssets(t *testing.T) {
	ts, _ := start(t, &node{password: "x"})
	res, body := call(t, ts, "GET", "/", "", "")
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/html") || !strings.Contains(body, "js/app.js") {
		t.Fatalf("index: %d", res.StatusCode)
	}
	csp := res.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'self'") || strings.Contains(csp, "unsafe") || res.Header.Get("X-Frame-Options") != "DENY" {
		t.Errorf("security headers: %v", res.Header)
	}
	if res.Header.Get("Strict-Transport-Security") != "" {
		t.Error("HSTS would make a self-signed certificate impossible to accept")
	}
	etag := res.Header.Get("ETag")
	if res, _ := call(t, ts, "GET", "/", "", "", "If-None-Match", etag); res.StatusCode != 304 {
		t.Errorf("revalidation: %d", res.StatusCode)
	}
	if res, _ := call(t, ts, "GET", "/app.css", "", "", "Accept-Encoding", "gzip"); res.Header.Get("Content-Encoding") != "gzip" {
		t.Errorf("stylesheet not compressed: %v", res.Header)
	}
	if res, _ := call(t, ts, "GET", "/nope.js", "", ""); res.StatusCode != 404 {
		t.Errorf("missing file: %d", res.StatusCode)
	}
	if res, _ := call(t, ts, "POST", "/", "", ""); res.StatusCode != 405 {
		t.Errorf("POST to a file: %d", res.StatusCode)
	}
	if res, body := call(t, ts, "GET", "/api/v2/status", "", ""); res.StatusCode != 404 || !strings.Contains(body, "v1") {
		t.Errorf("unknown API version: %d %s", res.StatusCode, body)
	}
}

// Every file the UI refers to is in the binary: a module import, a
// stylesheet's font, a link in index.html. The UI has no build step to
// catch a mistyped path, so this does.
func TestUIFilesResolve(t *testing.T) {
	root, _ := fs.Sub(uiFS, "ui")
	exists := func(p string) bool { _, err := fs.Stat(root, p); return err == nil }
	refs := map[string]*regexp.Regexp{
		".js":   regexp.MustCompile(`(?m)^\s*import\s[^'"]*['"]([^'"]+)['"]`),
		".css":  regexp.MustCompile(`url\(["']?([^"')]+)["']?\)`),
		".html": regexp.MustCompile(`(?:src|href)="([^"#]+)"`),
	}
	checked := 0
	fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		re := refs[path.Ext(p)]
		if err != nil || d.IsDir() || re == nil {
			return err
		}
		data, _ := fs.ReadFile(root, p)
		for _, m := range re.FindAllStringSubmatch(string(data), -1) {
			if strings.Contains(m[1], ":") {
				t.Errorf("%s refers outside the node: %s", p, m[1])
				continue
			}
			target := path.Join(path.Dir(p), m[1])
			if !exists(target) {
				t.Errorf("%s refers to %s, which is not in the binary", p, target)
			}
			checked++
		}
		return nil
	})
	if checked < 30 {
		t.Errorf("only %d references found: the patterns no longer match the UI's files", checked)
	}
	// Nothing is loaded from elsewhere: a node often has no internet.
	outside := regexp.MustCompile(`(?:src=|href=|url\(|from\s+|import\(|fetch\()\s*["']?(?:https?:)?//`)
	fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasPrefix(p, "fonts/") {
			return err
		}
		data, _ := fs.ReadFile(root, p)
		if m := outside.Find(data); m != nil {
			t.Errorf("%s loads something from outside the node: %s…", p, m)
		}
		if strings.Contains(string(data), "eval(") {
			t.Errorf("%s uses eval, which the page's content security policy forbids", p)
		}
		return nil
	})
}

func TestAllowed(t *testing.T) {
	prefix := netip.MustParsePrefix("10.73.0.0/16")
	for _, c := range []struct {
		local, remote string
		wan, want     bool
		why           string
	}{
		{"10.73.0.1", "10.73.12.105", false, true, "a device to the anycast address"},
		{"10.73.12.1", "10.73.41.7", false, true, "across the mesh to the node address"},
		{"127.0.0.1", "127.0.0.1", false, true, "loopback"},
		{"::1", "::1", false, true, "IPv6 loopback"},
		{"::ffff:10.73.0.1", "::ffff:10.73.12.105", false, true, "IPv4 on a dual-stack socket"},
		{"192.168.8.114", "192.168.8.20", false, false, "the uplink network"},
		{"10.73.12.1", "192.168.8.20", false, false, "an uplink host addressing the mesh address"},
		{"192.168.8.114", "10.73.12.105", false, false, "a mesh device to the uplink address"},
		{"127.0.0.1", "192.168.8.20", false, false, "a remote peer on a loopback address"},
		{"192.168.8.114", "192.168.8.20", true, true, "the uplink network with wan_access"},
		{"203.0.113.9", "198.51.100.4", true, true, "anywhere with wan_access"},
	} {
		got := allowed(netip.MustParseAddr(c.local), netip.MustParseAddr(c.remote), prefix, c.wan)
		if got != c.want {
			t.Errorf("%s: %s <- %s (wan_access %v) = %v, want %v", c.why, c.local, c.remote, c.wan, got, c.want)
		}
	}
}

func TestConnectionsPerAddress(t *testing.T) {
	s := &Server{}
	a, b := netip.MustParseAddr("10.73.4.4"), netip.MustParseAddr("10.73.5.5")
	var held []net.Conn
	for i := 0; i < connsPerAddr; i++ {
		c1, c2 := net.Pipe()
		defer c2.Close()
		c := s.track(c1, a)
		if c == nil {
			t.Fatalf("connection %d refused under the limit", i+1)
		}
		held = append(held, c)
	}
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	if s.track(c1, a) != nil {
		t.Error("a connection over the limit was accepted")
	}
	if c := s.track(c1, b); c == nil {
		t.Error("another address was refused")
	} else {
		c.Close()
	}
	held[0].Close()
	held[0].Close() // a second close gives nothing back twice
	c3, c4 := net.Pipe()
	defer c4.Close()
	if c := s.track(c3, a); c == nil {
		t.Error("a closed connection's slot was not given back")
	} else {
		held[0] = c
	}
	if c := s.track(c3, a); c != nil {
		t.Error("a double close gave back two slots")
	}
	for _, c := range held {
		c.Close()
	}
	if len(s.conns) != 0 {
		t.Errorf("counts left after every connection closed: %v", s.conns)
	}
}

func TestGuardedListener(t *testing.T) {
	_, s := start(t, &node{password: "x"})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback listener:", err)
	}
	defer ln.Close()
	g := s.guard(ln)
	go func() {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err == nil {
			c.Close()
		}
	}()
	c, err := g.Accept()
	if err != nil {
		t.Fatalf("a loopback connection was not accepted: %v", err)
	}
	c.Close()
}

func TestSessions(t *testing.T) {
	s := newSessions()
	now := time.Now()
	s.now = func() time.Time { return now }
	tok, _ := s.create()
	if !s.valid(tok) || s.valid("") || s.valid("nope") {
		t.Fatal("basic validity")
	}
	now = now.Add(sessionIdle - time.Minute)
	if !s.valid(tok) {
		t.Error("expired before the idle limit")
	}
	now = now.Add(sessionIdle + time.Minute)
	if s.valid(tok) {
		t.Error("still valid after the idle limit")
	}
	// Used often enough, a session still ends at the absolute limit.
	tok, _ = s.create()
	for i := 0; i < int(sessionMax/time.Hour)+2; i++ {
		now = now.Add(time.Hour)
		if !s.valid(tok) {
			break
		}
	}
	if s.valid(tok) {
		t.Error("still valid after the absolute limit")
	}
	// The table is bounded: the least recently used session goes first.
	first, _ := s.create()
	for i := 0; i < sessionLimit; i++ {
		now = now.Add(time.Second)
		s.create()
	}
	if len(s.m) != sessionLimit || s.valid(first) {
		t.Errorf("%d sessions kept, oldest still valid: %v", len(s.m), s.valid(first))
	}
}

func TestCertificate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	names := func() ([]string, []net.IP) {
		return []string{"node.mesh", "n1.mesh"}, []net.IP{net.IPv4(10, 73, 0, 1)}
	}
	a, err := loadOrCreateCert(dir, names, log)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(a.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := leaf.VerifyHostname("n1.mesh"); err != nil {
		t.Error(err)
	}
	if err := leaf.VerifyHostname("10.73.0.1"); err != nil {
		t.Error(err)
	}
	if time.Until(leaf.NotAfter) < 9*365*24*time.Hour {
		t.Errorf("certificate expires %v", leaf.NotAfter)
	}
	if fi, err := os.Stat(filepath.Join(dir, keyFile)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("key file: %v", err)
	}
	// The same certificate after a restart: a browser's exception holds.
	b, err := loadOrCreateCert(dir, func() ([]string, []net.IP) { t.Error("made a new certificate"); return nil, nil }, log)
	if err != nil || string(a.Certificate[0]) != string(b.Certificate[0]) {
		t.Errorf("certificate changed across restarts: %v", err)
	}
}

func TestRedirect(t *testing.T) {
	for port, want := range map[int]string{443: "https://node.mesh/x?y=1", 8443: "https://node.mesh:8443/x?y=1"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "http://node.mesh/x?y=1", nil)
		redirectToHTTPS(port).ServeHTTP(rec, req)
		if rec.Code != http.StatusTemporaryRedirect || rec.Header().Get("Location") != want {
			t.Errorf("port %d: %d %s", port, rec.Code, rec.Header().Get("Location"))
		}
	}
}

// A meshd that hangs must not hold up connections: the guard decides from
// the last answer, and asks again in the background.
func TestGuardDoesNotWaitOnMeshd(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "m.sock")
	hung, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer hung.Close()
	go func() {
		for {
			c, err := hung.Accept()
			if err != nil {
				return
			}
			defer c.Close() // accepted, never answered
		}
	}()
	s, err := New(Options{Version: "test", Socket: sock, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback listener:", err)
	}
	defer ln.Close()
	g := s.guard(ln)
	for i := 0; i < 3; i++ {
		go func() {
			if c, err := net.Dial("tcp", ln.Addr().String()); err == nil {
				defer c.Close()
				time.Sleep(time.Second)
			}
		}()
		began := time.Now()
		c, err := g.Accept()
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
		if d := time.Since(began); d > 500*time.Millisecond {
			t.Fatalf("connection %d waited %s on a hung meshd", i+1, d)
		}
	}
}

// A change made on the node (meshctl web wan-access) reaches the guard
// within refreshEvery, with no connection in between to prompt it.
func TestCachedInfoFollowsMeshd(t *testing.T) {
	n := &node{password: "x"}
	_, s := start(t, n)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.webInfo(ctx)
	go s.refreshInfo(ctx)
	if info, _ := s.cachedInfo(); info.WANAccess {
		t.Fatal("wan_access on before it was set")
	}
	time.Sleep(time.Second) // mid-way through an answer's life
	n.mu.Lock()
	n.wan = true
	n.mu.Unlock()
	time.Sleep(refreshEvery + 500*time.Millisecond)
	s.infoMu.Lock()
	got := s.info.WANAccess
	s.infoMu.Unlock()
	if !got {
		t.Errorf("wan_access set on the node is not in webd's answer after %s", refreshEvery+500*time.Millisecond)
	}
}

func (n *node) VoiceConfig() api.VoiceConfig { return api.VoiceConfig{Config: []byte("{}")} }
func (n *node) Voice(context.Context) (*api.VoiceState, error) {
	return &api.VoiceState{}, nil
}
func (n *node) VoiceTest(context.Context, api.VoiceTestInput) error { return nil }
