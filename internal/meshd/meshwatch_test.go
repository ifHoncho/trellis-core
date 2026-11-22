package meshd

import (
	"testing"
	"time"

	"meridian/trellis/internal/config"
)

func TestKernelWatch(t *testing.T) {
	t0 := time.Date(2026, 11, 21, 1, 0, 0, 0, time.UTC)
	var w kernelWatch
	at := func(d time.Duration) time.Time { return t0.Add(d) }

	// Joined with a channel: nothing to do.
	if a := w.observe(at(0), true, 2462); a != kernelOK {
		t.Fatalf("joined on a channel: %v", a)
	}
	// Not joined in wpa_supplicant's view: its own warnings cover it.
	if a := w.observe(at(time.Second), false, 0); a != kernelOK {
		t.Fatalf("not joined: %v", a)
	}
	// Joined, a channel shown before, then none: acts only after
	// kernelLeftAfter.
	if a := w.observe(at(2*time.Second), true, 0); a != kernelOK {
		t.Fatalf("first sight: %v", a)
	}
	if a := w.observe(at(10*time.Second), true, 0); a != kernelOK {
		t.Fatalf("within the grace: %v", a)
	}
	if a := w.observe(at(17*time.Second), true, 0); a != kernelRestart {
		t.Fatalf("after the grace: want restart, got %v", a)
	}
	// A channel comes back: the count starts again.
	if a := w.observe(at(20*time.Second), true, 2462); a != kernelOK {
		t.Fatalf("back: %v", a)
	}
	// Lost again within kernelRecreateWithin: recreate, after twice the
	// wait (the second action in a row).
	w.observe(at(60*time.Second), true, 0)
	if a := w.observe(at(76*time.Second), true, 0); a != kernelOK {
		t.Fatalf("second loss acted on before the doubled wait: %v", a)
	}
	if a := w.observe(at(91*time.Second), true, 0); a != kernelRecreate {
		t.Fatalf("second loss: want recreate, got %v", a)
	}
	// Up for good after it (held 5 min): the count starts again.
	w.observe(at(30*time.Minute), true, 2462)
	w.observe(at(36*time.Minute), true, 2462)
	// A loss long after: a plain restart again, after the plain wait.
	w.observe(at(time.Hour), true, 0)
	if a := w.observe(at(time.Hour+16*time.Second), true, 0); a != kernelRestart {
		t.Fatalf("much later: want restart, got %v", a)
	}
	// A blip shorter than the grace never acts.
	w.observe(at(2*time.Hour), true, 0)
	w.observe(at(2*time.Hour+5*time.Second), true, 2437)
	if a := w.observe(at(2*time.Hour+30*time.Second), true, 0); a != kernelOK {
		t.Fatalf("after a blip the grace starts again: %v", a)
	}
}

// A driver that never reports a channel for a mesh interface: left alone
// however long. One that has: a restart (reset) does not make the watch
// forget it, so a fault that outlives the restart is still acted on.
func TestKernelWatchSeen(t *testing.T) {
	t0 := time.Date(2026, 11, 21, 1, 0, 0, 0, time.UTC)
	var never kernelWatch
	never.observe(t0, true, 0)
	if a := never.observe(t0.Add(time.Hour), true, 0); a != kernelOK {
		t.Fatalf("no channel ever shown: %v", a)
	}
	var w kernelWatch
	w.observe(t0, true, 5180)
	w.observe(t0.Add(time.Second), true, 0)
	if a := w.observe(t0.Add(17*time.Second), true, 0); a != kernelRestart {
		t.Fatalf("lost: want restart, got %v", a)
	}
	w.reset() // the link restarted
	w.observe(t0.Add(18*time.Second), false, 0)
	// Joined again, the driver wedged: no channel at all.
	w.observe(t0.Add(20*time.Second), true, 0)
	if a := w.observe(t0.Add(51*time.Second), true, 0); a != kernelRecreate {
		t.Fatalf("still no channel after the restart: want recreate, got %v", a)
	}
}

// The count of actions starts again only once a channel has held
// kernelRecreateWithin without a break, not at a pass that happens to
// come that long after the last action.
func TestKernelWatchHeld(t *testing.T) {
	t0 := time.Date(2026, 11, 21, 1, 0, 0, 0, time.UTC)
	var w kernelWatch
	w.observe(t0, true, 5180)
	w.observe(t0.Add(time.Second), true, 0)
	if a := w.observe(t0.Add(17*time.Second), true, 0); a != kernelRestart {
		t.Fatalf("first: %v", a)
	}
	// Back just before 5 min, seen at 5 min, lost 15 s later.
	w.observe(t0.Add(5*time.Minute), true, 5180)
	w.observe(t0.Add(5*time.Minute+15*time.Second), true, 0)
	if a := w.observe(t0.Add(6*time.Minute), true, 0); a != kernelRecreate {
		t.Fatalf("lost again after 15 s up: want recreate, got %v", a)
	}
}

// A fault that action does not clear is acted on less and less often.
func TestKernelWatchBackoff(t *testing.T) {
	t0 := time.Date(2026, 11, 21, 1, 0, 0, 0, time.UTC)
	var w kernelWatch
	now := t0
	w.observe(now, true, 2462)
	var gaps []time.Duration
	last := now
	for range 8 {
		// The link rejoins briefly after each action, then loses its
		// channel again.
		w.observe(now, true, 2462)
		for {
			now = now.Add(time.Second)
			if a := w.observe(now, true, 0); a != kernelOK {
				gaps = append(gaps, now.Sub(last))
				last = now
				if len(gaps) > 1 && a != kernelRecreate {
					t.Fatalf("action %d with a %v wait: %v, want recreate", len(gaps), gaps[len(gaps)-1], a)
				}
				break
			}
		}
	}
	for i := 1; i < len(gaps); i++ {
		if gaps[i] < gaps[i-1] {
			t.Fatalf("wait shrank: %v", gaps)
		}
	}
	if gaps[len(gaps)-1] < 9*time.Minute || gaps[len(gaps)-1] > 11*time.Minute {
		t.Fatalf("wait not capped near %v: %v", kernelWatchMaxWait, gaps)
	}
}

func TestChanWatch(t *testing.T) {
	t0 := time.Date(2026, 11, 21, 3, 0, 0, 0, time.UTC)
	at := func(d time.Duration) time.Time { return t0.Add(d) }
	var w chanWatch
	if off, re := w.observe(at(0), true, 2462, 2462, false); off || re {
		t.Fatal("on its channel")
	}
	// Pulled to another channel: nothing for offChannelAfter, then back.
	if off, re := w.observe(at(time.Second), true, 2437, 2462, false); off || re {
		t.Fatal("just moved")
	}
	if off, re := w.observe(at(32*time.Second), true, 2437, 2462, false); !off || !re {
		t.Fatalf("after %s: off %v rejoin %v", offChannelAfter, off, re)
	}
	// Pulled again at once: shown, but not put back before offChannelEvery.
	w.observe(at(40*time.Second), true, 2437, 2462, false)
	if off, re := w.observe(at(2*time.Minute), true, 2437, 2462, false); !off || re {
		t.Fatalf("second pull: off %v rejoin %v", off, re)
	}
	if off, re := w.observe(at(11*time.Minute), true, 2437, 2462, false); !off || !re {
		t.Fatalf("after %s: off %v rejoin %v", offChannelEvery, off, re)
	}
	// Not joined, or no channel in the kernel: the kernel watch's case.
	if off, re := w.observe(at(12*time.Minute), false, 2437, 2462, false); off || re {
		t.Fatal("not joined")
	}
	// A radar plan: shown, never put back.
	var r chanWatch
	r.observe(at(0), true, 5180, 5500, true)
	if off, re := r.observe(at(time.Hour), true, 5180, 5500, true); !off || re {
		t.Fatalf("radar plan: off %v rejoin %v", off, re)
	}
}

func TestRadarPlan(t *testing.T) {
	for _, c := range []struct {
		band  string
		ch, w int
		want  bool
	}{
		{config.Band5G, 36, 80, false}, {config.Band5G, 149, 80, false}, {config.Band5G, 52, 20, true},
		{config.Band5G, 100, 80, true}, {config.Band5G, 144, 20, true}, {config.Band5G, 36, 160, true},
		{config.Band5G, 149, 160, false}, {config.Band2G, 11, 20, false},
	} {
		if got := radarPlan(config.RadioPlan{Band: c.band, Channel: c.ch, Width: c.w}); got != c.want {
			t.Errorf("%s ch %d %d MHz: %v", c.band, c.ch, c.w, got)
		}
	}
}
