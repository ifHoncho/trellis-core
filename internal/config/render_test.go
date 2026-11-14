package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestNodeYAMLRoundTrip(t *testing.T) {
	f, pw := false, 23
	n := Node{
		Callsign: "node-7", NodeID: 7,
		Radios: RadiosConfig{
			Roles:   RoleOverrides{Mesh: RoleSelection{Mode: "radio", Radio: "mac:00:11:22:33:44:55"}, AP: RoleSelection{Mode: RoleDisabled}},
			Devices: []DeviceRule{{Match: DeviceMatch{Port: "usb:1-1.2"}, Label: "Alfa", MaxPowerDBm: &pw}},
		},
		Ports:   []Port{{Name: "eth0", Role: PortRoleWAN}, {Name: "eth1", Role: PortRoleAuto}},
		AP:      APConfig{Enabled: &f, SSID: "team", Passphrase: "correct horse"},
		Gateway: GatewayConfig{Mode: GatewayOff, DownMbit: 50},
		Web:     WebConfig{WANAccess: true},
	}
	n.ApplyDefaults()
	if err := n.Validate(); err != nil {
		t.Fatal(err)
	}
	y, err := n.YAML("Written by a test.\nSecond line.")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(y), "# Written by a test.\n# Second line.\nschema: 0\n") {
		t.Errorf("header or schema missing:\n%s", y)
	}
	for _, absent := range []string{"mesh_mtu", "probe_targets", "up_mbit", "profile:", "mesh2"} {
		if strings.Contains(string(y), absent) {
			t.Errorf("default %q was written:\n%s", absent, y)
		}
	}
	back, err := ParseNode(y)
	if err != nil {
		t.Fatalf("written file does not parse: %v\n%s", err, y)
	}
	// isNode records how a selection was parsed; it is not part of the value.
	clear := func(n *Node) {
		for _, r := range []*RoleSelection{&n.Radios.Roles.Mesh, &n.Radios.Roles.AP} {
			r.isNode = false
		}
	}
	clear(&n)
	clear(back)
	if !reflect.DeepEqual(&n, back) {
		t.Errorf("round trip changed the config:\nwant %+v\ngot  %+v\n%s", n, *back, y)
	}
}

func TestNodeYAMLDefaultsStayOut(t *testing.T) {
	n := Node{Callsign: "n1"}
	n.ApplyDefaults()
	y, err := n.YAML("")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(y), "schema: 0\ncallsign: n1\nnode_id: 0\n"; got != want {
		t.Errorf("default node config:\n%s\nwant:\n%s", got, want)
	}
}

func TestNodeJSON(t *testing.T) {
	n := Node{Callsign: "n1", Radios: RadiosConfig{Roles: RoleOverrides{Mesh: RoleSelection{Mode: "port", Port: "usb:1-1"}}}}
	n.ApplyDefaults()
	js, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"callsign":"n1"`, `"mesh":{"port":"usb:1-1"}`, `"ap":"auto"`, `"wan_access":false`} {
		if !strings.Contains(string(js), want) {
			t.Errorf("JSON lacks %s:\n%s", want, js)
		}
	}
	back, err := ParseNodeJSON(js)
	if err != nil {
		t.Fatal(err)
	}
	if back.Radios.Roles.Mesh.Mode != "port" || back.Radios.Roles.Mesh.Port != "usb:1-1" || back.Radios.Roles.AP.Mode != RoleAuto {
		t.Errorf("roles: %+v", back.Radios.Roles)
	}
	if _, err := ParseNodeJSON([]byte(`{"callsign":"n1","tx_power_dbm":20}`)); err == nil || !strings.Contains(err.Error(), "tx_power_dbm") {
		t.Errorf("unknown field not named: %v", err)
	}
	if _, err := ParseNodeJSON([]byte(`{"callsign":"Bad Name"}`)); err == nil || !strings.Contains(err.Error(), "callsign") {
		t.Errorf("invalid callsign not named: %v", err)
	}
	if _, err := ParseNodeJSON([]byte(`{"callsign":"n1","radios":{"roles":{"mesh":{"radio":"a","port":"b"}}}}`)); err == nil {
		t.Error("radio and port together accepted")
	}
}

func TestProfileCode(t *testing.T) {
	fan := 4
	p := Profile{Name: "alpha", MeshID: "trellis-alpha", SAEPassphrase: "abcdef-ghjkmn-pqrstu-vwxyz2",
		Regulatory: Regulatory{Mode: RegModeCountry, Country: "US"}, Radio: RadioPlan{Band: Band5G, Channel: 149, Width: 80},
		Links: []RadioPlan{{Band: Band2G, Channel: 6}}, Batman: BatmanPlan{MulticastFanout: &fan}}
	p.ApplyDefaults()
	code, err := p.Code()
	if err != nil {
		t.Fatal(err)
	}
	// Short enough for a QR code of at most 61 modules at level M
	// (version 11: 251 bytes), which a phone reads off another screen.
	if !IsProfileCode(code) || len(code) > 251 {
		t.Errorf("code is %d characters: %s", len(code), code)
	}
	t.Logf("profile code with a second radio: %d characters", len(code))
	// Pasted from a message: wrapped lines and padding.
	wrapped := "  " + code[:40] + "\n" + code[40:90] + " \r\n" + code[90:] + "==\n"
	back, err := DecodeProfileCode(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(&p, back) {
		t.Errorf("round trip:\nwant %+v\ngot  %+v", p, *back)
	}
	y, err := p.YAML("profile")
	if err != nil {
		t.Fatal(err)
	}
	fromFile, err := ParseProfile(y)
	if err != nil || !reflect.DeepEqual(&p, fromFile) {
		t.Errorf("YAML round trip: %v\n%s", err, y)
	}
	// Several more links travel in the code too.
	p3 := p
	p3.Links = []RadioPlan{{Band: Band2G, Channel: 6, Width: 20}, {Band: BandS1G, Channel: 26, Width: 2}, {Band: Band6G, Channel: 37, Width: 80}}
	code3, err := p3.Code()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("profile code with four links: %d characters", len(code3))
	if back3, err := DecodeProfileCode(code3); err != nil || !reflect.DeepEqual(back3.Links, p3.Links) {
		t.Errorf("four-link code round trip: %+v %v", back3, err)
	}
	for _, bad := range []string{"", "WIFI:T:WPA;S:x;;", ProfileCodePrefix + "!!!", ProfileCodePrefix + "AAAA", code[:len(code)-9]} {
		if _, err := DecodeProfileCode(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
