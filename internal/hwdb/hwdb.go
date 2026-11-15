// Package hwdb is the hardware database: per-adapter facts that outrank what
// drivers report, used for radio ranking and for showing quirks in the UI.
package hwdb

import (
	_ "embed"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed hwdb.yaml
var embedded []byte

// Verification states.
const (
	Verified = "verified"
	Reported = "reported"
	Broken   = "broken"
	Unknown  = "unknown"
)

// Power sources, strongest first.
const (
	SourceMeasured  = "measured"
	SourceDatasheet = "datasheet"
	SourceReported  = "reported"
)

// Match selects adapters by id or driver.
type Match struct {
	USB    []string `yaml:"usb"`
	PCI    []string `yaml:"pci"`
	Driver string   `yaml:"driver"`
	// DriverFallback: the entry also describes adapters on its driver
	// whose ids it does not list. For drivers that run one chip family,
	// sold under many ids (ath9k_htc: AR9271 rebrands), where an unlisted
	// adapter must still get the entry's limits (its max_mesh_mtu keeps
	// the firmware alive). Not for drivers that run unlike chips
	// (rt2800usb: RT5572 and the 2.4 GHz-only RT5372).
	DriverFallback bool `yaml:"driver_fallback"`
}

// Entry is one adapter model.
type Entry struct {
	ID          string             `yaml:"id"`
	Chipset     string             `yaml:"chipset"`
	Match       Match              `yaml:"match"`
	MaxPowerDBm map[string]float64 `yaml:"max_power_dbm"`
	// Bands, when set, are the bands the radio really has, and replace what
	// the driver reports. Morse Micro's HaLow driver needs it: it registers
	// its sub-1 GHz channels with cfg80211 as 5 GHz ones (by design; Morse's
	// tools map between the two), and the planner must not take it for a
	// 5 GHz radio.
	Bands           []string `yaml:"bands"`
	PowerSource     string   `yaml:"power_source"`
	MeshPoint       string   `yaml:"mesh_point"`
	SAEMesh         string   `yaml:"sae_mesh"`
	MeshAPTogether  string   `yaml:"mesh_ap_together"`
	VerifiedKernels []string `yaml:"verified_kernels"`
	Quirks          []string `yaml:"quirks"`
	Notes           string   `yaml:"notes"`
	// ModuleParams are kernel module parameters the driver (Match.Driver)
	// needs for a working encrypted mesh. The package writes them to
	// /etc/modprobe.d and meshctl prepare reloads the module; meshd warns
	// while the loaded module differs.
	ModuleParams map[string]string `yaml:"module_params"`
	// MaxMeshMTU, when set, is the largest MTU meshd gives a mesh interface
	// on this radio, whatever node.yaml's mesh_mtu says: the radio cannot
	// send a larger frame (the AR9271's firmware stops for good on one).
	// batman-adv fragments what does not fit.
	MaxMeshMTU int `yaml:"max_mesh_mtu"`
}

// MeshMTU is the MTU for a mesh interface on this radio when the node asks
// for want. A nil entry has no limit.
func (e *Entry) MeshMTU(want int) int {
	if e != nil && e.MaxMeshMTU > 0 && want > e.MaxMeshMTU {
		return e.MaxMeshMTU
	}
	return want
}

// DB is the loaded database.
type DB struct {
	Schema  int     `yaml:"schema"`
	Entries []Entry `yaml:"entries"`
}

// Load returns the database embedded in the binary.
func Load() (*DB, error) { return Parse(embedded) }

// Parse loads a database from YAML.
func Parse(data []byte) (*DB, error) {
	var db DB
	if err := yaml.Unmarshal(data, &db); err != nil {
		return nil, fmt.Errorf("hardware database: %w", err)
	}
	for i := range db.Entries {
		e := &db.Entries[i]
		if e.ID == "" {
			return nil, fmt.Errorf("hardware database: entry %d has no id", i)
		}
		for j, id := range e.Match.USB {
			e.Match.USB[j] = strings.ToLower(id)
		}
		for j, id := range e.Match.PCI {
			e.Match.PCI[j] = strings.ToLower(id)
		}
		for _, b := range e.Bands {
			switch b {
			case "2g", "5g", "6g", "s1g":
			default:
				return nil, fmt.Errorf("hardware database: entry %s: unknown band %q", e.ID, b)
			}
		}
		if e.MaxMeshMTU != 0 && e.MaxMeshMTU < 1280 {
			return nil, fmt.Errorf("hardware database: entry %s: max_mesh_mtu %d is below 1280", e.ID, e.MaxMeshMTU)
		}
		if e.MeshPoint == "" {
			e.MeshPoint = Unknown
		}
		if e.SAEMesh == "" {
			e.SAEMesh = Unknown
		}
		if e.MeshAPTogether == "" {
			e.MeshAPTogether = Unknown
		}
	}
	return &db, nil
}

// Lookup finds the entry for an adapter. An id match (USB or PCI) wins over a
// driver-only match. Returns nil when nothing matches.
//
// A driver-only match is made only where it can't name the wrong chip: for
// an entry that lists no ids on the adapter's bus (it describes a driver),
// or for an adapter without an id. An entry that lists ids describes those
// chips, and one driver often runs many: rt2800usb drives the RT5572 and the
// 2.4 GHz-only RT5372 alike; matching by driver would give an RT5372 the
// RT5572's bands and power figures.
func (db *DB) Lookup(bus, deviceID, driver string) *Entry {
	deviceID = strings.ToLower(deviceID)
	var byDriver *Entry
	for i := range db.Entries {
		e := &db.Entries[i]
		ids := e.Match.USB
		if bus == "pci" {
			ids = e.Match.PCI
		}
		for _, id := range ids {
			if deviceID != "" && id == deviceID {
				return e
			}
		}
		if byDriver == nil && e.Match.Driver != "" && driver != "" && e.Match.Driver == driver && (len(ids) == 0 || deviceID == "" || e.Match.DriverFallback) {
			byDriver = e
		}
	}
	return byDriver
}

// ModprobeOptions returns one "options <module> k=v …" line per driver with
// required module parameters, sorted, for /etc/modprobe.d.
func (db *DB) ModprobeOptions() []string {
	byMod := map[string]map[string]string{}
	for _, e := range db.Entries {
		if e.Match.Driver == "" || len(e.ModuleParams) == 0 {
			continue
		}
		m := byMod[e.Match.Driver]
		if m == nil {
			m = map[string]string{}
			byMod[e.Match.Driver] = m
		}
		for k, v := range e.ModuleParams {
			m[k] = v
		}
	}
	var out []string
	for mod, params := range byMod {
		keys := make([]string, 0, len(params))
		for k := range params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		line := "options " + mod
		for _, k := range keys {
			line += " " + k + "=" + params[k]
		}
		out = append(out, line)
	}
	sort.Strings(out)
	return out
}

// ParamMatches compares a module parameter as sysfs shows it with the
// wanted value: bool parameters read back as Y/N.
func ParamMatches(have, want string) bool {
	norm := func(v string) string {
		switch strings.ToUpper(strings.TrimSpace(v)) {
		case "Y", "1", "TRUE", "ON":
			return "1"
		case "N", "0", "FALSE", "OFF":
			return "0"
		}
		return strings.TrimSpace(v)
	}
	return norm(have) == norm(want)
}
