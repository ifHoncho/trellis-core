package main

import (
	"context"
	"encoding/json"
	"math/rand"
	"time"

	"meridian/trellis/internal/api"
	"meridian/trellis/internal/voice"
)

// Made-up push-to-talk: talkers come and go on the node's groups, an
// OpenVLM is plugged in, the tests answer.

var (
	fakeLoopback time.Time
	fakeCheckAt  time.Time // a PTT check: waiting 3 s, held 3 s, played 3 s, then "cuts-mic"
	fakeCheckCh  string
)

func (f *fake) VoiceConfig() api.VoiceConfig {
	return api.VoiceConfig{Enable: f.node.Voice.Enable, Config: []byte("{}")}
}

func (f *fake) Voice(context.Context) (*api.VoiceState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, p := f.node.Voice, f.profile.Voice
	st := &api.VoiceState{Enabled: n.Enable}
	if !n.Enable {
		return st, nil
	}
	st.Running = true
	now := time.Now()
	sec := now.Unix()
	// Group 2's talker speaks 10 s of every 15; group 1's 3 s of every 40;
	// the listen group's now and then.
	talking := map[int]string{}
	if sec%15 < 10 {
		talking[2] = "bravo-3"
	}
	if sec%40 < 3 {
		talking[1] = "cmd-post"
	}
	if sec%60 > 50 {
		talking[4] = "medic-2"
	}
	used := map[int]bool{n.Channels.A.Group: true, n.Channels.B.Group: true}
	for _, g := range n.Listen {
		used[g] = true
	}
	var vs voice.Status
	for _, g := range p.Groups {
		ap, _ := g.VoiceAddr()
		gs := voice.GroupStatus{Number: g.Number, Name: g.Name, Addr: ap.String(), Listening: used[g.Number],
			Received: uint64(1200*g.Number) + uint64(sec%1000), Lost: uint64(g.Number % 3)}
		if t, ok := talking[g.Number]; ok && used[g.Number] {
			gs.Talker, gs.Busy = t, true
		}
		vs.Groups = append(vs.Groups, gs)
	}
	vol := func(v *int, d int) int {
		if v == nil {
			return d
		}
		return *v
	}
	for i, c := range []struct {
		g int
		e string
		v *int
	}{{n.Channels.A.Group, n.Channels.A.Ear, n.Channels.A.Volume}, {n.Channels.B.Group, n.Channels.B.Ear, n.Channels.B.Volume}} {
		ear := voice.Ear(c.e)
		if ear == "" {
			ear = voice.Both
		}
		out := -96.0
		if _, ok := talking[c.g]; ok {
			out = -18 - rand.Float64()*8
		}
		vs.Channels = append(vs.Channels, voice.ChannelStatus{Name: "ab"[i : i+1], Group: c.g, Ear: ear, Volume: vol(c.v, 80), OutDBFS: out})
	}
	vs.MicDBFS = -62 + rand.Float64()*6
	vs.Loopback = now.Before(fakeLoopback)
	if vs.Loopback {
		vs.MicDBFS = -24 + rand.Float64()*8
	}
	vs.Audio = &voice.AudioInfo{Device: "usb:0d8c:0012@1-1.3", Name: "OpenVLM", Model: "OpenVLM", PTT: "cm108:3", Mono: true,
		Devices: []voice.AudioDevice{
			{ID: "onboard:Headphones", Name: "Headphones", Playback: true},
			{ID: "usb:0d8c:0012@1-1.3", Name: "OpenVLM", Model: "OpenVLM", Capture: true, Playback: true, HID: true},
		}}
	if !fakeCheckAt.IsZero() {
		k := &voice.CheckStatus{Channel: fakeCheckCh, HeldDBFS: -78.4, ReleasedDBFS: -47.9, Seconds: 2.9}
		switch e := now.Sub(fakeCheckAt); {
		case e < 3*time.Second:
			k.State = "waiting"
		case e < 6*time.Second:
			k.State = "recording"
		case e < 9*time.Second:
			k.State = "playing"
		default:
			k.State, k.Verdict = "done", "cuts-mic"
		}
		vs.Check = k
	}
	vs.Events = []voice.Event{{At: now.Add(-40 * time.Second), Group: 2, Kind: "talk-heard", Talker: "bravo-3"}}
	st.Status, _ = json.Marshal(vs)
	return st, nil
}

func (f *fake) VoiceTest(_ context.Context, in api.VoiceTestInput) error {
	switch in.Kind {
	case "loopback":
		fakeLoopback = time.Now().Add(10 * time.Second)
	case "stop":
		fakeLoopback = time.Time{}
	case "ptt":
		fakeCheckAt, fakeCheckCh = time.Now(), in.Channel
	}
	return nil
}
