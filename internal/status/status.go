// Package status defines the JSON that meshd serves and that meshctl and webd
// consume. It is the one description of a node's state (architecture §12.7).
package status

import (
	"fmt"
	"time"
)

// Status is a complete snapshot of one node.
type Status struct {
	Version    string     `json:"version"`
	Hostname   string     `json:"hostname"`
	Time       time.Time  `json:"time"`
	StartedAt  time.Time  `json:"started_at"`
	Node       Node       `json:"node"`
	Profile    Profile    `json:"profile"`
	Regulatory Regulatory `json:"regulatory"`
	Radios     []Radio    `json:"radios"`
	Plan       Plan       `json:"plan"`
	MeshLink   MeshLink   `json:"mesh_link"`
	// MoreLinks are the profile's further mesh links (index 2, 3, …), each on
	// its own radio, whether this node has a radio for them or not.
	MoreLinks []MeshLink   `json:"more_links,omitempty"`
	Batman    Batman       `json:"batman"`
	Bridge    Bridge       `json:"bridge"`
	AP        AP           `json:"ap"`
	ClientNet ClientNet    `json:"client_network"`
	Gateway   GatewayState `json:"gateway"`
	// Clock is how this node keeps time, and GNSS its receiver.
	Clock Clock `json:"clock"`
	GNSS  GNSS  `json:"gnss"`
	// Positions: this node on the team's ATAK map, and the last known
	// positions of nodes that left the mesh.
	Positions Positions `json:"positions"`
	// SiteLink: this node's part in joining parts of the mesh in
	// different places over the internet.
	SiteLink SiteLink `json:"site_link"`
	// Resets are the last times the node went down other than by a
	// shutdown, newest first, each with what the kernel's crash record (or
	// the lack of one) says about why.
	Resets []Reset `json:"resets"`
	// Trials are new driver modules, firmware or board files being tried,
	// and the last trial of each: pending until the node has run well on
	// them for a while, undone if it restarts first.
	Trials []Trial `json:"trials"`
	// MeshChange is a mesh-wide profile change on its way, or the last one
	// for a while after it is in force (meshd meshchange.go); nil when none.
	MeshChange *MeshChange `json:"mesh_change,omitempty"`
	// Rollout is a mesh-wide upgrade under way, or the last one for a
	// while after it ended (meshd rollout.go); nil when none.
	Rollout   *Rollout   `json:"rollout,omitempty"`
	Ports     []Port     `json:"ports"` // Ethernet ports and their roles (architecture §7.1)
	Nodes     []MeshNode `json:"nodes"` // other nodes, from gossip
	Gossip    Gossip     `json:"gossip"`
	Reconcile Reconcile  `json:"reconcile"`
	Process   Process    `json:"process"`
	// Host is the computer the node runs on: temperatures, load, memory,
	// storage and its power supply.
	Host     Host      `json:"host"`
	Warnings []Warning `json:"warnings"`
	Setup    Setup     `json:"setup"`
}

// Trial is a new driver module, firmware or board file being tried.
type Trial struct {
	Name    string `json:"name"`
	Kind    string `json:"kind,omitempty"` // module, firmware, bcf
	Version string `json:"version,omitempty"`
	Module  string `json:"module,omitempty"`
	// State: armed (loads at the next boot), pending (loaded, not kept
	// yet), confirmed (kept), reverted (the old files are back).
	State string `json:"state"`
	// Latched: undone by the fallback; that version stays off until it is
	// re-armed. Reason says why it was undone.
	Latched bool   `json:"latched,omitempty"`
	Reason  string `json:"reason,omitempty"`
	// ConfirmAfterS is how long a pending trial still has to run well
	// before it is kept; 0 while it is not healthy.
	ConfirmAfterS int       `json:"confirm_after_s,omitempty"`
	RebootNeeded  bool      `json:"reboot_needed,omitempty"`
	Started       time.Time `json:"started"`
	Ended         time.Time `json:"ended,omitempty"`
	Files         []string  `json:"files"`
}

// Reset is one time the node went down other than by a shutdown.
type Reset struct {
	// Time is when the crash happened (the start of its boot plus the
	// uptime in its record); for a reset that left none, or a boot whose
	// clock was never set, when the next boot started.
	Time time.Time `json:"time"`
	// TimeUnknown: Time was read from a clock not set yet (no RTC, no
	// time source in reach), and may be days out.
	TimeUnknown bool `json:"time_unknown,omitempty"`
	// Verdict: pcie-link (an interrupt flood on the PCIe port, then a card
	// stopped answering), card-stopped (a card stopped answering on its
	// bus), sysrq (a reset request on a serial console), panic (another
	// kernel panic), power-loss (nothing recorded), unrecorded (the board
	// keeps no crash records).
	Verdict string `json:"verdict"`
	Summary string `json:"summary"`
	Detail  string `json:"detail,omitempty"`
	// UptimeS is how long the node had been up; 0 when not known.
	UptimeS float64 `json:"uptime_s,omitempty"`
}

// Setup says whether the node still waits to be provisioned. A node with
// no mesh profile runs alone on a private setup mesh, so that its client
// AP and ports serve the web UI's setup wizard (architecture §12.6).
type Setup struct {
	Needed bool `json:"needed"`
	// NodeConfig and Profile report which configuration files exist.
	NodeConfig bool `json:"node_config"`
	Profile    bool `json:"profile"`
}

// Node is this node's identity.
type Node struct {
	Callsign     string `json:"callsign"`
	NodeID       int    `json:"node_id"`
	NodeIDSource string `json:"node_id_source"` // config, derived or claimed
	MAC          string `json:"mac"`
	Address      string `json:"address"` // CIDR on br-mesh
	// ConfigDigest changes whenever the node's own configuration does:
	// whoever holds a copy knows to read it again.
	ConfigDigest string `json:"config_digest,omitempty"`
}

// Profile is the mesh profile in force, without secrets.
type Profile struct {
	Name    string `json:"name"`
	MeshID  string `json:"mesh_id"`
	Band    string `json:"band"`
	Channel int    `json:"channel"`
	// FreqMHz is a whole number on Wi-Fi; HaLow channels sit on half-MHz
	// steps (915.5).
	FreqMHz        float64 `json:"freq_mhz"`
	WidthMHz       int     `json:"width_mhz"`
	Prefix         string  `json:"prefix"`
	PinnedBatman   string  `json:"pinned_batman"`
	MinBatman      string  `json:"min_batman"`
	OrigIntervalMS int     `json:"orig_interval_ms"`
	// PositionsShared: the profile lets nodes share their positions.
	PositionsShared bool `json:"positions_shared"`
	// Revision counts the mesh-wide changes that made this profile; Digest
	// changes whenever the profile in force does (a mesh-wide change, an
	// install on this node), so that whoever holds a copy of the
	// configuration knows to read it again.
	Revision uint64 `json:"revision,omitempty"`
	Digest   string `json:"digest,omitempty"`
}

// Regulatory shows the requested mode and what is actually in force.
type Regulatory struct {
	RequestedMode string `json:"requested_mode"` // unrestricted or country
	Country       string `json:"country"`
	ActiveMode    string `json:"active_mode"` // country or unrestricted: what the kernel's rules are
	KernelAlpha2  string `json:"kernel_alpha2"`
	// Source is where the kernel's rules come from: "trellis" (trellis crda
	// answers the kernel's requests) or "kernel" (its own regulatory.db).
	Source string `json:"source,omitempty"`
	Note   string `json:"note,omitempty"`
}

// PowerFact is a maximum transmit power figure and its source.
type PowerFact struct {
	DBm    float64 `json:"dbm"`
	Source string  `json:"source"`
}

// Radio is one physical radio as the Radios page shows it.
type Radio struct {
	Fingerprint       string               `json:"fingerprint"`
	FingerprintSource string               `json:"fingerprint_source"`
	Phy               string               `json:"phy"`
	Label             string               `json:"label,omitempty"`
	Chipset           string               `json:"chipset,omitempty"`
	Driver            string               `json:"driver"`
	Bus               string               `json:"bus"`
	Port              string               `json:"port"`
	DeviceID          string               `json:"device_id,omitempty"`
	USBSpeedMbps      int                  `json:"usb_speed_mbps,omitempty"`
	PermAddr          string               `json:"perm_addr"`
	Bands             []string             `json:"bands"`
	Streams           int                  `json:"streams"`
	MeshPoint         bool                 `json:"mesh_point"`
	AP                bool                 `json:"ap"`
	MeshAndAP         bool                 `json:"mesh_and_ap"`
	Power             map[string]PowerFact `json:"power"`
	// PowerByWidth is the radio's transmit power in dBm by band, then by
	// channel width in MHz, where the hardware database has it.
	PowerByWidth map[string]map[int]float64 `json:"power_by_width,omitempty"`
	Role         string                     `json:"role"`
	Recommended  string                     `json:"recommended"`
	Interfaces   []string                   `json:"interfaces"`
	Quirks       []string                   `json:"quirks,omitempty"`
	Ignored      bool                       `json:"ignored,omitempty"`
	Broken       string                     `json:"broken,omitempty"`
	HardBlocked  bool                       `json:"hard_blocked,omitempty"`
	InUse        string                     `json:"in_use,omitempty"`
	// LED: the driver exposes an LED, so the radio can be identified by
	// blinking it.
	LED bool `json:"led"`
	// PCIe is the radio's link to its slot, for a PCIe radio.
	PCIe *PCIeLink `json:"pcie,omitempty"`
	// Thermal is the radio's temperature and transmit throttling, where
	// its driver has a sensor.
	Thermal *RadioThermal `json:"thermal,omitempty"`
}

// PCIeLink is a PCIe radio's link to the port it sits in. A card with two
// radios has one link, shown on both.
type PCIeLink struct {
	Device string `json:"device"`         // PCI address
	Port   string `json:"port,omitempty"` // the upstream port's
	// SpeedGTs and Width are what the link trained at, MaxSpeedGTs and
	// MaxWidth the most both ends support; Gen and MaxGen the same speeds
	// as PCIe generations. Zero when the kernel does not say.
	SpeedGTs    float64 `json:"speed_gts,omitempty"`
	Width       int     `json:"width,omitempty"`
	MaxSpeedGTs float64 `json:"max_speed_gts,omitempty"`
	MaxWidth    int     `json:"max_width,omitempty"`
	Gen         int     `json:"gen,omitempty"`
	MaxGen      int     `json:"max_gen,omitempty"`
	// Counted: the kernel has error counters for the link; Correctable,
	// NonFatal and Fatal are their totals since boot. On some boards they
	// stay at zero whatever the link does (see Health).
	Counted     bool   `json:"counted"`
	Correctable uint64 `json:"correctable"`
	NonFatal    uint64 `json:"nonfatal"`
	Fatal       uint64 `json:"fatal"`
	// Health is what sampling the link's error status found: "ok",
	// "marginal" (packets go unacknowledged and are sent again: the radio
	// can stop answering under traffic), "kernel" (the kernel counts the
	// errors itself; see the counters) or "unknown" (not sampled).
	Health string `json:"health"`
	// HealthDetail says it in words. ReplayPct is the share of the last
	// sample's windows with a replay rollover and PeakReplayPct the
	// highest of the last minutes, in percent; Errors the corrected
	// errors seen, at either end.
	HealthDetail  string    `json:"health_detail,omitempty"`
	ReplayPct     float64   `json:"replay_pct"`
	PeakReplayPct float64   `json:"peak_replay_pct"`
	Errors        []string  `json:"errors,omitempty"`
	SampledAt     time.Time `json:"sampled_at,omitempty"`
}

// String is the link in a line: what it trained at, the kernel's counters,
// and what sampling found.
func (l *PCIeLink) String() string {
	s := "speed unknown"
	if l.Gen > 0 {
		s = fmt.Sprintf("Gen%d x%d", l.Gen, l.Width)
		if l.MaxGen > l.Gen || l.MaxWidth > l.Width {
			s += fmt.Sprintf(" (card and slot can do Gen%d x%d)", l.MaxGen, l.MaxWidth)
		}
	}
	if l.Counted {
		s += fmt.Sprintf("; kernel counters: %d corrected, %d uncorrectable", l.Correctable, l.NonFatal+l.Fatal)
	}
	switch {
	case l.HealthDetail != "":
		s += "; error status: " + l.HealthDetail
	case l.Health == "unknown":
		s += "; error status not sampled"
	}
	return s
}

// String is the temperature and throttling in a line.
func (t *RadioThermal) String() string {
	s := fmt.Sprintf("%.0f °C", t.TempC)
	if t.ThrottleAtC > 0 {
		s += fmt.Sprintf(" (throttles at %.0f °C)", t.ThrottleAtC)
	}
	switch {
	case t.Throttling:
		s += fmt.Sprintf(", THROTTLING: transmit time cut to %d%%", t.DutyPct)
	case t.DutyPct >= 0:
		s += ", not throttling"
	}
	return s
}

// RadioThermal is a radio's temperature and transmit throttling.
type RadioThermal struct {
	TempC float64 `json:"temp_c"`
	// ThrottleAtC is the temperature at which the driver starts to cut the
	// radio's transmit time; 0 when it does not say.
	ThrottleAtC float64 `json:"throttle_at_c,omitempty"`
	// DutyPct is the share of time the radio may transmit: 100 when
	// nothing holds it back, -1 when the driver does not say.
	DutyPct    int  `json:"duty_pct"`
	Throttling bool `json:"throttling"`
}

// Plan is the role assignment result.
type Plan struct {
	MeshRadio   string    `json:"mesh_radio,omitempty"`
	APRadio     string    `json:"ap_radio,omitempty"`
	APBand      string    `json:"ap_band,omitempty"`
	Notices     []Warning `json:"notices"`
	Differs     bool      `json:"recommendation_differs"`
	GraceActive bool      `json:"grace_active,omitempty"` // a lost radio is being debounced
}

// MeshLink is the 802.11s link state.
type MeshLink struct {
	// Index numbers the mesh links from 1 (the profile's radio).
	Index      int        `json:"index"`
	Interface  string     `json:"interface,omitempty"`
	Transport  string     `json:"transport"` // wifi, wired, none
	Supplicant Supplicant `json:"supplicant"`
	// Band, Channel and WidthMHz are the profile's for this link (Wi-Fi
	// links only; a wired-only link has none).
	Band     string `json:"band,omitempty"`
	Channel  int    `json:"channel,omitempty"`
	WidthMHz int    `json:"width_mhz,omitempty"`
	// Visit is where the link runs instead of Band, Channel and WidthMHz
	// for now: most of the mesh is missing, and its mesh ID was heard
	// there. Absent when it runs its profile's.
	Visit *MeshLinkVisit `json:"visit,omitempty"`
	// FreqMHz is 0 until the mesh has started on the link.
	FreqMHz float64 `json:"freq_mhz,omitempty"`
	// ChannelBusyPct is how busy the channel is where this node is, over
	// the last few seconds, in percent; -1 when the driver does not say.
	// NoiseDBm is its noise floor, 0 when unknown.
	ChannelBusyPct int `json:"channel_busy_pct"`
	NoiseDBm       int `json:"noise_dbm,omitempty"`
	// RunningWidthMHz is the width the interface runs at once the mesh has
	// started (Wi-Fi links): wpa_supplicant, the radio or the regulatory
	// domain may narrow WidthMHz, and a warning says so.
	RunningWidthMHz int     `json:"running_width_mhz,omitempty"`
	MTU             int     `json:"mtu,omitempty"`
	TXPowerDBm      float64 `json:"tx_power_dbm,omitempty"`
	// WidthCost is what the link's width costs in link budget on this
	// node's radio, against the band's narrowest width; nil for a link at
	// that width, or with no radio here.
	WidthCost   *WidthCost `json:"width_cost,omitempty"`
	Peers       []Peer     `json:"peers"`
	SAEFailures []SAEEvent `json:"sae_failures,omitempty"`
	WiredPorts  []string   `json:"wired_ports,omitempty"`
}

// WidthCost is the link budget a channel width costs against the band's
// narrowest (BaseMHz: 20 on Wi-Fi). A wider channel takes in more noise
// (NoiseDB), and a radio sends it with less power (PowerDB, when the
// hardware database knows the radio's power per width: PowerKnown, with
// PowerDBm at this width and BasePowerDBm at the narrowest). TotalDB is
// their sum: how much weaker a signal the link would cope with at the
// narrowest width. Range falls with it; throughput close by rises.
type WidthCost struct {
	WidthMHz     int     `json:"width_mhz"`
	BaseMHz      int     `json:"base_mhz"`
	NoiseDB      float64 `json:"noise_db"`
	PowerKnown   bool    `json:"power_known"`
	PowerDBm     float64 `json:"power_dbm,omitempty"`
	BasePowerDBm float64 `json:"base_power_dbm,omitempty"`
	PowerDB      float64 `json:"power_db"`
	TotalDB      float64 `json:"total_db"`
}

// String is the cost in a line.
func (c *WidthCost) String() string {
	if c.PowerKnown {
		return fmt.Sprintf("%.1f dB less link budget than at %d MHz (this radio sends %g dBm at %d MHz against %g; %.0f dB more noise)",
			c.TotalDB, c.BaseMHz, c.PowerDBm, c.WidthMHz, c.BasePowerDBm, c.NoiseDB)
	}
	return fmt.Sprintf("%.0f dB less link budget than at %d MHz (more noise in the wider channel; this radio's power per width is not known)", c.TotalDB, c.BaseMHz)
}

// Supplicant is wpa_supplicant's state.
type Supplicant struct {
	Running  bool   `json:"running"`
	PID      int    `json:"pid,omitempty"`
	Restarts int    `json:"restarts,omitempty"`
	State    string `json:"state,omitempty"` // wpa_state
	Joined   bool   `json:"joined"`
}

// Peer is a mesh peer as nl80211 reports it.
type Peer struct {
	MAC              string  `json:"mac"`
	SignalDBm        int     `json:"signal_dbm"`
	TXMbps           float64 `json:"tx_mbps"`
	RXMbps           float64 `json:"rx_mbps"`
	InactiveMS       uint32  `json:"inactive_ms"`
	ConnectedSeconds uint32  `json:"connected_s"`
	PlinkState       string  `json:"plink_state"`
	Established      bool    `json:"established"`
}

// SAEEvent records a rejected peer.
type SAEEvent struct {
	Time time.Time `json:"time"`
	// Interface is the mesh link's interface it happened on.
	Interface string `json:"interface,omitempty"`
	MAC       string `json:"mac"`
	Kind      string `json:"kind"` // failure or blocked
	Message   string `json:"message"`
}

// Batman is the batman-adv state.
type Batman struct {
	Interface      string         `json:"interface"`
	Present        bool           `json:"present"`
	ModuleVersion  string         `json:"module_version"`
	ModuleInferred bool           `json:"module_inferred,omitempty"`
	ModulePath     string         `json:"module_path,omitempty"`
	PinnedMismatch bool           `json:"pinned_mismatch"`
	Algo           string         `json:"algo"`
	MAC            string         `json:"mac"`
	MTU            int            `json:"mtu"`
	MTUOK          bool           `json:"mtu_ok"`
	HardIfs        []HardIf       `json:"hard_interfaces"`
	Neighbors      []Neighbor     `json:"neighbors"`
	Originators    []Originator   `json:"originators"`
	Gateways       []Gateway      `json:"gateways"`
	Settings       map[string]any `json:"settings"`
	Multicast      Multicast      `json:"multicast"`
}

// SiteLink is this node's part in the site link (architecture §13, Site link).
type SiteLink struct {
	// PublicKey is this node's WireGuard key: listed in the profile's
	// site_link.members, it makes the node a member.
	PublicKey string `json:"public_key"`
	Member    bool   `json:"member"`
	// Members counts the profile's members, all sites.
	Members int    `json:"members"`
	Site    string `json:"site,omitempty"`
	// Address is this node's tunnel address; Port its UDP port.
	Address string `json:"address,omitempty"`
	Port    int    `json:"port,omitempty"`
	// Endpoint is where the profile says the other sites reach this node;
	// empty when it only dials out.
	Endpoint string     `json:"endpoint,omitempty"`
	Peers    []SitePeer `json:"peers"`
	Error    string     `json:"error,omitempty"`
	// GuardDropped counts packets for or from this node's tunnel address
	// that were not inside WireGuard, and were dropped: someone on the
	// uplink or the client network sending as a tunnel would.
	GuardDropped uint64 `json:"guard_dropped,omitempty"`
	// Roster is every member the profile lists, with what this node knows
	// of it: the same on a node that is no member, so that the link's
	// state shows anywhere in the mesh.
	Roster []SiteMemberState `json:"roster"`
}

// SiteMemberState is a member of the site link as this node sees it.
type SiteMemberState struct {
	Name      string `json:"name,omitempty"`
	Site      string `json:"site"`
	PublicKey string `json:"public_key"`
	Endpoint  string `json:"endpoint,omitempty"`
	// Self: this node. Heard: a node this one hears holds the key
	// (Callsign); a member of a site not joined is not heard.
	Self     bool   `json:"self,omitempty"`
	Heard    bool   `json:"heard"`
	Callsign string `json:"callsign,omitempty"`
	// Reported: the node says how its tunnels are (Up of Peers). A member
	// not heard, or on an older version, does not.
	Reported bool `json:"reported"`
	Up       int  `json:"up"`
	Peers    int  `json:"peers"`
}

// SitePeer is a member of another site this node tunnels to.
type SitePeer struct {
	Name string `json:"name,omitempty"`
	// Callsign is the node that holds this key, when it is one this node
	// hears (over the link, or without it).
	Callsign  string `json:"callsign,omitempty"`
	Site      string `json:"site"`
	PublicKey string `json:"public_key"`
	// Endpoint is the profile's; Resolved its address when it is a name;
	// Current where WireGuard sends to now (the peer's address as last
	// seen, behind NAT too).
	Endpoint  string `json:"endpoint,omitempty"`
	Resolved  string `json:"resolved,omitempty"`
	Current   string `json:"current,omitempty"`
	Interface string `json:"interface"`
	Address   string `json:"address"`
	// LastHandshake is zero when there has been none.
	LastHandshake time.Time `json:"last_handshake,omitzero"`
	// Up: a handshake within the last 180 s (WireGuard's session life).
	Up      bool   `json:"up"`
	RxBytes uint64 `json:"rx_bytes"`
	TxBytes uint64 `json:"tx_bytes"`
	// Batman: the tunnel is an active batman-adv interface, and the
	// neighbours batman-adv hears over it.
	Batman    bool `json:"batman"`
	Neighbors int  `json:"neighbors"`
	// Note says why the tunnel is not up, or what is odd about it, in
	// words; empty when there is nothing to say.
	Note string `json:"note,omitempty"`
}

// HardIf is a batman-adv hard interface.
type HardIf struct {
	Name   string `json:"name"`
	MAC    string `json:"mac"`
	Active bool   `json:"active"`
	MTU    int    `json:"mtu"`
}

// Neighbor is a single-hop neighbour.
type Neighbor struct {
	MAC            string  `json:"mac"`
	Via            string  `json:"via"`
	LastSeenMS     uint32  `json:"last_seen_ms"`
	ThroughputMbps float64 `json:"throughput_mbps"`
}

// Originator is a known node and the route to it.
type Originator struct {
	MAC            string  `json:"mac"`
	NextHop        string  `json:"next_hop"`
	Via            string  `json:"via"`
	LastSeenMS     uint32  `json:"last_seen_ms"`
	ThroughputMbps float64 `json:"throughput_mbps"`
	Best           bool    `json:"best"`
}

// Gateway is an announced internet gateway.
type Gateway struct {
	MAC            string  `json:"mac"`
	Router         string  `json:"router"`
	DownMbps       float64 `json:"down_mbps"`
	UpMbps         float64 `json:"up_mbps"`
	ThroughputMbps float64 `json:"throughput_mbps"`
	Best           bool    `json:"best"`
}

// Multicast summarises batman-adv's multicast flags.
type Multicast struct {
	Optimisations        bool   `json:"optimisations"`
	OwnFlags             uint32 `json:"own_flags"`
	Bridged              bool   `json:"bridged"`
	QuerierIPv4          bool   `json:"querier_ipv4"`
	QuerierIPv4Shadowing bool   `json:"querier_ipv4_shadowing"`
	QuerierIPv6          bool   `json:"querier_ipv6"`
	QuerierIPv6Shadowing bool   `json:"querier_ipv6_shadowing"`
	PeersWithFlags       int    `json:"peers_with_flags"`
	LegacyPeers          int    `json:"legacy_peers"` // peers without multicast optimisations
	// WantAllIPv4/IPv6: this node asks for every group (no usable querier).
	WantAllIPv4 bool `json:"want_all_ipv4"`
	WantAllIPv6 bool `json:"want_all_ipv6"`
	// PeersWantAllIPv4: nodes that receive every IPv4 group, so every
	// IPv4 multicast packet is sent to them too.
	PeersWantAllIPv4 int `json:"peers_want_all_ipv4"`
	PeersNoPtype     int `json:"peers_no_mc_ptype"` // peers without the multicast packet type
	// Path is a one-word summary: "efficient" (sent only where listeners
	// are), "partial" (some nodes still get everything) or "flooding".
	Path     string        `json:"path"`
	Counters McastCounters `json:"counters"`
}

// McastCounters are batman-adv's multicast packet type counters on this
// node, since bat0 was created ("batctl statistics"): proof that the
// efficient path is used.
type McastCounters struct {
	TX       uint64 `json:"tx"` // sent with the multicast packet type
	TXBytes  uint64 `json:"tx_bytes"`
	TXLocal  uint64 `json:"tx_local"` // of those, delivered to this node's own clients too
	RX       uint64 `json:"rx"`       // received with the multicast packet type
	RXLocal  uint64 `json:"rx_local"` // received and delivered to this node's clients
	Fwd      uint64 `json:"fwd"`      // forwarded on to further nodes
	FwdBytes uint64 `json:"fwd_bytes"`
}

// Bridge is br-mesh.
type Bridge struct {
	Name    string   `json:"name"`
	Present bool     `json:"present"`
	Ports   []string `json:"ports"`
	Address string   `json:"address,omitempty"`
	MTU     int      `json:"mtu,omitempty"`
}

// Reconcile summarises the control loop.
type Reconcile struct {
	Passes       uint64           `json:"passes"`
	LastPass     time.Time        `json:"last_pass"`
	LastDuration string           `json:"last_duration"`
	Converged    bool             `json:"converged"`
	Reconcilers  []ReconcilerInfo `json:"reconcilers"`
}

// ReconcilerInfo is one reconciler's recent history.
type ReconcilerInfo struct {
	Name        string    `json:"name"`
	LastError   string    `json:"last_error,omitempty"`
	LastChanged time.Time `json:"last_changed,omitempty"`
	Changes     uint64    `json:"changes"`
	Errors      uint64    `json:"errors"`
}

// Warning is a problem the operator should know about.
type Warning struct {
	Level   string `json:"level"` // info, warning, error
	Code    string `json:"code"`
	Message string `json:"message"`
	Source  string `json:"source,omitempty"`
}

// Process is meshd's own resource use, for spotting leaks.
type Process struct {
	PID        int `json:"pid"`
	OpenFiles  int `json:"open_files"`
	Goroutines int `json:"goroutines"`
}

// Host is the computer the node runs on, as its kernel reports it. A
// figure the kernel does not give is zero (or, for CPUPct, -1).
type Host struct {
	// Model is the board or machine: the device tree's model, else the
	// firmware's vendor and product name.
	Model   string  `json:"model,omitempty"`
	UptimeS float64 `json:"uptime_s"`
	CPUs    int     `json:"cpus"`
	// CPUPct is the share of all CPUs' time spent busy since the sample
	// before (a few seconds); -1 before there is one.
	CPUPct float64 `json:"cpu_pct"`
	// Load is the run queue averaged over 1, 5 and 15 minutes.
	Load [3]float64 `json:"load"`
	// CPUMHz is the first CPU's clock now, CPUMaxMHz the most it may run
	// at.
	CPUMHz    int `json:"cpu_mhz,omitempty"`
	CPUMaxMHz int `json:"cpu_max_mhz,omitempty"`
	// MemTotal and MemAvailable are in bytes; available is what programs
	// can still have without swapping (caches given back included).
	MemTotal     uint64 `json:"mem_total"`
	MemAvailable uint64 `json:"mem_available"`
	SwapTotal    uint64 `json:"swap_total"`
	SwapFree     uint64 `json:"swap_free"`
	// Temps are the board's thermal sensors (the SoC's, and on some
	// boards a GPU's or the package's), hottest first. Radios' are on
	// each radio.
	Temps []HostTemp `json:"temps"`
	// Undervoltage is whether the supply is too low now: nil when the
	// board does not say (only Raspberry Pis do). UndervoltageAt is the
	// last time meshd saw it low, since it started.
	Undervoltage   *bool      `json:"undervoltage,omitempty"`
	UndervoltageAt *time.Time `json:"undervoltage_at,omitempty"`
	// Filesystems are those on a block device, one entry per device.
	Filesystems []Filesystem `json:"filesystems"`
}

// HostTemp is one thermal sensor.
type HostTemp struct {
	Name  string  `json:"name"`
	TempC float64 `json:"temp_c"`
	// HotC is where the board starts to slow itself to cool down: the
	// sensor's passive trip point, else 80 °C (where a Raspberry Pi's
	// firmware does, a common figure for small boards). CritC is where
	// the kernel turns the computer off; 0 when it does not say.
	HotC  float64 `json:"hot_c"`
	CritC float64 `json:"crit_c,omitempty"`
}

// Filesystem is one mounted filesystem's space, in bytes. Avail is what
// programs may still write; Size - Avail - Used is reserved for root.
type Filesystem struct {
	Mount  string `json:"mount"`
	Device string `json:"device"`
	Type   string `json:"type"`
	Size   uint64 `json:"size"`
	Used   uint64 `json:"used"`
	Avail  uint64 `json:"avail"`
}

// UsedPct is how full a filesystem is, as df counts it: used against
// what is usable (used + avail).
func (f Filesystem) UsedPct() float64 {
	if f.Used+f.Avail == 0 {
		return 0
	}
	return 100 * float64(f.Used) / float64(f.Used+f.Avail)
}

// Helper is the state of a supervised helper process (hostapd, dnsmasq).
type Helper struct {
	Running  bool   `json:"running"`
	PID      int    `json:"pid,omitempty"`
	Restarts int    `json:"restarts,omitempty"`
	State    string `json:"state,omitempty"`
}

// AP is the client access point (architecture §10).
type AP struct {
	Enabled         bool     `json:"enabled"`
	Interface       string   `json:"interface,omitempty"`
	Radio           string   `json:"radio,omitempty"` // fingerprint
	SharesMeshRadio bool     `json:"shares_mesh_radio"`
	SSID            string   `json:"ssid,omitempty"`
	Band            string   `json:"band,omitempty"`
	Channel         int      `json:"channel,omitempty"`
	FreqMHz         int      `json:"freq_mhz,omitempty"`
	Hostapd         Helper   `json:"hostapd"`
	TXPowerDBm      float64  `json:"tx_power_dbm,omitempty"`
	Clients         []Client `json:"clients"`
}

// Client is a station joined to the AP, with its lease when known.
type Client struct {
	MAC              string  `json:"mac"`
	SignalDBm        int     `json:"signal_dbm"`
	TXMbps           float64 `json:"tx_mbps"`
	RXMbps           float64 `json:"rx_mbps"`
	InactiveMS       uint32  `json:"inactive_ms"`
	ConnectedSeconds uint32  `json:"connected_s"`
	IP               string  `json:"ip,omitempty"`
	Hostname         string  `json:"hostname,omitempty"`
}

// ClientNet is the flat client network on this node (architecture §7).
type ClientNet struct {
	Anycast Anycast `json:"anycast"`
	Filters Filters `json:"filters"`
	DHCP    DHCP    `json:"dhcp"`
	Leases  []Lease `json:"leases"`
}

// Anycast is the gateway address every node answers for.
type Anycast struct {
	Interface string `json:"interface"`
	Present   bool   `json:"present"`
	Address   string `json:"address,omitempty"`
	MAC       string `json:"mac,omitempty"`
}

// Filters is the bridge-family nftables ruleset on bat0 and client ports.
type Filters struct {
	Applied bool              `json:"applied"`
	Rules   int               `json:"rules,omitempty"`
	Drops   map[string]uint64 `json:"drops,omitempty"` // frames dropped, by rule purpose
	Error   string            `json:"error,omitempty"`
}

// DHCP is this node's dnsmasq.
type DHCP struct {
	Dnsmasq   Helper `json:"dnsmasq"`
	PoolStart string `json:"pool_start,omitempty"`
	PoolEnd   string `json:"pool_end,omitempty"`
	Domain    string `json:"domain,omitempty"`
}

// Lease is a DHCP lease this node handed out.
type Lease struct {
	MAC      string    `json:"mac"`
	IP       string    `json:"ip"`
	Hostname string    `json:"hostname,omitempty"`
	Expires  time.Time `json:"expires"`
}

// APJoin is what a phone needs to join this node's AP.
type APJoin struct {
	Enabled    bool   `json:"enabled"`
	SSID       string `json:"ssid"`
	Passphrase string `json:"passphrase"`
	Band       string `json:"band,omitempty"`
	Channel    int    `json:"channel,omitempty"`
	WiFiQR     string `json:"wifi_qr"`
	// QR is WiFiQR as a QR code symbol, ready to draw.
	QR   *QR    `json:"qr,omitempty"`
	Note string `json:"note,omitempty"`
}

// QR is a QR code symbol: one string per row, '1' for a dark module and
// '0' for a light one, without the quiet zone (draw four light modules
// around it).
type QR struct {
	Size int      `json:"size"`
	Rows []string `json:"rows"`
}

// Topology is the mesh as a graph: every node this node knows from gossip
// and the radio or wired links between them. Each node reports its own
// links when asked (architecture §12.5), so the graph is only as fresh as
// the last round of answers.
type Topology struct {
	Time  time.Time  `json:"time"`
	Nodes []TopoNode `json:"nodes"`
	Links []TopoLink `json:"links"`
	// IntervalMS is how often this node asks the others, at most. It grows
	// with the mesh to keep the airtime small.
	IntervalMS int64 `json:"interval_ms"`
}

// TopoNode is a node in the topology. ID is its identity MAC; a neighbour
// that gossip has not named yet appears under its radio's MAC with an
// empty callsign.
type TopoNode struct {
	ID         string `json:"id"`
	Callsign   string `json:"callsign"`
	NodeID     int    `json:"node_id,omitempty"`
	Address    string `json:"address,omitempty"`
	Self       bool   `json:"self,omitempty"`
	Version    string `json:"version,omitempty"`
	Gateway    bool   `json:"gateway"`     // announces a WAN
	WANHealthy bool   `json:"wan_healthy"` // and it is healthy
	// Reported: the node answered a link query, so Clients, SelectedGateway
	// and its side of every link are known. ReportAgeMS is how old that
	// answer is.
	Reported    bool  `json:"reported"`
	ReportAgeMS int64 `json:"report_age_ms,omitempty"`
	Clients     int   `json:"clients"` // end-user devices on the node
	// SelectedGateway is the node ID of the gateway this node's clients
	// use; 0 for none.
	SelectedGateway int `json:"selected_gateway,omitempty"`
}

// TopoLink is one link between two nodes. A node pair with two mesh links
// (dual backhaul) has two entries.
type TopoLink struct {
	A string `json:"a"` // node IDs
	B string `json:"b"`
	// Throughput as each end estimates it towards the other (batman-adv),
	// in Mbit/s; 0 when that end has not reported.
	ABMbps float64 `json:"ab_mbps"`
	BAMbps float64 `json:"ba_mbps"`
	// Signal each end receives from the other, in dBm; 0 when unknown or
	// wired.
	ASignalDBm int    `json:"a_signal_dbm,omitempty"`
	BSignalDBm int    `json:"b_signal_dbm,omitempty"`
	Medium     string `json:"medium"` // wifi, wired, or site (a site link tunnel)
	AMAC       string `json:"a_mac"`  // each end's interface address
	BMAC       string `json:"b_mac"`
	// MeshLink is the mesh link the two meet on, numbered from 1 in the
	// profile's order (MeshLink.Index); 0 when wired or not reported.
	MeshLink int `json:"mesh_link,omitempty"`
}

// MeshNode is another node as gossip describes it.
type MeshNode struct {
	Callsign    string   `json:"callsign"`
	NodeID      int      `json:"node_id"`
	Address     string   `json:"address"`
	MAC         string   `json:"mac"`
	Originators []string `json:"originators,omitempty"`
	Version     string   `json:"version,omitempty"`
	// MeshRadios lists the bands of each of the node's radios that can
	// carry a mesh link, as the node reports them: [["2g","5g"],["s1g"]].
	// Absent for a node whose version does not report it.
	MeshRadios [][]string `json:"mesh_radios,omitempty"`
	// ChannelBusy is how busy each mesh link's channel is where that node
	// is, in percent, in the profile's order; -1 unknown.
	ChannelBusy []int `json:"channel_busy,omitempty"`
	// Degraded lists the node's radios that run but not as they should,
	// as it reports them; absent while all are fine.
	Degraded []NodeDegraded `json:"degraded,omitempty"`
	// Position is the node's GNSS fix as it last shared it; nil without a
	// fix, or when it does not share it (PositionOff).
	Position    *Position `json:"position,omitempty"`
	PositionOff bool      `json:"position_off,omitempty"`
	Gateway     bool      `json:"gateway"`     // announces a WAN
	WANHealthy  bool      `json:"wan_healthy"` // and it is healthy
	HeardAgoMS  int64     `json:"heard_ago_ms"`
	IDConflict  bool      `json:"id_conflict,omitempty"`
	// SiteKey is the node's site link key (its WireGuard public key):
	// what the profile's site_link.members lists it by.
	SiteKey string `json:"site_key,omitempty"`
	// SiteLink is the node's part in the site link, as it reports it:
	// nil when it is not a member, or runs a version that does not say.
	SiteLink *NodeSiteLink `json:"site_link,omitempty"`
}

// NodeDegraded is one degraded radio of another node: its bands, and the
// reason code (thermal: throttling to cool; pcie: its PCIe link is
// faulty; another code from a newer version).
type NodeDegraded struct {
	Bands  []string `json:"bands"`
	Reason string   `json:"reason"`
}

// NodeSiteLink is another node's part in the site link: its site, the
// members of other sites it tunnels to, and how many tunnels are up.
type NodeSiteLink struct {
	Site  string `json:"site"`
	Up    int    `json:"up"`
	Peers int    `json:"peers"`
}

// GatewayState is this node's gateway role and its choice of gateway
// (architecture §8).
type GatewayState struct {
	// Firewall is what the node takes from outside the mesh: "closed" or
	// "open" (firewall.wan), the ports open while closed ("tcp/22"), and
	// how many packets it has turned away.
	Firewall     string    `json:"firewall"`
	FirewallOpen []string  `json:"firewall_open,omitempty"`
	FirewallDrop uint64    `json:"firewall_dropped,omitempty"`
	Mode         string    `json:"mode"` // auto or off
	Role         string    `json:"role"` // gateway, client
	WAN          string    `json:"wan,omitempty"`
	WANHealthy   bool      `json:"wan_healthy"`
	WANKnown     bool      `json:"wan_known"`
	WANSince     time.Time `json:"wan_since,omitempty"`
	RTTms        float64   `json:"rtt_ms,omitempty"`
	LossPct      int       `json:"loss_pct"`
	ProbeError   string    `json:"probe_error,omitempty"`
	ProbeTargets []string  `json:"probe_targets,omitempty"`
	// WebCheck is the WAN's web check: ok, portal (behind a login page),
	// no-answer, checking; empty when it is off. WebCheckDetail says what
	// was seen.
	WebCheck       string             `json:"web_check,omitempty"`
	WebCheckDetail string             `json:"web_check_detail,omitempty"`
	WebCheckAt     time.Time          `json:"web_check_at,omitempty"`
	Announced      string             `json:"announced,omitempty"` // e.g. 20/5 Mbit/s
	Selected       *SelectedGateway   `json:"selected,omitempty"`
	Candidates     []GatewayCandidate `json:"candidates"`
	DNS            []string           `json:"dns"`
	Switches       int                `json:"switches"`
}

// SelectedGateway is where this node's clients reach the internet.
type SelectedGateway struct {
	Self           bool      `json:"self"`
	Callsign       string    `json:"callsign"`
	NodeID         int       `json:"node_id"`
	Address        string    `json:"address"`
	ThroughputMbps float64   `json:"throughput_mbps,omitempty"`
	Since          time.Time `json:"since"`
}

// GatewayCandidate is a gateway batman-adv announces, and whether it can be
// used.
type GatewayCandidate struct {
	Originator     string  `json:"originator"`
	Callsign       string  `json:"callsign,omitempty"`
	NodeID         int     `json:"node_id,omitempty"`
	Address        string  `json:"address,omitempty"`
	ThroughputMbps float64 `json:"throughput_mbps"`
	LastSeenMS     uint32  `json:"last_seen_ms"`
	Usable         bool    `json:"usable"`
	Why            string  `json:"why,omitempty"` // why not usable
}

// Port is a wired port: its configured role and, for auto, what it became.
type Port struct {
	Name       string `json:"name"`
	Configured string `json:"configured"` // role in node.yaml; "" for an unlisted port treated as auto
	// Role in effect: wan, client, mesh, or for auto ports also probing,
	// no-link, peer (another Trellis node is on the cable), in-use (the
	// system uses it; left alone) or missing.
	Role    string     `json:"role"`
	Why     string     `json:"why,omitempty"`
	Carrier bool       `json:"carrier"`
	Since   time.Time  `json:"since,omitzero"`
	Lease   *PortLease `json:"lease,omitempty"` // auto WAN: the lease meshd holds
}

// PortLease is a DHCP lease held by meshd on an auto WAN port.
type PortLease struct {
	Address string    `json:"address"` // CIDR
	Router  string    `json:"router,omitempty"`
	DNS     []string  `json:"dns,omitempty"`
	Server  string    `json:"server,omitempty"`
	Expires time.Time `json:"expires"`
}

// Gossip is this node's gossip traffic since meshd started (architecture
// §12.5). Records and bytes are gossip packets as sent or received on
// br-mesh, before batman-adv carries them.
type Gossip struct {
	Sent            uint64 `json:"sent"`
	Received        uint64 `json:"received"`
	BytesSent       uint64 `json:"bytes_sent"`
	BytesReceived   uint64 `json:"bytes_received"`
	LastRecordBytes int    `json:"last_record_bytes"`
	AuthFailed      uint64 `json:"auth_failed"`
	Ignored         uint64 `json:"ignored"`
	LastError       string `json:"last_error,omitempty"`
}

// Clock is how the node keeps time: chrony, following GNSS, the mesh's time
// servers, the internet, or only itself.
type Clock struct {
	// Service: "chrony", or "none" (chrony not installed: the node neither
	// serves nor follows mesh time).
	Service string `json:"service"`
	Synced  bool   `json:"synced"`
	// Source in words: "GNSS receiver", "relay-1 (10.73.12.1)", "internet
	// (203.0.113.5)", "this node's own clock".
	Source string `json:"source,omitempty"`
	// SourceKind: gnss, mesh, internet, local.
	SourceKind string  `json:"source_kind,omitempty"`
	Stratum    int     `json:"stratum,omitempty"`
	OffsetMS   float64 `json:"offset_ms"`
	// Serving: this node serves time to the mesh. MeshServers are the
	// nodes it follows, by callsign.
	Serving     bool     `json:"serving"`
	MeshServers []string `json:"mesh_servers,omitempty"`
	Note        string   `json:"note,omitempty"`
}

// GNSS is the node's GNSS receiver, through gpsd.
type GNSS struct {
	// Receiver: gpsd runs and lists a receiver.
	Receiver bool   `json:"receiver"`
	Device   string `json:"device,omitempty"`
	Driver   string `json:"driver,omitempty"`
	// Fix: "none", "2d" or "3d".
	Fix string `json:"fix"`
	// Position, only with a fix.
	Lat  *float64 `json:"lat,omitempty"`
	Lon  *float64 `json:"lon,omitempty"`
	AltM *float64 `json:"alt_m,omitempty"`
	// Estimated errors in metres; 0 when unknown.
	HAccM    float64 `json:"h_acc_m,omitempty"`
	VAccM    float64 `json:"v_acc_m,omitempty"`
	SatsUsed int     `json:"sats_used"`
	SatsSeen int     `json:"sats_seen"`
	// Signals are the strengths (C/N0, dB-Hz) of the satellites heard,
	// strongest first, at most 8: known before any fix.
	Signals []int `json:"signals,omitempty"`
	// FixAgeMS: since the receiver's last report.
	FixAgeMS int64 `json:"fix_age_ms,omitempty"`
	// PPS: pulse-per-second reports arrive (sub-millisecond time).
	PPS  bool   `json:"pps"`
	Note string `json:"note,omitempty"`
}

// Positions is position sharing on this node.
type Positions struct {
	// Shared: this node puts itself on the ATAK map now (sharing on and a
	// fix). Note says why not, or how.
	Shared bool   `json:"shared"`
	Note   string `json:"note,omitempty"`
	// Off: sharing is turned off, by "profile" (the whole mesh) or "node".
	Off string `json:"off,omitempty"`
	// Sent counts this node's position reports; LastSentAgoMS is since the
	// last one (-1 for none).
	Sent          uint64 `json:"sent"`
	LastSentAgoMS int64  `json:"last_sent_ago_ms"`
	// LastKnown are nodes that left the mesh, at their last shared
	// position. Announcing says this node re-announces them to ATAK (the
	// lowest-numbered node on the mesh does).
	LastKnown  []LastKnown `json:"last_known,omitempty"`
	Announcing bool        `json:"announcing,omitempty"`
}

// Position is a GNSS fix.
type Position struct {
	Lat  float64  `json:"lat"`
	Lon  float64  `json:"lon"`
	AltM *float64 `json:"alt_m,omitempty"`
	// AccM: horizontal error in metres, 0 when unknown.
	AccM int `json:"acc_m,omitempty"`
	// Fix: "2d" or "3d"; Time: the fix's time.
	Fix  string    `json:"fix"`
	Time time.Time `json:"time"`
}

// LastKnown is a node no longer on the mesh, where it last was.
type LastKnown struct {
	Callsign string    `json:"callsign"`
	MAC      string    `json:"mac"`
	Position Position  `json:"position"`
	Heard    time.Time `json:"heard"` // last record from it
}

// MeshChange is a profile change for the whole mesh: one node sends it to
// all the others, and every node switches to it at ApplyAt.
type MeshChange struct {
	Revision uint64    `json:"revision"`
	ApplyAt  time.Time `json:"apply_at"`
	// From is the node that sent it; Sender: this node did.
	From   string `json:"from"`
	Sender bool   `json:"sender"`
	// State: "pending" (switches at ApplyAt), "applied", or "failed".
	State string `json:"state"`
	Error string `json:"error,omitempty"`
	// Summary is the change in words: "5 GHz · ch 149 at 80 MHz".
	Summary string `json:"summary,omitempty"`
	// Nodes is where it stands on each other node, on the node that sent
	// it: "sent" (no answer yet), "has it", or "refused: why".
	Nodes []MeshChangeNode `json:"nodes,omitempty"`
}

// Rollout is a mesh-wide upgrade: a release on its way through the mesh,
// one node at a time.
type Rollout struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	// Origin is the node the operator gave the release to; Sender: this
	// node.
	Origin    string           `json:"origin"`
	Sender    bool             `json:"sender,omitempty"`
	Started   time.Time        `json:"started"`
	Cancelled bool             `json:"cancelled,omitempty"`
	Ended     *time.Time       `json:"ended,omitempty"`
	Packages  []RolloutPackage `json:"packages"`
	// Phase is this node's: fetching, ready, upgrading, done, skipped,
	// rolled-back, failed or halted; Reason why it skipped, failed or
	// halted; To the version it installs.
	Phase  string `json:"phase"`
	Reason string `json:"reason,omitempty"`
	To     string `json:"to,omitempty"`
	// Waiting: whose turn it is while this node is ready, or why it does
	// not have its package yet while it fetches.
	Waiting string `json:"waiting,omitempty"`
	// Nodes in the order they upgrade.
	Nodes []RolloutNode `json:"nodes"`
}

// RolloutPackage is the release for one architecture.
type RolloutPackage struct {
	Arch   string `json:"arch"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// RolloutNode is one node's progress in a rollout, as this node last heard
// it.
type RolloutNode struct {
	Position int       `json:"position"`
	MAC      string    `json:"mac"`
	Callsign string    `json:"callsign"`
	NodeID   int       `json:"node_id,omitempty"`
	Self     bool      `json:"self,omitempty"`
	Arch     string    `json:"arch,omitempty"`
	Version  string    `json:"version,omitempty"`
	Phase    string    `json:"phase,omitempty"` // empty: not heard on it yet
	Reason   string    `json:"reason,omitempty"`
	Since    time.Time `json:"since,omitzero"`
}

// MeshLinkVisit is a channel a mesh link visits to look for the rest of
// its mesh.
type MeshLinkVisit struct {
	Band     string `json:"band"`
	Channel  int    `json:"channel"`
	WidthMHz int    `json:"width_mhz"`
}

// MeshChangeNode is one node's answer to a mesh-wide change.
type MeshChangeNode struct {
	Callsign string `json:"callsign"`
	State    string `json:"state"`
}

// Survey is a mesh-wide channel survey: every node scanned the
// bands of its mesh links once, and the node that asked judged each link's
// channels.
type Survey struct {
	Started time.Time `json:"started"`
	// Done: every node answered, or the time to answer ran out.
	Done        bool                `json:"done"`
	Nodes       []SurveyNode        `json:"nodes"`
	Suggestions []ChannelSuggestion `json:"suggestions"`
}

// SurveyNode is one node's survey.
type SurveyNode struct {
	Callsign string `json:"callsign"`
	// State: "asked", "answered", "no answer", or "failed: why".
	State string       `json:"state"`
	Links []SurveyLink `json:"links,omitempty"`
}

// SurveyLink is one mesh link's band as a node surveyed it.
type SurveyLink struct {
	Link     int             `json:"link"` // from 1, in the profile's order
	Band     string          `json:"band"`
	Channels []SurveyChannel `json:"channels"`
	Error    string          `json:"error,omitempty"`
}

// SurveyChannel is one channel as one node found it.
type SurveyChannel struct {
	Ch int `json:"ch"`
	// Busy in percent; -1 when the driver does not measure it.
	Busy      int `json:"busy"`
	Noise     int `json:"noise,omitempty"` // dBm
	Nets      int `json:"nets,omitempty"`  // other networks heard
	Strongest int `json:"strongest,omitempty"`
}

// ChannelSuggestion judges one link's channels across the mesh.
type ChannelSuggestion struct {
	Link  int    `json:"link"`
	Name  string `json:"name"` // the link as it runs: "5 GHz · ch 149"
	Width int    `json:"width"`
	// Current is the link's channel; Best a clearly better one, when there
	// is one; Ranked the clearest first, radar channels included (marked).
	Current *ChannelScore  `json:"current,omitempty"`
	Best    *ChannelScore  `json:"best,omitempty"`
	Ranked  []ChannelScore `json:"ranked,omitempty"`
	Note    string         `json:"note"`
}

// ChannelScore is one candidate channel, judged by the worst node: lower
// is clearer.
type ChannelScore struct {
	Ch        int     `json:"ch"`
	Score     float64 `json:"score"`
	WorstBusy int     `json:"worst_busy"` // percent; -1 unknown
	WorstNode string  `json:"worst_node,omitempty"`
	Nets      int     `json:"nets"`
	Strongest int     `json:"strongest,omitempty"`
	DFS       bool    `json:"dfs"`
}
