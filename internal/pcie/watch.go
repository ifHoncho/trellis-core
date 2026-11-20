package pcie

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"meridian/trellis/internal/fsutil"
)

// Where the watch leaves its report, and how it samples.
const (
	// DefaultReportPath is written by the watch (trellis-pcie.service)
	// and read by meshd and meshctl doctor.
	DefaultReportPath = "/run/trellis-pcie/links.json"
	// SampleWindows windows of SampleWindow make one sample: a second of
	// the port's status, read a hundred times.
	SampleWindows = 100
	SampleWindow  = 10 * time.Millisecond
	// WatchInterval is the time between samples of one link.
	WatchInterval = 15 * time.Second
	// PeakWindow is how far back Health.PeakReplayPct looks: long enough
	// to still show what a burst of traffic did a few minutes ago.
	PeakWindow = 10 * time.Minute
	// StaleAfter is the age past which a report says nothing about now.
	StaleAfter = 2 * time.Minute
)

// Report is what the watch found on every PCIe radio's link.
type Report struct {
	Time  time.Time `json:"time"`
	Links []Health  `json:"links"`
}

// Find returns the sample of a device's link, if the report has one.
func (r *Report) Find(device string) (Health, bool) {
	if r == nil {
		return Health{}, false
	}
	for _, h := range r.Links {
		if h.Device == device {
			return h, true
		}
	}
	return Health{}, false
}

// ReadReport reads the watch's report. A missing file, or one older than
// StaleAfter at now, is no report.
func ReadReport(path string, now time.Time) *Report {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var r Report
	if json.Unmarshal(b, &r) != nil || r.Time.IsZero() || now.Sub(r.Time) > StaleAfter {
		return nil
	}
	return &r
}

type peak struct {
	at  time.Time
	pct float64
}

// Watch samples the link of every PCIe radio each interval and writes the
// report, until ctx ends. It needs CAP_SYS_ADMIN (the registers) and
// nothing else. sample stands in for SampleDevice in tests.
func Watch(ctx context.Context, sysRoot, path string, interval time.Duration, sample func(addr string) Health) error {
	if sample == nil {
		sample = func(addr string) Health { return SampleDevice(sysRoot, addr, SampleWindows, SampleWindow) }
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	peaks := map[string][]peak{}
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = os.Remove(path)
			return nil
		case <-t.C:
		}
		rep := Report{Links: []Health{}}
		for _, addr := range RadioDevices(sysRoot) {
			h := sample(addr)
			now := h.Time
			ps := append(peaks[addr], peak{at: now, pct: h.ReplayPct()})
			for len(ps) > 0 && now.Sub(ps[0].at) > PeakWindow {
				ps = ps[1:]
			}
			peaks[addr] = ps
			for _, p := range ps {
				if p.pct > h.PeakReplayPct {
					h.PeakReplayPct = p.pct
				}
			}
			rep.Links = append(rep.Links, h)
		}
		rep.Time = time.Now()
		if b, err := json.Marshal(rep); err == nil {
			_ = fsutil.WriteFile(path, b, 0o644)
		}
		t.Reset(interval)
	}
}
