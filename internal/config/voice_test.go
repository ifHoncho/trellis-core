package config

import (
	"errors"
	"strings"
	"testing"
)

const voiceProfile = `
voice:
  groups:
    - {number: 1, name: Command, priority: true}
    - {number: 2, name: Alpha, address: 239.1.2.3, port: 5000}
  bitrate: 24000
`

func fieldsOf(err error) []string {
	var es Errors
	if !errors.As(err, &es) {
		return nil
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Field)
	}
	return out
}

func TestVoiceProfile(t *testing.T) {
	p, err := ParseProfile([]byte(goodProfile + voiceProfile))
	if err != nil {
		t.Fatal(err)
	}
	a1, _ := p.Voice.Groups[0].VoiceAddr()
	a2, _ := p.Voice.Groups[1].VoiceAddr()
	if a1.String() != "239.73.80.1:38801" || a2.String() != "239.1.2.3:5000" || p.Voice.Bitrate != 24000 {
		t.Errorf("addresses %v %v", a1, a2)
	}
	for bad, field := range map[string]string{
		"voice: {groups: [{number: 0, name: x}]}":                                                          "voice.groups[0].number",
		"voice: {groups: [{number: 1, name: x}, {number: 1, name: y}]}":                                    "voice.groups[1].number",
		"voice: {groups: [{number: 1, name: ''}]}":                                                         "voice.groups[0].name",
		"voice: {groups: [{number: 1, name: x, address: 192.0.2.1}]}":                                      "voice.groups[0].address",
		"voice: {groups: [{number: 1, name: x, address: 224.0.0.9}]}":                                      "voice.groups[0].address",
		"voice: {groups: [{number: 1, name: x, address: 239.73.73.73}]}":                                   "voice.groups[0].address",
		"voice: {groups: [{number: 1, name: x, port: 65535}]}":                                             "voice.groups[0].port",
		"voice: {groups: [{number: 1, name: x}, {number: 2, name: y, address: 239.73.80.1, port: 38802}]}": "voice.groups[1]",
		"voice: {bitrate: 100}": "voice.bitrate",
	} {
		_, err := ParseProfile([]byte(goodProfile + "\n" + bad + "\n"))
		if !strings.Contains(strings.Join(fieldsOf(err), " "), field) {
			t.Errorf("%s: want an error on %s, got %v", bad, field, err)
		}
	}
}

func TestVoiceNode(t *testing.T) {
	n, err := ParseNode([]byte(goodNode + `
voice:
  enable: true
  ptt: cm108:3
  channels:
    a: {group: 1, ear: left, volume: 90}
    b: {group: 2, ear: right, device: usb:0d8c:0012@1-1.4, ptt: auto}
  listen: [3]
`))
	if err != nil || !n.Voice.Enable || n.Voice.Channels.B.Device != "usb:0d8c:0012@1-1.4" {
		t.Fatalf("%+v %v", n.Voice, err)
	}
	for bad, field := range map[string]string{
		"voice: {device: hw:1}":                               "voice.device",
		"voice: {ptt: cm108:7}":                               "voice.ptt",
		"voice: {ptt: 'serial:/dev/ttyACM0:rts'}":             "voice.ptt",
		"voice: {channels: {a: {group: 1}, b: {group: 1}}}":   "voice.channels",
		"voice: {channels: {a: {ear: middle}}}":               "voice.channels.a.ear",
		"voice: {channels: {b: {volume: 101}}}":               "voice.channels.b.volume",
		"voice: {listen: [17]}":                               "voice.listen[0]",
		"voice: {mic_gain: 30}":                               "voice.mic_gain",
		"voice: {channels: {a: {key: 'serial:ttyUSB0:cts'}}}": "voice.channels.a.key",
	} {
		_, err := ParseNode([]byte(goodNode + "\n" + bad + "\n"))
		if !strings.Contains(strings.Join(fieldsOf(err), " "), field) {
			t.Errorf("%s: want an error on %s, got %v", bad, field, err)
		}
	}
	p, _ := ParseProfile([]byte(goodProfile + voiceProfile))
	if m := VoiceMissingGroups(p, n); len(m) != 1 || m[0] != "voice.listen 3" {
		t.Errorf("missing groups: %v", m)
	}
}

// VOX is the node's PTT: a channel of its own cannot have it.
func TestVoiceChannelVOXRefused(t *testing.T) {
	var es Errors
	v := VoiceNode{PTT: "vox"}
	v.validate(&es)
	if es.orNil() != nil {
		t.Fatalf("voice.ptt: vox refused: %v", es.orNil())
	}
	for _, v := range []VoiceNode{
		{Channels: VoiceChannels{A: VoiceChannel{PTT: "vox"}}},
		{Channels: VoiceChannels{B: VoiceChannel{PTT: "vox"}}},
	} {
		var es Errors
		v.validate(&es)
		if err := es.orNil(); err == nil || !strings.Contains(err.Error(), "VOX is the node's PTT") {
			t.Errorf("%+v: %v", v.Channels, err)
		}
	}
}
