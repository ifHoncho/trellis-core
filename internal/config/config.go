// Package config defines the mesh profile and node configuration (schema v0),
// loads them from YAML and validates them before anything is applied.
//
// Two documents exist (architecture §12.6):
//
//   - the mesh profile, shared by every node in a mesh;
//   - the node config, specific to one node.
//
// Validation is strict: unknown keys are rejected, and every error names the
// field it refers to, so an operator can fix a typo without reading source.
package config

import (
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// SchemaVersion is the only schema both documents accept today.
const SchemaVersion = 0

// Default paths. They can be overridden on the command line.
const (
	DefaultNodePath    = "/etc/trellis/node.yaml"
	DefaultProfilePath = "/etc/trellis/profile.yaml"
	DefaultStateDir    = "/var/lib/trellis"
	DefaultRunDir      = "/run/trellis"
)

// Regulatory modes (architecture §5.1).
const (
	RegModeUnrestricted = "unrestricted"
	RegModeCountry      = "country"
)

// Bands a mesh profile can select.
const (
	Band2G  = "2g"
	Band5G  = "5g"
	Band6G  = "6g"
	BandS1G = "s1g" // Wi-Fi HaLow (802.11ah, below 1 GHz); see S1GChannelInfo
)

// Port roles (architecture §7.1, plus "mesh" for wired backhaul and the lab).
const (
	PortRoleAuto   = "auto"
	PortRoleWAN    = "wan"
	PortRoleClient = "client"
	PortRoleMesh   = "mesh"
)

// RoleSelection values for a radio role.
const (
	RoleAuto     = "auto"
	RoleDisabled = "disabled"
)

// Profile is the mesh profile: everything every node in one mesh must agree on.
type Profile struct {
	Schema        int        `yaml:"schema" json:"schema"`
	Name          string     `yaml:"name" json:"name"`
	MeshID        string     `yaml:"mesh_id" json:"mesh_id"` // 802.11s mesh ID on the air; defaults to Name
	SAEPassphrase string     `yaml:"sae_passphrase" json:"sae_passphrase"`
	Regulatory    Regulatory `yaml:"regulatory" json:"regulatory"`
	// Radio is the first mesh link (mesh0).
	Radio RadioPlan `yaml:"radio" json:"radio"`
	// Links are more mesh links, each on its own radio (mesh1, mesh2, …),
	// typically one per band: 5 GHz for capacity, 2.4 GHz and HaLow for
	// reach. Every link is a batman-adv interface, and batman-adv picks the
	// faster path to each node (architecture §5.2). A node
	// runs the links it has radios for.
	Links []RadioPlan `yaml:"links,omitempty" json:"links,omitempty"`
	// LegacyRadio2 is the older spelling of one more link ("radio2"). It is
	// read and moved to the front of Links (ApplyDefaults); nothing writes it.
	LegacyRadio2 *RadioPlan `yaml:"radio2,omitempty" json:"radio2,omitempty"`
	Prefix       string     `yaml:"prefix" json:"prefix"` // IPv4 prefix, default 10.73.0.0/16
	Batman       BatmanPlan `yaml:"batman" json:"batman"`
	// Positions: whether nodes with a GNSS fix put themselves on the
	// team's ATAK map. Off here stops it on every node (emissions control).
	Positions Positions `yaml:"positions,omitempty" json:"positions"`
	// Revision counts mesh-wide changes: a profile sent to every node
	// (meshd meshchange.go) carries a higher one, and a node takes only a
	// profile whose revision is higher than its own. 0 for a profile that
	// was never sent that way.
	Revision uint64 `yaml:"revision,omitempty" json:"revision,omitempty"`

	// fromRadio2: Links[0] was written as "radio2", so that is what an
	// error about it names.
	fromRadio2 bool
}

// MaxMeshLinks is how many mesh links a profile can have. The interfaces
// are mesh0 to mesh9, the names the udev rule and the cleanup code know.
const MaxMeshLinks = 10

// AllLinks returns every mesh link in order: Radio, then Links. Link i's
// interface is "mesh<i>".
func (p *Profile) AllLinks() []RadioPlan {
	return append([]RadioPlan{p.Radio}, p.Links...)
}

// LinkNames names every link by its band and channel (RadioPlan.Name), the
// first link's first.
func (p *Profile) LinkNames() []string {
	var names []string
	for _, l := range p.AllLinks() {
		names = append(names, l.Name())
	}
	return names
}

// Link returns mesh link i (0 is Radio) and whether the profile has it.
func (p *Profile) Link(i int) (RadioPlan, bool) {
	switch {
	case i == 0:
		return p.Radio, true
	case i > 0 && i <= len(p.Links):
		return p.Links[i-1], true
	}
	return RadioPlan{}, false
}

// LinkField is the profile's name for link i's settings, for messages:
// radio, links[0], links[1], … ("radio2" where the file spelled it so).
func (p *Profile) LinkField(i int) string {
	switch {
	case i == 0:
		return "radio"
	case p.fromRadio2 && i == 1:
		return "radio2"
	case p.fromRadio2:
		return fmt.Sprintf("links[%d]", i-2)
	}
	return fmt.Sprintf("links[%d]", i-1)
}

// Regulatory selects how transmit power and channels are limited.
type Regulatory struct {
	Mode    string `yaml:"mode" json:"mode"`       // unrestricted (default) or country
	Country string `yaml:"country" json:"country"` // ISO 3166-1 alpha-2: the node's regulatory domain (mesh and AP), and HaLow's channel plan
}

// RadioPlan is the mesh radio's band and channel.
type RadioPlan struct {
	Band    string `yaml:"band" json:"band"`
	Channel int    `yaml:"channel" json:"channel"`
	// Width in MHz. Wi-Fi: 20 (default), 40, 80 or 160 (2.4 GHz: 20 or
	// 40). HaLow (s1g): 1, 2 (default), 4 or 8; the channel number must be
	// one of that width's in the country's plan.
	Width int `yaml:"width" json:"width"`
}

// DefaultWidth is the channel width a band gets when the profile names none.
func DefaultWidth(band string) int {
	if band == BandS1G {
		return 2
	}
	return 20
}

// BatmanPlan pins batman-adv behaviour across the mesh (architecture §6).
type BatmanPlan struct {
	RoutingAlgo    string `yaml:"routing_algo" json:"routing_algo"`         // BATMAN_V only
	OrigIntervalMS int    `yaml:"orig_interval_ms" json:"orig_interval_ms"` // default 1000
	MinVersion     string `yaml:"min_version" json:"min_version"`           // refuse to start below this; default 2024.0
	PinnedVersion  string `yaml:"pinned_version" json:"pinned_version"`     // warn when different; default 2026.3
	// BridgeLoopAvoidance protects against layer-2 loops when two nodes'
	// wired client ports share a switch (architecture §6). Default on.
	BridgeLoopAvoidance *bool `yaml:"bridge_loop_avoidance" json:"bridge_loop_avoidance"`
	// MulticastFanout: when a multicast packet cannot use batman-adv's
	// multicast packet type (too big, or a node without it), it goes as
	// separate unicasts to up to this many listener nodes, and floods
	// beyond (architecture §9). Unset: batman-adv's default, 16.
	MulticastFanout *int `yaml:"multicast_fanout" json:"multicast_fanout"`
}

// BLA reports whether bridge loop avoidance is on (default true).
func (b BatmanPlan) BLA() bool { return b.BridgeLoopAvoidance == nil || *b.BridgeLoopAvoidance }

// Node is one node's own configuration.
type Node struct {
	Schema      int           `yaml:"schema" json:"schema"`
	Callsign    string        `yaml:"callsign" json:"callsign"`
	NodeID      int           `yaml:"node_id" json:"node_id"`           // 0 = claim automatically
	ProfilePath string        `yaml:"profile,omitempty" json:"profile"` // path of the mesh profile; default /etc/trellis/profile.yaml
	Radios      RadiosConfig  `yaml:"radios,omitempty" json:"radios"`
	Ports       []Port        `yaml:"ports,omitempty" json:"ports"`
	AP          APConfig      `yaml:"ap,omitempty" json:"ap"`
	Gateway     GatewayConfig `yaml:"gateway,omitempty" json:"gateway"`
	MeshMTU     int           `yaml:"mesh_mtu,omitempty" json:"mesh_mtu"` // underlay MTU to request on mesh0; default 1560
	// AllowOldBatman lets meshd run on a batman-adv older than the profile's
	// minimum. For experiments only: such nodes make the whole mesh
	// fall back to flooding (architecture §9).
	AllowOldBatman bool      `yaml:"allow_old_batman,omitempty" json:"allow_old_batman"`
	Web            WebConfig `yaml:"web,omitempty" json:"web"`
	// Positions: whether this node, with a GNSS fix, puts itself on the
	// team's ATAK map. Off here stops it on this node only.
	Positions Positions `yaml:"positions,omitempty" json:"positions"`
}

// Positions controls position sharing (in the profile for the whole mesh,
// in node.yaml for one node). A node shares its position only when both
// allow it. Positions stay inside the mesh: they go to the mesh's ATAK
// group (multicast, never routed) and in gossip.
type Positions struct {
	// Share: nil means yes.
	Share *bool `yaml:"share,omitempty" json:"share,omitempty"`
}

// Shared reports whether p allows sharing.
func (p Positions) Shared() bool { return p.Share == nil || *p.Share }

// WebConfig controls the web UI and HTTP API that webd serves
// (architecture §11, §12.2).
type WebConfig struct {
	// WANAccess also serves the UI on the node's other addresses, such as
	// its uplink. Off by default: the UI is reachable only from the mesh and
	// this node's client ports.
	WANAccess bool `yaml:"wan_access,omitempty" json:"wan_access"`
}

// RadiosConfig holds role overrides and per-radio settings (architecture §5.2).
type RadiosConfig struct {
	Roles   RoleOverrides `yaml:"roles,omitempty" json:"roles"`
	Devices []DeviceRule  `yaml:"devices,omitempty" json:"devices"`
}

// RoleOverrides pins or disables each role. In YAML and JSON the keys are
// "mesh" (the first mesh link), "mesh2", "mesh3", … (the profile's further
// links, in order) and "ap".
type RoleOverrides struct {
	Mesh RoleSelection
	// More holds the further links: More[0] is mesh2. A link beyond its
	// end is automatic.
	More []RoleSelection
	AP   RoleSelection
}

// Link returns the selection for mesh link i (0 is Mesh). Links the config
// does not mention are automatic.
func (r RoleOverrides) Link(i int) RoleSelection {
	switch {
	case i == 0:
		return r.Mesh
	case i > 0 && i <= len(r.More):
		return r.More[i-1]
	}
	return RoleSelection{Mode: RoleAuto}
}

// SetLink sets the selection for mesh link i.
func (r *RoleOverrides) SetLink(i int, sel RoleSelection) {
	if i == 0 {
		r.Mesh = sel
		return
	}
	for len(r.More) < i {
		r.More = append(r.More, RoleSelection{Mode: RoleAuto})
	}
	r.More[i-1] = sel
}

// LinkRoleKey is the config key of mesh link i's role: mesh, mesh2, mesh3, …
func LinkRoleKey(i int) string {
	if i == 0 {
		return "mesh"
	}
	return fmt.Sprintf("mesh%d", i+1)
}

// LinkIfname is the interface meshd makes for mesh link i: mesh0, mesh1, …
// (not the role key: link 1 is role "mesh2" on interface mesh1).
func LinkIfname(i int) string { return "mesh" + strconv.Itoa(i) }

// LinkOfIfname is the inverse of LinkIfname: the link an interface name
// belongs to, or -1 when it is not a mesh link's.
func LinkOfIfname(name string) int {
	i, err := strconv.Atoi(strings.TrimPrefix(name, "mesh"))
	if !strings.HasPrefix(name, "mesh") || err != nil || i < 0 || i >= MaxMeshLinks || name != LinkIfname(i) {
		return -1
	}
	return i
}

// LinkOfRoleKey is the inverse of LinkRoleKey: "mesh" → 0, "mesh3" → 2.
func LinkOfRoleKey(key string) (int, bool) {
	if key == "mesh" {
		return 0, true
	}
	n, err := strconv.Atoi(strings.TrimPrefix(key, "mesh"))
	if !strings.HasPrefix(key, "mesh") || err != nil || n < 2 || n > MaxMeshLinks || key != LinkRoleKey(n-1) {
		return 0, false
	}
	return n - 1, true
}

// IsZero reports whether every role is automatic, so that a written
// node.yaml leaves the block out.
func (r RoleOverrides) IsZero() bool {
	for i := 0; i <= len(r.More); i++ {
		if !r.Link(i).IsZero() {
			return false
		}
	}
	return r.AP.IsZero()
}

// roleMap is the roles as an ordered key list and a map, for both encodings.
func (r RoleOverrides) roleMap(sparse bool) ([]string, map[string]RoleSelection) {
	var keys []string
	m := map[string]RoleSelection{}
	add := func(k string, sel RoleSelection) {
		if sparse && sel.IsZero() {
			return
		}
		keys = append(keys, k)
		m[k] = sel
	}
	for i := 0; i <= len(r.More); i++ {
		add(LinkRoleKey(i), r.Link(i))
	}
	add("ap", r.AP)
	return keys, m
}

// set stores one decoded role under its key.
func (r *RoleOverrides) set(key string, sel RoleSelection) error {
	if key == "ap" {
		r.AP = sel
		return nil
	}
	i, ok := LinkOfRoleKey(key)
	if !ok {
		return fmt.Errorf("unknown role %q (roles are mesh, mesh2 to mesh%d, and ap)", key, MaxMeshLinks)
	}
	r.SetLink(i, sel)
	return nil
}

// UnmarshalYAML reads the role mapping.
func (r *RoleOverrides) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return errors.New("roles: expected a mapping")
	}
	*r = RoleOverrides{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key := n.Content[i].Value
		var sel RoleSelection
		if err := n.Content[i+1].Decode(&sel); err != nil {
			return fmt.Errorf("line %d: roles.%s: %w", n.Content[i].Line, key, err)
		}
		if err := r.set(key, sel); err != nil {
			// The wording translateYAMLError turns into a field error.
			return fmt.Errorf("line %d: field %s not found in type config.RoleOverrides", n.Content[i].Line, key)
		}
	}
	return nil
}

// MarshalYAML writes the roles that are not automatic, in order.
func (r RoleOverrides) MarshalYAML() (any, error) {
	keys, m := r.roleMap(true)
	node := &yaml.Node{Kind: yaml.MappingNode}
	for _, k := range keys {
		var v yaml.Node
		if err := v.Encode(m[k]); err != nil {
			return nil, err
		}
		node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: k}, &v)
	}
	return node, nil
}

// RoleSelection is "auto", "disabled", or a pin to one radio or one port.
// In YAML it is either a plain string or a mapping with one of the keys.
type RoleSelection struct {
	Mode   string `yaml:"-" json:"-"`
	Radio  string `yaml:"radio,omitempty" json:"radio,omitempty"` // fingerprint
	Port   string `yaml:"port,omitempty" json:"port,omitempty"`   // port path, e.g. usb:1-1.2
	isNode bool
}

// UnmarshalYAML accepts "auto", "disabled", {radio: fp} or {port: path}.
func (r *RoleSelection) UnmarshalYAML(n *yaml.Node) error {
	r.isNode = true
	switch n.Kind {
	case yaml.ScalarNode:
		r.Mode = n.Value
		return nil
	case yaml.MappingNode:
		var m struct {
			Radio string `yaml:"radio" json:"radio"`
			Port  string `yaml:"port" json:"port"`
		}
		if err := n.Decode(&m); err != nil {
			return err
		}
		r.Radio, r.Port = m.Radio, m.Port
		switch {
		case m.Radio != "" && m.Port != "":
			return errors.New("set either radio or port, not both")
		case m.Radio != "":
			r.Mode = "radio"
		case m.Port != "":
			r.Mode = "port"
		default:
			return errors.New("expected radio: <fingerprint> or port: <port path>")
		}
		return nil
	default:
		return errors.New("expected a string or a mapping")
	}
}

// MarshalYAML renders the selection back in the same short forms.
func (r RoleSelection) MarshalYAML() (any, error) {
	switch r.Mode {
	case "radio":
		return map[string]string{"radio": r.Radio}, nil
	case "port":
		return map[string]string{"port": r.Port}, nil
	case "":
		return RoleAuto, nil
	default:
		return r.Mode, nil
	}
}

// DeviceRule applies per-radio settings to radios matching one selector.
type DeviceRule struct {
	Match       DeviceMatch `yaml:"match,omitempty" json:"match"`
	Ignore      bool        `yaml:"ignore,omitempty" json:"ignore"`
	Label       string      `yaml:"label,omitempty" json:"label"`
	MaxPowerDBm *int        `yaml:"max_power_dbm,omitempty" json:"max_power_dbm"` // overrides the hardware database for ranking
}

// DeviceMatch selects radios by fingerprint or by port path. Exactly one is set.
type DeviceMatch struct {
	Fingerprint string `yaml:"fingerprint,omitempty" json:"fingerprint"`
	Port        string `yaml:"port" json:"port"`
}

// Port is a wired interface and its role.
type Port struct {
	Name string `yaml:"name" json:"name"`
	Role string `yaml:"role" json:"role"`
}

// APConfig is the client access point (architecture §10).
type APConfig struct {
	Enabled    *bool  `yaml:"enabled,omitempty" json:"enabled"`
	SSID       string `yaml:"ssid,omitempty" json:"ssid"`             // default: the callsign
	Passphrase string `yaml:"passphrase,omitempty" json:"passphrase"` // default: generated once and kept in the state directory
	// Country is ignored, and read so older files still load: a node has
	// one regulatory domain, the mesh profile's, and hostapd would switch
	// the whole node to another (meshd notes it when it differs).
	Country string `yaml:"country,omitempty" json:"country"`
	Band    string `yaml:"band,omitempty" json:"band"`       // 5g preferred, 2g fallback
	Channel int    `yaml:"channel,omitempty" json:"channel"` // 0 = choose (away from the mesh channel)
	// Isolate stops this node's AP clients from reaching each other
	// directly. Off by default: the client network is flat.
	Isolate bool `yaml:"isolate,omitempty" json:"isolate"`
}

// Gateway modes (architecture §8.1).
const (
	GatewayAuto = "auto" // share a healthy WAN when this node has one
	GatewayOff  = "off"  // never share; still use other nodes' gateways
)

// GatewayConfig controls sharing this node's WAN with the mesh.
type GatewayConfig struct {
	Mode string `yaml:"mode,omitempty" json:"mode"`
	// WAN is the uplink interface. Empty: the first port with role wan,
	// else the interface of the system's own default route.
	WAN string `yaml:"wan,omitempty" json:"wan"`
	// ProbeTargets are pinged out of the WAN every probe interval; the WAN
	// is healthy while any of them answers (architecture §8.2).
	ProbeTargets    []string `yaml:"probe_targets,omitempty" json:"probe_targets"`
	ProbeIntervalMS int      `yaml:"probe_interval_ms,omitempty" json:"probe_interval_ms"`
	// Announced uplink capacity in Mbit/s (batman-adv gw_bandwidth).
	DownMbit int `yaml:"down_mbit,omitempty" json:"down_mbit"`
	UpMbit   int `yaml:"up_mbit,omitempty" json:"up_mbit"`
}

// DefaultProbeTargets are well-known anycast resolvers on separate networks.
var DefaultProbeTargets = []string{"1.1.1.1", "8.8.8.8", "9.9.9.9"}

// Field error -----------------------------------------------------------------

// FieldError is one validation problem, tied to the YAML path of the field.
type FieldError struct {
	Field string
	Msg   string
}

func (e *FieldError) Error() string { return e.Field + ": " + e.Msg }

// Errors is a list of field errors. It is returned as the error from Validate.
type Errors []*FieldError

func (es Errors) Error() string {
	if len(es) == 1 {
		return es[0].Error()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d problems:", len(es))
	for _, e := range es {
		b.WriteString("\n  - ")
		b.WriteString(e.Error())
	}
	return b.String()
}

func (es *Errors) add(field, format string, args ...any) {
	*es = append(*es, &FieldError{Field: field, Msg: fmt.Sprintf(format, args...)})
}

func (es Errors) orNil() error {
	if len(es) == 0 {
		return nil
	}
	sort.SliceStable(es, func(i, j int) bool { return es[i].Field < es[j].Field })
	return es
}

// Loading ---------------------------------------------------------------------

// decodeStrict decodes YAML rejecting unknown keys, and turns yaml's errors
// into field errors where it can.
func decodeStrict(data []byte, v any) error {
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return nil // empty document: defaults and validation decide
		}
		return translateYAMLError(err)
	}
	return nil
}

var reUnknownField = regexp.MustCompile(`line (\d+): field (\S+) not found in type`)

func translateYAMLError(err error) error {
	msg := err.Error()
	// yaml.v3 wraps several problems into one TypeError.
	var te *yaml.TypeError
	if errors.As(err, &te) {
		var es Errors
		for _, line := range te.Errors {
			if m := reUnknownField.FindStringSubmatch(line); m != nil {
				es.add(m[2], "unknown field (line %s)", m[1])
			} else {
				es.add("(document)", "%s", strings.TrimPrefix(line, "yaml: "))
			}
		}
		return es.orNil()
	}
	return Errors{{Field: "(document)", Msg: strings.TrimPrefix(msg, "yaml: ")}}
}

// LoadProfile reads, defaults and validates a mesh profile.
func LoadProfile(path string) (*Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseProfile(data)
}

// ParseProfile parses, defaults and validates a mesh profile from YAML.
func ParseProfile(data []byte) (*Profile, error) {
	var p Profile
	if err := decodeStrict(data, &p); err != nil {
		return nil, err
	}
	p.ApplyDefaults()
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// LoadNode reads, defaults and validates a node config.
func LoadNode(path string) (*Node, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseNode(data)
}

// ParseNode parses, defaults and validates a node config from YAML.
func ParseNode(data []byte) (*Node, error) {
	var n Node
	if err := decodeStrict(data, &n); err != nil {
		return nil, err
	}
	n.ApplyDefaults()
	if err := n.Validate(); err != nil {
		return nil, err
	}
	return &n, nil
}

// Defaults --------------------------------------------------------------------

// ApplyDefaults fills unset fields with the documented defaults.
func (p *Profile) ApplyDefaults() {
	if p.MeshID == "" {
		p.MeshID = p.Name
	}
	if p.Regulatory.Mode == "" {
		p.Regulatory.Mode = RegModeUnrestricted
	}
	if p.Regulatory.Country == "" {
		p.Regulatory.Country = "US"
	}
	p.Regulatory.Country = strings.ToUpper(p.Regulatory.Country)
	if p.Radio.Band == "" {
		p.Radio.Band = Band5G
	}
	if p.Radio.Width == 0 {
		p.Radio.Width = DefaultWidth(p.Radio.Band)
	}
	if p.LegacyRadio2 != nil {
		p.Links = append([]RadioPlan{*p.LegacyRadio2}, p.Links...)
		p.LegacyRadio2, p.fromRadio2 = nil, true
	}
	for i := range p.Links {
		if p.Links[i].Width == 0 {
			p.Links[i].Width = DefaultWidth(p.Links[i].Band)
		}
	}
	if p.Prefix == "" {
		p.Prefix = "10.73.0.0/16"
	}
	if p.Batman.RoutingAlgo == "" {
		p.Batman.RoutingAlgo = "BATMAN_V"
	}
	if p.Batman.OrigIntervalMS == 0 {
		p.Batman.OrigIntervalMS = 1000
	}
	if p.Batman.MinVersion == "" {
		p.Batman.MinVersion = "2024.0"
	}
	if p.Batman.PinnedVersion == "" {
		p.Batman.PinnedVersion = "2026.3"
	}
}

// validateRadio checks one mesh link's band, channel and width. country is
// the profile's: HaLow channels differ from country to country.
func validateRadio(es *Errors, field string, r RadioPlan, country string) {
	switch r.Band {
	case Band2G, Band5G, Band6G:
	case BandS1G:
		validateS1G(es, field, r, country)
		return
	default:
		es.add(field+".band", "must be one of 2g, 5g, 6g, s1g (got %q)", r.Band)
	}
	if _, err := ChannelFrequencyMHz(r.Band, r.Channel); err != nil {
		es.add(field+".channel", "%v", err)
	}
	if ws := ChannelWidths(r.Band); ws != nil && !slices.Contains(ws, r.Width) {
		es.add(field+".width", "%s takes %s MHz (got %d)", BandName(r.Band), orList(ws), r.Width)
	}
}

// BandName is a band in words: "2.4 GHz", "HaLow".
func BandName(band string) string {
	switch band {
	case Band2G:
		return "2.4 GHz"
	case Band5G:
		return "5 GHz"
	case Band6G:
		return "6 GHz"
	case BandS1G:
		return "HaLow"
	}
	return band
}

// orList writes 20, 40 or 80.
func orList(v []int) string {
	s := intList(v)
	if i := strings.LastIndex(s, ", "); i >= 0 {
		s = s[:i] + " or " + s[i+2:]
	}
	return s
}

// ApplyDefaults fills unset fields with the documented defaults.
func (n *Node) ApplyDefaults() {
	if n.ProfilePath == "" {
		n.ProfilePath = DefaultProfilePath
	}
	if n.Radios.Roles.Mesh.Mode == "" {
		n.Radios.Roles.Mesh.Mode = RoleAuto
	}
	if n.Radios.Roles.AP.Mode == "" {
		n.Radios.Roles.AP.Mode = RoleAuto
	}
	for i := range n.Radios.Roles.More {
		if n.Radios.Roles.More[i].Mode == "" {
			n.Radios.Roles.More[i].Mode = RoleAuto
		}
	}
	for i := range n.Ports {
		if n.Ports[i].Role == "" {
			n.Ports[i].Role = PortRoleAuto
		}
	}
	if n.AP.Enabled == nil {
		t := true
		n.AP.Enabled = &t
	}
	n.AP.Country = strings.ToUpper(n.AP.Country)
	if n.AP.Band == "" {
		n.AP.Band = Band5G
	}
	if n.MeshMTU == 0 {
		n.MeshMTU = 1560
	}
	if n.Gateway.Mode == "" {
		n.Gateway.Mode = GatewayAuto
	}
	if len(n.Gateway.ProbeTargets) == 0 {
		n.Gateway.ProbeTargets = append([]string{}, DefaultProbeTargets...)
	}
	if n.Gateway.ProbeIntervalMS == 0 {
		n.Gateway.ProbeIntervalMS = 1000
	}
	if n.Gateway.DownMbit == 0 {
		n.Gateway.DownMbit = 20
	}
	if n.Gateway.UpMbit == 0 {
		n.Gateway.UpMbit = 5
	}
}

// Validation ------------------------------------------------------------------

var (
	reCountry  = regexp.MustCompile(`^[A-Z]{2}$`)
	reCallsign = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	reIfname   = regexp.MustCompile(`^[^/\s:]{1,15}$`)
	reVersion  = regexp.MustCompile(`^\d{4}\.\d+$`)
)

// Validate checks the profile. The returned error, if any, is an Errors list.
func (p *Profile) Validate() error {
	var es Errors
	if p.Schema != SchemaVersion {
		es.add("schema", "must be %d (got %d)", SchemaVersion, p.Schema)
	}
	if p.Name == "" {
		es.add("name", "required")
	}
	if l := len(p.MeshID); l == 0 || l > 32 {
		es.add("mesh_id", "must be 1–32 bytes (got %d)", l)
	}
	if l := len(p.SAEPassphrase); l < 8 || l > 128 {
		es.add("sae_passphrase", "must be 8–128 characters (got %d)", l)
	} else if err := printableASCII(p.SAEPassphrase); err != nil {
		es.add("sae_passphrase", "%v", err)
	}
	switch p.Regulatory.Mode {
	case RegModeUnrestricted, RegModeCountry:
	default:
		es.add("regulatory.mode", "must be %q or %q (got %q)", RegModeUnrestricted, RegModeCountry, p.Regulatory.Mode)
	}
	if !reCountry.MatchString(p.Regulatory.Country) {
		es.add("regulatory.country", "must be a two-letter ISO country code (got %q)", p.Regulatory.Country)
	}
	validateRadio(&es, "radio", p.Radio, p.Regulatory.Country)
	if p.LegacyRadio2 != nil {
		es.add("radio2", "not applied: call ApplyDefaults first") // a programming error, not a file's
	}
	all := p.AllLinks()
	if len(all) > MaxMeshLinks {
		es.add("links", "at most %d mesh links in all (got %d)", MaxMeshLinks, len(all))
	}
	for i := 1; i < len(all); i++ {
		validateRadio(&es, p.LinkField(i), all[i], p.Regulatory.Country)
		for j := 0; j < i; j++ {
			if all[i].Band == all[j].Band && all[i].Channel == all[j].Channel {
				es.add(p.LinkField(i), "two mesh links must not use the same channel (%s channel %d is also %s)", all[i].Band, all[i].Channel, p.LinkField(j))
				break
			}
		}
	}
	if pfx, err := netip.ParsePrefix(p.Prefix); err != nil {
		es.add("prefix", "not a valid IPv4 prefix: %v", err)
	} else if !pfx.Addr().Is4() {
		es.add("prefix", "must be IPv4")
	} else if pfx.Bits() != 16 {
		es.add("prefix", "must be a /16 in this release (got /%d)", pfx.Bits())
	} else if pfx.Masked() != pfx {
		es.add("prefix", "host bits set; use %s", pfx.Masked())
	}
	if p.Batman.RoutingAlgo != "BATMAN_V" {
		es.add("batman.routing_algo", "only BATMAN_V is supported (got %q)", p.Batman.RoutingAlgo)
	}
	if f := p.Batman.MulticastFanout; f != nil && (*f < 0 || *f > 255) {
		es.add("batman.multicast_fanout", "must be 0–255 (got %d)", *f)
	}
	if p.Batman.OrigIntervalMS < 200 || p.Batman.OrigIntervalMS > 10000 {
		es.add("batman.orig_interval_ms", "must be 200–10000 (got %d)", p.Batman.OrigIntervalMS)
	}
	if !reVersion.MatchString(p.Batman.MinVersion) {
		es.add("batman.min_version", "must look like 2024.0 (got %q)", p.Batman.MinVersion)
	}
	if !reVersion.MatchString(p.Batman.PinnedVersion) {
		es.add("batman.pinned_version", "must look like 2026.3 (got %q)", p.Batman.PinnedVersion)
	}
	return es.orNil()
}

// Validate checks the node config. The returned error, if any, is an Errors list.
func (n *Node) Validate() error {
	var es Errors
	if n.Schema != SchemaVersion {
		es.add("schema", "must be %d (got %d)", SchemaVersion, n.Schema)
	}
	if n.Callsign == "" {
		es.add("callsign", "required")
	} else if !reCallsign.MatchString(n.Callsign) {
		es.add("callsign", "must be a DNS label: lowercase letters, digits and hyphens, 1–63 characters (got %q)", n.Callsign)
	}
	if n.NodeID < 0 || n.NodeID > 254 {
		es.add("node_id", "must be 1–254, or 0 to claim automatically (got %d)", n.NodeID)
	}
	if n.ProfilePath == "" {
		es.add("profile", "required")
	}
	validateRole := func(field string, r RoleSelection) {
		switch r.Mode {
		case RoleAuto, RoleDisabled:
		case "radio":
			if r.Radio == "" {
				es.add(field+".radio", "required")
			}
		case "port":
			if r.Port == "" {
				es.add(field+".port", "required")
			}
		default:
			es.add(field, "must be auto, disabled, {radio: <fingerprint>} or {port: <port>} (got %q)", r.Mode)
		}
	}
	validateRole("radios.roles.mesh", n.Radios.Roles.Mesh)
	validateRole("radios.roles.ap", n.Radios.Roles.AP)
	for i := range n.Radios.Roles.More {
		validateRole("radios.roles."+LinkRoleKey(i+1), n.Radios.Roles.More[i])
	}
	for i, d := range n.Radios.Devices {
		f := fmt.Sprintf("radios.devices[%d]", i)
		switch {
		case d.Match.Fingerprint == "" && d.Match.Port == "":
			es.add(f+".match", "set fingerprint or port")
		case d.Match.Fingerprint != "" && d.Match.Port != "":
			es.add(f+".match", "set either fingerprint or port, not both")
		}
		if d.MaxPowerDBm != nil && (*d.MaxPowerDBm < 0 || *d.MaxPowerDBm > 40) {
			es.add(f+".max_power_dbm", "must be 0–40 (got %d)", *d.MaxPowerDBm)
		}
	}
	seen := map[string]bool{}
	for i, p := range n.Ports {
		f := fmt.Sprintf("ports[%d]", i)
		if p.Name == "" {
			es.add(f+".name", "required")
		} else if !reIfname.MatchString(p.Name) {
			es.add(f+".name", "not a valid interface name (got %q)", p.Name)
		} else if seen[p.Name] {
			es.add(f+".name", "duplicate port %q", p.Name)
		}
		seen[p.Name] = true
		switch p.Role {
		case PortRoleAuto, PortRoleWAN, PortRoleClient, PortRoleMesh:
		default:
			es.add(f+".role", "must be auto, wan, client or mesh (got %q)", p.Role)
		}
	}
	if n.AP.SSID != "" && len(n.AP.SSID) > 32 {
		es.add("ap.ssid", "must be at most 32 bytes (got %d)", len(n.AP.SSID))
	}
	if n.AP.Passphrase != "" && (len(n.AP.Passphrase) < 8 || len(n.AP.Passphrase) > 63) {
		es.add("ap.passphrase", "must be 8–63 characters (got %d)", len(n.AP.Passphrase))
	} else if err := printableASCII(n.AP.Passphrase); err != nil {
		es.add("ap.passphrase", "%v", err)
	}
	if n.AP.Country != "" && !reCountry.MatchString(n.AP.Country) {
		es.add("ap.country", "must be a two-letter ISO country code (got %q)", n.AP.Country)
	}
	switch n.AP.Band {
	case Band2G, Band5G:
		if n.AP.Channel != 0 {
			if _, err := ChannelFrequencyMHz(n.AP.Band, n.AP.Channel); err != nil {
				es.add("ap.channel", "%v", err)
			}
		}
	default:
		es.add("ap.band", "must be 2g or 5g (got %q)", n.AP.Band)
	}
	if n.MeshMTU < 1280 || n.MeshMTU > 2304 {
		es.add("mesh_mtu", "must be 1280–2304 (got %d)", n.MeshMTU)
	}
	switch n.Gateway.Mode {
	case GatewayAuto, GatewayOff:
	default:
		es.add("gateway.mode", "must be auto or off (got %q)", n.Gateway.Mode)
	}
	if n.Gateway.WAN != "" && !reIfname.MatchString(n.Gateway.WAN) {
		es.add("gateway.wan", "not a valid interface name (got %q)", n.Gateway.WAN)
	}
	for i, t := range n.Gateway.ProbeTargets {
		if a, err := netip.ParseAddr(t); err != nil || !a.Is4() {
			es.add(fmt.Sprintf("gateway.probe_targets[%d]", i), "must be an IPv4 address (got %q)", t)
		}
	}
	if n.Gateway.ProbeIntervalMS < 200 || n.Gateway.ProbeIntervalMS > 10000 {
		es.add("gateway.probe_interval_ms", "must be 200–10000 (got %d)", n.Gateway.ProbeIntervalMS)
	}
	if n.Gateway.DownMbit < 1 || n.Gateway.DownMbit > 100000 {
		es.add("gateway.down_mbit", "must be 1–100000 (got %d)", n.Gateway.DownMbit)
	}
	if n.Gateway.UpMbit < 1 || n.Gateway.UpMbit > 100000 {
		es.add("gateway.up_mbit", "must be 1–100000 (got %d)", n.Gateway.UpMbit)
	}
	return es.orNil()
}

// Helpers ---------------------------------------------------------------------

// printableASCII rejects characters that cannot be written unescaped into
// wpa_supplicant and hostapd quoted strings.
func printableASCII(s string) error {
	for i, r := range s {
		if r < 0x20 || r > 0x7e {
			return fmt.Errorf("character %d is not printable ASCII", i+1)
		}
		if r == '"' || r == '\\' {
			return fmt.Errorf("character %d (%q) is not allowed", i+1, r)
		}
	}
	return nil
}

// ChannelFrequencyMHz converts a band and channel number to a centre
// frequency, the way the kernel does (ieee80211_channel_to_freq_khz). It
// keeps no list of its own: any number the kernel can map is accepted, and
// whether the radio has that channel, and may transmit there, is for the
// radio and the regulatory domain in force to say (meshd reports it).
func ChannelFrequencyMHz(band string, ch int) (int, error) {
	switch band {
	case Band2G:
		switch {
		case ch >= 1 && ch <= 13:
			return 2407 + 5*ch, nil
		case ch == 14:
			return 2484, nil
		}
		return 0, fmt.Errorf("2.4 GHz channel numbers run from 1 to 14 (got %d)", ch)
	case Band5G:
		switch {
		case ch >= 182 && ch <= 196: // 4.9 GHz, numbered in the 5 GHz band
			return 4000 + 5*ch, nil
		case ch >= 1 && ch <= 196:
			return 5000 + 5*ch, nil
		}
		return 0, fmt.Errorf("5 GHz channel numbers run from 1 to 196 (got %d)", ch)
	case Band6G:
		switch {
		case ch == 2:
			return 5935, nil
		case ch >= 1 && ch <= 233:
			return 5950 + 5*ch, nil
		}
		return 0, fmt.Errorf("6 GHz channel numbers run from 1 to 233 (got %d)", ch)
	}
	return 0, fmt.Errorf("unsupported band %q", band)
}

// Name is what the UI, messages and meshctl call a mesh link: its band and
// channel ("5 GHz · ch 149"), the same on every node of the mesh. Interface
// names (mesh0, mesh1) are for logs and kernel tools.
func (rp RadioPlan) Name() string {
	return fmt.Sprintf("%s · ch %d", BandName(rp.Band), rp.Channel)
}

// ChannelWidths are the widths in MHz a mesh link can ask for in a band.
// They are what the band and the software can carry, not a policy: 2.4 GHz
// is 83 MHz wide, wpa_supplicant (up to 2.11) builds no
// 320 MHz mesh, and no HaLow chip runs 16 MHz. A radio or regulatory domain
// that allows less narrows the link, and meshd reports it.
func ChannelWidths(band string) []int {
	switch band {
	case Band2G:
		return []int{20, 40}
	case Band5G, Band6G:
		return []int{20, 40, 80, 160}
	case BandS1G:
		return slices.Clone(S1GWidths)
	}
	return nil
}

// S1GChannel is one Wi-Fi HaLow (802.11ah) channel in one country.
type S1GChannel struct {
	Channel  int
	WidthMHz int // 1, 2, 4 or 8
	OpClass  int // global operating class (IEEE 802.11 Annex E, table E-5)
	FreqKHz  int // centre frequency
}

// FreqMHz is the centre frequency in MHz; HaLow channels sit on half-MHz steps.
func (c S1GChannel) FreqMHz() float64 { return float64(c.FreqKHz) / 1000 }

// s1gClass is one operating class: channels of one width in one region.
type s1gClass struct {
	widthMHz, opClass int
	startKHz          int   // channel n is centred on startKHz + 500 kHz x n
	chans             []int // channel numbers
	countries         []string
	// only narrows the channels for some of the countries.
	only map[string][]int
}

// channels is the class's channel list for one country.
func (c s1gClass) channels(country string) []int {
	if o, ok := c.only[country]; ok {
		return o
	}
	return c.chans
}

// Countries that share a HaLow channel plan, as Morse Micro's software
// groups them.
var (
	s1gAmericas = []string{"US", "CA", "BR", "MX", "BS", "BB", "BZ", "CR", "GY", "HN", "JM", "NI", "PA"}
	s1gAU       = []string{"AU", "AR", "CL", "CO", "EC", "SV", "HT", "UY"}
	s1gEU863    = []string{"EU", "GB", "IN", "AL", "AT", "BE", "BA", "BG", "HR", "CY", "CZ", "DK", "EE", "FI", "FR", "DE", "GR",
		"HU", "IS", "IE", "IT", "LV", "LI", "LT", "LU", "MT", "MD", "NL", "MK", "NO", "PL", "PT", "RO", "SK", "SI", "ES", "SE",
		"CH", "TR", "AD", "GF", "GE", "KE", "MC", "ME", "SM", "RS", "UG", "UA", "VA"}
	s1gEU916 = []string{"AT", "BE", "BG", "FI", "FR", "GB", "IS", "IE", "IT", "LT", "MD", "MK", "PT", "SK", "SI", "SE", "TR",
		"UG", "CY", "DK", "HU", "LV", "PL", "ES", "LU"}
)

// seq is first, first+step, … up to last.
func seq(first, last, step int) []int {
	var out []int
	for ch := first; ch <= last; ch += step {
		out = append(out, ch)
	}
	return out
}

// s1gClasses are the HaLow channel plans: every region Morse Micro's
// wpa_supplicant_s1g knows, which is what starts a HaLow mesh. The values
// are those of IEEE 802.11 Annex E and Morse Micro's own classes, as carried
// in their hostap fork (src/utils/morse.c, the REVmf table it uses by
// default). Trellis adds no limit of its own: a channel the supplicant
// accepts for a country is accepted here. The radio module may still allow
// less (many modules are built for one region), and the driver must
// be loaded for the same country (meshd checks).
var s1gClasses = []s1gClass{
	// The Americas, 902–928 MHz (us1–us4).
	{widthMHz: 1, opClass: 68, startKHz: 902000, chans: seq(1, 51, 2), countries: s1gAmericas},
	{widthMHz: 2, opClass: 69, startKHz: 902000, chans: seq(2, 50, 4), countries: s1gAmericas},
	{widthMHz: 4, opClass: 70, startKHz: 902000, chans: seq(8, 48, 8), countries: s1gAmericas},
	{widthMHz: 8, opClass: 71, startKHz: 902000, chans: seq(12, 44, 16), countries: s1gAmericas},
	// Europe, India and others, 863–868 MHz (eu6), and 916.4–919.4 MHz where allowed (eu30).
	{widthMHz: 1, opClass: 66, startKHz: 863000, chans: seq(1, 9, 2), countries: s1gEU863, only: map[string][]int{"IN": {5, 7, 9}}},
	{widthMHz: 1, opClass: 77, startKHz: 901400, chans: []int{31, 33, 35}, countries: s1gEU916,
		only: map[string][]int{"BG": {33, 35}, "FI": {33, 35}, "FR": {33, 35}, "GB": {33, 35}, "IT": {33, 35}, "TR": {33, 35}, "LU": {33, 35}}},
	// Japan (jp8–jp12, and the 850 MHz transition channels jp195, jp196).
	{widthMHz: 1, opClass: 73, startKHz: 916500, chans: []int{9, 13, 15, 17, 19, 21}, countries: []string{"JP"}},
	{widthMHz: 2, opClass: 64, startKHz: 922500, chans: []int{2, 4, 6, 8}, countries: []string{"JP"}},
	{widthMHz: 4, opClass: 65, startKHz: 906500, chans: []int{36, 38}, countries: []string{"JP"}},
	{widthMHz: 1, opClass: 195, startKHz: 800000, chans: []int{94, 96}, countries: []string{"JP"}},
	{widthMHz: 2, opClass: 196, startKHz: 800000, chans: []int{95}, countries: []string{"JP"}},
	// South Korea (kr14).
	{widthMHz: 1, opClass: 74, startKHz: 917000, chans: seq(1, 11, 2), countries: []string{"KR"}},
	// Singapore (sg18–sg31).
	{widthMHz: 1, opClass: 68, startKHz: 902000, chans: seq(31, 45, 2), countries: []string{"SG"}},
	{widthMHz: 2, opClass: 61, startKHz: 902000, chans: seq(32, 44, 4), countries: []string{"SG"}},
	{widthMHz: 4, opClass: 62, startKHz: 902000, chans: []int{34, 42}, countries: []string{"SG"}},
	{widthMHz: 8, opClass: 63, startKHz: 902000, chans: []int{38}, countries: []string{"SG"}},
	// Australia and the countries on its plan (au22–au25, and the proposed au39, au40).
	{widthMHz: 1, opClass: 50, startKHz: 902000, chans: seq(28, 50, 2), countries: s1gAU},
	{widthMHz: 2, opClass: 51, startKHz: 902000, chans: seq(29, 49, 4), countries: s1gAU},
	{widthMHz: 4, opClass: 52, startKHz: 902000, chans: []int{31, 39, 47}, countries: s1gAU},
	{widthMHz: 8, opClass: 53, startKHz: 902000, chans: []int{35, 43}, countries: s1gAU},
	{widthMHz: 4, opClass: 48, startKHz: 894000, chans: []int{51, 59}, countries: s1gAU},
	{widthMHz: 8, opClass: 49, startKHz: 894000, chans: []int{55}, countries: s1gAU},
	// New Zealand (nz26–nz29).
	{widthMHz: 1, opClass: 68, startKHz: 902000, chans: seq(27, 51, 2), countries: []string{"NZ"}},
	{widthMHz: 2, opClass: 69, startKHz: 902000, chans: seq(30, 50, 4), countries: []string{"NZ"}},
	{widthMHz: 4, opClass: 70, startKHz: 902000, chans: seq(32, 48, 8), countries: []string{"NZ"}},
	{widthMHz: 8, opClass: 71, startKHz: 902000, chans: []int{44}, countries: []string{"NZ"}},
}

// S1GWidths are the HaLow channel widths in MHz.
var S1GWidths = []int{1, 2, 4, 8}

// S1GCountries lists the countries with a HaLow channel plan, sorted.
func S1GCountries() []string {
	var out []string
	for _, c := range s1gClasses {
		for _, cc := range c.countries {
			if !slices.Contains(out, cc) {
				out = append(out, cc)
			}
		}
	}
	slices.Sort(out)
	return out
}

// S1GChannels lists a country's HaLow channel numbers at one width, in
// order, or nil when the country has none at that width.
func S1GChannels(country string, widthMHz int) []int {
	var out []int
	for _, c := range s1gClasses {
		if c.widthMHz == widthMHz && slices.Contains(c.countries, country) {
			out = append(out, c.channels(country)...)
		}
	}
	slices.Sort(out)
	return out
}

// S1GChannelInfo resolves a HaLow channel number at a width in a country.
// The errors are written for the profile's author.
func S1GChannelInfo(country string, widthMHz, ch int) (S1GChannel, error) {
	if !slices.Contains(S1GWidths, widthMHz) {
		return S1GChannel{}, fmt.Errorf("HaLow width must be 1, 2, 4 or 8 MHz (got %d)", widthMHz)
	}
	known := false
	for _, c := range s1gClasses {
		if !slices.Contains(c.countries, country) {
			continue
		}
		known = true
		if c.widthMHz == widthMHz && slices.Contains(c.channels(country), ch) {
			return S1GChannel{Channel: ch, WidthMHz: widthMHz, OpClass: c.opClass, FreqKHz: c.startKHz + 500*ch}, nil
		}
	}
	if !known {
		return S1GChannel{}, fmt.Errorf("Morse Micro's HaLow software has no channel plan for country %s, so no HaLow mesh can start under it; countries with one: %s",
			country, strings.Join(S1GCountries(), " "))
	}
	if chs := S1GChannels(country, widthMHz); len(chs) > 0 {
		return S1GChannel{}, fmt.Errorf("HaLow channel at %d MHz in %s must be one of %s (got %d)", widthMHz, country, intList(chs), ch)
	}
	var ws []int
	for _, w := range S1GWidths {
		if len(S1GChannels(country, w)) > 0 {
			ws = append(ws, w)
		}
	}
	return S1GChannel{}, fmt.Errorf("%s's HaLow plan has no %d MHz channels; it has %s MHz", country, widthMHz, orList(ws))
}

func validateS1G(es *Errors, field string, r RadioPlan, country string) {
	if !slices.Contains(S1GWidths, r.Width) {
		es.add(field+".width", "HaLow width must be 1, 2, 4 or 8 MHz (got %d)", r.Width)
		return
	}
	if !reCountry.MatchString(country) {
		return // reported on regulatory.country
	}
	if _, err := S1GChannelInfo(country, r.Width, r.Channel); err != nil {
		if len(S1GChannels(country, r.Width)) == 0 && slices.Contains(S1GCountries(), country) {
			es.add(field+".width", "%v", err) // the country has no channel of this width
		} else {
			es.add(field+".channel", "%v", err)
		}
	}
}

// LinkFrequencyMHz is the centre frequency of a mesh link's channel, 0 when
// the plan is not valid. Wi-Fi frequencies are whole MHz; HaLow's are not.
func LinkFrequencyMHz(r RadioPlan, country string) float64 {
	if r.Band == BandS1G {
		c, err := S1GChannelInfo(country, r.Width, r.Channel)
		if err != nil {
			return 0
		}
		return c.FreqMHz()
	}
	f, err := ChannelFrequencyMHz(r.Band, r.Channel)
	if err != nil {
		return 0
	}
	return float64(f)
}

// JoinInts writes numbers for a message: "52, 56, 60, 64".
func JoinInts(v []int) string { return intList(v) }

func intList(v []int) string {
	parts := make([]string, len(v))
	for i, n := range v {
		parts[i] = fmt.Sprint(n)
	}
	return strings.Join(parts, ", ")
}

// APEnabled reports whether the client AP is enabled.
func (n *Node) APEnabled() bool { return n.AP.Enabled == nil || *n.AP.Enabled }

// MeshPorts returns the names of wired ports with the mesh role.
func (n *Node) MeshPorts() []string {
	var out []string
	for _, p := range n.Ports {
		if p.Role == PortRoleMesh {
			out = append(out, p.Name)
		}
	}
	return out
}

// ClientPorts returns the names of wired ports with the client role.
func (n *Node) ClientPorts() []string {
	var out []string
	for _, p := range n.Ports {
		if p.Role == PortRoleClient {
			out = append(out, p.Name)
		}
	}
	return out
}

// Redacted returns a copy with secrets blanked, for status output.
func (p Profile) Redacted() Profile {
	if p.SAEPassphrase != "" {
		p.SAEPassphrase = "<redacted>"
	}
	return p
}

// Redacted returns a copy with secrets blanked, for status output.
func (n Node) Redacted() Node {
	if n.AP.Passphrase != "" {
		n.AP.Passphrase = "<redacted>"
	}
	return n
}

// AutoPorts returns the names of wired ports listed with the auto role.
func (n *Node) AutoPorts() []string {
	var out []string
	for _, p := range n.Ports {
		if p.Role == PortRoleAuto {
			out = append(out, p.Name)
		}
	}
	return out
}

// OwnedPorts returns the wired ports Trellis configures itself (client,
// mesh and auto), which the system's network managers must leave alone.
// WAN ports stay with the system.
func (n *Node) OwnedPorts() []string {
	var out []string
	for _, p := range n.Ports {
		if p.Role != PortRoleWAN {
			out = append(out, p.Name)
		}
	}
	return out
}

// PortRole returns the role a port is listed with, or "".
func (n *Node) PortRole(name string) string {
	for _, p := range n.Ports {
		if p.Name == name {
			return p.Role
		}
	}
	return ""
}

// WANPorts returns the names of wired ports with the wan role.
func (n *Node) WANPorts() []string {
	var out []string
	for _, p := range n.Ports {
		if p.Role == PortRoleWAN {
			out = append(out, p.Name)
		}
	}
	return out
}
