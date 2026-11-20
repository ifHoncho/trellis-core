package meshctl

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"meridian/trellis/internal/api"
	"meridian/trellis/internal/release"
	"meridian/trellis/internal/upgrade"
)

// upgradeUnit is where an upgrade runs (upgrade.RunUnit).
const upgradeUnit = upgrade.RunUnit

func cmdUpgrade(client *api.Client, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	fs.SetOutput(stdout)
	untrusted := fs.Bool("allow-untrusted", false, "install a package that is unsigned, or signed by a key this node does not trust")
	downgrade := fs.Bool("allow-downgrade", false, "install an older version than the one running")
	timeout := fs.Duration("timeout", 3*time.Minute, "how long the node has to become healthy on the new version")
	check := fs.Bool("check", false, "only check the package: its signature, architecture and version")
	resume := fs.Bool("resume", false, "finish, or undo, an upgrade that was interrupted")
	rollback := fs.Bool("rollback", false, "go back to the version before the last upgrade")
	showStatus := fs.Bool("status", false, "show the upgrade under way and how the last one ended")
	restoreConfig := fs.Bool("restore-config", false, "with --rollback: also put back the configuration and state from before the upgrade, undoing every change made since")
	dir := fs.String("dir", upgrade.DefaultDir, "where the upgrade keeps its files")
	mesh := fs.Bool("mesh", false, "upgrade every node of the mesh, one at a time (one package per architecture); without files, show the upgrade under way")
	yes := fs.Bool("yes", false, "with --mesh: start it (without: only show the plan)")
	follow := fs.Bool("follow", false, "with --mesh: follow its progress until it is over")
	cancelMesh := fs.Bool("cancel", false, "with --mesh: call off the mesh-wide upgrade started on this node")
	fs.Usage = func() {
		fmt.Fprint(stdout, `usage: meshctl upgrade [flags] FILE.deb
       meshctl upgrade --resume | --rollback [--restore-config] | --status
       meshctl upgrade --mesh [--yes] [--follow] FILE.deb...
       meshctl upgrade --mesh [--follow] | --mesh --cancel

Installs a Trellis release and checks the node afterwards: meshd on the new
version, nothing failing that worked before, a mesh peer back if there was
one. A node that is not healthy in time goes back to the version and
configuration it had.

With --mesh, every node of the mesh that can takes the release from this
node, one at a time: farthest first, this node last. Each checks its own
health; the first that goes back halts the rest.

`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *showStatus {
		return upgradeStatus(*dir, stdout)
	}
	if *mesh || *cancelMesh {
		return cmdUpgradeMesh(client, fs.Args(), *yes, *follow, *cancelMesh, stdout)
	}
	// Checking a package changes nothing, and needs no root.
	if os.Geteuid() != 0 && !*check {
		return errors.New("Root required: sudo meshctl upgrade")
	}
	switch {
	case *resume && *rollback:
		return errors.New("--resume and --rollback cannot be combined")
	case (*resume || *rollback) && fs.NArg() != 0:
		return errors.New("--resume and --rollback take no file")
	case !*resume && !*rollback && fs.NArg() != 1:
		fs.Usage()
		return errors.New("name one package file")
	}
	// An upgrade left unfinished is finished by the version it started
	// from, never by the one it installed: that may be the broken part.
	if *resume {
		if err := execKeptBinary(*dir, args); err != nil {
			return err
		}
	}

	// From a shell, the work goes to a unit of its own, and this process
	// follows its log. Under systemd already (that unit, or the boot-time
	// one), or with no systemd, it is done here.
	inUnit := os.Getenv("INVOCATION_ID") != "" || os.Getenv("TRELLIS_UPGRADE_INLINE") != ""
	systemdRun := ""
	if !inUnit {
		if _, err := os.Stat("/run/systemd/system"); err == nil {
			systemdRun, _ = exec.LookPath("systemd-run")
		}
	}
	ctx := context.Background()
	sys := upgrade.HostSystem{StatusFn: client.Status}

	// Done here: everything, refusals included, goes to the log, which is
	// what the shell following it shows. Its last line says how it ended.
	out := stdout
	var logf *os.File
	if systemdRun == "" {
		if err := os.MkdirAll(*dir, 0o700); err != nil {
			return err
		}
		var err error
		if logf, err = os.OpenFile(filepath.Join(*dir, "log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600); err != nil {
			return err
		}
		defer logf.Close()
		out = io.MultiWriter(stdout, logf)
	}
	end := func(err error) error {
		if logf != nil {
			if err != nil {
				fmt.Fprintln(logf, logError+err.Error())
			} else {
				fmt.Fprintln(logf, logDone)
			}
		}
		return err
	}

	keys, err := release.TrustedKeys(release.KeyDir)
	if err != nil {
		return end(err)
	}
	opts := upgrade.Options{
		Dir: *dir, Keys: keys, AllowUntrusted: *untrusted, AllowDowngrade: *downgrade, HealthTimeout: *timeout,
		RestoreConfig: *restoreConfig, Log: out,
	}
	opts.Self, _ = os.Executable()

	var pkg []byte
	if !*resume && !*rollback {
		if pkg, err = os.ReadFile(fs.Arg(0)); err != nil {
			return end(err)
		}
		c, err := upgrade.Check(ctx, pkg, opts, sys)
		switch {
		case errors.Is(err, upgrade.ErrNotTrusted):
			return end(fmt.Errorf("%w (--allow-untrusted overrides)", err))
		case errors.Is(err, upgrade.ErrOlder):
			return end(fmt.Errorf("%w (--allow-downgrade overrides)", err))
		case err != nil:
			return end(err)
		}
		if !inUnit {
			by := "NOT vouched for by a trusted key"
			if c.SignedBy != "" {
				by = "signed by " + c.SignedBy
			}
			fmt.Fprintf(stdout, "%s -> %s (%s), %s\n", c.Current.Version, c.Version, c.Info.Get("Architecture"), by)
		}
		if *check {
			return end(nil)
		}
	}
	if systemdRun != "" {
		return upgradeDetached(ctx, sys, *dir, pkg, args, fs.Arg(0), stdout)
	}

	var res *upgrade.Result
	switch {
	case *resume:
		if res, err = upgrade.Resume(ctx, opts, sys); err == nil && res == nil {
			fmt.Fprintln(out, "no upgrade is under way")
			return end(nil)
		}
	case *rollback:
		res, err = upgrade.Rollback(ctx, opts, sys)
	default:
		res, err = upgrade.Run(ctx, pkg, opts, sys)
	}
	// The copy upgradeDetached made for this run has served.
	_ = os.Remove(filepath.Join(*dir, upgrade.Incoming))
	if err != nil {
		return end(err)
	}
	err = resultError(res)
	end(err)
	// As a unit, going back to a healthy node is the job done; the shell
	// following the log reports the outcome from what was recorded.
	if inUnit && res.Result == upgrade.ResultRolledBack && res.HealthyAfter {
		return nil
	}
	return err
}

// The last line of a run's log: logDone, or logError and why.
const (
	logDone  = "== done"
	logError = "error: "
)

// execKeptBinary replaces this process with the copy of the old version's
// binary that an unfinished upgrade kept, unless this is that binary.
func execKeptBinary(dir string, args []string) error {
	if p, err := upgrade.ReadPending(dir); err != nil || p == nil {
		return err
	}
	kept := filepath.Join(dir, upgrade.ResumeBinary)
	ki, err := os.Stat(kept)
	if err != nil {
		return nil // an upgrade from before the copy was kept: nothing better to run
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if si, err := os.Stat(self); err == nil && os.SameFile(si, ki) {
		return nil
	}
	argv := append([]string{kept, "meshctl", "upgrade"}, args...)
	return syscall.Exec(kept, argv, os.Environ())
}

// resultError turns an upgrade's outcome into the command's.
func resultError(res *upgrade.Result) error {
	switch res.Result {
	case upgrade.ResultUpgraded, upgrade.ResultUnchanged:
		return nil
	case upgrade.ResultRolledBack:
		if res.Reason == upgrade.ReasonAsked {
			return nil
		}
		return fmt.Errorf("the node went back to %s: %s", res.From, res.Reason)
	}
	return fmt.Errorf("Upgrade failed, rollback failed: %s. After fixing the cause: %s", res.Reason, upgrade.ResumeHint)
}

// upgradeDetached runs the upgrade in a transient systemd unit and prints
// its log until it ends. Losing this session does not stop the upgrade.
func upgradeDetached(ctx context.Context, sys upgrade.HostSystem, dir string, pkg []byte, args []string, file string, stdout io.Writer) error {
	logPath := filepath.Join(dir, "log")
	before, _ := upgrade.ReadResult(dir)
	// This binary, not the installed one: when resuming, this is the kept
	// copy of the old version (execKeptBinary).
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := upgrade.StartDetached(ctx, sys, dir, exe, pkg, args, file); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Running as %s.service; safe to lose this session (log: %s)\n", upgradeUnit, logPath)
	var shown int64
	follow := func() {
		f, err := os.Open(logPath)
		if err != nil {
			return
		}
		defer f.Close()
		if _, err := f.Seek(shown, io.SeekStart); err != nil {
			return
		}
		n, _ := io.Copy(stdout, f)
		shown += n
	}
	for {
		follow()
		if !upgrade.UnitActive(ctx, sys) {
			break
		}
		time.Sleep(time.Second)
	}
	follow()
	res, err := upgrade.ReadResult(dir)
	if err != nil {
		return err
	}
	last := ""
	if b, err := os.ReadFile(logPath); err == nil {
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		last = lines[len(lines)-1]
	}
	switch {
	case res != nil && (before == nil || !res.Finished.Equal(before.Finished)):
		return resultError(res)
	case strings.HasPrefix(last, logError):
		return errors.New("nothing was changed (see above)")
	case last == logDone:
		return nil
	}
	// The unit ended without saying how: killed, or a crash.
	if p, _ := upgrade.ReadPending(dir); p != nil {
		return fmt.Errorf("Upgrade incomplete (journalctl -u %s). Resume from the starting version: %s", upgradeUnit, upgrade.ResumeHint)
	}
	return fmt.Errorf("Upgrade ended without a result (journalctl -u %s). State: sudo meshctl upgrade --status", upgradeUnit)
}

func upgradeStatus(dir string, stdout io.Writer) error {
	p, err := upgrade.ReadPending(dir)
	if errors.Is(err, os.ErrPermission) {
		return errors.New("Upgrade records require root: sudo meshctl upgrade --status")
	}
	if err != nil {
		return err
	}
	if p != nil {
		fmt.Fprintf(stdout, "under way: %s -> %s, %s since %s\n", p.From, p.To, p.Phase, p.Started.Local().Format("2006-01-02 15:04:05"))
		if p.Reason != "" {
			fmt.Fprintf(stdout, "  going back because: %s\n", p.Reason)
		}
	} else {
		fmt.Fprintln(stdout, "no upgrade is under way")
	}
	last, err := upgrade.ReadResult(dir)
	if err != nil {
		return err
	}
	if last == nil {
		fmt.Fprintln(stdout, "No upgrades recorded")
		return nil
	}
	fmt.Fprintf(stdout, "last: %s -> %s, %s on %s\n", last.From, last.To, last.Result, last.Finished.Local().Format("2006-01-02 15:04:05"))
	if last.Reason != "" {
		fmt.Fprintf(stdout, "  %s\n", last.Reason)
	}
	if upgrade.CanRollBack(dir, last, upgrade.QueryPackage(context.Background(), upgrade.HostSystem{}).Version) {
		fmt.Fprintf(stdout, "  %s is kept: meshctl upgrade --rollback goes back to it\n", last.From)
	}
	return nil
}
