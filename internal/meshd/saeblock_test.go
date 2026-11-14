package meshd

import (
	"testing"
	"time"
)

// A blocked peer is unblocked after a short time, a few times over; a peer
// that still fails after that is left to wpa_supplicant's own block.
func TestSAEBlockLifts(t *testing.T) {
	var b saeBlocks
	t0 := time.Date(2026, 11, 12, 2, 42, 0, 0, time.UTC)
	const peer = "a0:b0:c0:00:89:fa"

	d, ok := b.blocked(peer, t0, 7*time.Second)
	if !ok || d != saeLiftMin+7*time.Second {
		t.Fatalf("first block: lift in %v, %v", d, ok)
	}
	// The lift is shorter than a round of four attempts (about a minute):
	// the peer's next attempt must find this node listening.
	if d >= time.Minute {
		t.Errorf("a lift after %v leaves the peer's attempts unanswered", d)
	}
	if got := b.due(t0.Add(d - time.Second)); len(got) != 0 {
		t.Errorf("lifted early: %v", got)
	}
	if got := b.due(t0.Add(d)); len(got) != 1 || got[0] != peer {
		t.Fatalf("due = %v", got)
	}
	if got := b.due(t0.Add(d + time.Minute)); len(got) != 0 {
		t.Errorf("lifted twice: %v", got)
	}

	// It keeps failing: saeMaxLifts early lifts, then none.
	now := t0
	for i := 1; i < saeMaxLifts; i++ {
		now = now.Add(2 * time.Minute)
		if _, ok := b.blocked(peer, now, 0); !ok {
			t.Fatalf("block %d: no early lift", i+1)
		}
		if got := b.due(now.Add(saeLiftMin)); len(got) != 1 {
			t.Fatalf("block %d: due = %v", i+1, got)
		}
	}
	now = now.Add(2 * time.Minute)
	if _, ok := b.blocked(peer, now, 0); ok {
		t.Error("a peer that failed through every early lift got another")
	}
	if got := b.due(now.Add(time.Hour)); len(got) != 0 {
		t.Errorf("due after the lifts ran out: %v", got)
	}
	// An hour on it is tried afresh, and so is a peer that authenticated.
	if _, ok := b.blocked(peer, t0.Add(saeLiftWindow+time.Minute), 0); !ok {
		t.Error("no early lift after the window passed")
	}
	other := "a0:b0:c0:00:01:f1"
	for i := 0; i < saeMaxLifts; i++ {
		b.blocked(other, now, 0)
		b.due(now.Add(saeLiftMin))
	}
	b.connected(other)
	if _, ok := b.blocked(other, now.Add(time.Minute), 0); !ok {
		t.Error("a peer that authenticated in between did not start anew")
	}
	b.reset()
	if got := b.due(now.Add(24 * time.Hour)); len(got) != 0 {
		t.Errorf("due after a reset: %v", got)
	}
}
