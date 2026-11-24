package firstboot

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"meridian/trellis/internal/account"
)

// System returns Options with the node's own paths and calls.
func System(logf func(string, ...any)) Options {
	return Options{
		Account:     account.Host(),
		Hostname:    os.Hostname,
		SetHostname: func(name string) error { return syscall.Sethostname([]byte(name)) },
		ModuleLoaded: func(name string) bool {
			_, err := os.Stat("/sys/module/" + name)
			return err == nil
		},
		ReloadModule: func(name string) error {
			if out, err := exec.Command("modprobe", "-r", name).CombinedOutput(); err != nil {
				return fmt.Errorf("modprobe -r %s: %v: %s", name, err, strings.TrimSpace(string(out)))
			}
			if out, err := exec.Command("modprobe", name).CombinedOutput(); err != nil {
				return fmt.Errorf("modprobe %s: %v: %s", name, err, strings.TrimSpace(string(out)))
			}
			return nil
		},
		Logf: logf,
	}
}
