package meshd

import (
	"strings"
	"testing"
	"time"

	"meridian/trellis/internal/config"
	"meridian/trellis/internal/gossip"
	"meridian/trellis/internal/nl80211"
)

// Only the smaller part of a mesh looks for the rest.
func TestSearchWanted(t *testing.T) {
	for _, c := range []struct {
		name        string
		known, live int
		want        bool
	}{
		{"a mesh of one node", 0, 0, false},
		{"the only other node gone", 1, 0, true},
		{"two nodes together", 1, 1, false},
		{"one of five switched off: the four", 4, 3, false},
		{"two of five left behind: the two", 4, 1, true},
		{"two of five left behind: the three", 4, 2, false},
		{"an even split", 3, 1, true},
		{"alone of five", 4, 0, true},
	} {
		if got := searchWanted(c.known, c.live); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}

func TestMeshHeard(t *testing.T) {
	bss := []nl80211.BSS{
		{FreqMHz: 2412, SignalDBm: -60, MeshID: "trellis-alpha", WidthMHz: 20},
		{FreqMHz: 2412, SignalDBm: -40, MeshID: "trellis-alpha"},
		{FreqMHz: 2437, SignalDBm: -30, MeshID: "another-mesh"},
		{FreqMHz: 2462, SignalDBm: -30, SSID: "cafe"},
		{FreqMHz: 2462, SignalDBm: -50, MeshID: "trellis-alpha", SeenMSAgo: 60_000}, // heard a minute ago
		{FreqMHz: 5745, SignalDBm: -70, MeshID: "trellis-alpha", WidthMHz: 80},      // another band
	}
	got := meshHeard(bss, "trellis-alpha", findMeshSeen)
	if len(got) != 2 {
		t.Fatalf("heard: %+v", got)
	}
	if h := got[0]; h.band != config.Band2G || h.channel != 1 || h.signal != -40 || h.width != 20 {
		t.Errorf("2.4 GHz: %+v (the strongest beacon, with the width another gave)", h)
	}
	if h := got[1]; h.band != config.Band5G || h.channel != 149 || h.plan().Width != 80 {
		t.Errorf("5 GHz: %+v", h)
	}
	if p := (heardNet{band: config.Band5G, channel: 36}).plan(); p.Width != 20 {
		t.Errorf("no width in the beacons: %+v", p)
	}
}

// The link's own band first, however strong another band is.
func TestBetterHeard(t *testing.T) {
	home := heardNet{band: config.Band2G, channel: 11, signal: -80}
	other := heardNet{band: config.Band5G, channel: 149, signal: -40}
	if !betterHeard(home, config.Band2G, other, config.Band2G) || betterHeard(other, config.Band2G, home, config.Band2G) {
		t.Error("the link's own band first")
	}
	strong := heardNet{band: config.Band2G, channel: 6, signal: -50}
	if !betterHeard(strong, config.Band2G, home, config.Band2G) {
		t.Error("then the stronger")
	}
}

// A visit counts only under the profile it was made for.
func TestVisitPlan(t *testing.T) {
	d, _, _ := meshChangeDaemon(t)
	_, p := d.conf()
	rp, _ := p.Link(0)
	to := config.RadioPlan{Band: rp.Band, Channel: rp.Channel + 5, Width: 40}
	d.findMesh.visit = &meshVisit{link: 0, plan: to, from: rp, rev: p.Revision, change: p.ChangeID(), since: time.Now()}
	if got, ok := d.visitPlan(0, p); !ok || got != to {
		t.Fatalf("visiting: %+v %v", got, ok)
	}
	if _, ok := d.visitPlan(1, p); ok {
		t.Error("another link")
	}
	q := *p
	q.Revision++
	if _, ok := d.visitPlan(0, &q); ok {
		t.Error("a newer revision")
	}
	q = *p
	q.Radio.Channel = rp.Channel + 5
	if _, ok := d.visitPlan(0, &q); ok {
		t.Error("the profile moved the link")
	}
	q = *p
	q.StartChange(p, time.Now(), "node-2")
	if _, ok := d.visitPlan(0, &q); ok {
		t.Error("another change at the same revision")
	}
}

// How a visit ends.
func TestTendVisit(t *testing.T) {
	d, _, _ := meshChangeDaemon(t)
	_, p := d.conf()
	rp, _ := p.Link(0)
	now := time.Now()
	visit := func(age time.Duration) *meshVisit {
		return &meshVisit{link: 0, plan: config.RadioPlan{Band: rp.Band, Channel: 11, Width: 20}, from: rp, rev: p.Revision,
			since: now.Add(-age), before: map[string]bool{"02:00:00:00:00:05": true}}
	}
	same := gossip.Record{MAC: "02:00:00:00:00:07", ProfileRev: p.Revision}
	ahead := gossip.Record{MAC: "02:00:00:00:00:08", ProfileRev: p.Revision + 1}
	old := gossip.Record{MAC: "02:00:00:00:00:05", ProfileRev: p.Revision} // heard before the visit

	v := visit(3 * time.Minute)
	if why, gave := d.tendVisit(v, p, nil, 0, now); !strings.Contains(why, "no node there peered") || gave {
		t.Errorf("nobody there: %q %v", why, gave)
	}

	v = visit(20 * time.Second)
	v.lastPeer = now
	recs := map[string]gossip.Record{old.MAC: old, same.MAC: same}
	if why, _ := d.tendVisit(v, p, recs, 0, now); why != "" {
		t.Fatalf("ended at once: %q", why)
	}
	if why, gave := d.tendVisit(v, p, recs, 0, now.Add(findMeshSettle+time.Second)); !strings.Contains(why, "same profile") || gave {
		t.Errorf("the same there: %q %v", why, gave)
	}

	v = visit(20 * time.Second)
	v.lastPeer = now
	recs = map[string]gossip.Record{same.MAC: same, ahead.MAC: ahead}
	for _, at := range []time.Duration{0, findMeshSettle + time.Second} {
		if why, _ := d.tendVisit(v, p, recs, 0, now.Add(at)); why != "" {
			t.Fatalf("a node ahead there, the visit waits for its profile: %q", why)
		}
	}
	if why, gave := d.tendVisit(v, p, recs, 0, now.Add(findMeshGone+time.Second)); !strings.Contains(why, "left") || !gave {
		t.Errorf("the nodes there left: %q %v", why, gave)
	}

	q := *p
	q.Revision++
	if why, gave := d.tendVisit(visit(time.Second), &q, nil, 0, now); !strings.Contains(why, "profile changed") || !gave {
		t.Errorf("the profile arrived: %q %v", why, gave)
	}
	if why, _ := d.tendVisit(visit(findMeshVisitMax), p, recs, 0, now); !strings.Contains(why, "no outcome") {
		t.Errorf("the longest visit: %q", why)
	}
}

func TestOnClock(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Hour)
	if got := onClock(past, now); !got.Equal(past) {
		t.Errorf("a time with a monotonic reading: %v, want %v", got, past)
	}
	wall := past.Round(0) // as from a file or another node
	if got := onClock(wall, now); !got.Equal(wall) {
		t.Errorf("a wall-clock time: %v, want %v", got, wall)
	}
	if got := onClock(time.Time{}, now); !got.IsZero() {
		t.Errorf("zero: %v", got)
	}
}
