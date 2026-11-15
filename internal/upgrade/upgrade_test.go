package upgrade

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"meridian/trellis/internal/deb"
	"meridian/trellis/internal/status"
)

// fakeNode stands in for dpkg, systemd and meshd on a node. Installing a
// package swaps the "running" version; versions listed in bad misbehave
// in the way named.
type fakeNode struct {
	t    *testing.T
	root string // file system root the package's files live under
	etc  string
	now  time.Time

	arch      string
	installed *deb.Info // nil: not installed from a package
	files     []string  // dpkg -L
	scripts   map[string]string
	halfDone  bool // dpkg state not "ii"

	// onInstall runs as dpkg -i starts.
	onInstall func()
	// oldRejects: the old version's binary refuses the configuration with
	// this message ("" accepts).
	oldRejects string
	oldAsked   int

	meshd bool // the service is active
	web   bool
	peers int
	// bad: Trellis-Version → "crash" (meshd dies), "silent" (no API),
	// "nopeers", "reconciler", "rewrites" (changes the configuration, then
	// crashes), "noinstall" (dpkg -i fails).
	bad  map[string]string
	cmds []string
}

func (f *fakeNode) Now() time.Time        { return f.now }
func (f *fakeNode) Sleep(d time.Duration) { f.now = f.now.Add(d) }

func (f *fakeNode) running() string {
	if f.installed == nil {
		return ""
	}
	return f.installed.Get("Trellis-Version")
}

func (f *fakeNode) Status(context.Context) (*status.Status, error) {
	how := f.bad[f.running()]
	if !f.meshd || how == "crash" || how == "silent" || how == "rewrites" {
		return nil, errors.New("meshd is not running")
	}
	st := &status.Status{Version: f.running()}
	if how != "nopeers" {
		for i := 0; i < f.peers; i++ {
			st.MeshLink.Peers = append(st.MeshLink.Peers, status.Peer{MAC: fmt.Sprintf("a0:b0:c0:00:00:%02x", i)})
		}
	}
	if how == "reconciler" {
		st.Reconcile.Reconcilers = []status.ReconcilerInfo{{Name: "ap", LastError: "hostapd would not start"}}
	}
	return st, nil
}

func (f *fakeNode) Run(_ context.Context, name string, args ...string) (string, error) {
	cmd := name + " " + strings.Join(args, " ")
	f.cmds = append(f.cmds, cmd)
	if strings.HasSuffix(name, "/previous-trellis") {
		f.oldAsked++
		if body, err := os.ReadFile(name); err != nil || string(body) != "binary 0.3.0" {
			f.t.Fatalf("the old binary was not the kept package's: %q %v", body, err)
		}
		if f.oldRejects != "" {
			return "node config x: INVALID\n  " + f.oldRejects + "\n", errors.New("exit status 1")
		}
		return "node config x: OK (callsign alpha-1)\n", nil
	}
	switch {
	case cmd == "dpkg --print-architecture":
		return f.arch + "\n", nil
	case name == "dpkg-query" && args[0] == "-W":
		if f.installed == nil {
			return "", errors.New("no packages found matching trellis")
		}
		st := "ii "
		if f.halfDone {
			st = "iF "
		}
		return st + "\t" + f.installed.Get("Version") + "\t" + f.installed.Get("Trellis-Version"), nil
	case cmd == "dpkg-query -s trellis":
		var b strings.Builder
		b.WriteString("Package: trellis\nStatus: install ok installed\n")
		for _, fl := range f.installed.Control {
			if fl.Name != "Package" {
				fmt.Fprintf(&b, "%s: %s\n", fl.Name, fl.Value)
			}
		}
		b.WriteString("Conffiles:\n /etc/chrony/conf.d/trellis.conf 0123456789abcdef0123456789abcdef\n")
		return b.String(), nil
	case cmd == "dpkg-query -L trellis":
		return "/.\n/usr\n/usr/bin\n" + strings.Join(f.files, "\n") + "\n", nil
	case cmd == "dpkg-query --control-list trellis":
		out := "md5sums\nconffiles\n"
		for n := range f.scripts {
			out += n + "\n"
		}
		return out, nil
	case name == "dpkg-query" && args[0] == "--control-show":
		return f.scripts[args[2]], nil
	case name == "dpkg" && args[0] == "--compare-versions":
		if args[1] > args[3] { // good enough for the test versions
			return "", nil
		}
		return "", errors.New("exit status 1")
	case name == "dpkg" && args[0] == "--configure":
		f.halfDone = false
		return "", nil
	case name == "dpkg" && args[0] == "-i":
		if f.onInstall != nil {
			f.onInstall()
		}
		b, err := os.ReadFile(args[len(args)-1])
		if err != nil {
			return "", err
		}
		info, err := deb.Read(b)
		if err != nil {
			return "", err
		}
		if f.bad[info.Get("Trellis-Version")] == "noinstall" {
			return "", errors.New("dpkg: dependency problems prevent configuration of trellis:\n trellis depends on newthing; however:\n  Package newthing is not installed.")
		}
		f.installed, f.halfDone = info, false
		how := f.bad[f.running()]
		if how == "rewrites" {
			if err := os.WriteFile(filepath.Join(f.etc, "node.yaml"), []byte("callsign: migrated-by-new-version\n"), 0o644); err != nil {
				f.t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(f.etc, "added-by-new.yaml"), []byte("x: 1\n"), 0o644); err != nil {
				f.t.Fatal(err)
			}
		}
		// The package's postinst restarts a running meshd.
		if f.meshd && (how == "crash" || how == "rewrites") {
			f.meshd = false
		}
		return "", nil
	case name == "systemctl" && args[0] == "is-active":
		on := map[string]bool{"trellis-meshd.service": f.meshd, "trellis-webd.socket": f.web}[args[1]]
		if on {
			return "active\n", nil
		}
		return "inactive\n", errors.New("exit status 3")
	case name == "systemctl" && args[0] == "stop":
		f.meshd = false
		return "", nil
	case name == "systemctl" && args[0] == "start":
		how := f.bad[f.running()]
		f.meshd = how != "crash" && how != "rewrites"
		return "", nil
	}
	f.t.Fatalf("unexpected command: %s", cmd)
	return "", nil
}

func pkg(t *testing.T, ver, arch string, key string) []byte {
	t.Helper()
	p := &deb.Package{
		Control: []deb.Field{
			{Name: "Package", Value: "trellis"}, {Name: "Version", Value: ver}, {Name: "Architecture", Value: arch},
			{Name: "Maintainer", Value: "Example <ops@example.org>"}, {Name: "Trellis-Version", Value: ver},
			{Name: "Description", Value: "mesh node"},
		},
		Files: []deb.File{{Path: "/usr/bin/trellis", Mode: 0o755, Body: []byte("binary " + ver)}},
		Time:  time.Unix(1_800_000_000, 0),
	}
	b, err := p.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		priv, err := deb.ParseSecret(key)
		if err != nil {
			t.Fatal(err)
		}
		if b, err = deb.Sign(b, priv, p.Time); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

type bench struct {
	node   *fakeNode
	opts   Options
	secret string
	log    *bytes.Buffer
}

// newBench is a node running 0.3.0 with two mesh peers, and a release key
// it trusts.
func newBench(t *testing.T) *bench {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	etc := filepath.Join(dir, "etc-trellis")
	state := filepath.Join(dir, "state")
	for _, d := range []string{filepath.Join(root, "usr/bin"), filepath.Join(root, "etc/chrony/conf.d"), etc, filepath.Join(state, "upgrade")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name, body string, mode fs.FileMode) {
		if err := os.WriteFile(name, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "usr/bin/trellis"), "binary 0.3.0", 0o755)
	if err := os.Symlink("trellis", filepath.Join(root, "usr/bin/meshctl")); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(root, "etc/chrony/conf.d/trellis.conf"), "allow 10.73.0.0/16\n# operator edit\n", 0o644)
	write(filepath.Join(etc, "node.yaml"), "callsign: alpha-1\n", 0o644)
	write(filepath.Join(etc, "profile.yaml"), "secret: do-not-lose\n", 0o600)
	write(filepath.Join(state, "identity.yaml"), "node_id: 7\n", 0o600)

	secret, public, err := deb.NewKey("test release")
	if err != nil {
		t.Fatal(err)
	}
	keys, _ := deb.ParsePublicKeys(public)
	cur, err := deb.Read(pkg(t, "0.3.0", "arm64", ""))
	if err != nil {
		t.Fatal(err)
	}
	log := &bytes.Buffer{}
	return &bench{
		node: &fakeNode{
			t: t, root: root, etc: etc, now: time.Unix(1_800_000_000, 0), arch: "arm64", installed: cur,
			files:   []string{"/usr/bin/trellis", "/usr/bin/meshctl", "/etc/chrony/conf.d/trellis.conf", "/usr/share/gone"},
			scripts: map[string]string{"postinst": "#!/bin/sh\necho old postinst\n"},
			meshd:   true, web: true, peers: 2, bad: map[string]string{},
		},
		opts: Options{
			Dir: filepath.Join(state, "upgrade"), ConfigDir: etc, StateDir: state, Root: root, Keys: keys,
			HealthTimeout: 90 * time.Second, Settle: 9 * time.Second, Log: log,
		},
		secret: secret, log: log,
	}
}

func (b *bench) config(t *testing.T) string {
	t.Helper()
	var out strings.Builder
	for _, f := range []string{filepath.Join(b.opts.ConfigDir, "node.yaml"), filepath.Join(b.opts.ConfigDir, "profile.yaml"), filepath.Join(b.opts.StateDir, "identity.yaml")} {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		fi, _ := os.Stat(f)
		fmt.Fprintf(&out, "%s %o %s", filepath.Base(f), fi.Mode().Perm(), body)
	}
	entries, _ := os.ReadDir(b.opts.ConfigDir)
	fmt.Fprintf(&out, "%d files", len(entries))
	return out.String()
}

func TestUpgradeGood(t *testing.T) {
	b := newBench(t)
	before := b.config(t)
	res, err := Run(context.Background(), pkg(t, "0.3.1", "arm64", b.secret), b.opts, b.node)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK() || res.From != "0.3.0" || res.To != "0.3.1" || !res.HealthyAfter {
		t.Fatalf("result = %+v\n%s", res, b.log)
	}
	if b.node.running() != "0.3.1" {
		t.Fatalf("node runs %s", b.node.running())
	}
	if b.config(t) != before {
		t.Fatal("the upgrade changed configuration or identity")
	}
	if p, _ := ReadPending(b.opts.Dir); p != nil {
		t.Fatalf("pending left behind: %+v", p)
	}
	if last, _ := ReadResult(b.opts.Dir); last == nil || last.Result != ResultUpgraded {
		t.Fatalf("last result = %+v", last)
	}
	// The repacked old version is a package dpkg could install again.
	raw, err := os.ReadFile(filepath.Join(b.opts.Dir, "previous.deb"))
	if err != nil {
		t.Fatal(err)
	}
	prev, err := deb.Read(raw)
	if err != nil {
		t.Fatal(err)
	}
	if prev.Get("Version") != "0.3.0" || prev.Get("Status") != "" || prev.Get("Conffiles") != "" || prev.Get("Trellis-Version") != "0.3.0" {
		t.Fatalf("repacked control: %+v", prev.Control)
	}
	if !strings.Contains(b.log.String(), "signature good") {
		t.Errorf("log does not mention the signature:\n%s", b.log)
	}

	// Going back by hand afterwards keeps the changes made since...
	node := filepath.Join(b.opts.ConfigDir, "node.yaml")
	if err := os.WriteFile(node, []byte("callsign: renamed-later\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	units := t.TempDir()
	self := filepath.Join(t.TempDir(), "trellis")
	if err := os.WriteFile(self, []byte("new binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	b.opts.Self, b.opts.UnitDir = self, units
	armed := false
	b.node.onInstall = func() {
		_, err := os.Stat(filepath.Join(units, "multi-user.target.wants", ResumeUnit))
		armed = err == nil
	}
	res, err = Rollback(context.Background(), b.opts, b.node)
	if err != nil {
		t.Fatal(err)
	}
	if res.Result != ResultRolledBack || b.node.running() != "0.3.0" || !res.HealthyAfter {
		t.Fatalf("manual rollback: %+v, node runs %s\n%s", res, b.node.running(), b.log)
	}
	if got, _ := os.ReadFile(node); string(got) != "callsign: renamed-later\n" {
		t.Errorf("manual rollback undid a later change: %q", got)
	}
	if !armed {
		t.Error("the boot-time resume was not armed during the manual rollback")
	}
	if err := resultError(res); err != nil {
		t.Errorf("a rollback asked for is not an error: %v", err)
	}
}

// ...and puts the old configuration back only when asked.
func TestManualRollbackRestoresWhenAsked(t *testing.T) {
	b := newBench(t)
	before := b.config(t)
	if res, err := Run(context.Background(), pkg(t, "0.3.1", "arm64", b.secret), b.opts, b.node); err != nil || !res.OK() {
		t.Fatalf("upgrade: %+v, %v", res, err)
	}
	if err := os.WriteFile(filepath.Join(b.opts.ConfigDir, "node.yaml"), []byte("callsign: renamed-later\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b.opts.RestoreConfig = true
	res, err := Rollback(context.Background(), b.opts, b.node)
	if err != nil || res.Result != ResultRolledBack {
		t.Fatalf("rollback: %+v, %v", res, err)
	}
	if b.config(t) != before {
		t.Error("the configuration from before the upgrade was not put back")
	}
	if !strings.Contains(b.log.String(), "every change since is undone") {
		t.Errorf("the log does not warn:\n%s", b.log)
	}
}

// resultError mirrors meshctl's: a rollback the operator asked for is not
// an error.
func resultError(res *Result) error {
	if res.Result == ResultRolledBack && res.Reason == ReasonAsked {
		return nil
	}
	if res.OK() {
		return nil
	}
	return errors.New(res.Reason)
}

// While the new version is being installed and checked, the boot-time
// resume is armed with the old binary; afterwards nothing of it is left.
func TestResumeArmedWhilePending(t *testing.T) {
	for _, how := range []string{"", "crash"} {
		b := newBench(t)
		b.node.bad["0.3.1"] = how
		units := t.TempDir()
		self := filepath.Join(t.TempDir(), "trellis")
		if err := os.WriteFile(self, []byte("old binary"), 0o755); err != nil {
			t.Fatal(err)
		}
		b.opts.Self, b.opts.UnitDir = self, units
		bin := filepath.Join(b.opts.Dir, ResumeBinary)
		link := filepath.Join(units, "multi-user.target.wants", ResumeUnit)
		armed := 0
		b.node.onInstall = func() {
			body, err := os.ReadFile(bin)
			unit, err2 := os.ReadFile(link) // through the link, as systemd does
			if err != nil || err2 != nil || string(body) != "old binary" ||
				!strings.Contains(string(unit), "ExecStart="+bin+" meshctl upgrade --resume --dir "+b.opts.Dir) ||
				!strings.Contains(string(unit), "ConditionPathExists="+filepath.Join(b.opts.Dir, "pending.json")) {
				t.Errorf("resume not armed at install: %v, %v\n%s", err, err2, unit)
			}
			if p, _ := ReadPending(b.opts.Dir); p == nil {
				t.Error("no pending state at install")
			}
			armed++
		}
		if _, err := Run(context.Background(), pkg(t, "0.3.1", "arm64", b.secret), b.opts, b.node); err != nil {
			t.Fatal(err)
		}
		if armed == 0 {
			t.Fatal("dpkg -i never ran")
		}
		for _, f := range []string{bin, link, filepath.Join(units, ResumeUnit)} {
			if _, err := os.Lstat(f); err == nil {
				t.Errorf("%q: %s left behind", how, f)
			}
		}
	}
}

func TestUpgradeRollsBack(t *testing.T) {
	for how, wantReason := range map[string]string{
		"crash":      "meshd is not running",
		"silent":     "meshd does not answer",
		"nopeers":    "no mesh peer has come back (2 before)",
		"reconciler": "reconciler ap fails, and did not before",
		"rewrites":   "meshd is not running",
		"noinstall":  "the package did not install: Package newthing is not installed.",
	} {
		t.Run(how, func(t *testing.T) {
			b := newBench(t)
			b.node.bad["0.3.1"] = how
			before := b.config(t)
			res, err := Run(context.Background(), pkg(t, "0.3.1", "arm64", b.secret), b.opts, b.node)
			if err != nil {
				t.Fatal(err)
			}
			if res.Result != ResultRolledBack || res.Reason != wantReason || !res.HealthyAfter {
				t.Fatalf("result = %+v\n%s", res, b.log)
			}
			if b.node.running() != "0.3.0" || !b.node.meshd {
				t.Fatalf("node runs %q, meshd active %v", b.node.running(), b.node.meshd)
			}
			if got := b.config(t); got != before {
				t.Fatalf("configuration not as before:\n got %s\nwant %s", got, before)
			}
			if p, _ := ReadPending(b.opts.Dir); p != nil {
				t.Fatalf("pending left behind: %+v", p)
			}
		})
	}
}

func TestUpgradeRefusals(t *testing.T) {
	b := newBench(t)
	ctx := context.Background()
	otherSecret, _, _ := deb.NewKey("someone else")
	cases := []struct {
		name string
		pkg  []byte
		mod  func(*Options, *fakeNode)
		want string
	}{
		{"unsigned", pkg(t, "0.3.1", "arm64", ""), nil, "not signed"},
		{"untrusted key", pkg(t, "0.3.1", "arm64", otherSecret), nil, "does not trust"},
		{"other architecture", pkg(t, "0.3.1", "amd64", b.secret), nil, "for amd64; this node is arm64"},
		{"same version", pkg(t, "0.3.0", "arm64", b.secret), nil, "already installed"},
		{"older", pkg(t, "0.2.9", "arm64", b.secret), nil, "older than the installed"},
		{"not a package", []byte("hello"), nil, "not a Debian package"},
		{"script install", pkg(t, "0.3.1", "arm64", b.secret), func(_ *Options, n *fakeNode) { n.installed = nil }, "not installed from a package"},
	}
	// Run the cases against one bench, whose key signed them.
	for _, c := range cases {
		node := *b.node
		opts := b.opts
		if c.mod != nil {
			c.mod(&opts, &node)
		}
		_, err := Run(ctx, c.pkg, opts, &node)
		if strings.Contains(c.want, "not signed") || strings.Contains(c.want, "does not trust") {
			if !errors.Is(err, ErrNotTrusted) {
				t.Errorf("%s: not an ErrNotTrusted: %v", c.name, err)
			}
		}
		if c.want == "older than the installed" && !errors.Is(err, ErrOlder) {
			t.Errorf("%s: not an ErrOlder: %v", c.name, err)
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to mention %q", c.name, err, c.want)
		}
		if node.installed != nil && node.running() != b.node.running() {
			t.Errorf("%s: something was installed", c.name)
		}
	}
	// The overrides.
	node := *b.node
	opts := b.opts
	opts.AllowUntrusted = true
	if res, err := Run(ctx, pkg(t, "0.3.1", "arm64", ""), opts, &node); err != nil || !res.OK() {
		t.Errorf("--allow-untrusted: %+v, %v", res, err)
	}
	node = *b.node
	opts = b.opts
	opts.AllowDowngrade = true
	opts.Dir = filepath.Join(t.TempDir(), "up")
	if res, err := Run(ctx, pkg(t, "0.2.9", "arm64", b.secret), opts, &node); err != nil || !res.OK() {
		t.Errorf("--allow-downgrade: %+v, %v", res, err)
	}
}

func TestResume(t *testing.T) {
	ctx := context.Background()
	// start runs an upgrade up to the given phase and "loses power": the
	// state on disk is as Run left it.
	start := func(t *testing.T, how string) (*bench, string) {
		b := newBench(t)
		before := b.config(t)
		b.node.bad["0.3.1"] = how
		// A health timeout of zero ends the first check at once, as a
		// power loss would; the rollback is undone below.
		u := &upgrader{b.opts, b.node}
		u.o.defaults()
		c, err := u.check(ctx, pkg(t, "0.3.1", "arm64", b.secret))
		if err != nil {
			t.Fatal(err)
		}
		p := &Pending{Phase: PhaseInstalling, From: c.Current.Version, To: c.Version, FromTrellis: c.Current.Trellis, ToTrellis: c.Trellis, Started: b.node.Now()}
		p.Baseline = u.health(ctx)
		prev, err := u.repack(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := writeFileSync(u.path("previous.deb"), prev, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := writeFileSync(u.path("new.deb"), pkg(t, "0.3.1", "arm64", b.secret), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := u.snapshot(); err != nil {
			t.Fatal(err)
		}
		if err := u.writePending(p); err != nil {
			t.Fatal(err)
		}
		return b, before
	}

	t.Run("nothing pending", func(t *testing.T) {
		b := newBench(t)
		if res, err := Resume(ctx, b.opts, b.node); res != nil || err != nil {
			t.Fatalf("Resume = %+v, %v", res, err)
		}
	})
	t.Run("power lost before install", func(t *testing.T) {
		b, before := start(t, "")
		res, err := Resume(ctx, b.opts, b.node)
		if err != nil || res.Result != ResultUnchanged || b.node.running() != "0.3.0" || b.config(t) != before {
			t.Fatalf("Resume = %+v, %v; runs %s", res, err, b.node.running())
		}
		if p, _ := ReadPending(b.opts.Dir); p != nil {
			t.Fatal("pending left behind")
		}
	})
	t.Run("power lost after install, node healthy", func(t *testing.T) {
		b, _ := start(t, "")
		if _, err := b.node.Run(ctx, "dpkg", "-i", filepath.Join(b.opts.Dir, "new.deb")); err != nil {
			t.Fatal(err)
		}
		res, err := Resume(ctx, b.opts, b.node)
		if err != nil || !res.OK() || b.node.running() != "0.3.1" {
			t.Fatalf("Resume = %+v, %v; runs %s\n%s", res, err, b.node.running(), b.log)
		}
	})
	t.Run("power lost after install, node broken", func(t *testing.T) {
		b, before := start(t, "rewrites")
		if _, err := b.node.Run(ctx, "dpkg", "-i", filepath.Join(b.opts.Dir, "new.deb")); err != nil {
			t.Fatal(err)
		}
		res, err := Resume(ctx, b.opts, b.node)
		if err != nil || res.Result != ResultRolledBack || b.node.running() != "0.3.0" || b.config(t) != before || !res.HealthyAfter {
			t.Fatalf("Resume = %+v, %v; runs %s\n%s", res, err, b.node.running(), b.log)
		}
	})
	t.Run("power lost while half-installed", func(t *testing.T) {
		b, _ := start(t, "")
		if _, err := b.node.Run(ctx, "dpkg", "-i", filepath.Join(b.opts.Dir, "new.deb")); err != nil {
			t.Fatal(err)
		}
		b.node.halfDone = true
		res, err := Resume(ctx, b.opts, b.node)
		if err != nil || !res.OK() {
			t.Fatalf("Resume = %+v, %v\n%s", res, err, b.log)
		}
	})
	t.Run("a second upgrade waits for the first", func(t *testing.T) {
		b, _ := start(t, "")
		if _, err := Run(ctx, pkg(t, "0.3.2", "arm64", b.secret), b.opts, b.node); err == nil || !strings.Contains(err.Error(), "still under way") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestRepackKeepsWhatIsInstalled(t *testing.T) {
	b := newBench(t)
	u := &upgrader{b.opts, b.node}
	raw, err := u.repack(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	info, err := deb.Read(raw)
	if err != nil {
		t.Fatal(err)
	}
	if info.Get("Architecture") != "arm64" || info.Get("Maintainer") == "" {
		t.Fatalf("control: %+v", info.Control)
	}
	files, err := deb.Payload(raw)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]deb.File{}
	for _, f := range files {
		got[f.Path] = f
	}
	if f := got["/usr/bin/trellis"]; string(f.Body) != "binary 0.3.0" || f.Mode != 0o755 {
		t.Errorf("binary: %q mode %o", f.Body, f.Mode)
	}
	if f := got["/usr/bin/meshctl"]; f.Link != "trellis" {
		t.Errorf("symlink: %+v", f)
	}
	// The drop-in travels as the operator left it.
	if f := got["/etc/chrony/conf.d/trellis.conf"]; !strings.Contains(string(f.Body), "operator edit") {
		t.Errorf("conffile: %q", f.Body)
	}
	if _, ok := got["/usr/share/gone"]; ok {
		t.Error("a file missing on disk was packed")
	}
}

func TestJudge(t *testing.T) {
	up := Health{MeshdActive: true, WebSocket: true, API: true, Version: "0.3.1", Peers: 2}
	cases := []struct {
		name        string
		before, now Health
		want        string
	}{
		{"same", up, up, ""},
		{"fewer peers is fine", up, Health{MeshdActive: true, WebSocket: true, API: true, Version: "0.3.1", Peers: 1}, ""},
		{"alone before, alone after", Health{MeshdActive: true, API: true}, Health{MeshdActive: true, API: true, Version: "0.3.1"}, ""},
		{"old failure persists", Health{MeshdActive: true, API: true, Failing: []string{"reconciler ap"}}, Health{MeshdActive: true, API: true, Version: "0.3.1", Failing: []string{"reconciler ap"}}, ""},
		{"stopped before, stopped after", Health{}, Health{}, ""},
		{"web socket gone", up, Health{MeshdActive: true, API: true, Version: "0.3.1", Peers: 2}, "the web UI's socket no longer listens"},
		{"wrong version", up, Health{MeshdActive: true, WebSocket: true, API: true, Version: "0.3.0", Peers: 2}, "meshd answers as version 0.3.0, not 0.3.1"},
		{"new error", up, Health{MeshdActive: true, WebSocket: true, API: true, Version: "0.3.1", Peers: 2, Failing: []string{"no-mesh-radio"}}, "no-mesh-radio fails, and did not before"},
	}
	for _, c := range cases {
		if got := judge(c.before, c.now, "0.3.1"); got != c.want {
			t.Errorf("%s: judge = %q, want %q", c.name, got, c.want)
		}
	}
}

// A directory given with a trailing slash is the same directory: the
// snapshot must still leave it out, not copy itself into itself.
func TestUncleanDir(t *testing.T) {
	b := newBench(t)
	b.opts.Dir += "/"
	before := b.config(t)
	b.node.bad["0.3.1"] = "crash"
	res, err := Run(context.Background(), pkg(t, "0.3.1", "arm64", b.secret), b.opts, b.node)
	if err != nil || res.Result != ResultRolledBack || b.config(t) != before {
		t.Fatalf("result %+v, %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(b.opts.Dir, "snapshot", "state", "upgrade")); err == nil {
		t.Error("the snapshot holds the upgrade directory itself")
	}
}

func TestPackageState(t *testing.T) {
	for _, c := range []struct {
		state             string
		installed, halfOK bool
	}{
		{"ii", true, false}, {"hi", true, false}, {"iF", false, true}, {"iU", false, true},
		{"rc", false, false}, {"un", false, false}, {"", false, false},
	} {
		ps := PackageState{State: c.state}
		if ps.Installed() != c.installed || ps.HalfDone() != c.halfOK {
			t.Errorf("%q: installed %v half %v", c.state, ps.Installed(), ps.HalfDone())
		}
	}
}

// meshd that did not answer before the upgrade need not answer after it:
// the upgrade may be what fixes it.
func TestJudgeSilentBefore(t *testing.T) {
	before := Health{MeshdActive: true, WebSocket: true}
	if got := judge(before, Health{MeshdActive: true, WebSocket: true}, "0.3.1"); got != "" {
		t.Errorf("silent before and after: %q", got)
	}
	if got := judge(before, Health{MeshdActive: true, WebSocket: true, API: true, Version: "0.3.0"}, "0.3.1"); got == "" {
		t.Error("a wrong version answering was accepted")
	}
}

// After a successful upgrade, a later attempt that prepared and failed
// leaves another version in previous.deb: the rollback must not offer it.
func TestRollbackOnlyToTheVersionKept(t *testing.T) {
	b := newBench(t)
	if res, err := Run(context.Background(), pkg(t, "0.3.1", "arm64", b.secret), b.opts, b.node); err != nil || !res.OK() {
		t.Fatalf("upgrade: %+v %v", res, err)
	}
	last, _ := ReadResult(b.opts.Dir)
	if !CanRollBack(b.opts.Dir, last, "0.3.1") || CanRollBack(b.opts.Dir, last, "0.3.0") {
		t.Fatal("CanRollBack wrong right after the upgrade")
	}
	// What a failed later attempt leaves behind.
	cur, _ := deb.Read(pkg(t, "0.3.1", "arm64", ""))
	b.node.installed = cur
	other, _ := (&deb.Package{Control: cur.Control, Files: []deb.File{{Path: "/usr/bin/trellis", Mode: 0o755, Body: []byte("x")}}, Time: time.Unix(1, 0)}).Bytes()
	os.WriteFile(filepath.Join(b.opts.Dir, "previous.deb"), other, 0o600)
	os.WriteFile(filepath.Join(b.opts.Dir, PreviousVersion), []byte("0.3.1\n"), 0o600)
	if CanRollBack(b.opts.Dir, last, "0.3.1") {
		t.Error("offered a rollback to the wrong version")
	}
	if _, err := Rollback(context.Background(), b.opts, b.node); err == nil || !strings.Contains(err.Error(), "not the 0.3.0") {
		t.Errorf("Rollback with the wrong package kept: %v", err)
	}
}

// A rollback that keeps the configuration asks the old version whether it
// reads it, and puts the old configuration back when it does not.
func TestManualRollbackOldRejectsConfig(t *testing.T) {
	b := newBench(t)
	before := b.config(t)
	if res, err := Run(context.Background(), pkg(t, "0.3.1", "arm64", b.secret), b.opts, b.node); err != nil || !res.OK() {
		t.Fatalf("upgrade: %+v %v", res, err)
	}
	// The newer version's setting, which the older one does not know.
	node := filepath.Join(b.opts.ConfigDir, "node.yaml")
	if err := os.WriteFile(node, []byte("callsign: alpha-1\nfirewall: {wan: closed}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b.node.oldRejects = `firewall: field not found in type config.Node`
	res, err := Rollback(context.Background(), b.opts, b.node)
	if err != nil || res.Result != ResultRolledBack || b.node.running() != "0.3.0" {
		t.Fatalf("rollback: %+v %v", res, err)
	}
	if b.node.oldAsked != 1 {
		t.Errorf("the old version was asked %d times", b.node.oldAsked)
	}
	if b.config(t) != before {
		t.Error("the configuration the old version cannot read was kept")
	}
	if !strings.Contains(b.log.String(), "does not read the configuration as it is now (firewall: field not found") {
		t.Errorf("log:\n%s", b.log)
	}
	if _, err := os.Stat(filepath.Join(b.opts.Dir, "previous-trellis")); err == nil {
		t.Error("the extracted old binary was left behind")
	}
}

// Two upgrade processes never work on a node at once.
func TestUpgradeLock(t *testing.T) {
	b := newBench(t)
	u := &upgrader{b.opts, b.node}
	u.o.defaults()
	unlock, err := u.lock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), pkg(t, "0.3.1", "arm64", b.secret), b.opts, b.node); !errors.Is(err, ErrBusy) {
		t.Errorf("Run while locked: %v", err)
	}
	if _, err := Resume(context.Background(), b.opts, b.node); !errors.Is(err, ErrBusy) {
		t.Errorf("Resume while locked: %v", err)
	}
	unlock()
	if res, err := Run(context.Background(), pkg(t, "0.3.1", "arm64", b.secret), b.opts, b.node); err != nil || !res.OK() {
		t.Errorf("Run after unlock: %+v %v", res, err)
	}
}
