package radio

import (
	"net"
	"strings"
	"testing"

	"meridian/trellis/internal/config"
	"meridian/trellis/internal/hwdb"
	"meridian/trellis/internal/nl80211"
	"meridian/trellis/internal/sysfs"
)

func mac(s string) net.HardwareAddr {
	m, err := net.ParseMAC(s)
	if err != nil {
		panic(err)
	}
	return m
}

type spec struct {
	phy, mac, port, serial, id string
	bus                        string
	speed                      int
	mesh, ap, both             bool
	bands                      []string
	power                      map[string]float64
	streams                    int
}

func mkRadio(s spec) *Radio {
	r := &Radio{
		Phy:      s.phy,
		PermAddr: mac(s.mac),
		Device:   sysfs.Device{Bus: s.bus, Port: s.port, Serial: s.serial, USBSpeedMbps: s.speed, Path: "/sys/devices/x/" + s.phy},
		Caps:     Caps{MeshPoint: s.mesh, AP: s.ap, MeshAndAP: s.both, Bands: s.bands, Streams: s.streams},
		Power:    map[string]PowerFact{},
	}
	if s.id != "" {
		r.Device.VendorID, r.Device.ProductID = s.id[:4], s.id[5:]
	}
	if r.Device.Bus == "" {
		r.Device.Bus = sysfs.BusUSB
	}
	for b, p := range s.power {
		r.Power[b] = PowerFact{DBm: p, Source: PowerDriver}
	}
	return r
}

var (
	strong = spec{phy: "phy0", mac: "00:c0:ca:11:11:11", port: "usb:1-1", id: "0e8d:7612", bus: "usb", speed: 5000,
		mesh: true, ap: true, both: true, bands: []string{"2g", "5g"}, power: map[string]float64{"2g": 20, "5g": 23}, streams: 2}
	weak = spec{phy: "phy1", mac: "00:c0:ca:22:22:22", port: "usb:1-2", id: "0cf3:9271", bus: "usb", speed: 480,
		mesh: true, ap: true, both: true, bands: []string{"2g"}, power: map[string]float64{"2g": 17}, streams: 1}
	mid = spec{phy: "phy2", mac: "00:c0:ca:33:33:33", port: "usb:1-3", id: "148f:5572", bus: "usb", speed: 480,
		mesh: true, ap: true, both: false, bands: []string{"2g", "5g"}, power: map[string]float64{"2g": 18, "5g": 17}, streams: 2}
)

func radios(specs ...spec) []*Radio {
	var out []*Radio
	for _, s := range specs {
		out = append(out, mkRadio(s))
	}
	AssignFingerprints(out)
	return out
}

func auto() config.RoleOverrides {
	return config.RoleOverrides{Mesh: config.RoleSelection{Mode: config.RoleAuto}, AP: config.RoleSelection{Mode: config.RoleAuto}}
}

func TestFingerprintSources(t *testing.T) {
	// Good unique MACs.
	rs := radios(strong, weak)
	if rs[0].FingerprintSource != SourcePermanentMAC || rs[0].Fingerprint != "mac:00:c0:ca:11:11:11" {
		t.Errorf("good mac: %s %s", rs[0].Fingerprint, rs[0].FingerprintSource)
	}
	// Duplicated MAC across two units: both fall back; serial wins when present.
	a, b := strong, strong
	b.phy, b.port = "phy1", "usb:1-2"
	a.serial, b.serial = "SN0001", "SN0002"
	rs = radios(a, b)
	for _, r := range rs {
		if r.FingerprintSource != SourceUSBSerial {
			t.Errorf("dup mac with serial: %s %s", r.Fingerprint, r.FingerprintSource)
		}
	}
	// Duplicated MAC, bogus serials: port + id.
	a.serial, b.serial = "0000000000", "0000000000"
	rs = radios(a, b)
	for _, r := range rs {
		if r.FingerprintSource != SourcePortAndID {
			t.Errorf("dup mac no serial: %s %s", r.Fingerprint, r.FingerprintSource)
		}
	}
	if rs[0].Fingerprint == rs[1].Fingerprint {
		t.Error("fingerprints must differ")
	}
	// Random (locally administered) MAC alone: next source.
	r := strong
	r.mac = "02:11:22:33:44:55"
	rs = radios(r)
	if rs[0].FingerprintSource == SourcePermanentMAC {
		t.Errorf("locally administered MAC must not be used: %s", rs[0].Fingerprint)
	}
	// PCI radio uses its bus address.
	pci := spec{phy: "phy5", mac: "02:00:00:00:00:01", port: "pci:0000:01:00.0", bus: "pci", mesh: true, ap: true, bands: []string{"5g"}}
	rs = radios(pci)
	if rs[0].FingerprintSource != SourcePCIAddress || rs[0].Fingerprint != "pci:0000:01:00.0" {
		t.Errorf("pci: %s %s", rs[0].Fingerprint, rs[0].FingerprintSource)
	}
}

func TestPlanOrderIndependent(t *testing.T) {
	a := MakePlan(Input{Radios: radios(strong, weak, mid), MeshBand: "5g", APEnabled: true, Roles: auto()})
	b := MakePlan(Input{Radios: radios(mid, weak, strong), MeshBand: "5g", APEnabled: true, Roles: auto()})
	if a.MeshFingerprint() != b.MeshFingerprint() || a.APFingerprint() != b.APFingerprint() {
		t.Errorf("order dependent: %v vs %v", a.Roles, b.Roles)
	}
	if a.Mesh.Phy != "phy0" {
		t.Errorf("mesh should be the strongest 5g radio, got %s", a.Mesh.Phy)
	}
	// AP band is 2g (mesh on 5g); lowest power on 2g among the rest is weak (17).
	if a.AP.Phy != "phy1" {
		t.Errorf("ap should be the lowest-power 2g radio, got %s", a.AP.Phy)
	}
	if a.Roles["mac:00:c0:ca:33:33:33"] != RoleUnused {
		t.Errorf("third radio should be unused: %v", a.Roles)
	}
}

func TestPlanTieBreakStable(t *testing.T) {
	a, b := strong, strong
	b.phy, b.mac, b.port = "phy9", "00:c0:ca:00:00:01", "usb:1-9"
	p1 := MakePlan(Input{Radios: radios(a, b), MeshBand: "5g", APEnabled: true, Roles: auto()})
	p2 := MakePlan(Input{Radios: radios(b, a), MeshBand: "5g", APEnabled: true, Roles: auto()})
	if p1.MeshFingerprint() != p2.MeshFingerprint() || p1.MeshFingerprint() != "mac:00:c0:ca:00:00:01" {
		t.Errorf("tie-break by fingerprint failed: %s / %s", p1.MeshFingerprint(), p2.MeshFingerprint())
	}
}

func TestMeshLossTakesAPRadio(t *testing.T) {
	rs := radios(strong, mid)
	boot := MakePlan(Input{Radios: rs, MeshBand: "5g", APEnabled: true, Roles: auto()})
	if boot.Mesh.Phy != "phy0" || boot.AP.Phy != "phy2" {
		t.Fatalf("boot plan: %v", boot.Roles)
	}
	// Mesh radio disappears.
	after := MakePlan(Input{Radios: radios(mid), MeshBand: "5g", APEnabled: true, Roles: auto(), Current: boot.Roles})
	if after.Mesh == nil || after.Mesh.Phy != "phy2" {
		t.Fatalf("mesh should move to the remaining radio: %v", after.Roles)
	}
	// mid cannot do mesh+ap, so the AP is gone with a warning.
	if after.AP != nil || !hasNotice(after, "no-ap-radio") {
		t.Errorf("expected no AP and a warning: %+v", after.Notices)
	}
}

func TestNewRadioFillsEmptyRoleOnly(t *testing.T) {
	// Boot with only the weak radio: it carries mesh (on 2g) and AP together.
	boot := MakePlan(Input{Radios: radios(weak), MeshBand: "2g", APEnabled: true, Roles: auto()})
	if boot.Roles["mac:00:c0:ca:22:22:22"] != RoleMeshAP || !hasNotice(boot, "ap-shares-mesh-radio") {
		t.Fatalf("single radio: %v %+v", boot.Roles, boot.Notices)
	}
	// Plug in the strong radio at runtime.
	rt := MakePlan(Input{Radios: radios(weak, strong), MeshBand: "2g", APEnabled: true, Roles: auto(), Current: boot.Roles})
	if rt.Mesh.Phy != "phy1" {
		t.Errorf("mesh must stay on the working radio, got %s", rt.Mesh.Phy)
	}
	if rt.AP == nil || rt.AP.Phy != "phy0" {
		t.Errorf("new radio should take the empty AP role, got %+v", rt.Roles)
	}
	if !rt.Differs || rt.Recommended["mac:00:c0:ca:11:11:11"] != RoleMesh {
		t.Errorf("recommendation should prefer the strong radio for mesh: %v differs=%v", rt.Recommended, rt.Differs)
	}
}

func TestSingleRadioWithoutCombination(t *testing.T) {
	only := mid // mesh and AP, but not together
	p := MakePlan(Input{Radios: radios(only), MeshBand: "5g", APEnabled: true, Roles: auto()})
	if p.Roles["mac:00:c0:ca:33:33:33"] != RoleMesh || !hasNotice(p, "no-ap-radio") {
		t.Errorf("mesh only expected with warning: %v %+v", p.Roles, p.Notices)
	}
}

func TestPins(t *testing.T) {
	rs := radios(strong, weak)
	pinPort := config.RoleOverrides{
		Mesh: config.RoleSelection{Mode: "port", Port: "usb:1-2"},
		AP:   config.RoleSelection{Mode: config.RoleAuto},
	}
	p := MakePlan(Input{Radios: rs, MeshBand: "2g", APEnabled: true, Roles: pinPort})
	if p.Mesh.Phy != "phy1" || p.AP.Phy != "phy0" {
		t.Errorf("pin by port: %v", p.Roles)
	}
	missing := config.RoleOverrides{
		Mesh: config.RoleSelection{Mode: "radio", Radio: "mac:de:ad:be:ef:00:00"},
		AP:   config.RoleSelection{Mode: config.RoleAuto},
	}
	p = MakePlan(Input{Radios: rs, MeshBand: "2g", APEnabled: true, Roles: missing})
	if p.Mesh != nil || !hasNotice(p, "mesh-pin-missing") {
		t.Errorf("missing pin must not fall back silently: %v %+v", p.Roles, p.Notices)
	}
}

func TestIgnoredAndIneligible(t *testing.T) {
	rs := radios(strong, weak)
	rs[0].Ignored = true
	p := MakePlan(Input{Radios: rs, MeshBand: "5g", APEnabled: true, Roles: auto()})
	if p.Mesh != nil || !hasNotice(p, "no-mesh-radio") {
		t.Errorf("ignored strong radio and 2g-only weak radio: no 5g mesh expected, got %v %+v", p.Roles, p.Notices)
	}
	p = MakePlan(Input{Radios: nil, MeshBand: "5g", APEnabled: true, Roles: auto()})
	if !hasNotice(p, "no-radios") {
		t.Error("no radios notice expected")
	}
}

func TestMeshDisabledWithWiredBackhaul(t *testing.T) {
	roles := config.RoleOverrides{Mesh: config.RoleSelection{Mode: config.RoleDisabled}, AP: config.RoleSelection{Mode: config.RoleAuto}}
	p := MakePlan(Input{Radios: radios(strong), MeshBand: "5g", APEnabled: true, Roles: roles})
	if p.Mesh != nil || p.AP == nil || p.AP.Phy != "phy0" {
		t.Errorf("mesh disabled: %v", p.Roles)
	}
}

func hasNotice(p *Plan, code string) bool {
	for _, n := range p.Notices {
		if n.Code == code {
			return true
		}
	}
	return false
}

func TestInUseRadioLeftAloneUnlessPinned(t *testing.T) {
	// A Raspberry Pi: onboard brcmfmac (AP-capable, connected to the
	// operator's Wi-Fi) plus a USB mesh adapter.
	onboard := spec{phy: "phy7", mac: "b8:27:eb:00:00:01", port: "sdio:mmc1:0001:1", bus: "sdio",
		mesh: false, ap: true, bands: []string{"2g"}, power: map[string]float64{"2g": 18}}
	rs := radios(onboard, strong)
	rs[0].InUse = `wlan0 is connected to Wi-Fi network "home"`
	p := MakePlan(Input{Radios: rs, MeshBand: "5g", APEnabled: true, Roles: auto()})
	if p.Mesh == nil || p.Mesh.Phy != strong.phy {
		t.Fatalf("USB adapter should carry the mesh: %v", p.Roles)
	}
	if p.AP != nil && p.AP.Phy == "phy7" {
		t.Fatalf("connected onboard radio must not be taken for the AP: %v", p.Roles)
	}
	if !hasNotice(p, "radio-in-use") {
		t.Errorf("expected a radio-in-use notice: %+v", p.Notices)
	}
	// Explicit pin takes it over.
	pin := config.RoleOverrides{Mesh: config.RoleSelection{Mode: config.RoleAuto}, AP: config.RoleSelection{Mode: "port", Port: "sdio:mmc1:0001:1"}}
	p = MakePlan(Input{Radios: rs, MeshBand: "5g", APEnabled: true, Roles: pin})
	if p.AP == nil || p.AP.Phy != "phy7" {
		t.Errorf("pinned AP should take the in-use radio: %v %+v", p.Roles, p.Notices)
	}
}

func TestDetectInUse(t *testing.T) {
	noAddr := func(int) string { return "" }
	withAddr := func(i int) string {
		if i == 3 {
			return "192.168.1.20"
		}
		return ""
	}
	sta := nl80211.Interface{Ifindex: 3, Name: "wlan0", Type: nl80211.NL80211_IFTYPE_STATION}
	if got := DetectInUse([]nl80211.Interface{sta}, noAddr); got != "" {
		t.Errorf("idle station is not in use: %q", got)
	}
	if got := DetectInUse([]nl80211.Interface{sta}, withAddr); got == "" {
		t.Error("station with an address is in use")
	}
	sta.SSID = "home"
	if got := DetectInUse([]nl80211.Interface{sta}, noAddr); got == "" {
		t.Error("associated station is in use")
	}
	ours := nl80211.Interface{Ifindex: 3, Name: "mesh0", Type: nl80211.NL80211_IFTYPE_MESH_POINT, SSID: "x"}
	if got := DetectInUse([]nl80211.Interface{ours}, withAddr); got != "" {
		t.Errorf("Trellis interfaces are never 'in use by the system': %q", got)
	}
}

// A tested adapter wins the mesh role over an untested one whose driver
// claims more power (an rtw_8821cu reporting 30 dBm must not beat a verified
// MT7610U at 14 dBm).
func TestMeshPrefersTestedRadio(t *testing.T) {
	tested := spec{phy: "phy1", mac: "a0:b0:c0:00:02:26", port: "usb:1-1", bus: "usb", speed: 480,
		mesh: true, ap: true, both: true, bands: []string{"2g", "5g"}, power: map[string]float64{"2g": 14}, streams: 1}
	untested := spec{phy: "phy0", mac: "cc:64:1a:eb:81:64", port: "usb:2-1", bus: "usb", speed: 480,
		mesh: true, ap: true, bands: []string{"2g", "5g"}, power: map[string]float64{"2g": 30}, streams: 1}
	rs := radios(tested, untested)
	rs[0].HWDB = &hwdb.Entry{MeshPoint: hwdb.Verified}
	rs[1].HWDB = &hwdb.Entry{MeshPoint: hwdb.Unknown}
	p := MakePlan(Input{Radios: rs, MeshBand: "2g", APEnabled: true, Roles: auto()})
	if p.Mesh == nil || p.Mesh.Phy != "phy1" {
		t.Fatalf("mesh radio = %v, want the tested phy1", p.Mesh)
	}
}

// A dual-band-concurrent card (an MT7916): two wiphys on
// one PCI address, the second with a derived, locally administered MAC.
var (
	dbdcG = spec{phy: "phy0", mac: "02:0c:43:26:60:00", port: "pci:0000:01:00.0", bus: "pci", id: "14c3:7906",
		mesh: true, ap: true, both: true, bands: []string{"2g"}, power: map[string]float64{"2g": 21}, streams: 2}
	dbdcA = spec{phy: "phy1", mac: "02:0c:43:26:60:01", port: "pci:0000:01:00.0", bus: "pci", id: "14c3:7906",
		mesh: true, ap: true, both: true, bands: []string{"5g", "6g"}, power: map[string]float64{"5g": 20, "6g": 18}, streams: 2}
)

func dbdcRadios(order ...spec) []*Radio {
	rs := []*Radio{}
	for _, s := range order {
		r := mkRadio(s)
		if s.phy == "phy1" {
			r.PhyIndex = 1
		}
		rs = append(rs, r)
	}
	AssignFingerprints(rs)
	return rs
}

func TestFingerprintDualBandCard(t *testing.T) {
	rs := dbdcRadios(dbdcG, dbdcA)
	g, a := rs[0].Fingerprint, rs[1].Fingerprint
	if g == a {
		t.Fatalf("both radios of one card got fingerprint %q", g)
	}
	if g != "pci:0000:01:00.0#2g" || a != "pci:0000:01:00.0#5g+6g" {
		t.Errorf("fingerprints %q and %q; want the PCI address plus each radio's bands", g, a)
	}
	// Probe order (phy numbering) must not matter.
	rs2 := dbdcRadios(dbdcA, dbdcG)
	for _, r := range rs2 {
		want := map[string]string{"2g": g, "5g": a}[r.Caps.Bands[0]]
		if r.Fingerprint != want {
			t.Errorf("probe order changed %v's fingerprint to %q", r.Caps.Bands, r.Fingerprint)
		}
	}
	// Two radios with the same bands on one device still differ.
	twin := dbdcA
	twin.phy, twin.mac = "phy2", "02:0c:43:26:60:02"
	rs3 := dbdcRadios(dbdcA, twin)
	if rs3[0].Fingerprint == rs3[1].Fingerprint {
		t.Errorf("identical-band radios on one device collide: %q", rs3[0].Fingerprint)
	}
	// A lone PCI radio keeps its plain fingerprint (nothing changes on
	// existing nodes).
	if one := dbdcRadios(dbdcA); one[0].Fingerprint != "pci:0000:01:00.0" {
		t.Errorf("single radio fingerprint changed to %q", one[0].Fingerprint)
	}
}

func TestPlanDualBandCard(t *testing.T) {
	p := MakePlan(Input{Radios: dbdcRadios(dbdcG, dbdcA), MeshBand: "5g", APEnabled: true, Roles: auto()})
	if p.Mesh == nil || p.Mesh.Phy != "phy1" {
		t.Fatalf("mesh should be the card's 5 GHz radio, got %+v", p.Mesh)
	}
	if p.AP == nil || p.AP.Phy != "phy0" || p.APBand != "2g" {
		t.Fatalf("AP should be the card's 2.4 GHz radio on 2g, got %+v band %s", p.AP, p.APBand)
	}
	for _, n := range p.Notices {
		if n.Code == "ap-shares-mesh-radio" {
			t.Errorf("one card, two radios: the AP must not share the mesh radio (%s)", n.Message)
		}
	}
}

// A dual-band-concurrent card (two radios on one card) plus a HaLow radio.
var halow = spec{phy: "phy2", mac: "0c:bf:74:00:00:01", port: "spi:spi0.0", bus: "spi",
	mesh: true, ap: false, bands: []string{"s1g"}, power: map[string]float64{"s1g": 21}, streams: 1}

func TestPlanDualBackhaulReferenceBuild(t *testing.T) {
	rs := dbdcRadios(dbdcG, dbdcA)
	h := mkRadio(halow)
	h.PhyIndex = 2
	rs = append(rs, h)
	AssignFingerprints(rs)
	p := MakePlan(Input{Radios: rs, MeshBand: "5g", MoreBands: []string{"s1g"}, APEnabled: true, Roles: auto()})
	if p.Mesh == nil || p.Mesh.Phy != "phy1" {
		t.Fatalf("mesh0 should be the MT7916's 5 GHz radio, got %+v", p.Mesh)
	}
	if p.Link(1) == nil || p.Link(1).Phy != "phy2" {
		t.Fatalf("mesh1 should be the HaLow radio, got %+v", p.Link(1))
	}
	if p.AP == nil || p.AP.Phy != "phy0" || p.APBand != "2g" {
		t.Fatalf("AP should be the MT7916's 2.4 GHz radio, got %+v", p.AP)
	}
	if p.Roles[p.LinkFingerprint(1)] != Role("mesh2") || hasNotice(p, "ap-shares-mesh-radio") || hasNotice(p, "radios-unused") {
		t.Errorf("roles %v notices %v", p.Roles, p.Notices)
	}
}

// A 2.4 GHz profile, a dual-band card carrying the mesh and the AP, and a
// HaLow module fitted. Morse Micro's driver
// registers its S1G channels as 5 GHz ones, with AP mode and a low power
// figure, which would win the AP role. The hardware database's band list
// keeps it out of every 2.4/5 GHz role.
func TestHaLowRadioTakesNoWiFiRole(t *testing.T) {
	db, err := hwdb.Load()
	if err != nil {
		t.Fatal(err)
	}
	asDriver := halow
	asDriver.ap, asDriver.both = true, true
	asDriver.bands = []string{"5g"}
	asDriver.power = map[string]float64{"5g": 10}

	plan := func(h *Radio) *Plan {
		rs := dbdcRadios(dbdcG, dbdcA)
		h.PhyIndex = 2
		rs = append(rs, h)
		AssignFingerprints(rs)
		return MakePlan(Input{Radios: rs, MeshBand: "2g", APEnabled: true, Roles: auto()})
	}

	// Without the database entry the planner is fooled: this is the hazard.
	if p := plan(mkRadio(asDriver)); p.AP == nil || p.AP.Phy != "phy2" {
		t.Fatalf("expected the mislabelled radio to win the AP role without the database, got %+v", p.AP)
	}

	h := mkRadio(asDriver)
	h.Device.Driver = "morse_spi"
	h.HWDB = db.Lookup(h.Device.Bus, h.Device.DeviceID(), h.Device.Driver)
	if h.HWDB == nil || h.HWDB.ID != "mm6108" {
		t.Fatalf("hardware database lookup: %+v", h.HWDB)
	}
	h.Caps.Bands = realBands(h.Caps.Bands, h.HWDB)
	if len(h.Caps.Bands) != 1 || h.Caps.Bands[0] != "s1g" {
		t.Fatalf("bands = %v, want [s1g]", h.Caps.Bands)
	}
	p := plan(h)
	if p.Mesh == nil || p.Mesh.Phy != "phy0" {
		t.Errorf("mesh should stay on the MT7916's 2.4 GHz radio, got %+v", p.Mesh)
	}
	if p.AP == nil || p.AP.Phy != "phy1" {
		t.Errorf("AP should stay on the MT7916's 5 GHz radio, got %+v", p.AP)
	}
	if role := p.Roles[h.Fingerprint]; role != RoleUnused {
		t.Errorf("HaLow radio role = %q, want unused", role)
	}
	if got := realBands([]string{"2g", "5g"}, nil); len(got) != 2 {
		t.Errorf("no database entry must keep the driver's bands, got %v", got)
	}
}

func TestPlanDualBackhaulTwoRadios(t *testing.T) {
	// strong: 2g+5g mesh+AP capable; weak: 2g only. Links on 5g and 2g take
	// both radios; the AP shares the first link's radio.
	p := MakePlan(Input{Radios: radios(strong, weak), MeshBand: "5g", MoreBands: []string{"2g"}, APEnabled: true, Roles: auto()})
	if p.Mesh == nil || p.Mesh.Phy != "phy0" || p.Link(1) == nil || p.Link(1).Phy != "phy1" {
		t.Fatalf("links: mesh %+v mesh2 %+v", p.Mesh, p.Link(1))
	}
	if p.AP != p.Mesh || p.APLink != 0 || p.Roles[p.Mesh.Fingerprint] != RoleMeshAP || !hasNotice(p, "ap-shares-mesh-radio") {
		t.Fatalf("AP should share the first link's radio: AP %+v roles %v", p.AP, p.Roles)
	}
}

// The profile's link order must not cost a node a link. With a dual-band
// radio and a 2.4 GHz-only one, 2.4 GHz listed first would hand the
// dual-band radio (the stronger one) to the 2.4 GHz link and leave nothing
// for 5 GHz, if each link simply took its best radio in turn.
func TestPlanLinksInAnyOrder(t *testing.T) {
	p := MakePlan(Input{Radios: radios(strong, weak), MeshBand: "2g", MoreBands: []string{"5g"}, APEnabled: false, Roles: auto()})
	if p.Link(0) == nil || p.Link(0).Phy != "phy1" || p.Link(1) == nil || p.Link(1).Phy != "phy0" {
		t.Fatalf("2g then 5g: link 1 on %+v, link 2 on %+v; want the 2.4 GHz-only radio and the dual-band one", p.Link(0), p.Link(1))
	}
	if hasNotice(p, "no-mesh-radio") || hasNotice(p, "no-mesh2-radio") {
		t.Errorf("both links have radios, yet: %v", p.Notices)
	}
	// When either radio would do, the first link still gets the better one.
	both := weak
	both.bands, both.power = []string{"2g", "5g"}, map[string]float64{"2g": 17, "5g": 15}
	p = MakePlan(Input{Radios: radios(strong, both), MeshBand: "2g", MoreBands: []string{"5g"}, APEnabled: false, Roles: auto()})
	if p.Link(0).Phy != "phy0" || p.Link(1).Phy != "phy1" {
		t.Errorf("two dual-band radios: link 1 on %s, link 2 on %s; want the stronger radio on the first link", p.Link(0).Phy, p.Link(1).Phy)
	}
}

// A dual-band-concurrent card with one link per radio: 2.4 GHz and 5 GHz on the
// MT7916's two radios, HaLow on the third. The AP has no radio of its own
// and shares the first link's; never the HaLow radio, although Morse's
// driver advertises AP mode and both at once.
func TestPlanThreeLinksReferenceBuild(t *testing.T) {
	asDriver := halow
	asDriver.ap, asDriver.both = true, true
	build := func() []*Radio {
		rs := dbdcRadios(dbdcG, dbdcA)
		h := mkRadio(asDriver)
		h.PhyIndex = 2
		rs = append(rs, h)
		AssignFingerprints(rs)
		return rs
	}
	p := MakePlan(Input{Radios: build(), MeshBand: "2g", MoreBands: []string{"5g", "s1g"}, APEnabled: true, Roles: auto()})
	for i, phy := range []string{"phy0", "phy1", "phy2"} {
		if p.Link(i) == nil || p.Link(i).Phy != phy {
			t.Fatalf("link %d on %+v, want %s", i+1, p.Link(i), phy)
		}
	}
	if got := p.Roles[p.LinkFingerprint(2)]; got != "mesh3" || got.Link() != 2 || got.HasAP() {
		t.Errorf("HaLow radio role = %q", got)
	}
	if p.AP != p.Link(0) || p.APLink != 0 || p.Roles[p.LinkFingerprint(0)] != RoleMeshAP || !hasNotice(p, "ap-shares-mesh-radio") {
		t.Errorf("AP should share the first link's radio: AP %+v link %d roles %v", p.AP, p.APLink, p.Roles)
	}
	// HaLow first, and the Wi-Fi radios unable to share: still no AP on HaLow.
	g, a := dbdcG, dbdcA
	g.both, a.both = false, false
	rs := dbdcRadios(g, a)
	h := mkRadio(asDriver)
	h.PhyIndex = 2
	rs = append(rs, h)
	AssignFingerprints(rs)
	p = MakePlan(Input{Radios: rs, MeshBand: "s1g", MoreBands: []string{"2g", "5g"}, APEnabled: true, Roles: auto()})
	if p.AP != nil || !hasNotice(p, "no-ap-radio") {
		t.Errorf("no radio can share the AP, got AP %+v notices %v", p.AP, p.Notices)
	}
	// 6 GHz and 5 GHz are one radio on this card: a fourth link finds none.
	p = MakePlan(Input{Radios: build(), MeshBand: "2g", MoreBands: []string{"5g", "s1g", "6g"}, APEnabled: false, Roles: auto()})
	if p.Link(3) != nil || !hasNotice(p, "no-mesh4-radio") || len(p.Links) != 4 {
		t.Errorf("a fourth link with no radio left: %+v notices %v", p.Link(3), p.Notices)
	}
}

func TestPlanDualBackhaulMissingRadio(t *testing.T) {
	p := MakePlan(Input{Radios: radios(strong), MeshBand: "5g", MoreBands: []string{"2g"}, APEnabled: true, Roles: auto()})
	if p.Mesh == nil || p.Link(1) != nil || !hasNotice(p, "no-mesh2-radio") {
		t.Fatalf("one radio: mesh %+v mesh2 %+v notices %v", p.Mesh, p.Link(1), p.Notices)
	}
	// No second link asked for: no notice about it.
	p = MakePlan(Input{Radios: radios(strong), MeshBand: "5g", APEnabled: true, Roles: auto()})
	if hasNotice(p, "no-mesh2-radio") || p.Link(1) != nil || len(p.Links) != 1 {
		t.Fatalf("single-link profile mentions a second link: %v", p.Notices)
	}
}

// A pinned link gets its radio before any automatic link chooses.
func TestPlanLinkPins(t *testing.T) {
	rs := radios(strong, weak)
	roles := auto()
	roles.SetLink(1, config.RoleSelection{Mode: "radio", Radio: rs[0].Fingerprint})
	p := MakePlan(Input{Radios: rs, MeshBand: "5g", MoreBands: []string{"2g"}, APEnabled: true, Roles: roles})
	if p.Link(1) != rs[0] {
		t.Fatalf("mesh2 pinned to %s is on %+v", rs[0].Phy, p.Link(1))
	}
	// The other radio has no 5 GHz: the pin left the first link without one.
	if p.Mesh != nil || !hasNotice(p, "no-mesh-radio") {
		t.Errorf("first link: %+v notices %v", p.Mesh, p.Notices)
	}
	// Two links pinned to one radio: the first named keeps it.
	roles.SetLink(0, config.RoleSelection{Mode: "radio", Radio: rs[0].Fingerprint})
	p = MakePlan(Input{Radios: rs, MeshBand: "5g", MoreBands: []string{"2g"}, APEnabled: true, Roles: roles})
	if p.Mesh != rs[0] || p.Link(1) != nil || !hasNotice(p, "mesh2-pin-conflict") {
		t.Errorf("two pins on one radio: mesh %+v mesh2 %+v notices %v", p.Mesh, p.Link(1), p.Notices)
	}
	// A disabled link takes nothing and says so.
	roles = auto()
	roles.SetLink(1, config.RoleSelection{Mode: config.RoleDisabled})
	p = MakePlan(Input{Radios: rs, MeshBand: "5g", MoreBands: []string{"2g"}, APEnabled: true, Roles: roles})
	if p.Link(1) != nil || !hasNotice(p, "mesh2-disabled") || hasNotice(p, "no-mesh2-radio") {
		t.Errorf("disabled second link: %+v notices %v", p.Link(1), p.Notices)
	}
}

// Roles do not move while the node runs: a link added to the profile takes a
// free radio, and the radios already carrying links keep them.
func TestPlanNewLinkLeavesRunningLinksAlone(t *testing.T) {
	rs := radios(strong, mid)
	// Running: the first link (2.4 GHz) on the weaker radio, by an earlier choice.
	cur := map[string]Role{rs[0].Fingerprint: RoleUnused, rs[1].Fingerprint: RoleMesh}
	p := MakePlan(Input{Radios: rs, MeshBand: "2g", MoreBands: []string{"5g"}, APEnabled: false, Roles: auto(), Current: cur})
	if p.Mesh != rs[1] || p.Link(1) != rs[0] {
		t.Fatalf("with link 1 running on %s: link 1 on %+v, link 2 on %+v", rs[1].Phy, p.Mesh, p.Link(1))
	}
}

func TestRoleNames(t *testing.T) {
	for _, c := range []struct {
		role Role
		link int
		ap   bool
	}{{RoleMesh, 0, false}, {RoleMeshAP, 0, true}, {Role("mesh2"), 1, false}, {Role("mesh2+ap"), 1, true}, {"mesh3", 2, false}, {"mesh10+ap", 9, true},
		{RoleAP, -1, true}, {RoleUnused, -1, false}, {"mesh1", -1, false}, {"mesh0", -1, false}, {"mesh02", -1, false}, {"meshx", -1, false}} {
		if c.role.Link() != c.link || c.role.HasAP() != c.ap {
			t.Errorf("%q: link %d ap %v, want %d %v", c.role, c.role.Link(), c.role.HasAP(), c.link, c.ap)
		}
	}
	if LinkRole(0, false) != RoleMesh || LinkRole(1, true) != Role("mesh2+ap") || LinkRole(3, false) != "mesh4" {
		t.Error("LinkRole names")
	}
}

func noticeLevel(p *Plan, code string) string {
	for _, n := range p.Notices {
		if n.Code == code {
			return n.Level
		}
	}
	return ""
}

// A node is in the mesh over whichever links it has radios for. Carrying a
// later link without the first is an ordinary state, not a fault: a 2.4 GHz
// adapter in a mesh whose first link is 5 GHz, or a node's only radio pinned
// to the second link.
func TestPlanLinkWithoutTheFirst(t *testing.T) {
	// One dual-band radio, running the first link (2.4 GHz) and the AP; the
	// operator pins it to the second link (5 GHz).
	rs := radios(strong)
	fp := rs[0].Fingerprint
	roles := auto()
	roles.SetLink(1, config.RoleSelection{Mode: "radio", Radio: fp})
	p := MakePlan(Input{Radios: rs, MeshBand: "2g", MoreBands: []string{"5g"}, APEnabled: true, Roles: roles,
		Current: map[string]Role{fp: RoleMeshAP}})
	if p.Mesh != nil || p.Link(1) != rs[0] {
		t.Fatalf("pinned to the second link: first %+v second %+v", p.Mesh, p.Link(1))
	}
	if got := noticeLevel(p, "no-mesh-radio"); got != "info" {
		t.Errorf("first link without a radio while the second runs: level %q, want info (notices %v)", got, p.Notices)
	}
	if p.AP != rs[0] || p.APLink != 1 || p.Roles[fp] != Role("mesh2+ap") || !hasNotice(p, "ap-shares-mesh-radio") {
		t.Errorf("the AP should share the second link's radio: AP %+v link %d roles %v notices %v", p.AP, p.APLink, p.Roles, p.Notices)
	}

	// A 2.4 GHz-only adapter where the profile lists 5 GHz first.
	rs = radios(weak)
	p = MakePlan(Input{Radios: rs, MeshBand: "5g", MoreBands: []string{"2g"}, APEnabled: true, Roles: auto()})
	if p.Mesh != nil || p.Link(1) != rs[0] || noticeLevel(p, "no-mesh-radio") != "info" {
		t.Errorf("2.4 GHz-only radio: first %+v second %+v notices %v", p.Mesh, p.Link(1), p.Notices)
	}
	if p.AP != rs[0] || p.APLink != 1 {
		t.Errorf("the AP should share the only radio: AP %+v link %d", p.AP, p.APLink)
	}
	for _, n := range p.Notices {
		if n.Level == "error" {
			t.Errorf("a node with a working link reports a fault: %+v", n)
		}
	}

	// The same radio with the first link only: the missing second link is
	// information too.
	p = MakePlan(Input{Radios: rs, MeshBand: "2g", MoreBands: []string{"5g"}, APEnabled: true, Roles: auto()})
	if p.Mesh != rs[0] || noticeLevel(p, "no-mesh2-radio") != "info" {
		t.Errorf("first link only: %+v notices %v", p.Mesh, p.Notices)
	}

	// No radio for any link is still the fault it was.
	p = MakePlan(Input{Radios: rs, MeshBand: "5g", MoreBands: []string{"s1g"}, APEnabled: true, Roles: auto()})
	if p.Mesh != nil || p.Link(1) != nil || noticeLevel(p, "no-mesh-radio") != "error" || noticeLevel(p, "no-mesh2-radio") != "warning" {
		t.Errorf("no link at all: notices %v", p.Notices)
	}
	if p.AP != rs[0] || p.APLink != -1 {
		t.Errorf("with no mesh link the radio is free for the AP: AP %+v link %d", p.AP, p.APLink)
	}
}

// A dual-band card is two radios in one port. A role pinned to "whatever is
// in that port" means the radio of the card that can carry it: the 5 GHz
// one for a 5 GHz link, not the first one found.
func TestPortPinOnDualBandCard(t *testing.T) {
	port := config.RoleSelection{Mode: "port", Port: "pci:0000:01:00.0"}
	roles := auto()
	roles.More = []config.RoleSelection{port}
	p := MakePlan(Input{Radios: dbdcRadios(dbdcG, dbdcA), MeshBand: "2g", MoreBands: []string{"5g"}, APEnabled: false, Roles: roles})
	if p.Link(0) == nil || p.Link(0).Phy != "phy0" || p.Link(1) == nil || p.Link(1).Phy != "phy1" {
		t.Fatalf("link 2 pinned to the card's port: links on %+v and %+v, want phy0 and phy1; notices %v", p.Link(0), p.Link(1), p.Notices)
	}
	// Both links pinned to the port: each gets its band's radio, in either order.
	for _, rs := range [][]*Radio{dbdcRadios(dbdcG, dbdcA), dbdcRadios(dbdcA, dbdcG)} {
		roles := config.RoleOverrides{Mesh: port, More: []config.RoleSelection{port}, AP: config.RoleSelection{Mode: config.RoleAuto}}
		p = MakePlan(Input{Radios: rs, MeshBand: "2g", MoreBands: []string{"5g"}, APEnabled: false, Roles: roles})
		if p.Link(0) == nil || p.Link(0).Phy != "phy0" || p.Link(1) == nil || p.Link(1).Phy != "phy1" {
			t.Errorf("both links pinned to the port: %+v and %+v; notices %v", p.Link(0), p.Link(1), p.Notices)
		}
	}
	// The AP pinned to the port takes the radio no link has.
	roles = auto()
	roles.AP = port
	p = MakePlan(Input{Radios: dbdcRadios(dbdcG, dbdcA), MeshBand: "5g", APEnabled: true, Roles: roles})
	if p.Link(0) == nil || p.Link(0).Phy != "phy1" || p.AP == nil || p.AP.Phy != "phy0" || p.APLink != -1 {
		t.Errorf("AP pinned to the port: mesh %+v AP %+v link %d; notices %v", p.Link(0), p.AP, p.APLink, p.Notices)
	}
	// A port with nothing in it is still reported.
	roles = auto()
	roles.Mesh = config.RoleSelection{Mode: "port", Port: "usb:9-9"}
	if p = MakePlan(Input{Radios: dbdcRadios(dbdcG, dbdcA), MeshBand: "2g", APEnabled: false, Roles: roles}); !hasNotice(p, "mesh-pin-missing") {
		t.Errorf("empty port: notices %v", p.Notices)
	}
}

// Notices name a link by its band and channel when the input names the
// links, as the UI does, and keep the old wording otherwise.
func TestPlanNoticesNameLinks(t *testing.T) {
	rs := dbdcRadios(dbdcG, dbdcA)
	AssignFingerprints(rs)
	in := Input{Radios: rs, MeshBand: "5g", MoreBands: []string{"s1g"}, APEnabled: true, Roles: auto()}
	text := func(p *Plan) string {
		var all []string
		for _, n := range p.Notices {
			all = append(all, n.Message)
		}
		return strings.Join(all, "\n")
	}
	in.LinkNames = []string{"5 GHz · ch 149", "HaLow · ch 26"}
	if got := text(MakePlan(in)); !strings.Contains(got, "no radio for the HaLow · ch 26 link: this node meshes over the 5 GHz · ch 149 link") {
		t.Errorf("named links:\n%s", got)
	}
	in.LinkNames = nil
	if got := text(MakePlan(in)); !strings.Contains(got, "no radio for mesh link 2 (s1g)") {
		t.Errorf("unnamed links:\n%s", got)
	}
}
