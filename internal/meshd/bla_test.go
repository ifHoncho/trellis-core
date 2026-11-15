package meshd

import (
	"testing"

	"meridian/trellis/internal/config"
)

// Unset in the profile, bridge loop avoidance follows wired client ports,
// and once on it stays on for that bat0: unplugging and plugging a cable
// back in must not start its start-up wait again.
func TestBLAFollowsWiredPortsAndSticks(t *testing.T) {
	d := &Daemon{profile: &config.Profile{}, cfg: &config.Node{}, facts: &Facts{}}
	b := &batmanReconciler{d: d}
	if b.blaOn(7) {
		t.Fatal("on without a wired client port")
	}
	d.facts.Update(func(f *Facts) { f.AutoClients = []string{"eth1"} })
	if !b.blaOn(7) {
		t.Fatal("off with a wired client port")
	}
	d.facts.Update(func(f *Facts) { f.AutoClients = nil })
	if !b.blaOn(7) {
		t.Error("turned off again on the same bat0 when the cable was pulled")
	}
	if b.blaOn(8) {
		t.Error("still on for a new bat0 with no wired client port")
	}
	off := false
	d.profile.Batman.BridgeLoopAvoidance = &off
	d.facts.Update(func(f *Facts) { f.AutoClients = []string{"eth1"} })
	if b.blaOn(8) {
		t.Error("on although the profile turns it off")
	}
}
