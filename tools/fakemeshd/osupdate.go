package main

import (
	"context"
	"time"

	"meridian/trellis/internal/api"
	"meridian/trellis/internal/osupdate"
)

// OS package updates, made up: a check finds seven updates after 3 s; an
// upgrade installs them over 6 s and asks for a reboot.

var fakePackages = []string{"libssl3t64", "openssl", "curl", "libcurl4t64", "tzdata", "raspi-firmware", "libc6"}

func (f *fake) OSUpdates(ctx context.Context) (*osupdate.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.os
	now := time.Now().UTC()
	if s.Running() && now.Sub(s.StartedAt) > f.osTook(s.Phase) {
		if s.Phase == osupdate.PhaseChecking {
			n := len(fakePackages)
			s.Phase, s.Pending, s.Packages, s.CheckedAt = osupdate.PhaseIdle, &n, fakePackages, now
		} else {
			zero := 0
			s.Phase, s.Pending, s.Packages, s.Upgraded, s.RebootRequired = osupdate.PhaseDone, &zero, nil, len(fakePackages), true
			s.Log = []string{"Setting up raspi-firmware (1:1.20260917-1) ...", "Setting up libc6:arm64 (2.41-12) ..."}
		}
		s.Unit, s.FinishedAt = "", now
		f.os = s
	}
	return &s, nil
}

func (f *fake) osTook(phase string) time.Duration {
	if phase == osupdate.PhaseUpdating {
		return 6 * time.Second
	}
	return 3 * time.Second
}

func (f *fake) StartOSUpdate(ctx context.Context, op string) (*osupdate.State, error) {
	if op != "check" && op != "upgrade" {
		return nil, api.Invalid("OS updates: check or upgrade")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.os.Running() {
		s := f.os
		return &s, api.Errorf(409, "%v", osupdate.ErrRunning)
	}
	f.os.Phase, f.os.Unit, f.os.StartedAt, f.os.Error = osupdate.PhaseChecking, "trellis-os-"+op+"-demo", time.Now().UTC(), ""
	if op == "upgrade" {
		f.os.Phase = osupdate.PhaseUpdating
	}
	s := f.os
	return &s, nil
}
