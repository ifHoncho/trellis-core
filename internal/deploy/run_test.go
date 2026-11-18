package deploy

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"meridian/trellis/internal/config"
)

type codeErr int

func (e codeErr) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e codeErr) ExitCode() int { return int(e) }
func (e codeErr) Unwrap() error { return nil }

// fakeNode is one node's side of the commands the run sends.
type fakeNode struct {
	files     map[string]string // what was copied, by name
	sudoPass  string            // "" when sudo takes none
	install   string            // how install.sh ends: "done", "fail", "reboot"
	upgrade   string            // how upgrade.sh ends: "ok", "rollback", "drop"
	boot      int
	rebooting int // polls left before it is back
	last      string
	webPass   string
	exit      string // ~/.trellis-deploy/exit, as an apt install leaves it
	exitAfter int    // polls before the exit file appears
	errLine   string // the last "error:" line of its run.log
	lastTo    string // last.json's "to": the package's Debian version
	rebootReq bool   // /run/reboot-required exists
	refuse    bool   // ssh reaches it and is refused
	ran       []string
}

type fakeRemote struct {
	mu     sync.Mutex
	nodes  map[string]*fakeNode // by route target
	banner map[string]string    // what ssh prints for a target it is refused at
}

func (f *fakeRemote) node(r Route) *fakeNode { return f.nodes[r.Target] }

func (n *fakeNode) to() string {
	if n.lastTo != "" {
		return n.lastTo
	}
	return "0.2.0"
}

func (f *fakeRemote) Run(ctx context.Context, r Route, cmd string, stdin io.Reader, out io.Writer) error {
	f.mu.Lock()
	n := f.node(r)
	f.mu.Unlock()
	if b, ok := f.banner[r.Target]; ok {
		io.WriteString(out, b)
		return codeErr(255)
	}
	if n == nil {
		io.WriteString(out, "ssh: connect to host "+r.Target+" port 22: Connection timed out\n")
		return codeErr(255)
	}
	n.ran = append(n.ran, cmd)
	if n.refuse {
		io.WriteString(out, r.Target+": Permission denied (publickey,password).\n")
		return codeErr(255)
	}
	in := bufio.NewReader(orEmpty(stdin))
	if n.rebooting > 0 {
		n.rebooting--
		return codeErr(255)
	}
	if strings.HasPrefix(cmd, "sudo -S") {
		pw, _ := in.ReadString('\n')
		if strings.TrimSpace(pw) != n.sudoPass {
			io.WriteString(out, "Sorry, try again.\n")
			return codeErr(1)
		}
	}
	switch {
	case strings.Contains(cmd, "install.sh"):
		io.WriteString(out, "@@ step installing the packages\nGet:1 …\n@@ step preparing the system\n")
		switch n.install {
		case "fail":
			io.WriteString(out, "@@ fail the packages did not install\n")
			return codeErr(1)
		case "reboot":
			io.WriteString(out, "@@ reboot\n")
		}
		io.WriteString(out, "@@ done\n")
	case strings.Contains(cmd, "upgrade.sh") && strings.Contains(cmd, ".trellis-deploy reinstall "):
		io.WriteString(out, "@@ step reinstalling\n")
	case strings.Contains(cmd, "upgrade.sh") && strings.Contains(cmd, ".trellis-deploy install "):
		io.WriteString(out, "@@ step installing (no health check)\n")
		n.exit = "0"
		if n.upgrade == "drop" {
			return codeErr(255)
		}
	case strings.Contains(cmd, "/exit"):
		exit := n.exit
		if n.exitAfter > 0 {
			n.exitAfter--
			exit = ""
		}
		io.WriteString(out, exit+"\n"+n.errLine+"\n")
	case strings.Contains(cmd, "reboot-required"):
		if n.rebootReq {
			io.WriteString(out, "yes\n")
		}
	case strings.Contains(cmd, "upgrade.sh"):
		io.WriteString(out, "@@ step upgrading\n")
		switch n.upgrade {
		case "ok":
			n.last = `{"result":"upgraded","from":"0.1.0","to":"` + n.to() + `"}`
		case "rollback":
			n.last = `{"result":"rolled-back","from":"0.1.0","to":"0.2.0","reason":"meshd did not come back"}`
			return codeErr(1)
		case "untrusted":
			io.WriteString(out, "error: signed with key 0123456789abcdef, which this node does not trust\n@@ upgrade-exit 1\n")
			return codeErr(1)
		case "drop":
			n.last = `{"result":"upgraded","from":"0.1.0","to":"` + n.to() + `"}`
			return codeErr(255)
		}
	case strings.Contains(cmd, "last.json"):
		io.WriteString(out, n.last+"\n")
	case strings.Contains(cmd, "systemctl reboot"):
		n.boot++
		n.rebooting = 2
		return codeErr(255)
	case strings.Contains(cmd, "boot_id"):
		fmt.Fprintf(out, "boot-%d\n", n.boot)
	case strings.Contains(cmd, "is-active"):
		io.WriteString(out, "active\n")
	case strings.Contains(cmd, "web password"):
		pw, _ := in.ReadString('\n')
		n.webPass = strings.TrimSpace(pw)
	case strings.Contains(cmd, "meshctl ap --json"):
		io.WriteString(out, `{"enabled":true,"ssid":"`+r.Target+`","passphrase":"wifi-pass-`+r.Target+`"}`+"\n")
	case strings.Contains(cmd, "meshctl doctor --json"):
		io.WriteString(out, `{"checks":[{"status":"PASS","name":"kernel"},{"status":"WARN","name":"firewall"},{"status":"FAIL","name":"batman-adv module"}],"pass":1,"warn":1,"fail":1}`+"\n")
	case strings.Contains(cmd, "trellis version"):
		io.WriteString(out, "0.2.0\n")
	}
	return nil
}

func orEmpty(r io.Reader) io.Reader {
	if r == nil {
		return strings.NewReader("")
	}
	return r
}

func (f *fakeRemote) Output(ctx context.Context, r Route, cmd string) (string, error) {
	var b bytes.Buffer
	err := f.Run(ctx, r, cmd, nil, &b)
	return b.String(), err
}

func (f *fakeRemote) Put(ctx context.Context, r Route, local, dir, name string) error {
	f.node(r).files[name] = "from " + local
	return nil
}

func (f *fakeRemote) PutBytes(ctx context.Context, r Route, data io.Reader, dir, name string) error {
	b, _ := io.ReadAll(data)
	f.node(r).files[name] = string(b)
	return nil
}

func (f *fakeRemote) Disconnect(r Route) {}

type nopWC struct{ io.Writer }

func (nopWC) Close() error { return nil }

func step(target string, act Action, nopass bool) Step {
	return Step{Host: Host{Target: target, Addr: target, Callsign: target}, Action: act, Package: "/r/trellis_0.2.0_arm64.deb", Callsign: target,
		State: State{Route: Route{Target: target}, Reachable: true, NoPass: nopass, Batman: "2026.3-1"}}
}

func newRunner(f *fakeRemote) (*Runner, *bytes.Buffer) {
	var console bytes.Buffer
	return &Runner{SSH: f, Release: &Release{Version: "0.2.0", Debian: "0.2.0", Batman: "/r/dkms.deb", BatmanVersion: "2026.3-1"},
		Profile: []byte("name: alpha\nsae_passphrase: secret-not-for-logs\n"), Passwords: map[string]string{"a": "pw", "b": "pw", "c": "pw", "d": "pw"}, WebPassword: "admin-pw-1",
		Reboot: true, Jobs: 2, Out: &console, Poll: time.Millisecond, RebootWait: time.Second, MeshdWait: time.Second, ResultWait: time.Second,
		Log: func(Step) io.WriteCloser { return nopWC{io.Discard} }}, &console
}

func fake(names ...string) *fakeRemote {
	f := &fakeRemote{nodes: map[string]*fakeNode{}}
	for _, n := range names {
		f.nodes[n] = &fakeNode{files: map[string]string{}, sudoPass: "pw", install: "done", upgrade: "ok"}
	}
	return f
}

func TestRunInstalls(t *testing.T) {
	f := fake("a", "b", "c")
	f.nodes["c"].install = "fail"
	r, console := newRunner(f)
	outs := r.Run(context.Background(), []Step{step("a", Install, false), step("b", Install, true), step("c", Install, false)})
	if !outs[0].OK || !outs[1].OK || outs[2].OK || Failed(outs) != 1 {
		t.Fatalf("%+v\n%s", outs, console)
	}
	a := f.nodes["a"]
	if a.files["profile.yaml"] == "" || a.files["install.sh"] != installScript || a.files["dkms.deb"] == "" {
		t.Errorf("copied: %v", a.files)
	}
	if a.boot != 1 || a.webPass != "admin-pw-1" || outs[0].Version != "0.2.0" {
		t.Errorf("rebooted, waited for, admin password set: %+v %+v", a, outs[0])
	}
	// sudo without a password takes none on its input; the password never
	// travels on a command line.
	for _, c := range f.nodes["b"].ran {
		if strings.HasPrefix(c, "sudo -S") {
			t.Errorf("b's sudo takes no password: %s", c)
		}
	}
	for _, n := range f.nodes {
		for _, c := range n.ran {
			if strings.Contains(c, "pw") && !strings.Contains(c, "web password") || strings.Contains(c, "secret-not-for-logs") {
				t.Errorf("a secret on a command line: %s", c)
			}
		}
	}
	if !strings.Contains(console.String(), "[c] !! the packages did not install") {
		t.Errorf("console:\n%s", console)
	}
}

func TestRunUpgrades(t *testing.T) {
	f := fake("a", "b", "c", "d")
	f.nodes["b"].upgrade = "drop"     // reached over the mesh: the session goes
	f.nodes["c"].upgrade = "rollback" // stops the run
	r, console := newRunner(f)
	steps := []Step{step("a", Upgrade, false), step("b", Upgrade, false), step("c", Upgrade, false), step("d", Upgrade, false)}
	outs := r.Run(context.Background(), steps)
	if !outs[0].OK || !outs[1].OK || outs[1].Result != "upgraded to 0.2.0" {
		t.Fatalf("a and b: %+v\n%s", outs[:2], console)
	}
	if outs[2].OK || !strings.Contains(outs[2].Result, "went back to 0.1.0: meshd did not come back") {
		t.Errorf("c: %+v", outs[2])
	}
	if outs[3].OK || !strings.HasPrefix(outs[3].Result, "not run: c did not upgrade") || len(f.nodes["d"].ran) != 0 {
		t.Errorf("d is not touched after c went back: %+v %v", outs[3], f.nodes["d"].ran)
	}
	if _, ok := f.nodes["a"].files["dkms.deb"]; ok {
		t.Error("the batman-adv package goes only where it changed")
	}

	f = fake("c", "d")
	f.nodes["c"].upgrade = "rollback"
	r, _ = newRunner(f)
	r.KeepGoing = true
	outs = r.Run(context.Background(), []Step{step("c", Upgrade, false), step("d", Upgrade, false)})
	if !outs[1].OK {
		t.Errorf("--keep-going: %+v", outs[1])
	}
}

func TestRunWrongSudoPassword(t *testing.T) {
	f := fake("a")
	r, _ := newRunner(f)
	r.Passwords = map[string]string{"a": "typo"}
	outs := r.Run(context.Background(), []Step{step("a", Install, false)})
	if outs[0].OK {
		t.Errorf("%+v", outs[0])
	}
}

func TestSummary(t *testing.T) {
	var b bytes.Buffer
	Summary(&b, []Outcome{
		{Step: step("alpha-1", Install, true), OK: true, Result: "installed 0.2.0"},
		{Step: Step{Host: Host{Addr: "192.0.2.9"}, Action: Skip}, Result: "skipped: unreachable"},
		{Step: step("charlie", Upgrade, true), Result: "went back to 0.1.0: x"},
	})
	s := b.String()
	if !strings.Contains(s, "alpha-1") || !strings.Contains(s, "ok installed 0.2.0") || !strings.Contains(s, "-- skipped") || !strings.Contains(s, "!! went back") {
		t.Errorf("%s", s)
	}
}

// Nodes with different sudo passwords: each answer is tried on the next
// node first, a node none of them opens is asked for, and one left without
// a password is skipped while the others go on.
func TestFindPasswords(t *testing.T) {
	f := fake("a", "b", "c", "d", "e")
	f.nodes["c"].sudoPass = "other"
	f.nodes["d"].sudoPass = "third"
	r, _ := newRunner(f)
	r.Passwords = nil
	steps := []Step{step("a", Upgrade, false), step("b", Upgrade, false), step("c", Upgrade, false), step("d", Install, false),
		step("e", Upgrade, true), step("f", Current, false)}
	answers := map[string][]string{"a": {"pw"}, "c": {"typo", "other"}, "d": {""}}
	var asked []string
	steps, err := r.FindPasswords(context.Background(), steps, func(s Step, retry bool) (string, error) {
		asked = append(asked, fmt.Sprintf("%s/%v", s.Name(), retry))
		a := answers[s.Name()]
		pw := a[0]
		answers[s.Name()] = a[1:]
		return pw, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(asked, " ") != "a/false c/false c/true d/false" {
		t.Errorf("asked: %v (b takes a's password; e needs none)", asked)
	}
	if r.Passwords["a"] != "pw" || r.Passwords["b"] != "pw" || r.Passwords["c"] != "other" {
		t.Errorf("passwords: %v", r.Passwords)
	}
	if steps[3].Action != Skip || !strings.Contains(steps[3].Why, "no sudo password") || steps[2].Action != Upgrade || steps[4].Action != Upgrade {
		t.Errorf("steps: %+v", steps)
	}
	// The others run with their own passwords.
	outs := r.Run(context.Background(), steps[:3])
	for _, o := range outs {
		if !o.OK {
			t.Errorf("%s: %s", o.Step.Name(), o.Result)
		}
	}
}

// A node that refuses the package before upgrading: its reason is the result.
func TestRunRefused(t *testing.T) {
	f := fake("a")
	f.nodes["a"].upgrade = "untrusted"
	r, _ := newRunner(f)
	outs := r.Run(context.Background(), []Step{step("a", Upgrade, false)})
	if outs[0].OK || outs[0].Result != "refused by the node: signed with key 0123456789abcdef, which this node does not trust" {
		t.Errorf("%+v", outs[0])
	}
}

// --force on a node that runs the release: reinstalled, not upgraded.
func TestRunReinstall(t *testing.T) {
	f := fake("a")
	r, _ := newRunner(f)
	s := step("a", Upgrade, false)
	s.State.Cmp = "same"
	outs := r.Run(context.Background(), []Step{s})
	sent := false
	for _, c := range f.nodes["a"].ran {
		sent = sent || strings.Contains(c, "upgrade.sh \"$HOME\"/.trellis-deploy reinstall ")
	}
	if !outs[0].OK || outs[0].Result != "reinstalled 0.2.0" || !sent {
		t.Errorf("%+v %v", outs[0], f.nodes["a"].ran)
	}
}

// --no-health-check: apt instead of meshctl upgrade, the node key sent
// along, and the outcome read back when the session drops.
func TestRunFast(t *testing.T) {
	f := fake("a", "b")
	f.nodes["b"].upgrade = "drop"
	r, _ := newRunner(f)
	r.Fast, r.NodeKey = true, []byte("trellis-release-key v1 x dev\n")
	outs := r.Run(context.Background(), []Step{step("a", Upgrade, false), step("b", Upgrade, false)})
	for _, o := range outs {
		if !o.OK || o.Result != "installed (no health check) 0.2.0" {
			t.Errorf("%s: %+v", o.Step.Name(), o)
		}
	}
	if f.nodes["a"].files["node-key.pub"] == "" {
		t.Error("the node key was not sent")
	}
	for _, c := range f.nodes["a"].ran {
		if strings.Contains(c, "upgrade.sh") && !strings.Contains(c, ".trellis-deploy install ") {
			t.Errorf("not the apt mode: %s", c)
		}
	}
}

// --any-version over a newer build: meshctl upgrade --allow-downgrade.
func TestPlanAnyVersion(t *testing.T) {
	rel := &Release{Version: "0.2.0", Debian: "0.2.0", Arches: map[string]string{"arm64": "x.deb"}}
	hosts := []Host{{Target: "a", Addr: "a"}}
	st := []State{{Reachable: true, Apt: true, Arch: "arm64", Trellis: "0.3.0~dev", Cmp: "newer"}}
	if s := Plan(hosts, st, rel, PlanOptions{}); s[0].Action != Skip {
		t.Errorf("without --any-version: %+v", s[0])
	}
	if s := Plan(hosts, st, rel, PlanOptions{AnyVersion: true}); s[0].Action != Upgrade {
		t.Errorf("with --any-version: %+v", s[0])
	}
	f := fake("a")
	r, _ := newRunner(f)
	s := step("a", Upgrade, false)
	s.State.Cmp = "newer"
	r.Run(context.Background(), []Step{s})
	found := false
	for _, c := range f.nodes["a"].ran {
		found = found || strings.Contains(c, "upgrade.sh \"$HOME\"/.trellis-deploy downgrade ")
	}
	if !found {
		t.Errorf("downgrade mode not used: %v", f.nodes["a"].ran)
	}
}

// The summary carries what doctor finds wrong on a node after its step.
func TestRunDoctorNotes(t *testing.T) {
	f := fake("a")
	r, _ := newRunner(f)
	outs := r.Run(context.Background(), []Step{step("a", Upgrade, false)})
	want := []string{"doctor: 1 failed (batman-adv module)", "doctor: 1 warning(s) (firewall)"}
	if !outs[0].OK || strings.Join(outs[0].Notes, "|") != strings.Join(want, "|") {
		t.Errorf("%+v", outs[0])
	}
	var b bytes.Buffer
	Summary(&b, outs)
	if !strings.Contains(b.String(), "(doctor: 1 failed (batman-adv module); doctor: 1 warning(s) (firewall))") {
		t.Errorf("summary: %s", b.String())
	}
}

// A new node's Wi-Fi details come back for the console, not the log.
func TestRunJoinDetails(t *testing.T) {
	f := fake("a")
	r, _ := newRunner(f)
	var log bytes.Buffer
	r.Log = func(Step) io.WriteCloser { return nopWC{&log} }
	outs := r.Run(context.Background(), []Step{step("a", Install, false)})
	if outs[0].WiFi == nil || outs[0].WiFi.SSID != "a" || outs[0].WiFi.Passphrase != "wifi-pass-a" {
		t.Fatalf("%+v", outs[0])
	}
	if strings.Contains(log.String(), "wifi-pass-a") {
		t.Error("the Wi-Fi passphrase reached the log")
	}
	var b bytes.Buffer
	JoinDetails(&b, outs)
	if !strings.Contains(b.String(), "wifi-pass-a") || !strings.Contains(b.String(), "https://10.73.0.1") {
		t.Errorf("%s", b.String())
	}
}

func TestNewMesh(t *testing.T) {
	y, err := NewMesh("team1", "us", "2g")
	if err != nil {
		t.Fatal(err)
	}
	p, err := config.ParseProfile(y)
	if err != nil || p.Name != "team1" || p.Regulatory.Country != "US" || p.Radio.Channel != 6 || len(p.SAEPassphrase) < 20 {
		t.Fatalf("%+v %v", p, err)
	}
	y2, _ := NewMesh("team1", "US", "2g")
	if string(y) == string(y2) {
		t.Error("two meshes got the same secret")
	}
	for _, bad := range [][3]string{{"", "US", "2g"}, {"x", "USA", "2g"}, {"x", "US", "60g"}} {
		if _, err := NewMesh(bad[0], bad[1], bad[2]); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}

// A development build's Debian version is not its Trellis version: an
// upgrade whose session dropped is still read back as done.
func TestRunDropDebianVersion(t *testing.T) {
	f := fake("a")
	f.nodes["a"].upgrade, f.nodes["a"].lastTo = "drop", "0.2.0+3.gabc.1115.091547"
	r, _ := newRunner(f)
	r.Release.Version, r.Release.Debian = "0.2.0-3-gabc+1115.091547", "0.2.0+3.gabc.1115.091547"
	outs := r.Run(context.Background(), []Step{step("a", Upgrade, false)})
	if !outs[0].OK || outs[0].Result != "upgraded to 0.2.0-3-gabc+1115.091547" {
		t.Errorf("%+v", outs[0])
	}
}

// While an apt install still runs, its log's error line is not its end.
func TestAwaitExitStillRunning(t *testing.T) {
	f := fake("a")
	n := f.nodes["a"]
	n.upgrade, n.exitAfter, n.errLine = "drop", 2, "error: 1 step(s) failed: batman-adv"
	r, _ := newRunner(f)
	r.Fast = true
	outs := r.Run(context.Background(), []Step{step("a", Upgrade, false)})
	if !outs[0].OK {
		t.Errorf("an error line in the log was taken for the end: %+v", outs[0])
	}
}

func TestRebootNoteAfterUpgrade(t *testing.T) {
	f := fake("a")
	f.nodes["a"].rebootReq = true
	r, _ := newRunner(f)
	outs := r.Run(context.Background(), []Step{step("a", Upgrade, false)})
	if !outs[0].OK || !strings.Contains(strings.Join(outs[0].Notes, ";"), "reboot it to finish") {
		t.Errorf("%+v", outs[0])
	}
}

// A node reached over the mesh keeps its own SSH port.
func TestInspectMeshPort(t *testing.T) {
	f := fake("a@192.0.2.1", "b@10.73.2.1")
	hosts, _ := ParseHosts(strings.NewReader("a@192.0.2.1 port=2201\nb@192.0.2.2 port=2222 mesh=10.73.2.1\n"))
	states := Inspect(context.Background(), f, hosts, "0.2.0")
	if !states[1].Reachable || states[1].Route.Port != 2222 || states[1].Route.ViaPort != 2201 || states[1].Route.Via != "a@192.0.2.1" {
		t.Errorf("%+v", states[1].Route)
	}
}

// A node that runs Trellis but waits in setup mode gets the profile with its
// upgrade; one that has a profile does not.
func TestUpgradeSendsProfileToWaitingNode(t *testing.T) {
	f := fake("a", "b")
	r, _ := newRunner(f)
	waiting, configured := step("a", Upgrade, false), step("b", Upgrade, false)
	configured.State.Profile = true
	r.Run(context.Background(), []Step{waiting, configured})
	if f.nodes["a"].files["profile.yaml"] == "" || f.nodes["b"].files["profile.yaml"] != "" {
		t.Errorf("a: %q, b: %q", f.nodes["a"].files["profile.yaml"], f.nodes["b"].files["profile.yaml"])
	}
}

// A node that answered and refused the key is not tried again over the mesh.
func TestInspectNoMeshRetryAfterRefusal(t *testing.T) {
	f := fake("a@192.0.2.1", "b@192.0.2.2", "b@10.73.2.1")
	f.nodes["b@192.0.2.2"].refuse = true
	hosts, _ := ParseHosts(strings.NewReader("a@192.0.2.1\nb@192.0.2.2 mesh=10.73.2.1\n"))
	states := Inspect(context.Background(), f, hosts, "0.2.0")
	if states[1].Reachable || states[1].Route.Via != "" || !strings.Contains(states[1].Err, "Permission denied") {
		t.Errorf("%+v", states[1])
	}
}

// The jump to a node over the mesh carries the operator's options.
func TestProxyCarriesOptions(t *testing.T) {
	s := &SSH{Binary: "ssh", Options: []string{"IdentityFile=/home/op/my key"}}
	a := strings.Join(s.args(Route{Target: "b@10.73.2.1", Via: "a@192.0.2.1", ViaPort: 2201, Alias: "192.0.2.2"}), " ")
	for _, want := range []string{"ProxyCommand=ssh -o 'IdentityFile=/home/op/my key'", "-p 2201 -W %h:%p a@192.0.2.1", "HostKeyAlias=192.0.2.2"} {
		if !strings.Contains(a, want) {
			t.Errorf("%q not in %s", want, a)
		}
	}
	if strings.Contains(a, " -J ") {
		t.Errorf("-J: %s", a)
	}
}

// A changed host key: ssh's banner first, its verdict last. The node is not
// tried over the mesh, and the reason shown is the verdict.
func TestInspectHostKeyChanged(t *testing.T) {
	f := fake("a@192.0.2.1", "b@10.73.2.1")
	f.banner = map[string]string{"b@192.0.2.2": "@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@\n@    WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!     @\nHost key verification failed.\n"}
	hosts, _ := ParseHosts(strings.NewReader("a@192.0.2.1\nb@192.0.2.2 mesh=10.73.2.1\n"))
	states := Inspect(context.Background(), f, hosts, "0.2.0")
	if states[1].Reachable || states[1].Route.Via != "" || states[1].Err != "Host key verification failed." || !states[1].Refused {
		t.Errorf("%+v", states[1])
	}
}

func TestProxyEscapesTokens(t *testing.T) {
	s := &SSH{Binary: "ssh", Options: []string{"IdentityFile=~/.ssh/id_%u"}}
	p := s.proxy(Route{Target: "b@10.73.2.1", Via: "a@192.0.2.1"})
	if !strings.Contains(p, "id_%%u") {
		t.Errorf("%s", p)
	}
}

// The development key goes only with a run on the release keys alone.
func TestDropNodeKeyMark(t *testing.T) {
	for _, c := range []struct {
		drop    bool
		key     []byte
		mark, k bool
	}{{true, nil, true, false}, {false, nil, false, false}, {true, []byte("k"), false, true}} {
		f := fake("a")
		r, _ := newRunner(f)
		r.DropNodeKey, r.NodeKey = c.drop, c.key
		r.Run(context.Background(), []Step{step("a", Upgrade, false)})
		_, mark := f.nodes["a"].files["drop-node-key"]
		_, k := f.nodes["a"].files["node-key.pub"]
		if mark != c.mark || k != c.k {
			t.Errorf("drop=%v key=%q: mark %v, key %v", c.drop, c.key, mark, k)
		}
	}
}

// The voice package goes to nodes that have it, and with --voice to all,
// for the node's architecture only.
func TestRunVoice(t *testing.T) {
	f := fake("a", "b", "c")
	r, console := newRunner(f)
	r.Release.Voice = map[string]string{"arm64": "/r/trellis-voice_0.2.0_arm64.deb"}
	a, b, c := step("a", Upgrade, false), step("b", Upgrade, false), step("c", Upgrade, false)
	a.State.Arch, b.State.Arch, c.State.Arch = "arm64", "arm64", "armhf"
	a.State.Voice, c.State.Voice = "0.1.0", "0.1.0"
	outs := r.Run(context.Background(), []Step{a, b, c})
	if Failed(outs) != 0 {
		t.Fatalf("%+v\n%s", outs, console)
	}
	if f.nodes["a"].files["voice.deb"] == "" || f.nodes["b"].files["voice.deb"] != "" || f.nodes["c"].files["voice.deb"] != "" {
		t.Errorf("without --voice: a %q, b %q, c %q", f.nodes["a"].files["voice.deb"], f.nodes["b"].files["voice.deb"], f.nodes["c"].files["voice.deb"])
	}
	f = fake("b")
	r, _ = newRunner(f)
	r.Release.Voice = map[string]string{"arm64": "/r/trellis-voice_0.2.0_arm64.deb"}
	r.Voice = true
	r.Run(context.Background(), []Step{b})
	if f.nodes["b"].files["voice.deb"] == "" {
		t.Error("--voice did not install it")
	}
}
