package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Press to start, press to stop; a refusal leaves it off; a forgotten key
// goes after the limit.
func TestLatch(t *testing.T) {
	var l latchState
	var calls []bool
	busy := false
	set := func(down bool) bool { calls = append(calls, down); return down && !busy }
	t0 := time.Unix(0, 0)
	latchStep(&l, true, t0, set)
	if !l.on {
		t.Fatal("not on after a press")
	}
	latchStep(&l, false, t0.Add(time.Second), set)
	if !l.on {
		t.Fatal("off without a press")
	}
	latchStep(&l, true, t0.Add(2*time.Second), set)
	if l.on {
		t.Fatal("still on after the second press")
	}
	busy = true
	latchStep(&l, true, t0.Add(3*time.Second), set)
	if l.on {
		t.Fatal("on although refused")
	}
	busy = false
	latchStep(&l, true, t0.Add(4*time.Second), set)
	latchStep(&l, false, t0.Add(4*time.Second+toggleLimit+time.Second), set)
	if l.on || calls[len(calls)-1] {
		t.Errorf("not released after the limit: %v", calls)
	}
}

// Plugging an adapter in (or out) changes the hardware fingerprint the
// engine restarts on; nothing changing keeps it.
func TestHardwareFingerprint(t *testing.T) {
	sys := t.TempDir()
	o := &runOpts{sys: sys}
	mk := func(card, usb string) {
		dev := filepath.Join(sys, "devices", usb)
		os.MkdirAll(filepath.Join(dev, "sound", card), 0o755)
		os.WriteFile(filepath.Join(dev, "idVendor"), []byte("12d1\n"), 0o644)
		os.WriteFile(filepath.Join(dev, "idProduct"), []byte("0010\n"), 0o644)
		os.WriteFile(filepath.Join(dev, "sound", card, "id"), []byte("Audio\n"), 0o644)
		os.MkdirAll(filepath.Join(sys, "class/sound"), 0o755)
		os.Symlink(filepath.Join(dev, "sound", card), filepath.Join(sys, "class/sound", card))
		os.Symlink(dev, filepath.Join(dev, "sound", card, "device"))
	}
	empty := o.hardware(nil)
	mk("card0", "usb1/1-1.1")
	plugged := o.hardware(nil)
	if plugged == empty || !strings.Contains(plugged, "usb:12d1:0010@1-1.1") {
		t.Fatalf("plugged in: %q (before %q)", plugged, empty)
	}
	if o.hardware(nil) != plugged {
		t.Error("the fingerprint moved with nothing changed")
	}
	os.Remove(filepath.Join(sys, "class/sound/card0"))
	if o.hardware(nil) != empty {
		t.Error("unplugged: not back to empty")
	}
}

// A serial PTT cable plugged back in changes the fingerprint too, so its
// line is opened again.
func TestHardwareFingerprintLines(t *testing.T) {
	dir := t.TempDir()
	o := &runOpts{sys: t.TempDir()}
	tty := filepath.Join(dir, "ttyUSB0")
	raw := []byte(`{"ptt":"serial:` + tty + `:cts","channels":[{"group":1,"key":"gpio:gpiochip0:27"},{"ptt":"cm108:3"}]}`)
	lines := pttLines(raw)
	if len(lines) != 2 || lines[0] != tty || lines[1] != "/dev/gpiochip0" {
		t.Fatalf("lines %q", lines)
	}
	lines = lines[:1]
	gone := o.hardware(lines)
	os.WriteFile(tty, nil, 0o644)
	if o.hardware(lines) == gone {
		t.Error("the line's device appeared and the fingerprint stayed")
	}
}
