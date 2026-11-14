package config

import (
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const goodProfile = `
schema: 0
name: alpha
sae_passphrase: correct-horse-battery
regulatory:
  mode: country
  country: us
radio:
  band: 5g
  channel: 149
`

const goodNode = `
schema: 0
callsign: relay-1
node_id: 7
ports:
  - name: eth0
    role: auto
  - name: eth1
    role: mesh
radios:
  roles:
    mesh: auto
    ap: {port: "usb:1-1.2"}
  devices:
    - match: {fingerprint: "mac:00:11:22:33:44:55"}
      label: Alfa
      max_power_dbm: 17
`

func TestParseProfileDefaults(t *testing.T) {
	p, err := ParseProfile([]byte(goodProfile))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.MeshID != "alpha" {
		t.Errorf("mesh_id default = %q, want name", p.MeshID)
	}
	if p.Regulatory.Country != "US" {
		t.Errorf("country not upper-cased: %q", p.Regulatory.Country)
	}
	if p.Prefix != "10.73.0.0/16" || p.Batman.OrigIntervalMS != 1000 || p.Batman.PinnedVersion != "2026.3" || p.Radio.Width != 20 {
		t.Errorf("defaults not applied: %+v", p)
	}
}

func TestParseNode(t *testing.T) {
	n, err := ParseNode([]byte(goodNode))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n.Radios.Roles.AP.Mode != "port" || n.Radios.Roles.AP.Port != "usb:1-1.2" {
		t.Errorf("ap role selection = %+v", n.Radios.Roles.AP)
	}
	if n.Radios.Roles.Mesh.Mode != RoleAuto {
		t.Errorf("mesh role = %+v", n.Radios.Roles.Mesh)
	}
	if got := n.MeshPorts(); len(got) != 1 || got[0] != "eth1" {
		t.Errorf("MeshPorts = %v", got)
	}
	if !n.APEnabled() || n.AP.Country != "" || n.AP.Band != Band5G || n.MeshMTU != 1560 {
		t.Errorf("defaults not applied: %+v", n)
	}
	if d := n.Radios.Devices[0]; d.Label != "Alfa" || d.MaxPowerDBm == nil || *d.MaxPowerDBm != 17 {
		t.Errorf("device rule = %+v", d)
	}
}

func fieldErrors(t *testing.T, err error) map[string]string {
	t.Helper()
	var es Errors
	if !errors.As(err, &es) {
		t.Fatalf("expected Errors, got %T: %v", err, err)
	}
	m := map[string]string{}
	for _, e := range es {
		m[e.Field] = e.Msg
	}
	return m
}

func TestProfileErrorsNameFields(t *testing.T) {
	_, err := ParseProfile([]byte(`
schema: 1
name: ""
sae_passphrase: short
regulatory: {mode: lenient, country: USA}
radio: {band: 5g, channel: 197, width: 320}
prefix: 10.73.1.0/24
batman: {routing_algo: BATMAN_IV, orig_interval_ms: 50, min_version: new}
`))
	if err == nil {
		t.Fatal("expected error")
	}
	m := fieldErrors(t, err)
	for _, f := range []string{"schema", "name", "sae_passphrase", "regulatory.mode", "regulatory.country",
		"radio.channel", "radio.width", "prefix", "batman.routing_algo", "batman.orig_interval_ms", "batman.min_version"} {
		if _, ok := m[f]; !ok {
			t.Errorf("missing error for %s; got %v", f, m)
		}
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	_, err := ParseProfile([]byte(goodProfile + "\nsae_pasphrase: typo\n"))
	if err == nil {
		t.Fatal("expected error")
	}
	m := fieldErrors(t, err)
	if msg, ok := m["sae_pasphrase"]; !ok || !strings.Contains(msg, "unknown field") {
		t.Errorf("unknown field not reported by name: %v", m)
	}
}

func TestNodeErrorsNameFields(t *testing.T) {
	_, err := ParseNode([]byte(`
schema: 0
callsign: "Bad Callsign"
node_id: 300
ports:
  - name: eth0
    role: uplink
  - name: eth0
radios:
  roles:
    mesh: sometimes
  devices:
    - match: {}
      max_power_dbm: 99
ap: {country: U, band: 6g, passphrase: short}
mesh_mtu: 100
`))
	if err == nil {
		t.Fatal("expected error")
	}
	m := fieldErrors(t, err)
	for _, f := range []string{"callsign", "node_id", "ports[0].role", "ports[1].name", "radios.roles.mesh",
		"radios.devices[0].match", "radios.devices[0].max_power_dbm", "ap.country", "ap.band", "ap.passphrase", "mesh_mtu"} {
		if _, ok := m[f]; !ok {
			t.Errorf("missing error for %s; got %v", f, m)
		}
	}
}

func TestRoleSelectionBothKeys(t *testing.T) {
	_, err := ParseNode([]byte("schema: 0\ncallsign: a\nradios:\n  roles:\n    mesh: {radio: x, port: y}\n"))
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Errorf("expected 'not both' error, got %v", err)
	}
}

func TestChannelFrequency(t *testing.T) {
	cases := []struct {
		band string
		ch   int
		mhz  int
		ok   bool
	}{
		{Band2G, 1, 2412, true}, {Band2G, 11, 2462, true}, {Band2G, 14, 2484, true}, {Band2G, 15, 0, false},
		{Band2G, 0, 0, false},
		// Any number the kernel maps: whether a radio has it is the radio's to say.
		{Band5G, 36, 5180, true}, {Band5G, 149, 5745, true}, {Band5G, 165, 5825, true}, {Band5G, 150, 5750, true},
		{Band5G, 34, 5170, true}, {Band5G, 177, 5885, true}, {Band5G, 184, 4920, true}, {Band5G, 196, 4980, true},
		{Band5G, 0, 0, false}, {Band5G, 197, 0, false},
		{Band6G, 1, 5955, true}, {Band6G, 2, 5935, true}, {Band6G, 233, 7115, true}, {Band6G, 234, 0, false}, {Band6G, 0, 0, false},
	}
	for _, c := range cases {
		got, err := ChannelFrequencyMHz(c.band, c.ch)
		if (err == nil) != c.ok || got != c.mhz {
			t.Errorf("ChannelFrequencyMHz(%s,%d) = %d,%v; want %d ok=%v", c.band, c.ch, got, err, c.mhz, c.ok)
		}
	}
}

func TestRedacted(t *testing.T) {
	p, _ := ParseProfile([]byte(goodProfile))
	if p.Redacted().SAEPassphrase != "<redacted>" || p.SAEPassphrase == "<redacted>" {
		t.Error("Redacted must blank the copy only")
	}
}

func TestGatewayConfig(t *testing.T) {
	n, err := ParseNode([]byte("schema: 0\ncallsign: gw-1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if n.Gateway.Mode != GatewayAuto || len(n.Gateway.ProbeTargets) != 3 || n.Gateway.ProbeIntervalMS != 1000 {
		t.Errorf("gateway defaults: %+v", n.Gateway)
	}
	_, err = ParseNode([]byte("schema: 0\ncallsign: gw-1\ngateway: {mode: sometimes, probe_targets: [example.com], probe_interval_ms: 5}\n"))
	if err == nil {
		t.Fatal("bad gateway config accepted")
	}
	for _, want := range []string{"gateway.mode", "gateway.probe_targets[0]", "gateway.probe_interval_ms"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %s: %v", want, err)
		}
	}
}

func TestProfileSecondMeshLink(t *testing.T) {
	p, err := ParseProfile([]byte(goodProfile))
	if err != nil || len(p.Links) != 0 {
		t.Fatalf("a profile without radio2 has a second link: %+v %v", p.Links, err)
	}
	p, err = ParseProfile([]byte(goodProfile + "\nradio2: {band: 2g, channel: 6}\n"))
	if err != nil {
		t.Fatalf("valid radio2 rejected: %v", err)
	}
	if len(p.Links) != 1 || p.Links[0] != (RadioPlan{Band: "2g", Channel: 6, Width: 20}) || p.LegacyRadio2 != nil {
		t.Fatalf("links = %+v; want radio2 as the one more link, 2g ch 6, default width 20", p.Links)
	}
	for name, extra := range map[string]string{
		"same channel as radio": "\nradio2: {band: " + p.Radio.Band + ", channel: " + strconv.Itoa(p.Radio.Channel) + "}\n",
		"bad channel":           "\nradio2: {band: 5g, channel: 200}\n",
		"bad band":              "\nradio2: {band: 3g, channel: 1}\n",
		"2.4 GHz at 80 MHz":     "\nradio2: {band: 2g, channel: 6, width: 80}\n",
	} {
		_, err := ParseProfile([]byte(goodProfile + extra))
		if err == nil || !strings.Contains(err.Error(), "radio2") {
			t.Errorf("%s: want an error naming radio2, got %v", name, err)
		}
	}
}

// The shipped examples must stay valid, including with their optional
// sections switched on.
func TestExampleConfigs(t *testing.T) {
	prof, err := os.ReadFile("../../configs/examples/profile.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseProfile(prof); err != nil {
		t.Errorf("configs/examples/profile.yaml: %v", err)
	}
	// Uncomment the example links block.
	on := strings.NewReplacer("# links:", "links:", "#   - {", "  - {").Replace(string(prof))
	p, err := ParseProfile([]byte(on))
	if err != nil || len(p.Links) != 2 || p.Links[1].Band != BandS1G {
		t.Errorf("example profile with its links enabled: %+v, %v", p, err)
	}
	node, err := os.ReadFile("../../configs/examples/node.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseNode(node); err != nil {
		t.Errorf("configs/examples/node.yaml: %v", err)
	}
}

func TestProfileHaLowLink(t *testing.T) {
	// Wi-Fi on the first link, HaLow on the second.
	p, err := ParseProfile([]byte(goodProfile + "\nradio2: {band: s1g, channel: 26}\n"))
	if err != nil {
		t.Fatalf("valid HaLow radio2 rejected: %v", err)
	}
	if p.Links[0].Width != 2 {
		t.Errorf("HaLow default width = %d MHz, want 2", p.Links[0].Width)
	}
	if got := LinkFrequencyMHz(p.Links[0], p.Regulatory.Country); got != 915 {
		t.Errorf("channel 26 at 2 MHz = %v MHz, want 915", got)
	}
	if got := LinkFrequencyMHz(p.Radio, p.Regulatory.Country); got != 5745 {
		t.Errorf("Wi-Fi link frequency = %v, want 5745", got)
	}

	// The four US operating classes, as measured on real hardware.
	for _, c := range []struct {
		width, ch, class int
		mhz              float64
	}{{1, 27, 68, 915.5}, {2, 26, 69, 915}, {4, 24, 70, 914}, {8, 28, 71, 916}, {1, 1, 68, 902.5}, {1, 51, 68, 927.5}, {8, 44, 71, 924}} {
		got, err := S1GChannelInfo("US", c.width, c.ch)
		if err != nil || got.OpClass != c.class || got.FreqMHz() != c.mhz {
			t.Errorf("US %d MHz channel %d = %+v (%v MHz), %v; want class %d at %v MHz", c.width, c.ch, got, got.FreqMHz(), err, c.class, c.mhz)
		}
	}
	if got := S1GChannels("US", 8); len(got) != 3 || got[0] != 12 || got[2] != 44 {
		t.Errorf("US 8 MHz channels = %v, want 12 28 44", got)
	}
	if got := S1GChannels("US", 1); len(got) != 26 {
		t.Errorf("US has %d 1 MHz channels, want 26", len(got))
	}

	// Other regions, against Morse Micro's tables (src/utils/morse.c): the
	// class's start frequency + 500 kHz per channel number.
	for _, c := range []struct {
		country          string
		width, ch, class int
		mhz              float64
	}{
		{"CA", 2, 26, 69, 915},   // the Americas share the US plan
		{"GB", 1, 1, 66, 863.5},  // eu6
		{"DE", 1, 31, 0, 0},      // eu30 is not Germany's
		{"FR", 1, 31, 0, 0},      // eu30 allows France only 33 and 35
		{"FR", 1, 33, 77, 917.9}, // eu30
		{"IN", 1, 3, 0, 0},       // India: only 5, 7, 9 of eu6
		{"IN", 1, 7, 66, 866.5},  //
		{"JP", 2, 4, 64, 924.5},  // jp10 shares class 64 with jp9
		{"JP", 1, 94, 195, 847},  // the 850 MHz transition channels
		{"KR", 1, 11, 74, 922.5}, //
		{"SG", 8, 38, 63, 921},   //
		{"AU", 1, 28, 50, 916},   // REVmf, Morse's default
		{"AU", 4, 59, 48, 923.5}, // au39
		{"CL", 8, 43, 53, 923.5}, // Chile is on Australia's plan
		{"NZ", 8, 44, 71, 924},   //
		{"US", 16, 44, 0, 0},     // no 16 MHz anywhere
		{"US", 2, 54, 0, 0},      // past the band
		{"XX", 2, 26, 0, 0},      // no plan
	} {
		got, err := S1GChannelInfo(c.country, c.width, c.ch)
		if c.class == 0 {
			if err == nil {
				t.Errorf("%s %d MHz channel %d = %+v; want an error", c.country, c.width, c.ch, got)
			}
			continue
		}
		if err != nil || got.OpClass != c.class || got.FreqMHz() != c.mhz {
			t.Errorf("%s %d MHz channel %d = %+v (%v MHz), %v; want class %d at %v MHz", c.country, c.width, c.ch, got, got.FreqMHz(), err, c.class, c.mhz)
		}
	}
	if got := S1GChannels("JP", 2); !slices.Equal(got, []int{2, 4, 6, 8, 95}) {
		t.Errorf("JP 2 MHz channels = %v, want 2 4 6 8 95 (two classes and the 850 MHz one, merged)", got)
	}
	if got := len(S1GCountries()); got < 70 {
		t.Errorf("%d countries with a HaLow plan, want Morse's 70+", got)
	}

	for name, c := range map[string]struct{ extra, field string }{
		"a 1 MHz channel at 2 MHz": {"\nradio2: {band: s1g, channel: 27, width: 2}\n", "radio2.channel"},
		"a 2 MHz channel at 8 MHz": {"\nradio2: {band: s1g, channel: 26, width: 8}\n", "radio2.channel"},
		"channel past the band":    {"\nradio2: {band: s1g, channel: 54, width: 2}\n", "radio2.channel"},
		"a Wi-Fi width":            {"\nradio2: {band: s1g, channel: 26, width: 20}\n", "radio2.width"},
		"no channel":               {"\nradio2: {band: s1g}\n", "radio2.channel"},
	} {
		_, err := ParseProfile([]byte(goodProfile + c.extra))
		if err == nil || !strings.Contains(err.Error(), c.field) {
			t.Errorf("%s: want an error naming %s, got %v", name, c.field, err)
		}
	}

	// HaLow channel plans differ by country: Germany's has 1 MHz channels
	// only, and a country without a plan is named as such.
	other := strings.Replace(goodProfile, "country: us", "country: de", 1)
	if other == goodProfile {
		t.Fatal("test profile has no country: us line")
	}
	if _, err := ParseProfile([]byte(other + "\nradio2: {band: s1g, channel: 26}\n")); err == nil || !strings.Contains(err.Error(), "radio2.width") || !strings.Contains(err.Error(), "DE's HaLow plan") {
		t.Errorf("want a width error for HaLow at 2 MHz in DE, got %v", err)
	}
	if _, err := ParseProfile([]byte(other + "\nradio2: {band: s1g, channel: 3, width: 1}\n")); err != nil {
		t.Errorf("HaLow channel 3 at 1 MHz in DE rejected: %v", err)
	}
	none := strings.Replace(goodProfile, "country: us", "country: cn", 1)
	if _, err := ParseProfile([]byte(none + "\nradio2: {band: s1g, channel: 26}\n")); err == nil || !strings.Contains(err.Error(), "no channel plan for country CN") {
		t.Errorf("want an error about no HaLow plan for CN, got %v", err)
	}
	// HaLow as the only link is a valid profile too.
	halowOnly := strings.Replace(goodProfile, "band: 5g\n  channel: 149", "band: s1g\n  channel: 12\n  width: 8", 1)
	if halowOnly == goodProfile {
		t.Fatal("test profile's radio block has another form")
	}
	if p, err := ParseProfile([]byte(halowOnly)); err != nil || p.Radio.Band != "s1g" {
		t.Errorf("HaLow-only profile rejected: %+v %v", p, err)
	}
}

// Any number of mesh links: "radio" is the first, "links" the rest.
func TestProfileManyLinks(t *testing.T) {
	three := goodProfile + "\nlinks:\n  - {band: 2g, channel: 6}\n  - {band: s1g, channel: 26}\n  - {band: 6g, channel: 37, width: 80}\n"
	p, err := ParseProfile([]byte(three))
	if err != nil {
		t.Fatalf("four links rejected: %v", err)
	}
	all := p.AllLinks()
	want := []RadioPlan{{Band: "5g", Channel: 149, Width: 20}, {Band: "2g", Channel: 6, Width: 20}, {Band: "s1g", Channel: 26, Width: 2}, {Band: "6g", Channel: 37, Width: 80}}
	if len(all) != len(want) {
		t.Fatalf("links = %+v", all)
	}
	for i := range want {
		if l, ok := p.Link(i); !ok || l != want[i] || all[i] != want[i] {
			t.Errorf("link %d = %+v, want %+v", i, l, want[i])
		}
	}
	if _, ok := p.Link(4); ok {
		t.Error("link 4 of a four-link profile exists")
	}

	// The older "radio2" is the first of the further links, before "links".
	p, err = ParseProfile([]byte(goodProfile + "\nradio2: {band: 2g, channel: 6}\nlinks: [{band: s1g, channel: 26}]\n"))
	if err != nil || len(p.Links) != 2 || p.Links[0].Band != "2g" || p.Links[1].Band != "s1g" {
		t.Fatalf("radio2 plus links: %+v %v", p, err)
	}
	// Written back, it is "links" only.
	out, err := yaml.Marshal(p)
	if err != nil || strings.Contains(string(out), "radio2") || !strings.Contains(string(out), "links:") {
		t.Errorf("a profile is written with links, never radio2: %v\n%s", err, out)
	}
	if back, err := ParseProfile(out); err != nil || len(back.Links) != 2 {
		t.Errorf("round trip: %+v %v", back, err)
	}

	for name, c := range map[string]struct{ extra, field string }{
		"a link on the first link's channel": {"\nlinks: [{band: 5g, channel: 149}]\n", "links[0]"},
		"two further links on one channel":   {"\nlinks: [{band: 2g, channel: 6}, {band: 2g, channel: 6, width: 40}]\n", "links[1]"},
		"a bad channel in the third link":    {"\nlinks: [{band: 2g, channel: 6}, {band: 5g, channel: 200}]\n", "links[1].channel"},
		"radio2 clashing with a link":        {"\nradio2: {band: 2g, channel: 6}\nlinks: [{band: 2g, channel: 6}]\n", "links[0]"},
	} {
		_, err := ParseProfile([]byte(goodProfile + c.extra))
		if err == nil || !strings.Contains(err.Error(), c.field) {
			t.Errorf("%s: want an error naming %s, got %v", name, c.field, err)
		}
	}
	// Two links in one band are fine on different channels (two 5 GHz radios).
	if _, err := ParseProfile([]byte(goodProfile + "\nlinks: [{band: 5g, channel: 36}]\n")); err != nil {
		t.Errorf("two 5 GHz links on different channels rejected: %v", err)
	}
	many := goodProfile + "\nlinks:\n"
	for ch := 1; ch <= MaxMeshLinks; ch++ {
		many += "  - {band: 2g, channel: " + strconv.Itoa(ch) + "}\n"
	}
	if _, err := ParseProfile([]byte(many)); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Errorf("%d links in all: want a limit error, got %v", MaxMeshLinks+1, err)
	}
}

// Role overrides for any link: mesh, mesh2, mesh3, … and ap.
func TestRoleOverridesManyLinks(t *testing.T) {
	const bareNode = "schema: 0\ncallsign: n1\n"
	n, err := ParseNode([]byte(bareNode + "\nradios:\n  roles:\n    mesh3: {radio: \"mac:00:c0:ca:11:22:33\"}\n    ap: disabled\n"))
	if err != nil {
		t.Fatalf("mesh3 role rejected: %v", err)
	}
	r := n.Radios.Roles
	if r.Link(0).Mode != RoleAuto || r.Link(1).Mode != RoleAuto || r.Link(2).Mode != "radio" || r.Link(2).Radio != "mac:00:c0:ca:11:22:33" || r.Link(7).Mode != RoleAuto || r.AP.Mode != RoleDisabled {
		t.Fatalf("roles = %+v", r)
	}
	// YAML: only what is not automatic, and it reads back the same.
	out, err := n.YAML("test")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "mesh3:") || strings.Contains(string(out), "mesh2:") || strings.Contains(string(out), "mesh: ") {
		t.Errorf("sparse node.yaml roles:\n%s", out)
	}
	back, err := ParseNode(out)
	if err != nil || back.Radios.Roles.Link(2).Radio != r.Link(2).Radio || back.Radios.Roles.AP.Mode != RoleDisabled {
		t.Errorf("YAML round trip: %+v %v", back.Radios.Roles, err)
	}
	// JSON (the API): every role named, same keys.
	js, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"mesh":"auto"`, `"mesh2":"auto"`, `"mesh3":{"radio":"mac:00:c0:ca:11:22:33"}`, `"ap":"disabled"`} {
		if !strings.Contains(string(js), want) {
			t.Errorf("JSON roles lack %s: %s", want, js)
		}
	}
	var fromJSON RoleOverrides
	if err := json.Unmarshal(js, &fromJSON); err != nil || fromJSON.Link(2).Radio != r.Link(2).Radio || fromJSON.AP.Mode != RoleDisabled {
		t.Errorf("JSON round trip: %+v %v", fromJSON, err)
	}
	for name, extra := range map[string]string{
		"an unknown role": "\nradios:\n  roles:\n    mash: auto\n",
		"mesh1":           "\nradios:\n  roles:\n    mesh1: auto\n",
		"a link too far":  "\nradios:\n  roles:\n    mesh11: auto\n",
		"a bad selection": "\nradios:\n  roles:\n    mesh2: sometimes\n",
	} {
		if _, err := ParseNode([]byte(bareNode + extra)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := json.Unmarshal([]byte(`{"mesh":"auto","mesh0":"auto"}`), &fromJSON); err == nil {
		t.Error("JSON role mesh0 accepted")
	}
}
