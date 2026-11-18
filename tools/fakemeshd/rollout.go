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
	"meridian/trellis/internal/status"
)

// A made-up mesh-wide upgrade, for working on the UI. The packages are
// read for their version and architecture; the rollout then plays out
// over some 40 s: every node fetches at once, then one at a time upgrades.
// A version containing "bad" rolls back on the second node and halts the
// rest. scout-7 runs a release too old to take part; bravo-5 is armhf, so
// it is skipped unless an armhf package is given.

type fakeRollout struct {
	plan      *api.RolloutPlan
	started   time.Time
	cancelled time.Time
}

func (f *fake) rolloutKept() map[string]api.RolloutPackage {
	if f.rolloutPkgs == nil {
		f.rolloutPkgs = map[string]api.RolloutPackage{}
	}
	return f.rolloutPkgs
}

func (f *fake) RolloutPackage(ctx context.Context, pkg []byte) (*api.RolloutPackage, error) {
	info, err := deb.Read(pkg)
	if err != nil {
		return nil, api.Invalid("%v", err)
	}
	if info.Get("Package") != "trellis" {
		return nil, api.Invalid("this is the %s package, not trellis", info.Get("Package"))
	}
	if !info.Signed() {
		return nil, api.Invalid("the package is not signed. Only a package signed with a release key this node trusts is sent through the mesh")
	}
	sum := sha256.Sum256(pkg)
	p := api.RolloutPackage{SHA256: hex.EncodeToString(sum[:]), Arch: info.Get("Architecture"), Version: info.Get("Version"), Size: int64(len(pkg)), SignedBy: "1a2b3c4d demo release key"}
	f.mu.Lock()
	f.rolloutKept()[p.SHA256] = p
	f.mu.Unlock()
	return &p, nil
}

// fakeArch is each demo node's architecture; "" for one too old to say.
func fakeArch(callsign string) string {
	switch callsign {
	case "scout-7":
		return ""
	case "bravo-5":
		return "armhf"
	}
	return "arm64"
}

func (f *fake) Rollout(ctx context.Context, in api.RolloutRequest) (*api.RolloutPlan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r := f.rollout; r != nil && r.cancelled.IsZero() && !f.rolloutEnded(time.Now()) {
		return nil, api.Errorf(http.StatusConflict, "a mesh-wide upgrade to %s is under way (started %s by %s): cancel it first", r.plan.Version, r.started.Format("15:04"), f.node.Callsign)
	}
	if len(in.Packages) == 0 {
		return nil, api.Invalid("name the packages, one per architecture (POST /upgrade/mesh/package keeps them first)")
	}
	plan := &api.RolloutPlan{}
	archs := map[string]bool{}
	for _, sha := range in.Packages {
		p, ok := f.rolloutKept()[strings.ToLower(sha)]
		if !ok {
			return nil, api.Errorf(http.StatusConflict, "package %.12s is not kept on this node (any more): send it again", sha)
		}
		if plan.Version != "" && p.Version != plan.Version {
			return nil, api.Invalid("the packages are of different versions (%s and %s): one release at a time", plan.Version, p.Version)
		}
		if archs[p.Arch] {
			return nil, api.Invalid("two packages for %s", p.Arch)
		}
		plan.Version, archs[p.Arch] = p.Version, true
		plan.Packages = append(plan.Packages, p)
	}
	hops := map[string]int{"relay-1": 0, "ridge-2": 1, "truck-3": 1, "alpha-4": 2, "bravo-5": 2, "ops-6": 3, "scout-7": 1}
	var order, skipped []api.RolloutPlanNode
	for _, n := range nodes {
		a := fakeArch(n.callsign)
		pn := api.RolloutPlanNode{Callsign: n.callsign, NodeID: n.id, Self: n.callsign == "relay-1", Arch: a, Version: "demo", Hops: hops[n.callsign]}
		switch {
		case a == "":
			pn.Version = "0.2.0"
			pn.Skip = "runs a release that does not take mesh-wide upgrades: upgrade it with trellis-deploy or meshctl upgrade"
		case !archs[a]:
			pn.Skip = "no package for " + a
		}
		if pn.Skip != "" {
			skipped = append(skipped, pn)
		} else {
			order = append(order, pn)
		}
	}
	// Farthest first, then by node ID, this node last.
	for i := range order {
		for j := i + 1; j < len(order); j++ {
			a, b := order[i], order[j]
			if a.Self || (!b.Self && (b.Hops > a.Hops || (b.Hops == a.Hops && b.NodeID < a.NodeID))) {
				order[i], order[j] = b, a
			}
		}
	}
	for i := range order {
		order[i].Position = i + 1
	}
	plan.Nodes = append(order, skipped...)
	plan.Warnings = []string{"scout-7 was heard in the last day but not now: left out of this upgrade."}
	if in.DryRun {
		return plan, nil
	}
	plan.Started, plan.ID = true, "5e1f00c0ffee"
	f.rollout = &fakeRollout{plan: plan, started: time.Now()}
	return plan, nil
}

func (f *fake) CancelRollout(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rollout == nil || f.rolloutEnded(time.Now()) {
		return api.Errorf(http.StatusConflict, "no mesh-wide upgrade is under way")
	}
	if f.rollout.cancelled.IsZero() {
		f.rollout.cancelled = time.Now()
	}
	return nil
}

// rolloutStatus plays the rollout out: fetching for 3 s, then 6 s per
// node in the order.
func (f *fake) rolloutStatus(now time.Time) *status.Rollout {
	r := f.rollout
	if r == nil {
		return nil
	}
	p := r.plan
	st := &status.Rollout{ID: p.ID, Version: p.Version, Origin: f.node.Callsign, Sender: true, Started: r.started, Cancelled: !r.cancelled.IsZero()}
	for _, k := range p.Packages {
		st.Packages = append(st.Packages, status.RolloutPackage{Arch: k.Arch, SHA256: k.SHA256, Size: k.Size})
	}
	bad := strings.Contains(p.Version, "bad")
	halted, haltWhy := false, ""
	var ended time.Time
	for _, pn := range p.Nodes {
		if pn.Position == 0 {
			continue
		}
		n := status.RolloutNode{Position: pn.Position, Callsign: pn.Callsign, NodeID: pn.NodeID, Self: pn.Self, Arch: pn.Arch, Version: pn.Version, MAC: "6a:dc:ee:10:00:00"}
		start := r.started.Add(3*time.Second + time.Duration(pn.Position-1)*6*time.Second)
		cancelled := !r.cancelled.IsZero() && r.cancelled.Before(start)
		at := now
		if cancelled {
			at = r.cancelled
		}
		switch {
		case halted:
			n.Phase, n.Reason, n.Since = "halted", haltWhy, start.Add(-6*time.Second)
		case cancelled:
			n.Phase, n.Reason, n.Since = "halted", "the rollout was cancelled", r.cancelled
		case at.Before(r.started.Add(time.Second + time.Duration(pn.Position)*300*time.Millisecond)):
			n.Phase, n.Since = "fetching", r.started
		case at.Before(start):
			n.Phase, n.Since = "ready", r.started.Add(3*time.Second)
		case at.Before(start.Add(6 * time.Second)):
			n.Phase, n.Since = "upgrading", start
		case bad && pn.Position == 2:
			n.Phase, n.Reason, n.Since = "rolled-back", "meshd is not running", start.Add(6*time.Second)
			halted, haltWhy = true, pn.Callsign+" rolled-back: meshd is not running"
		default:
			n.Phase, n.Since = "done", start.Add(6*time.Second)
			n.Version = p.Version
		}
		if n.Since.After(ended) {
			ended = n.Since
		}
		if n.Self {
			st.Phase, st.Reason, st.To = n.Phase, n.Reason, p.Version
			if n.Phase == "ready" {
				st.Waiting = "ops-6's turn (upgrading)"
			}
		}
		st.Nodes = append(st.Nodes, n)
	}
	final := true
	for _, n := range st.Nodes {
		switch n.Phase {
		case "fetching", "ready", "upgrading":
			final = false
		}
	}
	if final {
		e := ended
		st.Ended = &e
	}
	return st
}

func (f *fake) rolloutEnded(now time.Time) bool {
	st := f.rolloutStatus(now)
	return st == nil || st.Ended != nil
}
