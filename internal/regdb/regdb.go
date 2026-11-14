// Package regdb reads wireless-regdb's regulatory.db and answers the
// kernel's regulatory requests with it, or with Trellis's permissive rules
// (architecture §5.1, "How unrestricted is implemented").
//
// The format is the one the kernel reads itself (net/wireless/reg.c,
// regdb_query_country): a header, a list of countries, and per country a
// collection of rule pointers. Offsets are in units of four bytes. The
// signature (regulatory.db.p7s) is not checked: the file comes from the
// distribution's package, and only root can replace it.
package regdb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"

	"meridian/trellis/internal/nl80211"
)

const (
	magic   = 0x52474442 // "RGDB"
	version = 20
)

// Rule flags in the file (fwdb_rule.flags).
const (
	flagNoOFDM    = 1 << 0
	flagNoOutdoor = 1 << 1
	flagDFS       = 1 << 2
	flagNoIR      = 1 << 3
	flagAutoBW    = 1 << 4
)

// DB is a parsed regulatory.db.
type DB struct {
	data      []byte
	countries map[string]uint16 // alpha2 -> collection pointer (x4)
	order     []string
}

// Load reads and parses a regulatory.db file.
func Load(path string) (*DB, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	db, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return db, nil
}

// Parse checks a regulatory.db's structure as the kernel does and indexes
// its countries.
func Parse(data []byte) (*DB, error) {
	if len(data) < 8 {
		return nil, errors.New("too short for a regulatory.db header")
	}
	if m := binary.BigEndian.Uint32(data); m != magic {
		return nil, fmt.Errorf("not a regulatory.db (magic %#x)", m)
	}
	if v := binary.BigEndian.Uint32(data[4:]); v != version {
		return nil, fmt.Errorf("regulatory.db version %d, this reader knows %d", v, version)
	}
	db := &DB{data: data, countries: map[string]uint16{}}
	for off := 8; ; off += 4 {
		if off+4 > len(data) {
			return nil, errors.New("country list runs past the end")
		}
		ptr := binary.BigEndian.Uint16(data[off+2:])
		if ptr == 0 {
			break
		}
		alpha2 := string(data[off : off+2])
		if _, err := db.collection(alpha2, ptr); err != nil {
			return nil, err
		}
		db.countries[alpha2] = ptr
		db.order = append(db.order, alpha2)
	}
	if len(db.countries) == 0 {
		return nil, errors.New("no countries")
	}
	return db, nil
}

// Countries lists the alpha2 codes in the file's order.
func (db *DB) Countries() []string { return append([]string(nil), db.order...) }

// Country returns a country's rules as the kernel would apply them from
// this file, and whether the file has the country.
func (db *DB) Country(alpha2 string) (*nl80211.RegDomain, bool) {
	ptr, ok := db.countries[strings.ToUpper(alpha2)]
	if !ok {
		return nil, false
	}
	rd, err := db.collection(strings.ToUpper(alpha2), ptr)
	if err != nil {
		return nil, false // checked in Parse
	}
	return rd, true
}

// collection decodes one country's collection of rules.
func (db *DB) collection(alpha2 string, ptr uint16) (*nl80211.RegDomain, error) {
	d := db.data
	at := int(ptr) << 2
	if at+3 > len(d) {
		return nil, fmt.Errorf("%s: collection past the end", alpha2)
	}
	length, nRules, dfsRegion := int(d[at]), int(d[at+1]), d[at+2]
	if length < 3 {
		return nil, fmt.Errorf("%s: collection header of %d bytes", alpha2, length)
	}
	rulesAt := at + (length+1)&^1 // ALIGN(len, 2)
	if rulesAt+2*nRules > len(d) {
		return nil, fmt.Errorf("%s: rule pointers past the end", alpha2)
	}
	rd := &nl80211.RegDomain{Alpha2: alpha2, DFSRegion: dfsRegion}
	for i := 0; i < nRules; i++ {
		r := int(binary.BigEndian.Uint16(d[rulesAt+2*i:])) << 2
		if r+16 > len(d) {
			return nil, fmt.Errorf("%s: rule %d past the end", alpha2, i)
		}
		rlen := int(d[r])
		if rlen < 16 || r+rlen > len(d) {
			return nil, fmt.Errorf("%s: rule %d of %d bytes", alpha2, i, rlen)
		}
		flags := d[r+1]
		rule := nl80211.RegRule{
			MaxEIRPMBm:      uint32(binary.BigEndian.Uint16(d[r+2:])),
			StartKHz:        binary.BigEndian.Uint32(d[r+4:]),
			EndKHz:          binary.BigEndian.Uint32(d[r+8:]),
			MaxBandwidthKHz: binary.BigEndian.Uint32(d[r+12:]),
		}
		for _, f := range []struct {
			file byte
			rrf  uint32
		}{
			{flagNoOFDM, nl80211.NL80211_RRF_NO_OFDM}, {flagNoOutdoor, nl80211.NL80211_RRF_NO_OUTDOOR},
			{flagDFS, nl80211.NL80211_RRF_DFS}, {flagNoIR, nl80211.NL80211_RRF_NO_IR}, {flagAutoBW, nl80211.NL80211_RRF_AUTO_BW},
		} {
			if flags&f.file != 0 {
				rule.Flags |= f.rrf
			}
		}
		if rlen >= 18 { // optional cac_timeout, in seconds
			rule.DFSCACTimeMS = 1000 * uint32(binary.BigEndian.Uint16(d[r+16:]))
		}
		if rule.StartKHz == 0 || rule.EndKHz <= rule.StartKHz || rule.MaxBandwidthKHz > rule.EndKHz-rule.StartKHz {
			return nil, fmt.Errorf("%s: rule %d has an impossible range", alpha2, i)
		}
		rd.Rules = append(rd.Rules, rule)
	}
	return rd, nil
}

// PermissiveEIRPMBm is the power in the permissive rules: above any Wi-Fi
// or HaLow radio's own limit, so the radio's driver and firmware decide.
const PermissiveEIRPMBm = 4000

// Permissive is the rule set of unrestricted mode, under the alpha2 the
// kernel asked for: every band at the widest width, no radar, passive-only,
// indoor or OFDM restriction, and power left to the radio.
func Permissive(alpha2 string) *nl80211.RegDomain {
	r := func(startMHz, endMHz, bwMHz uint32) nl80211.RegRule {
		return nl80211.RegRule{StartKHz: startMHz * 1000, EndKHz: endMHz * 1000, MaxBandwidthKHz: bwMHz * 1000, MaxEIRPMBm: PermissiveEIRPMBm}
	}
	return &nl80211.RegDomain{Alpha2: strings.ToUpper(alpha2), Rules: []nl80211.RegRule{
		r(750, 1000, 16),      // sub-1 GHz (HaLow), for radios that follow the kernel's domain
		r(2400, 2500, 40),     // 2.4 GHz, channels 1–14
		r(4900, 5925, 160),    // 4.9 and 5 GHz, channels 182–196 and 1–177
		r(5925, 7125, 320),    // 6 GHz
		r(57240, 71000, 2160), // 60 GHz
	}}
}

// IsPermissive reports whether a regulatory domain in force is Trellis's
// permissive rule set (whatever its alpha2), that is, whether unrestricted
// mode is what the kernel applies.
func IsPermissive(rd *nl80211.RegDomain) bool {
	if rd == nil {
		return false
	}
	want := Permissive(rd.Alpha2).Rules
	if len(rd.Rules) != len(want) {
		return false
	}
	for i, r := range rd.Rules {
		w := want[i]
		if r.StartKHz != w.StartKHz || r.EndKHz != w.EndKHz || r.MaxBandwidthKHz != w.MaxBandwidthKHz || r.Flags != w.Flags || r.MaxEIRPMBm != w.MaxEIRPMBm {
			return false
		}
	}
	return true
}
