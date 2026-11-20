package hoststat

import (
	"fmt"
	"strings"
	"time"

	"meridian/trellis/internal/status"
)

// Thresholds. A sensor is warm within WarmMarginC of where the board slows
// itself (HotC), and close to shutting down within CritMarginC of its
// critical point. A filesystem past FullWarnPct is nearly full, past
// FullBadPct full; memory below MemWarnPct available is low. A low supply
// seen within UndervoltageRecent is still reported.
const (
	WarmMarginC        = 10
	CritMarginC        = 5
	FullWarnPct        = 90
	FullBadPct         = 97
	MemWarnPct         = 10
	MemBadPct          = 5
	UndervoltageRecent = 10 * time.Minute
)

// Finding is something about the host worth an alert.
type Finding struct {
	Level   string // info, warning, error
	Code    string
	Message string
}

// Check says what in a sample is worth an alert.
func Check(h status.Host, now time.Time) []Finding {
	var out []Finding
	add := func(level, code, format string, args ...any) {
		out = append(out, Finding{level, code, fmt.Sprintf(format, args...)})
	}
	// One temperature alert, the most severe of any sensor: a cooler
	// sensor near its critical point outranks a hotter one near where the
	// board slows. Of equals, the first (the hottest) speaks for the board.
	var hot *Finding
	rank := map[string]int{"info": 1, "warning": 2, "error": 3}
	for _, t := range h.Temps {
		what := sensorLabel(t.Name)
		var f Finding
		switch {
		case t.CritC > 0 && t.TempC >= t.CritC-CritMarginC:
			f = Finding{"error", "host-hot", fmt.Sprintf("%s temperature critical: %.0f °C (shutdown at %.0f °C). Cool the node now.", what, t.TempC, t.CritC)}
		case t.TempC >= t.HotC:
			f = Finding{"warning", "host-hot", fmt.Sprintf("%s temperature high: %.0f °C (throttle at %.0f °C). Improve cooling.", what, t.TempC, t.HotC)}
		case t.TempC >= t.HotC-WarmMarginC:
			f = Finding{"info", "host-hot", fmt.Sprintf("%s temperature %.0f °C (throttle at %.0f °C).", what, t.TempC, t.HotC)}
		default:
			continue
		}
		if hot == nil || rank[f.Level] > rank[hot.Level] {
			hot = &f
		}
	}
	if hot != nil {
		out = append(out, *hot)
	}
	switch {
	case h.Undervoltage != nil && *h.Undervoltage:
		add("warning", "undervoltage", "Supply undervoltage: CPU throttled, radio and USB dropouts possible. Use a higher-rated supply or a shorter cable.")
	case h.UndervoltageAt != nil && now.Sub(*h.UndervoltageAt) < UndervoltageRecent:
		add("info", "undervoltage", "Supply undervoltage %s ago. Supply at its limit under load.", ago(now.Sub(*h.UndervoltageAt)))
	}
	for _, f := range h.Filesystems {
		pct := f.UsedPct()
		switch {
		case pct >= FullBadPct:
			add("error", "disk-full", "Filesystem %s %.0f%% full (%s free). Logging and upgrades may fail.", f.Mount, pct, Bytes(f.Avail))
		case pct >= FullWarnPct:
			add("warning", "disk-full", "Filesystem %s %.0f%% full (%s free).", f.Mount, pct, Bytes(f.Avail))
		}
	}
	if h.MemTotal > 0 {
		pct := 100 * float64(h.MemAvailable) / float64(h.MemTotal)
		switch {
		case pct < MemBadPct:
			add("error", "memory-low", "Memory low: %s free (%.0f%%). Processes may be killed.", Bytes(h.MemAvailable), pct)
		case pct < MemWarnPct:
			add("warning", "memory-low", "Memory low: %s free (%.0f%%).", Bytes(h.MemAvailable), pct)
		}
	}
	return out
}

// sensorLabel names a thermal sensor in an alert: "CPU" for the
// processor's or SoC's, else the sensor by its name (acpitz, nvme).
func sensorLabel(name string) string {
	n := strings.ToLower(name)
	for _, cpu := range []string{"cpu", "soc", "x86_pkg_temp", "coretemp", "k10temp", "zenpower"} {
		if strings.Contains(n, cpu) {
			return "CPU"
		}
	}
	if name == "" {
		return "Sensor"
	}
	return "Sensor " + name
}

func ago(d time.Duration) string {
	if d < time.Minute {
		return "<1 min"
	}
	return fmt.Sprintf("%d min", int(d.Minutes()))
}

// Bytes is a size in decimal units, as the web UI shows sizes: "512 MB",
// "1.9 GB", "30 GB".
func Bytes(n uint64) string {
	const k = 1000
	units := []string{"B", "kB", "MB", "GB", "TB"}
	v, i := float64(n), 0
	for v >= k && i < len(units)-1 {
		v /= k
		i++
	}
	if v < 10 && i > 0 {
		return fmt.Sprintf("%.1f %s", v, units[i])
	}
	return fmt.Sprintf("%.0f %s", v, units[i])
}
