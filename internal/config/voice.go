package config

import (
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

// Voice: push-to-talk talk groups (architecture §13, Voice). The groups are
// mesh-wide, in the profile; what a node does with them (its audio
// device, PTT, channels) is in its node config.

// VoiceMaxGroups is how many talk groups a mesh can have.
const VoiceMaxGroups = 16

// VoicePlan is the profile's voice section.
type VoicePlan struct {
	Groups  []VoiceGroup `yaml:"groups,omitempty" json:"groups,omitempty"`
	Bitrate int          `yaml:"bitrate,omitempty" json:"bitrate,omitempty"` // Opus bit/s; default 32000
}

// VoiceGroup is a talk group.
type VoiceGroup struct {
	Number   int    `yaml:"number" json:"number"` // 1–16
	Name     string `yaml:"name" json:"name"`
	Address  string `yaml:"address,omitempty" json:"address,omitempty"` // IPv4 multicast; default 239.73.80.<number>
	Port     int    `yaml:"port,omitempty" json:"port,omitempty"`       // RTP; RTCP is the next; default 38799+2×number
	Priority bool   `yaml:"priority,omitempty" json:"priority,omitempty"`
}

// VoiceAddr is the group's address and port, defaults filled.
func (g VoiceGroup) VoiceAddr() (netip.AddrPort, error) {
	a := netip.AddrFrom4([4]byte{239, 73, 80, byte(g.Number)})
	if g.Address != "" {
		var err error
		if a, err = netip.ParseAddr(g.Address); err != nil {
			return netip.AddrPort{}, err
		}
	}
	port := g.Port
	if port == 0 {
		port = 38799 + 2*g.Number
	}
	return netip.AddrPortFrom(a, uint16(port)), nil
}

// VoiceNode is a node's voice section.
type VoiceNode struct {
	Enable bool `yaml:"enable,omitempty" json:"enable"`
	// Device: auto (the best one found: a known adapter such as an
	// OpenVLM, then a CM108, then any USB audio, then onboard), or
	// usb:<vendor>:<product>@<port path>, or onboard:<ALSA card id>.
	Device string `yaml:"device,omitempty" json:"device,omitempty"`
	// PTT: auto (from the device's model), none, vox, cm108:<pin>,
	// gpio:<chip>:<line>, serial:<device>:<cts|dsr|dcd>.
	PTT string `yaml:"ptt,omitempty" json:"ptt,omitempty"`
	// PTTMode: hold (the default) or toggle (press to start, press to
	// stop: for a PTT that silences the mic while held).
	PTTMode      string        `yaml:"ptt_mode,omitempty" json:"ptt_mode,omitempty"`
	Channels     VoiceChannels `yaml:"channels,omitempty" json:"channels"`
	Listen       []int         `yaml:"listen,omitempty" json:"listen,omitempty"`
	ListenVolume *int          `yaml:"listen_volume,omitempty" json:"listen_volume,omitempty"` // default 60
	MicGain      int           `yaml:"mic_gain,omitempty" json:"mic_gain,omitempty"`           // dB, -20–20
	MicAGC       *bool         `yaml:"mic_agc,omitempty" json:"mic_agc,omitempty"`             // default on
	Sidetone     bool          `yaml:"sidetone,omitempty" json:"sidetone,omitempty"`
	Tones        *bool         `yaml:"tones,omitempty" json:"tones,omitempty"` // default on
}

// VoiceChannels are a node's two channels.
type VoiceChannels struct {
	A VoiceChannel `yaml:"a,omitempty" json:"a"`
	B VoiceChannel `yaml:"b,omitempty" json:"b"`
}

// VoiceChannel is one channel: its group, ear and volume, and optionally
// a device and PTT of its own (two adapters: dual PTT).
type VoiceChannel struct {
	Group  int    `yaml:"group,omitempty" json:"group"` // 0: off
	Ear    string `yaml:"ear,omitempty" json:"ear,omitempty"`
	Volume *int   `yaml:"volume,omitempty" json:"volume,omitempty"` // default 80
	Device string `yaml:"device,omitempty" json:"device,omitempty"`
	PTT    string `yaml:"ptt,omitempty" json:"ptt,omitempty"`
	// Key keys a radio bridged into this channel's group while the group
	// plays (RoIP): cm108:<pin>, serial:<device>:<rts|dtr>,
	// gpio:<chip>:<line>. The radio's carrier sense is the channel's PTT.
	Key string `yaml:"key,omitempty" json:"key,omitempty"`
}

var (
	reVoiceDevice = regexp.MustCompile(`^(auto|usb:[0-9a-f]{4}:[0-9a-f]{4}@[0-9][0-9.-]*|onboard:[A-Za-z0-9_-]{1,32})$`)
	reVoiceKey    = regexp.MustCompile(`^(cm108:[1-4]|gpio:[a-z0-9_-]{1,32}:[0-9]{1,3}|serial:[A-Za-z0-9_./-]{1,64}:(rts|dtr))$`)
	reVoicePTT    = regexp.MustCompile(`^(auto|none|vox|button|cm108:[1-4]|gpio:[a-z0-9_-]{1,32}:[0-9]{1,3}|serial:[A-Za-z0-9_./-]{1,64}:(cts|dsr|dcd))$`)
)

func (v *VoicePlan) validate(es *Errors) {
	if len(v.Groups) > VoiceMaxGroups {
		es.add("voice.groups", "at most %d talk groups (got %d)", VoiceMaxGroups, len(v.Groups))
	}
	numbers := map[int]bool{}
	ports := map[netip.AddrPort]string{}
	for i, g := range v.Groups {
		f := fmt.Sprintf("voice.groups[%d]", i)
		if g.Number < 1 || g.Number > VoiceMaxGroups {
			es.add(f+".number", "must be 1–%d (got %d)", VoiceMaxGroups, g.Number)
		} else if numbers[g.Number] {
			es.add(f+".number", "group %d appears twice", g.Number)
		}
		numbers[g.Number] = true
		if l := len(g.Name); l == 0 || l > 32 {
			es.add(f+".name", "must be 1–32 characters (got %d)", l)
		} else if err := printableASCII(g.Name); err != nil {
			es.add(f+".name", "%v", err)
		}
		if g.Port != 0 && (g.Port < 1024 || g.Port > 65534) {
			es.add(f+".port", "must be 1024–65534 (RTCP uses the next port; got %d)", g.Port)
			continue
		}
		ap, err := g.VoiceAddr()
		switch {
		case err != nil:
			es.add(f+".address", "not an IPv4 address (got %q)", g.Address)
			continue
		case !ap.Addr().Is4() || !ap.Addr().IsMulticast():
			es.add(f+".address", "must be an IPv4 multicast address (got %s)", ap.Addr())
			continue
		case netip.MustParsePrefix("224.0.0.0/24").Contains(ap.Addr()):
			es.add(f+".address", "224.0.0.0/24 is for local network control, not a talk group (got %s)", ap.Addr())
			continue
		case ap.Addr() == netip.MustParseAddr("239.73.73.73"):
			es.add(f+".address", "239.73.73.73 is the nodes' own gossip group")
			continue
		}
		for _, p := range []netip.AddrPort{ap, netip.AddrPortFrom(ap.Addr(), ap.Port()+1)} {
			if o, dup := ports[p]; dup {
				es.add(f, "shares %s with %s", p, o)
			}
			ports[p] = f
		}
	}
	if v.Bitrate != 0 && (v.Bitrate < 6000 || v.Bitrate > 128000) {
		es.add("voice.bitrate", "must be 6000–128000 bit/s (got %d)", v.Bitrate)
	}
}

func (v *VoiceNode) validate(es *Errors) {
	if v.Device != "" && !reVoiceDevice.MatchString(v.Device) {
		es.add("voice.device", "must be auto, usb:<vendor>:<product>@<port>, or onboard:<card id> (got %q)", v.Device)
	}
	if v.PTT != "" && !reVoicePTT.MatchString(v.PTT) {
		es.add("voice.ptt", "must be auto, none, vox, button, cm108:<pin 1–4>, gpio:<chip>:<line> or serial:<device>:<cts|dsr|dcd> (got %q)", v.PTT)
	}
	switch v.PTTMode {
	case "", "hold", "toggle":
	default:
		es.add("voice.ptt_mode", "must be hold or toggle (got %q)", v.PTTMode)
	}
	for name, c := range map[string]VoiceChannel{"a": v.Channels.A, "b": v.Channels.B} {
		f := "voice.channels." + name
		if c.Group < 0 || c.Group > VoiceMaxGroups {
			es.add(f+".group", "must be 1–%d, or 0 for off (got %d)", VoiceMaxGroups, c.Group)
		}
		switch c.Ear {
		case "", "left", "right", "both":
		default:
			es.add(f+".ear", "must be left, right or both (got %q)", c.Ear)
		}
		if c.Volume != nil && (*c.Volume < 0 || *c.Volume > 100) {
			es.add(f+".volume", "must be 0–100 (got %d)", *c.Volume)
		}
		if c.Device != "" && !reVoiceDevice.MatchString(c.Device) {
			es.add(f+".device", "must be auto, usb:<vendor>:<product>@<port>, or onboard:<card id> (got %q)", c.Device)
		}
		if c.Key != "" && !reVoiceKey.MatchString(c.Key) {
			es.add(f+".key", "must be cm108:<pin 1–4>, serial:<device>:<rts|dtr> or gpio:<chip>:<line> (got %q)", c.Key)
		}
		switch {
		case c.PTT == "vox":
			// VOX keys channel A, from the node's microphone: one setting.
			es.add(f+".ptt", "VOX is the node's PTT (voice.ptt: vox) and keys channel A; a channel's own PTT is auto, none, button, cm108:<pin 1–4>, gpio:<chip>:<line> or serial:<device>:<cts|dsr|dcd>")
		case c.PTT != "" && !reVoicePTT.MatchString(c.PTT):
			es.add(f+".ptt", "must be auto, none, button, cm108:<pin 1–4>, gpio:<chip>:<line> or serial:<device>:<cts|dsr|dcd> (got %q)", c.PTT)
		}
	}
	if a, b := v.Channels.A.Group, v.Channels.B.Group; a != 0 && a == b {
		es.add("voice.channels", "a and b are both on group %d", a)
	}
	for i, n := range v.Listen {
		if n < 1 || n > VoiceMaxGroups {
			es.add(fmt.Sprintf("voice.listen[%d]", i), "must be 1–%d (got %d)", VoiceMaxGroups, n)
		}
	}
	if v.ListenVolume != nil && (*v.ListenVolume < 0 || *v.ListenVolume > 100) {
		es.add("voice.listen_volume", "must be 0–100 (got %d)", *v.ListenVolume)
	}
	if v.MicGain < -20 || v.MicGain > 20 {
		es.add("voice.mic_gain", "must be -20–20 dB (got %d)", v.MicGain)
	}
}

// VoiceMissingGroups lists the groups a node's voice settings name that the
// profile does not have: meshd reports them, and those channels stay off.
func VoiceMissingGroups(p *Profile, n *Node) []string {
	have := map[int]bool{}
	for _, g := range p.Voice.Groups {
		have[g.Number] = true
	}
	var out []string
	check := func(field string, g int) {
		if g != 0 && !have[g] {
			out = append(out, field+" "+strconv.Itoa(g))
		}
	}
	check("voice.channels.a.group", n.Voice.Channels.A.Group)
	check("voice.channels.b.group", n.Voice.Channels.B.Group)
	for _, g := range n.Voice.Listen {
		check("voice.listen", g)
	}
	return out
}

// String describes a group in one line, for logs.
func (g VoiceGroup) String() string {
	ap, _ := g.VoiceAddr()
	return strings.TrimSpace(fmt.Sprintf("%d %s %s", g.Number, g.Name, ap))
}
