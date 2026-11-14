package radio

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"meridian/trellis/internal/config"
	"meridian/trellis/internal/hwdb"
	"meridian/trellis/internal/sysfs"
)

// Role is what a radio does: "mesh" (the first mesh link), "mesh2", "mesh3",
// … (the profile's further links), "ap", a link with "+ap" when the client
// AP shares its radio, or "unused".
type Role string

// Roles.
const (
	RoleMesh   Role = "mesh"
	RoleAP     Role = "ap"
	RoleMeshAP Role = "mesh+ap"
	// Further links have no constant: see LinkRole ("mesh2", "mesh3+ap", …).
	RoleUnused Role = "unused"
)

// LinkRole is the role of the radio carrying mesh link i (0 is the first),
// with the client AP on the same radio or not.
func LinkRole(i int, ap bool) Role {
	if ap {
		return Role(config.LinkRoleKey(i) + "+ap")
	}
	return Role(config.LinkRoleKey(i))
}

// Link is the mesh link a role carries (0 is the first), or -1 for none.
func (r Role) Link() int {
	if i, ok := config.LinkOfRoleKey(strings.TrimSuffix(string(r), "+ap")); ok {
		return i
	}
	return -1
}

// HasAP reports whether the role includes the client AP.
func (r Role) HasAP() bool { return r == RoleAP || strings.HasSuffix(string(r), "+ap") }

// Notice is a warning or error produced while planning.
type Notice struct {
	Level   string `json:"level"` // info, warning, error
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Input is everything the planner needs.
type Input struct {
	Radios   []*Radio
	MeshBand string // the band of the first mesh link (profile radio)
	// MoreBands are the bands of the profile's further links, in order:
	// MoreBands[0] is the second link's.
	MoreBands []string
	APEnabled bool
	Roles     config.RoleOverrides
	// Current is the active assignment (fingerprint → role), or nil at boot.
	// With Current set, roles only change when a radio is lost: a newly
	// present radio only fills an empty role.
	Current map[string]Role
}

// bands lists every link's band, the first link's first.
func (in Input) bands() []string {
	return append([]string{in.MeshBand}, in.MoreBands...)
}

// Plan is the result: which radio carries which role.
type Plan struct {
	// Mesh is the first link's radio (Links[0]), or nil.
	Mesh *Radio `json:"-"`
	// Links has one entry per profile link, in order: the radio that carries
	// it, or nil when this node has none for it.
	Links []*Radio `json:"-"`
	AP    *Radio   `json:"-"`
	// APLink is the link whose radio the AP shares, or -1.
	APLink      int             `json:"-"`
	Roles       map[string]Role `json:"roles"` // fingerprint → role, for every radio present
	APBand      string          `json:"ap_band"`
	Notices     []Notice        `json:"notices"`
	Recommended map[string]Role `json:"recommended"` // the boot-time result for the radios present
	Differs     bool            `json:"recommendation_differs"`
}

func (p *Plan) note(level, code, format string, args ...any) {
	p.Notices = append(p.Notices, Notice{Level: level, Code: code, Message: fmt.Sprintf(format, args...)})
}

// Link is the radio carrying mesh link i, or nil.
func (p *Plan) Link(i int) *Radio {
	if p == nil || i < 0 || i >= len(p.Links) {
		return nil
	}
	return p.Links[i]
}

// LinkFingerprint is the fingerprint of the radio carrying link i, or "".
func (p *Plan) LinkFingerprint(i int) string {
	if r := p.Link(i); r != nil {
		return r.Fingerprint
	}
	return ""
}

// MeshFingerprint is the mesh radio's fingerprint or "".
func (p *Plan) MeshFingerprint() string { return p.LinkFingerprint(0) }

// hasLink reports whether any mesh link has a radio: a node is in the mesh
// over whichever links it can carry, the first or not.
func (p *Plan) hasLink() bool {
	for _, lr := range p.Links {
		if lr != nil {
			return true
		}
	}
	return false
}

// linkOf is the link a radio carries, or -1.
func (p *Plan) linkOf(r *Radio) int {
	for i, lr := range p.Links {
		if lr != nil && lr == r {
			return i
		}
	}
	return -1
}

// APFingerprint is the AP radio's fingerprint or "".
func (p *Plan) APFingerprint() string {
	if p.AP == nil {
		return ""
	}
	return p.AP.Fingerprint
}

// APBandFor picks the AP band: 5 GHz unless the mesh is there, then 2.4 GHz
// (architecture §10: always a different band from a standard Wi-Fi mesh).
func APBandFor(meshBand string) string {
	if meshBand == config.Band5G {
		return config.Band2G
	}
	return config.Band5G
}

// apBandFor is APBandFor for several mesh links: the first of 5 GHz and
// 2.4 GHz that no link uses. When the links use both, the first link
// decides, as it does alone.
func apBandFor(linkBands []string) string {
	for _, b := range []string{config.Band5G, config.Band2G} {
		if !slices.Contains(linkBands, b) {
			return b
		}
	}
	return APBandFor(linkBands[0])
}

func busRank(d sysfs.Device) int {
	switch d.Bus {
	case sysfs.BusPCI:
		return 3
	case sysfs.BusUSB:
		if d.USBSpeedMbps >= 5000 {
			return 2
		}
		return 1
	}
	return 0
}

func (in Input) meshEligible(r *Radio) (bool, string) {
	return in.meshEligibleFor(r, in.MeshBand, false)
}

// meshEligibleFor checks a radio for a mesh link on one band. explicit is true when the
// node config pins the role to this radio, which overrides InUse. InUse is
// checked last so that a radio which could never carry the mesh is reported
// for that reason, not with advice to pin it.
func (in Input) meshEligibleFor(r *Radio, band string, explicit bool) (bool, string) {
	switch {
	case r.Ignored:
		return false, "ignored by config"
	case r.Broken != "":
		return false, r.Broken
	case r.HardBlocked:
		return false, "blocked by a hardware rf-kill switch"
	case !r.Caps.MeshPoint:
		return false, "no mesh point support"
	case !r.HasBand(band):
		return false, "no " + band + " band"
	case r.InUse != "" && !explicit:
		return false, "in use by the system (" + r.InUse + "); pin the role to it in node.yaml to take it over"
	}
	return true, ""
}

func (in Input) apEligible(r *Radio, apBand string) (bool, string) {
	return in.apEligibleAs(r, apBand, false)
}

func (in Input) apEligibleAs(r *Radio, apBand string, explicit bool) (bool, string) {
	switch {
	case r.Ignored:
		return false, "ignored by config"
	case r.HardBlocked:
		return false, "blocked by a hardware rf-kill switch"
	case !r.Caps.AP:
		return false, "no AP support"
	case !r.HasBand(apBand) && !r.HasBand(config.Band2G) && !r.HasBand(config.Band5G):
		return false, "no 2.4 or 5 GHz band"
	case r.InUse != "" && !explicit:
		return false, "in use by the system (" + r.InUse + "); pin the role to it in node.yaml to take it over"
	}
	return true, ""
}

func power(r *Radio, band string) float64 {
	if p, ok := r.Power[band]; ok {
		return p.DBm
	}
	return -1
}

// meshTrust ranks how well mesh point is known to work on a radio. Driver
// power figures can't break this tie: some drivers report the regulatory
// limit, so an untested adapter would otherwise beat a proven one.
func meshTrust(r *Radio) int {
	if r.HWDB == nil {
		return 1
	}
	switch r.HWDB.MeshPoint {
	case hwdb.Verified:
		return 3
	case hwdb.Reported:
		return 2
	}
	return 1
}

// rankMesh orders mesh candidates best first (architecture §5.2 table), after
// first preferring radios whose mesh support is better established.
func rankMesh(cands []*Radio, band string) {
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if ta, tb := meshTrust(a), meshTrust(b); ta != tb {
			return ta > tb
		}
		if pa, pb := power(a, band), power(b, band); pa != pb {
			return pa > pb
		}
		if a.Caps.Streams != b.Caps.Streams {
			return a.Caps.Streams > b.Caps.Streams
		}
		if ra, rb := busRank(a.Device), busRank(b.Device); ra != rb {
			return ra > rb
		}
		return a.Fingerprint < b.Fingerprint
	})
}

// rankAP orders AP candidates best first: lowest power, then supports the
// preferred AP band, then fingerprint.
func rankAP(cands []*Radio, apBand string) {
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if ha, hb := a.HasBand(apBand), b.HasBand(apBand); ha != hb {
			return ha
		}
		if pa, pb := power(a, apBand), power(b, apBand); pa != pb {
			return pa < pb
		}
		if ha, hb := a.HasBand(config.Band5G), b.HasBand(config.Band5G); ha != hb {
			return ha
		}
		return a.Fingerprint < b.Fingerprint
	})
}

// MakePlan assigns roles.
func MakePlan(in Input) *Plan {
	p := plan(in)
	if in.Current != nil {
		fresh := in
		fresh.Current = nil
		rec := plan(fresh)
		p.Recommended = rec.Roles
		for fp, role := range rec.Roles {
			if p.Roles[fp] != role {
				p.Differs = true
			}
		}
	} else {
		p.Recommended = p.Roles
	}
	return p
}

// linkName is how notices call mesh link i: "mesh" for the first, as ever,
// then "mesh link 2", "mesh link 3", …
func linkName(i int) string {
	if i == 0 {
		return "mesh"
	}
	return fmt.Sprintf("mesh link %d", i+1)
}

func plan(in Input) *Plan {
	bands := in.bands()
	p := &Plan{Roles: map[string]Role{}, APBand: apBandFor(bands), Links: make([]*Radio, len(bands)), APLink: -1}
	for _, r := range in.Radios {
		p.Roles[r.Fingerprint] = RoleUnused
	}
	if len(in.Radios) == 0 {
		if in.Roles.Mesh.Mode == config.RoleDisabled {
			p.note("info", "no-radios", "no Wi-Fi radios found (mesh role disabled; wired ports only)")
		} else {
			p.note("error", "no-radios", "no Wi-Fi radios found")
		}
		return p
	}

	p.planLinks(in, bands)
	p.Mesh = p.Links[0]
	used := func(r *Radio) bool { return p.linkOf(r) >= 0 }

	// AP ----------------------------------------------------------------------
	if !in.APEnabled {
		p.note("info", "ap-disabled", "client AP disabled in node config")
	} else {
		var apCands []*Radio
		for _, r := range in.Radios {
			if used(r) {
				continue
			}
			if ok, _ := in.apEligible(r, p.APBand); ok {
				apCands = append(apCands, r)
			}
		}
		rankAP(apCands, p.APBand)

		switch in.Roles.AP.Mode {
		case config.RoleDisabled:
			p.note("info", "ap-disabled", "AP role disabled in node config")
		case "radio", "port":
			var pinned *Radio
			if in.Roles.AP.Mode == "radio" {
				pinned = Find(in.Radios, in.Roles.AP.Radio)
			} else {
				// Of a card's radios in that port: one free of mesh links
				// that can run the AP, on the AP's band if there is one.
				pinned = FindByPort(in.Radios, in.Roles.AP.Port, func(r *Radio) bool {
					ok, _ := in.apEligibleAs(r, p.APBand, true)
					return ok && !used(r) && r.HasBand(p.APBand)
				})
				if pinned == nil || used(pinned) {
					if free := FindByPort(in.Radios, in.Roles.AP.Port, func(r *Radio) bool {
						ok, _ := in.apEligibleAs(r, p.APBand, true)
						return ok && !used(r)
					}); free != nil {
						pinned = free
					}
				}
			}
			switch {
			case pinned == nil:
				p.note("error", "ap-pin-missing", "AP is pinned to a radio that is not present; no AP")
			case used(pinned):
				if in.canShareAP(pinned) {
					p.AP = pinned
				} else {
					p.note("error", "ap-pin-conflict", "AP is pinned to the mesh radio %s, which cannot run both at once", pinned.Describe())
				}
			default:
				if ok, why := in.apEligibleAs(pinned, p.APBand, true); !ok {
					p.note("error", "ap-pin-ineligible", "AP is pinned to %s, which cannot carry it: %s", pinned.Describe(), why)
				} else {
					p.AP = pinned
				}
			}
		default: // auto
			if cur := currentHolder(in, RoleAP); cur != nil && !used(cur) {
				if ok, _ := in.apEligible(cur, p.APBand); ok {
					p.AP = cur
				}
			}
			if p.AP == nil && len(apCands) > 0 {
				p.AP = apCands[0]
			}
			if p.AP == nil && p.hasLink() {
				// Share a mesh radio: the first link's that can, in order.
				for i, lr := range p.Links {
					if lr == nil || !in.canShareAP(lr) {
						continue
					}
					p.AP = lr
					if i == 0 {
						p.note("warning", "ap-shares-mesh-radio", "no free radio: the client AP shares the mesh radio, channel and airtime")
					} else {
						p.note("warning", "ap-shares-mesh-radio", "no free radio: the client AP shares the radio, channel and airtime of %s", linkName(i))
					}
					break
				}
				if p.AP == nil {
					p.note("warning", "no-ap-radio", "no radio left for the client AP: connect the EUD by cable")
				}
			}
		}
		if p.AP != nil {
			p.APLink = p.linkOf(p.AP)
		}
		if p.AP != nil && p.APLink < 0 && !p.AP.HasBand(p.APBand) {
			other := config.Band2G
			if p.APBand == config.Band2G {
				other = config.Band5G
			}
			p.note("warning", "ap-band-fallback", "AP radio %s has no %s band; the AP will use %s, the same band as the mesh", p.AP.Describe(), p.APBand, other)
		}
	}

	for _, r := range in.Radios {
		if r.InUse != "" && !used(r) && r != p.AP {
			p.note("info", "radio-in-use", "%s left alone: %s", r.Describe(), r.InUse)
		}
	}

	// Roles map ------------------------------------------------------------------
	for i, lr := range p.Links {
		if lr != nil {
			p.Roles[lr.Fingerprint] = LinkRole(i, false)
		}
	}
	if p.AP != nil {
		if p.APLink >= 0 {
			p.Roles[p.AP.Fingerprint] = LinkRole(p.APLink, true)
		} else {
			p.Roles[p.AP.Fingerprint] = RoleAP
		}
	}
	unused := 0
	for _, r := range in.Radios {
		if p.Roles[r.Fingerprint] == RoleUnused && !r.Ignored {
			unused++
		}
	}
	if unused > 0 && p.hasLink() && (p.AP != nil || !in.APEnabled) {
		p.note("info", "radios-unused", "%d radio(s) unused: no role left for them", unused)
	}
	return p
}

// canShareAP reports whether a radio that carries a mesh link can run the
// client AP as well: both at once, and a band phones use. (A HaLow radio
// advertises AP mode too, but no phone would find it.)
func (in Input) canShareAP(r *Radio) bool {
	return r.Caps.MeshAndAP && r.Caps.AP && (r.HasBand(config.Band2G) || r.HasBand(config.Band5G))
}

// planLinks gives each mesh link a radio that has its band, one radio per
// link. Links come before the AP, which can share a mesh radio; a link no
// radio is left for only leaves this node without that link.
//
// Order of claims: links the node config pins, then links whose radio is
// already carrying them (roles don't move while the node runs), then the
// rest. The rest are matched so that as many links as possible get a radio:
// each link takes its best-ranked free radio, and an earlier link moves to
// its next choice when that is the only way a later link gets one. With a
// dual-band radio and a 2.4 GHz-only one, links on 5 GHz and 2.4 GHz both
// come up, whichever the profile lists first.
func (p *Plan) planLinks(in Input, bands []string) {
	owner := map[*Radio]int{} // radio → link
	fixed := map[*Radio]bool{}
	var auto []int
	for i, band := range bands {
		key := config.LinkRoleKey(i)
		sel := in.Roles.Link(i)
		switch sel.Mode {
		case config.RoleDisabled:
			if i == 0 {
				p.note("info", "mesh-disabled", "mesh role disabled in node config")
			} else {
				p.note("info", key+"-disabled", "%s (%s) disabled in node config", linkName(i), band)
			}
		case "radio", "port":
			var pinned *Radio
			if sel.Mode == "radio" {
				pinned = Find(in.Radios, sel.Radio)
			} else {
				// Of a card's radios in that port: the one that can carry
				// this link (its band) and no other link has.
				pinned = FindByPort(in.Radios, sel.Port, func(r *Radio) bool {
					_, taken := owner[r]
					ok, _ := in.meshEligibleFor(r, band, true)
					return ok && !taken
				})
			}
			if pinned == nil {
				p.note("error", key+"-pin-missing", "%s is pinned to %s%s, which is not present", linkName(i), sel.Mode+" ", sel.Radio+sel.Port)
				continue
			}
			if j, taken := owner[pinned]; taken {
				p.note("error", key+"-pin-conflict", "%s is pinned to %s, the radio %s is pinned to", linkName(i), pinned.Describe(), linkName(j))
				continue
			}
			if ok, why := in.meshEligibleFor(pinned, band, true); !ok {
				p.note("error", key+"-pin-ineligible", "%s is pinned to %s, which cannot carry it: %s", linkName(i), pinned.Describe(), why)
				continue
			}
			owner[pinned], fixed[pinned] = i, true
		default:
			auto = append(auto, i)
		}
	}
	// Radios already carrying an automatic link keep it.
	var open []int
	for _, i := range auto {
		cur := currentHolder(in, LinkRole(i, false), LinkRole(i, true))
		if _, taken := owner[cur]; cur != nil && !taken {
			if ok, _ := in.meshEligibleFor(cur, bands[i], false); ok {
				owner[cur], fixed[cur] = i, true
				continue
			}
		}
		open = append(open, i)
	}
	// The rest: a maximum matching, links in order, radios best first.
	cands := map[int][]*Radio{}
	for _, i := range open {
		for _, r := range in.Radios {
			if fixed[r] {
				continue
			}
			if ok, _ := in.meshEligibleFor(r, bands[i], false); ok {
				cands[i] = append(cands[i], r)
			}
		}
		rankMesh(cands[i], bands[i])
	}
	var place func(i int, seen map[*Radio]bool) bool
	place = func(i int, seen map[*Radio]bool) bool {
		// A free radio first, so that no link is moved without need.
		for _, r := range cands[i] {
			if _, taken := owner[r]; !taken && !seen[r] {
				seen[r] = true
				owner[r] = i
				return true
			}
		}
		// None free: take one from a link that can move to another radio.
		for _, r := range cands[i] {
			if seen[r] {
				continue
			}
			seen[r] = true
			if j := owner[r]; place(j, seen) {
				owner[r] = i
				return true
			}
		}
		return false
	}
	var missing []int
	for _, i := range open {
		if !place(i, map[*Radio]bool{}) {
			missing = append(missing, i)
		}
	}
	for r, i := range owner {
		p.Links[i] = r
	}
	// A node is in the mesh over the links it has radios for, so a link
	// without one is only a fault when no link has any. Otherwise it is a
	// fact about this node (a 2.4 GHz-only adapter in a mesh that also has
	// a 5 GHz link, or the one radio pinned to another link), not a warning
	// to carry for good.
	for _, i := range missing {
		code := "no-" + config.LinkRoleKey(i) + "-radio"
		switch {
		case p.hasLink():
			p.note("info", code, "no radio for %s (%s): this node meshes over %s", linkName(i), bands[i], p.carried(bands))
		case i == 0:
			p.note("error", code, "no radio can carry the %s mesh: %s. Plug in an adapter that supports mesh point (docs/hardware.md lists tested ones)", bands[0], ineligibleSummary(in))
		default:
			// The first link is reported as the fault (above, or by its
			// pin or its disabled role); this one only adds its band.
			p.note("warning", code, "no radio left that can carry %s (%s)", linkName(i), bands[i])
		}
	}
}

// carried names the links that have a radio, for a notice: "mesh link 2
// (5g)", "the mesh (2g) and mesh link 3 (s1g)".
func (p *Plan) carried(bands []string) string {
	var names []string
	for i, lr := range p.Links {
		if lr == nil {
			continue
		}
		name := linkName(i)
		if i == 0 {
			name = "the first mesh link"
		}
		names = append(names, fmt.Sprintf("%s (%s)", name, bands[i]))
	}
	switch len(names) {
	case 0:
		return "no link"
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

func currentHolder(in Input, roles ...Role) *Radio {
	for fp, role := range in.Current {
		for _, want := range roles {
			if role == want {
				if r := Find(in.Radios, fp); r != nil {
					return r
				}
			}
		}
	}
	return nil
}

func ineligibleSummary(in Input) string {
	s := ""
	for _, r := range in.Radios {
		if ok, why := in.meshEligible(r); !ok {
			if s != "" {
				s += "; "
			}
			s += r.Phy + ": " + why
		}
	}
	return s
}
