package osupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const simulated = `NOTE: This is only a simulation!
Reading package lists...
The following packages have been kept back:
  linux-image-rpi-v8
The following packages will be upgraded:
  libssl3t64 openssl
Inst libssl3t64 [3.5.1-1] (3.5.4-1~deb13u1 Debian-Security:13/stable-security [arm64])
Inst openssl [3.5.1-1] (3.5.4-1~deb13u1 Debian-Security:13/stable-security [arm64])
Conf libssl3t64 (3.5.4-1~deb13u1 Debian-Security:13/stable-security [arm64])
`

func TestPending(t *testing.T) {
	if got := strings.Join(Pending(simulated), ","); got != "libssl3t64,openssl" {
		t.Fatalf("pending %s", got)
	}
	if len(Pending("Reading package lists...\n0 upgraded\n")) != 0 {
		t.Fatal("nothing pending read as something")
	}
}

type fakeApt struct {
	cmds    []string
	failOn  string
	pending string // what apt-get -s upgrade prints
}

func (f *fakeApt) run(ctx context.Context, w io.Writer, name string, args ...string) error {
	cmd := name + " " + strings.Join(args, " ")
	f.cmds = append(f.cmds, cmd)
	if f.failOn != "" && strings.Contains(cmd, f.failOn) {
		fmt.Fprintln(w, "E: Temporary failure resolving 'deb.debian.org'")
		return errors.New("exit status 100")
	}
	switch {
	case strings.Contains(cmd, " -s "):
		fmt.Fprint(w, f.pending)
	case strings.HasSuffix(cmd, " upgrade"):
		fmt.Fprintln(w, "Unpacking openssl (3.5.4-1~deb13u1) over (3.5.1-1) ...")
		fmt.Fprintln(w, "Setting up libssl3t64:arm64 (3.5.4-1~deb13u1) ...")
		fmt.Fprintln(w, "Setting up openssl (3.5.4-1~deb13u1) ...")
		f.pending = "Reading package lists...\n"
	}
	return nil
}

func newHelper(t *testing.T, f *fakeApt) *Helper {
	dir := t.TempDir()
	return &Helper{Path: filepath.Join(dir, "os-updates.json"), Unit: "trellis-os-x", Run: f.run,
		Reboot: filepath.Join(dir, "reboot-required"), Now: func() time.Time { return time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC) }}
}

func TestCheckAndUpgrade(t *testing.T) {
	f := &fakeApt{pending: simulated}
	h := newHelper(t, f)
	ctx := context.Background()
	if err := h.Do(ctx, "check"); err != nil {
		t.Fatal(err)
	}
	s, err := Load(h.Path)
	if err != nil || s.Phase != PhaseIdle || s.Pending == nil || *s.Pending != 2 || s.Running() {
		t.Fatalf("after check: %+v %v", s, err)
	}
	for _, c := range f.cmds {
		if strings.HasSuffix(c, " upgrade") && !strings.Contains(c, " -s ") {
			t.Fatal("a check upgraded")
		}
	}

	if err := os.WriteFile(h.Reboot, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f.cmds = nil
	if err := h.Do(ctx, "upgrade"); err != nil {
		t.Fatal(err)
	}
	s, _ = Load(h.Path)
	if s.Phase != PhaseDone || s.Upgraded != 2 || *s.Pending != 0 || !s.RebootRequired || s.Unit != "" {
		t.Fatalf("after upgrade: %+v", s)
	}
	if f.cmds[0] != "dpkg --configure -a" || !strings.Contains(strings.Join(f.cmds, "\n"), "--force-confold") {
		t.Fatalf("commands: %v", f.cmds)
	}
	for _, c := range f.cmds {
		if strings.Contains(c, "dist-upgrade") || strings.Contains(c, "full-upgrade") {
			t.Fatal("packages may be removed or added: " + c)
		}
	}
}

func TestUpgradeFails(t *testing.T) {
	f := &fakeApt{pending: simulated, failOn: "update"}
	h := newHelper(t, f)
	if err := h.Do(context.Background(), "upgrade"); err == nil {
		t.Fatal("no error")
	}
	s, _ := Load(h.Path)
	if s.Phase != PhaseFailed || !strings.Contains(s.Error, "no internet") || len(s.Log) == 0 || s.Running() {
		t.Fatalf("%+v", s)
	}
}

// A start is on record before it returns, a second start is refused while
// the first runs, and a run whose unit has gone reads as failed.
func TestStartAndInterrupted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "os-updates.json")
	active := map[string]bool{}
	oldStart, oldActive := startUnit, unitActive
	defer func() { startUnit, unitActive = oldStart, oldActive }()
	var started []string
	startUnit = func(ctx context.Context, unit, bin, op string) error {
		// The record already says so when the unit starts.
		if s, err := Load(path); err != nil || !s.Running() || s.Unit != unit {
			t.Errorf("record at the unit's start: %+v %v", s, err)
		}
		started = append(started, unit)
		active[unit] = true
		return nil
	}
	unitActive = func(ctx context.Context, unit string) bool { return active[unit] }
	ctx := context.Background()

	s, err := Start(ctx, path, "", "upgrade")
	if err != nil || s.Phase != PhaseUpdating {
		t.Fatalf("%+v %v", s, err)
	}
	if got, err := Current(ctx, path); err != nil || got.Phase != PhaseUpdating || got.Unit != s.Unit {
		t.Fatalf("read after the start: %+v %v", got, err)
	}
	if _, err := Start(ctx, path, "", "check"); !errors.Is(err, ErrRunning) || len(started) != 1 {
		t.Fatalf("second start: %v, started %v", err, started)
	}

	// Power lost: the unit is gone, the record still says updating.
	delete(active, s.Unit)
	got, err := Current(ctx, path)
	if err != nil || got.Phase != PhaseFailed || got.Error != "interrupted" || got.Running() {
		t.Fatalf("interrupted run: %+v %v", got, err)
	}
	if on, _ := Load(path); on.Phase != PhaseFailed {
		t.Fatalf("not saved: %+v", on)
	}
	if _, err := Start(ctx, path, "", "check"); err != nil || len(started) != 2 {
		t.Fatalf("start after it: %v, started %v", err, started)
	}

	// A unit that does not start leaves the record as it was.
	delete(active, started[1])
	before, _ := Current(ctx, path)
	startUnit = func(context.Context, string, string, string) error { return errors.New("no systemd") }
	if _, err := Start(ctx, path, "", "check"); err == nil {
		t.Fatal("started without a unit")
	}
	if after, _ := Load(path); after.Phase != before.Phase || after.Running() {
		t.Fatalf("record after a failed start: %+v", after)
	}
}
