package meshd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"meridian/trellis/internal/gossip"
	"meridian/trellis/internal/rollout"
	"meridian/trellis/internal/upgrade"
)

// testUpgrader stands in for the upgrade in unit tests.
type testUpgrader struct {
	mu       sync.Mutex
	accepts  bool
	why      string
	checkErr error
	started  []string
	running  bool
	result   *upgrade.Result
}

func (u *testUpgrader) SelfCheck(ctx context.Context) (string, bool, string) {
	return "arm64", u.accepts, u.why
}
func (u *testUpgrader) Vouch(ctx context.Context, pkg []byte) (string, string, string, error) {
	return "0.3.1", "arm64", "test", nil
}
func (u *testUpgrader) Check(ctx context.Context, pkg []byte) (string, error) {
	return "0.3.1", u.checkErr
}
func (u *testUpgrader) Start(ctx context.Context, path string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.started = append(u.started, path)
	u.running = true
	return nil
}
func (u *testUpgrader) Outcome(ctx context.Context, since time.Time) (bool, *upgrade.Result) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.running, u.result
}

const (
	macMe     = "02:00:00:00:00:01"
	macFirst  = "02:00:00:00:00:02"
	macOrigin = "02:00:00:00:00:03"
)

func rolloutDaemon(t *testing.T) (*Daemon, string, []byte, *testUpgrader) {
	t.Helper()
	d, dir, key := meshChangeDaemon(t)
	d.ctx = context.Background()
	up := &testUpgrader{accepts: true}
	d.rollouts.upOnce.Do(func() {})
	d.rollouts.up = up
	return d, dir, key, up
}

// testRollout: macFirst, then this node, then the origin; the package
// for arm64 already kept, so fetching finds it.
func testRollout(t *testing.T, d *Daemon) rollout.Rollout {
	t.Helper()
	p, err := rollout.Keep(d.releasesDir(), "arm64", []byte("a package"))
	if err != nil {
		t.Fatal(err)
	}
	return rollout.Rollout{ID: "0a1b2c3d4e5f", Version: "0.3.1", Packages: []rollout.Package{p},
		Order: []string{macFirst, macMe, macOrigin}, Origin: macOrigin, OriginAddr: "10.73.3.1", Started: time.Now()}
}

func sendRolloutMsg(t *testing.T, d *Daemon, key []byte, r rollout.Rollout) {
	t.Helper()
	raw, _ := json.Marshal(r)
	pkt, err := gossip.SealRollout(key, &gossip.RolloutMsg{V: 1, MAC: macOrigin, Callsign: "origin", Rollout: raw})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.handleRollout(key, pkt); err != nil {
		t.Fatal(err)
	}
}

// peerSays puts a node's rollout progress in the gossip table.
func peerSays(d *Daemon, mac, callsign string, id int, ro *gossip.RolloutState) {
	d.peers.Update(&gossip.Record{V: 1, MAC: mac, ID: id, Callsign: callsign, Addr: "10.73.2.1", Arch: "arm64", Rollouts: true, Rollout: ro, Seq: uint64(time.Now().UnixNano())}, "10.73.2.1")
}

// step runs one pass of this node's part.
func step(d *Daemon) *rolloutRecord {
	s := &d.rollouts
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		return nil
	}
	d.noteOthers(s.cur, time.Now())
	d.rolloutStep(context.Background(), s.cur, macMe, time.Now())
	c := *s.cur
	return &c
}

func phase(d *Daemon) rollout.Phase {
	s := &d.rollouts
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		return "none"
	}
	return s.cur.Phase
}

// A node takes a rollout, fetches and checks its package, waits for the
// node before it, upgrades on its turn, and says done.
func TestRolloutNodeTurn(t *testing.T) {
	d, _, key, up := rolloutDaemon(t)
	r := testRollout(t, d)
	sendRolloutMsg(t, d, key, r)
	if _, err := os.Stat(filepath.Join(d.opts.StateDir, rolloutFile)); err != nil {
		t.Fatalf("the rollout is not kept on disk: %v", err)
	}
	step(d) // "" → fetching
	if p := phase(d); p != rollout.Fetching {
		t.Fatalf("phase %q, want fetching", p)
	}
	step(d) // starts the fetch: the package is kept already
	waitFor(t, "ready", func() bool { return phase(d) == rollout.Ready })
	c := step(d)
	if c.Phase != rollout.Ready || c.To != "0.3.1" {
		t.Fatalf("before the first node is done: %+v", c)
	}
	if w := d.rolloutStatus().Waiting; w == "" {
		t.Fatal("status says nothing about whose turn it is")
	}
	peerSays(d, macFirst, "first", 2, &gossip.RolloutState{ID: r.ID, Phase: "upgrading"})
	if step(d).Phase != rollout.Ready {
		t.Fatal("started while the node before it upgrades")
	}
	peerSays(d, macFirst, "first", 2, &gossip.RolloutState{ID: r.ID, Phase: "done"})
	if c := step(d); c.Phase != rollout.Upgrading || len(up.started) != 1 {
		t.Fatalf("its turn: %+v, started %v", c, up.started)
	}
	if g := d.rolloutGossip(); g == nil || g.Phase != "upgrading" || g.ID != r.ID {
		t.Fatalf("gossip says %+v", g)
	}
	step(d)
	if phase(d) != rollout.Upgrading {
		t.Fatal("left upgrading while the upgrade runs")
	}
	up.mu.Lock()
	up.running, up.result = false, &upgrade.Result{Result: upgrade.ResultUpgraded, Finished: time.Now()}
	up.mu.Unlock()
	if c := step(d); c.Phase != rollout.Done {
		t.Fatalf("after a good upgrade: %+v", c)
	}
}

// A node before this one that rolls back halts it; so does one that went
// silent on its turn for too long.
func TestRolloutHalts(t *testing.T) {
	d, _, key, up := rolloutDaemon(t)
	r := testRollout(t, d)
	sendRolloutMsg(t, d, key, r)
	step(d)
	step(d)
	waitFor(t, "ready", func() bool { return phase(d) == rollout.Ready })
	peerSays(d, macFirst, "first", 2, &gossip.RolloutState{ID: r.ID, Phase: "rolled-back", Reason: "meshd is not running"})
	c := step(d)
	if c.Phase != rollout.Halted || c.Reason != "first rolled-back: meshd is not running" || len(up.started) != 0 {
		t.Fatalf("after the node before it rolled back: %+v", c)
	}

	d2, _, key2, _ := rolloutDaemon(t)
	r2 := testRollout(t, d2)
	sendRolloutMsg(t, d2, key2, r2)
	// Got past the stall time ago (by this node's clock: the origin's
	// start is not used).
	d2.rollouts.mu.Lock()
	d2.rollouts.cur.Received = time.Now().Add(-rollout.Stalled - time.Minute)
	d2.rollouts.mu.Unlock()
	step(d2)
	if c := step(d2); c.Phase != rollout.Halted {
		t.Fatalf("with the node before it silent past the stall: %+v", c)
	}
}

// A node that does not take the release says skipped, with why; a
// rollout without it in its order, or cancelled, is not taken.
func TestRolloutSkips(t *testing.T) {
	d, _, key, up := rolloutDaemon(t)
	up.accepts, up.why = false, "Trellis is not installed from its package"
	sendRolloutMsg(t, d, key, testRollout(t, d))
	if c := step(d); c.Phase != rollout.Skipped || c.Reason != up.why {
		t.Fatalf("not packaged: %+v", c)
	}

	d, _, key, up = rolloutDaemon(t)
	up.checkErr = errors.New("version 0.3.1 is already installed")
	sendRolloutMsg(t, d, key, testRollout(t, d))
	step(d)
	step(d)
	waitFor(t, "skipped", func() bool { return phase(d) == rollout.Skipped })

	d, _, key, _ = rolloutDaemon(t)
	r := testRollout(t, d)
	r.Packages[0].Arch = "armhf"
	sendRolloutMsg(t, d, key, r)
	if c := step(d); c.Phase != rollout.Skipped || c.Reason != "no package for arm64" {
		t.Fatalf("no package: %+v", c)
	}

	d, _, key, _ = rolloutDaemon(t)
	r = testRollout(t, d)
	r.Order = []string{macFirst, macOrigin}
	sendRolloutMsg(t, d, key, r)
	if phase(d) != "none" || !d.rollouts.notMine[r.ID] {
		t.Fatal("took a rollout without this node in its order")
	}
}

// Cancelled by the origin: a node that has not begun stops.
func TestRolloutCancel(t *testing.T) {
	d, _, key, _ := rolloutDaemon(t)
	r := testRollout(t, d)
	sendRolloutMsg(t, d, key, r)
	step(d)
	r.Cancelled = true
	sendRolloutMsg(t, d, key, r)
	if c := step(d); c.Phase != rollout.Halted || c.Reason != "the rollout was cancelled" {
		t.Fatalf("after cancel: %+v", c)
	}
}

// The upgrade restarts meshd: the node picks its part up from disk and
// reads the upgrade's result.
func TestRolloutAcrossRestart(t *testing.T) {
	d, _, key, _ := rolloutDaemon(t)
	r := testRollout(t, d)
	sendRolloutMsg(t, d, key, r)
	step(d)
	step(d)
	waitFor(t, "ready", func() bool { return phase(d) == rollout.Ready })
	peerSays(d, macFirst, "first", 2, &gossip.RolloutState{ID: r.ID, Phase: "done"})
	step(d)
	if phase(d) != rollout.Upgrading {
		t.Fatal("did not start")
	}

	// A new daemon on the same state directory.
	d2, _, _, up2 := rolloutDaemon(t)
	d2.opts.StateDir = d.opts.StateDir
	d2.loadRollout()
	if phase(d2) != rollout.Upgrading {
		t.Fatalf("after the restart: %q", phase(d2))
	}
	up2.result = &upgrade.Result{Result: upgrade.ResultRolledBack, Reason: "no mesh peer came back", Finished: time.Now()}
	if c := step(d2); c.Phase != rollout.RolledBack || c.Reason != "no mesh peer came back" {
		t.Fatalf("after a rollback: %+v", c)
	}
}

// Every node in the order finished: the rollout is over.
func TestRolloutOver(t *testing.T) {
	d, _, key, up := rolloutDaemon(t)
	up.accepts = false
	r := testRollout(t, d)
	sendRolloutMsg(t, d, key, r)
	step(d)
	peerSays(d, macFirst, "first", 2, &gossip.RolloutState{ID: r.ID, Phase: "done"})
	d.peers.Update(&gossip.Record{V: 1, MAC: macOrigin, ID: 3, Callsign: "origin", Addr: "10.73.3.1", Rollout: &gossip.RolloutState{ID: r.ID, Phase: "done"}, Seq: 1}, "10.73.3.1")
	step(d)
	s := &d.rollouts
	s.mu.Lock()
	over := rollout.Over(&s.cur.Rollout, macMe, s.cur.Phase, s.cur.received(), s.others, time.Now())
	s.mu.Unlock()
	if !over {
		t.Fatal("not over with every node finished")
	}
}

// A node away when the rollout halted: back, it halts too instead of
// upgrading by itself; and the rollout, stopped, ends once the nodes still
// out have been silent for the stall time.
func TestRolloutBackAfterHalt(t *testing.T) {
	d, _, key, up := rolloutDaemon(t)
	r := testRollout(t, d)
	r.Order = []string{macMe, macFirst, macOrigin} // this node first
	sendRolloutMsg(t, d, key, r)
	peerSays(d, macFirst, "first", 2, &gossip.RolloutState{ID: r.ID, Phase: "halted", Reason: "me did not move on its turn"})
	step(d)
	c := step(d)
	if c.Phase != rollout.Halted || c.Reason != "me did not move on its turn" || len(up.started) != 0 {
		t.Fatalf("back after the halt: %+v, started %v", c, up.started)
	}
}
