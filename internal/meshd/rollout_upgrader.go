package meshd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"meridian/trellis/internal/deb"
	"meridian/trellis/internal/release"
	"meridian/trellis/internal/rollout"
	"meridian/trellis/internal/upgrade"
)

// rolloutUpgrader is what a mesh-wide upgrade does to this node: the real
// one installs the package as meshctl upgrade does (internal/upgrade); the
// lab's stands in for it (meshd --test-upgrader DIR), since a lab node is
// not a package install.
type rolloutUpgrader interface {
	// SelfCheck: this node's Debian architecture, and whether it takes
	// mesh-wide upgrades (why not).
	SelfCheck(ctx context.Context) (arch string, accepts bool, why string)
	// Vouch reads a package the operator gave this node to send through
	// the mesh: its version, architecture and the key that signed it.
	Vouch(ctx context.Context, pkg []byte) (version, arch, signedBy string, err error)
	// Check decides whether this node installs pkg: its version, or why
	// not (the node then skips the rollout).
	Check(ctx context.Context, pkg []byte) (version string, err error)
	// Start begins installing the package at path, detached: the real
	// upgrade restarts meshd.
	Start(ctx context.Context, path string) error
	// Outcome: an upgrade runs now, or how the last one that finished
	// after since ended (nil: none).
	Outcome(ctx context.Context, since time.Time) (running bool, res *upgrade.Result)
}

func (d *Daemon) rolloutUpgrader() rolloutUpgrader {
	d.rollouts.upOnce.Do(func() {
		if dir := d.opts.TestUpgraderDir; dir != "" {
			l := &labUpgrader{dir: dir, version: d.opts.Version}
			d.rollouts.up = l
			// The lab's stall time, shorter than a real one (tests only).
			if n, err := time.ParseDuration(l.read("stall", "") + "s"); err == nil && n > 0 {
				rollout.Stalled = n
			}
		} else {
			d.rollouts.up = &hostUpgrader{d: d}
		}
	})
	return d.rollouts.up
}

// hostUpgrader is the real node.
type hostUpgrader struct{ d *Daemon }

func (h *hostUpgrader) SelfCheck(ctx context.Context) (string, bool, string) {
	return h.d.rolloutSelfCheck(ctx)
}

func (h *hostUpgrader) Vouch(ctx context.Context, pkg []byte) (string, string, string, error) {
	keys, err := release.TrustedKeys(release.KeyDir)
	if err != nil {
		return "", "", "", err
	}
	if len(keys) == 0 {
		return "", "", "", fmt.Errorf("this node trusts no release key, so it cannot vouch for a package; on the node, put the release's public key in %s/NAME.pub", release.KeyDir)
	}
	return vouch(pkg, keys)
}

// vouch reads a package and checks its signature against keys.
func vouch(pkg []byte, keys []deb.PublicKey) (version, arch, signedBy string, err error) {
	info, err := deb.Read(pkg)
	if err != nil {
		return "", "", "", err
	}
	if info.Get("Package") != upgrade.Package {
		return "", "", "", fmt.Errorf("this is the %s package, not %s", info.Get("Package"), upgrade.Package)
	}
	key, err := info.Verify(keys)
	if err != nil {
		return "", "", "", fmt.Errorf("%v. Only a package signed with a release key this node trusts is sent through the mesh", err)
	}
	return info.Get("Version"), info.Get("Architecture"), strings.TrimSpace(key.ID() + " " + key.Comment), nil
}

func (h *hostUpgrader) Check(ctx context.Context, pkg []byte) (string, error) {
	d := h.d
	if err := d.packagedLayout(); err != nil {
		return "", err
	}
	sys := d.upgradeSys()
	keys, err := release.TrustedKeys(release.KeyDir)
	if err != nil {
		return "", err
	}
	c, err := upgrade.Check(ctx, pkg, upgrade.Options{Dir: d.upgradeDir(), Keys: keys}, sys)
	if err != nil {
		return "", err
	}
	return c.Version, nil
}

func (h *hostUpgrader) Start(ctx context.Context, path string) error {
	d := h.d
	if err := d.upgradeBusy(ctx); err != nil {
		return err
	}
	pkg, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return upgrade.StartDetached(ctx, d.upgradeSys(), d.upgradeDir(), exe, pkg, []string{"--dir", d.upgradeDir()}, "")
}

func (h *hostUpgrader) Outcome(ctx context.Context, since time.Time) (bool, *upgrade.Result) {
	d := h.d
	if upgrade.UnitActive(ctx, d.upgradeSys()) {
		return true, nil
	}
	if p, _ := upgrade.ReadPending(d.upgradeDir()); p != nil {
		return true, nil
	}
	r, err := upgrade.ReadResult(d.upgradeDir())
	if err != nil || r == nil || r.Finished.Before(since) {
		return false, nil
	}
	return false, r
}

// labUpgrader stands in for the upgrade in the namespace lab. Its
// directory holds:
//
//	keys/*.pub   the release keys it trusts
//	installed    the version it runs (default: meshd's)
//	outcome      how an upgrade ends: "upgraded" (default), "rolled-back"
//	             or "failed"
//	refuse       when present, the node does not take rollouts (its text
//	             says why)
//	delay        seconds an upgrade takes (default 3)
//	stall        seconds a node's turn may go unmoved (rollout.Stalled;
//	             read at the start)
//	started      written: when the last upgrade began (RFC 3339)
//	last.json    written: how the last upgrade ended
type labUpgrader struct {
	dir, version string
	mu           sync.Mutex
	running      bool
}

func (l *labUpgrader) read(name, def string) string {
	b, err := os.ReadFile(filepath.Join(l.dir, name))
	if err != nil {
		return def
	}
	return strings.TrimSpace(string(b))
}

func (l *labUpgrader) SelfCheck(ctx context.Context) (string, bool, string) {
	if why, err := os.ReadFile(filepath.Join(l.dir, "refuse")); err == nil {
		return debArch(), false, strings.TrimSpace(string(why))
	}
	return debArch(), true, ""
}

func (l *labUpgrader) keys() ([]deb.PublicKey, error) {
	return release.TrustedKeys(filepath.Join(l.dir, "keys"))
}

func (l *labUpgrader) Vouch(ctx context.Context, pkg []byte) (string, string, string, error) {
	keys, err := l.keys()
	if err != nil {
		return "", "", "", err
	}
	return vouch(pkg, keys)
}

func (l *labUpgrader) Check(ctx context.Context, pkg []byte) (string, error) {
	keys, err := l.keys()
	if err != nil {
		return "", err
	}
	v, _, _, err := vouch(pkg, keys)
	if err != nil {
		return "", err
	}
	if v == l.read("installed", l.version) {
		return "", fmt.Errorf("version %s is already installed", v)
	}
	return v, nil
}

func (l *labUpgrader) Start(ctx context.Context, path string) error {
	pkg, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	info, err := deb.Read(pkg)
	if err != nil {
		return err
	}
	l.mu.Lock()
	if l.running {
		l.mu.Unlock()
		return errors.New("an upgrade is running already")
	}
	l.running = true
	l.mu.Unlock()
	from, to := l.read("installed", l.version), info.Get("Version")
	_ = os.WriteFile(filepath.Join(l.dir, "started"), []byte(time.Now().Format(time.RFC3339Nano)+"\n"), 0o644)
	delay := 3 * time.Second
	if s := l.read("delay", ""); s != "" {
		if n, err := time.ParseDuration(s + "s"); err == nil {
			delay = n
		}
	}
	go func() {
		time.Sleep(delay)
		res := &upgrade.Result{Result: l.read("outcome", upgrade.ResultUpgraded), From: from, To: to, Finished: time.Now(), HealthyAfter: true}
		switch res.Result {
		case upgrade.ResultUpgraded:
			_ = os.WriteFile(filepath.Join(l.dir, "installed"), []byte(to+"\n"), 0o644)
		case upgrade.ResultRolledBack:
			res.Reason = "the lab says so"
		default:
			res.HealthyAfter, res.Reason = false, "the lab says so"
		}
		b, _ := json.Marshal(res)
		_ = os.WriteFile(filepath.Join(l.dir, "last.json"), b, 0o644)
		l.mu.Lock()
		l.running = false
		l.mu.Unlock()
	}()
	return nil
}

func (l *labUpgrader) Outcome(ctx context.Context, since time.Time) (bool, *upgrade.Result) {
	l.mu.Lock()
	running := l.running
	l.mu.Unlock()
	if running {
		return true, nil
	}
	b, err := os.ReadFile(filepath.Join(l.dir, "last.json"))
	if err != nil {
		return false, nil
	}
	var r upgrade.Result
	if json.Unmarshal(b, &r) != nil || r.Finished.Before(since) {
		return false, nil
	}
	return false, &r
}
