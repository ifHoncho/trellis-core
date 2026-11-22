package meshd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"meridian/trellis/internal/chrony"
	"meridian/trellis/internal/gnss"
	"meridian/trellis/internal/gossip"
)

func timeTestDaemon(t *testing.T, peers ...*gossip.Record) *Daemon {
	t.Helper()
	d, _ := testDaemon(t, map[string]string{"node.yaml": "schema: 0\ncallsign: node-1\n", "profile.yaml": testProfile})
	d.peers = gossip.NewTable("02:00:00:00:00:01")
	for i, r := range peers {
		r.V, r.Boot, r.Seq = 1, 1, uint64(i+1)
		r.MAC = fmt.Sprintf("02:00:00:00:01:%02x", i+1)
		d.peers.Update(r, r.Addr)
	}
	return d
}

// A node follows the nodes on GNSS; with none, every node that serves
// time, so that nodes without an upstream settle on one clock. Every node
// picks the same ones (lowest node IDs), and only a few.
func TestMeshTimeServers(t *testing.T) {
	d := timeTestDaemon(t,
		&gossip.Record{ID: 89, Callsign: "gnss-1", Addr: "10.73.89.1", Time: gossip.TimeGNSS},
		&gossip.Record{ID: 2, Callsign: "node-2", Addr: "10.73.2.1", Time: gossip.TimeServer},
		&gossip.Record{ID: 52, Callsign: "old", Addr: "10.73.52.1"},
	)
	tr := newTimeReconciler(d)
	if got := tr.meshServers(); len(got) != 1 || got[0].Callsign != "gnss-1" {
		t.Errorf("with a GNSS node: %+v", got)
	}

	var many []*gossip.Record
	for _, id := range []int{9, 3, 7, 5, 4, 8} {
		many = append(many, &gossip.Record{ID: id, Callsign: "n", Addr: fmt.Sprintf("10.73.%d.1", id), Time: gossip.TimeServer})
	}
	d = timeTestDaemon(t, many...)
	got := newTimeReconciler(d).meshServers()
	if len(got) != maxMeshTimeServers || got[0].ID != 3 || got[3].ID != 7 {
		t.Errorf("without GNSS: %+v", got)
	}
}

func TestDescribeSource(t *testing.T) {
	d := timeTestDaemon(t, &gossip.Record{ID: 89, Callsign: "gnss-1", Addr: "10.73.89.1", Time: gossip.TimeGNSS})
	tr := newTimeReconciler(d)
	for _, c := range []struct {
		track      chrony.Tracking
		text, kind string
	}{
		{chrony.Tracking{RefName: "GNSS", Stratum: 1, Leap: "Normal"}, "GNSS receiver", "gnss"},
		{chrony.Tracking{RefName: "PPS", Stratum: 1, Leap: "Normal"}, "GNSS receiver (pulse per second)", "gnss"},
		{chrony.Tracking{RefName: "10.73.89.1", Stratum: 2, Leap: "Normal"}, "gnss-1 (10.73.89.1)", "mesh"},
		{chrony.Tracking{RefName: "10.73.40.1", Stratum: 2, Leap: "Normal"}, "mesh node 10.73.40.1", "mesh"},
		{chrony.Tracking{RefName: "199.85.8.46", Stratum: 3, Leap: "Normal"}, "internet (199.85.8.46)", "internet"},
		{chrony.Tracking{RefID: "7F7F0101", RefName: "127.127.1.1", Stratum: 10, Leap: "Normal"}, "this node's own clock (no other source in reach)", "local"},
		{chrony.Tracking{Leap: "Not synchronised"}, "", ""},
	} {
		if text, kind := tr.describeSource(c.track); text != c.text || kind != c.kind {
			t.Errorf("%+v: %q %q, want %q %q", c.track, text, kind, c.text, c.kind)
		}
	}
}

// chrony's tracking and the servers file, through a stand-in chronyc.
func TestTimeReconciler(t *testing.T) {
	d := timeTestDaemon(t, &gossip.Record{ID: 89, Callsign: "gnss-1", Addr: "10.73.89.1", Time: gossip.TimeGNSS})
	dir := t.TempDir()
	fake := filepath.Join(dir, "chronyc")
	log := filepath.Join(dir, "calls")
	script := "#!/bin/sh\necho \"$*\" >> " + log + "\ncase \"$*\" in\n" +
		"*tracking*) echo '0A495901,10.73.89.1,2,1794637769.0,0.004200000,0,0,0,0,0,0,0,64.0,Normal' ;;\nesac\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	tr := newTimeReconciler(d)
	tr.chrony.Path, tr.dir = fake, filepath.Join(dir, "sources")
	res, err := tr.Reconcile(context.Background())
	if err != nil || !res.Changed || len(res.Warnings) != 0 {
		t.Fatalf("first pass: %+v %v", res, err)
	}
	c := tr.Clock()
	if !c.Synced || c.Source != "gnss-1 (10.73.89.1)" || c.SourceKind != "mesh" || c.Stratum != 2 || c.OffsetMS != 4.2 || !c.Serving ||
		len(c.MeshServers) != 1 || c.MeshServers[0] != "gnss-1" || tr.Role() != gossip.TimeServer {
		t.Errorf("clock: %+v role %q", c, tr.Role())
	}
	data, _ := os.ReadFile(filepath.Join(dir, "sources", chrony.SourceFile))
	calls, _ := os.ReadFile(log)
	if !strings.Contains(string(data), "server 10.73.89.1 iburst") || !strings.Contains(string(calls), "reload sources") {
		t.Errorf("sources file:\n%s\ncalls:\n%s", data, calls)
	}
	// Asked again only after a while; the list unchanged is not reloaded.
	tr.last = time.Time{}
	os.Remove(log)
	if res, _ := tr.Reconcile(context.Background()); res.Changed {
		t.Error("an unchanged list reloaded chrony")
	}
	if calls, _ := os.ReadFile(log); strings.Contains(string(calls), "reload") {
		t.Errorf("calls: %s", calls)
	}

	none := newTimeReconciler(d)
	none.chrony.Path = filepath.Join(dir, "missing")
	none.dir = filepath.Join(dir, "none")
	res, _ = none.Reconcile(context.Background())
	if c := none.Clock(); c.Service != "chrony" || c.Note == "" || len(res.Warnings) != 1 || none.Role() != "" {
		t.Errorf("chronyd not answering: %+v %+v", c, res.Warnings)
	}
}

func TestGNSSStatus(t *testing.T) {
	now := time.Now()
	if g := gnssStatus(&gnss.Watcher{}, now); g.Receiver || g.Fix != "none" || !strings.Contains(g.Note, "gpsd is not running") {
		t.Errorf("no gpsd: %+v", g)
	}
	// A 3D fix with sea level only (gpsd before 3.20): no height, and why.
	st := gnss.State{Running: true, Receiver: true, Mode: gnss.Mode3D, Lat: 52, Lon: 5, Heard: now, MSLM: 12, HasMSL: true}
	if g := gnssStatusOf(st, now); g.AltM != nil || !strings.Contains(g.Note, "Height not shared") {
		t.Errorf("sea level only: %+v", g)
	}
	st.AltM, st.HasAlt = 59, true
	if g := gnssStatusOf(st, now); g.AltM == nil || g.Note != "" {
		t.Errorf("with ellipsoid height: %+v", g)
	}
}
