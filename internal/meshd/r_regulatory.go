package meshd

import (
	"fmt"
	"os"
	"time"

	"meridian/trellis/internal/config"
	"meridian/trellis/internal/nl80211"
	"meridian/trellis/internal/regdb"
	"meridian/trellis/internal/status"
)

// regulatoryRule is the udev rule scripts/install-regulatory.sh installs:
// with it, "trellis crda" answers the kernel's regulatory requests
// (architecture §5.1, "How unrestricted is implemented").
var regulatoryRule = regdb.UdevRule

// regStateDir is where trellis crda records its answers.
var regStateDir = regdb.StateDir

// Retry and patience for regulatory requests.
const (
	regRetry      = 30 * time.Second // a request the kernel has not applied is sent again
	regPatience   = 10 * time.Second // then it is reported
	regSwitchWait = 15 * time.Second // a mode switch still pending is reported
)

// regNL is the part of nl80211 the regulatory domain needs.
type regNL interface {
	GetReg() (*nl80211.RegDomain, error)
	SetReg(alpha2 string) error
}

// regBackoff is how long to wait before asking again for a mode the kernel
// did not take after tries attempts.
func regBackoff(tries int) time.Duration {
	d := regRetry
	for i := 1; i < tries && d < 10*time.Minute; i++ {
		d *= 2
	}
	return min(d, 10*time.Minute)
}

// regulatory keeps the kernel's regulatory domain on the profile's country,
// and, where Trellis answers the kernel's requests, on the profile's mode.
// The mode in force is read from the kernel's rules (Trellis's permissive
// set or not), not taken from what the helper says it sent: udev runs it
// in parallel, and a late answer may be refused. It returns the status and
// whether it asked the kernel for something.
func (r *radioReconciler) regulatory(nl regNL, warn func(level, code, format string, args ...any)) (status.Regulatory, bool) {
	d := r.d
	want := d.profile.Regulatory.Country
	wantUnrestricted := d.profile.Regulatory.Mode == config.RegModeUnrestricted
	reg := status.Regulatory{RequestedMode: d.profile.Regulatory.Mode, Country: want, ActiveMode: config.RegModeCountry, Source: "kernel"}
	answered, _ := regdb.ReadState(regStateDir) // nil: trellis crda has answered nothing since boot
	if answered != nil {
		reg.Source = "trellis"
	}
	rd, err := nl.GetReg()
	if err != nil {
		warn("warning", "regdomain", "cannot read regulatory domain: %v", err)
		return reg, false
	}
	reg.KernelAlpha2 = rd.Alpha2
	permissive := regdb.IsPermissive(rd)
	if permissive {
		reg.ActiveMode = config.RegModeUnrestricted
	}

	changed := false
	if rd.Alpha2 != want {
		first := r.regApplied != want
		if first || time.Since(r.regAppliedAt) > regRetry {
			if err := nl.SetReg(want); err != nil {
				warn("warning", "regdomain", "cannot set regulatory domain %s: %v", want, err)
			} else {
				if first {
					d.log.Info("requested regulatory domain", "alpha2", want, "was", rd.Alpha2)
					r.regFirstAsk = time.Now()
					changed = true // retries are not changes
				}
				r.regApplied, r.regAppliedAt = want, time.Now()
			}
		}
		if !r.regFirstAsk.IsZero() && time.Since(r.regFirstAsk) > regPatience {
			msg := fmt.Sprintf("the kernel is still in regulatory domain %s %s after meshd requested %s", rd.Alpha2, time.Since(r.regFirstAsk).Round(time.Second), want)
			switch {
			case answered != nil && answered.Error != "":
				msg += "; trellis crda's last answer was refused: " + answered.Error + " (journalctl -t trellis-crda)"
			case answered != nil:
				msg += "; trellis crda answers the kernel: see journalctl -t trellis-crda"
			case rd.Alpha2 == "00":
				msg += "; it most likely rejects /lib/firmware/regulatory.db (signature). Run sudo scripts/install-regulatory.sh and reboot, or see sudo meshctl doctor"
			}
			warn("error", "regdomain-not-applied", "%s", msg)
		}
	} else {
		r.regFirstAsk = time.Time{}
		// The country is right but the rules are the other mode's (the
		// profile's mode changed). A request for the domain in force is
		// ignored, so ask for the world domain first; the kernel handles
		// requests in order, and trellis crda answers both by the new mode.
		// A new mode is asked for at once; the same one again after a wait
		// that doubles each time (30 s up to 10 min), since each round
		// takes the kernel through the world domain and back.
		mode := d.profile.Regulatory.Mode
		if wantUnrestricted == permissive || r.regSwitchTo != mode {
			r.regSwitchTries = 0
		}
		if answered != nil && wantUnrestricted != permissive && (r.regSwitchTo != mode || time.Since(r.regSwitchAt) > regBackoff(r.regSwitchTries)) {
			err := nl.SetReg("00")
			if err == nil {
				err = nl.SetReg(want)
			}
			if err != nil {
				warn("warning", "regdomain", "cannot ask for the %s rules: %v", d.profile.Regulatory.Mode, err)
			} else {
				d.log.Info("asked the kernel again for the regulatory rules of the profile's mode", "alpha2", want, "mode", d.profile.Regulatory.Mode)
				changed = true
			}
			r.regSwitchAt, r.regSwitchTo = time.Now(), mode
			r.regSwitchTries++
		}
	}

	switching := answered != nil && rd.Alpha2 == want && wantUnrestricted != permissive
	switch {
	case wantUnrestricted && permissive:
		reg.Note = "Trellis's rules, without regulatory limits: each radio's driver and firmware set its power"
	case wantUnrestricted && answered == nil:
		if _, err := os.Stat(regulatoryRule); err == nil {
			reg.Note = "unrestricted mode is installed and takes effect at the next boot (the kernel keeps the regulatory database it loaded at start); operating under " + want + " limits until then"
		} else {
			reg.Note = "unrestricted mode needs Trellis to answer the kernel's regulatory requests: run sudo scripts/install-regulatory.sh (make deploy does) and reboot; operating under " + want + " limits"
		}
		warn("warning", "unrestricted-unavailable", "%s", reg.Note)
	case switching:
		reg.Note = "switching to the " + d.profile.Regulatory.Mode + " rules"
		if !r.regSwitchAt.IsZero() && time.Since(r.regSwitchAt) > regSwitchWait {
			msg := fmt.Sprintf("the kernel still has the %s rules %s after meshd asked for the %s ones", reg.ActiveMode, time.Since(r.regSwitchAt).Round(time.Second), d.profile.Regulatory.Mode)
			if answered.Error != "" {
				msg += "; trellis crda's last answer was refused: " + answered.Error
			}
			warn("error", "regmode-not-applied", "%s (journalctl -t trellis-crda)", msg)
		}
	}
	return reg, changed
}
