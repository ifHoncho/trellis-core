package meshd

import (
	"time"

	"meridian/trellis/internal/config"
)

// A mesh interface can leave its mesh in the kernel while wpa_supplicant
// still reports it joined: a driver that fails to follow a peer's channel
// switch ("failed to finalize CSA, disconnecting") does this, and the link then
// stays dead with nothing in wpa_supplicant's state to show it. The
// kernel's side is the interface's channel: none means not in a mesh.

const (
	// kernelLeftAfter: how long the kernel may show no channel while
	// wpa_supplicant says joined (a join or a switch in progress) before
	// meshd acts.
	kernelLeftAfter = 15 * time.Second
	// kernelRecreateWithin: a loss before a channel has held this long
	// after an action means restarting wpa_supplicant was not enough:
	// the interface is deleted and made again, which resets the driver's
	// state for it.
	kernelRecreateWithin = 5 * time.Minute
)

type kernelAction int

const (
	kernelOK kernelAction = iota
	kernelRestart
	kernelRecreate
)

// kernelWatchMaxWait: a fault that action does not clear is acted on
// less and less often (the wait doubles with each action in a row), up
// to this.
const kernelWatchMaxWait = 10 * time.Minute

// kernelWatch follows one mesh interface.
type kernelWatch struct {
	offSince time.Time
	// actions in a row with no channel held kernelRecreateWithin between:
	// each doubles the wait before the next. upSince: since when the
	// channel has been shown without a break, after the last action.
	actions int
	upSince time.Time
	// seen: the kernel has shown a channel for this link at some point
	// (kept across restarts: the radio's driver reports one). Only then
	// does none mean the mesh was left; a driver that never reports one
	// for a mesh interface is left alone.
	seen bool
}

// observe takes wpa_supplicant's view (joined) and the kernel's (the
// interface's frequency, 0 for none) and says what to do.
func (w *kernelWatch) observe(now time.Time, joined bool, kernelFreqMHz int) kernelAction {
	if !joined {
		w.offSince, w.upSince = time.Time{}, time.Time{} // not joined is not "held"
		return kernelOK
	}
	if kernelFreqMHz != 0 || !w.seen {
		w.offSince = time.Time{}
		w.seen = w.seen || kernelFreqMHz != 0
		if kernelFreqMHz != 0 {
			if w.upSince.IsZero() {
				w.upSince = now
			}
			if now.Sub(w.upSince) >= kernelRecreateWithin {
				w.actions = 0 // held long enough: fixed
			}
		}
		return kernelOK
	}
	w.upSince = time.Time{}
	if w.offSince.IsZero() {
		w.offSince = now
		return kernelOK
	}
	wait := kernelLeftAfter << min(w.actions, 6)
	if wait > kernelWatchMaxWait {
		wait = kernelWatchMaxWait
	}
	if now.Sub(w.offSince) < wait {
		return kernelOK
	}
	w.actions++
	// The first action of a run restarts wpa_supplicant; a loss after it
	// (no channel held kernelRecreateWithin in between) recreates the
	// interface, however long the backoff has grown.
	act := kernelRestart
	if w.actions > 1 {
		act = kernelRecreate
	}
	w.offSince = time.Time{}
	return act
}

// reset forgets a loss in progress (wpa_supplicant restarted for another
// reason); the last action is kept.
func (w *kernelWatch) reset() { w.offSince = time.Time{} }

// A link can also be pulled off its channel. A mesh radio follows any
// peer's channel switch announcement, in the kernel: a peer that hears
// radar moves the mesh that way, and a peer whose driver announces a
// switch and never makes it moves every node that follows to a channel
// the rest of the mesh is not on. Trellis announces none itself. The
// kernel then shows a channel the plan does not have.

const (
	offChannelAfter = 30 * time.Second // on another channel this long …
	offChannelEvery = 10 * time.Minute // … is put back at most this often
)

// chanWatch follows one mesh interface's channel against its plan.
type chanWatch struct {
	offSince time.Time
	lastFix  time.Time
}

// observe takes wpa_supplicant's view (joined), the kernel's frequency
// and the plan's. off says the link has been on another channel for
// offChannelAfter; rejoin, that it is time to restart it on the plan's.
// On a radar plan (radar) the link is never moved back: the move may be
// the mesh's own radar move, and the plan's channel is then out of use
// for half an hour.
func (w *chanWatch) observe(now time.Time, joined bool, kernelMHz, planMHz int, radar bool) (off, rejoin bool) {
	if !joined || kernelMHz == 0 || planMHz == 0 || kernelMHz == planMHz {
		w.offSince = time.Time{}
		return false, false
	}
	if w.offSince.IsZero() {
		w.offSince = now
	}
	if now.Sub(w.offSince) < offChannelAfter {
		return false, false
	}
	if radar || (!w.lastFix.IsZero() && now.Sub(w.lastFix) < offChannelEvery) {
		return true, false
	}
	w.offSince, w.lastFix = time.Time{}, now
	return true, true
}

// radarPlan reports whether a link's plan covers a radar (DFS) channel:
// 5 GHz channels 52 to 144 nearly everywhere, and the 160 MHz block of
// 36 to 64. By channel number, not the radio's regulatory flags: where
// they differ the guard only holds back.
func radarPlan(rp config.RadioPlan) bool {
	if rp.Band != config.Band5G {
		return false
	}
	return (rp.Channel >= 52 && rp.Channel <= 144) || (rp.Width >= 160 && rp.Channel < 52)
}
