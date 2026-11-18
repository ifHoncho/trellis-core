package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"meridian/trellis/internal/voice"
	"meridian/trellis/internal/voice/alsa"
	"meridian/trellis/internal/voice/cm108"
	"meridian/trellis/internal/voice/devices"
	"meridian/trellis/internal/voice/pttline"
	"meridian/trellis/internal/voice/wav"
)

// latencyUS is the ALSA buffer for capture and playback: three frames.
const latencyUS = 60000

// audio is one cycle's audio: a capture and playback per channel (the same
// device for both unless channel B names its own), the PTT inputs, the
// radio keys and the volume buttons.
type audio struct {
	in      [2]voice.Capture // in[1] nil: one device for both channels
	out     [2]voice.Playback
	closers []func() error
	ptt     [2]func() (bool, error) // per channel; nil: none
	key     [2]func(bool) error     // per channel: keys a bridged radio; nil: none
	volume  [2]*cm108.HIDRaw        // per channel: the CM108 whose volume buttons step it
	// buttonVolume: a headset adapter's volume keys, per channel.
	buttonVolume [2]chan int
	// toggle: press to start, press to stop (per channel).
	toggle [2]bool
	// cutWatch: the channel's PTT can short its microphone (a headset
	// button): the engine watches for it.
	cutWatch [2]bool
	what     string
	info     voice.AudioInfo
	reports  map[*cm108.HIDRaw]*hidRead // report (the PTT loop's goroutine only)
}

// hidRead is a CM108's input report as last read.
type hidRead struct {
	at  time.Time
	r   cm108.Report
	err error
}

// report reads a CM108's input report, once for everything that asks
// within one pass of the PTT loop (a channel's PTT, the other's on the
// same adapter, the volume buttons).
func (a *audio) report(h *cm108.HIDRaw) (cm108.Report, error) {
	if a.reports == nil {
		a.reports = map[*cm108.HIDRaw]*hidRead{}
	}
	c := a.reports[h]
	if c == nil {
		c = &hidRead{}
		a.reports[h] = c
	}
	if now := time.Now(); now.Sub(c.at) > 5*time.Millisecond {
		c.at = now
		c.r, c.err = h.Input()
	}
	return c.r, c.err
}

func (a *audio) close() {
	for i := len(a.closers) - 1; i >= 0; i-- {
		_ = a.closers[i]()
	}
}

// cand is a sound card, with the adapter it is when Trellis knows it.
type cand struct {
	d     devices.Device
	model *cm108.Model
	rank  int // for "auto": 0 a known adapter, 1 a CM108 with HID, 2 USB, 3 onboard, 9 no microphone
}

// device is one opened sound card, with its HID interface opened on demand.
type device struct {
	cand
	hid *cm108.HIDRaw
}

func (d *device) openHID(a *audio) (*cm108.HIDRaw, error) {
	if d.hid == nil {
		h, err := cm108.OpenHIDRaw(d.d.HIDRaw)
		if err != nil {
			return nil, err
		}
		d.hid, a.closers = h, append(a.closers, h.Close)
	}
	return d.hid, nil
}

func scan(sys string, log *slog.Logger) ([]cand, error) {
	devs, err := devices.Scan(sys)
	if err != nil {
		return nil, err
	}
	var out []cand
	for _, d := range devs {
		c := cand{d: d, rank: 9}
		if d.USB && d.Vendor == cm108.Vendor && d.HIDRaw != "" {
			if h, err := cm108.OpenHIDRaw(d.HIDRaw); err == nil {
				if r, err := h.Input(); err == nil {
					c.model = cm108.Identify(d.Product, d.Name, r)
				} else {
					log.Warn("voice: cannot read the CM108's HID input report", "device", d.ID, "hidraw", d.HIDRaw, "err", err)
				}
				h.Close()
			} else {
				log.Warn("voice: cannot open the CM108's HID interface", "device", d.ID, "hidraw", d.HIDRaw, "err", err)
			}
		}
		switch {
		case !d.Capture:
			// No microphone: still the earpiece and the PTT's adapter, when
			// it is a USB one (a headset adapter can show no microphone
			// until a plug with one goes in). Onboard playback-only cards
			// (HDMI) are not headsets.
			if d.USB && d.Playback {
				c.rank = 5
			}
		case c.model != nil:
			c.rank = 0
		case d.USB && d.Vendor == cm108.Vendor && d.HIDRaw != "":
			c.rank = 1
		case d.USB:
			c.rank = 2
		default:
			c.rank = 3
		}
		out = append(out, c)
	}
	return out, nil
}

// pick chooses a device: by ID, or "auto" (or ""), the best rank; skip
// is an ID already taken.
func pick(cands []cand, want, skip string) *cand {
	var best *cand
	for i := range cands {
		c := &cands[i]
		switch {
		case c.d.ID == skip:
		case want != "" && want != "auto":
			if c.d.ID == want {
				return c
			}
		case c.rank < 9 && (best == nil || c.rank < best.rank):
			best = c
		}
	}
	return best
}

// openAudio finds and opens the devices (architecture §13, Voice): the
// node's (cfg.Device, or channel A's own) for channel A and, unless
// channel B names another, for B too. With no device that records, voice
// still listens: a timer is the clock.
func openAudio(sys string, cfg voice.Config, log *slog.Logger) (*audio, error) {
	cands, err := scan(sys, log)
	if err != nil {
		return nil, err
	}
	a := &audio{info: voice.AudioInfo{PTT: "none"}}
	for _, c := range cands {
		d := voice.AudioDevice{ID: c.d.ID, Name: c.d.Name, Capture: c.d.Capture, Playback: c.d.Playback, HID: isCM108(c.d), Button: len(c.d.Inputs) > 0}
		if c.model != nil {
			d.Model = c.model.Name
		}
		a.info.Devices = append(a.info.Devices, d)
	}

	wantA := cfg.Channels[0].Device
	if wantA == "" {
		wantA = cfg.Device
	}
	ca := pick(cands, wantA, "")
	if ca == nil {
		if wantA != "" && wantA != "auto" {
			log.Warn("voice: the configured audio device is not here; listening only", "device", wantA)
		} else {
			log.Warn("voice: no audio device that records; listening only")
		}
		a.in[0], a.out[0], a.what = wav.SilenceCapture(), wav.Discard{}, "none"
		a.info.Device = "none"
		return a, nil
	}
	devs := [2]*device{{cand: *ca}}
	devs[1] = devs[0]
	if wantB := cfg.Channels[1].Device; wantB != "" && wantB != ca.d.ID {
		if cb := pick(cands, wantB, ca.d.ID); cb != nil {
			devs[1] = &device{cand: *cb}
		} else {
			log.Warn("voice: channel B's audio device is not here; channel B shares channel A's", "device", wantB)
		}
	}
	describe := func(d *device) string {
		if d.model != nil {
			return d.d.ID + " (" + d.model.Name + ")"
		}
		return d.d.ID
	}
	a.what = describe(devs[0])
	a.info.Device, a.info.Name = devs[0].d.ID, devs[0].d.Name
	if devs[0].model != nil {
		a.info.Model, a.info.Mono = devs[0].model.Name, devs[0].model.Mono
	}
	for i, d := range devs {
		if i == 1 && d == devs[0] {
			break
		}
		// The card's own levels to unity (a KT0211 starts at -20 dB).
		if levels, err := alsa.Unity(d.d.CardID, false); err != nil {
			log.Warn("voice: the card's mixer could not be set", "card", d.d.CardID, "err", err)
		} else {
			for _, l := range levels {
				a.info.Levels = append(a.info.Levels, voice.MixerLevel{Control: l.Control, Kind: l.Kind, DB: l.DB})
			}
			log.Info("voice: card levels", "card", d.d.CardID, "levels", levels)
		}
		if !d.d.Capture {
			// No microphone: a timer is the clock, and nothing can be sent.
			a.in[i] = wav.SilenceCapture()
			a.info.NoMic = true
			log.Warn("voice: the audio device shows no microphone; listening only", "device", d.d.ID)
		} else {
			in, err := alsa.OpenCapture(d.d.CardID, voice.SampleRate, latencyUS)
			if err != nil {
				a.close()
				return nil, err
			}
			a.in[i], a.closers = in, append(a.closers, in.Close)
		}
		if d.d.Playback {
			out, err := alsa.OpenPlayback(d.d.CardID, voice.SampleRate, latencyUS)
			if err != nil {
				a.close()
				return nil, err
			}
			a.out[i], a.closers = out, append(a.closers, out.Close)
			if d.model != nil && d.model.Mono {
				a.out[i] = monoOut{out}
			}
		} else {
			a.out[i] = wav.Discard{}
		}
		if i == 1 {
			a.what += ", B: " + describe(d)
			a.info.Device += ", B: " + d.d.ID
		}
	}

	// PTT, per channel, on that channel's device: channel A takes channel
	// a's own spec or the node's; channel B only its own, or, on a device
	// of its own, that device's PTT.
	specs := [2]string{cfg.Channels[0].PTT, cfg.Channels[1].PTT}
	if specs[0] == "" {
		specs[0] = cfg.PTT
	}
	names := [2]string{"none", "none"}
	for ch, spec := range specs {
		d := devs[ch]
		own := ch == 0 || d != devs[0]
		pin, high := 0, true
		switch {
		case spec == "none":
		case spec == "" || spec == "auto":
			switch {
			case !own:
			case d.model != nil && d.d.HIDRaw != "":
				pin, high = d.model.PTTPin, d.model.PTTHigh
				if d.model.VolumeButtons {
					if h, err := d.openHID(a); err == nil {
						a.volume[ch] = h
					} else {
						log.Warn("voice: the adapter's volume buttons cannot be read", "device", d.d.ID, "err", err)
					}
				}
			case len(d.d.Inputs) > 0 && !isCM108(d.d):
				// No CM108 pins: the headset button, as a phone-style
				// PTT adapter presses it.
				a.button(ch, d, names[:], log)
			}
		case spec == "button":
			a.button(ch, d, names[:], log)
		case spec == "vox":
			if ch == 0 {
				names[ch] = "vox" // the engine keys channel A itself
			} else {
				log.Warn("voice: VOX keys channel A only; no PTT on channel B")
			}
		case strings.HasPrefix(spec, "cm108:"):
			if !isCM108(d.d) {
				log.Warn("voice: PTT on a CM108 pin, but this adapter is not a CM108 (C-Media); no PTT on this channel", "device", d.d.ID, "ptt", spec)
				break
			}
			// A button to 3.3 V against the chip's pull-down: high when pressed.
			pin, _ = strconv.Atoi(strings.TrimPrefix(spec, "cm108:"))
			if pin < 1 || pin > 4 {
				log.Warn("voice: a CM108 has pins 1–4; no PTT on this channel", "ptt", spec)
				pin = 0
				break
			}
			if h, err := d.openHID(a); err == nil && own {
				a.volume[ch] = h
			}
		default:
			l, err := pttline.Open(spec)
			if err != nil {
				log.Warn("voice: PTT line not opened; no PTT on this channel until it is there (the voice service may open USB serial ports, ttyUSB and ttyACM, and GPIO chips; another serial port needs a DeviceAllow= in its unit)",
					"channel", "ab"[ch:ch+1], "ptt", spec, "err", err)
				break
			}
			a.closers = append(a.closers, l.Close)
			a.ptt[ch], names[ch] = l.Pressed, spec
		}
		if pin == 0 {
			continue
		}
		h, err := d.openHID(a)
		if err != nil {
			// Voice runs without this PTT rather than not at all.
			log.Warn("voice: the CM108's HID interface cannot be opened; no PTT on this channel", "device", d.d.ID, "err", err)
			continue
		}
		a.ptt[ch] = func() (bool, error) {
			r, err := a.report(h)
			if err != nil {
				return false, err
			}
			v, ok := r.GPIO(pin)
			return ok && v == high, nil
		}
		names[ch] = "cm108:" + strconv.Itoa(pin)
	}
	for ch, n := range names {
		a.cutWatch[ch] = n == "button" // only a headset button can short its mic
		// Hold to talk unless press-to-start is chosen (never by default).
		a.toggle[ch] = cfg.PTTMode == "toggle" && n != "none" && n != "vox"
		if a.toggle[ch] {
			names[ch] += " (press to start, press to stop)"
		} else if n == "button" && !a.info.NoMic {
			a.info.ButtonHold = append(a.info.ButtonHold, "ab"[ch:ch+1])
			log.Warn("voice: the PTT is the adapter's headset button, which silences the microphone while held: with hold to talk nothing is sent. Choose press to start, press to stop (ptt_mode: toggle), or VOX", "channel", "ab"[ch:ch+1])
		}
	}
	if names[0] == "none" && (specs[0] == "" || specs[0] == "auto") {
		a.info.NoPTT = true
		log.Warn("voice: this audio device offers no PTT (no CM108 pins, no headset button); channel A listens only. Use a USB adapter with a headset button, a CM108, a GPIO or serial PTT line, or VOX", "device", devs[0].d.ID)
	}
	a.info.PTT = names[0]
	if names[1] != "none" {
		a.info.PTT += ", B: " + names[1]
	}

	// Keys: a radio bridged into a channel's group (RoIP).
	for ch, c := range cfg.Channels {
		spec := c.Key
		switch {
		case spec == "":
			continue
		case strings.HasPrefix(spec, "cm108:"):
			d := devs[ch]
			if !isCM108(d.d) {
				log.Warn("voice: radio key on a CM108 pin, but this adapter is not a CM108 (C-Media); no radio key", "device", d.d.ID, "key", spec)
				continue
			}
			h, err := d.openHID(a)
			if err != nil {
				log.Warn("voice: the CM108's HID interface cannot be opened; no radio key", "device", d.d.ID, "err", err)
				continue
			}
			pin, _ := strconv.Atoi(strings.TrimPrefix(spec, "cm108:"))
			if err := h.SetGPIO(pin, false); err != nil {
				log.Warn("voice: cannot drive the CM108 pin for the radio key", "key", spec, "err", err)
				continue
			}
			a.key[ch] = func(on bool) error { return h.SetGPIO(pin, on) }
			a.closers = append(a.closers, func() error { return h.SetGPIO(pin, false) })
		default:
			o, err := pttline.OpenOutput(spec)
			if err != nil {
				log.Warn("voice: radio key not opened", "channel", "ab"[ch:ch+1], "key", spec, "err", err)
				continue
			}
			a.key[ch] = o.Set
			a.closers = append(a.closers, o.Close)
		}
		a.info.PTT += ", key " + "ab"[ch:ch+1] + ": " + spec
	}
	return a, nil
}

// toggleLimit releases a press-to-start key nobody pressed again: a
// forgotten key must not hold a group.
const toggleLimit = 60 * time.Second

// keyLimit is the longest a bridged radio's transmitter is keyed in one
// go.
const keyLimit = 3 * time.Minute

// latchState is a press-to-start, press-to-stop PTT.
type latchState struct {
	on    bool
	since time.Time
}

// latchStep takes one poll: a press flips the key (set keys or releases
// it and reports whether it was granted: a refusal leaves it off); a key
// on for toggleLimit is released. It reports a change.
func latchStep(l *latchState, press bool, now time.Time, set func(down bool) bool) bool {
	if l.on && now.Sub(l.since) > toggleLimit {
		l.on = false
		set(false)
		return true
	}
	if !press {
		return false
	}
	l.on, l.since = !l.on, now
	if granted := set(l.on); l.on && !granted {
		l.on = false // refused (busy): the busy tone played
		set(false)
	}
	return true
}

// isCM108 is a C-Media adapter with its HID interface: CM108 GPIO pins.
func isCM108(d devices.Device) bool { return d.USB && d.Vendor == cm108.Vendor && d.HIDRaw != "" }

// button makes a channel's PTT the device's headset button, its volume
// keys stepping that channel.
func (a *audio) button(ch int, d *device, names []string, log *slog.Logger) {
	b, err := pttline.OpenButton(d.d.Inputs)
	if err != nil {
		log.Warn("voice: the adapter's headset button cannot be read; no PTT on this channel", "device", d.d.ID, "err", err)
		return
	}
	a.closers = append(a.closers, b.Close)
	a.ptt[ch], names[ch] = b.Pressed, "button"
	a.buttonVolume[ch] = b.Volume
}

// monoOut plays both ears into the left channel, for a device with one
// earpiece (OpenVLM wires the left output only).
type monoOut struct{ voice.Playback }

func (m monoOut) Write(f []int16) error {
	for i := 0; i+1 < len(f); i += 2 {
		v := int32(f[i]) + int32(f[i+1])
		f[i], f[i+1] = int16(min(32767, max(-32768, v))), 0
	}
	return m.Playback.Write(f)
}

// watchPTT polls the channels' PTT inputs, drives the radio keys, and
// reads the CM108s' volume buttons, every 10 ms (PTT debounced 20 ms),
// until ctx ends. An input that fails (unplugged) ends the cycle: errc.
func watchPTT(ctx context.Context, a *audio, eng *voice.Engine, log *slog.Logger, errc chan<- error) {
	if a.ptt[0] == nil && a.ptt[1] == nil && a.key[0] == nil && a.key[1] == nil && a.volume == [2]*cm108.HIDRaw{} && a.buttonVolume == [2]chan int{} {
		return
	}
	t := time.NewTicker(10 * time.Millisecond)
	defer t.Stop()
	deb := [2]pttline.Debounce{{Stable: 20 * time.Millisecond}, {Stable: 20 * time.Millisecond}}
	var latch [2]latchState
	var keyed, keyHeld, up, down [2]bool
	var keyedAt [2]time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			for ch := range 2 {
				name := "ab"[ch : ch+1]
				if read := a.ptt[ch]; read != nil {
					v, err := read()
					if err != nil {
						errc <- fmt.Errorf("PTT on channel %s: %w", name, err)
						return
					}
					changed, pressed := deb[ch].Update(v, now)
					switch {
					case a.toggle[ch]:
						// A press a PTT check took counts as on: its next
						// press then ends the check.
						set := func(down bool) bool {
							g, _ := eng.SetPTT(ch, down)
							return g || (down && eng.CheckRecording(ch))
						}
						if latchStep(&latch[ch], changed && pressed, now, set) {
							log.Debug("voice: PTT (press to start, press to stop)", "channel", name, "talking", latch[ch].on)
						}
					case changed:
						granted, err := eng.SetPTT(ch, pressed)
						log.Debug("voice: PTT", "channel", name, "pressed", pressed, "granted", granted, "err", err)
					}
				}
				if set := a.key[ch]; set != nil {
					// A radio keyed for keyLimit is let go until its group
					// has fallen quiet: a group that never stops talking
					// must not hold a transmitter for ever.
					on := eng.Playing(ch)
					switch {
					case !on:
						keyHeld[ch] = false
					case keyed[ch] && now.Sub(keyedAt[ch]) > keyLimit:
						keyHeld[ch] = true
						log.Warn("voice: the radio key has been on for its limit; released until the group is quiet", "channel", name, "limit", keyLimit)
					}
					if on = on && !keyHeld[ch]; on != keyed[ch] {
						keyedAt[ch] = now
						if err := set(on); err != nil {
							errc <- fmt.Errorf("radio key on channel %s: %w", name, err)
							return
						}
						keyed[ch] = on
						log.Debug("voice: radio key", "channel", name, "on", on)
					}
				}
				if v := a.buttonVolume[ch]; v != nil {
					select {
					case d := <-v:
						eng.AdjustVolume(ch, 5*d)
					default:
					}
				}
				if h := a.volume[ch]; h != nil {
					r, err := a.report(h)
					if err != nil {
						errc <- fmt.Errorf("volume buttons: %w", err)
						return
					}
					if u := r.VolumeUp(); u != up[ch] {
						if up[ch] = u; u {
							eng.AdjustVolume(ch, 5)
						}
					}
					if d := r.VolumeDown(); d != down[ch] {
						if down[ch] = d; d {
							eng.AdjustVolume(ch, -5)
						}
					}
				}
			}
		}
	}
}
