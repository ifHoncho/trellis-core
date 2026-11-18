package regdb

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"meridian/trellis/internal/fsutil"
	"meridian/trellis/internal/nl80211"
)

// Paths are where the distribution's database is looked for, in order: the
// copy moved aside so the kernel turns to Trellis (dpkg-divert names it
// .distrib); Debian's update-alternatives choice and its two variants (the
// /lib/firmware link itself is what gets moved there); the usual place (a
// node not switched over yet); then the copy Trellis ships.
var Paths = []string{
	"/lib/firmware/regulatory.db.distrib",
	"/usr/lib/firmware/regulatory.db.distrib",
	"/etc/alternatives/regulatory.db",
	"/lib/firmware/regulatory.db-debian",
	"/lib/firmware/regulatory.db-upstream",
	"/lib/firmware/regulatory.db",
	"/usr/lib/firmware/regulatory.db",
	"/usr/share/trellis/regulatory.db",
}

// Modes, as in the mesh profile's regulatory.mode.
const (
	ModeCountry      = "country"
	ModeUnrestricted = "unrestricted"
)

// Answer is the regulatory domain to send for a request, and where its
// rules come from.
type Answer struct {
	Domain *nl80211.RegDomain
	Mode   string // country or unrestricted
	Source string // the database file, or "trellis" for the permissive rules
	Note   string // why the answer is not the country's own rules, if so
}

// Decide answers a request for alpha2 in a mode. In country mode it takes
// the first database in paths that loads; a country the database lacks gets
// the world domain's rules under its own alpha2, which is where the kernel
// would end up too (its request would time out).
func Decide(alpha2, mode string, paths []string) (*Answer, error) {
	alpha2 = strings.ToUpper(alpha2)
	if len(alpha2) != 2 {
		return nil, fmt.Errorf("country %q is not two characters", alpha2)
	}
	if mode == ModeUnrestricted {
		return &Answer{Domain: Permissive(alpha2), Mode: mode, Source: "trellis"}, nil
	}
	var errs []string
	for _, p := range paths {
		db, err := Load(p)
		if err != nil {
			if !os.IsNotExist(err) {
				errs = append(errs, err.Error())
			}
			continue
		}
		if rd, ok := db.Country(alpha2); ok {
			return &Answer{Domain: rd, Mode: ModeCountry, Source: p}, nil
		}
		if rd, ok := db.Country("00"); ok {
			rd.Alpha2 = alpha2
			return &Answer{Domain: rd, Mode: ModeCountry, Source: p, Note: alpha2 + " is not in the database: the world domain's rules"}, nil
		}
		errs = append(errs, p+": no "+alpha2+" and no world domain")
	}
	if len(errs) == 0 {
		errs = append(errs, "none of "+strings.Join(paths, ", ")+" exists")
	}
	return nil, fmt.Errorf("no regulatory database for country mode: %s", strings.Join(errs, "; "))
}

// State is what the helper last sent the kernel, for meshd to report.
type State struct {
	Alpha2 string    `json:"alpha2"`
	Mode   string    `json:"mode"`
	Source string    `json:"source"`
	Note   string    `json:"note,omitempty"`
	Time   time.Time `json:"time"`
	Error  string    `json:"error,omitempty"` // the kernel refused it
}

// StateDir holds the state: under /run, so it starts empty at each boot,
// and not meshd's runtime directory, which systemd empties whenever meshd
// stops.
const StateDir = "/run/trellis-crda"

// UdevRule is the rule scripts/install-regulatory.sh installs, which runs
// "trellis crda" for each of the kernel's requests.
const UdevRule = "/etc/udev/rules.d/85-trellis-regulatory.rules"

// StateFile is the state's name in StateDir.
const StateFile = "regdom.json"

// WriteState records an answer in dir, atomically.
func WriteState(dir string, s State) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFile(filepath.Join(dir, StateFile), append(data, '\n'), 0o644)
}

// ReadState reads the last answer recorded in dir.
func ReadState(dir string) (*State, error) {
	data, err := os.ReadFile(filepath.Join(dir, StateFile))
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}
