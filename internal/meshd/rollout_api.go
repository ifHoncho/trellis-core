package meshd

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"meridian/trellis/internal/api"
	"meridian/trellis/internal/rollout"
)

// The origin's side of a mesh-wide upgrade, through the API: the operator
// gives this node one package per architecture, sees the plan (the order,
// and who is left out and why), then starts it, and may cancel it.

// RolloutPackage keeps a package on this node for a mesh-wide upgrade
// (POST /upgrade/mesh/package), once this node vouches for it.
func (d *Daemon) RolloutPackage(ctx context.Context, pkg []byte) (*api.RolloutPackage, error) {
	v, arch, by, err := d.rolloutUpgrader().Vouch(ctx, pkg)
	if err != nil {
		return nil, api.Invalid("%v", err)
	}
	p, err := rollout.Keep(d.releasesDir(), arch, pkg)
	if err != nil {
		return nil, fmt.Errorf("cannot keep the package: %w", err)
	}
	// Touched, so that pruning counts its time from this upload.
	now := time.Now()
	_ = os.Chtimes(rollout.File(d.releasesDir(), p.SHA256), now, now)
	return &api.RolloutPackage{SHA256: p.SHA256, Arch: arch, Version: v, Size: p.Size, SignedBy: by}, nil
}

// Rollout plans a mesh-wide upgrade with packages kept on this node and,
// unless it is a dry run, starts it (POST /upgrade/mesh).
func (d *Daemon) Rollout(ctx context.Context, in api.RolloutRequest) (*api.RolloutPlan, error) {
	if d.setupState().Needed {
		return nil, api.Invalid("this node has no mesh yet: install a profile on it first")
	}
	me := d.identity()
	if me == nil || d.gossip == nil || d.gossip.currentKey() == nil {
		return nil, api.Errorf(http.StatusConflict, "Gossip not started; retry shortly")
	}
	if len(in.Packages) == 0 {
		return nil, api.Invalid("name the packages, one per architecture (POST /upgrade/mesh/package keeps them first)")
	}
	s := &d.rollouts
	s.mu.Lock()
	if c := s.cur; c != nil && !c.Dropped && c.Ended.IsZero() && !c.Rollout.Cancelled {
		origin := d.rolloutName(c.Rollout.Origin)
		s.mu.Unlock()
		return nil, api.Errorf(http.StatusConflict, "a mesh-wide upgrade to %s is under way (started %s by %s): cancel it first",
			c.Rollout.Version, c.Rollout.Started.Local().Format("15:04"), origin)
	}
	if c := s.cur; c != nil && !c.Dropped && c.Phase == rollout.Upgrading {
		s.mu.Unlock()
		return nil, api.Errorf(http.StatusConflict, "this node is upgrading now")
	}
	s.mu.Unlock()

	plan := &api.RolloutPlan{}
	up := d.rolloutUpgrader()
	var pkgs []rollout.Package
	for _, sha := range in.Packages {
		sha = strings.ToLower(sha)
		pkg, err := os.ReadFile(rollout.File(d.releasesDir(), sha))
		if err != nil || hexSHA256(pkg) != sha {
			return nil, api.Errorf(http.StatusConflict, "package %.12s is not kept on this node (any more): send it again", sha)
		}
		v, arch, by, err := up.Vouch(ctx, pkg)
		if err != nil {
			return nil, api.Invalid("package %.12s: %v", sha, err)
		}
		switch {
		case plan.Version == "":
			plan.Version = v
		case v != plan.Version:
			return nil, api.Invalid("the packages are of different versions (%s and %s): one release at a time", plan.Version, v)
		}
		if slices.ContainsFunc(pkgs, func(p rollout.Package) bool { return p.Arch == arch }) {
			return nil, api.Invalid("two packages for %s", arch)
		}
		pkgs = append(pkgs, rollout.Package{Arch: arch, SHA256: sha, Size: int64(len(pkg))})
		plan.Packages = append(plan.Packages, api.RolloutPackage{SHA256: sha, Arch: arch, Version: v, Size: int64(len(pkg)), SignedBy: by})
	}

	// Who takes it, and how far each is from here.
	hops := d.rolloutHops(ctx)
	mac := strings.ToLower(me.MAC)
	cur, _ := d.conf()
	var nodes []rollout.Node
	var skipped []api.RolloutPlanNode
	arch, accepts, why := up.SelfCheck(ctx)
	self := api.RolloutPlanNode{Callsign: cur.Callsign, NodeID: me.NodeID, Self: true, Arch: arch, Version: d.opts.Version}
	switch {
	case !accepts:
		self.Skip = why
	case !slices.ContainsFunc(pkgs, func(p rollout.Package) bool { return p.Arch == arch }):
		self.Skip = "no package for " + arch
	default:
		for _, p := range pkgs {
			if p.Arch != arch {
				continue
			}
			if pkg, err := os.ReadFile(rollout.File(d.releasesDir(), p.SHA256)); err == nil {
				if _, err := up.Check(ctx, pkg); err != nil {
					self.Skip = err.Error()
				}
			}
		}
	}
	planned := map[string]api.RolloutPlanNode{}
	if self.Skip == "" {
		nodes = append(nodes, rollout.Node{MAC: mac, ID: me.NodeID, Hops: 0})
		planned[mac] = self
	} else {
		skipped = append(skipped, self)
	}
	for _, p := range d.peers.Peers() {
		r := p.Record
		m := strings.ToLower(r.MAC)
		n := api.RolloutPlanNode{Callsign: r.Callsign, NodeID: r.ID, Arch: r.Arch, Version: r.Version, Hops: -1}
		if h, ok := hops[m]; ok {
			n.Hops = h
		}
		switch {
		case r.Arch == "":
			n.Skip = "runs a release that does not take mesh-wide upgrades: upgrade it with trellis-deploy or meshctl upgrade"
		case !r.Rollouts:
			n.Skip = "does not take mesh-wide upgrades (not installed from the package); meshctl upgrade on the node shows why"
		case !slices.ContainsFunc(pkgs, func(p rollout.Package) bool { return p.Arch == r.Arch }):
			n.Skip = "no package for " + r.Arch
		case net.ParseIP(r.Addr) == nil:
			n.Skip = "its address is not known yet"
		}
		if n.Skip != "" {
			skipped = append(skipped, n)
			continue
		}
		h := n.Hops
		if h < 0 {
			h = 1 << 10 // not in the topology: as far as can be, first
		}
		nodes = append(nodes, rollout.Node{MAC: m, ID: r.ID, Hops: max(h, 1)})
		planned[m] = n
	}
	order := rollout.Order(nodes)
	for i, m := range order {
		n := planned[m]
		n.Position = i + 1
		plan.Nodes = append(plan.Nodes, n)
	}
	slices.SortFunc(skipped, func(a, b api.RolloutPlanNode) int { return a.NodeID - b.NodeID })
	plan.Nodes = append(plan.Nodes, skipped...)

	// Nodes heard in the last day but not now will miss it (they ask for
	// it when back, but are not in its order).
	live := map[string]bool{}
	for _, p := range d.peers.Peers() {
		live[strings.ToLower(p.Record.MAC)] = true
	}
	var away []string
	for m, callsign := range d.rosterKnown() {
		if !live[strings.ToLower(m)] {
			away = append(away, callsign)
		}
	}
	if len(away) > 0 {
		slices.Sort(away)
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s %s heard in the last day but not now: left out of this upgrade.", listAnd(away), plural(len(away), "was", "were")))
	}
	if len(order) == 0 {
		plan.Warnings = append(plan.Warnings, "No node takes this release.")
	}
	if in.DryRun {
		return plan, nil
	}
	if len(order) == 0 {
		return nil, api.Invalid("no node takes this release")
	}

	addr, _, _ := strings.Cut(d.facts.bridgeAddress(), "/")
	r := rollout.Rollout{ID: rollout.NewID(), Version: plan.Version, Packages: pkgs, Order: order, Origin: mac, OriginAddr: addr, Started: time.Now().UTC().Truncate(time.Second)}
	if err := r.Validate(); err != nil {
		return nil, api.Invalid("%v", err)
	}
	c := &rolloutRecord{Rollout: r, Since: time.Now(), Received: time.Now(), Sender: true}
	s.mu.Lock()
	if err := d.saveRollout(c); err != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("cannot keep the mesh-wide upgrade: %w", err)
	}
	s.cur, s.others, s.wait, s.fetchErr = c, map[string]rollout.State{}, "", ""
	s.sent, s.fastTill = map[string]time.Time{}, time.Now().Add(rolloutResendFast)
	s.mu.Unlock()
	d.log.Info("mesh-wide upgrade started", "id", r.ID, "version", r.Version, "order", len(order))
	d.Kick("mesh-wide upgrade started")
	plan.Started, plan.ID = true, r.ID
	return plan, nil
}

// CancelRollout calls off the mesh-wide upgrade this node started: nodes
// that have not begun upgrading stop; one upgrading finishes.
func (d *Daemon) CancelRollout(ctx context.Context) error {
	s := &d.rollouts
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.cur
	switch {
	case c == nil || c.Dropped || !c.Ended.IsZero():
		return api.Errorf(http.StatusConflict, "no mesh-wide upgrade is under way")
	case !c.Sender:
		return api.Errorf(http.StatusConflict, "this upgrade was started on %s: cancel it there", d.rolloutName(c.Rollout.Origin))
	case c.Rollout.Cancelled:
		return nil
	}
	c.Rollout.Cancelled = true
	switch c.Phase {
	case "", rollout.Fetching, rollout.Ready:
		d.setPhase(c, rollout.Halted, "the rollout was cancelled")
	default:
		if err := d.saveRollout(c); err != nil {
			return err
		}
	}
	s.sent, s.fastTill = map[string]time.Time{}, time.Now().Add(rolloutResendFast)
	d.log.Info("mesh-wide upgrade cancelled", "id", c.Rollout.ID)
	d.Kick("mesh-wide upgrade cancelled")
	return nil
}

// rolloutHops is each node's distance from this one in the mesh's
// topology, by identity MAC.
func (d *Daemon) rolloutHops(ctx context.Context) map[string]int {
	out := map[string]int{}
	t, err := d.Topology(ctx)
	if err != nil {
		return out
	}
	adj := map[string][]string{}
	var self string
	for _, n := range t.Nodes {
		if n.Self {
			self = n.ID
		}
	}
	for _, l := range t.Links {
		adj[l.A] = append(adj[l.A], l.B)
		adj[l.B] = append(adj[l.B], l.A)
	}
	if self == "" {
		return out
	}
	out[self] = 0
	queue := []string{self}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		for _, m := range adj[n] {
			if _, seen := out[m]; !seen {
				out[m] = out[n] + 1
				queue = append(queue, m)
			}
		}
	}
	return out
}
