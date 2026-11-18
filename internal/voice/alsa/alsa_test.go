package alsa

import (
	"os"
	"testing"
	"time"
)

// TRELLIS_ALSA_CARD names a card to try (for instance PCH on a PC): 1 s
// of capture and of playback at 48 kHz, in real time.
func TestCard(t *testing.T) {
	card := os.Getenv("TRELLIS_ALSA_CARD")
	if card == "" {
		t.Skip("set TRELLIS_ALSA_CARD to an ALSA card id")
	}
	if err := Load(); err != nil {
		t.Skip(err)
	}
	in, err := OpenCapture(card, 48000, 60000)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := OpenPlayback(card, 48000, 60000)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	mic, stereo := make([]int16, 960), make([]int16, 1920)
	start := time.Now()
	for i := 0; i < 50; i++ {
		if err := in.Read(mic); err != nil {
			t.Fatal(err)
		}
		if err := out.Write(stereo); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d < 900*time.Millisecond || d > 1300*time.Millisecond {
		t.Errorf("50 frames of 20 ms took %v", d)
	}
}
