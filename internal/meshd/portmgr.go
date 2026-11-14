package meshd

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// A portManager is a system network manager (NetworkManager,
// systemd-networkd) that may run an Ethernet port meshd would like to use.
// meshd takes such a port over only at runtime, only while no DHCP server
// answers on it, and only when the manager would configure it with DHCP
// anyway (architecture §7.1). Release and Restore must be safe to call
// from any goroutine.
type portManager interface {
	Name() string
	// Manages describes the manager's hold on the port ("state
	// connecting"), or returns "" when it leaves the port alone.
	Manages(name string, ifindex int) string
	// Static explains why the manager's own configuration for the port is
	// not plain DHCP (a static address, a shared connection), or returns "".
	// Such a port is never taken over: something else is meant to be there.
	Static(name string, ifindex int, mac net.HardwareAddr) string
	// Release tells the manager to leave the port alone until Restore or
	// the next boot.
	Release(name string) error
	Restore(name string) error
}

// candidateLister is implemented by the lab's test manager, whose ports
// are veths (isEthernetPort rejects those).
type candidateLister interface {
	Candidates() []string
}

func runCmd(timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
		}
		return out, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return out, nil
}

// ---------------------------------------------------------------- NetworkManager

type nmManager struct {
	mu     sync.Mutex
	states map[string]string // nmcli device states, cached
	at     time.Time
	run    func(timeout time.Duration, name string, args ...string) ([]byte, error)
}

func newNMManager() *nmManager { return &nmManager{run: runCmd} }

func (m *nmManager) Name() string { return "NetworkManager" }

func (m *nmManager) Manages(name string, _ int) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.states == nil || time.Since(m.at) > nmCacheFor {
		m.states, m.at = map[string]string{}, time.Now()
		if out, err := m.run(2*time.Second, "nmcli", "-t", "-f", "DEVICE,STATE", "device"); err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				if f := nmSplit(line); len(f) == 2 {
					m.states[f[0]] = f[1]
				}
			}
		}
	}
	st := m.states[name]
	if st == "" || st == "unmanaged" {
		return ""
	}
	return "state " + st
}

func (m *nmManager) forget() {
	m.mu.Lock()
	m.states = nil
	m.mu.Unlock()
}

// Static looks at every Ethernet profile NetworkManager could autoconnect
// on the port. One that does not use DHCP for IPv4 (manual, shared,
// link-local, disabled) means the port is meant for something else.
func (m *nmManager) Static(name string, _ int, mac net.HardwareAddr) string {
	out, err := m.run(5*time.Second, "nmcli", "-t", "-f", "UUID,TYPE,AUTOCONNECT", "connection", "show")
	if err != nil {
		return "cannot read NetworkManager's profiles: " + err.Error()
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := nmSplit(line)
		if len(f) != 3 || f[1] != "802-3-ethernet" || f[2] != "yes" {
			continue
		}
		prof, err := m.run(5*time.Second, "nmcli", "-t", "-f", "connection.id,connection.interface-name,802-3-ethernet.mac-address,ipv4.method", "connection", "show", f[0])
		if err != nil {
			return "cannot read NetworkManager profile " + f[0] + ": " + err.Error()
		}
		if why := nmProfileStatic(string(prof), name, mac); why != "" {
			return why
		}
	}
	return ""
}

// nmProfileStatic reads one profile (nmcli -t "field:value" lines) and
// explains why it would not configure the port with DHCP, or returns "".
func nmProfileStatic(prof, name string, mac net.HardwareAddr) string {
	kv := map[string]string{}
	for _, line := range strings.Split(prof, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			kv[k] = nmUnescape(v)
		}
	}
	if ifn := kv["connection.interface-name"]; ifn != "" && ifn != name {
		return ""
	}
	if pm := kv["802-3-ethernet.mac-address"]; pm != "" && !strings.EqualFold(pm, mac.String()) {
		return ""
	}
	if method := kv["ipv4.method"]; method != "" && method != "auto" {
		return fmt.Sprintf("NetworkManager profile %q does not use DHCP on it (ipv4.method %s)", kv["connection.id"], method)
	}
	return ""
}

// nmSplit splits an nmcli terse line on the colons nmcli did not escape.
func nmSplit(line string) []string {
	if line == "" {
		return nil
	}
	var out []string
	var cur strings.Builder
	for i := 0; i < len(line); i++ {
		switch {
		case line[i] == '\\' && i+1 < len(line):
			i++
			cur.WriteByte(line[i])
		case line[i] == ':':
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(line[i])
		}
	}
	return append(out, cur.String())
}

func nmUnescape(v string) string {
	if f := nmSplit(v); len(f) > 0 {
		return strings.Join(f, ":")
	}
	return v
}

// Release and Restore change the device's managed flag at runtime only:
// NetworkManager forgets it on restart and at boot.
func (m *nmManager) Release(name string) error {
	defer m.forget()
	_, err := m.run(10*time.Second, "nmcli", "device", "set", name, "managed", "no")
	return err
}

func (m *nmManager) Restore(name string) error {
	defer m.forget()
	_, err := m.run(10*time.Second, "nmcli", "device", "set", name, "managed", "yes")
	return err
}

// ---------------------------------------------------------------- systemd-networkd

// networkdClaimPath lives in /run (gone at boot) and sorts before
// netplan's 10-netplan-*.network, so its match wins for the ports it names.
const networkdClaimPath = "/run/systemd/network/00-trellis-claimed.network"

type networkdManager struct {
	mu      sync.Mutex
	claimed map[string]bool
	path    string
	run     func(timeout time.Duration, name string, args ...string) ([]byte, error)
}

func newNetworkdManager() *networkdManager {
	return &networkdManager{path: networkdClaimPath, run: runCmd}
}

func (m *networkdManager) Name() string { return "systemd-networkd" }

func (m *networkdManager) Manages(_ string, ifindex int) string {
	st := networkdState(ifindex)
	if st == "" || st == "unmanaged" || st == "linger" {
		return ""
	}
	return "state " + st
}

func (m *networkdManager) Static(_ string, ifindex int, _ net.HardwareAddr) string {
	file := networkdLinkField(ifindex, "NETWORK_FILE")
	if file == "" {
		return ""
	}
	files := []string{file}
	if more, err := filepath.Glob(file + ".d/*.conf"); err == nil {
		files = append(files, more...)
	}
	var all strings.Builder
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return "cannot read " + f + ": " + err.Error()
		}
		all.Write(data)
		all.WriteByte('\n')
	}
	if why := networkdStatic(all.String()); why != "" {
		return filepath.Base(file) + " " + why
	}
	return ""
}

// networkdStatic reads a .network file (with its drop-ins appended) and
// explains why it would not configure IPv4 by DHCP, or returns "".
func networkdStatic(content string) string {
	section, dhcp := "", ""
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch {
		case (section == "[Network]" || section == "[Address]") && k == "Address" && v != "":
			return "gives it a static address (" + v + ")"
		case section == "[Network]" && k == "DHCP":
			dhcp = strings.ToLower(v) // the last one wins, as in networkd
		}
	}
	switch dhcp {
	case "yes", "true", "on", "1", "ipv4", "both":
		return ""
	}
	return "does not use DHCPv4 on it"
}

// networkdLinkField reads one field of networkd's state file for a link.
func networkdLinkField(ifindex int, field string) string {
	f, err := os.Open(fmt.Sprintf("/run/systemd/netif/links/%d", ifindex))
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), field+"="); ok {
			return v
		}
	}
	return ""
}

func (m *networkdManager) load() {
	if m.claimed != nil {
		return
	}
	m.claimed = map[string]bool{}
	// A meshd that crashed left its claims behind: keep them, so the ports
	// are given back properly.
	data, err := os.ReadFile(m.path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "Name="); ok {
			for _, n := range strings.Fields(v) {
				m.claimed[n] = true
			}
		}
	}
}

func (m *networkdManager) write() error {
	if len(m.claimed) == 0 {
		if err := os.Remove(m.path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	names := make([]string, 0, len(m.claimed))
	for n := range m.claimed {
		names = append(names, n)
	}
	sort.Strings(names)
	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		return err
	}
	content := "# Written by trellis meshd: Ethernet ports Trellis took over at runtime\n# (no DHCP server on them). Removed when it gives them back.\n[Match]\nName=" +
		strings.Join(names, " ") + "\n\n[Link]\nUnmanaged=yes\n"
	return os.WriteFile(m.path, []byte(content), 0o644)
}

func (m *networkdManager) Release(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.load()
	if m.claimed[name] {
		if _, err := os.Stat(m.path); err == nil {
			return nil
		}
	}
	m.claimed[name] = true
	if err := m.write(); err != nil {
		delete(m.claimed, name)
		return err
	}
	_, err := m.run(10*time.Second, "networkctl", "reload")
	return err
}

func (m *networkdManager) Restore(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.load()
	delete(m.claimed, name)
	if err := m.write(); err != nil {
		return err
	}
	if _, err := m.run(10*time.Second, "networkctl", "reload"); err != nil {
		return err
	}
	_, err := m.run(10*time.Second, "networkctl", "reconfigure", name)
	return err
}

// ---------------------------------------------------------------- lab

// testManager stands in for a system network manager in the namespace lab
// (meshd --test-port-manager DIR). A file DIR/<port> holding "dhcp" or
// "static" makes the port managed; DIR/<port>.released exists while meshd
// holds it.
type testManager struct{ dir string }

func (m *testManager) Name() string { return "test-manager" }

func (m *testManager) method(name string) string {
	data, err := os.ReadFile(filepath.Join(m.dir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func (m *testManager) released(name string) bool {
	_, err := os.Stat(filepath.Join(m.dir, name+".released"))
	return err == nil
}

func (m *testManager) Manages(name string, _ int) string {
	if meth := m.method(name); meth != "" && !m.released(name) {
		return "method " + meth
	}
	return ""
}

func (m *testManager) Static(name string, _ int, _ net.HardwareAddr) string {
	if meth := m.method(name); meth != "" && meth != "dhcp" {
		return "the test manager configures it (" + meth + ")"
	}
	return ""
}

func (m *testManager) Release(name string) error {
	return os.WriteFile(filepath.Join(m.dir, name+".released"), nil, 0o644)
}

func (m *testManager) Restore(name string) error {
	if err := os.Remove(filepath.Join(m.dir, name+".released")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (m *testManager) Candidates() []string {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && !strings.HasSuffix(e.Name(), ".released") {
			out = append(out, e.Name())
		}
	}
	return out
}
