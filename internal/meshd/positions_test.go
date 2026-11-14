package meshd

import (
	"math"
	"strings"
	"testing"
	"time"

	"meridian/trellis/internal/config"
	"meridian/trellis/internal/cot"
	"meridian/trellis/internal/gnss"
	"meridian/trellis/internal/gossip"
)

func TestDistance(t *testing.T) {
	// One minute of latitude is a nautical mile.
	if d := distanceM(52, 5, 52+1.0/60, 5); math.Abs(d-1853) > 3 {
		t.Errorf("one minute of latitude: %.1f m", d)
	}
	if d := distanceM(52.5, 5.5, 52.5, 5.5); d != 0 {
		t.Errorf("same point: %v", d)
	}
}

func TestNodeEvent(t *testing.T) {
	now := time.Unix(1794651635, 0)
	fix := gnss.State{Mode: gnss.Mode3D, Lat: 52.1, Lon: 5.2, AltM: 250, HasAlt: true, HAccM: 23, VAccM: 31, SatsUsed: 8}
	ev := nodeEvent("a0:b0:c0:00:00:01", "relay-1", fix, now)
	if ev.UID != "trellis-node-a0b0c0000001" || ev.Type != cot.NodeType || ev.Callsign != "relay-1" || ev.HAE != 250 || ev.CE != 23 || ev.LE != 31 {
		t.Errorf("event: %+v", ev)
	}
	if !ev.Stale.Equal(now.Add(posStaleAfter)) || ev.Remarks != "Trellis mesh node. 3D fix, 8 satellites, ±23 m." {
		t.Errorf("stale %v, remarks %q", ev.Stale, ev.Remarks)
	}
	// Without height or error, CoT's unknown.
	ev = nodeEvent("a0:b0:c0:00:00:01", "relay-1", gnss.State{Mode: gnss.Mode2D, Lat: 1, Lon: 2}, now)
	if ev.HAE != cot.Unknown || ev.CE != cot.Unknown || ev.LE != cot.Unknown {
		t.Errorf("unknowns: %+v", ev)
	}
}

func TestLastKnownEvent(t *testing.T) {
	heard := time.Date(2026, 1, 2, 15, 4, 0, 0, time.UTC)
	k := &lastKnownPos{callsign: "ridge-2", heard: heard, pos: gossip.Position{Lat: 52.2, Lon: 5.3, Acc: 12, Fix: 3}}
	now := heard.Add(20 * time.Minute)
	ev := lastKnownEvent("a0:b0:c0:00:00:02", k, now)
	// The same marker as the node's own, already stale.
	if ev.UID != "trellis-node-a0b0c0000002" || ev.Callsign != "ridge-2 (last known)" || !ev.Time.Equal(now) || !ev.Start.Equal(heard) || !ev.Stale.Before(now) {
		t.Errorf("event: %+v", ev)
	}
	if !strings.Contains(ev.Remarks, "since 15:04 UTC") || ev.CE != 12 || ev.HAE != cot.Unknown {
		t.Errorf("event: %+v", ev)
	}
	if _, err := ev.Marshal(); err != nil {
		t.Error(err)
	}
}

func TestPositionsOff(t *testing.T) {
	no := false
	d := &Daemon{profile: &config.Profile{}, cfg: &config.Node{}}
	if d.positionsOff() != "" {
		t.Error("off by default")
	}
	d.cfg.Positions.Share = &no
	if d.positionsOff() != "node" {
		t.Errorf("node off: %q", d.positionsOff())
	}
	d.profile.Positions.Share = &no
	if d.positionsOff() != "profile" {
		t.Errorf("profile off: %q", d.positionsOff())
	}
	// Off, the record says so and carries no position.
	if pos, off := d.gossipPosition(time.Now()); pos != nil || !off {
		t.Errorf("gossip with sharing off: %+v %v", pos, off)
	}
	// On, with no receiver: no position, but not off.
	d.profile.Positions.Share, d.cfg.Positions.Share = nil, nil
	if pos, off := d.gossipPosition(time.Now()); pos != nil || off {
		t.Errorf("gossip without a receiver: %+v %v", pos, off)
	}
}
