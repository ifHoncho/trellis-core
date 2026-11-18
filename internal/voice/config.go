// Package voice is the push-to-talk engine of trellis-voiced
// (architecture §13, Voice): talk groups as Opus over multicast RTP, two
// channels, half-duplex per group, tones. Audio devices and the network
// are interfaces, so the engine runs the same on a node and in tests.
package voice

import (
	"fmt"
	"net/netip"
)

// Audio format: Opus at 48 kHz, mono, 20 ms frames.
const (
	SampleRate  = 48000
	FrameMs     = 20
	FrameLen    = SampleRate * FrameMs / 1000 // samples per channel in a frame
	PayloadType = 111
	// InitialLossPerc is the loss the encoder plans FEC for at start;
	// the engine then follows the loss it hears (adaptFEC).
	InitialLossPerc = 20
	maxGroups       = 16
)

// DefaultAddr is the default multicast address and port of talk group n
// (1–16): 239.73.80.n, port 38799+2n. RTCP uses the port after it.
func DefaultAddr(n int) netip.AddrPort {
	return netip.AddrPortFrom(netip.AddrFrom4([4]byte{239, 73, 80, byte(n)}), uint16(38799+2*n))
}

// Group is a talk group.
type Group struct {
	Number   int            `yaml:"number" json:"number"`
	Name     string         `yaml:"name" json:"name"`
	Addr     netip.AddrPort `yaml:"-" json:"addr"`
	Priority bool           `yaml:"priority,omitempty" json:"priority,omitempty"`
}

// Ear is where a channel plays.
type Ear string

const (
	Left  Ear = "left"
	Right Ear = "right"
	Both  Ear = "both"
)

// Channel is one of the node's two channels: a group, an ear and a volume,
// and optionally a device and PTT of its own (empty: the node's).
type Channel struct {
	Group  int    `yaml:"group" json:"group"` // 0: off
	Ear    Ear    `yaml:"ear" json:"ear"`
	Volume int    `yaml:"volume" json:"volume"` // 0–100
	Device string `yaml:"device,omitempty" json:"device,omitempty"`
	PTT    string `yaml:"ptt,omitempty" json:"ptt,omitempty"`
	// Key is an output keyed while the channel's group plays: a radio
	// bridged into the group (RoIP), its carrier sense as the PTT.
	Key string `yaml:"key,omitempty" json:"key,omitempty"`
}

// Config is the engine's configuration.
type Config struct {
	Callsign string     `json:"callsign"`
	Groups   []Group    `json:"groups"`
	Channels [2]Channel `json:"channels"` // A, B
	Listen   []int      `json:"listen,omitempty"`
	Volume   int        `json:"listen_volume"` // listen-only groups, 0–100
	Bitrate  int        `json:"bitrate"`
	Tones    bool       `json:"tones"`
	// JitterMs is how much audio a receiver holds before it plays a talker.
	JitterMs int `json:"jitter_ms"`
	// Device and PTT: the node's audio device and PTT input (node.yaml's
	// voice.device and voice.ptt); trellis-voiced opens them.
	Device  string `json:"device,omitempty"`
	PTT     string `json:"ptt,omitempty"`
	MicGain int    `json:"mic_gain,omitempty"`
	// PTTMode: hold (talk while pressed; the default) or toggle (press to
	// start, press to stop: for a PTT that silences the mic while pressed,
	// as a phone-style adapter's does). Only ever chosen, never automatic.
	PTTMode string `json:"ptt_mode,omitempty"`
	// MicAGC brings speech from the node's microphones to a steady level.
	MicAGC   bool `json:"mic_agc,omitempty"`
	Sidetone bool `json:"sidetone,omitempty"`
}

// Defaults fills what is unset.
func (c *Config) Defaults() {
	if c.Bitrate == 0 {
		c.Bitrate = 32000
	}
	if c.JitterMs == 0 {
		c.JitterMs = 60
	}
	for i := range c.Groups {
		if !c.Groups[i].Addr.IsValid() {
			c.Groups[i].Addr = DefaultAddr(c.Groups[i].Number)
		}
	}
	for i := range c.Channels {
		if c.Channels[i].Ear == "" {
			c.Channels[i].Ear = Both
		}
	}
	// A group is listened to once: one that is a channel's, or is named
	// twice, would be mixed twice, the second time into both ears.
	taken := map[int]bool{c.Channels[0].Group: true, c.Channels[1].Group: true}
	var listen []int
	for _, n := range c.Listen {
		if !taken[n] {
			taken[n] = true
			listen = append(listen, n)
		}
	}
	c.Listen = listen
}

// Validate checks the configuration, naming the field at fault.
func (c *Config) Validate() error {
	if len(c.Groups) > maxGroups {
		return fmt.Errorf("groups: %d, at most %d", len(c.Groups), maxGroups)
	}
	seen := map[int]bool{}
	addrs := map[netip.AddrPort]int{}
	for _, g := range c.Groups {
		if g.Number < 1 || g.Number > maxGroups {
			return fmt.Errorf("groups: number %d is not 1–%d", g.Number, maxGroups)
		}
		if seen[g.Number] {
			return fmt.Errorf("groups: number %d appears twice", g.Number)
		}
		seen[g.Number] = true
		if !g.Addr.Addr().Is4() || !g.Addr.Addr().IsMulticast() || g.Addr.Port() == 0 || g.Addr.Port() == 65535 {
			return fmt.Errorf("groups: %d: %v is not an IPv4 multicast address and port (RTCP needs the port after it)", g.Number, g.Addr)
		}
		for _, p := range []netip.AddrPort{g.Addr, netip.AddrPortFrom(g.Addr.Addr(), g.Addr.Port()+1)} {
			if o, ok := addrs[p]; ok {
				return fmt.Errorf("groups: %d and %d share %v", o, g.Number, p)
			}
			addrs[p] = g.Number
		}
	}
	for i, ch := range c.Channels {
		name := "ab"[i : i+1]
		if ch.Group != 0 && !seen[ch.Group] {
			return fmt.Errorf("channels.%s.group: no talk group %d", name, ch.Group)
		}
		if ch.Ear != Left && ch.Ear != Right && ch.Ear != Both {
			return fmt.Errorf("channels.%s.ear: %q is not left, right or both", name, ch.Ear)
		}
		if ch.Volume < 0 || ch.Volume > 100 {
			return fmt.Errorf("channels.%s.volume: %d is not 0–100", name, ch.Volume)
		}
	}
	if c.Channels[0].Group != 0 && c.Channels[0].Group == c.Channels[1].Group {
		return fmt.Errorf("channels: a and b are both on group %d", c.Channels[0].Group)
	}
	for _, n := range c.Listen {
		if !seen[n] {
			return fmt.Errorf("listen: no talk group %d", n)
		}
	}
	if c.Bitrate < 6000 || c.Bitrate > 128000 {
		return fmt.Errorf("bitrate: %d is not 6000–128000", c.Bitrate)
	}
	if c.JitterMs < FrameMs || c.JitterMs > 400 {
		return fmt.Errorf("jitter_ms: %d is not %d–400", c.JitterMs, FrameMs)
	}
	return nil
}
