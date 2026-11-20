package trial

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"meridian/trellis/internal/crashlog"
)

// System is the real host: modules are reloaded with modprobe, with meshd
// stopped meanwhile (it holds the radios), and the module index is rebuilt
// when a module file changed.
type System struct {
	// StateDir is the node's state directory: meshd's reset history
	// there counts a crash during the reload (crashlog.Reloading).
	StateDir string
}

const meshdUnit = "trellis-meshd.service"

// neverReload are modules that must not be reloaded with their adapter
// attached: the AR9271 keeps its firmware across a reload and the driver
// cannot upload it again, so the radio is gone until it is replugged.
var neverReload = map[string]bool{"ath9k_htc": true}

func (sys System) Reload(module string) error {
	if neverReload[module] {
		return fmt.Errorf("%s is not reloaded with its adapter attached: replug the adapter, or reboot", module)
	}
	if _, err := os.Stat("/sys/module/" + strings.ReplaceAll(module, "-", "_")); err != nil {
		// Not loaded: it reads the files when it next loads.
		return nil
	}
	restart := exec.Command("systemctl", "is-active", "--quiet", meshdUnit).Run() == nil
	if restart {
		if err := exec.Command("systemctl", "stop", meshdUnit).Run(); err != nil {
			return fmt.Errorf("cannot stop meshd to reload %s: %w", module, err)
		}
	}
	done := func() {}
	if restart && sys.StateDir != "" {
		done = crashlog.Reloading(sys.StateDir, crashlog.BootID())
	}
	var errs []error
	if out, err := exec.Command("modprobe", "-r", module).CombinedOutput(); err != nil {
		errs = append(errs, fmt.Errorf("modprobe -r %s: %s", module, firstLine(string(out)+" "+err.Error())))
	} else if out, err := exec.Command("modprobe", module).CombinedOutput(); err != nil {
		errs = append(errs, fmt.Errorf("modprobe %s: %s", module, firstLine(string(out)+" "+err.Error())))
	}
	done()
	if restart {
		if err := exec.Command("systemctl", "start", meshdUnit).Run(); err != nil {
			errs = append(errs, fmt.Errorf("meshd did not start again: %w", err))
		}
	}
	return errors.Join(errs...)
}

var reModuleDir = regexp.MustCompile(`/lib/modules/([^/]+)/`)

func (System) FilesChanged(paths []string) {
	seen := map[string]bool{}
	for _, p := range paths {
		m := reModuleDir.FindStringSubmatch(p)
		if m == nil || seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		_ = exec.Command("depmod", "-a", m[1]).Run()
	}
}
