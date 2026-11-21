// Package hwdb is the hardware database: per-adapter facts that outrank what
// drivers report, used for radio ranking and for showing quirks in the UI.
package hwdb

import (
	_ "embed"
	"fmt"
	"math"
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
	Bands []string `yaml:"bands"`
	// ExclusiveBands are bands one radio of the adapter has but runs one
	// at a time, chosen by its driver. A driver that offers them all at
	// once has not chosen: the radio then works on none of them.
	ExclusiveBands []string `yaml:"exclusive_bands"`
	// PowerByWidthDBm is the transmit power per chain at each channel
	// width, by band and width in MHz, where the adapter's maker says: a
	// wider channel is sent at higher rates, which need a cleaner signal,
	// so the amplifiers are backed off.
	PowerByWidthDBm map[string]map[int]float64 `yaml:"power_by_width_dbm"`
	PowerSource     string                     `yaml:"power_source"`
	MeshPoint       string                     `yaml:"mesh_point"`
	SAEMesh         string                     `yaml:"sae_mesh"`
	MeshAPTogether  string                     `yaml:"mesh_ap_together"`
	VerifiedKernels []string                   `yaml:"verified_kernels"`
	Quirks          []string                   `yaml:"quirks"`
	Notes           string                     `yaml:"notes"`
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
	// One value per module parameter: ModprobeOptions merges the entries
	// of a driver, and a conflict would come out in map order.
	type param struct{ value, entry string }
	params := map[string]param{} // "driver parameter" -> its value, and the entry that set it
	for i := range db.Entries {
		e := &db.Entries[i]
		if e.ID == "" {
			return nil, fmt.Errorf("hardware database: entry %d has no id", i)
		}
		for k, v := range e.ModuleParams {
			key := e.Match.Driver + " " + k
			if prev, ok := params[key]; ok && prev.value != v {
				return nil, fmt.Errorf("hardware database: entry %s: module parameter %s=%s conflicts with %s=%s (entry %s)", e.ID, key, v, key, prev.value, prev.entry)
			}
			params[key] = param{v, e.ID}
		}
		for j, id := range e.Match.USB {
			e.Match.USB[j] = strings.ToLower(id)
		}
		for j, id := range e.Match.PCI {
			e.Match.PCI[j] = strings.ToLower(id)
		}
		for _, b := range e.Bands {
			if !knownBand(b) {
				return nil, fmt.Errorf("hardware database: entry %s: unknown band %q", e.ID, b)
			}
		}
		for _, b := range e.ExclusiveBands {
			if !knownBand(b) {
				return nil, fmt.Errorf("hardware database: entry %s: exclusive_bands: unknown band %q", e.ID, b)
			}
		}
		for b, byWidth := range e.PowerByWidthDBm {
			if !knownBand(b) {
				return nil, fmt.Errorf("hardware database: entry %s: power_by_width_dbm: unknown band %q", e.ID, b)
			}
			for w := range byWidth {
				if w <= 0 {
					return nil, fmt.Errorf("hardware database: entry %s: power_by_width_dbm: width %d", e.ID, w)
				}
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

func knownBand(b string) bool {
	switch b {
	case "2g", "5g", "6g", "s1g":
		return true
	}
	return false
}

// BaseWidthMHz is a band's narrowest channel width: what a link's budget
// is measured against.
func BaseWidthMHz(band string) int {
	if band == "s1g" {
		return 1
	}
	return 20
}

// WidthCost is the link budget a channel width costs against the band's
// narrowest: the noise a wider channel takes in, and the transmit power an
// adapter gives up at it.
type WidthCost struct {
	WidthMHz, BaseMHz      int
	NoiseDB                float64
	PowerKnown             bool
	PowerDBm, BasePowerDBm float64
	PowerDB, TotalDB       float64
}

// WidthCost works out what a width costs on this adapter. ok is false at
// the band's narrowest width (and below): nothing is lost there. A nil
// entry, or one without power per width for the band, gives the noise
// alone.
func (e *Entry) WidthCost(band string, width int) (c WidthCost, ok bool) {
	base := BaseWidthMHz(band)
	if width <= base {
		return c, false
	}
	c = WidthCost{WidthMHz: width, BaseMHz: base, NoiseDB: 10 * math.Log10(float64(width)/float64(base))}
	if e != nil {
		pw, okW := e.PowerByWidthDBm[band][width]
		pb, okB := e.PowerByWidthDBm[band][base]
		if okW && okB {
			c.PowerKnown, c.PowerDBm, c.BasePowerDBm, c.PowerDB = true, pw, pb, pb-pw
		}
	}
	c.TotalDB = c.NoiseDB + c.PowerDB
	return c, true
}
