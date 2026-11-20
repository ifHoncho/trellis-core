package pcie

import (
	"meridian/trellis/internal/status"
)

// Health values of status.PCIeLink.
const (
	HealthOK       = "ok"
	HealthMarginal = "marginal"
	HealthKernel   = "kernel"
	HealthUnknown  = "unknown"
)

// Status joins a link's facts with a sample of it (nil: not sampled) as
// status shows them.
func Status(pl Link, h *Health) *status.PCIeLink {
	l := &status.PCIeLink{
		Device: pl.Device, Port: pl.Port, SpeedGTs: pl.SpeedGTs, Width: pl.Width, MaxSpeedGTs: pl.MaxSpeedGTs, MaxWidth: pl.MaxWidth,
		Gen: Gen(pl.SpeedGTs), MaxGen: Gen(pl.MaxSpeedGTs),
		Counted: pl.Counted, Correctable: pl.Correctable, NonFatal: pl.NonFatal, Fatal: pl.Fatal,
		Health: HealthUnknown,
	}
	if h == nil {
		return l
	}
	l.SampledAt, l.HealthDetail = h.Time, h.Describe()
	l.ReplayPct, l.PeakReplayPct = h.ReplayPct(), h.PeakReplayPct
	l.Errors = append(append([]string{}, h.PortErrors...), h.DeviceErrors...)
	switch {
	case h.Error != "":
	case h.Mode == ModeKernel:
		l.Health = HealthKernel
	case h.Marginal() || h.PeakReplayPct > 0:
		l.Health = HealthMarginal
	case h.Windows > 0:
		l.Health = HealthOK
	}
	return l
}
