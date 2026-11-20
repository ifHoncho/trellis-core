// Package crashlog says why a node went down. The kernel leaves what it
// can in pstore when it panics; meshd reads the records at start, gives
// each a verdict, and keeps its own short log: the system's pstore service
// moves the records on at boot and writes each crash over the last, so the
// records alone do not hold a node's history.
//
// A node that went down while meshd ran, without a shutdown and without a
// record, lost power (or was reset from outside the kernel): that is a
// verdict too.
//
// Times. A board without an RTC starts each boot from a stale saved time,
// and chrony sets the clock some seconds later: whatever meshd reads at
// its start may be days out. So the log keeps when each boot started as
// read once the clock is set (ClockSet), and dates a crash by that start
// plus the uptime in its record. A reset that cannot be dated so is dated
// by the boot that found it, placed again once that boot's clock is set.
package crashlog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"meridian/trellis/internal/fsutil"
)

// Verdicts.
const (
	// PCIeLink: an interrupt flood on a PCIe port, then a device stopped
	// answering: the link to the card.
	PCIeLink = "pcie-link"
	// CardStopped: a device stopped answering on its bus (an SError)
	// with no flood before it.
	CardStopped = "card-stopped"
	// SysRq: a Magic SysRq reset, which a serial console takes from
	// whatever talks on its line.
	SysRq = "sysrq"
	// Panic: any other kernel panic or oops.
	Panic = "panic"
	// PowerLoss: down without a shutdown, and nothing recorded.
	PowerLoss = "power-loss"
	// Unrecorded: down without a shutdown on a board that keeps no crash
	// records, so the cause is not known.
	Unrecorded = "unrecorded"
)

// DefaultDirs are where crash records are: the kernel's pstore, and where
// systemd-pstore moves them at boot.
var DefaultDirs = []string{"/sys/fs/pstore", "/var/lib/systemd/pstore"}

// FileName is the log's file in the state directory.
const FileName = "resets.json"

// maxEntries is how many resets the log keeps; maxSeen how many records it
// remembers having read.
const (
	maxEntries = 40
	maxSeen    = 200
	maxRecord  = 2 << 20
)

// Entry is one time the node went down other than by a shutdown.
type Entry struct {
	// Time is when the crash happened (the start of its boot plus the
	// uptime in its record), or, where that is not known, when the next
	// boot started.
	Time    time.Time `json:"time"`
	Verdict string    `json:"verdict"`
	// Summary says what happened in a line; Detail is the kernel's own
	// line for it, when there is one.
	Summary string `json:"summary"`
	Detail  string `json:"detail,omitempty"`
	// UptimeS is how long the node had been up, by the kernel's clock;
	// 0 when not known.
	UptimeS float64 `json:"uptime_s,omitempty"`
	// TimePending: Time is the clock of the boot that found the reset,
	// read before that clock was set; ClockSet places it again.
	TimePending bool `json:"time_pending,omitempty"`
	// TimeUnknown: the boot that found the reset never had its clock set,
	// so Time is that boot's stale clock and nothing will place it.
	TimeUnknown bool `json:"time_unknown,omitempty"`
	// Boot is the ID of the boot that found the reset (the boot after it).
	Boot string `json:"boot,omitempty"`
	// Record is the crash record's file name.
	Record string `json:"record,omitempty"`
}

// Boot states.
const (
	stateRunning  = "running"  // meshd is up: a new boot from here is a reset
	stateStopped  = "stopped"  // meshd was stopped while the system ran on
	stateShutdown = "shutdown" // meshd stopped as the system shut down
	// stateReload: meshd stopped for another program to reload a driver
	// module (Reloading): a new boot from here is a reset, as with
	// stateRunning.
	stateReload = "reload"
)

// downUnclean: a boot that ends in this state went down without a
// shutdown.
func downUnclean(state string) bool { return state == stateRunning || state == stateReload }

// Log is the node's reset history.
type Log struct {
	BootID      string    `json:"boot_id"`
	BootStarted time.Time `json:"boot_started"`
	// BootStartKnown: BootStarted was read with the clock set (ClockSet),
	// not from the clock the board started with.
	BootStartKnown bool     `json:"boot_start_known,omitempty"`
	State          string   `json:"state"`
	Seen           []string `json:"seen,omitempty"` // digests of records read
	Entries        []Entry  `json:"entries"`        // oldest first

	path string
	mu   sync.Mutex // meshd's reconcilers, its status and its stop
}

// Options says where to look.
type Options struct {
	StateDir string
	BootID   string
	Now      time.Time
	// Uptime is the system's uptime at Now (CLOCK_BOOTTIME); 0: not known.
	Uptime time.Duration
	Dirs   []string // crash records (DefaultDirs)
	// Backend: the kernel has somewhere to keep crash records.
	Backend bool
}

// Load reads the log without changing it; a missing file is an empty log.
func Load(stateDir string) (*Log, error) {
	l := &Log{path: filepath.Join(stateDir, FileName), Entries: []Entry{}}
	b, err := os.ReadFile(l.path)
	if errors.Is(err, fs.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return l, err
	}
	if err := json.Unmarshal(b, l); err != nil {
		// A log that cannot be read is started again: it is a record of
		// the past, not state anything depends on.
		return &Log{path: l.path, Entries: []Entry{}}, fmt.Errorf("%s: %w", l.path, err)
	}
	if l.Entries == nil {
		l.Entries = []Entry{}
	}
	return l, nil
}

// Open reads the log at meshd's start and brings it up to this boot: on
// the first start of a boot it reads the crash records it has not seen,
// and notes a reset that left none. It returns the log and the entries
// this start added.
func Open(o Options) (*Log, []Entry, error) {
	l, loadErr := Load(o.StateDir)
	if o.Dirs == nil {
		o.Dirs = DefaultDirs
	}
	var added []Entry
	if l.BootID != o.BootID {
		// The boot before ended with meshd stopped: its console log is a
		// boot that ended well, whatever it holds.
		clean := l.BootID != "" && !downUnclean(l.State)
		// When the boot before started, if its clock was ever set: its
		// crash happened its record's uptime later.
		prevStart, prevKnown := l.BootStarted, l.BootStartKnown && l.BootID != ""
		// Resets an earlier boot found and never dated: that boot's clock
		// was never set, and nothing places them now.
		for i := range l.Entries {
			if l.Entries[i].TimePending {
				l.Entries[i].TimePending, l.Entries[i].TimeUnknown = false, true
			}
		}
		seen := map[string]bool{}
		for _, d := range l.Seen {
			seen[d] = true
		}
		for _, rec := range Scan(o.Dirs) {
			if seen[rec.Digest] {
				continue
			}
			seen[rec.Digest] = true
			l.Seen = append(l.Seen, rec.Digest)
			if !rec.Crash && clean {
				continue
			}
			e, fatal := classify(rec.Text)
			switch {
			case fatal:
			case rec.Crash && e.Verdict != "":
				// A crash record whose panic line was cut off: its oops.
			case rec.Crash:
				e = Entry{Verdict: Panic, Summary: "unreadable crash record"}
			default:
				continue // a console log with no crash at its end
			}
			e.Record, e.Boot = rec.Name, o.BootID
			// Not the record file's time: it is the clock of the boot that
			// crashed, or of this one (systemd-pstore copies it), either
			// of them perhaps not set yet.
			if prevKnown && e.UptimeS > 0 {
				e.Time = prevStart.Add(time.Duration(e.UptimeS * float64(time.Second)))
			} else {
				e.Time, e.TimePending = o.Now, true
			}
			if !sameCrash(added, e) {
				added = append(added, e)
			}
		}
		sort.SliceStable(added, func(i, j int) bool { return added[i].Time.Before(added[j].Time) })
		if len(added) == 0 && l.BootID != "" && downUnclean(l.State) {
			e := Entry{Time: o.Now, TimePending: true, Boot: o.BootID, Verdict: PowerLoss,
				Summary: "no crash record: power loss or external reset (watchdog, reset line)"}
			if !o.Backend {
				e.Verdict = Unrecorded
				e.Summary = "cause unknown: no crash records on this board"
			}
			if prevKnown {
				e.Detail = "the boot before started " + prevStart.Format("2006-01-02 15:04")
			}
			if l.State == stateReload {
				e.Detail = strings.TrimSuffix("during a driver module reload; "+e.Detail, "; ")
			}
			added = append(added, e)
		}
		l.Entries = append(l.Entries, added...)
		if len(l.Entries) > maxEntries {
			l.Entries = l.Entries[len(l.Entries)-maxEntries:]
		}
		if len(l.Seen) > maxSeen {
			l.Seen = l.Seen[len(l.Seen)-maxSeen:]
		}
		l.BootID, l.BootStarted, l.BootStartKnown = o.BootID, o.Now.Add(-o.Uptime), false
	}
	l.State = stateRunning
	return l, added, errors.Join(loadErr, l.save())
}

// Stopped records that meshd stops: with the system shutting down, or
// while it runs on. Only a boot that follows a running meshd is a reset.
func (l *Log) Stopped(shutdown bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.State = stateStopped
	if shutdown {
		l.State = stateShutdown
	}
	return l.save()
}

// Reloading marks the log while another program has stopped meshd to
// reload a driver module (internal/trial): a crash in the reload, which
// leaves no record on a board without pstore, is then a reset at the next
// boot. The returned function takes the mark back once the module is in;
// meshd's next start marks the log running anyway. Nothing is marked
// unless meshd ran in this boot and stopped.
func Reloading(stateDir, bootID string) (done func()) {
	l, err := Load(stateDir)
	if err != nil || bootID == "" || l.BootID != bootID || l.State != stateStopped {
		return func() {}
	}
	l.State = stateReload
	if l.save() != nil {
		return func() {}
	}
	return func() {
		if l, err := Load(stateDir); err == nil && l.BootID == bootID && l.State == stateReload {
			l.State = stateStopped
			_ = l.save()
		}
	}
}

// ClockSet notes that the clock is set (chrony synchronised) at now,
// uptime into the boot: the boot's start is known from here, and the
// resets this boot found before are dated by it. Once a boot.
func (l *Log) ClockSet(now time.Time, uptime time.Duration) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.BootStartKnown {
		return nil
	}
	l.BootStarted, l.BootStartKnown = now.Add(-uptime).Round(0), true
	for i := range l.Entries {
		if l.Entries[i].TimePending {
			l.Entries[i].Time, l.Entries[i].TimePending = l.BootStarted, false
		}
	}
	return l.save()
}

// Recent returns the entries not older than d at now, newest first; d
// 0: all of them. Before this boot's clock is set, now may be days behind
// and an age means nothing: only the resets this boot found are recent.
func (l *Log) Recent(now time.Time, d time.Duration) []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Entry
	for i := len(l.Entries) - 1; i >= 0; i-- {
		e := l.Entries[i]
		switch {
		case d <= 0:
		case !l.BootStartKnown:
			if e.Boot == "" || e.Boot != l.BootID {
				continue
			}
		case now.Sub(e.Time) > d:
			continue
		}
		out = append(out, e)
	}
	return out
}

func (l *Log) save() error {
	if l.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFile(l.path, append(b, '\n'), 0o644)
}

// sameCrash: the kernel log and the console log of one crash are two
// records with one story.
func sameCrash(have []Entry, e Entry) bool {
	for _, h := range have {
		if h.Verdict == e.Verdict && h.UptimeS == e.UptimeS && h.Detail == e.Detail {
			return true
		}
	}
	return false
}

// Records ----------------------------------------------------------------------

// Record is one crash record file.
type Record struct {
	Name    string
	Path    string
	ModTime time.Time
	Digest  string
	Text    string
	// Crash: the kernel wrote it because it was going down (dmesg-*,
	// except an Oops# record: the kernel can carry on after an oops); a
	// console-* record is written at every boot's end, good or bad.
	Crash bool
}

// Scan reads the crash records under dirs (one level of subdirectories
// too: some versions of systemd-pstore file them by time).
func Scan(dirs []string) []Record {
	var out []Record
	for _, dir := range dirs {
		paths, _ := filepath.Glob(filepath.Join(dir, "*"))
		more, _ := filepath.Glob(filepath.Join(dir, "*", "*"))
		for _, p := range append(paths, more...) {
			name := filepath.Base(p)
			crash := strings.HasPrefix(name, "dmesg-")
			if !crash && !strings.HasPrefix(name, "console-") {
				continue
			}
			st, err := os.Stat(p)
			if err != nil || !st.Mode().IsRegular() {
				continue
			}
			f, err := os.Open(p)
			if err != nil {
				continue
			}
			b, err := io.ReadAll(io.LimitReader(f, maxRecord))
			f.Close()
			if err != nil || len(b) == 0 {
				continue
			}
			if crash && strings.HasPrefix(string(b), "Oops#") {
				crash = false
			}
			sum := sha256.Sum256(b)
			out = append(out, Record{Name: name, Path: p, ModTime: st.ModTime(), Digest: hex.EncodeToString(sum[:8]), Text: string(b), Crash: crash})
		}
	}
	// Kernel logs before console logs: they say more of the same crash.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Crash && !out[j].Crash })
	return out
}

// Backend reports whether the kernel has a pstore backend (ramoops, EFI
// variables): without one a crash leaves no record.
func Backend(sysRoot string) bool {
	b, err := os.ReadFile(filepath.Join(sysRoot, "module", "pstore", "parameters", "backend"))
	if err != nil {
		return false
	}
	s := strings.TrimSpace(string(b))
	return s != "" && s != "(null)"
}

// BootID is this boot's ID.
func BootID() string {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Verdicts ---------------------------------------------------------------------

var (
	reStamp  = regexp.MustCompile(`^(?:<\d+>)?\[\s*(\d+\.\d+)\]\s?(.*)$`)
	reFlood  = regexp.MustCompile(`Disabling IRQ #\d+|irq \d+: nobody cared`)
	reModule = regexp.MustCompile(`\[([a-z0-9_]+)\]`)
	rePanic  = regexp.MustCompile(`Kernel panic - not syncing: (.*)`)
	reOops   = regexp.MustCompile(`Unable to handle kernel .*|Internal error: .*|kernel BUG at .*|BUG: .*`)
)

// Classify reads a crash record and gives its verdict. ok is false for a
// record with no crash in it: an oops alone is not one (the kernel can
// carry on after it); a panic, an SError or a SysRq reset is.
func Classify(text string) (e Entry, ok bool) {
	e, ok = classify(text)
	if !ok {
		return Entry{}, false
	}
	return e, true
}

// classify is Classify that also gives the verdict of an oops with no
// panic after it, with fatal false.
func classify(text string) (e Entry, fatal bool) {
	var (
		serrorLine      string
		panicLine, oops string
		serror, sysrq   bool
		flood, inTrace  bool
		modules         []string
		undervolt       int
		at              float64
	)
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimRight(raw, "\r")
		var stamp float64
		if m := reStamp.FindStringSubmatch(line); m != nil {
			stamp, _ = strconv.ParseFloat(m[1], 64)
			line = m[2]
		}
		switch {
		case strings.Contains(line, "sysrq: Resetting"):
			sysrq, at = true, stamp
		case strings.Contains(line, "Undervoltage detected"):
			undervolt++
		case reFlood.MatchString(line):
			flood = true
		case strings.Contains(line, "SError Interrupt") && !serror:
			serror, serrorLine, at = true, strings.TrimSpace(line), stamp
		case panicLine == "" && rePanic.MatchString(line):
			panicLine = strings.TrimSpace(rePanic.FindStringSubmatch(line)[1])
			if at == 0 {
				at = stamp
			}
		case oops == "" && reOops.MatchString(line):
			oops = strings.TrimSpace(reOops.FindString(line))
			if at == 0 {
				at = stamp
			}
		case strings.Contains(line, "Call trace:"):
			inTrace = true
		case inTrace && len(modules) < 2:
			// The first frames in a module are where the kernel was
			// when it went down: for a card that stopped answering, its
			// driver (often a library module, then the driver proper).
			if m := reModule.FindStringSubmatch(line); m != nil && !slices.Contains(modules, m[1]) {
				modules = append(modules, m[1])
			}
		}
	}
	e.UptimeS = at
	power := ""
	if undervolt > 0 {
		power = fmt.Sprintf(", after %d undervoltage events", undervolt)
		if undervolt == 1 {
			power = ", after 1 undervoltage event"
		}
	}
	in := ""
	if len(modules) > 0 {
		in = " in " + strings.Join(modules, ", ")
	}
	switch {
	case serror && flood:
		e.Verdict, e.Detail = PCIeLink, serrorLine
		e.Summary = "PCIe error flood, then card hang" + in + ": PCIe link fault" + power
	case serror:
		e.Verdict, e.Detail = CardStopped, serrorLine
		e.Summary = "card hang" + in + " (SError): suspect PCIe link or power" + power
	case sysrq:
		e.Verdict = SysRq
		e.Summary = "SysRq reset from serial console: disconnect the console or set kernel.sysrq=0"
	case panicLine != "":
		e.Verdict, e.Detail = Panic, panicLine
		e.Summary = "kernel panic" + in + ": " + panicLine + power
	case oops != "":
		e.Verdict, e.Detail = Panic, oops
		e.Summary = "kernel oops" + in + ": " + oops + power
		return e, false
	default:
		return Entry{}, false
	}
	return e, true
}
