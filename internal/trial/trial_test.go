package trial

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// host records reloads; a reload fails while the installed module file
// holds "bad".
type host struct {
	module  string // the module's file
	reloads []string
	changed [][]string
}

func (h *host) Reload(module string) error {
	b, _ := os.ReadFile(h.module)
	h.reloads = append(h.reloads, module+":"+strings.TrimSpace(string(b)))
	if strings.Contains(string(b), "bad") {
		return errors.New("Exec format error")
	}
	return nil
}

func (h *host) FilesChanged(p []string) { h.changed = append(h.changed, p) }

type fixture struct {
	t       *testing.T
	s       *Store
	h       *host
	module  string // installed module
	bcf     string // installed board file, absent at first
	src     string
	now     time.Time
	trialNo int
}

func setup(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{t: t, module: filepath.Join(root, "lib", "modules", "6.1.0", "updates", "standin.ko"),
		bcf: filepath.Join(root, "lib", "firmware", "standin", "board.bin"), src: filepath.Join(root, "src"),
		now: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
	f.put(f.module, "v1")
	f.h = &host{module: f.module}
	f.s = &Store{Dir: filepath.Join(root, "state", DirName), Host: f.h, Now: func() time.Time { return f.now }}
	return f
}

func (f *fixture) put(path, text string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) read(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "<" + filepath.Base(path) + " absent>"
	}
	return string(b)
}

// begin starts a trial of the module (and, with bcf set, a board file).
func (f *fixture) begin(module, bcf string, r Request) (*Trial, error) {
	f.t.Helper()
	f.trialNo++
	dir := filepath.Join(f.src, string(rune('a'+f.trialNo)))
	f.put(filepath.Join(dir, "standin.ko"), module)
	r.Files = map[string]string{f.module: filepath.Join(dir, "standin.ko")}
	if bcf != "" {
		f.put(filepath.Join(dir, "board.bin"), bcf)
		r.Files[f.bcf] = filepath.Join(dir, "board.bin")
	}
	if r.Name == "" {
		r.Name = "standin"
	}
	r.Module = "standin"
	return f.s.Begin(r)
}

func (f *fixture) state(name string) *Trial {
	f.t.Helper()
	ts, err := f.s.List()
	if err != nil {
		f.t.Fatal(err)
	}
	for _, t := range ts {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// A good trial: recorded, loaded, confirmed, and the version it replaced
// kept for a revert by hand.
func TestGoodTrialIsKept(t *testing.T) {
	f := setup(t)
	tr, err := f.begin("v2", "bcf2", Request{BootID: "boot-1", Version: "2", Kind: "module", HadPeer: true})
	if err != nil {
		t.Fatal(err)
	}
	if tr.State != Pending || !tr.HadPeer || len(tr.Files) != 2 || tr.Digest == "" {
		t.Fatalf("trial = %+v", tr)
	}
	if f.read(f.module) != "v2" || f.read(f.bcf) != "bcf2" {
		t.Fatalf("installed: %s %s", f.read(f.module), f.read(f.bcf))
	}
	if len(f.h.reloads) != 1 || f.h.reloads[0] != "standin:v2" || len(f.h.changed) != 1 {
		t.Errorf("reloads %v, changed %v", f.h.reloads, f.h.changed)
	}
	// A second trial of the same thing waits.
	if _, err := f.begin("v3", "", Request{BootID: "boot-1"}); err == nil || !strings.Contains(err.Error(), "pending already") {
		t.Errorf("second trial: %v", err)
	}
	// The same boot's hook leaves it alone.
	if ch, err := f.s.Boot("boot-1"); err != nil || len(ch) != 0 {
		t.Errorf("hook in the trial's own boot: %v %v", ch, err)
	}
	if _, err := f.s.Confirm("standin"); err != nil {
		t.Fatal(err)
	}
	// Confirmed: a later boot changes nothing.
	if ch, _ := f.s.Boot("boot-2"); len(ch) != 0 || f.read(f.module) != "v2" {
		t.Errorf("a confirmed trial was touched at boot: %v", ch)
	}
	// The last good version can still be put back by hand; the board
	// file, which was not there before, goes.
	tr, err = f.s.Revert("standin", "asked for", false)
	if err != nil || tr.State != Reverted || tr.Latched {
		t.Fatalf("revert by hand: %+v %v", tr, err)
	}
	if f.read(f.module) != "v1" || !strings.Contains(f.read(f.bcf), "absent") {
		t.Errorf("after the revert: %s %s", f.read(f.module), f.read(f.bcf))
	}
	if got := f.h.reloads[len(f.h.reloads)-1]; got != "standin:v1" {
		t.Errorf("last reload = %s", got)
	}
}

// A trial that takes the node down: the next boot's hook puts the old
// files back before anything loads, and latches it.
func TestRebootUndoesAndLatches(t *testing.T) {
	f := setup(t)
	if _, err := f.begin("v2-panics", "", Request{BootID: "boot-1", Version: "2"}); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Minute)
	ch, err := f.s.Boot("boot-2")
	if err != nil || len(ch) != 1 {
		t.Fatalf("hook: %v %v", ch, err)
	}
	tr := f.state("standin")
	if tr.State != Reverted || !tr.Latched || !strings.Contains(tr.Reason, "reboot before") || !tr.Ended.Equal(f.now) {
		t.Fatalf("after the hook: %+v", tr)
	}
	if f.read(f.module) != "v1" {
		t.Fatalf("installed after the hook: %s", f.read(f.module))
	}
	// The hook reloads nothing: no driver has loaded yet.
	if len(f.h.reloads) != 1 {
		t.Errorf("reloads = %v", f.h.reloads)
	}
	// Run again (the same boot, or the next): nothing more.
	if ch, _ := f.s.Boot("boot-3"); len(ch) != 0 {
		t.Errorf("hook again: %v", ch)
	}
	// The same files are refused; other files are not; re-armed, the same
	// are taken.
	if _, err := f.begin("v2-panics", "", Request{BootID: "boot-3"}); !errors.Is(err, ErrLatched) {
		t.Fatalf("the latched version again: %v", err)
	}
	if f.read(f.module) != "v1" {
		t.Fatal("a refused trial changed the file")
	}
	if _, err := f.begin("v2-panics", "", Request{BootID: "boot-3", Rearm: true}); err != nil {
		t.Fatalf("re-armed on begin: %v", err)
	}
	if _, err := f.s.Revert("standin", "by hand", true); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Rearm("standin"); err != nil {
		t.Fatal(err)
	}
	if f.state("standin") != nil {
		t.Error("a re-armed trial is still listed")
	}
	if _, err := f.begin("v2-panics", "", Request{BootID: "boot-3"}); err != nil {
		t.Fatalf("after rearm: %v", err)
	}
}

// A trial that needs a reboot is armed, becomes pending at the next boot,
// and is undone by the one after if nobody confirmed it.
func TestArmedTrial(t *testing.T) {
	f := setup(t)
	tr, err := f.begin("v2", "", Request{BootID: "boot-1", Reboot: true})
	if err != nil || tr.State != Armed {
		t.Fatalf("begin: %+v %v", tr, err)
	}
	if len(f.h.reloads) != 0 || f.read(f.module) != "v2" {
		t.Fatalf("an armed trial reloaded (%v) or did not install (%s)", f.h.reloads, f.read(f.module))
	}
	if ch, _ := f.s.Boot("boot-2"); len(ch) != 1 || f.state("standin").State != Pending || f.state("standin").BootID != "boot-2" {
		t.Fatalf("first boot: %+v", f.state("standin"))
	}
	if ch, _ := f.s.Boot("boot-3"); len(ch) != 1 || f.state("standin").State != Reverted || f.read(f.module) != "v1" {
		t.Fatalf("second boot: %+v, installed %s", f.state("standin"), f.read(f.module))
	}
}

// New files the module does not even load with are undone at once, and
// not latched: nothing ran.
func TestModuleThatDoesNotLoad(t *testing.T) {
	f := setup(t)
	tr, err := f.begin("bad", "", Request{BootID: "boot-1"})
	if err == nil || tr == nil || tr.State != Reverted || tr.Latched || tr.RebootNeeded {
		t.Fatalf("begin: %+v %v", tr, err)
	}
	if f.read(f.module) != "v1" {
		t.Errorf("installed: %s", f.read(f.module))
	}
	if got := strings.Join(f.h.reloads, " "); got != "standin:bad standin:v1" {
		t.Errorf("reloads = %s", got)
	}
}

// A file's place is taken by its clean path; the trial is pending from
// the uptime its files load at: at Begin, or at the boot it was armed for.
func TestUncleanPlaceAndLoadTime(t *testing.T) {
	f := setup(t)
	up := 42 * time.Second
	f.s.Uptime = func() time.Duration { return up }
	src := filepath.Join(f.src, "x", "standin.ko")
	f.put(src, "v2")
	unclean := filepath.Dir(f.module) + "//" + filepath.Base(f.module)
	tr, err := f.s.Begin(Request{Name: "standin", Module: "standin", BootID: "boot-1", Files: map[string]string{unclean: src}})
	if err != nil || tr.Files[0].Path != f.module || f.read(f.module) != "v2" || tr.LoadedUptimeS != 42 {
		t.Fatalf("begin: %+v %v", tr, err)
	}
	both := map[string]string{unclean: src, f.module: src}
	if _, err := f.s.Begin(Request{Name: "other", BootID: "boot-1", Files: both}); err == nil || !strings.Contains(err.Error(), "listed twice") {
		t.Errorf("a place listed twice: %v", err)
	}
	if _, err := f.s.Revert("standin", "test", false); err != nil {
		t.Fatal(err)
	}
	if tr, err = f.s.Begin(Request{Name: "standin", Module: "standin", BootID: "boot-1", Reboot: true, Files: map[string]string{f.module: src}}); err != nil || tr.LoadedUptimeS != 0 {
		t.Fatalf("armed: %+v %v", tr, err)
	}
	up = 7 * time.Second
	if _, err := f.s.Boot("boot-2"); err != nil {
		t.Fatal(err)
	}
	if tr := f.state("standin"); tr.State != Pending || tr.LoadedUptimeS != 7 {
		t.Fatalf("after the boot: %+v", tr)
	}
}

// Two open trials do not replace the same file.
func TestOneOpenTrialAFile(t *testing.T) {
	f := setup(t)
	if _, err := f.begin("v2", "", Request{Name: "first", BootID: "boot-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.begin("v3", "", Request{Name: "second", BootID: "boot-1"}); err == nil || !strings.Contains(err.Error(), "trial first") {
		t.Fatalf("second trial of the same file: %v", err)
	}
	if f.read(f.module) != "v2" || f.state("second") != nil {
		t.Fatal("the refused trial left something behind")
	}
	if _, err := f.s.Confirm("first"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.begin("v3", "", Request{Name: "second", BootID: "boot-1"}); err != nil {
		t.Fatalf("after the first was confirmed: %v", err)
	}
}

func TestBadRequests(t *testing.T) {
	f := setup(t)
	for _, r := range []Request{
		{Name: "Bad Name", BootID: "b", Files: map[string]string{f.module: f.module}},
		{Name: "x", BootID: "b"},
		{Name: "x", Files: map[string]string{f.module: f.module}},
		{Name: "x", BootID: "b", Files: map[string]string{"relative/path": f.module}},
		{Name: "x", BootID: "b", Files: map[string]string{f.module: filepath.Join(f.src, "missing")}},
		{Name: "x", BootID: "b", Files: map[string]string{filepath.Dir(f.module): f.module}},
	} {
		if _, err := f.s.Begin(r); err == nil {
			t.Errorf("taken: %+v", r)
		}
	}
	if f.read(f.module) != "v1" || f.s.Any() {
		t.Error("a refused request left something behind")
	}
	if _, err := f.s.Confirm("nothing"); err == nil {
		t.Error("confirmed a trial that does not exist")
	}
	if err := f.s.Rearm("nothing"); err == nil {
		t.Error("re-armed a trial that does not exist")
	}
}

// reloadHook runs during before each reload: what another program does
// while meshctl reloads the module.
type reloadHook struct {
	*host
	during func()
}

func (h reloadHook) Reload(module string) error {
	h.during()
	return h.host.Reload(module)
}

// meshd's store does not wait for the lock: meshctl holds it while the
// reload stops meshd, and meshd blocked on it would not stop.
func TestNoWaitWhileReloading(t *testing.T) {
	f := setup(t)
	meshd := &Store{Dir: f.s.Dir, Host: f.h, Now: f.s.Now, NoWait: true}
	var busy []error
	f.s.Host = reloadHook{f.h, func() {
		done := make(chan error, 2)
		go func() {
			_, err := meshd.Confirm("standin")
			done <- err
		}()
		go func() {
			_, err := meshd.Arrive("boot-1")
			done <- err
		}()
		for range 2 {
			select {
			case err := <-done:
				busy = append(busy, err)
			case <-time.After(5 * time.Second):
				t.Fatal("meshd's store waited for the lock")
			}
		}
	}}
	if _, err := f.begin("v2", "", Request{BootID: "boot-1"}); err != nil {
		t.Fatal(err)
	}
	for _, err := range busy {
		if !errors.Is(err, ErrBusy) {
			t.Errorf("during the reload: %v, want ErrBusy", err)
		}
	}
	// Free again: it goes through.
	if _, err := meshd.Confirm("standin"); err != nil {
		t.Fatal(err)
	}
}

// A trial that fails to begin leaves the confirmed one before it whole:
// its last good version can still be put back.
func TestFailedBeginKeepsLastGood(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file of mode 0")
	}
	f := setup(t)
	if _, err := f.begin("v2", "bcf2", Request{BootID: "boot-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Confirm("standin"); err != nil {
		t.Fatal(err)
	}
	// The installed board file cannot be read to keep it.
	if err := os.Chmod(f.bcf, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.begin("v3", "bcf3", Request{BootID: "boot-1"}); err == nil {
		t.Fatal("began without keeping the board file")
	}
	if err := os.Chmod(f.bcf, 0o644); err != nil {
		t.Fatal(err)
	}
	if tr := f.state("standin"); tr.State != Confirmed || f.read(f.module) != "v2" {
		t.Fatalf("after the failed begin: %+v, module %s", tr, f.read(f.module))
	}
	if _, err := f.s.Revert("standin", "asked for", false); err != nil {
		t.Fatal(err)
	}
	if f.read(f.module) != "v1" || !strings.Contains(f.read(f.bcf), "absent") {
		t.Errorf("after the revert: %s %s", f.read(f.module), f.read(f.bcf))
	}
	if entries, _ := os.ReadDir(f.s.Dir); len(entries) != 3 { // lock, state.json, standin
		t.Errorf("left in the trials directory: %v", entries)
	}
}
