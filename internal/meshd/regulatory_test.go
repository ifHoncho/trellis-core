package meshd

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"meridian/trellis/internal/config"
	"meridian/trellis/internal/nl80211"
	"meridian/trellis/internal/regdb"
)

// fakeReg is the kernel's regulatory domain, and optionally trellis crda
// answering each request by the profile's mode.
type fakeReg struct {
	cur     *nl80211.RegDomain
	asked   []string
	answers bool
	mode    func() string
}

var countryRules = []nl80211.RegRule{{StartKHz: 2400000, EndKHz: 2472000, MaxBandwidthKHz: 40000, MaxEIRPMBm: 3000},
	{StartKHz: 5250000, EndKHz: 5350000, MaxBandwidthKHz: 80000, MaxEIRPMBm: 2400, Flags: nl80211.NL80211_RRF_DFS}}

func (f *fakeReg) GetReg() (*nl80211.RegDomain, error) { return f.cur, nil }
func (f *fakeReg) SetReg(a string) error {
	f.asked = append(f.asked, a)
	if f.answers {
		if f.mode() == config.RegModeUnrestricted {
			f.cur = regdb.Permissive(a)
		} else {
			f.cur = &nl80211.RegDomain{Alpha2: a, Rules: countryRules}
		}
	}
	return nil
}

func TestRegulatoryModes(t *testing.T) {
	run := t.TempDir()
	was, wasDir := regulatoryRule, regStateDir
	t.Cleanup(func() { regulatoryRule, regStateDir = was, wasDir })
	regStateDir = run
	regulatoryRule = filepath.Join(t.TempDir(), "85-trellis-regulatory.rules")
	profile := &config.Profile{Regulatory: config.Regulatory{Mode: config.RegModeCountry, Country: "US"}}
	d := &Daemon{log: slog.New(slog.NewTextHandler(io.Discard, nil)), profile: profile, opts: Options{RunDir: run}}
	r := &radioReconciler{d: d}
	k := &fakeReg{cur: &nl80211.RegDomain{Alpha2: "US", Rules: countryRules}, mode: func() string { return profile.Regulatory.Mode }}
	pass := func() ([]string, map[string]string) {
		var codes []string
		msgs := map[string]string{}
		reg, _ := r.regulatory(k, func(level, code, format string, args ...any) {
			codes = append(codes, level+":"+code)
			msgs[code] = format
			if len(args) > 0 {
				if s, ok := args[0].(string); ok {
					msgs[code] = s
				}
			}
		})
		msgs["active"], msgs["source"], msgs["note"] = reg.ActiveMode, reg.Source, reg.Note
		return codes, msgs
	}

	// The kernel's own database, country mode: nothing to do.
	codes, m := pass()
	if len(codes) != 0 || len(k.asked) != 0 || m["active"] != "country" || m["source"] != "kernel" {
		t.Fatalf("country mode on the kernel's database: %v %v asked %v", codes, m, k.asked)
	}
	// Unrestricted asked for, but nothing answers the kernel: say how to get it.
	profile.Regulatory.Mode = config.RegModeUnrestricted
	if codes, m = pass(); len(codes) != 1 || codes[0] != "warning:unrestricted-unavailable" || !strings.Contains(m["note"], "meshctl prepare") || len(k.asked) != 0 {
		t.Errorf("unrestricted without the path: %v %v asked %v", codes, m, k.asked)
	}
	// Installed, not yet rebooted.
	os.WriteFile(regulatoryRule, []byte("rule\n"), 0o644)
	if codes, m = pass(); len(codes) != 1 || !strings.Contains(m["note"], "next boot") {
		t.Errorf("unrestricted installed, before the reboot: %v %v", codes, m)
	}

	// After the reboot trellis crda answers: the mode switch goes through 00.
	regdb.WriteState(run, regdb.State{Alpha2: "US", Mode: "country", Source: "db", Time: time.Now()})
	k.answers = true
	codes, m = pass()
	if strings.Join(k.asked, ",") != "00,US" || m["active"] != "country" || m["source"] != "trellis" {
		t.Fatalf("switch to unrestricted: asked %v, %v %v", k.asked, codes, m)
	}
	if codes, m = pass(); len(codes) != 0 || m["active"] != "unrestricted" || !strings.Contains(m["note"], "without regulatory limits") {
		t.Errorf("unrestricted in force: %v %v", codes, m)
	}
	// Back to country mode right away (not after the 30 s retry),
	// and asked once, not on every pass.
	profile.Regulatory.Mode = config.RegModeCountry
	k.asked = nil
	pass()
	pass()
	if strings.Join(k.asked, ",") != "00,US" {
		t.Errorf("switch back asked %v, want 00,US once", k.asked)
	}
	if _, m = pass(); m["active"] != "country" {
		t.Errorf("country mode not back: %v", m)
	}

	// A switch the kernel does not take is reported after a while.
	k.answers = false
	profile.Regulatory.Mode = config.RegModeUnrestricted
	r.regSwitchAt = time.Time{}
	pass()
	r.regSwitchAt = time.Now().Add(-regSwitchWait - time.Second)
	if codes, _ = pass(); len(codes) != 1 || codes[0] != "error:regmode-not-applied" {
		t.Errorf("stuck switch: %v", codes)
	}

	// Another country in the kernel: asked for at once.
	k.asked, k.cur = nil, &nl80211.RegDomain{Alpha2: "00", Rules: countryRules}
	pass()
	if strings.Join(k.asked, ",") != "US" {
		t.Errorf("kernel in 00: asked %v, want US", k.asked)
	}
}

func TestRegBackoff(t *testing.T) {
	for tries, want := range map[int]time.Duration{0: 30 * time.Second, 1: 30 * time.Second, 2: time.Minute, 3: 2 * time.Minute, 5: 8 * time.Minute, 6: 10 * time.Minute, 40: 10 * time.Minute} {
		if got := regBackoff(tries); got != want {
			t.Errorf("after %d tries: %s, want %s", tries, got, want)
		}
	}
}
