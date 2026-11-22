package meshd

import (
	"strings"
	"testing"
	"time"

	"meridian/trellis/internal/config"
	"meridian/trellis/internal/gossip"
)

func TestCongestion(t *testing.T) {
	t0 := time.Date(2026, 11, 21, 2, 0, 0, 0, time.UTC)
	var c congestion
	names, keys := []string{"2.4 GHz · ch 11"}, []string{"2g/11/20"}
	step := func(min int, worst int, at string) int {
		return len(c.observe(t0.Add(time.Duration(min)*time.Minute), names, keys, []linkSample{{worst: worst, at: at}}))
	}
	// Busy, but not yet for long enough.
	if n := step(0, 72, "node-1"); n != 0 {
		t.Fatalf("raised at once: %d", n)
	}
	if n := step(9, 65, "node-1"); n != 0 {
		t.Fatalf("raised before 10 min: %d", n)
	}
	// Ten minutes at or above 60 %.
	w := c.observe(t0.Add(10*time.Minute), names, keys, []linkSample{{worst: 70, at: "node-3"}})
	if len(w) != 1 || w[0].Code != "channel-busy" || !strings.Contains(w[0].Message, "70 % at node-3") || !strings.Contains(w[0].Message, "2.4 GHz · ch 11") {
		t.Fatalf("not raised after 10 min: %+v", w)
	}
	// Between the marks it stays raised.
	if n := step(11, 55, "node-3"); n != 1 {
		t.Fatalf("dropped between the marks: %d", n)
	}
	// Below 50 % for less than 2 min: still raised; then cleared.
	if n := step(12, 40, "node-3"); n != 1 {
		t.Fatalf("cleared at once: %d", n)
	}
	if n := step(14, 35, "node-3"); n != 0 {
		t.Fatalf("not cleared after 2 min below: %d", n)
	}
	// A dip to 58 (between the marks) does not break a run; one under 50
	// does.
	step(20, 80, "node-1")
	step(25, 58, "node-1")
	if n := step(30, 80, "node-1"); n != 1 {
		t.Fatalf("a run through a dip to 58 not raised at 10 min: %d", n)
	}
	step(31, 40, "node-1")
	step(32, 40, "node-1")
	if n := step(33, 40, "node-1"); n != 0 {
		t.Fatalf("not cleared after 2 min below 50: %d", n)
	}
	step(34, 80, "node-1")
	step(35, 45, "node-1")
	if n := step(41, 80, "node-1"); n != 0 {
		t.Fatalf("a run broken by a dip under 50 still raised: %d", n)
	}
	// Raised from the run that began at 41; then no node measuring.
	if n := step(51, 90, "node-1"); n != 1 {
		t.Fatalf("not raised 10 min into the new run: %d", n)
	}
	if w := c.observe(t0.Add(52*time.Minute), names, keys, []linkSample{{worst: -1}}); len(w) != 1 || !strings.Contains(w[0].Message, "90 % at node-1") {
		t.Fatalf("one unmeasured sample: %+v", w)
	}
	// Nobody measuring for 2 min: cleared, as a quiet channel would be.
	if n := step(54, -1, ""); n != 0 {
		t.Fatalf("an alert with no data kept: %d", n)
	}
	// A gap with no data breaks a busy run.
	step(60, 70, "node-1")
	step(65, -1, "")
	if n := step(71, 61, "node-1"); n != 0 {
		t.Fatalf("a run across a gap raised: %d", n)
	}
	// The link moved (another key): a fresh start, the old state dropped.
	if w := c.observe(t0.Add(73*time.Minute), []string{"2.4 GHz · ch 6"}, []string{"2g/6/20"}, []linkSample{{worst: 90, at: "node-1"}}); len(w) != 0 {
		t.Fatalf("a moved link carried the alert: %+v", w)
	}
	if len(c.links) != 1 {
		t.Fatalf("old link state kept: %d", len(c.links))
	}
}

func TestOtherSite(t *testing.T) {
	at := func(site string) gossip.Record { return gossip.Record{Site: &gossip.SiteState{Site: site}} }
	switch {
	case !otherSite(at("hq"), "field"):
		t.Error("a node at another site not taken as one")
	case otherSite(at("field"), "field"):
		t.Error("a node at this site taken as another's")
	case otherSite(gossip.Record{}, "field"):
		t.Error("a node in no site link taken as another site's")
	case otherSite(at("hq"), ""):
		t.Error("a node not in the site link judged others by site")
	}
}

func TestOnPlan(t *testing.T) {
	p149 := config.RadioPlan{Band: config.Band5G, Channel: 149, Width: 80}
	switch {
	case !onPlan(p149, 5745):
		t.Error("on the plan's channel: not taken")
	case onPlan(p149, 5180):
		t.Error("a figure from ch 36 taken for ch 149")
	case !onPlan(p149, 0):
		t.Error("channel not known: refused")
	case !onPlan(config.RadioPlan{Band: config.BandS1G, Channel: 27, Width: 2}, 5180):
		t.Error("HaLow (its kernel channel a 5 GHz stand-in): refused")
	}
}

// While a raised alert clears, it keeps the figure that raised it.
func TestCongestionClearingFigure(t *testing.T) {
	t0 := time.Date(2026, 11, 21, 2, 0, 0, 0, time.UTC)
	var c congestion
	names, keys := []string{"5 GHz · ch 149"}, []string{"5g/149/80"}
	obs := func(min, worst int) []string {
		var out []string
		for _, w := range c.observe(t0.Add(time.Duration(min)*time.Minute), names, keys, []linkSample{{worst: worst, at: "bravo-5"}}) {
			out = append(out, w.Message)
		}
		return out
	}
	obs(0, 75)
	obs(10, 75)
	if w := obs(11, 20); len(w) != 1 || !strings.Contains(w[0], "75 % at bravo-5") {
		t.Fatalf("clearing: %+v", w)
	}
}
