package meshd

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"meridian/trellis/internal/hoststat"
)

// The hardware reconciler raises the host's alerts with the radios'.
func TestHardwareHostAlerts(t *testing.T) {
	root := hwTree(t, "2.5 GT/s PCIe", "54000", "100")
	d, r := hwDaemon(t, root)
	r.startUnit = func(context.Context) error { return nil }
	proc := t.TempDir()
	put := func(dir, rel, v string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	put(root, "class/thermal/thermal_zone0/type", "cpu-thermal\n")
	put(root, "class/thermal/thermal_zone0/temp", "45000\n")
	put(root, "class/hwmon/hwmon7/name", "rpi_volt\n")
	put(root, "class/hwmon/hwmon7/in0_lcrit_alarm", "0\n")
	put(proc, "self/mountinfo", "")
	put(proc, "meminfo", "MemTotal: 1000 kB\nMemAvailable: 500 kB\n")
	d.host = hoststat.New(proc, root)

	res, err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := codes(res); got != "" {
		t.Errorf("healthy host: warnings %s", got)
	}

	put(root, "class/thermal/thermal_zone0/temp", "83000\n")
	put(root, "class/hwmon/hwmon7/in0_lcrit_alarm", "1\n")
	r.now = func() time.Time { return time.Now().Add(2 * time.Second) } // past the sample cache
	res, _ = r.Reconcile(context.Background())
	if got := codes(res); got != "warning:host-hot warning:undervoltage" {
		t.Errorf("hot and short of power: warnings %s", got)
	}
}
