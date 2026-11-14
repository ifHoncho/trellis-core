package meshd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"meridian/trellis/internal/api"
	"meridian/trellis/internal/config"
	"meridian/trellis/internal/doctor"
	"meridian/trellis/internal/nodestate"
	"meridian/trellis/internal/qr"
	"meridian/trellis/internal/radio"
	"meridian/trellis/internal/status"
)

// Configuration changes through the API (architecture §12.6). The API
// never applies a setting by itself: it validates the new document, writes
// the file meshd reads at start, and reloads. What is on disk and what
// runs therefore cannot drift apart, and a change that the reload rejects
// is rolled back.
//
// Every change can be tried first (dry run): the answer lists what it
// would do to the running node, so the UI can warn before a change that
// drops the mesh link or the operator's own connection (§5.2).

const nodeFileHeader = `Trellis node configuration (schema v0), one per node.
Written by meshd from the web UI or the API on %s. Only settings that
differ from the defaults are listed, and comments are not kept.
Every setting is described in docs/api.md and configs/examples/node.yaml.`

const profileFileHeader = `Trellis mesh profile. Share this file, its QR code or its profile code with
every node in the mesh. It holds the mesh secret: keep it private.
Written by meshd from the web UI or the API on %s.`

// defaultChannel is the channel a new mesh gets on each band when none is
// asked for: non-DFS, usable in the US and most of Europe.
var defaultChannel = map[string]int{config.Band2G: 6, config.Band5G: 149, config.Band6G: 37, config.BandS1G: 26}

func stamp() string { return time.Now().UTC().Format("2006-01-02 15:04 UTC") }

// fileChange is one configuration file to replace, with what it takes to
// put the old one back.
type fileChange struct {
	path string
	data []byte
	mode os.FileMode
	old  []byte
	had  bool
}

// writeFiles replaces the files, or none of them.
func writeFiles(changes []*fileChange) error {
	for i, c := range changes {
		old, err := os.ReadFile(c.path)
		c.old, c.had = old, err == nil
		if err := writeFileAtomic(c.path, c.data, c.mode); err != nil {
			restoreFiles(changes[:i])
			return fmt.Errorf("cannot write %s: %w", c.path, err)
		}
	}
	return nil
}

func restoreFiles(changes []*fileChange) {
	for _, c := range changes {
		if c.had {
			_ = writeFileAtomic(c.path, c.old, c.mode)
		} else {
			_ = os.Remove(c.path)
		}
	}
}

// commit writes the files and reloads. A reload that fails puts the old
// files back: the running configuration and the files stay in step.
func (d *Daemon) commit(changes ...*fileChange) error {
	if err := writeFiles(changes); err != nil {
		return err
	}
	if err := d.Reload(); err != nil {
		restoreFiles(changes)
		return fmt.Errorf("the new configuration was written but meshd refused it, so the old one was put back: %w", err)
	}
	return nil
}

func sameYAML(a, b interface{ YAML(string) ([]byte, error) }) bool {
	x, err1 := a.YAML("")
	y, err2 := b.YAML("")
	return err1 == nil && err2 == nil && bytes.Equal(x, y)
}

// SetNode replaces the node configuration with the JSON document raw.
func (d *Daemon) SetNode(ctx context.Context, raw []byte, dryRun bool) (*api.ApplyResult, error) {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	cur, profile := d.conf()
	setup := d.setupState()
	n, err := config.ParseNodeJSON(raw)
	if err != nil {
		return nil, err
	}
	// Secrets are redacted in what the API hands out; sent back unchanged
	// they mean "keep it".
	if n.AP.Passphrase == config.Redacted {
		n.AP.Passphrase = cur.AP.Passphrase
	}
	// Where the profile lives is the installation's business, not the API's.
	n.ProfilePath = cur.ProfilePath

	res := &api.ApplyResult{Changed: !sameYAML(*cur, *n) || !setup.NodeConfig, Effects: d.nodeEffects(cur, n, profile, setup.Needed)}
	if apChanged(cur, n) {
		res.AP = d.apJoinFor(n, setup.Needed)
	}
	if dryRun || !res.Changed {
		return res, nil
	}
	data, err := n.YAML(fmt.Sprintf(nodeFileHeader, stamp()))
	if err != nil {
		return nil, err
	}
	if err := d.commit(&fileChange{path: d.opts.NodePath, data: data, mode: 0o644}); err != nil {
		return nil, err
	}
	d.log.Info("node configuration replaced through the API", "path", d.opts.NodePath, "effects", len(res.Effects))
	res.Applied = true
	return res, nil
}

// profileFrom turns an API profile input into a validated profile.
func (d *Daemon) profileFrom(in api.ProfileInput, cur *config.Profile, setup bool) (*config.Profile, error) {
	forms := 0
	for _, set := range []bool{in.Code != "", in.YAML != "", in.Profile != nil, in.New != nil} {
		if set {
			forms++
		}
	}
	if forms != 1 {
		return nil, api.Invalid("give the profile in exactly one form: code, yaml, profile or new (got %d)", forms)
	}
	var p *config.Profile
	var err error
	switch {
	case in.Code != "":
		p, err = config.DecodeProfileCode(in.Code)
		if err != nil {
			var es config.Errors
			if !errors.As(err, &es) {
				err = api.Invalid("%v", err)
			}
		}
	case in.YAML != "":
		p, err = config.ParseProfile([]byte(in.YAML))
	case in.Profile != nil:
		cp := *in.Profile
		if cp.SAEPassphrase == config.Redacted {
			if setup {
				return nil, api.Invalid("this node has no mesh secret to keep yet: send the profile with its sae_passphrase")
			}
			cp.SAEPassphrase = cur.SAEPassphrase
		}
		cp.ApplyDefaults()
		p, err = &cp, cp.Validate()
	default:
		p, err = newProfile(*in.New)
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

// newProfile makes a profile for a new mesh, with a fresh secret.
func newProfile(in api.NewProfile) (*config.Profile, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, config.Errors{{Field: "name", Msg: "required"}}
	}
	raw, err := nodestate.GeneratePassphrase(24)
	if err != nil {
		return nil, err
	}
	// Groups of six are easier to read out and type.
	secret := raw[:6] + "-" + raw[6:12] + "-" + raw[12:18] + "-" + raw[18:]
	p := &config.Profile{
		Schema: config.SchemaVersion, Name: name, MeshID: "trellis-" + name, SAEPassphrase: secret,
		Regulatory: config.Regulatory{Mode: in.Mode, Country: strings.ToUpper(in.Country)},
		Radio:      config.RadioPlan{Band: in.Band, Channel: in.Channel, Width: in.Width},
		Links:      append([]config.RadioPlan(nil), in.Links...),
	}
	if p.Regulatory.Mode == "" {
		p.Regulatory.Mode = config.RegModeCountry
	}
	p.ApplyDefaults()
	if p.Radio.Channel == 0 {
		p.Radio.Channel = defaultChannel[p.Radio.Band]
	}
	for i := range p.Links {
		if p.Links[i].Channel == 0 {
			p.Links[i].Channel = defaultChannel[p.Links[i].Band]
		}
	}
	if len(p.MeshID) > 32 {
		p.MeshID = p.MeshID[:32]
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

// SetProfile installs a mesh profile.
func (d *Daemon) SetProfile(ctx context.Context, in api.ProfileInput, dryRun bool) (*api.ApplyResult, error) {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	cur, curProfile := d.conf()
	setup := d.setupState()
	p, err := d.profileFrom(in, curProfile, setup.Needed)
	if err != nil {
		return nil, err
	}
	res := &api.ApplyResult{Changed: setup.Needed || !sameYAML(*curProfile, *p), Effects: profileEffects(curProfile, p, setup.Needed, d.profileView(cur, curProfile, p))}
	if setup.Needed {
		res.AP = d.apJoinFor(cur, false)
	}
	if dryRun || !res.Changed {
		return res, nil
	}
	data, err := p.YAML(fmt.Sprintf(profileFileHeader, stamp()))
	if err != nil {
		return nil, err
	}
	path := d.profilePath(cur)
	if err := d.commit(&fileChange{path: path, data: data, mode: 0o600}); err != nil {
		return nil, err
	}
	d.log.Info("mesh profile installed through the API", "path", path, "mesh", p.MeshID, "band", p.Radio.Band, "channel", p.Radio.Channel)
	res.Applied = true
	return res, nil
}

// Setup provisions the node in one step: the mesh profile and the node's
// name, written together and applied in one reload.
func (d *Daemon) Setup(ctx context.Context, in api.SetupInput, dryRun bool) (*api.ApplyResult, error) {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	cur, curProfile := d.conf()
	setup := d.setupState()
	p, err := d.profileFrom(in.ProfileInput, curProfile, setup.Needed)
	if err != nil {
		return nil, err
	}
	n := *cur
	n.Callsign, n.NodeID = strings.TrimSpace(in.Callsign), in.NodeID
	n.ApplyDefaults()
	if err := n.Validate(); err != nil {
		return nil, err
	}
	res := &api.ApplyResult{Changed: true}
	res.Effects = append(profileEffects(curProfile, p, setup.Needed, d.profileView(&n, curProfile, p)), d.nodeEffects(cur, &n, p, false)...)
	sortEffects(res.Effects)
	res.AP = d.apJoinFor(&n, false)
	if dryRun {
		return res, nil
	}
	nodeData, err := n.YAML(fmt.Sprintf(nodeFileHeader, stamp()))
	if err != nil {
		return nil, err
	}
	profileData, err := p.YAML(fmt.Sprintf(profileFileHeader, stamp()))
	if err != nil {
		return nil, err
	}
	if err := d.commit(&fileChange{path: d.opts.NodePath, data: nodeData, mode: 0o644},
		&fileChange{path: d.profilePath(&n), data: profileData, mode: 0o600}); err != nil {
		return nil, err
	}
	d.log.Info("node provisioned through the API", "callsign", n.Callsign, "mesh", p.MeshID)
	res.Applied = true
	return res, nil
}

// ProfileExport returns the mesh profile with its secret, as a file, a
// profile code and that code's QR symbol.
func (d *Daemon) ProfileExport() (*api.ProfileExport, error) {
	if d.setupState().Needed {
		return nil, api.Errorf(409, "this node has no mesh profile yet: there is nothing to share")
	}
	_, p := d.conf()
	y, err := p.YAML("Trellis mesh profile. Share it with every node in the mesh.\nIt holds the mesh secret: keep it private.")
	if err != nil {
		return nil, err
	}
	code, err := p.Code()
	if err != nil {
		return nil, err
	}
	out := &api.ProfileExport{Profile: *p, YAML: string(y), Code: code}
	if c, err := qr.Encode(code, qr.M); err == nil {
		out.QR = &status.QR{Size: c.Size, Rows: c.Rows()}
	}
	return out, nil
}

// Doctor runs the host checks and returns the paste-able report.
func (d *Daemon) Doctor(ctx context.Context) (string, error) {
	rep := doctor.Run(doctor.Options{Version: d.opts.Version, NodePath: d.opts.NodePath, ProfilePath: d.opts.ProfilePath, Socket: d.opts.Socket})
	return rep.Text(), nil
}

// Effects -----------------------------------------------------------------------

func effect(level, code, format string, args ...any) status.Warning {
	return status.Warning{Level: level, Code: code, Message: fmt.Sprintf(format, args...)}
}

// noNil makes an empty list of effects encode as [] rather than null: the
// API promises a list.
func noNil(es []status.Warning) []status.Warning {
	if es == nil {
		return []status.Warning{}
	}
	return es
}

// sortEffects puts the most serious first, keeping the order within a level.
func sortEffects(es []status.Warning) {
	rank := map[string]int{"error": 0, "warning": 1, "info": 2}
	slices.SortStableFunc(es, func(a, b status.Warning) int { return rank[a.Level] - rank[b.Level] })
}

// apChanged reports whether joining the client AP differs between two
// node configurations.
func apChanged(a, b *config.Node) bool {
	ssid := func(n *config.Node) string {
		if n.AP.SSID != "" {
			return n.AP.SSID
		}
		return n.Callsign
	}
	return a.APEnabled() != b.APEnabled() || ssid(a) != ssid(b) || a.AP.Passphrase != b.AP.Passphrase
}

// previewPlan is the radio plan a node configuration would give with the
// radios present now, the way the radios reconciler would compute it.
func (d *Daemon) previewPlan(n *config.Node, p *config.Profile) *radio.Plan {
	if d.nl == nil {
		return nil
	}
	radios, err := radio.Inventory(d.nl, d.opts.SysRoot, d.hw, n.Radios)
	if err != nil {
		return nil
	}
	// As the reconciler does: roles stay where they are unless this
	// configuration asks for another plan than the one in force.
	var current map[string]radio.Role
	if cur, curProfile := d.conf(); planKey(cur, curProfile) == planKey(n, p) {
		current = map[string]radio.Role{}
		d.facts.Read(func(f *Facts) {
			for fp, role := range f.Roles {
				current[fp] = role
			}
		})
	}
	return radio.MakePlan(radio.Input{Radios: radios, MeshBand: p.Radio.Band, MoreBands: linkBands(p), LinkNames: p.LinkNames(), APEnabled: n.APEnabled(), Roles: n.Radios.Roles, Current: current})
}

// nodeEffects lists what replacing the node configuration cur with n does
// to the running node.
func (d *Daemon) nodeEffects(cur, n *config.Node, profile *config.Profile, setup bool) []status.Warning {
	var es []status.Warning
	add := func(level, code, format string, args ...any) { es = append(es, effect(level, code, format, args...)) }

	if cur.Callsign != n.Callsign {
		add("info", "renamed", "The node is renamed from %s to %s; on the mesh it becomes %s.mesh.", cur.Callsign, n.Callsign, n.Callsign)
	}
	if cur.NodeID != n.NodeID {
		if n.NodeID == 0 {
			add("info", "node-id", "The node ID is no longer pinned; the node keeps its current ID unless another node claims it.")
		} else if id := d.identity(); id == nil || id.NodeID != n.NodeID {
			add("warning", "node-id", "The node ID changes to %d: the node gets a new address, and devices on it are sent for new addresses (a short interruption for them).", n.NodeID)
		}
	}

	// Radios: compare the plan in force with the one this configuration gives.
	var radios []*radio.Radio
	var before *radio.Plan
	d.facts.Read(func(f *Facts) { radios, before = f.Radios, f.Plan })
	if after := d.previewPlan(n, profile); after != nil && before != nil {
		name := func(fp string) string {
			if r := radio.Find(radios, fp); r != nil {
				return r.Describe()
			}
			return fp
		}
		// The links the node would still carry, and its neighbours on each
		// link now: what a link going down costs.
		var left []string
		for i, lr := range after.Links {
			if l, ok := profile.Link(i); ok && lr != nil {
				left = append(left, linkName(l))
			}
		}
		peers := d.linkPeers(profile)
		linkIndex := 0
		link := func(title, was, now string) {
			i := linkIndex
			linkIndex++
			switch {
			case was == now:
			case now == "":
				drops := ""
				if i < len(peers) && len(peers[i]) == 1 {
					drops = fmt.Sprintf(" Its direct link there, to %s, drops.", peers[i][0])
				} else if i < len(peers) && len(peers[i]) > 1 {
					drops = fmt.Sprintf(" Its %d direct links there (%s) drop.", len(peers[i]), listAnd(peers[i]))
				}
				if len(left) > 0 {
					add("error", "mesh-link-down", "%s goes down on this node: no radio carries it any more.%s The node stays in the mesh over %s only, and reaches the nodes in range there. You lose this page if you reach it through a node that is not.", title, drops, listAnd(left))
				} else {
					add("error", "mesh-link-down", "%s goes down: no radio carries it any more.%s It is this node's only link: the node leaves the mesh, and you lose this page if you reach it through another node.", title, drops)
				}
			case was == "":
				add("info", "mesh-link-up", "%s comes up on %s.", title, name(now))
			default:
				add("warning", "mesh-link-moves", "%s moves from %s to %s and drops for a few seconds.", title, name(was), name(now))
			}
		}
		// Each link by its band and channel, as everywhere in the UI.
		title := func(i int) string {
			if l, ok := profile.Link(i); ok {
				return "The " + linkName(l) + " link"
			}
			return "The mesh link"
		}
		link(title(0), before.MeshFingerprint(), after.MeshFingerprint())
		for i := 1; i < max(len(before.Links), len(after.Links)); i++ {
			link(title(i), before.LinkFingerprint(i), after.LinkFingerprint(i))
		}
		was, now := before.APFingerprint(), after.APFingerprint()
		switch {
		case was == now:
		case now == "":
			add("warning", "ap-down", "The client Wi-Fi turns off. Devices on it are disconnected, and so are you if you are on it.")
		case was == "":
			add("info", "ap-up", "The client Wi-Fi comes up on %s.", name(now))
		default:
			add("warning", "ap-moves", "The client Wi-Fi moves from %s to %s; devices on it reconnect.", name(was), name(now))
		}
		// On a mesh radio the client Wi-Fi uses that link's channel: when
		// the radio changes link, the Wi-Fi changes band with it.
		sharedBand := func(pl *radio.Plan) string {
			if l, ok := profile.Link(pl.APLink); ok && pl.AP != nil {
				return l.Band
			}
			return ""
		}
		if a, b := sharedBand(before), sharedBand(after); was == now && a != "" && b != "" && a != b {
			add("warning", "ap-band", "The client Wi-Fi shares that radio and moves with it, from %s to %s: devices on it reconnect, and those that cannot use %s lose it.", bandName(a), bandName(b), bandName(b))
		}
		for _, nt := range after.Notices {
			if nt.Level == "error" {
				add("error", nt.Code, "%s", strings.ToUpper(nt.Message[:1])+nt.Message[1:])
			}
		}
	}

	if apChanged(cur, n) && n.APEnabled() && cur.APEnabled() {
		add("warning", "ap-credentials", "The client Wi-Fi's name or passphrase changes. Devices must join again with the new ones, and so must you if you are on it.")
	} else if n.APEnabled() && cur.APEnabled() && (cur.AP.Band != n.AP.Band || cur.AP.Channel != n.AP.Channel || cur.AP.Country != n.AP.Country || cur.AP.Isolate != n.AP.Isolate) {
		add("warning", "ap-restart", "The client Wi-Fi restarts with the new settings; devices on it reconnect.")
	}

	if cur.Gateway.Mode != n.Gateway.Mode {
		if n.Gateway.Mode == config.GatewayOff {
			add("warning", "gateway-off", "This node stops sharing its internet uplink with the mesh. Its own devices use another node's gateway, if there is one.")
		} else {
			add("info", "gateway-auto", "This node shares its internet uplink with the mesh whenever it has a healthy one.")
		}
	} else if cur.Gateway.WAN != n.Gateway.WAN || cur.Gateway.DownMbit != n.Gateway.DownMbit || cur.Gateway.UpMbit != n.Gateway.UpMbit ||
		!slices.Equal(cur.Gateway.ProbeTargets, n.Gateway.ProbeTargets) || cur.Gateway.ProbeIntervalMS != n.Gateway.ProbeIntervalMS {
		add("info", "gateway-settings", "The uplink settings change; the uplink is probed again before it is shared.")
	}

	if !slices.Equal(cur.Ports, n.Ports) {
		roles := func(ps []config.Port) map[string]string {
			m := map[string]string{}
			for _, p := range ps {
				m[p.Name] = p.Role
			}
			return m
		}
		was, now := roles(cur.Ports), roles(n.Ports)
		var names []string
		for name := range was {
			names = append(names, name)
		}
		for name := range now {
			if _, ok := was[name]; !ok {
				names = append(names, name)
			}
		}
		slices.Sort(names)
		for _, name := range names {
			a, b := was[name], now[name]
			if a == b {
				continue
			}
			if a == "" {
				a = "unlisted"
			}
			if b == "" {
				b = "unlisted"
			}
			level := "info"
			if a == config.PortRoleWAN || a == config.PortRoleMesh {
				level = "warning"
			}
			add(level, "port-role", "Port %s changes from %s to %s.", name, a, b)
		}
	}

	if cur.Web.WANAccess != n.Web.WANAccess {
		if n.Web.WANAccess {
			add("warning", "web-wan-access", "This page also becomes reachable from the node's uplink network, not only from the mesh. The admin password is then all that protects it there.")
		} else {
			add("warning", "web-wan-access", "This page stops answering on the node's uplink network. If that is how you reach it now, you lose it: use the mesh or one of the node's client ports instead.")
		}
	}
	if cur.MeshMTU != n.MeshMTU {
		add("warning", "mesh-mtu", "The mesh link's MTU changes; the link restarts.")
	}
	if cur.AllowOldBatman != n.AllowOldBatman && n.AllowOldBatman {
		add("warning", "old-batman", "The node may run an old batman-adv: with one in the mesh, every node falls back to flooding multicast.")
	}
	sortEffects(es)
	return noNil(es)
}

// ProfileView is the mesh as this node sees it when a profile change is
// weighed: the neighbours each of its links has, the radios it would have
// for the new profile's links, and what the other nodes say about theirs.
// The zero value (nothing known) gives the cautious answer.
type ProfileView struct {
	// Peers lists, per link of the current profile (0 is the first), the
	// nodes this node has an established direct link to: callsigns, or
	// addresses where the node is not known.
	Peers [][]string
	// Has reports whether this node has a radio on link i of the running
	// profile now. Nil when not known: every link is then taken as carried.
	Has func(i int) bool
	// Carries reports whether this node would have a radio for link i of
	// the new profile. Nil when the radios cannot be read: every link is
	// then taken as carried.
	Carries func(i int) bool
	// Wired: the node also meshes over wired ports, which no profile
	// change touches.
	Wired bool
	// Nodes are the other nodes known.
	Nodes []status.MeshNode
	// APBand is the band the client Wi-Fi runs on now, and APBandAfter the
	// one it would run on ("" when it is off or not known). It follows a
	// mesh link when it shares that link's radio.
	APBand, APBandAfter string
}

// profileView reads the live part of a profile change's effects.
func (d *Daemon) profileView(n *config.Node, cur, p *config.Profile) ProfileView {
	v := ProfileView{Wired: len(n.MeshPorts()) > 0, Nodes: d.meshNodes()}
	if plan := d.previewPlan(n, p); plan != nil {
		v.Carries = func(i int) bool { return plan.Link(i) != nil }
		if l, ok := p.Link(plan.APLink); ok && plan.AP != nil {
			v.APBandAfter = l.Band
		}
	}
	d.facts.Read(func(f *Facts) {
		if f.APShared && f.AP.Band != "" {
			v.APBand = f.AP.Band
		}
		if now := f.Plan; now != nil {
			v.Has = func(i int) bool { return now.Link(i) != nil }
		}
	})
	v.Peers = d.linkPeers(cur)
	return v
}

// linkPeers lists, per link of the running profile (0 is the first), the
// nodes this node has an established direct link to: callsigns, or radio
// addresses where the node is not known.
func (d *Daemon) linkPeers(cur *config.Profile) [][]string {
	var out [][]string
	var ifindex []int
	d.facts.Read(func(f *Facts) {
		ifindex = append(ifindex, f.MeshIfindex)
		for _, l := range f.MoreLinks {
			ifindex = append(ifindex, l.Ifindex)
		}
	})
	for i := 0; i <= len(cur.Links); i++ {
		var names []string
		if d.nl != nil && i < len(ifindex) && ifindex[i] != 0 {
			if stations, err := d.nl.Stations(ifindex[i]); err == nil {
				for _, st := range stations {
					if !st.Established() {
						continue
					}
					name := st.MAC.String()
					if d.peers != nil {
						if peer, ok := d.peers.ByOriginator(name); ok && peer.Record.Callsign != "" {
							name = peer.Record.Callsign
						}
					}
					names = append(names, name)
				}
			}
		}
		slices.Sort(names)
		out = append(out, names)
	}
	return out
}

// ProfileEffects is profileEffects for a caller outside the daemon (the
// development server, which shows the same check over made-up data).
func ProfileEffects(cur, p *config.Profile, setup bool, v ProfileView) []status.Warning {
	return profileEffects(cur, p, setup, v)
}

// linkDesc is a link's channel in words: "5 GHz channel 149 (80 MHz)".
func linkDesc(l config.RadioPlan) string {
	return fmt.Sprintf("%s channel %d (%d MHz)", bandName(l.Band), l.Channel, l.Width)
}

func linkName(l config.RadioPlan) string { return l.Name() }

// listAnd joins names for a sentence: "a", "a and b", "a, b and c"; past
// six it counts the rest.
func listAnd(names []string) string {
	if len(names) > 6 {
		names = append(append([]string{}, names[:5]...), fmt.Sprintf("%d more", len(names)-5))
	}
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// directLinks is "its 3 direct links (a, b and c)" or "its direct link to a".
func directLinks(names []string) string {
	if len(names) == 1 {
		return "its direct link to " + names[0]
	}
	return fmt.Sprintf("its %d direct links (%s)", len(names), listAnd(names))
}

// canCarry reports whether a node with these radios (the bands of each)
// has one for the band.
func canCarry(radios [][]string, band string) bool {
	for _, bands := range radios {
		if slices.Contains(bands, band) {
			return true
		}
	}
	return false
}

// profileEffects lists what replacing the mesh profile cur with p does on
// this node. A profile is installed on one node at a time, so the effects
// are about this node's links to the others, which keep their profile: a
// link both profiles have, and that this node still has a radio for, goes
// on working; the neighbours on any other link are lost until they change
// too.
func profileEffects(cur, p *config.Profile, setup bool, v ProfileView) []status.Warning {
	var es []status.Warning
	add := func(level, code, format string, args ...any) { es = append(es, effect(level, code, format, args...)) }
	const farthestFirst = "install it on every node, the farthest first, or this one is cut off from the rest (and from you, if you reach it through the mesh)"
	meshChange := false // something every node has to agree on changes
	if setup {
		var on []string
		for _, l := range p.AllLinks() {
			on = append(on, linkDesc(l))
		}
		add("info", "joins-mesh", "The node joins the mesh %q on %s and leaves setup mode.", p.Name, listAnd(on))
		add("warning", "ap-credentials", "The setup Wi-Fi goes away. The node's own client Wi-Fi replaces it under a new name and passphrase: note them before applying if that is how you are connected.")
	} else {
		was, now := cur.AllLinks(), p.AllLinks()
		peers := func(i int) []string {
			if i < len(v.Peers) {
				return v.Peers[i]
			}
			return nil
		}
		carries := func(j int) bool { return v.Carries == nil || v.Carries(j) }
		var what []string
		if cur.MeshID != p.MeshID {
			what = append(what, "the mesh ID")
		}
		if cur.SAEPassphrase != p.SAEPassphrase {
			what = append(what, "the mesh secret")
		}

		// Each link of the running profile: does this node keep it?
		var lost, kept []string                 // neighbours on links that go, and on links that stay
		var gone, noRadio, stay, moved []string // those links, described
		for i, l := range was {
			j := slices.Index(now, l)
			switch {
			case len(what) > 0:
				lost = append(lost, peers(i)...)
			case j < 0 && v.Has != nil && !v.Has(i):
				// A link this node has no radio for: nothing of its own
				// goes with it.
			case j < 0:
				gone = append(gone, linkDesc(l))
				lost = append(lost, peers(i)...)
			case !carries(j):
				noRadio = append(noRadio, linkDesc(l))
				lost = append(lost, peers(i)...)
			default:
				stay = append(stay, linkDesc(l))
				kept = append(kept, peers(i)...)
				if j != i {
					moved = append(moved, fmt.Sprintf("%s moves from place %d to %d", linkName(l), i+1, j+1))
				}
			}
		}
		slices.Sort(lost)
		slices.Sort(kept)
		var added []int // links of the new profile the running one lacks
		for j, l := range now {
			if !slices.Contains(was, l) {
				added = append(added, j)
			}
		}
		drops := ""
		if len(lost) > 0 {
			drops = " " + strings.ToUpper(directLinks(lost)[:1]) + directLinks(lost)[1:] + " drop"
			if len(lost) == 1 {
				drops += "s"
			}
			drops += " until the other end has the same profile."
		}
		switch {
		case len(what) > 0:
			meshChange = true
			add("error", "leaves-mesh", "This changes %s.%s The node then only reaches nodes that have the same profile: %s.", listAnd(what), drops, farthestFirst)
		case len(gone)+len(noRadio) > 0:
			meshChange = true
			off := listAnd(append(append([]string{}, gone...), noRadio...))
			onto := ""
			if len(added) > 0 {
				var to []string
				for _, j := range added {
					to = append(to, linkDesc(now[j]))
				}
				onto = " and puts it on " + listAnd(to)
			}
			switch {
			case len(stay) == 0 && !v.Wired:
				add("error", "leaves-mesh", "This takes the node off %s%s.%s It then only reaches nodes that have the same profile: %s.", off, onto, drops, farthestFirst)
			case len(lost) > 0 && len(kept) == 0 && !v.Wired:
				add("error", "leaves-mesh", "This takes the node off %s%s.%s It keeps %s, but has no neighbour there now: it is cut off from the rest (and from you, if you reach it through the mesh) until the other nodes have this profile too.", off, onto, drops, listAnd(stay))
			default:
				over := "its wired mesh ports"
				if len(stay) > 0 {
					over = listAnd(stay)
					if len(kept) > 0 {
						over += fmt.Sprintf(", where it has %s", strings.TrimPrefix(directLinks(kept), "its "))
					}
				}
				add("warning", "links-drop", "This takes the node off %s%s.%s It stays in the mesh over %s.", off, onto, drops, over)
			}
			if len(noRadio) > 0 {
				add("info", "link-no-radio", "The profile keeps %s, but with the new link order this node has no radio left for it.", listAnd(noRadio))
			}
		}
		if len(what) == 0 {
			// Who cannot follow: a node with no radio for any link of the
			// new profile would be left out of a mesh that runs it.
			var cannot, unknown []string
			if len(gone)+len(added) > 0 {
				for _, n := range v.Nodes {
					switch {
					case n.MeshRadios == nil:
						unknown = append(unknown, n.Callsign)
					case !slices.ContainsFunc(now, func(l config.RadioPlan) bool { return canCarry(n.MeshRadios, l.Band) }):
						cannot = append(cannot, n.Callsign)
					}
				}
				slices.Sort(cannot)
				slices.Sort(unknown)
			}
			for _, j := range added {
				meshChange = true
				l := now[j]
				msg := fmt.Sprintf("New link: %s.", linkDesc(l))
				if len(gone)+len(noRadio) > 0 && len(now) == 1 {
					msg = "" // the one link's move is told above
				} else if carries(j) {
					msg += " It comes up on this node, and reaches the nodes that have this profile and a radio for it."
				} else {
					msg += " This node has no radio free for it, and goes on without it."
				}
				// Nodes that stay in the mesh but have no radio for this link.
				var without []string
				for _, n := range v.Nodes {
					if n.MeshRadios != nil && !canCarry(n.MeshRadios, l.Band) && !slices.Contains(cannot, n.Callsign) {
						without = append(without, n.Callsign)
					}
				}
				if slices.Sort(without); len(without) > 0 {
					msg += fmt.Sprintf(" No %s mesh radio on %s: %s without this link.", bandName(l.Band), listAnd(without), map[bool]string{true: "it stays", false: "they stay"}[len(without) == 1])
				}
				if msg != "" {
					add("info", "link-added", "%s", strings.TrimSpace(msg))
				}
			}
			if len(cannot) > 0 {
				var bands []string
				for _, l := range now {
					if b := bandName(l.Band); !slices.Contains(bands, b) {
						bands = append(bands, b)
					}
				}
				verb := map[bool]string{true: "has", false: "have"}[len(cannot) == 1]
				add("warning", "nodes-cannot-follow", "%s %s no mesh radio for %s: a mesh that runs this profile leaves %s out, whatever is installed there.", listAnd(cannot), verb, listAnd(bands),
					map[bool]string{true: "it", false: "them"}[len(cannot) == 1])
			}
			if len(unknown) > 0 && len(gone) > 0 {
				add("info", "nodes-unknown-radios", "Not known whether %s can follow: %s not report its radios (an older version).", listAnd(unknown),
					map[bool]string{true: "it does", false: "they do"}[len(unknown) == 1])
			}
			if len(moved) > 0 {
				meshChange = true
				add("warning", "link-restarts", "The link order changes: %s. Each restarts, and is away for a few seconds.", listAnd(moved))
			}
		}
		if v.APBand != "" && v.APBandAfter != "" && v.APBand != v.APBandAfter {
			add("warning", "ap-band", "The client Wi-Fi shares the mesh radio and moves with it, from %s to %s: devices on it reconnect, and those that cannot use %s lose it.", bandName(v.APBand), bandName(v.APBandAfter), bandName(v.APBandAfter))
		}
		if cur.Prefix != p.Prefix {
			meshChange = true
			add("error", "prefix", "The mesh prefix changes from %s to %s: every node and device gets a new address, and nodes on the old prefix cannot reach those on the new one.", cur.Prefix, p.Prefix)
		}
		if cur.Name != p.Name {
			add("info", "mesh-name", "The mesh is renamed from %q to %q (a label only).", cur.Name, p.Name)
		}
		if cur.Batman != p.Batman && fmt.Sprint(cur.Batman.BLA(), deref(cur.Batman.MulticastFanout), cur.Batman.OrigIntervalMS, cur.Batman.MinVersion, cur.Batman.PinnedVersion) !=
			fmt.Sprint(p.Batman.BLA(), deref(p.Batman.MulticastFanout), p.Batman.OrigIntervalMS, p.Batman.MinVersion, p.Batman.PinnedVersion) {
			meshChange = true
			add("info", "batman-settings", "batman-adv settings change; they should be the same on every node.")
		}
	}
	if setup || cur.Regulatory != p.Regulatory {
		meshChange = meshChange || !setup
		if p.Regulatory.Mode == config.RegModeUnrestricted {
			add("warning", "regulatory", "Regulatory mode is unrestricted: radios use every channel and the full power the hardware allows. That is lawful only with frequency authorization; without it, use country mode.")
		} else if !setup {
			add("info", "regulatory", "Regulatory mode is country (%s): radios follow that country's channel and power limits.", p.Regulatory.Country)
		}
	}
	if meshChange && len(v.Nodes) > 0 {
		var names []string
		for _, n := range v.Nodes {
			names = append(names, n.Callsign)
		}
		slices.Sort(names)
		one := len(names) == 1
		add("info", "this-node-only", "Only this node changes. The %s of this mesh (%s) %s until the profile is installed on %s.",
			map[bool]string{true: "other node", false: fmt.Sprintf("%d other nodes", len(names))}[one], listAnd(names),
			map[bool]string{true: "keeps its own", false: "keep theirs"}[one], map[bool]string{true: "it too", false: "each of them"}[one])
	}
	sortEffects(es)
	return noNil(es)
}

func deref(p *int) int {
	if p == nil {
		return -1
	}
	return *p
}

func bandName(b string) string { return config.BandName(b) }
