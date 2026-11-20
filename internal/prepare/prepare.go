// Package prepare readies a node's system for Trellis once its packages
// are installed (meshctl prepare): the pinned batman-adv built and loaded,
// mt7915e where the kernel lacks it, the radio driver options the hardware
// database asks for, and the regulatory rules in Trellis's hands. Each step
// is skipped when already done, so it is safe to run again; the hardware
// work itself is the scripts under /usr/share/trellis/scripts, which also
// work alone.
//
// A step that fails is reported and the others still run. A reboot a step
// needs is reported, and marked the Debian way (/run/reboot-required), so
// a tool driving many nodes can see it.
package prepare

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"meridian/trellis/internal/crashlog"
)

// DefaultDir is where the package puts the scripts and what they install.
const DefaultDir = "/usr/share/trellis"

// Host is the system prepare works on; a variable part for tests.
type Host interface {
	// Run runs a command with its output written to out, the variables in
	// env added to the environment. A command that ran and failed returns
	// an error with ExitCode.
	Run(ctx context.Context, out io.Writer, env []string, name string, args ...string) error
	// Output runs a command quietly and returns its standard output.
	Output(ctx context.Context, name string, args ...string) (string, error)
	ReadFile(path string) ([]byte, error)
	WriteFile(path string, data []byte, perm os.FileMode) error
	Glob(pattern string) ([]string, error)
}

// Options says what to prepare and where.
type Options struct {
	Dir  string // the package's files (DefaultDir)
	Host Host
	Out  io.Writer
	// DriverOptions are modprobe.d lines ("options mod key=value …") from
	// the hardware database.
	DriverOptions []string
	// StateDir is meshd's state directory: while meshd is stopped for a
	// module to load, its reset history counts a crash as a reset
	// (crashlog.Reloading). Empty: not marked. BootID is this boot's
	// (crashlog.BootID when empty).
	StateDir, BootID string
}

// Result says how it went.
type Result struct {
	Reboot   []string // why a reboot is needed, if it is
	Problems []string // steps that failed
	Advice   []string // things only a person can do (replug an adapter)
}

// ExitCode is how a failed command ended.
type ExitCode interface{ ExitCode() int }

// rebootLine is how the scripts say a step finishes only after a reboot.
var rebootLine = regexp.MustCompile(`REBOOT NEEDED|at the next boot|after a reboot`)

type run struct {
	ctx         context.Context
	o           Options
	res         Result
	stoppedMesh bool
}

// Run prepares the node.
func Run(ctx context.Context, o Options) (*Result, error) {
	if o.Dir == "" {
		o.Dir = DefaultDir
	}
	if _, err := o.Host.ReadFile(filepath.Join(o.Dir, "scripts", "install-regulatory.sh")); err != nil {
		return nil, fmt.Errorf("the Trellis package's scripts are not in %s: install the trellis package first", o.Dir)
	}
	r := &run{ctx: ctx, o: o}
	// A step cut short (Ctrl-C, the time limit) ends the run, and meshd
	// is started again all the same.
	for _, step := range []func(){r.batman, r.mt7915e, r.driverOptions, r.regulatory} {
		if ctx.Err() != nil {
			break
		}
		step()
	}
	if r.stoppedMesh {
		r.say("\n== meshd\n")
		if err := r.o.Host.Run(context.WithoutCancel(ctx), r.o.Out, nil, "systemctl", "start", "trellis-meshd"); err != nil {
			r.problem("meshd did not start again: %v", err)
		} else {
			r.say("-- started again\n")
		}
	}
	if len(r.res.Reboot) > 0 {
		_ = o.Host.WriteFile("/run/reboot-required", []byte("*** System restart required ***\n"), 0o644)
		pkgs, _ := o.Host.ReadFile("/run/reboot-required.pkgs")
		if !slices.Contains(strings.Fields(string(pkgs)), "trellis") {
			_ = o.Host.WriteFile("/run/reboot-required.pkgs", append(pkgs, "trellis\n"...), 0o644)
		}
	}
	if err := ctx.Err(); err != nil {
		return &r.res, fmt.Errorf("prepare stopped early: %w", err)
	}
	return &r.res, nil
}

func (r *run) say(format string, args ...any) { fmt.Fprintf(r.o.Out, format, args...) }

func (r *run) problem(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	r.res.Problems = append(r.res.Problems, msg)
	r.say("!! %s\n", msg)
}

// script runs one of the package's scripts, watching its output for a
// reboot it asks for.
func (r *run) script(env []string, name string, args ...string) error {
	w := &rebootWatch{out: r.o.Out}
	err := r.o.Host.Run(r.ctx, w, env, filepath.Join(r.o.Dir, "scripts", name), args...)
	w.flush()
	for _, l := range w.found {
		if !slices.Contains(r.res.Reboot, l) {
			r.res.Reboot = append(r.res.Reboot, l)
		}
	}
	return err
}

// stopMeshd stops meshd before a module it may hold is reloaded; Run
// starts it again at the end.
func (r *run) stopMeshd() {
	if r.stoppedMesh {
		return
	}
	if _, err := r.o.Host.Output(r.ctx, "systemctl", "is-active", "--quiet", "trellis-meshd"); err != nil {
		return // not running
	}
	r.say("-- stopping trellis-meshd for this (started again at the end)\n")
	if err := r.o.Host.Run(r.ctx, r.o.Out, nil, "systemctl", "stop", "trellis-meshd"); err == nil {
		r.stoppedMesh = true
	}
}

// loading runs a step that loads a kernel module with meshd stopped: a
// crash in it is a reset in meshd's history (crashlog.Reloading).
func (r *run) loading(step func()) {
	if r.stoppedMesh && r.o.StateDir != "" {
		boot := r.o.BootID
		if boot == "" {
			boot = crashlog.BootID()
		}
		defer crashlog.Reloading(r.o.StateDir, boot)()
	}
	step()
}

func (r *run) read(path string) string {
	b, _ := r.o.Host.ReadFile(path)
	return strings.TrimSpace(string(b))
}

func (r *run) output(name string, args ...string) string {
	s, _ := r.o.Host.Output(r.ctx, name, args...)
	return strings.TrimSpace(s)
}

// pin is the batman-adv release this node runs: the installed
// trellis-batman-adv-dkms package's, else the package's pin file.
func (r *run) pin() string {
	if v := r.output("dpkg-query", "-W", "-f=${Version}", "trellis-batman-adv-dkms"); v != "" {
		if i := strings.LastIndex(v, "-"); i > 0 {
			v = v[:i] // the Debian revision
		}
		return v
	}
	for _, l := range strings.Split(r.read(filepath.Join(r.o.Dir, "packaging", "dkms", "source")), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "BATADV_VERSION="); ok {
			return strings.Trim(v, `"'`)
		}
	}
	return ""
}

func (r *run) batman() {
	pin := r.pin()
	r.say("\n== batman-adv %s\n", pin)
	if pin == "" {
		r.problem("no batman-adv release pinned: the package's files are incomplete")
		return
	}
	loaded := r.read("/sys/module/batman_adv/version")
	avail := r.output("modinfo", "-F", "version", "batman-adv")
	dkms := strings.Contains(r.output("modinfo", "-n", "batman-adv"), "/updates/")
	// The same release built with other patches (a new packaging revision)
	// is another module: the loaded one must be the installed file.
	current := r.read("/sys/module/batman_adv/srcversion") == r.output("modinfo", "-F", "srcversion", "batman-adv")
	switch {
	case loaded == pin && dkms && current:
		r.say("-- %s (DKMS build) is loaded\n", pin)
		return
	case avail == pin && dkms:
		r.say("-- loading the installed %s DKMS build (was: %s)\n", pin, or(loaded, "not loaded"))
		r.stopMeshd()
		failed := false
		r.loading(func() {
			_ = r.o.Host.Run(r.ctx, r.o.Out, nil, "modprobe", "-r", "batman-adv")
			if err := r.o.Host.Run(r.ctx, r.o.Out, nil, "modprobe", "batman-adv"); err != nil {
				r.problem("batman-adv %s did not load: %v", pin, err)
				failed = true
			}
		})
		if failed {
			return
		}
	default:
		r.say("-- not built for this kernel (loaded: %s); building it (a few minutes)\n", or(loaded, "none"))
		r.stopMeshd() // so the script can load it
		var err error
		r.loading(func() { err = r.script([]string{"BATADV_VERSION=" + pin}, "install-batman-adv-dkms.sh") })
		if err != nil {
			r.problem("batman-adv %s did not build or load (%v); see above", pin, err)
			return
		}
	}
	if now := r.read("/sys/module/batman_adv/version"); now != "" {
		r.say("-- loaded: %s\n", now)
	}
}

// mt7915IDs are the PCI device IDs of MediaTek's MT7915 and MT7916;
// mt7916IDs the MT7916's.
var (
	mt7915IDs = []string{"0x7915", "0x7906", "0x790a"}
	mt7916IDs = []string{"0x7906", "0x790a"}
)

func (r *run) mt7915e() {
	r.say("\n== MT7915/MT7916 driver\n")
	devs, _ := r.o.Host.Glob("/sys/bus/pci/devices/*")
	var cards []string
	mt7916 := false
	for _, d := range devs {
		if id := r.read(d + "/device"); r.read(d+"/vendor") == "0x14c3" && slices.Contains(mt7915IDs, id) {
			cards = append(cards, filepath.Base(d))
			mt7916 = mt7916 || slices.Contains(mt7916IDs, id)
		}
	}
	have := r.output("modinfo", "-n", "mt7915e") != ""
	// An mt7915e older than Linux 6.14 registers both bands of an
	// MT7916's 5/6 GHz radio, and the radio then works on neither; the
	// fix shows as the module option enable_6ghz.
	old := have && mt7916 && !strings.Contains(r.output("modinfo", "-F", "parm", "mt7915e"), "enable_6ghz")
	switch {
	case len(cards) == 0:
		r.say("-- no MT7915/MT7916 card\n")
	case old && r.mt7915eBuilt():
		// This package's build is installed and still has no band
		// selection: the kernel's source is not the code the fix was
		// written for (band-select.sh refused it). Building again gives
		// the same driver.
		msg := "MT7916 5/6 GHz radio stays off the mesh: this kernel's mt7915e source does not take the band selection. Use Linux 6.14 or newer"
		r.res.Advice = append(r.res.Advice, msg)
		r.say("-- card %s; mt7915e (DKMS build) is available\n!! %s\n", strings.Join(cards, ", "), msg)
	case old:
		r.say("-- card %s; the mt7915e for this kernel cannot select the band of a 5/6 GHz radio (it predates Linux 6.14); building one that can (a few minutes)\n", strings.Join(cards, ", "))
		r.stopMeshd() // so the script can unload the one in use
		var err error
		r.loading(func() { err = r.script(nil, "install-mt7915e-dkms.sh") })
		if err != nil {
			r.problem("mt7915e was not replaced (%v); see above. The card's 5/6 GHz radio stays off the mesh", err)
		}
	case have:
		r.say("-- card %s; mt7915e is available\n", strings.Join(cards, ", "))
	default:
		r.say("-- card %s but no mt7915e for this kernel; building it (a few minutes)\n", strings.Join(cards, ", "))
		if err := r.script(nil, "install-mt7915e-dkms.sh"); err != nil {
			r.problem("mt7915e did not install (%v); see above. meshd runs without the card", err)
		}
	}
}

// mt7915eBuilt reports whether this package's mt7915e DKMS build (the
// version its install script names) is installed for the running kernel
// and is the module modprobe loads.
func (r *run) mt7915eBuilt() bool {
	if !strings.Contains(r.output("modinfo", "-n", "mt7915e"), "/updates/") {
		return false
	}
	v := ""
	for _, l := range strings.Split(r.read(filepath.Join(r.o.Dir, "scripts", "install-mt7915e-dkms.sh")), "\n") {
		if rest, ok := strings.CutPrefix(l, "PKG_VERSION="); ok {
			v = strings.Fields(rest + " ")[0]
			break
		}
	}
	kernel := r.read("/proc/sys/kernel/osrelease")
	if v == "" || kernel == "" {
		return false
	}
	// "trellis-mt7915e/2, 6.12.47+rpt-rpi-v8, aarch64: installed" (older
	// dkms: "trellis-mt7915e, 2, ...").
	for _, l := range strings.Split(r.output("dkms", "status", "-m", "trellis-mt7915e", "-v", v, "-k", kernel), "\n") {
		if strings.Contains(l, kernel) && strings.HasSuffix(strings.TrimSpace(l), ": installed") {
			return true
		}
	}
	return false
}

func (r *run) driverOptions() {
	r.say("\n== Radio driver options\n")
	if len(r.o.DriverOptions) == 0 {
		r.say("-- none needed\n")
		return
	}
	for _, line := range r.o.DriverOptions {
		f := strings.Fields(line)
		if len(f) < 3 || f[0] != "options" {
			continue
		}
		mod := f[1]
		if !r.exists("/sys/module/" + mod) {
			r.say("-- %s: not loaded; the options apply when it is\n", mod)
			continue
		}
		for _, kv := range f[2:] {
			k, want, _ := strings.Cut(kv, "=")
			have := r.read("/sys/module/" + mod + "/parameters/" + k)
			switch have {
			case "Y":
				have = "1"
			case "N":
				have = "0"
			}
			if have == want {
				continue
			}
			if mod == "ath9k_htc" {
				// The AR9271 keeps its firmware running across a module
				// reload, and the reloaded driver cannot upload it again:
				// the radio is gone until replugged.
				msg := fmt.Sprintf("%s runs with %s=%s and needs %s: unplug and replug the adapter once (or reboot)", mod, k, have, want)
				r.res.Advice = append(r.res.Advice, msg)
				r.say("!! %s\n", msg)
				break
			}
			r.say("-- reloading %s (%s=%s, needs %s)\n", mod, k, have, want)
			r.stopMeshd()
			r.loading(func() {
				if err := r.o.Host.Run(r.ctx, r.o.Out, nil, "modprobe", "-r", mod); err != nil {
					r.problem("could not unload %s (%v): unplug and replug the adapter, or reboot", mod, err)
					return
				}
				if err := r.o.Host.Run(r.ctx, r.o.Out, nil, "modprobe", mod); err != nil {
					r.problem("could not load %s again (%v): reboot", mod, err)
				}
			})
			break
		}
	}
	r.say("-- in place: %s\n", strings.Join(r.o.DriverOptions, "; "))
}

func (r *run) exists(path string) bool {
	m, _ := r.o.Host.Glob(path)
	return len(m) > 0
}

func (r *run) regulatory() {
	r.say("\n== Regulatory rules\n")
	err := r.script(nil, "install-regulatory.sh")
	var ec ExitCode
	switch {
	case err == nil:
	case errors.As(err, &ec) && ec.ExitCode() == 10:
		// No CRDA support in this kernel: its own database stays in
		// charge, and must be one it accepts.
		if err := r.script(nil, "install-regdb-fallback.sh"); err != nil {
			r.problem("the fallback regulatory database did not install (%v)", err)
		}
	default:
		r.problem("the regulatory rules are as they were: install-regulatory.sh failed (%v)", err)
	}
}

func or(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}

// rebootWatch passes output through and notes the lines that ask for a
// reboot.
type rebootWatch struct {
	out   io.Writer
	part  []byte
	found []string
}

func (w *rebootWatch) Write(p []byte) (int, error) {
	n, err := w.out.Write(p)
	w.part = append(w.part, p...)
	for {
		i := strings.IndexByte(string(w.part), '\n')
		if i < 0 {
			break
		}
		w.line(string(w.part[:i]))
		w.part = w.part[i+1:]
	}
	return n, err
}

func (w *rebootWatch) flush() {
	if len(w.part) > 0 {
		w.line(string(w.part))
		w.part = nil
	}
}

func (w *rebootWatch) line(l string) {
	if rebootLine.MatchString(l) {
		l = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), "=!-"))
		w.found = append(w.found, l)
	}
}

// System is the real host.
type System struct{}

func (System) Run(ctx context.Context, out io.Writer, env []string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = out, out
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	return cmd.Run()
}

func (System) Output(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}

func (System) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

func (System) WriteFile(path string, data []byte, perm os.FileMode) error {
	return os.WriteFile(path, data, perm)
}

func (System) Glob(pattern string) ([]string, error) { return filepath.Glob(pattern) }
