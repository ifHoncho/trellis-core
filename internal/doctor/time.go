package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// checkTime looks at mesh time: chrony, and a GNSS receiver
// that shares its serial port with the kernel console. On such a port the
// receiver's sentences reach the console and its login prompt, and a
// serial BREAK followed by a "b" reboots the node (Magic SysRq).
func checkTime(r *Report) {
	if _, err := exec.LookPath("chronyd"); err == nil || fileExists("/usr/sbin/chronyd") {
		r.add(Pass, "time service", "chrony (serves and follows mesh time)", "")
	} else {
		r.add(Warn, "time service", "chrony is not installed: this node neither serves nor follows mesh time", "sudo apt install chrony")
	}
	cmdline, _ := os.ReadFile("/proc/cmdline")
	gpsd, err := os.ReadFile("/etc/default/gpsd")
	if err != nil {
		return
	}
	dev, console := gnssOnConsole(string(cmdline), string(gpsd), resolveDev)
	if dev == "" {
		r.add(Pass, "GNSS serial port", "gpsd's receiver is not on the kernel console", "")
		return
	}
	fix := "remove console=" + console + " from the kernel command line (/boot/firmware/cmdline.txt on Raspberry Pi OS), mask serial-getty@" + filepath.Base(dev) + ".service, and reboot"
	if b, err := os.ReadFile("/proc/sys/kernel/sysrq"); err == nil && strings.TrimSpace(string(b)) != "0" {
		fix += "; set kernel.sysrq = 0 as a backstop"
	}
	r.add(Fail, "GNSS serial port", "gpsd reads "+dev+", which is also the kernel console (console="+console+"): the receiver's output can reboot the node", fix)
}

// gnssOnConsole returns the device gpsd reads that is also a kernel
// console, and the console= value naming it; empty when there is none.
// resolve turns a device path into its target (symlinks such as
// /dev/serial0).
func gnssOnConsole(cmdline, gpsdDefaults string, resolve func(string) string) (dev, console string) {
	consoles := map[string]string{}
	for _, f := range strings.Fields(cmdline) {
		v, ok := strings.CutPrefix(f, "console=")
		if !ok {
			continue
		}
		name, _, _ := strings.Cut(v, ",")
		if name == "" || strings.HasPrefix(name, "tty") && len(name) > 3 && name[3] >= '0' && name[3] <= '9' {
			continue // the screen (tty0, tty1)
		}
		consoles[resolve("/dev/"+name)] = v
	}
	for _, line := range strings.Split(gpsdDefaults, "\n") {
		v, ok := strings.CutPrefix(strings.TrimSpace(line), "DEVICES=")
		if !ok {
			continue
		}
		for _, d := range strings.Fields(strings.Trim(v, `"'`)) {
			if c, ok := consoles[resolve(d)]; ok {
				return d, c
			}
		}
	}
	return "", ""
}

func resolveDev(path string) string {
	if p, err := filepath.EvalSymlinks(path); err == nil {
		return p
	}
	return path
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
