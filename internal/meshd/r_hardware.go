package meshd

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"os/exec"
	"slices"
	"strings"
	"time"

	"meridian/trellis/internal/gossip"
	"meridian/trellis/internal/hoststat"
	"meridian/trellis/internal/pcie"
	"meridian/trellis/internal/radio"
	"meridian/trellis/internal/reconcile"
	"meridian/trellis/internal/status"
	"meridian/trellis/internal/sysfs"
)

// pcieUnit samples the error status of PCIe radios' links (pcie.Watch). It
// has the one capability that takes, which meshd and the helpers that
// inherit meshd's bounds do without; meshd starts it on a node with a PCIe
// radio and reads its report.
const pcieUnit = "trellis-pcie.service"

// bcm2711RootPort is the PCIe root port of the BCM2711 (Pi 4, CM4, Pi 400).
const bcm2711RootPort = "14e4:2711"

// hotMarginC is how close to its throttling temperature a radio is called
// hot.
const hotMarginC = 10

// hardwareFacts is what the hardware reconciler last read, by radio
// fingerprint.
type hardwareFacts struct {
	PCIe    map[string]*status.PCIeLink
	Thermal map[string]*status.RadioThermal
	// Degraded is why each radio runs but not as it should, as the
	// warning-level alerts say: gossip.DegradedThermal, DegradedPCIe.
	Degraded map[string][]string
}

// hardwareReconciler watches what the radios' hardware says about itself:
// the PCIe link a radio sits on, and its temperature and throttling. It
// changes nothing but the watch unit's state.
type hardwareReconciler struct {
	d *Daemon
	// reportPath is the watch's report; startUnit starts the watch.
	// Variable for tests.
	reportPath string
	startUnit  func(ctx context.Context) error
	startTried time.Time
	now        func() time.Time
	// thermal is each radio's temperature as last read, by fingerprint,
	// and thermalAt when: a read makes some drivers ask the card (mt76
	// sends it a command), so it is not done on every pass.
	thermal   map[string]sysfs.Thermal
	thermalAt time.Time
}

// thermalEvery is the time between reads of the radios' temperature.
const thermalEvery = time.Minute

func newHardwareReconciler(d *Daemon) *hardwareReconciler {
	return &hardwareReconciler{d: d, reportPath: pcie.DefaultReportPath, now: time.Now,
		startUnit: func(ctx context.Context) error {
			return exec.CommandContext(ctx, "systemctl", "start", "--no-block", pcieUnit).Run()
		}}
}

func (r *hardwareReconciler) Name() string { return "hardware" }

func (r *hardwareReconciler) Teardown(context.Context) error { return nil }

func (r *hardwareReconciler) Reconcile(ctx context.Context) (reconcile.Result, error) {
	var res reconcile.Result
	warn := func(level, code, format string, args ...any) {
		res.Warnings = append(res.Warnings, reconcile.Warning{Level: level, Code: code, Message: fmt.Sprintf(format, args...)})
	}
	var radios []*radio.Radio
	r.d.facts.Read(func(f *Facts) { radios = f.Radios })
	hw := hardwareFacts{PCIe: map[string]*status.PCIeLink{}, Thermal: map[string]*status.RadioThermal{}, Degraded: map[string][]string{}}
	now := r.now()
	report := pcie.ReadReport(r.reportPath, now)
	links := map[string]*status.PCIeLink{} // by PCI address: one link per card
	badLink := map[string]bool{}           // by PCI address
	hasPCI := false
	if r.thermal == nil || now.Sub(r.thermalAt) >= thermalEvery {
		r.thermal, r.thermalAt = map[string]sysfs.Thermal{}, now
		for _, rd := range radios {
			if t, ok := sysfs.RadioThermal(r.d.opts.SysRoot, rd.Phy); ok && !rd.Ignored {
				r.thermal[rd.Fingerprint] = t
			}
		}
	}
	for _, rd := range radios {
		if rd.Ignored {
			continue
		}
		if t, ok := r.thermal[rd.Fingerprint]; ok {
			th := &status.RadioThermal{TempC: t.TempC, ThrottleAtC: t.MaxC, DutyPct: t.DutyPct, Throttling: t.Throttling()}
			hw.Thermal[rd.Fingerprint] = th
			switch {
			case th.Throttling:
				hw.Degraded[rd.Fingerprint] = append(hw.Degraded[rd.Fingerprint], gossip.DegradedThermal)
				warn("warning", "radio-throttling", "%s thermal throttling: %.0f °C, Tx duty %d%%. Improve card cooling.",
					radioWithBands(rd), th.TempC, th.DutyPct)
			case th.ThrottleAtC > 0 && th.TempC >= th.ThrottleAtC-hotMarginC:
				warn("info", "radio-hot", "%s temperature %.0f °C (throttle at %.0f °C).", radioWithBands(rd), th.TempC, th.ThrottleAtC)
			}
		}
		if rd.Device.Bus != sysfs.BusPCI {
			continue
		}
		hasPCI = true
		addr := strings.TrimPrefix(rd.Device.Port, "pci:")
		if l, done := links[addr]; done {
			hw.PCIe[rd.Fingerprint] = l
			if badLink[addr] {
				hw.Degraded[rd.Fingerprint] = append(hw.Degraded[rd.Fingerprint], gossip.DegradedPCIe)
			}
			continue
		}
		pl, err := pcie.ReadLink(r.d.opts.SysRoot, addr)
		if err != nil {
			continue
		}
		var sample *pcie.Health
		if h, ok := report.Find(addr); ok {
			sample = &h
		}
		l := pcie.Status(pl, sample)
		links[addr] = l
		hw.PCIe[rd.Fingerprint] = l
		if badLink[addr] = pcieWarnings(rd, pl, l, warn); badLink[addr] {
			hw.Degraded[rd.Fingerprint] = append(hw.Degraded[rd.Fingerprint], gossip.DegradedPCIe)
		}
	}
	r.d.facts.Update(func(f *Facts) { f.Hardware = hw })
	// The computer itself: sampled on every pass too, so a short drop in
	// the supply is caught even with nobody asking for status.
	if r.d.host != nil {
		for _, f := range hoststat.Check(r.d.host.Sample(now), now) {
			warn(f.Level, f.Code, "%s", f.Message)
		}
	}
	if w, ok := resetWarning(r.d.crashes, now); ok {
		res.Warnings = append(res.Warnings, w)
	}
	// The watch: wanted while there is a PCIe radio. Asked for again now
	// and then, in case it was stopped; a package without the unit is left
	// in peace after the first try.
	if hasPCI && report == nil && !r.d.opts.NoHostConfig && (r.startTried.IsZero() || now.Sub(r.startTried) > 5*time.Minute) {
		r.startTried = now
		if err := r.startUnit(ctx); err != nil {
			r.d.log.Debug("could not start the PCIe link watch", "unit", pcieUnit, "err", err)
		}
	}
	return res, nil
}

// pcieWarnings reports what is wrong with one card's link, and whether
// that degrades the card's radios (a warning, not a note).
func pcieWarnings(rd *radio.Radio, pl pcie.Link, l *status.PCIeLink, warn func(level, code, format string, args ...any)) (degraded bool) {
	card := rd.Name()
	if rd.HWDB != nil && rd.HWDB.Chipset != "" {
		card = rd.HWDB.Chipset
	}
	where := fmt.Sprintf("%s (%s)", card, pl.Device)
	// install-mt7915e-dkms.sh limits the link to Gen1 for every card on
	// mt7915e (MT7915 and MT7916) on this board; meshctl doctor tests the
	// same.
	gen1ByDesign := pl.PortID == bcm2711RootPort && rd.Device.Driver == "mt7915e"
	switch {
	case l.Health == pcie.HealthMarginal:
		degraded = true
		warn("warning", "pcie-link-marginal", "PCIe link to %s marginal: %s. Risk of card hang and node reset. Reseat card and adapter, or change adapter or slot.",
			where, l.HealthDetail)
	case pl.Fatal > 0 || pl.NonFatal > 0:
		degraded = true
		warn("warning", "pcie-errors", "PCIe link to %s: %d uncorrectable errors since boot (%d fatal). Check card seating and power.",
			where, pl.Fatal+pl.NonFatal, pl.Fatal)
	case pl.Correctable > 0:
		warn("info", "pcie-errors", "PCIe link to %s: %d corrected errors since boot.", where, pl.Correctable)
	}
	switch {
	case gen1ByDesign && l.Gen >= 2:
		degraded = true
		warn("warning", "pcie-gen2", "PCIe link to %s at Gen%d: unstable with M.2 adapters on this board. Limit to Gen1: sudo /usr/share/trellis/scripts/install-mt7915e-dkms.sh, then reboot.",
			where, l.Gen)
	case gen1ByDesign:
		// Gen1 is what Trellis sets on this board.
	case l.Gen > 0 && l.MaxGen > l.Gen, l.Width > 0 && l.MaxWidth > l.Width:
		warn("info", "pcie-link-degraded", "PCIe link to %s at Gen%d x%d (supported: Gen%d x%d).",
			where, l.Gen, l.Width, l.MaxGen, l.MaxWidth)
	}
	return degraded
}

// radioWithBands names a radio with its bands: a dual-band card's two
// radios share a name.
func radioWithBands(rd *radio.Radio) string {
	if len(rd.Caps.Bands) == 0 {
		return rd.Name()
	}
	return rd.Name() + " (" + strings.Join(rd.Caps.Bands, "/") + ")"
}

// gossipDegraded is this node's degraded radios for its gossip record,
// sorted; nil while every radio is fine.
func (d *Daemon) gossipDegraded() []gossip.Degraded {
	var out []gossip.Degraded
	d.facts.Read(func(f *Facts) {
		for _, rd := range f.Radios {
			if rd.Ignored || len(rd.Caps.Bands) == 0 {
				continue
			}
			for _, why := range f.Hardware.Degraded[rd.Fingerprint] {
				out = append(out, gossip.Degraded{Radio: gossip.RadioEntry(rd.Caps.Bands), Why: why})
			}
		}
	})
	slices.SortFunc(out, func(a, b gossip.Degraded) int {
		return cmp.Or(cmp.Compare(a.Radio, b.Radio), cmp.Compare(a.Why, b.Why))
	})
	out = slices.Compact(out)
	if len(out) > gossip.MaxDegraded {
		out = out[:gossip.MaxDegraded]
	}
	return out
}

// widthCost is what a mesh link's width costs in link budget on the radio
// that carries it: at the width it runs at, else the profile's.
func widthCost(rd *radio.Radio, ml status.MeshLink) *status.WidthCost {
	width := ml.RunningWidthMHz
	if width == 0 {
		width = ml.WidthMHz
	}
	c, ok := rd.HWDB.WidthCost(ml.Band, width)
	if !ok {
		return nil
	}
	round := func(v float64) float64 { return math.Round(v*10) / 10 }
	return &status.WidthCost{WidthMHz: c.WidthMHz, BaseMHz: c.BaseMHz, NoiseDB: round(c.NoiseDB), PowerKnown: c.PowerKnown,
		PowerDBm: c.PowerDBm, BasePowerDBm: c.BasePowerDBm, PowerDB: round(c.PowerDB), TotalDB: round(c.TotalDB)}
}
