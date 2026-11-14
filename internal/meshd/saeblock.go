package meshd

import (
	"sync"
	"time"
)

// wpa_supplicant blocks a mesh peer for five minutes after four failed SAE
// attempts, and ignores that peer's authentication frames meanwhile. A
// failed attempt is not always a wrong passphrase: it is also a peer that
// did not answer, because it had just left the channel, was restarting, or
// was itself blocking this node. That last case feeds itself. A blocks B;
// B's attempts go unanswered, so B blocks A; A's block ends, its attempts
// go unanswered, so it blocks B again; and so on for as long as the two
// blocks stay out of step, which is for good. Two nodes with the same
// profile then never peer again (after one of them changed band and came
// back, for example).
//
// meshd therefore lifts a block itself after a short, random time: shorter
// than one round of attempts, so that whichever side tries next finds the
// other listening. A peer that keeps failing is given saeMaxLifts early
// lifts in saeLiftWindow and then left to wpa_supplicant's own five
// minutes: that is a node with another passphrase, and asking it faster
// only costs airtime and log lines.
const (
	saeLiftMin    = 20 * time.Second
	saeLiftSpread = 20 * time.Second // the lift comes saeLiftMin plus up to this much after the block
	saeMaxLifts   = 5
	saeLiftWindow = time.Hour
)

// saeBlocks tracks the peers wpa_supplicant has blocked on one mesh link.
type saeBlocks struct {
	mu    sync.Mutex
	peers map[string]*saeBlock
}

type saeBlock struct {
	liftAt time.Time // when to lift the block; zero when none is pending
	lifts  int       // early lifts given since first
	first  time.Time
}

// blocked records that wpa_supplicant blocked a peer. It returns how long
// to wait before lifting the block, or false when the peer has used up
// its early lifts. spread is a random share of saeLiftSpread.
func (b *saeBlocks) blocked(addr string, now time.Time, spread time.Duration) (time.Duration, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.peers == nil {
		b.peers = map[string]*saeBlock{}
	}
	p := b.peers[addr]
	if p == nil || now.Sub(p.first) > saeLiftWindow {
		p = &saeBlock{first: now}
		b.peers[addr] = p
	}
	if p.lifts >= saeMaxLifts {
		p.liftAt = time.Time{}
		return 0, false
	}
	delay := saeLiftMin + spread
	p.liftAt = now.Add(delay)
	return delay, true
}

// connected forgets a peer that authenticated: its next block starts anew.
func (b *saeBlocks) connected(addr string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.peers, addr)
}

// due returns the peers whose block is to be lifted now, counting the lift
// against each.
func (b *saeBlocks) due(now time.Time) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for addr, p := range b.peers {
		if !p.liftAt.IsZero() && !now.Before(p.liftAt) {
			p.liftAt = time.Time{}
			p.lifts++
			out = append(out, addr)
		}
	}
	return out
}

// reset forgets everything: wpa_supplicant restarted and holds no blocks.
func (b *saeBlocks) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.peers = nil
}
