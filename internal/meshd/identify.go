package meshd

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"meridian/trellis/internal/api"
	"meridian/trellis/internal/radio"
)

// Identify (architecture §5.2): blink a radio's LED so the operator can
// tell which adapter the Radios page is talking about. Wi-Fi drivers name
// their LEDs after the phy ("ath9k_htc-phy5", "mt76-phy1", "phy0-led",
// "rt2800usb-phy0::radio"), which is how a radio's LEDs are found.

var identifying sync.Map // LED directory → true while a blink is running

// radioLEDs returns the sysfs directories of the LEDs that belong to phy.
func radioLEDs(sysRoot, phy string) []string {
	if phy == "" {
		return nil
	}
	re, err := regexp.Compile(`(^|[^a-z0-9])` + regexp.QuoteMeta(phy) + `($|[^0-9])`)
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(sysRoot, "class", "leds"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if re.MatchString(e.Name()) {
			out = append(out, filepath.Join(sysRoot, "class", "leds", e.Name()))
		}
	}
	return out
}

// IdentifyRadio blinks the LEDs of the radio with the fingerprint for the
// given time, then gives them back to their trigger.
func (d *Daemon) IdentifyRadio(fingerprint string, seconds int) error {
	if seconds <= 0 {
		seconds = 10
	}
	seconds = min(seconds, 60)
	var radios []*radio.Radio
	d.facts.Read(func(f *Facts) { radios = f.Radios })
	r := radio.Find(radios, fingerprint)
	if r == nil {
		return api.Errorf(404, "no radio with fingerprint %q is present", fingerprint)
	}
	leds := radioLEDs(d.opts.SysRoot, r.Phy)
	if len(leds) == 0 {
		return api.Errorf(409, "%s has no LED the driver exposes; it cannot be identified this way", r.Describe())
	}
	started := 0
	for _, led := range leds {
		if _, busy := identifying.LoadOrStore(led, true); busy {
			continue
		}
		started++
		go func() {
			defer identifying.Delete(led)
			if err := blinkLED(led, time.Duration(seconds)*time.Second, d.ctx.Done()); err != nil {
				d.log.Warn("identify: cannot blink the LED", "led", filepath.Base(led), "err", err)
			}
		}()
	}
	if started > 0 {
		d.log.Info("identifying a radio by its LED", "radio", r.Describe(), "leds", len(leds), "seconds", seconds)
	}
	return nil
}

// blinkLED takes the LED from its trigger, blinks it, and restores the
// trigger and brightness.
func blinkLED(dir string, d time.Duration, stop <-chan struct{}) error {
	read := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(dir, name))
		return strings.TrimSpace(string(b))
	}
	write := func(name, v string) error { return os.WriteFile(filepath.Join(dir, name), []byte(v), 0o644) }

	trigger := "none"
	for _, t := range strings.Fields(read("trigger")) {
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			trigger = strings.Trim(t, "[]")
		}
	}
	bright := read("brightness")
	maxB := read("max_brightness")
	if maxB == "" || maxB == "0" {
		maxB = "1"
	}
	if err := write("trigger", "none"); err != nil {
		return fmt.Errorf("take the LED from trigger %s: %w", trigger, err)
	}
	defer func() {
		if trigger != "none" {
			_ = write("trigger", trigger)
		} else {
			_ = write("brightness", bright)
		}
	}()
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(d)
	on := false
	for {
		on = !on
		v := "0"
		if on {
			v = maxB
		}
		if err := write("brightness", v); err != nil {
			return err
		}
		select {
		case <-tick.C:
		case <-deadline:
			return nil
		case <-stop:
			return nil
		}
	}
}
