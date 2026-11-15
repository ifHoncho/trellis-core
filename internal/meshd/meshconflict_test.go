package meshd

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"meridian/trellis/internal/api"
	"meridian/trellis/internal/config"
	"meridian/trellis/internal/gossip"
)

// changedProfile is the test profile, as a mesh-wide change after prev.
func changedProfile(t *testing.T, prev *config.Profile, rev uint64, channel int, at time.Time, by string) *config.Profile {
	t.Helper()
	p, err := config.ParseProfile([]byte(testProfile))
	if err != nil {
		t.Fatal(err)
	}
	p.Radio.Channel, p.Revision = channel, rev
	p.StartChange(prev, at, by)
	return p
}

func yamlOf(t *testing.T, p *config.Profile) string {
	t.Helper()
	b, err := p.YAML("")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Two parts of the mesh changed the profile apart: the later change is
// kept, as a new revision above both, and the other is kept on disk.
func TestMeshChangeConflict(t *testing.T) {
	d, dir, key := meshChangeDaemon(t)
	from := &net.UDPAddr{IP: net.ParseIP("10.73.2.1")}
	t0 := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	common := changedProfile(t, nil, 1, 6, t0, "node-1")
	mine := changedProfile(t, common, 2, 1, t0.Add(30*time.Minute), "node-1")
	theirs := changedProfile(t, common, 2, 11, t0.Add(time.Hour), "node-4") // later: kept
	d.profile = mine

	if err := d.handleProfileOffer(key, offer(t, key, gossip.Offer{Rev: 2, Profile: yamlOf(t, theirs)}), from); err != nil {
		t.Fatal(err)
	}
	c := d.meshChangeState()
	if c == nil || c.Rev != 3 || c.State != "pending" || !c.Sender {
		t.Fatalf("settled change: %+v", c)
	}
	merged, err := config.ParseProfile([]byte(c.YAML))
	if err != nil {
		t.Fatal(err)
	}
	want, _, _, _ := config.Reconcile(theirs, mine)
	if merged.Radio.Channel != 11 || merged.ChangeID() != want.ChangeID() {
		t.Errorf("kept: channel %d, change %s (want %s)", merged.Radio.Channel, merged.ChangeID(), want.ChangeID())
	}
	if d := time.Until(c.ApplyAt); d <= 0 || d > meshConflictSwitch+time.Second {
		t.Errorf("switch in %v", d)
	}
	dropped, err := config.LoadProfile(filepath.Join(dir, "state", meshConflictFile))
	if err != nil || dropped.Radio.Channel != 1 || dropped.ChangeID() != mine.ChangeID() {
		t.Errorf("the dropped profile is kept: %+v %v", dropped, err)
	}
	if b, _ := os.Stat(filepath.Join(dir, "state", meshConflictFile)); b == nil || b.Mode().Perm() != 0o600 {
		t.Error("the dropped profile holds the secret: 0600")
	}
	msg, ok := d.meshConflictWarning()
	if !ok || !strings.Contains(msg, "node-4's") || !strings.Contains(msg, meshConflictFile) {
		t.Errorf("alert: %q", msg)
	}

	// The same settlement from the other part, or the other's offer again:
	// nothing more happens.
	if err := d.handleProfileOffer(key, offer(t, key, gossip.Offer{Rev: 3, Profile: c.YAML}), from); err != nil {
		t.Fatal(err)
	}
	if err := d.handleProfileOffer(key, offer(t, key, gossip.Offer{Rev: 2, Profile: yamlOf(t, theirs)}), from); err != nil {
		t.Fatal(err)
	}
	if c2 := d.meshChangeState(); c2.Rev != 3 || c2.ApplyAt != c.ApplyAt {
		t.Errorf("settled twice: %+v", c2)
	}
	d.meshChanges.timer.Stop()
}

// A node of the other part takes the settlement as an ordinary newer
// change; an older change of this node's own line is refused.
func TestMeshChangeSettlementSpreads(t *testing.T) {
	d, _, key := meshChangeDaemon(t)
	from := &net.UDPAddr{IP: net.ParseIP("10.73.2.1")}
	t0 := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	common := changedProfile(t, nil, 1, 6, t0, "node-1")
	mine := changedProfile(t, common, 2, 1, t0.Add(30*time.Minute), "node-1")
	theirs := changedProfile(t, common, 2, 11, t0.Add(time.Hour), "node-4")
	merged, _, _, _ := config.Reconcile(mine, theirs)
	d.profile = mine

	_ = d.handleProfileOffer(key, offer(t, key, gossip.Offer{Rev: 1, Profile: yamlOf(t, common)}), from)
	if c := d.meshChangeState(); c != nil {
		t.Fatalf("an earlier change was taken: %+v", c)
	}
	_ = d.handleProfileOffer(key, offer(t, key, gossip.Offer{Rev: 3, Profile: yamlOf(t, merged)}), from)
	c := d.meshChangeState()
	if c == nil || c.Rev != 3 || c.Sender {
		t.Fatalf("the settlement: %+v", c)
	}
	if _, ok := d.meshConflictWarning(); ok {
		t.Error("a node that only received the settlement settled nothing itself")
	}
	d.meshChanges.timer.Stop()
}

// The moment goes by the offer's delay when it gives one, whatever the
// two clocks say: a part of the mesh whose clock went wrong still switches
// with the rest.
func TestMeshChangeByDelay(t *testing.T) {
	d, _, key := meshChangeDaemon(t)
	from := &net.UDPAddr{IP: net.ParseIP("10.73.2.1")}
	dayAhead := time.Now().Add(24 * time.Hour).UnixMilli() // the sender's clock is a day ahead
	_ = d.handleProfileOffer(key, offer(t, key, gossip.Offer{Rev: 4, ApplyAtMS: dayAhead, InMS: 60_000, Profile: profileAt(t, 40, 4, nil)}), from)
	c := d.meshChangeState()
	if c == nil || c.State != "pending" {
		t.Fatalf("refused: %+v", c)
	}
	if in := time.Until(c.ApplyAt); in < 55*time.Second || in > 61*time.Second {
		t.Errorf("switch in %v, want about a minute", in)
	}
	d.meshChanges.timer.Stop()
}

// A peer that runs a change this node does not know is noted (and asked
// after askHoldoff); one this node has passed is not.
func TestUnknownChangeNoted(t *testing.T) {
	d, _, key := meshChangeDaemon(t)
	common := changedProfile(t, nil, 1, 6, time.Now().Add(-time.Hour), "node-1")
	d.profile = changedProfile(t, common, 2, 1, time.Now(), "node-1")
	d.peers.Update(&gossip.Record{V: 1, MAC: "02:00:00:00:00:02", ID: 2, Callsign: "node-2", Addr: "10.73.2.1", Boot: 1, Seq: 1, ProfileRev: 1, ProfileChange: common.ChangeID()}, "10.73.2.1")
	d.peers.Update(&gossip.Record{V: 1, MAC: "02:00:00:00:00:03", ID: 3, Callsign: "node-3", Addr: "10.73.3.1", Boot: 1, Seq: 1, ProfileRev: 2, ProfileChange: "00112233445566ff"}, "10.73.3.1")
	d.askForNewerProfile(key)
	if len(d.meshChanges.unknown) != 1 || d.meshChanges.unknown["00112233445566ff"].IsZero() {
		t.Errorf("unknown changes: %v", d.meshChanges.unknown)
	}
	if h := askHoldoff(9); h < 2*time.Second || h > 10*time.Second {
		t.Errorf("hold-off %v", h)
	}
}

// The roster survives a restart, with ages, and names the nodes a change
// would miss.
func TestRoster(t *testing.T) {
	d, dir, _ := meshChangeDaemon(t)
	d.time = newTimeReconciler(d)
	peers := []gossip.Peer{
		{Record: gossip.Record{MAC: "02:00:00:00:00:02", Callsign: "node-2"}},
		{Record: gossip.Record{MAC: "02:00:00:00:00:03", Callsign: "node-3"}},
	}
	d.noteRoster(peers, true)
	d.roster.mu.Lock()
	n := d.roster.nodes["02:00:00:00:00:03"]
	n.seen = time.Now().Add(-2 * time.Hour)
	d.roster.nodes["02:00:00:00:00:03"] = n
	d.roster.nodes["02:00:00:00:00:09"] = rosterNode{callsign: "gone", seen: time.Now().Add(-rosterKeep - time.Minute)}
	d.roster.mu.Unlock()
	d.noteRoster(nil, true)

	d2, err := New(Options{NodePath: filepath.Join(dir, "node.yaml"), ProfilePath: filepath.Join(dir, "profile.yaml"),
		StateDir: filepath.Join(dir, "state"), RunDir: filepath.Join(dir, "run"), SysRoot: filepath.Join(dir, "sys"), Log: d.log, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	d2.loadRoster()
	known := d2.rosterKnown()
	if len(known) != 2 || known["02:00:00:00:00:03"] != "node-3" {
		t.Fatalf("after a restart: %v", known)
	}
	if age := time.Since(d2.roster.nodes["02:00:00:00:00:03"].seen); age < 2*time.Hour-time.Minute || age > 2*time.Hour+time.Minute {
		t.Errorf("age kept: %v", age)
	}

	// node-2 is in reach, node-3 is not: a change would miss node-3.
	d.peers.Update(&gossip.Record{V: 1, MAC: "02:00:00:00:00:02", ID: 2, Callsign: "node-2", Addr: "10.73.2.1", Boot: 1, Seq: 1, MeshChange: true}, "10.73.2.1")
	res, err := d.SetMeshProfile(t.Context(), api.MeshProfileInput{ProfileInput: api.ProfileInput{YAML: profileAt(t, 40, 0, nil)}}, true)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range res.Effects {
		if e.Code == "mesh-change-away" {
			found = strings.Contains(e.Message, "node-3") && !strings.Contains(e.Message, "node-2")
		}
	}
	if !found {
		t.Errorf("effects: %+v", res.Effects)
	}
}
