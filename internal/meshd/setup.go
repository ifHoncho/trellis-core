package meshd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"meridian/trellis/internal/config"
	"meridian/trellis/internal/nodestate"
)

// Setup mode (architecture §12.6). A node with no mesh profile cannot join
// a mesh, but it must still be reachable, or it could only be provisioned
// over SSH. So it runs everything as usual on a private mesh profile of its
// own, which no other node has: its client AP and wired ports come up,
// hand out addresses and serve the web UI, where the setup wizard installs
// the real profile and names the node. The private profile is kept in the
// state directory so restarts do not change it, and removed once the node
// is provisioned.

const setupProfileFile = "setup-profile.yaml"

// setupProfile loads the node's private setup profile, creating it on
// first use.
func setupProfile(stateDir string) (*config.Profile, error) {
	path := filepath.Join(stateDir, setupProfileFile)
	if p, err := config.LoadProfile(path); err == nil {
		return p, nil
	}
	tag, err := nodestate.GeneratePassphrase(8)
	if err != nil {
		return nil, err
	}
	secret, err := nodestate.GeneratePassphrase(32)
	if err != nil {
		return nil, err
	}
	p := &config.Profile{
		Schema: config.SchemaVersion, Name: "setup", MeshID: "trellis-setup-" + tag, SAEPassphrase: secret,
		// Country mode and a 2.4 GHz channel every regulatory domain
		// allows: nothing is known yet about where this node is.
		Regulatory: config.Regulatory{Mode: config.RegModeCountry, Country: "US"},
		Radio:      config.RadioPlan{Band: config.Band2G, Channel: 6, Width: 20},
	}
	p.ApplyDefaults()
	if err := p.Validate(); err != nil {
		return nil, err
	}
	data, err := p.YAML("Private mesh profile of a node that waits to be set up (meshd setup mode).\nNo other node has it. meshd removes it when a real profile is installed.")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return nil, err
	}
	return p, nil
}

func removeSetupProfile(stateDir string) {
	if err := os.Remove(filepath.Join(stateDir, setupProfileFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return
	}
}

// defaultNode is the configuration of a node that has no node.yaml: the
// defaults, under a callsign made from the hostname.
func defaultNode(hostname string) (*config.Node, error) {
	// A node Trellis sets up starts closed to its uplink (docs/security.md).
	n := &config.Node{Schema: config.SchemaVersion, Callsign: defaultCallsign(hostname),
		Firewall: config.FirewallConfig{WAN: config.FirewallClosed}}
	n.ApplyDefaults()
	if err := n.Validate(); err != nil {
		return nil, err
	}
	return n, nil
}

// defaultCallsign turns a hostname into a valid callsign (a DNS label).
func defaultCallsign(hostname string) string {
	host, _, _ := strings.Cut(strings.ToLower(hostname), ".")
	var b strings.Builder
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteByte('-')
		}
	}
	cs := strings.Trim(b.String(), "-")
	if len(cs) > 63 {
		cs = strings.Trim(cs[:63], "-")
	}
	if cs == "" || cs == "localhost" {
		return "trellis-node"
	}
	return cs
}
