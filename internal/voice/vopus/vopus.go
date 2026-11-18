// Package vopus is the voice engine's codec: Opus through internal/opus.
// It is a package of its own because importing libopus links a binary
// dynamically; only trellis-voiced imports it.
package vopus

import (
	"meridian/trellis/internal/opus"
	"meridian/trellis/internal/voice"
)

// Codec makes Opus encoders and decoders with the voice settings: 48 kHz
// mono, VoIP mode, complexity 5, in-band FEC, no DTX.
type Codec struct {
	// LossPerc is the loss the encoder plans FEC for, 0–100.
	LossPerc int
}

func (c Codec) NewEncoder(bitrate int) (voice.Encoder, error) {
	return opus.NewEncoder(opus.EncoderConfig{SampleRate: voice.SampleRate, Channels: 1, Bitrate: bitrate,
		Complexity: 5, InbandFEC: true, PacketLossPerc: c.LossPerc})
}

func (c Codec) NewDecoder() (voice.Decoder, error) {
	return opus.NewDecoder(voice.SampleRate, 1)
}
