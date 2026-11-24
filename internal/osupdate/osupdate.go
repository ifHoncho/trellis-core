// Package osupdate checks for and installs the operating system's package
// updates (apt) without anyone logging in to the node. Held packages
// stay held: an image holds its kernel and headers, whose DKMS drivers
// must build for any new one. Trellis's own
// packages are not from apt and are upgraded by meshctl upgrade and
// rollouts, never here.
//
// meshd is sandboxed and cannot run apt: it starts the helper (trellis os
// check|upgrade) as a transient systemd unit, detached, and reads the
// helper's record (StatePath) for the API. One run at a time; a run cut
// short reads as failed.
package osupdate

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"meridian/trellis/internal/fsutil"
)

// Phases of a run.
const (
	PhaseIdle     = ""
	PhaseChecking = "checking"
	PhaseUpdating = "updating"
	PhaseDone     = "done"
	PhaseFailed   = "failed"
)

// State is the record of the last check and update.
type State struct {
	Phase          string    `json:"phase"`
	Unit           string    `json:"unit,omitempty"` // the transient unit of a run under way
	Pending        *int      `json:"pending,omitempty"`
	Packages       []string  `json:"packages,omitempty"` // the pending ones, at most 50
	CheckedAt      time.Time `json:"checked_at,omitzero"`
	StartedAt      time.Time `json:"started_at,omitzero"`
	FinishedAt     time.Time `json:"finished_at,omitzero"`
	Upgraded       int       `json:"upgraded,omitempty"`
	RebootRequired bool      `json:"reboot_required,omitempty"`
	Error          string    `json:"error,omitempty"`
	Log            []string  `json:"log,omitempty"` // the last lines of apt's output
}

// Running says whether a run is under way.
func (s *State) Running() bool { return s.Phase == PhaseChecking || s.Phase == PhaseUpdating }

// StatePath is where the helper keeps its record.
const StatePath = "/var/lib/trellis/os-updates.json"

// Load reads the record (an empty one when there is none).
func Load(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &State{}, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func save(path string, s *State) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFile(path, append(data, '\n'), 0o644)
}

// mu orders, in meshd, a start against another start and against a read
// of the record: a start is on record before either looks at it.
var mu sync.Mutex

// Current is the record for the API. A run on record whose unit has gone
// (power lost during it, or the helper killed) is marked failed here, so
// that it does not read as under way for good.
func Current(ctx context.Context, path string) (*State, error) {
	mu.Lock()
	defer mu.Unlock()
	return current(ctx, path)
}

func current(ctx context.Context, path string) (*State, error) {
	s, err := Load(path)
	if err != nil || !s.Running() || unitActive(ctx, s.Unit) {
		return s, err
	}
	// The helper writes its last record before its unit ends: read again.
	if s, err = Load(path); err != nil || !s.Running() {
		return s, err
	}
	s.Phase, s.Error, s.Unit, s.FinishedAt = PhaseFailed, "interrupted", "", time.Now().UTC()
	_ = save(path, s)
	return s, nil
}

// Start begins a check or an update in a transient unit and returns at
// once, the run on record: a read straight after it, or a second start,
// finds it under way.
func Start(ctx context.Context, path, bin, op string) (*State, error) {
	if op != "check" && op != "upgrade" {
		return nil, fmt.Errorf("os updates: check or upgrade (got %q)", op)
	}
	mu.Lock()
	defer mu.Unlock()
	s, err := current(ctx, path)
	if err != nil {
		return nil, err
	}
	if s.Running() {
		return s, ErrRunning
	}
	if bin == "" {
		bin = "/usr/bin/trellis"
	}
	id := make([]byte, 4)
	_, _ = rand.Read(id)
	unit := "trellis-os-" + op + "-" + hex.EncodeToString(id)
	// On record before the unit starts: the helper's own writes follow it.
	before := *s
	s.Unit, s.StartedAt, s.FinishedAt, s.Error, s.Log = unit, time.Now().UTC(), time.Time{}, "", nil
	if op == "check" {
		s.Phase = PhaseChecking
	} else {
		s.Phase = PhaseUpdating
	}
	if err := save(path, s); err != nil {
		return nil, err
	}
	if err := startUnit(ctx, unit, bin, op); err != nil {
		_ = save(path, &before)
		return nil, err
	}
	return s, nil
}

// ErrRunning is a start refused while another run is under way.
var ErrRunning = errors.New("an OS update check or upgrade is already running")

// startUnit and unitActive are systemd's part; variables for tests.
var startUnit = func(ctx context.Context, unit, bin, op string) error {
	out, err := exec.CommandContext(ctx, "systemd-run", "--quiet", "--collect", "--service-type=exec",
		"--unit="+unit, "--description=Trellis: OS package "+op, bin, "os", op, "--unit", unit).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemd-run: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

var unitActive = func(ctx context.Context, unit string) bool {
	if unit == "" {
		return false
	}
	out, _ := exec.CommandContext(ctx, "systemctl", "is-active", unit).Output()
	s := strings.TrimSpace(string(out))
	return s == "active" || s == "activating"
}

// Runner runs a command, its output to w (tests give a fake).
type Runner func(ctx context.Context, w io.Writer, name string, args ...string) error

func run(ctx context.Context, w io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = w, w
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive", "APT_LISTCHANGES_FRONTEND=none", "NEEDRESTART_MODE=a")
	return cmd.Run()
}

// Helper is the other end: trellis os check|upgrade, as root.
type Helper struct {
	Path   string // the record (StatePath)
	Unit   string // this run's unit
	Run    Runner
	Reboot string // the file a package leaves when a reboot is needed
	Now    func() time.Time
}

// NewHelper is the node's own.
func NewHelper(unit string) *Helper {
	return &Helper{Path: StatePath, Unit: unit, Run: run, Reboot: "/run/reboot-required", Now: time.Now}
}

var aptOpts = []string{"-q", "-o", "Dpkg::Options::=--force-confdef", "-o", "Dpkg::Options::=--force-confold"}

// Do runs op ("check" or "upgrade") and keeps the record.
func (h *Helper) Do(ctx context.Context, op string) error {
	s, err := Load(h.Path)
	if err != nil {
		s = &State{}
	}
	s.Unit, s.Error, s.Log, s.StartedAt, s.FinishedAt = h.Unit, "", nil, h.Now().UTC(), time.Time{}
	s.Phase = PhaseChecking
	if op == "upgrade" {
		s.Phase = PhaseUpdating
	}
	_ = save(h.Path, s)

	var log tail
	fail := func(err error) error {
		s.Phase, s.Error, s.Unit, s.Log, s.FinishedAt = PhaseFailed, err.Error(), "", log.lines(), h.Now().UTC()
		_ = save(h.Path, s)
		return err
	}
	if op == "upgrade" {
		// A run a power loss cut short leaves packages half configured.
		if err := h.Run(ctx, &log, "dpkg", "--configure", "-a"); err != nil {
			return fail(fmt.Errorf("dpkg --configure -a: %v", err))
		}
	}
	if err := h.Run(ctx, &log, "apt-get", append([]string{"update"}, aptOpts...)...); err != nil {
		return fail(fmt.Errorf("apt-get update: %v (no internet?)", err))
	}
	if op == "upgrade" {
		before := log.count("Setting up ")
		args := append([]string{"-y"}, aptOpts...)
		if err := h.Run(ctx, &log, "apt-get", append(args, "upgrade")...); err != nil {
			return fail(fmt.Errorf("apt-get upgrade: %v", err))
		}
		s.Upgraded = log.count("Setting up ") - before
	}
	var sim bytes.Buffer
	if err := h.Run(ctx, &sim, "apt-get", "-s", "-o", "Debug::NoLocking=1", "upgrade"); err != nil {
		return fail(fmt.Errorf("apt-get -s upgrade: %v", err))
	}
	pkgs := Pending(sim.String())
	n := len(pkgs)
	if len(pkgs) > 50 {
		pkgs = pkgs[:50]
	}
	s.Pending, s.Packages, s.CheckedAt = &n, pkgs, h.Now().UTC()
	_, err = os.Stat(h.Reboot)
	s.RebootRequired = err == nil
	s.Phase, s.Unit, s.Log, s.FinishedAt = PhaseIdle, "", log.lines(), h.Now().UTC()
	if op == "upgrade" {
		s.Phase = PhaseDone
	}
	return save(h.Path, s)
}

var instLine = regexp.MustCompile(`^Inst (\S+) `)

// Pending reads apt-get -s upgrade's output: the packages it would install.
func Pending(out string) []string {
	var pkgs []string
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		if m := instLine.FindStringSubmatch(sc.Text()); m != nil {
			pkgs = append(pkgs, m[1])
		}
	}
	return pkgs
}

// tail keeps the last lines of a command's output, and counts some.
type tail struct {
	buf    bytes.Buffer
	counts map[string]int
	all    []string
}

func (t *tail) Write(p []byte) (int, error) {
	t.buf.Write(p)
	for {
		i := bytes.IndexByte(t.buf.Bytes(), '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(t.buf.Next(i+1)), "\r\n")
		t.all = append(t.all, line)
		if len(t.all) > 400 {
			t.all = t.all[len(t.all)-400:]
		}
		if t.counts == nil {
			t.counts = map[string]int{}
		}
		if strings.HasPrefix(line, "Setting up ") {
			t.counts["Setting up "]++
		}
	}
	return len(p), nil
}

func (t *tail) count(prefix string) int { return t.counts[prefix] }

func (t *tail) lines() []string {
	if len(t.all) <= 20 {
		return append([]string(nil), t.all...)
	}
	return append([]string(nil), t.all[len(t.all)-20:]...)
}

// HelperMain is trellis os check|upgrade [--unit NAME].
func HelperMain(args []string, stderr io.Writer) int {
	if len(args) < 1 || (args[0] != "check" && args[0] != "upgrade") {
		fmt.Fprintln(stderr, "usage: trellis os check|upgrade (meshd runs it)")
		return 2
	}
	unit := ""
	if len(args) == 3 && args[1] == "--unit" {
		unit = args[2]
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "trellis os: run as root")
		return 1
	}
	if err := os.MkdirAll(filepath.Dir(StatePath), 0o755); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	if err := NewHelper(unit).Do(ctx, args[0]); err != nil {
		fmt.Fprintln(stderr, "trellis os:", err)
		return 1
	}
	return 0
}
