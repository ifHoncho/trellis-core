package meshd

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"meridian/trellis/internal/crashlog"
	"meridian/trellis/internal/reconcile"
	"meridian/trellis/internal/status"
	"meridian/trellis/internal/trial"
)

// resetWarnFor is how long a reset stays a warning; after that it is
// history, in status.
const resetWarnFor = 24 * time.Hour

// resetsShown is how many resets status lists.
const resetsShown = 10

// openCrashLog brings the node's reset history up to this boot: the crash
// records the kernel left are read and given a verdict, once each.
func (d *Daemon) openCrashLog() {
	o := crashlog.Options{StateDir: d.opts.StateDir, BootID: d.opts.BootID, Now: time.Now(), Uptime: trial.Uptime(), Backend: crashlog.Backend(d.opts.SysRoot)}
	if d.opts.NoHostConfig {
		o.Dirs = []string{} // a lab's host's crash records are not this node's
	}
	l, added, err := crashlog.Open(o)
	if err != nil {
		d.log.Warn("reset history", "err", err)
	}
	d.crashes = l
	for _, e := range added {
		d.log.Warn("the node went down before this boot", "verdict", e.Verdict, "what", e.Summary, "detail", e.Detail, "at", e.Time.Format(time.RFC3339))
	}
}

// clockSet tells the reset history that the clock is set: the boot's
// start is known from now, and resets dated by the clock the board
// started with are dated again (crashlog.Log.ClockSet).
func (d *Daemon) clockSet() {
	if d.crashes == nil {
		return
	}
	if err := d.crashes.ClockSet(time.Now(), trial.Uptime()); err != nil {
		d.log.Warn("reset history", "err", err)
	}
}

// closeCrashLog notes that meshd stops, and whether the system is shutting
// down: a boot that follows a meshd still running is a reset; one that
// follows a meshd stopped by hand says nothing either way. Once: the
// signal and the way out both call it.
func (d *Daemon) closeCrashLog() {
	d.crashClosed.Do(func() {
		if d.crashes == nil {
			return
		}
		if err := d.crashes.Stopped(d.systemStopping()); err != nil {
			d.log.Warn("reset history", "err", err)
		}
	})
}

// systemStopping reports whether the system is shutting down or
// rebooting, as systemd says it. A lab's host is not this node's system.
func (d *Daemon) systemStopping() bool {
	if d.opts.NoHostConfig {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "systemctl", "is-system-running").Output()
	return strings.TrimSpace(string(out)) == "stopping"
}

// resetsStatus lists the last resets, newest first.
func (d *Daemon) resetsStatus() []status.Reset {
	out := []status.Reset{}
	if d.crashes == nil {
		return out
	}
	for _, e := range d.crashes.Recent(time.Now(), 0) {
		if len(out) == resetsShown {
			break
		}
		out = append(out, status.Reset{Time: e.Time, TimeUnknown: e.TimePending || e.TimeUnknown, Verdict: e.Verdict, Summary: e.Summary, Detail: e.Detail, UptimeS: e.UptimeS})
	}
	return out
}

// resetWarning is the warning for resets in the last day: the latest, and
// how many there were.
func resetWarning(l *crashlog.Log, now time.Time) (reconcile.Warning, bool) {
	if l == nil {
		return reconcile.Warning{}, false
	}
	recent := l.Recent(now, resetWarnFor)
	if len(recent) == 0 {
		return reconcile.Warning{}, false
	}
	last := recent[0]
	at := "at " + last.Time.Format("15:04 2 Jan")
	if last.TimePending || last.TimeUnknown {
		at = "(time unknown)"
	}
	msg := fmt.Sprintf("Unplanned reset %s: %s.", at, last.Summary)
	if n := len(recent); n > 1 {
		msg = fmt.Sprintf("%d unplanned resets in 24 h, last %s: %s.", n, at, last.Summary)
	}
	return reconcile.Warning{Level: "warning", Code: "node-reset", Message: msg}, true
}
