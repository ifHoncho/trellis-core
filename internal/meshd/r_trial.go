package meshd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"meridian/trellis/internal/reconcile"
	"meridian/trellis/internal/status"
	"meridian/trellis/internal/trial"
)

// How long a trial must run well before it is kept, and how long it has
// to get there.
const (
	trialHealthyFor = 5 * time.Minute
	trialDeadline   = 15 * time.Minute
)

// trialReconciler sees driver, firmware and board file trials through
// (internal/trial): it confirms a pending one once the radios it concerns
// have run well for a while, and has one that does not get there undone.
// The old files are put back by meshctl, outside meshd's bounds: they are
// under /lib, which meshd cannot write.
type trialReconciler struct {
	d     *Daemon
	store *trial.Store
	// healthySince is when each pending trial last became healthy.
	healthySince map[string]time.Time
	// lostAt is when a pending trial that has been healthy in this run
	// of meshd stopped being so: it has healthyFor to get back, and the
	// deadline no longer applies to it.
	lostAt map[string]time.Time
	// seen is when this run of meshd first saw each pending trial. These
	// times are not kept across a restart of meshd: a trial it finds past
	// its deadline (meshd restarted late in the trial) gets healthyFor
	// from then, not an undo before its links could come up.
	seen map[string]time.Time
	// reverting is when the undo of a trial was asked for: not asked
	// again for a while.
	reverting map[string]time.Time
	// arrived: armed trials of earlier boots were made pending, once.
	arrived bool
	now     func() time.Time
	uptime  func() time.Duration
	// revert has a trial undone. Variable for tests.
	revert func(ctx context.Context, t *trial.Trial, why string) error
}

func newTrialReconciler(d *Daemon) *trialReconciler {
	r := &trialReconciler{d: d, store: trial.New(d.opts.StateDir), healthySince: map[string]time.Time{}, lostAt: map[string]time.Time{}, seen: map[string]time.Time{}, reverting: map[string]time.Time{}, now: time.Now, uptime: trial.Uptime}
	r.revert = r.revertOutside
	// meshd never waits on the lock: meshctl may hold it while it stops
	// meshd. A pass that finds it taken tries again later.
	r.store.NoWait = true
	if dir := d.opts.TestTrialDir; dir != "" {
		// A lab: the stand-in driver is a file, loaded by nothing, and
		// meshd may write it.
		r.store.Host = labTrialHost{dir: dir}
		r.revert = func(_ context.Context, t *trial.Trial, why string) error {
			_, err := r.store.Revert(t.Name, why, true)
			return err
		}
	}
	return r
}

func (r *trialReconciler) Name() string { return "trials" }

func (r *trialReconciler) Teardown(context.Context) error { return nil }

func (r *trialReconciler) healthyFor() time.Duration {
	if d := r.d.opts.TrialHealthyFor; d > 0 {
		return d
	}
	return trialHealthyFor
}

func (r *trialReconciler) deadline() time.Duration {
	if d := r.d.opts.TrialDeadline; d > 0 {
		return d
	}
	return trialDeadline
}

func (r *trialReconciler) Reconcile(ctx context.Context) (reconcile.Result, error) {
	var res reconcile.Result
	warn := func(level, code, format string, args ...any) {
		res.Warnings = append(res.Warnings, reconcile.Warning{Level: level, Code: code, Message: fmt.Sprintf(format, args...)})
	}
	if !r.store.Any() {
		r.d.facts.Update(func(f *Facts) { f.Trials = nil })
		return res, nil
	}
	if !r.arrived {
		// Where the boot hook did not run, a trial armed for this boot
		// is still marked armed though its files have loaded.
		switch _, err := r.store.Arrive(r.d.opts.BootID); {
		case errors.Is(err, trial.ErrBusy):
			res.RequeueAfter = 5 * time.Second
		case err != nil:
			r.d.log.Warn("trials", "err", err)
			r.arrived = true
		default:
			r.arrived = true
		}
	}
	trials, err := r.store.List()
	if err != nil {
		return res, err
	}
	now := r.now()
	out := make([]status.Trial, 0, len(trials))
	for _, t := range trials {
		st := status.Trial{Name: t.Name, Kind: t.Kind, Version: t.Version, Module: t.Module, State: t.State, Latched: t.Latched,
			Reason: t.Reason, Started: t.Started, Ended: t.Ended, RebootNeeded: t.RebootNeeded}
		for _, f := range t.Files {
			st.Files = append(st.Files, f.Path)
		}
		switch {
		case t.State == trial.Armed:
			warn("info", "trial-armed", "Trial %s armed: loads at next boot; confirms after %s healthy, reverts on reboot before that",
				t.Describe(), r.healthyFor())
		case t.State == trial.Pending && t.BootID != r.d.opts.BootID:
			// The boot hook did not run: the new files loaded again in
			// this boot. Undone now, late.
			r.undo(ctx, t, "the node restarted before the trial was confirmed (found after boot: the boot hook, trellis-trial.service, did not run)", warn)
		case t.State == trial.Pending:
			var kept bool
			if st.ConfirmAfterS, kept = r.watch(ctx, t, now, &res, warn); kept {
				st.State, st.Ended = trial.Confirmed, now
			}
		case t.State == trial.Reverted && t.Latched:
			more := ""
			if t.RebootNeeded {
				more = " Reboot to load the previous version."
			}
			warn("warning", "trial-reverted", "Trial %s reverted: %s. Locked out; re-arm: sudo meshctl trial rearm %s.%s", t.Describe(), t.Reason, t.Name, more)
		}
		out = append(out, st)
	}
	r.d.facts.Update(func(f *Facts) { f.Trials = out })
	for name := range r.healthySince {
		if !pendingNamed(trials, name) {
			delete(r.healthySince, name)
		}
	}
	for name := range r.lostAt {
		if !pendingNamed(trials, name) {
			delete(r.lostAt, name)
		}
	}
	for name := range r.seen {
		if !pendingNamed(trials, name) {
			delete(r.seen, name)
		}
	}
	for name := range r.reverting {
		if !pendingNamed(trials, name) {
			delete(r.reverting, name)
		}
	}
	return res, nil
}

func pendingNamed(ts []*trial.Trial, name string) bool {
	for _, t := range ts {
		if t.Name == name && t.State == trial.Pending {
			return true
		}
	}
	return false
}

// watch judges a trial pending in this boot: confirmed after healthyFor
// of unbroken health, undone at the deadline (loadedFor) if it has not
// been healthy, or after healthyFor without health if it has. A trial
// this run of meshd has just found has healthyFor at least. It returns
// the seconds until it would be kept, 0 when that is not counting, and
// whether it was kept just now.
func (r *trialReconciler) watch(ctx context.Context, t *trial.Trial, now time.Time, res *reconcile.Result, warn func(level, code, format string, args ...any)) (int, bool) {
	// A crash ends the boot, and a trial pending from an earlier boot is
	// undone before it gets here: the crashes meshd knows of in this boot
	// came before the trial's files loaded.
	seen, known := r.seen[t.Name]
	if !known {
		seen = now
		r.seen[t.Name] = now
	}
	ok, why := r.healthy(t)
	if !ok {
		_, wasHealthy := r.healthySince[t.Name]
		delete(r.healthySince, t.Name)
		lost, hadBeen := r.lostAt[t.Name]
		if wasHealthy && !hadBeen {
			lost, hadBeen = now, true
			r.lostAt[t.Name] = now
		}
		left := max(r.deadline()-r.loadedFor(t), r.healthyFor()-now.Sub(seen))
		if hadBeen {
			left = r.healthyFor() - now.Sub(lost)
		}
		if left <= 0 {
			if hadBeen {
				r.undo(ctx, t, fmt.Sprintf("not healthy for %s after it had been (%s)", spoken(r.healthyFor()), why), warn)
			} else {
				r.undo(ctx, t, fmt.Sprintf("not healthy within %s (%s)", spoken(r.deadline()), why), warn)
			}
		} else {
			warn("info", "trial-pending", "Trial %s: not healthy (%s). Reverts in %s.", t.Describe(), why, spoken(left))
			res.RequeueAfter = min(left, 15*time.Second)
		}
		return 0, false
	}
	since, counting := r.healthySince[t.Name]
	if !counting {
		since = now
		r.healthySince[t.Name] = now
	}
	delete(r.lostAt, t.Name)
	left := r.healthyFor() - now.Sub(since)
	if left > 0 {
		warn("info", "trial-pending", "Trial %s: confirms in %s if healthy; reverts on reboot", t.Describe(), left.Round(time.Second))
		res.RequeueAfter = min(left, 15*time.Second)
		return int(left/time.Second) + 1, false
	}
	if _, err := r.store.Confirm(t.Name); errors.Is(err, trial.ErrBusy) {
		res.RequeueAfter = 5 * time.Second
		return 0, false
	} else if err != nil {
		warn("warning", "trial-pending", "Cannot record confirmation of trial %s: %v", t.Describe(), err)
		return 0, false
	}
	delete(r.healthySince, t.Name)
	r.d.log.Info("trial confirmed", "trial", t.Describe(), "healthy_for", r.healthyFor())
	res.Changed = true
	return 0, true
}

// loadedFor is how long a trial pending in this boot has had its files
// loaded, by the system's uptime: an armed trial's deadline runs from the
// boot it loads in, not from when it was armed, and a clock set after
// boot does not move it.
func (r *trialReconciler) loadedFor(t *trial.Trial) time.Duration {
	return max(r.uptime()-time.Duration(t.LoadedUptimeS*float64(time.Second)), 0)
}

// undo has a trial undone and latched, once.
func (r *trialReconciler) undo(ctx context.Context, t *trial.Trial, why string, warn func(level, code, format string, args ...any)) {
	if at, asked := r.reverting[t.Name]; asked && r.now().Sub(at) < 2*time.Minute {
		warn("warning", "trial-reverting", "Reverting trial %s: %s", t.Describe(), why)
		return
	}
	r.d.log.Warn("undoing a trial", "trial", t.Describe(), "why", why)
	if err := r.revert(ctx, t, why); err != nil {
		warn("warning", "trial-reverting", "Trial %s revert failed (%s): %v. Revert manually: sudo meshctl trial revert %s", t.Describe(), why, err, t.Name)
		return
	}
	r.reverting[t.Name] = r.now()
	delete(r.healthySince, t.Name)
	delete(r.lostAt, t.Name)
}

// revertOutside runs meshctl trial revert in a unit of its own: it writes
// under /lib and reloads the module, which stops and starts meshd.
func (r *trialReconciler) revertOutside(ctx context.Context, t *trial.Trial, why string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return exec.CommandContext(ctx, "systemd-run", "--quiet", "--collect", "--description=Trellis: undo a trial",
		exe, "meshctl", "trial", "revert", "--latch", "--reason", why, "--state-dir", r.d.opts.StateDir, t.Name).Run()
}

// healthy reports whether the radios a trial concerns run well: present,
// not broken, the mesh started on the links they carry, and a peer back
// if there was one when the trial began.
func (r *trialReconciler) healthy(t *trial.Trial) (bool, string) {
	if dir := r.d.opts.TestTrialDir; dir != "" {
		return labTrialHealth(dir, t)
	}
	driver := t.RadioDriver()
	if driver == "" {
		return true, ""
	}
	type link struct {
		ifindex int
		joined  bool
		name    string
	}
	var links []link
	found, broken := 0, ""
	r.d.facts.Read(func(f *Facts) {
		for _, rd := range f.Radios {
			if rd.Device.Driver != driver {
				continue
			}
			// An ignored radio counts as present; its state is not the
			// trial's to judge.
			found++
			if rd.Ignored {
				continue
			}
			if rd.Broken != "" {
				broken = rd.Name() + ": " + rd.Broken
			}
			n := f.Roles[rd.Fingerprint].Link()
			if n < 0 {
				continue
			}
			l := link{ifindex: f.MeshIfindex, joined: f.Supplicant.Joined, name: f.MeshIfname}
			if n > 0 {
				lf := f.linkFacts(n)
				l = link{ifindex: lf.Ifindex, joined: lf.Supplicant.Joined, name: lf.Ifname}
			}
			if l.name == "" {
				l.name = meshIfname(n)
			}
			links = append(links, l)
		}
	})
	switch {
	case found == 0:
		return false, "no radio on the " + driver + " driver is present"
	case broken != "":
		return false, broken
	}
	peers := 0
	for _, l := range links {
		if !l.joined {
			return false, "the mesh has not started on " + l.name
		}
		if r.d.nl == nil || l.ifindex == 0 {
			continue
		}
		if sts, err := r.d.nl.Stations(l.ifindex); err == nil {
			for _, s := range sts {
				if s.Established() {
					peers++
				}
			}
		}
	}
	if t.HadPeer && peers == 0 {
		return false, "no mesh peer is back on its links"
	}
	return true, ""
}

// trialsStatus is the trials as the reconciler last saw them.
func (d *Daemon) trialsStatus() []status.Trial {
	out := []status.Trial{}
	d.facts.Read(func(f *Facts) { out = append(out, f.Trials...) })
	return out
}

// Lab stand-ins ---------------------------------------------------------------

// labTrialHost is the trials' system in a lab: a reload is a line in a
// log, and fails while the directory holds a file named "reload-fails".
type labTrialHost struct{ dir string }

func (h labTrialHost) Reload(module string) error {
	if _, err := os.Stat(filepath.Join(h.dir, "reload-fails")); err == nil {
		return fmt.Errorf("lab: %s does not load", module)
	}
	f, err := os.OpenFile(filepath.Join(h.dir, "reloads"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, module)
	return err
}

func (labTrialHost) FilesChanged([]string) {}

// labTrialHealth reads the stand-in radio's health from files the lab
// writes: "present", "joined" and "peer".
func labTrialHealth(dir string, t *trial.Trial) (bool, string) {
	has := func(name string) bool {
		_, err := os.Stat(filepath.Join(dir, name))
		return err == nil
	}
	switch {
	case !has("present"):
		return false, "no radio on the " + t.RadioDriver() + " driver is present"
	case !has("joined"):
		return false, "the mesh has not started on its link"
	case t.HadPeer && !has("peer"):
		return false, "no mesh peer is back on its links"
	}
	return true, ""
}
