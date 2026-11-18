package voice

import (
	"errors"
	"sort"
	"time"
)

// Two guards for a PTT that cuts the microphone while it is held (a
// phone-style adapter's hook switch shorts the mic line): a hold-to-talk
// talk whose microphone is dead stops sending and warns the operator in
// the ear (micCutCheck); and a PTT check the operator runs (StartCheck):
// press, speak, release, hear what would have been sent, get a verdict.

const (
	cutFrames    = 50    // a talk's first second is what is judged
	cutDBFS      = -65.0 // a shorted microphone reads below this (measured -78)
	cutDropDB    = 20.0  // and the press made it drop this much (measured 30)
	checkWait    = 20 * time.Second
	checkMax     = 500 // frames recorded at most: 10 s
	checkKeep    = 2 * time.Minute
	quietDBFS    = -40.0 // speech below this while held is too quiet
	checkMinHeld = 15    // frames: a press shorter than 0.3 s is not a check
)

// SetCutWatch says which channels' PTT can short the microphone: a
// headset button (a phone-style adapter keys by shorting the mic line).
// Only those are watched; a PTT read apart from the microphone (CM108
// pins, GPIO, serial) never is.
func (e *Engine) SetCutWatch(watch [2]bool) {
	e.mu.Lock()
	e.cutWatch = watch
	e.mu.Unlock()
}

// micCutCheck looks at the raw microphone (before any gain) of each
// watched channel in hold-to-talk. A second into a talk, if the press
// made the microphone drop (its median level held is cutDropDB below its
// level at rest, and under cutDBFS), the PTT shorted it: the talk is
// released (nothing more is sent), toneMicCut plays (even with tones
// off), and the channel is marked until a talk with sound. A microphone
// that is as quiet at rest as held (a gated headset, a silent room) is
// not cut: nothing changed at the press. Call with mu held.
func (e *Engine) micCutCheck(rawDB [2]float64, now time.Time) {
	for i, c := range e.ch {
		db := rawDB[i]
		if !c.granted {
			if !c.pttDown {
				// The mic at rest: a running mean, from its first reading.
				if !e.restSeen[i] {
					e.restSeen[i], e.releasedDB[i] = true, db
				}
				e.releasedDB[i] = 0.9*e.releasedDB[i] + 0.1*db
			}
			continue
		}
		if db > cutDBFS {
			if e.soundFrames[i]++; e.soundFrames[i] == cutFrames {
				e.cutAt[i] = time.Time{} // a talk with sound: the warning goes
			}
		}
		if !e.cutWatch[i] || e.cfg.PTTMode == "toggle" || len(e.heldDB[i]) >= cutFrames {
			continue
		}
		e.heldDB[i] = append(e.heldDB[i], db)
		if len(e.heldDB[i]) < cutFrames {
			continue
		}
		held := median(e.heldDB[i])
		if held >= cutDBFS || e.releasedDB[i]-held < cutDropDB {
			continue
		}
		g := e.groups[e.cfg.Channels[i].Group]
		c.granted = false
		g.tx--
		e.sendRTCP(i, true)
		e.cutAt[i] = now
		e.tones[i].start(toneMicCut)
		e.event(Event{At: now, Group: g.g.Number, Kind: "mic-cut", SSRC: c.ssrc, Talker: e.cfg.Callsign, Channel: "ab"[i : i+1]})
		e.log.Warn("voice: the PTT silences the microphone while held; the talk is stopped. Choose press to start, press to stop, or VOX",
			"channel", "ab"[i:i+1], "mic_at_rest_dbfs", round1(e.releasedDB[i]), "mic_held_dbfs", round1(held))
	}
}

func median(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[len(s)/2]
}

// CheckStatus is a PTT check's progress and verdict.
type CheckStatus struct {
	Channel string `json:"channel"`
	// State: waiting (for a press), recording (held), playing (what
	// would have been sent, in the channel's ear), done.
	State string `json:"state"`
	// Verdict, when done: ok, cuts-mic (silent while held), quiet (too
	// low while held), short (released at once), no-ptt (no press came).
	Verdict      string  `json:"verdict,omitempty"`
	HeldDBFS     float64 `json:"held_dbfs"`
	ReleasedDBFS float64 `json:"released_dbfs"`
	Seconds      float64 `json:"seconds"`
}

type pttCheck struct {
	CheckStatus
	ch     int
	since  time.Time
	levels []float64
	frames [][]int16 // what would have been sent (after the AGC)
	play   int
}

// StartCheck begins a PTT check on a channel: its next press does not
// transmit; what the mic gives while it is held is recorded, played back
// in the channel's ear at release, and judged.
func (e *Engine) StartCheck(channel int) error {
	if channel < 0 || channel > 1 {
		return errors.New("no such channel")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ch[channel].granted {
		return errors.New("the channel is talking")
	}
	e.check = &pttCheck{ch: channel, since: e.now(), CheckStatus: CheckStatus{Channel: "ab"[channel : channel+1], State: "waiting"}}
	return nil
}

// CheckRecording reports whether a PTT check holds the channel's press
// now: the press was taken by the check, not refused. A press-to-start
// PTT stays "on" through it, so that its next press ends the check.
func (e *Engine) CheckRecording(channel int) bool {
	if channel < 0 || channel > 1 {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.check != nil && e.check.ch == channel && e.check.State == "recording"
}

// checkPTT takes a PTT press or release that a check is waiting for:
// it reports true when the check took it (nothing is transmitted). Call
// with mu held.
func (e *Engine) checkPTT(channel int, down bool) bool {
	k := e.check
	if k == nil || k.ch != channel {
		return false
	}
	switch {
	case down && k.State == "waiting":
		k.State, k.since = "recording", e.now()
		return true
	case !down && k.State == "recording":
		e.finishCheck(k)
		return true
	}
	return false
}

// checkTick records, plays back and times out. raw is the mic before the
// AGC (for its level), sent what would have been sent. Call with mu held.
func (e *Engine) checkTick(raw, sent []int16, now time.Time) {
	k := e.check
	if k == nil {
		return
	}
	switch k.State {
	case "waiting":
		if now.Sub(k.since) > checkWait {
			k.State, k.Verdict = "done", "no-ptt"
			k.ReleasedDBFS = round1(e.releasedDB[k.ch])
		}
	case "recording":
		var l level
		l.add(raw, now)
		k.levels = append(k.levels, l.dbfs())
		k.frames = append(k.frames, append([]int16(nil), sent...))
		if len(k.frames) >= checkMax {
			e.finishCheck(k)
		}
	case "playing":
		if k.play < len(k.frames) {
			e.checkOut = k.frames[k.play]
			k.play++
		} else {
			k.State = "done"
		}
	case "done":
		if now.Sub(k.since) > checkKeep {
			e.check = nil
		}
	}
}

func (e *Engine) finishCheck(k *pttCheck) {
	k.Seconds = round1(float64(len(k.frames)) * FrameMs / 1000)
	k.ReleasedDBFS = round1(e.releasedDB[k.ch])
	var med float64
	if len(k.levels) > 0 {
		s := append([]float64(nil), k.levels...)
		sort.Float64s(s)
		k.HeldDBFS = round1(s[len(s)*9/10]) // the loud end: speech, not pauses
		med = s[len(s)/2]                   // the bulk: a press click does not count
	}
	switch {
	case len(k.frames) < checkMinHeld:
		k.Verdict = "short"
	case med < cutDBFS && k.HeldDBFS < -50 && e.releasedDB[k.ch]-med >= cutDropDB:
		k.HeldDBFS = round1(med)
		k.Verdict = "cuts-mic"
	case k.HeldDBFS < quietDBFS:
		k.Verdict = "quiet"
	default:
		k.Verdict = "ok"
	}
	k.State, k.since = "playing", e.now()
	if k.Verdict == "short" {
		k.State = "done"
	}
}

func round1(v float64) float64 { return float64(int(v*10)) / 10 }

// Status's view of the check.
func (e *Engine) checkStatus() *CheckStatus {
	if e.check == nil {
		return nil
	}
	s := e.check.CheckStatus
	return &s
}
