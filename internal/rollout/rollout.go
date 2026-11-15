// Package rollout decides a rolling mesh-wide upgrade: the order the
// nodes take a release in, and, on each node, whether it is its turn.
//
// No node drives the others. The node the operator gave the release to
// (the origin) sends every node the rollout: the version, a package per
// architecture, and the order. Each node then fetches its package, checks
// it, and upgrades when every node before it in the order is done or
// skipped, reading their progress from gossip. The first node that rolls
// back halts everyone after it. So the origin may go away, or be the node
// upgrading, without stalling the rest.
package rollout

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"time"
)

// Phase is where a node is in a rollout.
type Phase string

const (
	// Fetching: getting and checking its package.
	Fetching Phase = "fetching"
	// Ready: package checked, waiting for its turn.
	Ready Phase = "ready"
	// Upgrading: installing, or checking its health after.
	Upgrading Phase = "upgrading"
	// Done: runs the release.
	Done Phase = "done"
	// Skipped: does not take this release (Reason says why); the rollout
	// goes on past it.
	Skipped Phase = "skipped"
	// RolledBack: installed it, was not healthy, went back. Halts the rest.
	RolledBack Phase = "rolled-back"
	// Failed: could not upgrade (Reason). Halts the rest.
	Failed Phase = "failed"
	// Halted: will not upgrade, because a node before it rolled back or
	// failed, or went silent on its turn, or the rollout was cancelled.
	Halted Phase = "halted"
)

// Final reports whether a node in this phase has finished with the rollout.
func (p Phase) Final() bool {
	switch p {
	case Done, Skipped, RolledBack, Failed, Halted:
		return true
	}
	return false
}

// Stalled is how long a node's turn may go without its phase moving before
// the nodes after it halt: one that never got the rollout, one that cannot
// be heard, one stuck. An upgrade with its health check takes some 5 min.
// Not skipped past: a node that cannot be heard may be in the middle of
// installing, and the next one would then upgrade beside it.
const Stalled = 15 * time.Minute

// Package is the release for one architecture.
type Package struct {
	Arch   string `json:"arch"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Rollout is a release on its way through the mesh.
type Rollout struct {
	ID       string    `json:"id"`
	Version  string    `json:"version"`
	Packages []Package `json:"packages"`
	// Order lists the nodes by identity MAC, in the order they upgrade.
	Order []string `json:"order"`
	// Origin is the identity MAC of the node the operator gave the release
	// to; OriginAddr its node address, where the packages are first served.
	Origin     string    `json:"origin"`
	OriginAddr string    `json:"origin_addr"`
	Started    time.Time `json:"started"`
	Cancelled  bool      `json:"cancelled,omitempty"`
}

// NewID returns a random rollout ID.
func NewID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Package returns the package for an architecture, or nil.
func (r *Rollout) Package(arch string) *Package {
	for i := range r.Packages {
		if r.Packages[i].Arch == arch {
			return &r.Packages[i]
		}
	}
	return nil
}

// Validate checks a rollout received from another node.
func (r *Rollout) Validate() error {
	switch {
	case len(r.ID) == 0 || len(r.ID) > 32:
		return fmt.Errorf("rollout: bad id")
	case r.Version == "" || len(r.Version) > 64:
		return fmt.Errorf("rollout: bad version")
	case len(r.Packages) == 0 || len(r.Packages) > 8:
		return fmt.Errorf("rollout: %d packages", len(r.Packages))
	case len(r.Order) == 0 || len(r.Order) > 256:
		return fmt.Errorf("rollout: %d nodes in the order", len(r.Order))
	}
	for _, p := range r.Packages {
		if len(p.SHA256) != 64 || p.Size <= 0 || p.Arch == "" {
			return fmt.Errorf("rollout: bad package for %q", p.Arch)
		}
		if _, err := hex.DecodeString(p.SHA256); err != nil {
			return fmt.Errorf("rollout: bad package hash for %q", p.Arch)
		}
	}
	seen := map[string]bool{}
	for _, m := range r.Order {
		if seen[m] {
			return fmt.Errorf("rollout: %s twice in the order", m)
		}
		seen[m] = true
	}
	return nil
}

// Node is a node as the origin sees it when it orders the rollout.
type Node struct {
	MAC  string
	ID   int
	Hops int // from the origin; 0 for the origin itself
}

// Order puts the nodes in the order they upgrade: farthest from the origin
// first, then by node ID, the origin last. A node's links drop while it
// upgrades; taking the leaves first keeps the paths to everyone else up
// longest, and the origin, last, can be watched throughout.
func Order(nodes []Node) []string {
	n := slices.Clone(nodes)
	slices.SortStableFunc(n, func(a, b Node) int {
		if (a.Hops == 0) != (b.Hops == 0) {
			if a.Hops == 0 {
				return 1
			}
			return -1
		}
		if a.Hops != b.Hops {
			return b.Hops - a.Hops
		}
		return a.ID - b.ID
	})
	out := make([]string, len(n))
	for i, x := range n {
		out[i] = x.MAC
	}
	return out
}

// State is a node's progress as it says in gossip, as one node sees it.
type State struct {
	Phase  Phase // "" when it says nothing about this rollout
	Reason string
	// Since is when this node first saw it in this phase (zero: never
	// heard of it).
	Since time.Time
}

// Action is what a node does next.
type Action int

const (
	Wait Action = iota
	Start
	Halt
)

// Decision is the outcome of Turn.
type Decision struct {
	Action Action
	// Why: whose turn it is while waiting; what halted it.
	Why string
}

// Turn decides, for node me, ready to upgrade, whether to start now, keep
// waiting, or halt. states holds the other nodes' progress by MAC; names
// gives a readable name for a MAC in the reasons.
func Turn(r *Rollout, me string, states map[string]State, names func(string) string, now time.Time) Decision {
	if r.Cancelled {
		return Decision{Halt, "the rollout was cancelled"}
	}
	if !slices.Contains(r.Order, me) {
		return Decision{Halt, "this node is not in the rollout's order"}
	}
	// turn is when the node whose turn it is got it: the rollout's start,
	// or the moment the one before it finished.
	turn := r.Started
	for _, mac := range r.Order {
		if mac == me {
			return Decision{Action: Start}
		}
		s := states[mac]
		switch s.Phase {
		case Done, Skipped:
			if s.Since.After(turn) {
				turn = s.Since
			}
			continue
		case RolledBack, Failed:
			why := fmt.Sprintf("%s %s", names(mac), s.Phase)
			if s.Reason != "" {
				why += ": " + s.Reason
			}
			return Decision{Halt, why}
		case Halted:
			return Decision{Halt, fmt.Sprintf("%s halted: %s", names(mac), s.Reason)}
		}
		// Its turn: wait, unless its phase has not moved for too long.
		since := turn
		if s.Since.After(since) {
			since = s.Since
		}
		if now.Sub(since) > Stalled {
			return Decision{Halt, fmt.Sprintf("%s did not move on its turn for %s (%s)", names(mac), Stalled, phaseOr(s.Phase, "never heard of the rollout"))}
		}
		return Decision{Wait, fmt.Sprintf("%s's turn (%s)", names(mac), phaseOr(s.Phase, "not heard yet"))}
	}
	return Decision{Halt, "this node is not in the rollout's order"}
}

func phaseOr(p Phase, def string) string {
	if p == "" {
		return def
	}
	return string(p)
}
