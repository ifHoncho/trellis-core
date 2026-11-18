// Package upgrade moves a node from one Trellis release to another and
// back again if the node is not healthy afterwards.
//
// An upgrade is one .deb file. Before installing it the node checks its
// signature, notes how healthy it is now, rebuilds the package of the
// version it runs (so going back never depends on a file someone kept)
// and copies its configuration, mesh profile and identity aside. After
// installing, it watches itself: the daemon must run the new version, no
// reconciler may fail that worked before, a mesh peer must be back if
// there was one, and the web UI's socket must listen. If that does not
// hold within the time allowed, the old package and the copied
// configuration go back in.
//
// The state of an upgrade under way is on disk (pending.json), so a node
// that loses power in the middle finishes the job, or undoes it, when it
// next boots. Nothing of that depends on the new version: the upgrade
// leaves a copy of the old binary and a boot-time unit of its own that
// runs it, and removes both when it is done.
package upgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"meridian/trellis/internal/deb"
	"meridian/trellis/internal/fsutil"
	"meridian/trellis/internal/status"
)

// Package is the Debian package an upgrade replaces.
const Package = "trellis"

// ResumeBinary is the copy of the old version's binary, and ResumeUnit the
// systemd unit that runs it at boot while an upgrade is pending. Starting
// that unit is also how an operator finishes an interrupted upgrade by
// hand (ResumeHint): it works when the installed binary does not.
const (
	ResumeBinary = "resume"
	ResumeUnit   = "trellis-upgrade-resume.service"
	ResumeHint   = "sudo systemctl start " + ResumeUnit
)

// armResume sets up the boot-time resume: the old binary and a unit that
// runs it. The unit is enabled by its link alone; systemd reads both at
// the next boot, which is the only time they matter.
func (u *upgrader) armResume() error {
	if u.o.Self == "" {
		return nil
	}
	self, err := os.ReadFile(u.o.Self)
	if err != nil {
		return err
	}
	bin := u.path(ResumeBinary)
	if err := writeFileSync(bin, self, 0o700); err != nil {
		return err
	}
	unit := fmt.Sprintf(`# Written by "meshctl upgrade" for the upgrade under way, and removed when
# it ends. Not part of any package.
[Unit]
Description=Trellis: finish or roll back an interrupted upgrade
ConditionPathExists=%s
After=trellis-meshd.service

# exec, not oneshot: the boot goes on while the upgrade is finished, which
# takes minutes when the node is checked or put back.
[Service]
Type=exec
ExecStart=%s meshctl upgrade --resume --dir %s

[Install]
WantedBy=multi-user.target
`, u.path("pending.json"), bin, u.o.Dir)
	wants := filepath.Join(u.o.UnitDir, "multi-user.target.wants")
	if err := os.MkdirAll(wants, 0o755); err != nil {
		return err
	}
	if err := writeFileSync(filepath.Join(u.o.UnitDir, ResumeUnit), []byte(unit), 0o644); err != nil {
		return err
	}
	link := filepath.Join(wants, ResumeUnit)
	_ = os.Remove(link)
	if err := os.Symlink("../"+ResumeUnit, link); err != nil {
		return err
	}
	return syncDir(wants)
}

func (u *upgrader) disarmResume() {
	_ = os.Remove(filepath.Join(u.o.UnitDir, "multi-user.target.wants", ResumeUnit))
	_ = os.Remove(filepath.Join(u.o.UnitDir, ResumeUnit))
	_ = os.Remove(u.path(ResumeBinary))
}

// System is what an upgrade does to the node, apart from files under its
// own directories. Tests replace it.
type System interface {
	// Run runs a command and returns its standard output. A failure's
	// error carries the command's error output.
	Run(ctx context.Context, name string, args ...string) (string, error)
	// Status asks the running meshd for its status.
	Status(ctx context.Context) (*status.Status, error)
	Sleep(d time.Duration)
	Now() time.Time
}

// Options configure an upgrade.
type Options struct {
	// Dir holds the upgrade's own files: the pending state, the packages
	// and the configuration snapshot.
	Dir string
	// ConfigDir and StateDir are what a rollback puts back as they were.
	ConfigDir, StateDir string
	// Root is prefixed to the installed files' paths when the running
	// version is repacked ("" on a node; a directory in tests).
	Root string
	// Self is the running trellis binary. A copy is kept beside the
	// pending state for the boot-time resume: after a power loss the
	// installed binary is the new version's, which may be the broken part.
	// Empty: no boot-time resume is set up (tests).
	Self string
	// UnitDir is where the boot-time resume unit goes: the administrator's
	// systemd directory, which no package writes to.
	UnitDir string
	// Keys are the release keys this node trusts.
	Keys []deb.PublicKey
	// AllowUntrusted installs a package that is unsigned or signed by an
	// unknown key. AllowDowngrade installs an older version.
	AllowUntrusted, AllowDowngrade bool
	// RestoreConfig: a rollback asked for by the operator also puts back
	// the configuration and state as they were before the upgrade, which
	// undoes every change made since. Without it the node keeps them.
	// (A rollback after a failed health check always puts them back: they
	// are minutes old, and the new version may have rewritten them.)
	RestoreConfig bool
	// HealthTimeout is how long the node has to become healthy, and
	// Settle how long it must then stay so.
	HealthTimeout, Settle time.Duration
	// Log receives progress, one line at a time.
	Log io.Writer
}

func (o *Options) defaults() {
	if o.Dir == "" {
		o.Dir = DefaultDir
	}
	if o.ConfigDir == "" {
		o.ConfigDir = "/etc/trellis"
	}
	if o.StateDir == "" {
		o.StateDir = "/var/lib/trellis"
	}
	if o.UnitDir == "" {
		o.UnitDir = "/etc/systemd/system"
	}
	// Clean, so that comparing paths (the snapshot leaves the upgrade's own
	// directory out) does not depend on how they were written.
	o.Dir, o.ConfigDir, o.StateDir = filepath.Clean(o.Dir), filepath.Clean(o.ConfigDir), filepath.Clean(o.StateDir)
	if o.HealthTimeout <= 0 {
		o.HealthTimeout = 3 * time.Minute
	}
	if o.Settle <= 0 {
		o.Settle = 10 * time.Second
	}
	if o.Log == nil {
		o.Log = io.Discard
	}
}

// Health is what an upgrade compares before and after.
type Health struct {
	MeshdActive bool `json:"meshd_active"`
	WebSocket   bool `json:"web_socket"`
	API         bool `json:"api"`
	// Version is what the running meshd says it is.
	Version string `json:"version,omitempty"`
	Peers   int    `json:"peers"`
	// Failing names the reconcilers in error and the error-level warnings.
	Failing []string `json:"failing,omitempty"`
}

// Phases of an upgrade under way.
const (
	PhaseInstalling  = "installing"
	PhaseChecking    = "checking"
	PhaseRollingBack = "rolling-back"
)

// Pending is the state of an upgrade under way, kept on disk.
type Pending struct {
	Phase string `json:"phase"`
	// From and To are package versions; FromTrellis and ToTrellis what the
	// binaries report.
	From        string    `json:"from"`
	To          string    `json:"to"`
	FromTrellis string    `json:"from_trellis"`
	ToTrellis   string    `json:"to_trellis"`
	Started     time.Time `json:"started"`
	Baseline    Health    `json:"baseline"`
	// Reason is why a rollback began.
	Reason string `json:"reason,omitempty"`
	// Manual: the operator asked for the rollback. KeepConfig: it leaves
	// the configuration and state as they are now.
	Manual     bool `json:"manual,omitempty"`
	KeepConfig bool `json:"keep_config,omitempty"`
}

// Check's refusals that root on the node may override (meshctl's
// --allow-untrusted and --allow-downgrade); never through the API.
var (
	ErrNotTrusted = errors.New("not trusted")
	ErrOlder      = errors.New("older")
)

// refusal is one of those: its own message, and errors.Is its kind.
type refusal struct {
	kind error
	msg  string
}

func (r *refusal) Error() string        { return r.msg }
func (r *refusal) Is(target error) bool { return target == r.kind }

// PreviousVersion names the file that says which version previous.deb is.
const PreviousVersion = "previous.version"

// CanRollBack says whether the last upgrade can be undone: it upgraded,
// the node still runs what it installed, and the version it replaced is
// the one kept.
func CanRollBack(dir string, last *Result, installed string) bool {
	if last == nil || last.Result != ResultUpgraded || installed != last.To {
		return false
	}
	b, err := os.ReadFile(filepath.Join(dir, PreviousVersion))
	return err == nil && strings.TrimSpace(string(b)) == last.From
}

// ReasonAsked is the reason of a rollback the operator asked for.
const ReasonAsked = "asked for by the operator"

// Results of an upgrade.
const (
	ResultUpgraded   = "upgraded"
	ResultRolledBack = "rolled-back"
	ResultUnchanged  = "unchanged"
	ResultFailed     = "failed"
)

// Result is how the last upgrade ended, kept on disk (last.json).
type Result struct {
	Result   string    `json:"result"`
	From     string    `json:"from"`
	To       string    `json:"to"`
	Reason   string    `json:"reason,omitempty"`
	Finished time.Time `json:"finished"`
	// HealthyAfter: the node passed its health check at the end, on
	// whichever version it ended up with.
	HealthyAfter bool `json:"healthy_after"`
}

// OK says whether the node now runs the version that was asked for.
func (r *Result) OK() bool { return r.Result == ResultUpgraded }

type upgrader struct {
	o   Options
	sys System
}

func (u *upgrader) logf(format string, a ...any) {
	fmt.Fprintf(u.o.Log, format+"\n", a...)
}

func (u *upgrader) path(name string) string { return filepath.Join(u.o.Dir, name) }

// Installed describes the installed trellis package.
type Installed struct {
	Version string // the package's
	Trellis string // what the binary reports
}

func (u *upgrader) installed(ctx context.Context) (Installed, error) {
	ps := QueryPackage(ctx, u.sys)
	switch {
	case ps.Installed():
		return Installed{Version: ps.Version, Trellis: ps.Trellis}, nil
	case ps.HalfDone():
		return Installed{}, fmt.Errorf("the %s package is not cleanly installed (dpkg state %q): run dpkg --configure -a", Package, ps.State)
	}
	return Installed{}, errNotPackaged
}

// PackageState is what dpkg knows of the trellis package.
type PackageState struct {
	// State is dpkg's abbreviation ("ii" installed, "rc" removed with its
	// configuration left, "iF" half-configured, …); empty when dpkg has
	// never heard of the package, or there is no dpkg.
	State   string
	Version string
	// Trellis is the version the binary reports (the package's
	// Trellis-Version field; Version when absent).
	Trellis string
}

// dpkg's abbreviation: the wanted state ("i" install, "h" hold, "r"
// remove, …), then the actual one ("i" installed, "c" config files only,
// "n" not installed, and the half states "U", "F", "H", "W", "t").

// Installed: the package is installed and configured (held or not).
func (p PackageState) Installed() bool { return len(p.State) >= 2 && p.State[1] == 'i' }

// HalfDone: dpkg left the package between states (an interrupted install).
func (p PackageState) HalfDone() bool {
	return len(p.State) >= 2 && strings.ContainsRune("UFHWt", rune(p.State[1]))
}

// QueryPackage asks dpkg about the trellis package.
func QueryPackage(ctx context.Context, sys System) PackageState {
	out, err := sys.Run(ctx, "dpkg-query", "-W", "-f=${db:Status-Abbrev}\t${Version}\t${Trellis-Version}", Package)
	if err != nil {
		return PackageState{}
	}
	f := strings.Split(out, "\t")
	if len(f) != 3 {
		return PackageState{}
	}
	ps := PackageState{State: strings.TrimSpace(f[0]), Version: strings.TrimSpace(f[1]), Trellis: strings.TrimSpace(f[2])}
	if ps.Trellis == "" {
		ps.Trellis = ps.Version
	}
	return ps
}

var errNotPackaged = errors.New("Trellis was not installed from a package on this node, so there is no version to go back to. Install the package once with apt (sudo apt install ./FILE.deb); later upgrades can then use meshctl upgrade")

// Candidate is a package checked for installing on this node.
type Candidate struct {
	Info    *deb.Info
	Version string
	Trellis string
	// SignedBy is the trusted key's name, or empty for a package that was
	// let through unsigned or untrusted.
	SignedBy string
	Current  Installed
}

// Check reads a package and decides whether this node may install it.
func Check(ctx context.Context, pkg []byte, o Options, sys System) (*Candidate, error) {
	o.defaults()
	u := &upgrader{o, sys}
	return u.check(ctx, pkg)
}

func (u *upgrader) check(ctx context.Context, pkg []byte) (*Candidate, error) {
	info, err := deb.Read(pkg)
	if err != nil {
		return nil, err
	}
	if info.Get("Package") != Package {
		return nil, fmt.Errorf("this is the %s package, not %s", info.Get("Package"), Package)
	}
	c := &Candidate{Info: info, Version: info.Get("Version"), Trellis: info.Get("Trellis-Version")}
	if c.Trellis == "" {
		c.Trellis = c.Version
	}
	key, err := info.Verify(u.o.Keys)
	switch {
	case err == nil:
		c.SignedBy = strings.TrimSpace(key.ID() + " " + key.Comment)
	case !u.o.AllowUntrusted:
		return nil, &refusal{ErrNotTrusted, err.Error() + ". Only a package signed with a release key this node trusts is installed"}
	}
	arch, err := u.sys.Run(ctx, "dpkg", "--print-architecture")
	if err != nil {
		return nil, fmt.Errorf("cannot ask dpkg for this node's architecture: %w", err)
	}
	if a := info.Get("Architecture"); a != strings.TrimSpace(arch) && a != "all" {
		return nil, fmt.Errorf("the package is for %s; this node is %s", a, strings.TrimSpace(arch))
	}
	if c.Current, err = u.installed(ctx); err != nil {
		return nil, err
	}
	if c.Version == c.Current.Version {
		return nil, fmt.Errorf("version %s is already installed", c.Version)
	}
	if _, err := u.sys.Run(ctx, "dpkg", "--compare-versions", c.Version, "gt", c.Current.Version); err != nil && !u.o.AllowDowngrade {
		return nil, &refusal{ErrOlder, fmt.Sprintf("version %s is older than the installed %s", c.Version, c.Current.Version)}
	}
	return c, nil
}

// Run upgrades the node to the package in pkg, and goes back if the node
// is not healthy afterwards. The error is for an upgrade that could not
// start; one that started always gives a Result.
func Run(ctx context.Context, pkg []byte, o Options, sys System) (*Result, error) {
	o.defaults()
	u := &upgrader{o, sys}
	unlock, err := u.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	if p, err := u.readPending(); err != nil {
		return nil, fmt.Errorf("the state of an earlier upgrade cannot be read (%v); nothing is changed until it is settled: %s", err, ResumeHint)
	} else if p != nil {
		return nil, fmt.Errorf("an upgrade from %s to %s is still under way (%s); finish it first: %s", p.From, p.To, p.Phase, ResumeHint)
	}
	c, err := u.check(ctx, pkg)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(u.o.Dir, 0o700); err != nil {
		return nil, err
	}
	// Half-written files of a run that lost power before it began.
	fsutil.RemoveStale(u.o.Dir)
	u.logf("upgrade %s -> %s", c.Current.Version, c.Version)
	if c.SignedBy != "" {
		u.logf("signature good: key %s", c.SignedBy)
	} else {
		u.logf("WARNING: installing a package no trusted key vouches for (--allow-untrusted)")
	}

	p := &Pending{
		Phase: PhaseInstalling, From: c.Current.Version, To: c.Version,
		FromTrellis: c.Current.Trellis, ToTrellis: c.Trellis, Started: u.sys.Now(),
	}
	p.Baseline = u.health(ctx)
	u.logf("before: %s", p.Baseline.describe())

	// Everything a rollback needs, before anything changes.
	prev, err := u.repack(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot keep the installed version to go back to: %w", err)
	}
	if err := writeFileSync(u.path("previous.deb"), prev, 0o600); err != nil {
		return nil, err
	}
	// Which version previous.deb holds, read without opening it: if this
	// upgrade goes no further, the last one's rollback must not offer it.
	if err := writeFileSync(u.path(PreviousVersion), []byte(p.From+"\n"), 0o600); err != nil {
		return nil, err
	}
	if err := writeFileSync(u.path("new.deb"), pkg, 0o600); err != nil {
		return nil, err
	}
	if err := u.snapshot(); err != nil {
		return nil, fmt.Errorf("cannot copy the configuration aside: %w", err)
	}
	if err := u.armResume(); err != nil {
		return nil, fmt.Errorf("cannot set up the boot-time resume: %w", err)
	}
	u.logf("kept: the %s package, configuration, mesh profile and identity", p.From)
	if err := u.writePending(p); err != nil {
		u.disarmResume()
		return nil, err
	}
	return u.install(ctx, p), nil
}

// install installs the new package, then checks.
func (u *upgrader) install(ctx context.Context, p *Pending) *Result {
	u.logf("installing %s", p.To)
	if _, err := u.sys.Run(ctx, "dpkg", "-i", "--force-confold", "--force-confdef", "--force-downgrade", u.path("new.deb")); err != nil {
		return u.rollback(ctx, p, "the package did not install: "+lastLine(err.Error()))
	}
	p.Phase = PhaseChecking
	if err := u.writePending(p); err != nil {
		return u.rollback(ctx, p, "cannot record the upgrade's state: "+err.Error())
	}
	return u.verify(ctx, p)
}

// verify waits for the node to be healthy on the new version.
func (u *upgrader) verify(ctx context.Context, p *Pending) *Result {
	u.logf("checking the node's health (up to %s)", u.o.HealthTimeout)
	if why := u.waitHealthy(ctx, p.Baseline, p.ToTrellis); why != "" {
		return u.rollback(ctx, p, why)
	}
	u.logf("healthy on %s", p.To)
	return u.finish(p, &Result{Result: ResultUpgraded, HealthyAfter: true})
}

// rollback puts the old package and the copied configuration back.
func (u *upgrader) rollback(ctx context.Context, p *Pending, why string) *Result {
	if p.Manual {
		u.logf("going back to %s, as asked", p.From)
	} else {
		u.logf("NOT healthy: %s", why)
		u.logf("going back to %s", p.From)
	}
	p.Phase, p.Reason = PhaseRollingBack, why
	_ = u.writePending(p)
	res := &Result{Result: ResultRolledBack, Reason: why}

	// Stopped, so the new version cannot rewrite what is put back.
	if p.Baseline.MeshdActive {
		_, _ = u.sys.Run(ctx, "systemctl", "stop", "trellis-meshd.service")
	}
	if p.KeepConfig {
		u.logf("the configuration and state stay as they are")
	} else if err := u.restore(); err != nil {
		u.logf("cannot put the configuration back: %v", err)
		res.Result, res.Reason = ResultFailed, why+"; and the configuration could not be put back: "+err.Error()
	}
	if _, err := u.sys.Run(ctx, "dpkg", "-i", "--force-confold", "--force-confdef", "--force-downgrade", u.path("previous.deb")); err != nil {
		u.logf("cannot reinstall %s: %v", p.From, err)
		res.Result, res.Reason = ResultFailed, why+"; and the old version could not be reinstalled: "+lastLine(err.Error())
	}
	if p.Baseline.MeshdActive {
		if _, err := u.sys.Run(ctx, "systemctl", "start", "trellis-meshd.service"); err != nil {
			u.logf("cannot start meshd: %v", err)
		}
	}
	if res.Result == ResultRolledBack {
		if bad := u.waitHealthy(ctx, p.Baseline, p.FromTrellis); bad != "" {
			u.logf("back on %s, but not as healthy as before: %s", p.From, bad)
		} else {
			res.HealthyAfter = true
			u.logf("back on %s and healthy", p.From)
		}
	}
	return u.finish(p, res)
}

// finish records the outcome and clears the pending state.
func (u *upgrader) finish(p *Pending, res *Result) *Result {
	res.From, res.To, res.Finished = p.From, p.To, u.sys.Now()
	if b, err := json.MarshalIndent(res, "", "  "); err == nil {
		_ = writeFileSync(u.path("last.json"), append(b, '\n'), 0o600)
	}
	// A failed rollback stays pending: --resume tries it again.
	if res.Result != ResultFailed {
		_ = os.Remove(u.path("pending.json"))
		_ = os.Remove(u.path("new.deb"))
		u.disarmResume()
	}
	return res
}

// Resume finishes an upgrade that was interrupted: by a power loss, or by
// the death of the process that ran it. Without one it returns nil.
func Resume(ctx context.Context, o Options, sys System) (*Result, error) {
	o.defaults()
	u := &upgrader{o, sys}
	unlock, err := u.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	p, err := u.readPending()
	if err != nil || p == nil {
		return nil, err
	}
	u.logf("resuming the upgrade %s -> %s (was %s)", p.From, p.To, p.Phase)
	switch p.Phase {
	case PhaseRollingBack:
		return u.rollback(ctx, p, p.Reason), nil
	case PhaseChecking:
		return u.verify(ctx, p), nil
	}
	// Interrupted while installing: what is on the disk decides.
	in, err := u.installed(ctx)
	if err != nil {
		u.logf("the package was left half-installed; letting dpkg finish")
		_, _ = u.sys.Run(ctx, "dpkg", "--configure", "-a")
		in, err = u.installed(ctx)
	}
	switch {
	case err != nil:
		return u.rollback(ctx, p, "the new package was left half-installed"), nil
	case in.Version == p.To:
		p.Phase = PhaseChecking
		_ = u.writePending(p)
		return u.verify(ctx, p), nil
	case in.Version == p.From:
		u.logf("nothing was installed; the node still runs %s", p.From)
		return u.finish(p, &Result{Result: ResultUnchanged, Reason: "interrupted before the new version was installed"}), nil
	}
	return u.rollback(ctx, p, fmt.Sprintf("found version %s installed, neither the old nor the new", in.Version)), nil
}

// Rollback goes back to the version before the last upgrade, by hand.
func Rollback(ctx context.Context, o Options, sys System) (*Result, error) {
	o.defaults()
	u := &upgrader{o, sys}
	unlock, err := u.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	if p, err := u.readPending(); err != nil {
		return nil, fmt.Errorf("the state of an earlier upgrade cannot be read (%v); nothing is changed until it is settled: %s", err, ResumeHint)
	} else if p != nil {
		return nil, fmt.Errorf("an upgrade is still under way (%s); finish it first: %s", p.Phase, ResumeHint)
	}
	last, err := ReadResult(o.Dir)
	if err != nil || last == nil || last.Result != ResultUpgraded {
		return nil, errors.New("there is no finished upgrade to go back from")
	}
	if _, err := os.Stat(u.path("previous.deb")); err != nil {
		return nil, errors.New("the previous version's package is no longer kept")
	}
	in, err := u.installed(ctx)
	if err != nil {
		return nil, err
	}
	if in.Version != last.To {
		return nil, fmt.Errorf("the node runs %s, not the %s the last upgrade installed", in.Version, last.To)
	}
	b, err := os.ReadFile(u.path("previous.deb"))
	if err != nil {
		return nil, err
	}
	prev, err := deb.Read(b)
	if err != nil {
		return nil, err
	}
	if prev.Get("Version") != last.From {
		return nil, fmt.Errorf("the package kept is %s, not the %s the last upgrade replaced", prev.Get("Version"), last.From)
	}
	p := &Pending{From: last.From, To: last.To, FromTrellis: prev.Get("Trellis-Version"), ToTrellis: in.Trellis,
		Started: u.sys.Now(), Manual: true, KeepConfig: !o.RestoreConfig}
	if p.FromTrellis == "" {
		p.FromTrellis = last.From
	}
	if !o.RestoreConfig {
		// The configuration stays only if the version gone back to reads
		// it: settings the newer one added (a new key in node.yaml) would
		// keep the old meshd from starting.
		if why := u.oldRejectsConfig(ctx, b); why != "" {
			u.logf("%s does not read the configuration as it is now (%s): the configuration from before the upgrade goes back with it", last.From, why)
			p.KeepConfig = false
		}
	}
	if !p.KeepConfig {
		fi, err := os.Stat(u.path("snapshot"))
		if err != nil {
			return nil, errors.New("the configuration from before the upgrade is no longer kept")
		}
		u.logf("the configuration and state go back to %s: every change since is undone",
			fi.ModTime().Local().Format("2006-01-02 15:04"))
	}
	p.Baseline = u.health(ctx)
	// A power loss in the middle is finished at the next boot, as for an
	// upgrade.
	if err := u.armResume(); err != nil {
		return nil, fmt.Errorf("cannot set up the boot-time resume: %w", err)
	}
	return u.rollback(ctx, p, ReasonAsked), nil
}

// oldRejectsConfig runs the binary of the package kept (prev) on the
// configuration as it is now ("meshctl config validate") and returns why
// it refuses it, or "" when it reads it, or when it cannot be asked.
func (u *upgrader) oldRejectsConfig(ctx context.Context, prev []byte) string {
	node := filepath.Join(u.o.ConfigDir, "node.yaml")
	if _, err := os.Stat(node); err != nil {
		return ""
	}
	files, err := deb.Payload(prev)
	if err != nil {
		return ""
	}
	var bin []byte
	for _, f := range files {
		if f.Path == "/usr/bin/trellis" {
			bin = f.Body
		}
	}
	if bin == nil {
		return ""
	}
	exe := u.path("previous-trellis")
	if err := os.WriteFile(exe, bin, 0o700); err != nil {
		return ""
	}
	defer os.Remove(exe)
	args := []string{"meshctl", "config", "validate", node}
	if prof := filepath.Join(u.o.ConfigDir, "profile.yaml"); fileExists(prof) {
		args = append(args, prof)
	}
	out, err := u.sys.Run(ctx, exe, args...)
	if err == nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "INVALID") {
			continue
		}
		if l := strings.TrimSpace(line); l != "" && !strings.HasSuffix(l, "OK") && !strings.Contains(l, ": OK") {
			return l
		}
	}
	return lastLine(err.Error())
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// ErrBusy: another process works on an upgrade of this node now.
var ErrBusy = errors.New("another upgrade process is working on this node now (meshctl upgrade --status)")

// lock takes the node's upgrade lock for the whole of a run, a resume or a
// rollback: two of them at once (the boot-time resume and one started by
// hand) would install and restore over each other.
func (u *upgrader) lock() (func(), error) {
	if err := os.MkdirAll(u.o.Dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(u.path("lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, ErrBusy
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// ReadPending returns the upgrade under way, or nil.
func ReadPending(dir string) (*Pending, error) {
	return (&upgrader{o: Options{Dir: dir}}).readPending()
}

// ReadResult returns how the last upgrade ended, or nil.
func ReadResult(dir string) (*Result, error) {
	b, err := os.ReadFile(filepath.Join(dir, "last.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r Result
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (u *upgrader) readPending() (*Pending, error) {
	b, err := os.ReadFile(u.path("pending.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p Pending
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("%s: %w", u.path("pending.json"), err)
	}
	return &p, nil
}

func (u *upgrader) writePending(p *Pending) error {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return writeFileSync(u.path("pending.json"), append(b, '\n'), 0o600)
}

// health reads the node's state now.
func (u *upgrader) health(ctx context.Context) Health {
	active := func(unit string) bool {
		out, _ := u.sys.Run(ctx, "systemctl", "is-active", unit)
		return strings.TrimSpace(out) == "active"
	}
	h := Health{MeshdActive: active("trellis-meshd.service"), WebSocket: active("trellis-webd.socket")}
	st, err := u.sys.Status(ctx)
	if err != nil {
		return h
	}
	h.API, h.Version = true, st.Version
	h.Peers = len(st.MeshLink.Peers)
	for _, l := range st.MoreLinks {
		h.Peers += len(l.Peers)
	}
	for _, r := range st.Reconcile.Reconcilers {
		if r.LastError != "" {
			h.Failing = append(h.Failing, "reconciler "+r.Name)
		}
	}
	for _, w := range st.Warnings {
		if w.Level == "error" {
			h.Failing = append(h.Failing, w.Code)
		}
	}
	sort.Strings(h.Failing)
	return h
}

func (h Health) describe() string {
	if !h.MeshdActive {
		return "meshd is not running"
	}
	if !h.API {
		return "meshd runs but does not answer"
	}
	s := fmt.Sprintf("meshd %s, %d mesh peer(s)", h.Version, h.Peers)
	if len(h.Failing) > 0 {
		s += ", failing: " + strings.Join(h.Failing, ", ")
	}
	return s
}

// judge compares the node now with how it was before. It returns "" when
// the node is at least as healthy, or what is wrong.
func judge(before, now Health, wantVersion string) string {
	if before.WebSocket && !now.WebSocket {
		return "the web UI's socket no longer listens"
	}
	if !before.MeshdActive {
		// meshd was stopped before and is left stopped: nothing to compare.
		return ""
	}
	switch {
	case !now.MeshdActive:
		return "meshd is not running"
	case !now.API && before.API:
		return "meshd does not answer"
	case now.API && wantVersion != "" && now.Version != wantVersion:
		return fmt.Sprintf("meshd answers as version %s, not %s", now.Version, wantVersion)
	}
	was := map[string]bool{}
	for _, f := range before.Failing {
		was[f] = true
	}
	for _, f := range now.Failing {
		if !was[f] {
			return f + " fails, and did not before"
		}
	}
	// Peers are read through meshd's API: unknown while it is silent.
	if now.API && before.Peers > 0 && now.Peers == 0 {
		return fmt.Sprintf("no mesh peer has come back (%d before)", before.Peers)
	}
	return ""
}

// waitHealthy returns "" once the node has been healthy for the settle
// time, or what was still wrong when the time ran out.
func (u *upgrader) waitHealthy(ctx context.Context, before Health, wantVersion string) string {
	const step = 3 * time.Second
	deadline := u.sys.Now().Add(u.o.HealthTimeout)
	var goodSince time.Time
	why, said := "", ""
	for {
		why = judge(before, u.health(ctx), wantVersion)
		now := u.sys.Now()
		if why == "" {
			if goodSince.IsZero() {
				goodSince = now
			}
			if now.Sub(goodSince) >= u.o.Settle {
				return ""
			}
		} else {
			goodSince = time.Time{}
			if why != said {
				u.logf("  waiting: %s", why)
				said = why
			}
		}
		if !now.Before(deadline) || ctx.Err() != nil {
			if why == "" {
				why = "the node did not stay healthy long enough"
			}
			return why
		}
		u.sys.Sleep(step)
	}
}

// repack rebuilds the installed package from dpkg's records and the files
// on disk, so it can be installed again.
func (u *upgrader) repack(ctx context.Context) ([]byte, error) {
	st, err := u.sys.Run(ctx, "dpkg-query", "-s", Package)
	if err != nil {
		return nil, err
	}
	p := &deb.Package{Scripts: map[string][]byte{}, Time: u.sys.Now().UTC().Truncate(time.Second)}
	for _, f := range deb.ParseControl(st) {
		switch strings.ToLower(f.Name) {
		case "status", "installed-size":
		case "conffiles":
			for _, line := range strings.Split(f.Value, "\n") {
				if fields := strings.Fields(line); len(fields) >= 2 {
					p.Conffiles = append(p.Conffiles, fields[0])
				}
			}
		default:
			p.Control = append(p.Control, f)
		}
	}
	list, err := u.sys.Run(ctx, "dpkg-query", "-L", Package)
	if err != nil {
		return nil, err
	}
	for _, name := range strings.Split(strings.TrimSpace(list), "\n") {
		name = strings.TrimSpace(name)
		if name == "" || name == "/." || !strings.HasPrefix(name, "/") {
			continue
		}
		disk := filepath.Join(u.o.Root, name)
		fi, err := os.Lstat(disk)
		if err != nil {
			continue // removed by the operator; dpkg copes
		}
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(disk)
			if err != nil {
				return nil, err
			}
			p.Files = append(p.Files, deb.File{Path: name, Mode: fs.ModeSymlink | 0o777, Link: link})
		case fi.Mode().IsRegular():
			body, err := os.ReadFile(disk)
			if err != nil {
				return nil, err
			}
			p.Files = append(p.Files, deb.File{Path: name, Mode: fi.Mode().Perm(), Body: body})
		}
	}
	if len(p.Files) == 0 {
		return nil, errors.New("dpkg lists no files for the installed package")
	}
	have, _ := u.sys.Run(ctx, "dpkg-query", "--control-list", Package)
	for _, s := range strings.Fields(have) {
		switch s {
		case "preinst", "postinst", "prerm", "postrm":
			body, err := u.sys.Run(ctx, "dpkg-query", "--control-show", Package, s)
			if err != nil {
				return nil, err
			}
			p.Scripts[s] = []byte(body)
		}
	}
	return p.Bytes()
}

// snapshot copies the configuration and state aside.
func (u *upgrader) snapshot() error {
	snap := u.path("snapshot")
	if err := os.RemoveAll(snap); err != nil {
		return err
	}
	if err := copyTree(u.o.ConfigDir, filepath.Join(snap, "etc"), ""); err != nil {
		return err
	}
	if err := copyTree(u.o.StateDir, filepath.Join(snap, "state"), u.o.Dir); err != nil {
		return err
	}
	return syncDir(snap)
}

// restore puts the snapshot back, removing what the new version added.
func (u *upgrader) restore() error {
	snap := u.path("snapshot")
	for _, pair := range [][2]string{{"etc", u.o.ConfigDir}, {"state", u.o.StateDir}} {
		src := filepath.Join(snap, pair[0])
		if _, err := os.Stat(src); err != nil {
			return fmt.Errorf("no snapshot of %s", pair[1])
		}
		if err := emptyDir(pair[1], u.o.Dir); err != nil {
			return err
		}
		if err := copyTree(src, pair[1], ""); err != nil {
			return err
		}
	}
	return nil
}

// emptyDir removes everything in dir except the path keep.
func emptyDir(dir, keep string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if keep != "" && (p == keep || strings.HasPrefix(keep, p+string(filepath.Separator))) {
			continue
		}
		if err := os.RemoveAll(p); err != nil {
			return err
		}
	}
	return nil
}

// copyTree copies files, directories and symlinks from src to dst with
// their modes and owners, leaving out the path skip. A missing src gives
// an empty dst.
func copyTree(src, dst, skip string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == src && errors.Is(err, fs.ErrNotExist) {
				return filepath.SkipAll
			}
			return err
		}
		if skip != "" && p == skip {
			return filepath.SkipDir
		}
		rel, _ := filepath.Rel(src, p)
		to := filepath.Join(dst, rel)
		fi, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			if err := os.MkdirAll(to, 0o755); err != nil {
				return err
			}
			if err := os.Chmod(to, fi.Mode().Perm()); err != nil {
				return err
			}
		case fi.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			_ = os.Remove(to)
			return os.Symlink(link, to)
		case fi.Mode().IsRegular():
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if err := writeFileSync(to, b, fi.Mode().Perm()); err != nil {
				return err
			}
			_ = os.Chtimes(to, fi.ModTime(), fi.ModTime())
		default:
			return nil // sockets and the like are not configuration
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && os.Geteuid() == 0 {
			_ = os.Lchown(to, int(st.Uid), int(st.Gid))
		}
		return nil
	})
}

// writeFileSync writes a file so that it is whole on disk before anything
// depends on it (fsutil.WriteFile).
func writeFileSync(name string, b []byte, mode fs.FileMode) error {
	return fsutil.WriteFile(name, b, mode)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		// dpkg's last line says most.
		lines := strings.Split(s, "\n")
		return strings.TrimSpace(lines[len(lines)-1])
	}
	return s
}
