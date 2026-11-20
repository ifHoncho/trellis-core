// Package trial tries a new driver module, firmware or board
// configuration file on a node without the risk of losing the node to it.
//
// A trial puts new files in place and keeps the ones they replace. It is
// pending from before the new files first load until meshd confirms it:
// the radio is there, its link is up, a peer is back if there was one, for
// a while. A node that restarts with a trial still pending gets the old
// files back before any driver loads (the boot hook, Boot), and the trial
// is latched off: nothing tries that version again until an operator
// re-arms it. One that is confirmed keeps the files it replaced, so the
// last good version can be put back by hand.
//
// State is one file under the node's state directory, changed under a
// lock: meshctl, the boot hook and meshd all work on it.
package trial

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"meridian/trellis/internal/fsutil"
)

// States of a trial.
const (
	// Armed: the new files are in place and load at the next boot.
	Armed = "armed"
	// Pending: the new files are loaded; meshd has not confirmed them.
	Pending = "pending"
	// Confirmed: kept. The files it replaced are kept too.
	Confirmed = "confirmed"
	// Reverted: the files it replaced are back.
	Reverted = "reverted"
)

// DirName is the trials' directory in the state directory.
const DirName = "trials"

// File is one file a trial replaces.
type File struct {
	Path string `json:"path"`
	// Had: a file was there before; reverting puts it back. Without one,
	// reverting removes the new file.
	Had    bool   `json:"had"`
	Digest string `json:"digest"` // of the new file
	Mode   uint32 `json:"mode"`
}

// Trial is one thing being tried, or the last trial of it.
type Trial struct {
	// Name says what is tried ("mt7915e-power", "halow-bcf"): one trial
	// of a name at a time.
	Name string `json:"name"`
	// Kind is module, firmware or bcf; Version what the new one is called.
	Kind    string `json:"kind,omitempty"`
	Version string `json:"version,omitempty"`
	// Module is the kernel module that loads the files: reloaded to start
	// a trial without a reboot, and again to undo one. Radio is the driver
	// name of the radios it runs, as sysfs has it, when that differs.
	Module string `json:"module,omitempty"`
	Radio  string `json:"radio,omitempty"`
	Files  []File `json:"files"`
	// Digest names exactly what was tried: the latch holds it.
	Digest string `json:"digest"`
	State  string `json:"state"`
	// Latched: undone by the fallback. A trial of the same digest is
	// refused until it is re-armed.
	Latched bool   `json:"latched,omitempty"`
	Reason  string `json:"reason,omitempty"`
	// BootID is the boot the new files loaded in (pending), or were put
	// in place in (armed).
	BootID  string    `json:"boot_id"`
	Started time.Time `json:"started"`
	// LoadedUptimeS is the system's uptime, in seconds, when the trial
	// became pending in BootID: about when its files loaded. meshd counts
	// the trial's deadline from it; a boot's clock may be stepped, its
	// uptime is not.
	LoadedUptimeS float64   `json:"loaded_uptime_s,omitempty"`
	Ended         time.Time `json:"ended,omitempty"`
	// HadPeer: a radio it concerns had a mesh peer when it began; meshd
	// then wants one back before it confirms.
	HadPeer bool `json:"had_peer,omitempty"`
	// RebootNeeded: the files are back, but the module could not be
	// reloaded: the old version loads at the next boot.
	RebootNeeded bool `json:"reboot_needed,omitempty"`
}

// RadioDriver is the driver name of the radios the trial concerns.
func (t *Trial) RadioDriver() string {
	if t.Radio != "" {
		return t.Radio
	}
	return t.Module
}

// Open is whether the trial still waits for its outcome.
func (t *Trial) Open() bool { return t.State == Armed || t.State == Pending }

// Describe names the trial for a person.
func (t *Trial) Describe() string {
	s := t.Name
	if t.Version != "" {
		s += " " + t.Version
	}
	return s
}

// Host is what a trial does to the system besides files; a variable part
// for tests and labs.
type Host interface {
	// Reload unloads and loads a kernel module, so that it reads its
	// files again.
	Reload(module string) error
	// FilesChanged is called after files under a kernel's module
	// directory changed (depmod).
	FilesChanged(paths []string)
}

// Store is the trials of one node.
type Store struct {
	Dir  string // <state>/trials
	Host Host
	Now  func() time.Time
	// Uptime is how long the system has been up (Uptime when nil).
	Uptime func() time.Duration
	// NoWait: a change finds the lock taken and returns ErrBusy instead
	// of waiting. meshd's: meshctl holds the lock while it reloads a
	// module, which stops meshd and waits for it to exit.
	NoWait bool
}

// ErrBusy: another program is changing the trials (NoWait).
var ErrBusy = errors.New("the trials are being changed by another program")

// New returns the store under a state directory, working on the real
// system.
func New(stateDir string) *Store {
	return &Store{Dir: filepath.Join(stateDir, DirName), Host: System{StateDir: stateDir}, Now: time.Now, Uptime: Uptime}
}

// Uptime is how long the system has been up, suspended time included
// (CLOCK_BOOTTIME); 0 if it cannot be read.
func Uptime() time.Duration {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		return 0
	}
	return time.Duration(ts.Nano())
}

// loaded marks a trial pending from now: its files load, or have loaded,
// in this boot.
func (s *Store) loaded(t *Trial, bootID string) {
	up := Uptime
	if s.Uptime != nil {
		up = s.Uptime
	}
	t.State, t.BootID, t.LoadedUptimeS = Pending, bootID, up().Seconds()
}

type state struct {
	Trials []*Trial `json:"trials"`
}

func (s *Store) statePath() string { return filepath.Join(s.Dir, "state.json") }

// lock takes the store's lock; the returned function releases it.
func (s *Store) lock() (func(), error) {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(s.Dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	how := syscall.LOCK_EX
	if s.NoWait {
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrBusy
		}
		return nil, err
	}
	return func() { f.Close() }, nil
}

func (s *Store) load() (*state, error) {
	st := &state{}
	b, err := os.ReadFile(s.statePath())
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, st); err != nil {
		return nil, fmt.Errorf("%s: %w", s.statePath(), err)
	}
	return st, nil
}

func (s *Store) save(st *state) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFile(s.statePath(), append(b, '\n'), 0o644)
}

func (st *state) find(name string) *Trial {
	for _, t := range st.Trials {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// List returns the trials, by name. It takes no lock: the state file is
// replaced whole, never written in place.
func (s *Store) List() ([]*Trial, error) {
	st, err := s.load()
	if err != nil {
		return nil, err
	}
	sort.Slice(st.Trials, func(i, j int) bool { return st.Trials[i].Name < st.Trials[j].Name })
	return st.Trials, nil
}

// Any reports whether there is a state file at all: the cheap check for
// callers that run often.
func (s *Store) Any() bool {
	_, err := os.Stat(s.statePath())
	return err == nil
}

var reName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,39}$`)

// Request starts a trial.
type Request struct {
	Name, Kind, Version string
	Module, Radio       string
	// Files maps where each file goes to the new file to put there.
	Files map[string]string
	// Reboot: the files load at the next boot; the module is not reloaded
	// now.
	Reboot  bool
	HadPeer bool
	BootID  string
	// Rearm starts it even though the same files were undone before.
	Rearm bool
}

// ErrLatched: these exact files were tried and undone.
var ErrLatched = errors.New("This version was reverted and is locked out until re-armed")

// Begin starts a trial: the files being replaced are kept, the trial is
// recorded, and only then are the new files put in place and, unless the
// trial waits for a reboot, the module reloaded.
func (s *Store) Begin(r Request) (*Trial, error) {
	if !reName.MatchString(r.Name) {
		return nil, fmt.Errorf("trial name %q: lower-case letters, digits, dot, dash and underscore, at most 40", r.Name)
	}
	if len(r.Files) == 0 {
		return nil, errors.New("a trial needs at least one file")
	}
	if r.BootID == "" {
		return nil, errors.New("no boot ID")
	}
	unlock, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	st, err := s.load()
	if err != nil {
		return nil, err
	}
	// Each place by its clean path, and the new file for it.
	srcs := make(map[string]string, len(r.Files))
	dests := make([]string, 0, len(r.Files))
	for d, src := range r.Files {
		if !filepath.IsAbs(d) {
			return nil, fmt.Errorf("%s: a file's place must be an absolute path", d)
		}
		c := filepath.Clean(d)
		if _, dup := srcs[c]; dup {
			return nil, fmt.Errorf("%s: listed twice", c)
		}
		srcs[c] = src
		dests = append(dests, c)
	}
	sort.Strings(dests)
	t := &Trial{Name: r.Name, Kind: r.Kind, Version: r.Version, Module: r.Module, Radio: r.Radio,
		State: Armed, BootID: r.BootID, Started: s.Now(), HadPeer: r.HadPeer}
	if !r.Reboot {
		s.loaded(t, r.BootID)
	}
	sum := sha256.New()
	news := make([][]byte, len(dests))
	for i, d := range dests {
		b, err := os.ReadFile(srcs[d])
		if err != nil {
			return nil, err
		}
		news[i] = b
		fd := sha256.Sum256(b)
		f := File{Path: d, Digest: hex.EncodeToString(fd[:]), Mode: 0o644}
		if fi, err := os.Stat(d); err == nil {
			if !fi.Mode().IsRegular() {
				return nil, fmt.Errorf("%s is not a regular file", d)
			}
			f.Had, f.Mode = true, uint32(fi.Mode().Perm())
		}
		t.Files = append(t.Files, f)
		fmt.Fprintf(sum, "%s\x00%s\x00", d, f.Digest)
	}
	t.Digest = hex.EncodeToString(sum.Sum(nil)[:16])

	if old := st.find(r.Name); old != nil {
		switch {
		case old.Open():
			return nil, fmt.Errorf("a trial of %s is %s already: confirm or revert it first", r.Name, old.State)
		case old.Latched && old.Digest == t.Digest && !r.Rearm:
			return nil, fmt.Errorf("%w (%s: %s)", ErrLatched, old.Describe(), old.Reason)
		}
	}
	// One open trial a file: undoing one would put back its old file over
	// the other's.
	for _, other := range st.Trials {
		if !other.Open() || other.Name == r.Name {
			continue
		}
		for _, of := range other.Files {
			if slices.Contains(dests, of.Path) {
				return nil, fmt.Errorf("trial %s, %s, replaces %s too: confirm or revert it first", other.Describe(), other.State, of.Path)
			}
		}
	}
	// Keep what is there now. A confirmed trial's kept files are the
	// version before it: they give way to the one being replaced now, but
	// only once the new trial is recorded. Until then the new kept files
	// are staged beside them (a name no trial can have), so a trial that
	// fails to begin leaves the last one as it was, revert included.
	dir := s.trialDir(r.Name)
	stage, prev := filepath.Join(s.Dir, ".new-"+r.Name), filepath.Join(s.Dir, ".old-"+r.Name)
	if err := errors.Join(os.RemoveAll(stage), os.RemoveAll(prev)); err != nil {
		return nil, err
	}
	for i, f := range t.Files {
		if f.Had {
			b, err := os.ReadFile(f.Path)
			if err == nil {
				err = fsutil.WriteFile(keptIn(stage, "old", i), b, 0o600)
			}
			if err != nil {
				_ = os.RemoveAll(stage)
				return nil, err
			}
		}
		if err := fsutil.WriteFile(keptIn(stage, "new", i), news[i], 0o600); err != nil {
			_ = os.RemoveAll(stage)
			return nil, err
		}
	}
	if err := os.Rename(dir, prev); err != nil && !errors.Is(err, fs.ErrNotExist) {
		_ = os.RemoveAll(stage)
		return nil, err
	}
	if err := os.Rename(stage, dir); err != nil {
		_ = os.Rename(prev, dir)
		_ = os.RemoveAll(stage)
		return nil, err
	}
	// Recorded before anything of it can load.
	st.Trials = append(without(st.Trials, r.Name), t)
	if err := s.save(st); err != nil {
		_ = os.RemoveAll(dir)
		_ = os.Rename(prev, dir)
		return nil, err
	}
	_ = os.RemoveAll(prev)
	var changed []string
	for i, f := range t.Files {
		if err := fsutil.WriteFile(f.Path, news[i], fs.FileMode(f.Mode)); err != nil {
			_ = s.restore(t)
			t.State, t.Reason, t.Ended = Reverted, "could not put the new files in place: "+err.Error(), s.Now()
			_ = s.save(st)
			return t, err
		}
		changed = append(changed, f.Path)
	}
	s.Host.FilesChanged(changed)
	if t.State == Pending && t.Module != "" {
		if err := s.Host.Reload(t.Module); err != nil {
			// The new version did not even load: back to the old one. Not
			// latched: nothing ran.
			_ = s.restore(t)
			s.Host.FilesChanged(changed)
			rerr := s.Host.Reload(t.Module)
			t.State, t.Ended = Reverted, s.Now()
			t.Reason = "module failed to load: " + firstLine(err.Error())
			t.RebootNeeded = rerr != nil
			_ = s.save(st)
			return t, fmt.Errorf("%s did not load with the new files, and they were undone: %w", t.Module, err)
		}
	}
	return t, nil
}

func without(ts []*Trial, name string) []*Trial {
	out := ts[:0:0]
	for _, t := range ts {
		if t.Name != name {
			out = append(out, t)
		}
	}
	return out
}

func (s *Store) trialDir(name string) string { return filepath.Join(s.Dir, name) }

func (s *Store) kept(name, which string, i int) string {
	return keptIn(s.trialDir(name), which, i)
}

// keptIn is where a trial's directory dir keeps file i: "old", the file
// it replaced, or "new", the one it put there.
func keptIn(dir, which string, i int) string {
	return filepath.Join(dir, which, fmt.Sprint(i))
}

// restore puts back the files a trial replaced, and removes those it
// added.
func (s *Store) restore(t *Trial) error {
	var errs []error
	for i, f := range t.Files {
		if !f.Had {
			if err := os.Remove(f.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
			continue
		}
		b, err := os.ReadFile(s.kept(t.Name, "old", i))
		if err == nil {
			err = fsutil.WriteFile(f.Path, b, fs.FileMode(f.Mode))
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", f.Path, err))
		}
	}
	return errors.Join(errs...)
}

func (t *Trial) paths() []string {
	out := make([]string, len(t.Files))
	for i, f := range t.Files {
		out[i] = f.Path
	}
	return out
}

// Boot is the boot hook: run once at each boot before drivers load. A
// trial that was pending in an earlier boot did not live to be confirmed:
// its old files are put back and it is latched. One that was armed for
// this boot becomes pending. It returns the trials it changed.
func (s *Store) Boot(bootID string) ([]*Trial, error) {
	if bootID == "" {
		return nil, errors.New("no boot ID")
	}
	unlock, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	st, err := s.load()
	if err != nil {
		return nil, err
	}
	var changed []*Trial
	var errs []error
	for _, t := range st.Trials {
		if !t.Open() || t.BootID == bootID {
			continue
		}
		switch t.State {
		case Armed:
			s.loaded(t, bootID)
		case Pending:
			if err := s.restore(t); err != nil {
				errs = append(errs, err)
				continue // still pending: the next boot tries again
			}
			s.Host.FilesChanged(t.paths())
			t.State, t.Latched, t.Ended = Reverted, true, s.Now()
			t.Reason = "reboot before confirmation"
		}
		changed = append(changed, t)
	}
	if len(changed) > 0 {
		errs = append(errs, s.save(st))
	}
	return changed, errors.Join(errs...)
}

// Arrive makes the trials armed in an earlier boot pending in this one,
// and nothing else: for a system where the boot hook did not run. It
// returns how many it changed.
func (s *Store) Arrive(bootID string) (int, error) {
	unlock, err := s.lock()
	if err != nil {
		return 0, err
	}
	defer unlock()
	st, err := s.load()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, t := range st.Trials {
		if t.State == Armed && t.BootID != bootID {
			s.loaded(t, bootID)
			n++
		}
	}
	if n == 0 {
		return 0, nil
	}
	return n, s.save(st)
}

// Confirm keeps a pending trial. The files it replaced stay kept.
func (s *Store) Confirm(name string) (*Trial, error) {
	return s.change(name, func(t *Trial) error {
		if t.State != Pending {
			return fmt.Errorf("the trial of %s is %s, not pending", name, t.State)
		}
		t.State, t.Ended, t.Reason = Confirmed, s.Now(), ""
		return nil
	})
}

// Revert puts back the files a trial replaced and reloads the module:
// for a pending or armed trial, and for a confirmed one (the last good
// version, by hand). latch refuses the same files until re-armed.
func (s *Store) Revert(name, reason string, latch bool) (*Trial, error) {
	return s.change(name, func(t *Trial) error {
		if t.State == Reverted {
			return fmt.Errorf("the trial of %s is undone already", name)
		}
		loaded := t.State != Armed
		if err := s.restore(t); err != nil {
			return err
		}
		s.Host.FilesChanged(t.paths())
		t.State, t.Latched, t.Reason, t.Ended = Reverted, latch, reason, s.Now()
		if loaded && t.Module != "" {
			if err := s.Host.Reload(t.Module); err != nil {
				t.RebootNeeded = true
				t.Reason += " (the module could not be reloaded: the old version loads at the next boot)"
			}
		}
		return nil
	})
}

// Rearm forgets a trial that was undone, so that it can be tried again.
func (s *Store) Rearm(name string) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	st, err := s.load()
	if err != nil {
		return err
	}
	t := st.find(name)
	switch {
	case t == nil:
		return fmt.Errorf("no trial of %s", name)
	case t.State != Reverted:
		return fmt.Errorf("Trial %s is %s: only a reverted trial can be re-armed", name, t.State)
	}
	st.Trials = without(st.Trials, name)
	_ = os.RemoveAll(s.trialDir(name))
	return s.save(st)
}

func (s *Store) change(name string, fn func(*Trial) error) (*Trial, error) {
	unlock, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	st, err := s.load()
	if err != nil {
		return nil, err
	}
	t := st.find(name)
	if t == nil {
		return nil, fmt.Errorf("no trial of %s", name)
	}
	if err := fn(t); err != nil {
		return t, err
	}
	return t, s.save(st)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}
