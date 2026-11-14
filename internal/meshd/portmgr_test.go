package meshd

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNMSplit(t *testing.T) {
	cases := map[string][]string{
		"eth0:connected": {"eth0", "connected"},
		"eth1:connecting (getting IP configuration)": {"eth1", "connecting (getting IP configuration)"},
		`a:802-3-ethernet.mac-address:AA\:BB\:CC`:    {"a", "802-3-ethernet.mac-address", "AA:BB:CC"},
		"": nil,
	}
	for in, want := range cases {
		got := nmSplit(in)
		if strings.Join(got, "|") != strings.Join(want, "|") || len(got) != len(want) {
			t.Errorf("nmSplit(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNMProfileStatic(t *testing.T) {
	mac, _ := net.ParseMAC("f2:4d:7f:9c:fe:1e")
	prof := func(ifn, pmac, method string) string {
		return "connection.id:Wired connection 2\nconnection.interface-name:" + ifn +
			"\n802-3-ethernet.mac-address:" + strings.ReplaceAll(pmac, ":", `\:`) + "\nipv4.method:" + method + "\n"
	}
	cases := []struct {
		name, prof string
		static     bool
	}{
		{"dhcp profile for the port", prof("eth1", "", "auto"), false},
		{"static profile for the port", prof("eth1", "", "manual"), true},
		{"shared profile for the port", prof("eth1", "", "shared"), true},
		{"disabled IPv4", prof("eth1", "", "disabled"), true},
		{"static profile for another port", prof("eth0", "", "manual"), false},
		{"unbound static profile", prof("", "", "manual"), true},
		{"static profile bound to this MAC", prof("", "F2:4D:7F:9C:FE:1E", "manual"), true},
		{"static profile bound to another MAC", prof("", "00:11:22:33:44:55", "manual"), false},
	}
	for _, c := range cases {
		why := nmProfileStatic(c.prof, "eth1", mac)
		if (why != "") != c.static {
			t.Errorf("%s: got %q, want static=%v", c.name, why, c.static)
		}
	}
}

func TestNMManager(t *testing.T) {
	var calls []string
	profiles := map[string]string{
		"u1": "connection.id:Wired connection 1\nconnection.interface-name:eth0\n802-3-ethernet.mac-address:\nipv4.method:manual\n",
		"u2": "connection.id:Wired connection 2\nconnection.interface-name:eth1\n802-3-ethernet.mac-address:\nipv4.method:auto\n",
	}
	m := &nmManager{run: func(_ time.Duration, name string, args ...string) ([]byte, error) {
		cmd := name + " " + strings.Join(args, " ")
		calls = append(calls, cmd)
		switch {
		case cmd == "nmcli -t -f DEVICE,STATE device":
			return []byte("eth0:connected\neth1:unavailable\nbr-mesh:unmanaged\nlo:connected (externally)\n"), nil
		case cmd == "nmcli -t -f UUID,TYPE,AUTOCONNECT connection show":
			return []byte("u1:802-3-ethernet:yes\nu2:802-3-ethernet:yes\nu3:loopback:no\n"), nil
		case strings.HasPrefix(cmd, "nmcli -t -f connection.id"):
			return []byte(profiles[args[len(args)-1]]), nil
		}
		return nil, nil
	}}
	if got := m.Manages("eth1", 0); got != "state unavailable" {
		t.Errorf("Manages(eth1) = %q", got)
	}
	if got := m.Manages("br-mesh", 0); got != "" {
		t.Errorf("Manages(br-mesh) = %q, want unmanaged", got)
	}
	if got := m.Manages("eth9", 0); got != "" {
		t.Errorf("Manages(eth9) = %q, want unknown", got)
	}
	if n := len(calls); n != 1 {
		t.Errorf("device states read %d times, want once (cached)", n)
	}
	if why := m.Static("eth0", 0, nil); !strings.Contains(why, "manual") {
		t.Errorf("Static(eth0) = %q, want the manual profile", why)
	}
	if why := m.Static("eth1", 0, nil); why != "" {
		t.Errorf("Static(eth1) = %q, want DHCP", why)
	}
	calls = nil
	if err := m.Release("eth1"); err != nil {
		t.Fatal(err)
	}
	if err := m.Restore("eth1"); err != nil {
		t.Fatal(err)
	}
	want := []string{"nmcli device set eth1 managed no", "nmcli device set eth1 managed yes"}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Errorf("calls %q, want %q", calls, want)
	}
	m.Manages("eth1", 0)
	if n := len(calls); n != 3 {
		t.Errorf("states not read again after Restore (calls %q)", calls)
	}
}

func TestNetworkdStatic(t *testing.T) {
	cases := []struct {
		name, content string
		static        bool
	}{
		{"netplan dhcp4", "[Match]\nName=eth0\n\n[Network]\nDHCP=ipv4\nLinkLocalAddressing=ipv6\n\n[DHCP]\nRouteMetric=100\n", false},
		{"dhcp yes", "[Network]\nDHCP=yes\n", false},
		{"static", "[Network]\nAddress=192.168.1.5/24\nGateway=192.168.1.1\n", true},
		{"address section", "[Network]\nDHCP=yes\n[Address]\nAddress=10.0.0.2/24\n", true},
		{"ipv6 only", "[Network]\nDHCP=ipv6\n", true},
		{"no dhcp", "[Match]\nName=eth0\n[Network]\nLinkLocalAddressing=no\n", true},
		{"drop-in turns dhcp off", "[Network]\nDHCP=yes\n# drop-in\n[Network]\nDHCP=no\n", true},
		{"commented address", "[Network]\n#Address=1.2.3.4/24\nDHCP=yes\n", false},
	}
	for _, c := range cases {
		why := networkdStatic(c.content)
		if (why != "") != c.static {
			t.Errorf("%s: got %q, want static=%v", c.name, why, c.static)
		}
	}
}

func TestNetworkdManagerClaims(t *testing.T) {
	dir := t.TempDir()
	var calls []string
	m := &networkdManager{path: filepath.Join(dir, "network", "00-trellis-claimed.network"), run: func(_ time.Duration, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil, nil
	}}
	if err := m.Release("eth0"); err != nil {
		t.Fatal(err)
	}
	if err := m.Release("eth1"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(m.path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "Name=eth0 eth1\n") || !strings.Contains(string(data), "Unmanaged=yes") {
		t.Errorf("claim file:\n%s", data)
	}
	// Releasing again changes nothing and reloads nothing.
	n := len(calls)
	if err := m.Release("eth0"); err != nil || len(calls) != n {
		t.Errorf("second release: err %v, calls %q", err, calls[n:])
	}
	// A new meshd (after a crash) picks the claims up from the file.
	m2 := &networkdManager{path: m.path, run: m.run}
	if err := m2.Restore("eth0"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(m.path)
	if !strings.Contains(string(data), "Name=eth1\n") {
		t.Errorf("after restoring eth0:\n%s", data)
	}
	if err := m2.Restore("eth1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.path); !os.IsNotExist(err) {
		t.Errorf("claim file still there with no claims: %v", err)
	}
	if last := calls[len(calls)-1]; last != "networkctl reconfigure eth1" {
		t.Errorf("last call %q, want a reconfigure", last)
	}
}

func TestTestManager(t *testing.T) {
	dir := t.TempDir()
	m := &testManager{dir: dir}
	os.WriteFile(filepath.Join(dir, "eth4"), []byte("dhcp\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "eth6"), []byte("static\n"), 0o644)
	if m.Manages("eth4", 0) == "" || m.Static("eth4", 0, nil) != "" {
		t.Error("eth4 should be managed with DHCP")
	}
	if m.Static("eth6", 0, nil) == "" {
		t.Error("eth6 should be static")
	}
	if err := m.Release("eth4"); err != nil {
		t.Fatal(err)
	}
	if m.Manages("eth4", 0) != "" {
		t.Error("eth4 still managed after Release")
	}
	if got := strings.Join(m.Candidates(), ","); got != "eth4,eth6" {
		t.Errorf("candidates %q", got)
	}
	if err := m.Restore("eth4"); err != nil || m.Manages("eth4", 0) == "" {
		t.Errorf("eth4 not managed after Restore (%v)", err)
	}
}
