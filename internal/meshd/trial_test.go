package meshd

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"meridian/trellis/internal/radio"
	"meridian/trellis/internal/sysfs"
	"meridian/trellis/internal/trial"
)

type noReload struct{}

func (noReload) Reload(string) error   { return nil }
func (noReload) FilesChanged([]string) {}

// trialBoot boots trialDaemon's system again, now: its uptime starts over.
var trialBoot func()

// trialDaemon is a daemon with one radio on the mt7915e driver carrying
// the first mesh link, and a trial of that driver pending in this boot.
func trialDaemon(t *testing.T) (*Daemon, *trialReconciler, *time.Time, *[]string) {
	t.Helper()
	state := t.TempDir()
	d := &Daemon{log: slog.New(slog.NewTextHandler(io.Discard, nil)), facts: &Facts{Roles: map[string]radio.Role{}},
		opts: Options{StateDir: state, BootID: "boot-1"}}
	d.facts.Radios = []*radio.Radio{{Fingerprint: "r1", Phy: "phy0", Device: sysfs.Device{Bus: sysfs.BusPCI, Driver: "mt7915e"}}}
	d.facts.Roles["r1"] = radio.RoleMesh
	d.facts.MeshIfname = "mesh0"
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	// The system came up a minute before; a test that boots it again
	// moves boot.
	boot := now.Add(-time.Minute)
	r := newTrialReconciler(d)
	r.now = func() time.Time { return now }
	r.uptime = func() time.Duration { return now.Sub(boot) }
	r.store.Host, r.store.Now, r.store.Uptime = noReload{}, r.now, r.uptime
	trialBoot = func() { boot = now }
	var undone []string
	r.revert = func(_ context.Context, tr *trial.Trial, why string) error {
		undone = append(undone, tr.Name+": "+why)
		_, err := r.store.Revert(tr.Name, why, true)
		return err
	}
	place := filepath.Join(state, "lib", "mt7915e.ko")
	src := filepath.Join(state, "new.ko")
	for p, text := range map[string]string{place: "old", src: "new"} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.store.Begin(trial.Request{Name: "mt7915e-power", Version: "2", Module: "mt7915e", BootID: "boot-1",
		Files: map[string]string{place: src}}); err != nil {
		t.Fatal(err)
	}
	return d, r, &now, &undone
}

func trialState(t *testing.T, d *Daemon) (state string, confirmAfter int) {
	t.Helper()
	ts := d.trialsStatus()
	if len(ts) != 1 {
		t.Fatalf("%d trials in status", len(ts))
	}
	return ts[0].State, ts[0].ConfirmAfterS
}

// The radio must be there, its mesh started, for the whole healthy
// period: a dip starts the count again.
func TestTrialConfirmedAfterUnbrokenHealth(t *testing.T) {
	d, r, now, undone := trialDaemon(t)
	ctx := context.Background()
	pass := func() string {
		res, err := r.Reconcile(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return codes(res)
	}
	// The mesh has not started on the radio yet.
	if got := pass(); got != "info:trial-pending" {
		t.Fatalf("warnings = %s", got)
	}
	if st, left := trialState(t, d); st != trial.Pending || left != 0 {
		t.Fatalf("not healthy: %s, %d s", st, left)
	}
	d.facts.Supplicant.Joined = true
	*now = now.Add(time.Minute)
	pass()
	if st, left := trialState(t, d); st != trial.Pending || left != 301 {
		t.Fatalf("healthy: %s, %d s", st, left)
	}
	// Four minutes in, the radio is broken for a pass: the count restarts.
	*now = now.Add(4 * time.Minute)
	d.facts.Radios[0].Broken = "firmware stopped"
	pass()
	d.facts.Radios[0].Broken = ""
	*now = now.Add(30 * time.Second)
	pass()
	*now = now.Add(4 * time.Minute)
	pass()
	if st, _ := trialState(t, d); st != trial.Pending {
		t.Fatalf("kept %s after a dip", st)
	}
	*now = now.Add(61 * time.Second)
	if got := pass(); got != "" {
		t.Errorf("warnings at the confirmation: %s", got)
	}
	if st, _ := trialState(t, d); st != trial.Confirmed || len(*undone) != 0 {
		t.Fatalf("state %s, undone %v", st, *undone)
	}
	if ts, _ := r.store.List(); ts[0].State != trial.Confirmed {
		t.Errorf("stored state %s", ts[0].State)
	}
}

func TestTrialUndoneAtDeadlineAndWhenRadioGone(t *testing.T) {
	d, r, now, undone := trialDaemon(t)
	ctx := context.Background()
	// The radio never shows up on the new driver.
	d.facts.Radios = nil
	if _, err := r.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(14 * time.Minute)
	res, _ := r.Reconcile(ctx)
	if got := codes(res); got != "info:trial-pending" || !strings.Contains(res.Warnings[0].Message, "no radio on the mt7915e driver") {
		t.Fatalf("before the deadline: %s %+v", got, res.Warnings)
	}
	*now = now.Add(61 * time.Second)
	if _, err := r.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if len(*undone) != 1 || !strings.Contains((*undone)[0], "not healthy within 15 min (no radio") {
		t.Fatalf("undone = %v", *undone)
	}
	res, _ = r.Reconcile(ctx)
	if got := codes(res); got != "warning:trial-reverted" {
		t.Errorf("after the undo: %s", got)
	}
	if st, _ := trialState(t, d); st != trial.Reverted {
		t.Errorf("state %s", st)
	}
}

// A trial that has been healthy is not undone by the deadline for one bad
// pass after it: it has healthyFor to get back.
func TestTrialHealthyThenDipPastDeadline(t *testing.T) {
	d, r, now, undone := trialDaemon(t)
	ctx := context.Background()
	pass := func() {
		if _, err := r.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// Healthy from minute 12 (the system came up a minute before).
	*now = now.Add(11 * time.Minute)
	d.facts.Supplicant.Joined = true
	pass()
	// Minute 16: one pass without health, then back.
	*now = now.Add(4 * time.Minute)
	d.facts.Supplicant.Joined = false
	pass()
	if len(*undone) != 0 {
		t.Fatalf("undone by one bad pass after the deadline: %v", *undone)
	}
	*now = now.Add(15 * time.Second)
	d.facts.Supplicant.Joined = true
	pass()
	*now = now.Add(5*time.Minute + time.Second)
	pass()
	if st, _ := trialState(t, d); st != trial.Confirmed || len(*undone) != 0 {
		t.Fatalf("state %s, undone %v", st, *undone)
	}

	// Another trial, healthy, then lost for good: undone after healthyFor.
	d, r, now, undone = trialDaemon(t)
	*now = now.Add(11 * time.Minute)
	d.facts.Supplicant.Joined = true
	pass()
	*now = now.Add(4 * time.Minute)
	d.facts.Supplicant.Joined = false
	pass()
	*now = now.Add(4 * time.Minute)
	pass()
	if len(*undone) != 0 {
		t.Fatalf("undone within the grace: %v", *undone)
	}
	*now = now.Add(61 * time.Second)
	pass()
	if len(*undone) != 1 || !strings.Contains((*undone)[0], "not healthy for 5 min after it had been") {
		t.Fatalf("undone = %v", *undone)
	}
}

// meshd restarted late in a trial (another trial's reload, an upgrade):
// what it knew of the trial's health is gone, and its links are not up
// on its first pass. The trial gets healthyFor from then, not an undo.
func TestTrialPastDeadlineAfterMeshdRestart(t *testing.T) {
	d, r, now, undone := trialDaemon(t)
	ctx := context.Background()
	pass := func() {
		if _, err := r.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}
	restart := func() {
		again := newTrialReconciler(d)
		again.now, again.uptime, again.revert = r.now, r.uptime, r.revert
		again.store.Host, again.store.Now, again.store.Uptime = noReload{}, r.now, r.uptime
		r = again
	}
	// Healthy from minute 2 to 6, then the peer's link drops.
	*now = now.Add(time.Minute)
	d.facts.Supplicant.Joined = true
	pass()
	*now = now.Add(4 * time.Minute)
	d.facts.Supplicant.Joined = false
	pass()
	// Minute 16: meshd starts again, the mesh not yet joined.
	*now = now.Add(10 * time.Minute)
	restart()
	pass()
	if len(*undone) != 0 {
		t.Fatalf("undone on meshd's first pass: %v", *undone)
	}
	// Joined a minute later, healthy for healthyFor: kept.
	*now = now.Add(time.Minute)
	d.facts.Supplicant.Joined = true
	pass()
	*now = now.Add(5*time.Minute + time.Second)
	pass()
	if st, _ := trialState(t, d); st != trial.Confirmed || len(*undone) != 0 {
		t.Fatalf("state %s, undone %v", st, *undone)
	}

	// Never healthy after the restart: undone healthyFor after it.
	d, r, now, undone = trialDaemon(t)
	*now = now.Add(16 * time.Minute)
	restart()
	pass()
	*now = now.Add(4 * time.Minute)
	pass()
	if len(*undone) != 0 {
		t.Fatalf("undone within the grace: %v", *undone)
	}
	*now = now.Add(61 * time.Second)
	pass()
	if len(*undone) != 1 || !strings.Contains((*undone)[0], "not healthy within 15 min") {
		t.Fatalf("undone = %v", *undone)
	}
}

// A trial pending from another boot means the boot hook did not run: it
// is undone, not watched.
func TestTrialFromAnotherBootUndone(t *testing.T) {
	d, r, _, undone := trialDaemon(t)
	d.facts.Supplicant.Joined = true
	d.opts.BootID = "boot-2"
	if _, err := r.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*undone) != 1 || !strings.Contains((*undone)[0], "boot hook") {
		t.Fatalf("undone = %v", *undone)
	}
}

// A trial armed for the next boot has its whole deadline from that boot,
// however long before it was armed.
func TestTrialArmedDeadlineFromItsBoot(t *testing.T) {
	d, r, now, undone := trialDaemon(t)
	ctx := context.Background()
	if _, err := r.store.Revert("mt7915e-power", "test", false); err != nil {
		t.Fatal(err)
	}
	state := d.opts.StateDir
	if _, err := r.store.Begin(trial.Request{Name: "mt7915e-power", Version: "3", Module: "mt7915e", BootID: "boot-1", Reboot: true,
		Files: map[string]string{filepath.Join(state, "lib", "mt7915e.ko"): filepath.Join(state, "new.ko")}}); err != nil {
		t.Fatal(err)
	}
	// Rebooted 30 minutes after arming; the boot hook makes it pending.
	*now = now.Add(30 * time.Minute)
	trialBoot()
	d.opts.BootID = "boot-2"
	if _, err := r.store.Boot("boot-2"); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(20 * time.Second)
	res, err := r.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := codes(res); got != "info:trial-pending" || len(*undone) != 0 {
		t.Fatalf("just after the boot: %s %+v, undone %v", got, res.Warnings, *undone)
	}
	*now = now.Add(15 * time.Minute)
	if _, err := r.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if len(*undone) != 1 || !strings.Contains((*undone)[0], "not healthy within 15 min") {
		t.Fatalf("at the deadline: undone = %v", *undone)
	}
}

// A radio the operator ignores is not the trial's to judge: broken, it
// does not hold the trial back.
func TestTrialIgnoresIgnoredRadio(t *testing.T) {
	d, r, _, _ := trialDaemon(t)
	d.facts.Radios = append(d.facts.Radios, &radio.Radio{Fingerprint: "r2", Phy: "phy1", Ignored: true, Broken: "no mesh point mode",
		Device: sysfs.Device{Bus: sysfs.BusPCI, Driver: "mt7915e"}})
	d.facts.Supplicant.Joined = true
	tr := &trial.Trial{Module: "mt7915e"}
	if ok, why := r.healthy(tr); !ok {
		t.Fatalf("not healthy: %s", why)
	}
	d.facts.Radios[0].Broken = "firmware stopped"
	if ok, _ := r.healthy(tr); ok {
		t.Fatal("healthy with the radio in use broken")
	}
}
