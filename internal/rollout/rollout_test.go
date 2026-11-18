package rollout

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestOrder(t *testing.T) {
	got := Order([]Node{
		{MAC: "origin", ID: 1, Hops: 0},
		{MAC: "near-b", ID: 7, Hops: 1},
		{MAC: "far", ID: 9, Hops: 3},
		{MAC: "near-a", ID: 2, Hops: 1},
		{MAC: "mid", ID: 4, Hops: 2},
	})
	want := []string{"far", "mid", "near-a", "near-b", "origin"}
	if !slices.Equal(got, want) {
		t.Errorf("order %v, want %v", got, want)
	}
}

var t0 = time.Date(2026, 11, 15, 1, 0, 0, 0, time.UTC)

func rollout(order ...string) *Rollout {
	return &Rollout{ID: "r1", Version: "0.4.1", Order: order, Started: t0,
		Packages: []Package{{Arch: "arm64", SHA256: strings.Repeat("a", 64), Size: 1}}}
}

func name(m string) string { return m }

func TestTurn(t *testing.T) {
	r := rollout("a", "b", "c")
	at := func(min int) time.Time { return t0.Add(time.Duration(min) * time.Minute) }
	for _, c := range []struct {
		name   string
		me     string
		states map[string]State
		now    time.Time
		want   Action
		why    string
	}{
		{"first goes at once", "a", nil, at(0), Start, ""},
		{"second waits for the first", "b", map[string]State{"a": {Upgrading, "", at(1)}}, at(3), Wait, "a's turn (upgrading)"},
		{"second goes when the first is done", "b", map[string]State{"a": {Done, "", at(4)}}, at(4), Start, ""},
		{"a skipped node does not hold the rest", "c", map[string]State{"a": {Skipped, "no package for armhf", at(1)}, "b": {Done, "", at(5)}}, at(5), Start, ""},
		{"a rollback halts the rest", "c", map[string]State{"a": {Done, "", at(4)}, "b": {RolledBack, "meshd is not running", at(9)}}, at(9), Halt, "b rolled-back: meshd is not running"},
		{"a halt passes on", "c", map[string]State{"a": {Failed, "x", at(4)}, "b": {Halted, "a failed: x", at(5)}}, at(5), Halt, "a failed: x"},
		{"silent first node stalls the rest", "b", nil, at(16), Halt, "a did not move on its turn"},
		{"a long-ready node is not stalled when its turn comes", "c",
			map[string]State{"a": {Done, "", at(20)}, "b": {Ready, "", at(1)}}, at(21), Wait, "b's turn (ready)"},
		{"the turn's clock starts when the one before finished", "c",
			map[string]State{"a": {Done, "", at(20)}, "b": {Ready, "", at(1)}}, at(36), Halt, "b did not move on its turn"},
		{"a phase change restarts the clock", "c",
			map[string]State{"a": {Done, "", at(20)}, "b": {Upgrading, "", at(30)}}, at(40), Wait, "b's turn (upgrading)"},
		{"not in the order", "z", nil, at(0), Halt, "not in the rollout's order"},
		{"a node away when the rest halted halts when back", "a",
			map[string]State{"b": {Halted, "a did not move on its turn for 15 min", at(20)}, "c": {Halted, "a did not move", at(20)}}, at(300), Halt, "a did not move on its turn for 15 min"},
		{"one that rolled back after this node stops it too", "a", map[string]State{"c": {RolledBack, "x", at(5)}}, at(6), Halt, "c rolled-back"},
		{"a rollback wins over a halt", "a", map[string]State{"b": {Halted, "c rolled-back: x", at(5)}, "c": {RolledBack, "x", at(5)}}, at(6), Halt, "c rolled-back: x"},
		{"a halt with no reason names the node", "a", map[string]State{"b": {Halted, "", at(5)}}, at(6), Halt, "b halted"},
	} {
		d := Turn(r, c.me, time.Time{}, c.states, name, c.now)
		if d.Action != c.want || !strings.Contains(d.Why, c.why) {
			t.Errorf("%s: %v %q, want %v %q", c.name, d.Action, d.Why, c.want, c.why)
		}
	}
	r.Cancelled = true
	if d := Turn(r, "a", time.Time{}, nil, name, t0); d.Action != Halt {
		t.Errorf("cancelled: %v", d)
	}
}

// Turns count from when this node got the rollout, by its own clock.
func TestTurnFromReceived(t *testing.T) {
	r := rollout("a", "b")
	// The origin's clock is an hour behind: the start looks old.
	got := t0.Add(time.Hour)
	if d := Turn(r, "b", got, nil, name, got.Add(time.Minute)); d.Action != Wait {
		t.Errorf("a minute after it arrived: %v %q", d.Action, d.Why)
	}
	if d := Turn(r, "b", got, nil, name, got.Add(Stalled+time.Minute)); d.Action != Halt {
		t.Errorf("past the stall: %v %q", d.Action, d.Why)
	}
}

func TestOver(t *testing.T) {
	r := rollout("a", "b", "c")
	at := func(min int) time.Time { return t0.Add(time.Duration(min) * time.Minute) }
	for _, c := range []struct {
		name   string
		me     string
		mine   Phase
		states map[string]State
		now    time.Time
		want   bool
	}{
		{"every node done", "c", Done, map[string]State{"a": {Done, "", at(1)}, "b": {Skipped, "", at(2)}}, at(3), true},
		{"this node still upgrading", "c", Upgrading, map[string]State{"a": {Done, "", at(1)}, "b": {Done, "", at(2)}}, at(3), false},
		{"one still waiting, nothing stopped", "a", Done, map[string]State{"b": {Ready, "", at(1)}}, at(60), false},
		{"halted on a silent node: not yet", "c", Halted, map[string]State{"b": {Halted, "", at(16)}}, at(10), false},
		{"halted on a silent node: over after the stall", "c", Halted, map[string]State{"b": {Halted, "", at(16)}}, at(16), true},
		{"a rollback, the rest halted", "c", Halted, map[string]State{"a": {RolledBack, "", at(4)}, "b": {Halted, "", at(4)}}, at(5), true},
		{"a node upgrading lately is waited for", "c", Halted, map[string]State{"a": {Upgrading, "", at(10)}, "b": {Halted, "", at(11)}}, at(20), false},
	} {
		if got := Over(r, c.me, c.mine, time.Time{}, c.states, c.now); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	r.Cancelled = true
	if !Over(r, "c", Halted, time.Time{}, map[string]State{"a": {Done, "", t0}, "b": {Halted, "", t0}}, t0) {
		t.Error("cancelled, every node final: not over")
	}
}

func TestValidate(t *testing.T) {
	if err := rollout("a", "b").Validate(); err != nil {
		t.Fatal(err)
	}
	for name, r := range map[string]*Rollout{
		"twice":    rollout("a", "a"),
		"no nodes": rollout(),
		"bad hash": {ID: "x", Version: "1", Order: []string{"a"}, Packages: []Package{{Arch: "arm64", SHA256: "zz", Size: 1}}},
	} {
		if r.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if p := rollout("a").Package("armhf"); p != nil {
		t.Error("a package for an architecture it does not have")
	}
}
