package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// remoteDir is where the run puts its files on a node, in the user's home.
const remoteDir = `"$HOME"/.trellis-deploy`

// Runner carries out a plan.
type Runner struct {
	SSH     Remote
	Release *Release
	// Profile is a mesh profile's YAML for nodes without one; it holds the
	// mesh secret, so it is written only to the node, never to a log.
	Profile []byte
	// Passwords are the sudo passwords of the nodes whose sudo asks for
	// one, by address (FindPasswords); never logged.
	Passwords   map[string]string
	WebPassword string // the web UI's admin password for new nodes; empty: not set
	Reboot      bool   // reboot new nodes (and nodes that ask for it) and wait for them
	RebootWait  time.Duration
	Poll        time.Duration // how often a node is asked while waiting (10 s)
	MeshdWait   time.Duration // for meshd after a reboot (2 min)
	ResultWait  time.Duration // for an upgrade's result after the connection dropped (10 min)
	Jobs        int           // new installs at once
	KeepGoing   bool          // go on with the upgrades after one goes back
	// Fast installs over an older (or newer) Trellis with apt, without
	// meshctl upgrade's health check and rollback: a developer's quick
	// iterations.
	Fast bool
	// NodeKey is a release public key the nodes are made to trust
	// (/etc/trellis/release-keys/deploy-dev.pub): a development key.
	NodeKey []byte
	// DropNodeKey: the run uses the built-in release keys alone, so a
	// development key an earlier run gave a node is removed once the node
	// took this release.
	DropNodeKey bool
	// Log is where a node's whole output goes.
	Log func(s Step) io.WriteCloser
	Out io.Writer // the console
	mu  sync.Mutex
}

// Outcome is how a node's step ended.
type Outcome struct {
	Step    Step
	OK      bool
	Result  string // in words: "installed 0.2.0", "went back to 0.1.0: …"
	Version string // what the node runs at the end, as it says
	Notes   []string
	// WiFi is how a phone joins a new node's own Wi-Fi; nil when unknown.
	// Shown at the end of the run, never written to a log.
	WiFi *Join
}

// Join is a node's Wi-Fi for phones and laptops.
type Join struct {
	SSID       string `json:"ssid"`
	Passphrase string `json:"passphrase"`
	Enabled    bool   `json:"enabled"`
}

// Name is how the console and the summary call a node.
func (s Step) Name() string {
	if s.Label != "" {
		return s.Label
	}
	return s.baseName()
}

func (s Step) baseName() string {
	switch {
	case s.Host.Callsign != "":
		return s.Host.Callsign
	case s.State.Hostname != "":
		return s.State.Hostname
	}
	return s.Host.Key()
}

func (r *Runner) say(s Step, format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintf(r.Out, "[%s] %s\n", s.Name(), fmt.Sprintf(format, args...))
}

// Run carries out the steps: new installs Jobs at a time, then the
// upgrades one by one, stopping at the first that went back unless
// KeepGoing. Steps with nothing to do get an outcome too.
func (r *Runner) Run(ctx context.Context, steps []Step) []Outcome {
	out := make([]Outcome, len(steps))
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(1, r.Jobs))
	for i, s := range steps {
		switch s.Action {
		case Skip:
			out[i] = Outcome{Step: s, Result: "skipped: " + s.Why}
		case Current:
			out[i] = Outcome{Step: s, OK: true, Result: "up to date", Version: s.State.Trellis}
		case Install:
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				out[i] = r.do(ctx, s, r.install)
			}()
		}
	}
	wg.Wait()
	stopped := ""
	for i, s := range steps {
		if s.Action != Upgrade {
			continue
		}
		if stopped != "" {
			out[i] = Outcome{Step: s, Result: "not run: " + stopped}
			continue
		}
		out[i] = r.do(ctx, s, r.upgrade)
		if !out[i].OK && !r.KeepGoing {
			stopped = s.Name() + " did not upgrade"
		}
	}
	return out
}

func (r *Runner) do(ctx context.Context, s Step, f func(context.Context, Step, io.Writer) Outcome) Outcome {
	log := r.Log(s)
	defer log.Close()
	fmt.Fprintf(log, "== %s (%s), %s, %s\n", s.Name(), s.Host.Target, s.Action, time.Now().Format(time.RFC3339))
	o := f(ctx, s, &markers{r: r, s: s, log: log})
	fmt.Fprintf(log, "== %s\n", o.Result)
	return o
}

// markers passes a node's output to its log, and its "@@" lines to the
// console.
type markers struct {
	r    *Runner
	s    Step
	log  io.Writer
	part []byte
	seen []string
	// lastError is the last "error: …" line a command on the node printed:
	// meshctl's reason for refusing.
	lastError string
}

func (m *markers) Write(p []byte) (int, error) {
	m.log.Write(p)
	m.part = append(m.part, p...)
	for {
		i := strings.IndexByte(string(m.part), '\n')
		if i < 0 {
			return len(p), nil
		}
		line := strings.TrimSpace(string(m.part[:i]))
		m.part = m.part[i+1:]
		if e, ok := strings.CutPrefix(line, "error: "); ok {
			m.lastError = e
		}
		if rest, ok := strings.CutPrefix(line, "@@ "); ok {
			m.seen = append(m.seen, rest)
			switch k, msg, _ := strings.Cut(rest, " "); k {
			case "step":
				m.r.say(m.s, "%s", msg)
			case "warn", "fail":
				m.r.say(m.s, "!! %s", msg)
			}
		}
	}
}

func (m *markers) has(word string) bool {
	for _, l := range m.seen {
		if l == word || strings.HasPrefix(l, word+" ") {
			return true
		}
	}
	return false
}

// sudo runs a command as root on the node: without a password where sudo
// takes none, else with the run's password on its standard input (never
// on a command line). more follows on standard input, for the command.
func (r *Runner) sudo(ctx context.Context, s Step, cmd string, more string, out io.Writer) error {
	var in io.Reader
	full := "sudo -n " + cmd
	if !s.State.NoPass {
		full = "sudo -S -p '' " + cmd
		in = strings.NewReader(r.Passwords[s.Host.Key()] + "\n" + more)
	} else if more != "" {
		in = strings.NewReader(more)
	}
	return r.SSH.Run(ctx, s.State.Route, full, in, out)
}

// FindPasswords finds the sudo password of every node that will be worked
// on and whose sudo asks for one. Nodes often share a password, so each one
// given is tried on the next node first; ask is called only for a node none
// of them opens (retry: the last answer did not work there), up to three
// times. An empty answer leaves that node out, as does a node that cannot
// be reached to try: it is returned as a Skip, and the rest go on.
func (r *Runner) FindPasswords(ctx context.Context, steps []Step, ask func(s Step, retry bool) (string, error)) ([]Step, error) {
	if r.Passwords == nil {
		r.Passwords = map[string]string{}
	}
	var known []string
	for i, s := range steps {
		if (s.Action != Install && s.Action != Upgrade && s.Action != Setup) || s.State.NoPass {
			continue
		}
		found, why := false, ""
		for _, pw := range known {
			ok, err := r.trySudo(ctx, s, pw)
			if err != nil {
				why = err.Error()
				break
			}
			if ok {
				r.Passwords[s.Host.Key()], found = pw, true
				break
			}
		}
		for try := 0; !found && why == "" && try < 3; try++ {
			pw, err := ask(s, try > 0)
			if err != nil {
				return steps, err
			}
			if pw == "" {
				why = "no sudo password given"
				break
			}
			ok, err := r.trySudo(ctx, s, pw)
			switch {
			case err != nil:
				why = err.Error()
			case ok:
				r.Passwords[s.Host.Key()], found = pw, true
				known = append(known, pw)
			}
		}
		if !found {
			if why == "" {
				why = "the sudo password did not work"
			}
			steps[i].Action, steps[i].Why = Skip, why
		}
	}
	return steps, nil
}

// trySudo reports whether pw opens sudo on the node; an error when the node
// cannot be reached to try.
func (r *Runner) trySudo(ctx context.Context, s Step, pw string) (bool, error) {
	var out strings.Builder
	err := r.SSH.Run(ctx, s.State.Route, "sudo -S -p '' -k true", strings.NewReader(pw+"\n"), &out)
	if err != nil && Unreachable(err) {
		return false, fmt.Errorf("unreachable: %s", firstLine(out.String(), err))
	}
	return err == nil, nil
}

func (r *Runner) put(ctx context.Context, s Step, files map[string]string, data map[string][]byte) error {
	for name, local := range files {
		if err := r.SSH.Put(ctx, s.State.Route, local, remoteDir, name); err != nil {
			return err
		}
	}
	for name, b := range data {
		if err := r.SSH.PutBytes(ctx, s.State.Route, strings.NewReader(string(b)), remoteDir, name); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) cleanup(ctx context.Context, s Step) {
	_, _ = r.SSH.Output(ctx, s.State.Route, "rm -rf "+remoteDir)
}

func (r *Runner) install(ctx context.Context, s Step, w io.Writer) Outcome {
	o := Outcome{Step: s}
	m := w.(*markers)
	r.say(s, "copying the packages")
	files := map[string]string{"trellis.deb": s.Package}
	if r.Release.Batman != "" {
		files["dkms.deb"] = r.Release.Batman
	}
	data := map[string][]byte{"install.sh": []byte(installScript)}
	r.keyFiles(data)
	if len(r.Profile) > 0 && !s.State.Profile {
		data["profile.yaml"] = r.Profile
	}
	if err := r.put(ctx, s, files, data); err != nil {
		o.Result = "failed: " + err.Error()
		return o
	}
	defer r.cleanup(ctx, s)
	err := r.sudo(ctx, s, fmt.Sprintf("sh %s/install.sh %s %s %d", remoteDir, remoteDir, quote(s.Callsign), s.Host.NodeID), "", w)
	if err != nil || !m.has("done") {
		o.Result = "failed: the install did not finish (see its log)"
		if err != nil && Unreachable(err) {
			o.Result = "failed: the connection was lost during the install (see its log)"
		}
		return o
	}
	o.OK, o.Result = true, "installed "+r.Release.Version
	if !r.Reboot {
		o.Notes = append(o.Notes, "reboot it to start Trellis")
		return o
	}
	r.say(s, "rebooting")
	if err := r.reboot(ctx, s, w); err != nil {
		o.OK, o.Result = false, "installed, but "+err.Error()
		return o
	}
	if r.WebPassword != "" {
		if err := r.sudo(ctx, s, "/usr/bin/meshctl web password --stdin", r.WebPassword+"\n", w); err != nil {
			o.Notes = append(o.Notes, "the web UI's admin password was not set (see the log)")
		} else {
			r.say(s, "web UI admin password set")
		}
	}
	o.Version = r.version(ctx, s)
	o.Notes = append(o.Notes, r.doctor(ctx, s)...)
	o.WiFi = r.join(ctx, s)
	return o
}

// join reads how a phone joins the node's Wi-Fi (meshctl ap): into memory
// only, as it holds the Wi-Fi's passphrase.
func (r *Runner) join(ctx context.Context, s Step) *Join {
	var b strings.Builder
	if err := r.sudo(ctx, s, "/usr/bin/meshctl ap --json", "", &b); err != nil {
		return nil
	}
	text := b.String()
	var j Join
	if i := strings.IndexByte(text, '{'); i < 0 || json.Unmarshal([]byte(text[i:]), &j) != nil || !j.Enabled || j.SSID == "" {
		return nil
	}
	return &j
}

// JoinDetails writes how to join each new node's Wi-Fi, for the console.
func JoinDetails(w io.Writer, outs []Outcome) {
	var js []Outcome
	width := 4
	for _, o := range outs {
		if o.WiFi != nil {
			js = append(js, o)
			width = max(width, len(o.Step.Name()))
		}
	}
	if len(js) == 0 {
		return
	}
	fmt.Fprintf(w, "\nEach node's own Wi-Fi, for phones and laptops (then open https://10.73.0.1):\n")
	for _, o := range js {
		fmt.Fprintf(w, "  %-*s  %-24s  %s\n", width, o.Step.Name(), o.WiFi.SSID, o.WiFi.Passphrase)
	}
}

// doctorReport is what the run reads of meshctl doctor --json.
type doctorReport struct {
	Checks []struct {
		Status string `json:"status"`
		Name   string `json:"name"`
	} `json:"checks"`
	Warn int `json:"warn"`
	Fail int `json:"fail"`
}

// doctor asks the node's doctor how it is, and says what is not right: its
// failed checks and its warnings, by name.
func (r *Runner) doctor(ctx context.Context, s Step) []string {
	var b strings.Builder
	if err := r.sudo(ctx, s, "/usr/bin/meshctl doctor --json", "", &b); err != nil && !strings.Contains(b.String(), "{") {
		return nil
	}
	text := b.String()
	var rep doctorReport
	if i := strings.IndexByte(text, '{'); i < 0 || json.Unmarshal([]byte(text[i:]), &rep) != nil {
		return nil
	}
	names := func(status string) string {
		var n []string
		for _, c := range rep.Checks {
			if c.Status == status {
				n = append(n, c.Name)
			}
		}
		if len(n) > 3 {
			n = append(n[:3], "…")
		}
		return strings.Join(n, ", ")
	}
	var out []string
	if rep.Fail > 0 {
		out = append(out, fmt.Sprintf("doctor: %d failed (%s)", rep.Fail, names("FAIL")))
	}
	if rep.Warn > 0 {
		out = append(out, fmt.Sprintf("doctor: %d warning(s) (%s)", rep.Warn, names("WARN")))
	}
	return out
}

// reboot restarts the node and waits until it is back with meshd running.
func (r *Runner) reboot(ctx context.Context, s Step, w io.Writer) error {
	boot, _ := r.SSH.Output(ctx, s.State.Route, "cat /proc/sys/kernel/random/boot_id")
	_ = r.sudo(ctx, s, "systemctl reboot", "", w) // the connection goes with it
	r.SSH.Disconnect(s.State.Route)
	wait := r.RebootWait
	if wait == 0 {
		wait = 6 * time.Minute
	}
	deadline := time.Now().Add(wait)
	for {
		if err := sleep(ctx, r.poll()); err != nil {
			return err
		}
		now, err := r.SSH.Output(ctx, s.State.Route, "cat /proc/sys/kernel/random/boot_id")
		if err == nil && strings.TrimSpace(now) != "" && strings.TrimSpace(now) != strings.TrimSpace(boot) {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("it was not back within %s of the reboot", wait)
		}
		r.SSH.Disconnect(s.State.Route)
	}
	for end := time.Now().Add(orDefault(r.MeshdWait, 2*time.Minute)); ; {
		if out, err := r.SSH.Output(ctx, s.State.Route, "systemctl is-active trellis-meshd"); err == nil && strings.TrimSpace(out) == "active" {
			r.say(s, "back, meshd running")
			return nil
		}
		if time.Now().After(end) {
			return fmt.Errorf("meshd is not running after the reboot (sudo meshctl doctor on the node)")
		}
		if err := sleep(ctx, r.poll()/2); err != nil {
			return err
		}
	}
}

func (r *Runner) upgrade(ctx context.Context, s Step, w io.Writer) Outcome {
	o := Outcome{Step: s}
	r.say(s, "copying the package")
	files := map[string]string{"trellis.deb": s.Package}
	if r.Release.Batman != "" && r.Release.BatmanVersion != s.State.Batman {
		files["dkms.deb"] = r.Release.Batman
	}
	data := map[string][]byte{"upgrade.sh": []byte(upgradeScript)}
	r.keyFiles(data)
	if len(r.Profile) > 0 && !s.State.Profile {
		data["profile.yaml"] = r.Profile // a node waiting in setup mode joins
	}
	if err := r.put(ctx, s, files, data); err != nil {
		o.Result = "failed: " + err.Error()
		return o
	}
	mode, done := "upgrade", "upgraded to "
	switch {
	case r.Fast:
		mode, done = "install", "installed (no health check) "
	case s.State.Cmp == "same":
		mode, done = "reinstall", "reinstalled "
	case s.State.Cmp == "newer":
		mode, done = "downgrade", "installed the older build "
	}
	err := r.sudo(ctx, s, fmt.Sprintf("sh %s/upgrade.sh %s %s %s %d", remoteDir, remoteDir, mode, quote(callsignFor(s)), s.Host.NodeID), "", w)
	switch {
	case err == nil:
		o.OK, o.Result = true, done+r.Release.Version
	case (mode == "install" || mode == "reinstall") && Unreachable(err):
		r.say(s, "the connection dropped (the node restarts its mesh); waiting for the result")
		o.OK, o.Result = r.awaitExit(ctx, s, done+r.Release.Version)
	case mode == "install" || mode == "reinstall":
		o.Result = "failed: " + or(w.(*markers).lastError, "it did not finish (see its log)")
	case Unreachable(err):
		// A node reached over the mesh loses its own path when its meshd
		// restarts: the upgrade goes on there. Read how it ended.
		r.say(s, "the connection dropped (the node restarts its mesh); waiting for the result")
		o.OK, o.Result = r.awaitUpgrade(ctx, s, w)
	default:
		o.OK, o.Result = r.lastUpgrade(ctx, s, w)
		if o.OK {
			o.Result = "upgraded to " + r.Release.Version
		} else if e := w.(*markers).lastError; e != "" && !strings.HasPrefix(o.Result, "went back") {
			// Refused before it began (a key the node does not trust, a
			// package for another architecture): the node said why.
			o.Result = "refused by the node: " + e
		}
	}
	r.cleanup(ctx, s)
	if o.OK {
		o.Version = r.version(ctx, s)
		if out, err := r.SSH.Output(ctx, s.State.Route, "test -e /run/reboot-required && echo yes"); err == nil && strings.TrimSpace(out) == "yes" {
			o.Notes = append(o.Notes, "reboot it to finish (the node asks for one)")
		}
		o.Notes = append(o.Notes, r.doctor(ctx, s)...)
		r.say(s, "%s", o.Result)
	} else {
		r.say(s, "!! %s", o.Result)
	}
	return o
}

// lastResult is the node's last.json (internal/upgrade.Result).
type lastResult struct {
	Result string `json:"result"`
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

func (r *Runner) readLast(ctx context.Context, s Step) (lastResult, bool, error) {
	var b strings.Builder
	err := r.sudo(ctx, s, `sh -c 'cat /var/lib/trellis/upgrade/last.json; test -e /var/lib/trellis/upgrade/pending.json && echo PENDING'`, "", &b)
	var l lastResult
	text := b.String()
	pending := strings.Contains(text, "PENDING")
	if i := strings.IndexByte(text, '{'); i >= 0 {
		_ = json.NewDecoder(strings.NewReader(text[i:])).Decode(&l)
	}
	return l, pending, err
}

// describe reads a node's last.json against the release: its "to" is the
// package's Debian version (r.Release.Debian), not the Trellis version.
func (r *Runner) describe(l lastResult) (bool, string) {
	switch {
	case l.Result == "upgraded" && l.To == r.Release.Debian:
		return true, "upgraded to " + r.Release.Version
	case l.Result == "rolled-back":
		return false, "went back to " + l.From + ": " + l.Reason
	case l.Result == "":
		return false, "failed: no upgrade result on the node (see its log)"
	}
	return false, l.Result + ": " + l.Reason
}

func (r *Runner) lastUpgrade(ctx context.Context, s Step, w io.Writer) (bool, string) {
	l, _, err := r.readLast(ctx, s)
	if err != nil {
		return false, "failed: the upgrade did not finish, and its result cannot be read (see its log)"
	}
	return r.describe(l)
}

func (r *Runner) awaitUpgrade(ctx context.Context, s Step, w io.Writer) (bool, string) {
	wait := orDefault(r.ResultWait, 10*time.Minute)
	for end := time.Now().Add(wait); time.Now().Before(end); {
		if err := sleep(ctx, r.poll()); err != nil {
			return false, "stopped: " + err.Error()
		}
		r.SSH.Disconnect(s.State.Route)
		l, pending, err := r.readLast(ctx, s)
		// A result about another version is an earlier upgrade's.
		if err != nil || pending || l.To != r.Release.Debian {
			continue
		}
		return r.describe(l)
	}
	return false, fmt.Sprintf("no result within %s: sudo meshctl upgrade --status on the node", wait)
}

// keyFiles adds what the scripts need for the development key: the key to
// trust, or the mark that it goes.
func (r *Runner) keyFiles(data map[string][]byte) {
	switch {
	case len(r.NodeKey) > 0:
		data["node-key.pub"] = r.NodeKey
	case r.DropNodeKey:
		data["drop-node-key"] = []byte("1\n")
	}
}

// callsignFor is the callsign a node gets when the run configures it.
func callsignFor(s Step) string {
	switch {
	case s.Callsign != "":
		return s.Callsign
	case s.Host.Callsign != "":
		return s.Host.Callsign
	}
	return CallsignFrom(s.State.Hostname)
}

// awaitExit reads the outcome an apt install left on the node after the
// session was lost (upgradeScript writes it).
func (r *Runner) awaitExit(ctx context.Context, s Step, done string) (bool, string) {
	wait := orDefault(r.ResultWait, 10*time.Minute)
	for end := time.Now().Add(wait); time.Now().Before(end); {
		if err := sleep(ctx, r.poll()); err != nil {
			return false, "stopped: " + err.Error()
		}
		r.SSH.Disconnect(s.State.Route)
		out, err := r.SSH.Output(ctx, s.State.Route, "cat "+remoteDir+"/exit 2>/dev/null; echo; grep '^error: ' "+remoteDir+"/run.log | tail -n 1")
		if err != nil {
			continue
		}
		// Line one is the exit file (empty while the script runs), line two
		// the last error line of its log.
		code, rest, _ := strings.Cut(out, "\n")
		switch strings.TrimSpace(code) {
		case "":
			continue // still running
		case "0":
			return true, done
		default:
			return false, "failed: " + or(strings.TrimPrefix(strings.TrimSpace(rest), "error: "), "see its log")
		}
	}
	return false, fmt.Sprintf("no result within %s: see ~/.trellis-deploy/run.log on the node", wait)
}

func (r *Runner) version(ctx context.Context, s Step) string {
	out, err := r.SSH.Output(ctx, s.State.Route, "/usr/bin/trellis version")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(firstLine(out, nil))
}

func or(s, alt string) string {
	if s != "" {
		return s
	}
	return alt
}

func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

func (r *Runner) poll() time.Duration {
	if r.Poll > 0 {
		return r.Poll
	}
	return 10 * time.Second
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Summary writes the outcomes as a table.
func Summary(w io.Writer, outs []Outcome) {
	width := 4
	for _, o := range outs {
		width = max(width, len(o.Step.Name()))
	}
	fmt.Fprintf(w, "\n%-*s  %-22s  %s\n", width, "node", "address", "result")
	for _, o := range outs {
		mark := "ok"
		if !o.OK {
			mark = "--"
			if o.Step.Action != Skip && !strings.HasPrefix(o.Result, "not run") {
				mark = "!!"
			}
		}
		res := o.Result
		if len(o.Notes) > 0 {
			res += " (" + strings.Join(o.Notes, "; ") + ")"
		}
		fmt.Fprintf(w, "%-*s  %-22s  %s %s\n", width, o.Step.Name(), o.Step.Host.Key(), mark, res)
	}
}

// Failed counts the outcomes that did not do what they set out to.
func Failed(outs []Outcome) int {
	n := 0
	for _, o := range outs {
		if !o.OK && (o.Step.Action == Install || o.Step.Action == Upgrade) {
			n++
		}
	}
	return n
}
