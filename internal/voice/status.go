package voice

import (
	"sort"
	"time"
)

// Status is the engine's state for the API and the UI.
type Status struct {
	// Version is trellis-voiced's own (set by voiced, not the engine):
	// meshd compares it with its own.
	Version  string          `json:"version,omitempty"`
	Groups   []GroupStatus   `json:"groups"`
	Channels []ChannelStatus `json:"channels"`
	Events   []Event         `json:"events"`
	// MicDBFS is the microphone's level over the last 200 ms (dBFS; -96
	// for silence); Loopback whether the mic is played back here.
	MicDBFS float64 `json:"mic_dbfs"`
	// MicDead: the first microphone has given exact digital silence for
	// over 3 s: no microphone behind it (a board's jack without one), or a
	// headset that gates its own silence.
	MicDead bool `json:"mic_dead,omitempty"`
	// AGCDB is the gain the microphone's AGC applies now (when on).
	AGCDB    float64    `json:"agc_db"`
	Loopback bool       `json:"loopback"`
	Audio    *AudioInfo `json:"audio,omitempty"`
	// Check is the PTT check under way or just done.
	Check *CheckStatus `json:"check,omitempty"`
	// FECLossPerc is the loss the encoder now plans its redundancy for.
	FECLossPerc int `json:"fec_loss_perc"`
	// Consoles are the browsers monitoring through this node.
	Consoles []ConsoleStatus `json:"consoles"`
}

// AudioInfo is the audio hardware trellis-voiced found and uses.
type AudioInfo struct {
	Device string `json:"device"` // the ID in use, or "none", or "files"
	Name   string `json:"name,omitempty"`
	Model  string `json:"model,omitempty"` // a known adapter, as OpenVLM
	PTT    string `json:"ptt"`             // the PTT source in use, or "none"
	Mono   bool   `json:"mono,omitempty"`
	// ButtonHold: a channel's PTT is a headset button in hold-to-talk,
	// which cannot work (the press shorts the microphone).
	ButtonHold []string `json:"button_hold,omitempty"`
	// NoMic: the device in use shows no microphone now (it plays only).
	NoMic bool `json:"no_mic,omitempty"`
	// NoPTT: channel A's PTT is automatic and the device offers none (no
	// CM108 pins, no headset button).
	NoPTT bool `json:"no_ptt,omitempty"`
	// Levels are the card's mixer controls as voiced set them.
	Levels  []MixerLevel  `json:"levels,omitempty"`
	Devices []AudioDevice `json:"devices"`
}

// AudioDevice is a sound card the node has.
type AudioDevice struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Model    string `json:"model,omitempty"`
	Capture  bool   `json:"capture"`
	Playback bool   `json:"playback"`
	HID      bool   `json:"hid"`    // a CM108 (C-Media) HID interface: PTT and radio key on its pins
	Button   bool   `json:"button"` // input devices: a headset button can be the PTT
}

type GroupStatus struct {
	Number    int    `json:"number"`
	Name      string `json:"name"`
	Addr      string `json:"addr"`
	Listening bool   `json:"listening"`
	Busy      bool   `json:"busy"`
	Talker    string `json:"talker,omitempty"`   // callsign, or the SSRC in hex before it is known
	AllCall   bool   `json:"all_call,omitempty"` // the talker is an all-call
	Sending   bool   `json:"sending"`
	Received  uint64 `json:"received"`
	Lost      uint64 `json:"lost"`
	Late      uint64 `json:"late"`
}

type ChannelStatus struct {
	Name    string  `json:"name"`
	Group   int     `json:"group"`
	Ear     Ear     `json:"ear"`
	Volume  int     `json:"volume"`
	PTT     bool    `json:"ptt"`
	Sending bool    `json:"sending"`
	OutDBFS float64 `json:"out_dbfs"` // what it plays, last 200 ms
	// MicCut: a talk was stopped because the microphone was dead while
	// the PTT was held (a PTT that shorts the mic); cleared by a good talk.
	MicCut bool `json:"mic_cut,omitempty"`
	// LocalTalker: someone on this node talks on the channel's group now
	// (a console, or the other channel); what the channel plays is that.
	LocalTalker string `json:"local_talker,omitempty"`
}

// Status reports the engine's state.
func (e *Engine) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	used := map[int]bool{}
	for _, n := range e.used() {
		used[n] = true
	}
	var st Status
	for _, g := range e.cfg.Groups {
		gs := e.groups[g.Number]
		s := GroupStatus{Number: g.Number, Name: g.Name, Addr: g.Addr.String(), Listening: used[g.Number],
			Busy: now.Sub(gs.lastRx) < busyFor, Sending: gs.tx > 0, Received: gs.received, Lost: gs.lost, Late: gs.late}
		if t := gs.talker; t != nil && t.started {
			s.Talker, s.AllCall = gs.names[t.ssrc], gs.allCall[t.ssrc]
			if s.Talker == "" {
				s.Talker = "ssrc-" + hex32(t.ssrc)
			}
		}
		st.Groups = append(st.Groups, s)
	}
	for i, c := range e.cfg.Channels {
		st.Channels = append(st.Channels, ChannelStatus{Name: "ab"[i : i+1], Group: c.Group, Ear: c.Ear, Volume: c.Volume,
			PTT: e.ch[i].pttDown, Sending: e.ch[i].granted, OutDBFS: e.outLevel[i].dbfs(), MicCut: !e.cutAt[i].IsZero(), LocalTalker: e.localTalker(c.Group, now)})
	}
	st.Events = append([]Event(nil), e.events...)
	st.MicDBFS, st.Loopback = e.micLevel.dbfs(), now.Before(e.loopbackUntil)
	st.MicDead = e.zeroFrames > 150
	if e.cfg.MicAGC {
		st.AGCDB = e.agcs[0].dB()
	}
	st.FECLossPerc = e.fecPerc
	st.Check = e.checkStatus()
	st.Consoles = []ConsoleStatus{}
	for _, c := range e.consoles {
		cs := ConsoleStatus{ID: c.ID, Name: c.Name, Monitor: []int{}, Sending: c.tx.granted || c.all != nil, AllCall: c.all != nil}
		for n := range c.monitor {
			cs.Monitor = append(cs.Monitor, n)
		}
		sort.Ints(cs.Monitor)
		if c.tx.granted {
			cs.Group = c.group
		}
		st.Consoles = append(st.Consoles, cs)
	}
	if e.audio != nil {
		a := *e.audio
		st.Audio = &a
	}
	return st
}

func hex32(v uint32) string {
	const digits = "0123456789abcdef"
	b := make([]byte, 8)
	for i := 7; i >= 0; i-- {
		b[i] = digits[v&0xf]
		v >>= 4
	}
	return string(b)
}

// MixerLevel is one of a card's mixer controls.
type MixerLevel struct {
	Control string  `json:"control"`
	Kind    string  `json:"kind"`
	DB      float64 `json:"db"`
}

// localTalker names who on this node talks on group n now. Call with mu
// held.
func (e *Engine) localTalker(n int, now time.Time) string {
	if n == 0 || now.Sub(e.localAt[n]) > busyFor {
		return ""
	}
	return e.localBy[n]
}
