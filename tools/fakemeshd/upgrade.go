package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"meridian/trellis/internal/api"
	"meridian/trellis/internal/deb"
)

// A made-up upgrade, for working on the UI's Software panel. A real
// package file is read for its version (tools/mkdeb builds one); the
// "upgrade" then plays out over some 15 s as it does on a node. A version
// containing "bad" fails its health check and goes back.

type fakeUpgrade struct {
	from, to  string
	signedBy  string
	rollback  bool // going back by hand
	bad       bool
	started   time.Time
	installed string // the package version installed now
	last      *api.UpgradeOutcome
	running   bool
	logPrefix []string
}

func (f *fake) upgradeInit() {
	if f.upg == nil {
		f.upg = &fakeUpgrade{installed: "0.4.0"}
	}
}

// fakeUpgradeSteps is the log as it grows: seconds after the start, line.
func (u *fakeUpgrade) steps() [][2]any {
	if u.rollback {
		return [][2]any{
			{0, "going back to " + u.from + ", as asked"}, {1, "the configuration and state stay as they are"},
			{6, "  waiting: no mesh peer has come back (4 before)"}, {11, "back on " + u.from + " and healthy"}, {11, "== done"},
		}
	}
	s := [][2]any{
		{0, "upgrade " + u.from + " -> " + u.to}, {0, "signature good: key " + u.signedBy},
		{1, "before: meshd " + u.from + ", 4 mesh peer(s)"}, {2, "kept: the " + u.from + " package, configuration, mesh profile and identity"},
		{3, "installing " + u.to}, {6, "checking the node's health (up to 3m0s)"}, {7, "  waiting: no mesh peer has come back (4 before)"},
	}
	if u.bad {
		return append(s, [][2]any{{9, "  waiting: meshd is not running"}, {14, "NOT healthy: meshd is not running"},
			{14, "going back to " + u.from}, {18, "back on " + u.from + " and healthy"}, {18, "error: the node went back to " + u.from + ": meshd is not running"}}...)
	}
	return append(s, [][2]any{{13, "healthy on " + u.to}, {13, "== done"}}...)
}

func (f *fake) UpgradeState(ctx context.Context) (*api.UpgradeState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upgradeInit()
	u := f.upg
	st := &api.UpgradeState{Version: "demo", Package: u.installed, Arch: "arm64", TrustedKeys: 1, Last: u.last}
	if u.running {
		el := time.Since(u.started).Seconds()
		var log []string
		end := 0.0
		for _, s := range u.steps() {
			at := float64(s[0].(int))
			end = at
			if el >= at {
				log = append(log, s[1].(string))
			}
		}
		st.Log = log
		if el < end {
			st.Running = true
			phase := "installing"
			if el >= 6 {
				phase = "checking"
			}
			if u.bad && el >= 14 || u.rollback {
				phase = "rolling-back"
			}
			st.Pending = &api.UpgradePending{From: u.from, To: u.to, Phase: phase, Started: u.started}
		} else {
			u.running = false
			res := &api.UpgradeOutcome{From: u.from, To: u.to, Finished: time.Now(), HealthyAfter: true}
			switch {
			case u.rollback:
				// As the real node records it: the upgrade it undid.
				res.Result, res.Reason = "rolled-back", "asked for by the operator"
				u.installed = u.from
			case u.bad:
				res.Result, res.Reason = "rolled-back", "meshd is not running"
			default:
				res.Result, res.CanRollBack = "upgraded", true
				u.installed = u.to
			}
			u.last, st.Last, st.Package = res, res, u.installed
			u.logPrefix = log
		}
	} else {
		st.Log = u.logPrefix
	}
	return st, nil
}

func (f *fake) Upgrade(ctx context.Context, in api.UpgradeRequest) (*api.UpgradeCheck, error) {
	pkg, dryRun := in.Package, in.DryRun
	if in.SHA256 != "" {
		f.mu.Lock()
		kept := f.kept
		f.mu.Unlock()
		if sum := sha256.Sum256(kept); hex.EncodeToString(sum[:]) != in.SHA256 {
			return nil, api.Errorf(http.StatusConflict, "the package checked before is no longer kept on this node: choose the file again")
		}
		pkg = kept
	}
	info, err := deb.Read(pkg)
	if err != nil {
		return nil, api.Invalid("%v", err)
	}
	if info.Get("Package") != "trellis" {
		return nil, api.Invalid("this is the %s package, not trellis", info.Get("Package"))
	}
	if !info.Signed() {
		return nil, api.Invalid("the package is not signed. Only a package signed with a release key this node trusts is installed")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upgradeInit()
	u := f.upg
	if u.running {
		return nil, api.Errorf(http.StatusConflict, "an upgrade is running on this node now")
	}
	to := info.Get("Version")
	if to == u.installed {
		return nil, api.Invalid("version %s is already installed", to)
	}
	sum := sha256.Sum256(pkg)
	c := &api.UpgradeCheck{From: u.installed, To: to, Arch: info.Get("Architecture"), SignedBy: "a0b1c2d3e4f50617 Trellis release", SHA256: hex.EncodeToString(sum[:])}
	f.kept = pkg
	if dryRun {
		return c, nil
	}
	*u = fakeUpgrade{from: u.installed, to: to, signedBy: c.SignedBy, bad: strings.Contains(to, "bad"),
		started: time.Now(), installed: u.installed, last: u.last, running: true}
	c.Started = true
	return c, nil
}

func (f *fake) UpgradeRollback(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upgradeInit()
	u := f.upg
	if u.running || u.last == nil || !u.last.CanRollBack {
		return api.Errorf(http.StatusConflict, "there is no upgrade to go back from (or the version before it is no longer kept)")
	}
	*u = fakeUpgrade{from: u.last.From, to: u.last.To, rollback: true, started: time.Now(), installed: u.installed, last: u.last, running: true}
	return nil
}
