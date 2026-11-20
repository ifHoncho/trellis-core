package meshd

import (
	"strings"
	"testing"
	"time"

	"meridian/trellis/internal/crashlog"
)

// The 24 h reset alert before the clock is set: only the reset this boot
// found counts, and its time is not given.
func TestResetWarningClockNotSet(t *testing.T) {
	stale := time.Date(2026, 11, 14, 0, 44, 0, 0, time.UTC)
	l := &crashlog.Log{BootID: "boot-3", Entries: []crashlog.Entry{
		{Time: stale.Add(6 * 24 * time.Hour), Verdict: crashlog.CardStopped, Summary: "card hang", Boot: "boot-2"},
		{Time: stale, TimePending: true, Verdict: crashlog.PowerLoss, Summary: "no crash record", Boot: "boot-3"},
	}}
	w, ok := resetWarning(l, stale.Add(time.Minute))
	if !ok || w.Message != "Unplanned reset (time unknown): no crash record." {
		t.Fatalf("before the clock is set: %+v %v", w, ok)
	}
	if err := l.ClockSet(stale.Add(6*24*time.Hour+time.Hour), time.Minute); err != nil {
		t.Fatal(err)
	}
	w, ok = resetWarning(l, stale.Add(6*24*time.Hour+time.Hour))
	if !ok || !strings.HasPrefix(w.Message, "2 unplanned resets in 24 h, last at ") {
		t.Fatalf("once set: %+v %v", w, ok)
	}
}
