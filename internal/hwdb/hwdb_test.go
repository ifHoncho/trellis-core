package hwdb

import (
	"strings"
	"testing"
)

func TestEmbeddedLoads(t *testing.T) {
	db, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(db.Entries) == 0 {
		t.Fatal("no entries")
	}
	e := db.Lookup("usb", "0E8D:7612", "mt76x2u")
	if e == nil || e.ID != "mt7612u" {
		t.Fatalf("lookup by id: %+v", e)
	}
	if e.MaxPowerDBm["5g"] != 20 {
		t.Errorf("power = %v", e.MaxPowerDBm)
	}
	// An adapter with an id the entry does not list is another chip, even on
	// the same driver.
	if e := db.Lookup("usb", "ffff:ffff", "mt76x2u"); e != nil {
		t.Errorf("an unlisted id matched by driver: %+v", e)
	}
	// Without an id, the driver is all there is to go on.
	if e := db.Lookup("usb", "", "mt76x2u"); e == nil || e.ID != "mt7612u" {
		t.Errorf("lookup by driver without an id: %+v", e)
	}
	// An entry for a driver rather than for chips matches every adapter on it.
	if e := db.Lookup("pci", "8086:2725", "iwlwifi"); e == nil || e.ID != "intel-iwlwifi" {
		t.Errorf("driver entry: %+v", e)
	}
	// An RT5372 (148f:5372), 2.4 GHz only, on the same driver as the RT5572:
	// it must get its own entry, not the RT5572's.
	if e := db.Lookup("usb", "148F:5372", "rt2800usb"); e == nil || e.ID != "rt5372" || len(e.Bands) != 1 || e.Bands[0] != "2g" {
		t.Errorf("RT5372: %+v", e)
	}
	// An AR9271 under an id the entry does not list still gets it: its
	// MTU limit keeps the firmware alive (driver_fallback).
	if e := db.Lookup("usb", "0846:9030", "ath9k_htc"); e == nil || e.ID != "ar9271" || e.MeshMTU(1560) != 1480 {
		t.Errorf("unlisted AR9271 = %+v, want the ar9271 entry with its MTU limit", e)
	}
	if e := db.Lookup("usb", "148f:3070", "rt2800usb"); e != nil {
		t.Errorf("another rt2800usb chip matched an entry: %+v", e)
	}
	if e := db.Lookup("usb", "ffff:ffff", "nothing"); e != nil {
		t.Errorf("unexpected match: %+v", e)
	}
	if e := db.Lookup("usb", "", "88XXau"); e == nil || e.MeshPoint != Broken {
		t.Errorf("broken driver: %+v", e)
	}
}

func TestModprobeOptions(t *testing.T) {
	db, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, l := range db.ModprobeOptions() {
		have[l] = true
	}
	for _, want := range []string{"options ath9k_htc nohwcrypt=1", "options rt2800usb nohwcrypt=1"} {
		if !have[want] {
			t.Errorf("%q missing: %v", want, db.ModprobeOptions())
		}
	}
	if !ParamMatches("Y", "1") || !ParamMatches("0", "0") || ParamMatches("0", "1") {
		t.Error("ParamMatches")
	}
}

func TestReferenceBuildEntries(t *testing.T) {
	db, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if e := db.Lookup("pci", "14C3:7906", "mt7915e"); e == nil || e.ID != "mt7916" {
		t.Errorf("MT7916 by PCI id: %+v", e)
	}
	if e := db.Lookup("pci", "", "mt7915e"); e == nil || e.ID != "mt7916" {
		t.Errorf("MT7916 by driver: %+v", e)
	}
	e := db.Lookup("spi", "", "morse_spi")
	if e == nil || e.ID != "mm6108" {
		t.Fatalf("MM6108 by driver: %+v", e)
	}
	// Its driver registers the channels as 5 GHz ones; the
	// database must say what they are.
	if len(e.Bands) != 1 || e.Bands[0] != "s1g" {
		t.Errorf("MM6108 bands = %v, want [s1g]", e.Bands)
	}
	if len(db.ModprobeOptions()) == 0 {
		t.Fatal("existing module options lost")
	}
	for _, line := range db.ModprobeOptions() {
		if strings.Contains(line, "mt7915e") || strings.Contains(line, "morse") {
			t.Errorf("no module options are known yet: %q", line)
		}
	}
}

func TestParseRejectsUnknownBand(t *testing.T) {
	_, err := Parse([]byte("schema: 0\nentries:\n  - id: x\n    bands: [900m]\n"))
	if err == nil || !strings.Contains(err.Error(), "unknown band") {
		t.Errorf("want an unknown band error, got %v", err)
	}
}

func TestMeshMTU(t *testing.T) {
	db, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	ar := db.Lookup("usb", "0cf3:9271", "ath9k_htc")
	if ar == nil {
		t.Fatal("no AR9271 entry")
	}
	// The AR9271's firmware stops on a frame over 1600 bytes with headers.
	if got := ar.MeshMTU(1560); got != 1480 {
		t.Errorf("AR9271 with mesh_mtu 1560: MTU %d, want 1480", got)
	}
	if got := ar.MeshMTU(1400); got != 1400 {
		t.Errorf("AR9271 with mesh_mtu 1400: MTU %d, want 1400", got)
	}
	var none *Entry
	if got := none.MeshMTU(1560); got != 1560 {
		t.Errorf("unknown radio: MTU %d, want 1560", got)
	}
	if got := (&Entry{}).MeshMTU(1560); got != 1560 {
		t.Errorf("radio without a limit: MTU %d, want 1560", got)
	}
	if _, err := Parse([]byte("schema: 1\nentries:\n  - id: x\n    max_mesh_mtu: 900\n")); err == nil {
		t.Error("max_mesh_mtu 900 accepted")
	}
}

func TestWidthCost(t *testing.T) {
	db, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	mt := db.Lookup("pci", "14c3:7906", "mt7915e")
	if mt == nil {
		t.Fatal("no MT7916 entry")
	}
	if _, ok := mt.WidthCost("5g", 20); ok {
		t.Error("20 MHz has a cost")
	}
	c, ok := mt.WidthCost("5g", 80)
	if !ok || !c.PowerKnown || c.PowerDBm != 13.5 || c.BasePowerDBm != 20 || c.PowerDB != 6.5 {
		t.Fatalf("80 MHz: %+v", c)
	}
	if c.NoiseDB < 6.0 || c.NoiseDB > 6.1 || c.TotalDB < 12.5 || c.TotalDB > 12.6 {
		t.Errorf("80 MHz: noise %.2f total %.2f", c.NoiseDB, c.TotalDB)
	}
	// 2.4 GHz has no 80 MHz figure, and an adapter without a table none at
	// all: the noise alone.
	for _, e := range []*Entry{mt, nil, db.Lookup("usb", "0e8d:7610", "mt76x0u")} {
		band := "2g"
		c, ok := e.WidthCost(band, 80)
		if !ok || c.PowerKnown || c.PowerDB != 0 || c.TotalDB != c.NoiseDB {
			t.Errorf("%v: %+v", e, c)
		}
	}
	// HaLow is measured against 1 MHz.
	if c, ok := (*Entry)(nil).WidthCost("s1g", 4); !ok || c.BaseMHz != 1 || c.NoiseDB < 6 || c.NoiseDB > 6.1 {
		t.Errorf("s1g 4 MHz: %+v", c)
	}
	if _, err := Parse([]byte("entries:\n  - id: x\n    power_by_width_dbm: {7g: {20: 1}}\n")); err == nil {
		t.Error("an unknown band in power_by_width_dbm was taken")
	}
}

// Two entries of one driver may not ask for different values of the same
// module parameter: the modprobe line would depend on map order.
func TestParseModuleParamConflict(t *testing.T) {
	db := []byte(`schema: 1
entries:
  - id: a
    match: {driver: drv, usb: ["1234:0001"]}
    module_params: {nohwcrypt: "1"}
  - id: b
    match: {driver: drv, usb: ["1234:0002"]}
    module_params: {nohwcrypt: "1"}
`)
	if _, err := Parse(db); err != nil {
		t.Fatalf("the same value twice: %v", err)
	}
	bad := []byte(string(db) + `  - id: c
    match: {driver: drv, usb: ["1234:0003"]}
    module_params: {nohwcrypt: "0"}
`)
	if _, err := Parse(bad); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("conflicting values: %v", err)
	}
}
