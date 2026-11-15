package prepare

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type exitErr int

func (e exitErr) Error() string { return "exit status " + string(rune('0'+int(e)%10)) }
func (e exitErr) ExitCode() int { return int(e) }

// fakeHost is a node: files, and commands answered from a table. Running a
// command may change files, as loading a module does.
type fakeHost struct {
	files map[string]string
	cmds  map[string]func(h *fakeHost) (out string, code int)
	ran   []string
}

func newHost() *fakeHost {
	return &fakeHost{files: map[string]string{
		DefaultDir + "/scripts/install-regulatory.sh": "#!/bin/sh",
		DefaultDir + "/packaging/dkms/source":         "BATADV_VERSION=2026.3\nBATADV_SHA256=x\n",
	}, cmds: map[string]func(*fakeHost) (string, int){}}
}

func (h *fakeHost) answer(cmd string, out string, code int) {
	h.cmds[cmd] = func(*fakeHost) (string, int) { return out, code }
}

func (h *fakeHost) exec(name string, args []string) (string, error) {
	line := strings.TrimSpace(strings.TrimPrefix(name, DefaultDir+"/scripts/") + " " + strings.Join(args, " "))
	h.ran = append(h.ran, line)
	f, ok := h.cmds[line]
	if !ok {
		return "", exitErr(1)
	}
	out, code := f(h)
	if code != 0 {
		return out, exitErr(code)
	}
	return out, nil
}

func (h *fakeHost) Run(ctx context.Context, out io.Writer, env []string, name string, args ...string) error {
	s, err := h.exec(name, args)
	io.WriteString(out, s)
	return err
}

func (h *fakeHost) Output(ctx context.Context, name string, args ...string) (string, error) {
	return h.exec(name, args)
}

func (h *fakeHost) ReadFile(path string) ([]byte, error) {
	s, ok := h.files[path]
	if !ok {
		return nil, os.ErrNotExist
	}
	return []byte(s), nil
}

func (h *fakeHost) WriteFile(path string, data []byte, perm os.FileMode) error {
	h.files[path] = string(data)
	return nil
}

func (h *fakeHost) Glob(pattern string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for p := range h.files {
		for _, cand := range []string{p, filepath.Dir(p), filepath.Dir(filepath.Dir(p))} {
			if ok, _ := filepath.Match(pattern, cand); ok && !seen[cand] {
				seen[cand] = true
				out = append(out, cand)
			}
		}
	}
	slices.Sort(out)
	return out, nil
}

func (h *fakeHost) did(cmd string) bool { return slices.Contains(h.ran, cmd) }

// A node with everything in place: nothing to do, nothing stopped.
func ready() *fakeHost {
	h := newHost()
	h.files["/sys/module/batman_adv/version"] = "2026.3\n"
	h.answer("dpkg-query -W -f=${Version} trellis-batman-adv-dkms", "2026.3-1", 0)
	h.answer("modinfo -F version batman-adv", "2026.3", 0)
	h.answer("modinfo -n batman-adv", "/lib/modules/6.1.0/updates/dkms/batman-adv.ko", 0)
	h.answer("systemctl is-active --quiet trellis-meshd", "", 0)
	h.answer("install-regulatory.sh", "-- in effect\n", 0)
	return h
}

func prepare(t *testing.T, h *fakeHost, opts ...string) (*Result, string) {
	t.Helper()
	var out bytes.Buffer
	res, err := Run(context.Background(), Options{Host: h, Out: &out, DriverOptions: opts})
	if err != nil {
		t.Fatal(err)
	}
	return res, out.String()
}

func TestNothingToDo(t *testing.T) {
	h := ready()
	res, out := prepare(t, h)
	if len(res.Problems) > 0 || len(res.Reboot) > 0 || h.did("systemctl stop trellis-meshd") {
		t.Fatalf("%+v\n%s", res, out)
	}
	if !strings.Contains(out, "2026.3 (DKMS build) is loaded") || !strings.Contains(out, "no MT7915/MT7916 card") {
		t.Errorf("output:\n%s", out)
	}
	if _, ok := h.files["/run/reboot-required"]; ok {
		t.Error("no reboot needed, none marked")
	}
}

// The DKMS build is installed but the kernel's own module is loaded: meshd
// stops, the module is swapped, meshd starts again.
func TestBatmanReload(t *testing.T) {
	h := ready()
	h.files["/sys/module/batman_adv/version"] = "2022.3"
	h.answer("systemctl stop trellis-meshd", "", 0)
	h.answer("systemctl start trellis-meshd", "", 0)
	h.answer("modprobe -r batman-adv", "", 0)
	h.cmds["modprobe batman-adv"] = func(h *fakeHost) (string, int) {
		h.files["/sys/module/batman_adv/version"] = "2026.3"
		return "", 0
	}
	res, out := prepare(t, h)
	if len(res.Problems) > 0 {
		t.Fatalf("%+v\n%s", res, out)
	}
	order := strings.Join(h.ran, "|")
	stop, load, start := strings.Index(order, "systemctl stop"), strings.Index(order, "modprobe batman-adv"), strings.Index(order, "systemctl start")
	if stop < 0 || load < stop || start < load {
		t.Errorf("stop, load, start in that order: %v", h.ran)
	}
	if !strings.Contains(out, "loaded: 2026.3") {
		t.Errorf("output:\n%s", out)
	}
}

// Not built for this kernel: the script builds it, with the package's
// release; a failed build is a problem, and the other steps still run.
func TestBatmanBuild(t *testing.T) {
	h := ready()
	delete(h.files, "/sys/module/batman_adv/version")
	h.answer("modinfo -F version batman-adv", "2022.3", 0)
	h.answer("modinfo -n batman-adv", "/lib/modules/6.1.0/kernel/net/batman-adv/batman-adv.ko", 0)
	h.answer("systemctl stop trellis-meshd", "", 0)
	h.answer("systemctl start trellis-meshd", "", 0)
	h.answer("install-batman-adv-dkms.sh", "!! no headers\n", 1)
	res, _ := prepare(t, h)
	if !h.did("install-batman-adv-dkms.sh") || len(res.Problems) != 1 || !strings.Contains(res.Problems[0], "batman-adv 2026.3") {
		t.Fatalf("%+v %v", res, h.ran)
	}
	if !h.did("install-regulatory.sh") || !h.did("systemctl start trellis-meshd") {
		t.Errorf("the rest still runs, and meshd comes back: %v", h.ran)
	}
}

// Without the DKMS package the pin file says which release.
func TestPinFromFile(t *testing.T) {
	h := ready()
	delete(h.cmds, "dpkg-query -W -f=${Version} trellis-batman-adv-dkms")
	_, out := prepare(t, h)
	if !strings.Contains(out, "== batman-adv 2026.3") {
		t.Errorf("output:\n%s", out)
	}
}

// An MT7916 without a driver: the script builds it and asks for a reboot,
// which is reported and marked.
func TestMT7915eAndReboot(t *testing.T) {
	h := ready()
	h.files["/sys/bus/pci/devices/0000:01:00.0/vendor"] = "0x14c3\n"
	h.files["/sys/bus/pci/devices/0000:01:00.0/device"] = "0x7906\n"
	h.files["/sys/bus/pci/devices/0000:00:00.0/vendor"] = "0x14e4\n"
	h.answer("install-mt7915e-dkms.sh", "-- built\n== REBOOT NEEDED: the driver is installed but not loaded; it starts after a reboot\n", 0)
	res, out := prepare(t, h)
	if !h.did("install-mt7915e-dkms.sh") || len(res.Reboot) != 1 || !strings.HasPrefix(res.Reboot[0], "REBOOT NEEDED") {
		t.Fatalf("%+v\n%s", res, out)
	}
	if h.files["/run/reboot-required"] == "" || !strings.Contains(h.files["/run/reboot-required.pkgs"], "trellis") {
		t.Error("the reboot is marked for other tools")
	}
	// Again: the mark is not repeated.
	prepare(t, h)
	if strings.Count(h.files["/run/reboot-required.pkgs"], "trellis") != 1 {
		t.Errorf("pkgs: %q", h.files["/run/reboot-required.pkgs"])
	}
}

func TestDriverOptions(t *testing.T) {
	h := ready()
	h.files["/sys/module/mt76x2u/parameters/disable_usb_sg"] = "N"
	h.files["/sys/module/ath9k_htc/parameters/nohwcrypt"] = "0"
	h.answer("systemctl stop trellis-meshd", "", 0)
	h.answer("systemctl start trellis-meshd", "", 0)
	h.answer("modprobe -r mt76x2u", "", 0)
	h.answer("modprobe mt76x2u", "", 0)
	res, out := prepare(t, h,
		"options mt76x2u disable_usb_sg=1",
		"options ath9k_htc nohwcrypt=1",
		"options rtl8xxxu ht40_2g=1") // not loaded
	if !h.did("modprobe -r mt76x2u") || !h.did("modprobe mt76x2u") {
		t.Errorf("mt76x2u reloaded: %v", h.ran)
	}
	if h.did("modprobe -r ath9k_htc") || len(res.Advice) != 1 || !strings.Contains(res.Advice[0], "replug") {
		t.Errorf("ath9k_htc only advised: %+v %v", res, h.ran)
	}
	if !strings.Contains(out, "rtl8xxxu: not loaded") {
		t.Errorf("output:\n%s", out)
	}
}

// A kernel that cannot ask userspace (exit 10): the fallback database.
func TestRegulatoryFallback(t *testing.T) {
	h := ready()
	h.answer("install-regulatory.sh", "", 10)
	h.answer("install-regdb-fallback.sh", "-- installed\n", 0)
	res, _ := prepare(t, h)
	if !h.did("install-regdb-fallback.sh") || len(res.Problems) > 0 {
		t.Errorf("%+v %v", res, h.ran)
	}
	h = ready()
	h.answer("install-regulatory.sh", "!! takes effect at the next boot: the kernel keeps the database it loaded at start\n", 0)
	if res, _ := prepare(t, h); len(res.Reboot) != 1 {
		t.Errorf("the next boot: %+v", res)
	}
}

func TestNeedsThePackage(t *testing.T) {
	h := ready()
	delete(h.files, DefaultDir+"/scripts/install-regulatory.sh")
	if _, err := Run(context.Background(), Options{Host: h, Out: io.Discard}); err == nil || !strings.Contains(err.Error(), "install the trellis package") {
		t.Errorf("err: %v", err)
	}
}
