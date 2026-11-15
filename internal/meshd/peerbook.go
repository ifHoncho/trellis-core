package meshd

import (
	"sync"
	"time"
)

// peerBook is what one mesh link knows of its peers since its
// wpa_supplicant started.
//
//   - Which peers have been established on the link, and when last. Such a
//     peer that falls silent is restarting or out of range for a moment, not
//     a node on another channel: a failed SAE exchange with it is a real
//     failure (its block is lifted early, saeblock.go), and its entry is
//     never removed as a stray candidate (peermove.go). Without this a peer
//     that restarted was taken for one heard while scanning, blocked for
//     five minutes and its entry removed while it was peering again.
//   - Which peers wpa_supplicant has just reported connected, to be checked
//     against the kernel (resetPhantomPeers): the two can disagree when the
//     kernel's entry goes during the exchange, and wpa_supplicant then holds
//     a link the kernel does not, ignoring the peer from then on.
type peerBook struct {
	mu      sync.Mutex
	seen    map[string]time.Time // established, last seen so
	checkAt map[string]time.Time // reported connected; check from then
	resetAt map[string]time.Time // last dropped as a phantom (resetPhantomPeers)
}

const (
	// knownPeerFor is how long a peer stays known after it was last seen
	// established: longer than a node's restart takes (a minute or two,
	// with a slow USB radio), and short, because a known peer that really
	// moved to another channel keeps its stale entry and its SAE retries
	// for that long.
	knownPeerFor = 3 * time.Minute
	// connectedCheckAfter is how long after wpa_supplicant reports a peer
	// connected the kernel must hold it established.
	connectedCheckAfter = 3 * time.Second
	// phantomResetEvery limits how often one peer is dropped as a phantom,
	// should the kernel keep refusing it.
	phantomResetEvery = time.Minute
)

// mayReset says whether a phantom peer may be dropped now, and if so
// records that it was. When not, the check comes back once it may
// (recheck), so a peer that turns phantom twice in a minute is not lost.
func (b *peerBook) mayReset(mac string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if t, ok := b.resetAt[mac]; ok && now.Sub(t) < phantomResetEvery {
		b.recheckLocked(mac, t.Add(phantomResetEvery))
		return false
	}
	if b.resetAt == nil {
		b.resetAt = map[string]time.Time{}
	}
	b.resetAt[mac] = now
	return true
}

// established records a peer seen established on the link.
func (b *peerBook) established(mac string, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.seen == nil {
		b.seen = map[string]time.Time{}
	}
	b.seen[mac] = now
}

// known says whether the peer has been established on the link lately.
func (b *peerBook) known(mac string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.seen[mac]
	return ok && now.Sub(t) < knownPeerFor
}

// connected records wpa_supplicant's report that a peer is connected.
func (b *peerBook) connected(mac string, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.seen == nil {
		b.seen = map[string]time.Time{}
	}
	if b.checkAt == nil {
		b.checkAt = map[string]time.Time{}
	}
	b.seen[mac] = now
	b.checkAt[mac] = now.Add(connectedCheckAfter)
}

// recheck schedules another check of a peer at a given time.
func (b *peerBook) recheck(mac string, at time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.recheckLocked(mac, at)
}

func (b *peerBook) recheckLocked(mac string, at time.Time) {
	if b.checkAt == nil {
		b.checkAt = map[string]time.Time{}
	}
	if t, ok := b.checkAt[mac]; !ok || at.Before(t) {
		b.checkAt[mac] = at
	}
}

// nextCheck is when the earliest pending check falls due (zero: none).
func (b *peerBook) nextCheck() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	var next time.Time
	for _, t := range b.checkAt {
		if next.IsZero() || t.Before(next) {
			next = t
		}
	}
	return next
}

// disconnected forgets a pending check: a peer that left is no phantom.
func (b *peerBook) disconnected(mac string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.checkAt, mac)
}

// dueChecks returns the peers whose check is due, and forgets them.
func (b *peerBook) dueChecks(now time.Time) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for mac, t := range b.checkAt {
		if !now.Before(t) {
			out = append(out, mac)
			delete(b.checkAt, mac)
		}
	}
	return out
}

// reset forgets everything: wpa_supplicant (re)started on a new interface.
func (b *peerBook) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seen, b.checkAt, b.resetAt = nil, nil, nil
}
