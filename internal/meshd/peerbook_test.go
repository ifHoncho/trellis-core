package meshd

import (
	"sort"
	"testing"
	"time"
)

func TestPeerBook(t *testing.T) {
	var b peerBook
	t0 := time.Unix(1_800_000_000, 0)
	const p, q = "a0:b0:c0:00:00:01", "a0:b0:c0:00:00:02"

	if b.known(p, t0) {
		t.Fatal("a peer never seen is known")
	}
	b.established(p, t0)
	if !b.known(p, t0.Add(knownPeerFor-time.Second)) {
		t.Error("a peer seen established is forgotten too soon")
	}
	if b.known(p, t0.Add(knownPeerFor+time.Second)) {
		t.Error("a peer gone for long stays known")
	}

	// A connection is checked once, after the delay, and makes the peer
	// known.
	b.connected(q, t0)
	if !b.known(q, t0) {
		t.Error("a connected peer is not known")
	}
	if due := b.dueChecks(t0.Add(connectedCheckAfter - time.Millisecond)); len(due) != 0 {
		t.Errorf("checked too early: %v", due)
	}
	b.connected(p, t0.Add(time.Second))
	due := b.dueChecks(t0.Add(connectedCheckAfter + time.Second))
	sort.Strings(due)
	if len(due) != 2 || due[0] != p || due[1] != q {
		t.Errorf("due = %v", due)
	}
	if again := b.dueChecks(t0.Add(time.Hour)); len(again) != 0 {
		t.Errorf("checked twice: %v", again)
	}

	// A phantom is dropped at most once a minute, and a refused one is
	// checked again when it may be.
	if !b.mayReset(p, t0) || b.mayReset(p, t0.Add(20*time.Second)) {
		t.Error("phantom resets not limited to one a minute")
	}
	if next := b.nextCheck(); !next.Equal(t0.Add(phantomResetEvery)) {
		t.Errorf("refused reset rechecked at %v, want %v", next, t0.Add(phantomResetEvery))
	}
	if due := b.dueChecks(t0.Add(phantomResetEvery)); len(due) != 1 || due[0] != p || !b.mayReset(p, t0.Add(phantomResetEvery)) {
		t.Errorf("recheck: due %v", due)
	}

	b.reset()
	if b.known(p, t0.Add(2*time.Second)) || len(b.dueChecks(t0.Add(time.Hour))) != 0 {
		t.Error("reset kept something")
	}
}
