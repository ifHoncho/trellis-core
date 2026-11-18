package alsa

import (
	"os"
	"testing"
)

// TRELLIS_ALSA_CARD names a card to read the mixer of (nothing is set).
func TestUnityDry(t *testing.T) {
	card := os.Getenv("TRELLIS_ALSA_CARD")
	if card == "" {
		t.Skip("set TRELLIS_ALSA_CARD to an ALSA card id")
	}
	levels, err := Unity(card, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%+v", levels)
}

func TestPlaybackKind(t *testing.T) {
	for name, want := range map[string]int{
		"Headphone": output, "Master": output, "PCM": output, "Speaker": output, "Line Out": output,
		// A USB headset's own name for its output: not switched off.
		"Headset": output,
		"Mic":     monitor, "Mic Playback": monitor, "Front Mic": monitor, "Front Mic Boost": monitor, "Line In": monitor,
		"Headset Mic": monitor, "Sidetone": monitor,
		"Auto Gain Control": cardAGC, "Mic AGC": cardAGC,
		// Not known: left alone, never switched off.
		"IEC958": unknown, "Bass Boost": unknown, "Loudness": unknown, "Jabra Something": unknown,
	} {
		if got := playbackKind(name); got != want {
			t.Errorf("%s: kind %d, want %d", name, got, want)
		}
	}
}
