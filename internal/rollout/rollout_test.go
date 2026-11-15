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
	} {
		d := Turn(r, c.me, c.states, name, c.now)
		if d.Action != c.want || !strings.Contains(d.Why, c.why) {
			t.Errorf("%s: %v %q, want %v %q", c.name, d.Action, d.Why, c.want, c.why)
		}
	}
	r.Cancelled = true
	if d := Turn(r, "a", nil, name, t0); d.Action != Halt {
		t.Errorf("cancelled: %v", d)
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
