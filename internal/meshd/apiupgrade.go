package meshd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"meridian/trellis/internal/api"
	"meridian/trellis/internal/config"
	"meridian/trellis/internal/release"
	"meridian/trellis/internal/upgrade"
)

// Upgrades through the API (the web UI's Software panel). meshd checks the
// package and starts the upgrade in its own transient unit, exactly as
// "meshctl upgrade" from a shell does, then answers: the upgrade restarts
// meshd and webd, and goes back by itself if the node is not healthy
// afterwards (internal/upgrade). Through the API a package must be signed
// by a key the node trusts, and may not be older than the one installed;
// overriding either is for root on the node.

func (d *Daemon) upgradeDir() string { return filepath.Join(d.opts.StateDir, "upgrade") }

func (d *Daemon) upgradeSys() upgrade.HostSystem { return upgrade.HostSystem{StatusFn: d.Status} }

// packagedLayout refuses an upgrade or rollback through the API when meshd
// runs with other paths than the package's unit gives it: the upgrade
// replaces the packaged binary, snapshots /etc/trellis and
// /var/lib/trellis, checks health on the default socket, and resumes from
// a boot unit that knows only those.
func (d *Daemon) packagedLayout() error {
	o := d.opts
	if o.NodePath != config.DefaultNodePath || o.StateDir != config.DefaultStateDir ||
		o.Socket != filepath.Join(config.DefaultRunDir, "meshd.sock") ||
		(o.ProfilePath != "" && o.ProfilePath != config.DefaultProfilePath) {
		return api.Errorf(http.StatusConflict, "this meshd does not run with the installed package's paths (node %s, state %s, socket %s): upgrades through the API work only on a node installed from the package", o.NodePath, o.StateDir, o.Socket)
	}
	return nil
}

// UpgradeState is the node's software and its upgrades.
func (d *Daemon) UpgradeState(ctx context.Context) (*api.UpgradeState, error) {
	dir, sys := d.upgradeDir(), d.upgradeSys()
	st := &api.UpgradeState{Version: d.opts.Version}
	if ps := upgrade.QueryPackage(ctx, sys); ps.Installed() {
		st.Package = ps.Version
	}
	if out, err := sys.Run(ctx, "dpkg", "--print-architecture"); err == nil {
		st.Arch = strings.TrimSpace(out)
	}
	if keys, err := release.TrustedKeys(release.KeyDir); err == nil {
		st.TrustedKeys = len(keys)
	}
	st.Running = upgrade.UnitActive(ctx, sys)
	if p, err := upgrade.ReadPending(dir); err == nil && p != nil {
		st.Pending = &api.UpgradePending{From: p.From, To: p.To, Phase: p.Phase, Started: p.Started, Reason: p.Reason}
	}
	if r, err := upgrade.ReadResult(dir); err == nil && r != nil {
		st.Last = &api.UpgradeOutcome{Result: r.Result, From: r.From, To: r.To, Reason: r.Reason, Finished: r.Finished, HealthyAfter: r.HealthyAfter}
		st.Last.CanRollBack = upgrade.CanRollBack(dir, r, st.Package)
	}
	st.Log = upgrade.LogTail(dir, 80)
	return st, nil
}

// busy refuses a second upgrade while one runs or is unfinished.
func (d *Daemon) upgradeBusy(ctx context.Context) error {
	if upgrade.UnitActive(ctx, d.upgradeSys()) {
		return api.Errorf(http.StatusConflict, "an upgrade is running on this node now")
	}
	if p, _ := upgrade.ReadPending(d.upgradeDir()); p != nil {
		return api.Errorf(http.StatusConflict, "an upgrade from %s to %s was left unfinished (%s); on the node: %s", p.From, p.To, p.Phase, upgrade.ResumeHint)
	}
	return nil
}

// Upgrade checks a package and, unless it is a dry run, starts installing
// it. A dry run keeps the package (upgrade.Incoming) so that the request
// that starts it may name it by its hash instead of sending it again.
func (d *Daemon) Upgrade(ctx context.Context, in api.UpgradeRequest) (*api.UpgradeCheck, error) {
	if err := d.packagedLayout(); err != nil {
		return nil, err
	}
	if err := d.upgradeBusy(ctx); err != nil {
		return nil, err
	}
	keys, err := release.TrustedKeys(release.KeyDir)
	if err != nil {
		return nil, api.Errorf(http.StatusInternalServerError, "release keys: %v", err)
	}
	if len(keys) == 0 {
		return nil, api.Errorf(http.StatusConflict, "this node trusts no release key, so it takes no package from here; on the node, put the release's public key in %s/NAME.pub", release.KeyDir)
	}
	dir := d.upgradeDir()
	incoming := filepath.Join(dir, upgrade.Incoming)
	pkg := in.Package
	if in.SHA256 != "" {
		if pkg, err = os.ReadFile(incoming); err != nil || hexSHA256(pkg) != strings.ToLower(in.SHA256) {
			return nil, api.Errorf(http.StatusConflict, "the package checked before is no longer kept on this node: choose the file again")
		}
	}
	sys := d.upgradeSys()
	c, err := upgrade.Check(ctx, pkg, upgrade.Options{Dir: dir, Keys: keys}, sys)
	if err != nil {
		return nil, api.Invalid("%v", err)
	}
	out := &api.UpgradeCheck{From: c.Current.Version, To: c.Version, Arch: c.Info.Get("Architecture"), SignedBy: c.SignedBy, SHA256: hexSHA256(pkg)}
	if in.SHA256 == "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(incoming, pkg, 0o600); err != nil {
			return nil, err
		}
	}
	if in.DryRun {
		return out, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	// The kept file is the package; the unit keeps its state where meshd
	// looks for it.
	if err := upgrade.StartDetached(ctx, sys, dir, exe, nil, []string{"--dir", dir, incoming}, ""); err != nil {
		return nil, api.Errorf(http.StatusConflict, "%v", err)
	}
	d.log.Info("upgrade started through the API", "from", out.From, "to", out.To, "signed_by", out.SignedBy)
	out.Started = true
	return out, nil
}

func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// UpgradeRollback goes back to the version before the last upgrade,
// keeping the configuration as it is now.
func (d *Daemon) UpgradeRollback(ctx context.Context) error {
	if err := d.packagedLayout(); err != nil {
		return err
	}
	if err := d.upgradeBusy(ctx); err != nil {
		return err
	}
	installed := ""
	if ps := upgrade.QueryPackage(ctx, d.upgradeSys()); ps.Installed() {
		installed = ps.Version
	}
	last, _ := upgrade.ReadResult(d.upgradeDir())
	if last == nil || !upgrade.CanRollBack(d.upgradeDir(), last, installed) {
		return api.Errorf(http.StatusConflict, "there is no upgrade to go back from (or the version before it is no longer kept)")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := upgrade.StartDetached(ctx, d.upgradeSys(), d.upgradeDir(), exe, nil, []string{"--dir", d.upgradeDir(), "--rollback"}, ""); err != nil {
		if errors.Is(err, os.ErrPermission) {
			return api.Errorf(http.StatusInternalServerError, "%v", err)
		}
		return api.Errorf(http.StatusConflict, "%v", err)
	}
	d.log.Info("rollback started through the API", "from", last.To, "to", last.From)
	return nil
}
