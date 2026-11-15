package meshd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/vishvananda/netlink"

	"golang.org/x/sys/unix"

	"meridian/trellis/internal/config"
	"meridian/trellis/internal/hwdb"
	"meridian/trellis/internal/nl80211"
	"meridian/trellis/internal/radio"
	"meridian/trellis/internal/reconcile"
	"meridian/trellis/internal/sysfs"
	"meridian/trellis/internal/wpa"
)

// removalGrace is how long a vanished radio keeps its role before roles are
// recomputed, so a flaky USB connection does not thrash (architecture §5.2).
const removalGrace = 3 * time.Second

// Churn guard: if an interface meshd created has to be recreated this many
// times within the window, something else is reconfiguring it; meshd stops
// fighting and reports it.
const (
	churnLimit  = 3
	churnWindow = 2 * time.Minute
)

// radioReconciler owns the radio inventory, role assignment, the Trellis
// interfaces (mesh0, ap0), the regulatory domain and the mesh underlay MTU.
type radioReconciler struct {
	d              *Daemon
	booted         bool
	active         map[string]radio.Role
	missingSince   map[string]time.Time
	regApplied     string
	regAppliedAt   time.Time
	regFirstAsk    time.Time
	regSwitchAt    time.Time              // last re-request for a mode change (through 00)
	regSwitchTo    string                 // the mode it was for
	regSwitchTries int                    // attempts for that mode not yet taken (back-off)
	planKey        string                 // what the last plan was asked for (planKey)
	created        map[string][]time.Time // interface name -> recent creation times
	foreign        map[string]int         // foreign interfaces removed from the mesh radio
}

func newRadioReconciler(d *Daemon) *radioReconciler {
	return &radioReconciler{d: d, active: map[string]radio.Role{}, missingSince: map[string]time.Time{}}
}

func (r *radioReconciler) Name() string { return "radios" }

// meshLinkSetup prepares one mesh link's interface on its radio: rf-kill,
// the underlay MTU and a sanity check of its channel. It returns the
// interface (nil when it does not exist yet), its MTU and whether it
// changed anything.
func (r *radioReconciler) meshLinkSetup(rd *radio.Radio, ifname string, rp config.RadioPlan, alpha2 string, warn func(level, code, format string, args ...any)) (*nl80211.Interface, int, bool) {
	d := r.d
	cfg := d.cfg
	if rd == nil {
		return nil, 0, false
	}
	changed := false
	var meshIf *nl80211.Interface
	for i := range rd.Interfaces {
		if rd.Interfaces[i].Name == ifname {
			meshIf = &rd.Interfaces[i]
		}
	}
	if unblocked, err := sysfs.RFKillUnblock(d.opts.SysRoot, rd.Phy); err != nil {
		warn("warning", "rfkill", "%v", err)
	} else if unblocked {
		d.log.Info("cleared soft rf-kill block", "phy", rd.Phy)
		changed = true
	}
	mtu := 0
	if meshIf != nil {
		if link, err := netlink.LinkByIndex(meshIf.Ifindex); err == nil {
			// The radio's own limit wins over node.yaml (hwdb max_mesh_mtu).
			want := rd.HWDB.MeshMTU(cfg.MeshMTU)
			if link.Attrs().MTU != want {
				if err := netlink.LinkSetMTU(link, want); err != nil {
					warn("warning", "mesh-mtu", "cannot set %s MTU to %d (%v); keeping %d, batman-adv fragmentation covers the difference", ifname, want, err, link.Attrs().MTU)
				} else if want != cfg.MeshMTU {
					d.log.Info("set underlay MTU to the radio's limit; batman-adv fragments larger packets", "interface", ifname, "mtu", want, "mesh_mtu", cfg.MeshMTU)
					changed = true
				} else {
					d.log.Info("set underlay MTU", "interface", ifname, "mtu", want)
					changed = true
				}
			}
			if link, err = netlink.LinkByIndex(meshIf.Ifindex); err == nil {
				mtu = link.Attrs().MTU
			}
		}
	}
	if rp.Band == config.BandS1G {
		// No channel check here: the driver lists its HaLow channels as
		// 5 GHz ones (hwdb.Entry.Bands), and wpa_supplicant_s1g and the
		// chip's board file decide what may be used. The driver does carry
		// its own country, set when its module loads, and the channel plan
		// follows it.
		want := d.profile.Regulatory.Country
		if have, err := os.ReadFile(filepath.Join(d.opts.SysRoot, "module", "morse", "parameters", "country")); err == nil {
			if got := strings.TrimSpace(string(have)); got != "" && !strings.EqualFold(got, want) {
				warn("error", "halow-country", "the HaLow driver on %s is loaded for country %s, but the mesh profile says %s: its channels will not match the mesh's. "+
					"Rerun sudo /usr/share/trellis/scripts/install-wm6108-halow.sh --country %s on this node and reboot it", rd.Phy, got, want, want)
			}
		}
	} else if rd.Wiphy != nil {
		checkMeshChannel(rd, rp, alpha2, warn)
	}
	return meshIf, mtu, changed
}

// checkMeshChannel reports what the radio and the regulatory domain in force
// say about a Wi-Fi mesh link's channel. Trellis refuses nothing here: the
// link is started either way, and these say why it may not come up, or not
// at the width asked for.
func checkMeshChannel(rd *radio.Radio, rp config.RadioPlan, alpha2 string, warn func(level, code, format string, args ...any)) {
	freq, _ := config.ChannelFrequencyMHz(rp.Band, rp.Channel)
	link := "the " + linkName(rp) + " link"
	f := rd.Wiphy.Frequency(uint32(freq))
	switch {
	case f == nil:
		warn("error", "channel-unsupported", "%s has no channel %d (%d MHz): %s cannot start on it", rd.Name(), rp.Channel, freq, link)
		return
	case f.Disabled:
		warn("error", "channel-disabled", "regulatory domain %s disables channel %d (%d MHz) on %s: %s cannot transmit there", alpha2, rp.Channel, freq, rd.Name(), link)
		return
	case f.Radar:
		warn("warning", "channel-dfs", "channel %d (%d MHz) for %s needs radar detection (DFS) under regulatory domain %s: the link starts after a channel availability check "+
			"(a minute or more), where the driver supports radar detection on a mesh point, and stops if radar is heard",
			rp.Channel, freq, link, alpha2)
	case f.NoIR:
		warn("error", "channel-no-ir", "regulatory domain %s makes channel %d (%d MHz) passive-only (no-IR) on %s: the kernel does not let %s start there", alpha2, rp.Channel, freq, rd.Name(), link)
	}
	if !f.Radar {
		var radar []int
		for _, mhz := range blockFreqs(freq, rp.Width) {
			if g := rd.Wiphy.Frequency(uint32(mhz)); g != nil && g.Radar {
				radar = append(radar, (mhz-5000)/5)
			}
		}
		if len(radar) > 0 {
			warn("warning", "channel-dfs", "the %d MHz channel around channel %d for %s includes channels that need radar detection (DFS) under regulatory domain %s (%s): "+
				"the link starts after a channel availability check (a minute or more), where the driver supports radar detection on a mesh point, and stops if radar is heard",
				rp.Width, rp.Channel, link, alpha2, config.JoinInts(radar))
		}
	}
	switch {
	case rp.Width >= 160 && f.No160MHz:
		warn("warning", "channel-width", "%s, or regulatory domain %s, allows no 160 MHz channel around channel %d: %s runs narrower", rd.Name(), alpha2, rp.Channel, link)
	case rp.Width >= 80 && f.No80MHz:
		warn("warning", "channel-width", "%s, or regulatory domain %s, allows no 80 MHz channel around channel %d: %s runs narrower", rd.Name(), alpha2, rp.Channel, link)
	case rp.Width == 40 && f.NoHT40Minus && f.NoHT40Plus:
		warn("warning", "channel-width", "%s, or regulatory domain %s, allows no 40 MHz channel around channel %d: %s runs at 20 MHz", rd.Name(), alpha2, rp.Channel, link)
	}
}

// blockFreqs lists the 20 MHz channels of the 80 or 160 MHz 5 GHz block a
// primary channel lies in, as wpa_supplicant places them; nil for other
// widths and bands (6 GHz has no radar channels).
func blockFreqs(primaryMHz, widthMHz int) []int {
	var centre int
	switch widthMHz {
	case 80:
		centre = wpa.CenterFreq80(primaryMHz)
	case 160:
		for _, c := range []int{5250, 5570, 5815} { // channels 36–64, 100–128, 149–177
			if primaryMHz >= c-80 && primaryMHz < c+80 {
				centre = c
			}
		}
	}
	if centre == 0 {
		return nil
	}
	var out []int
	for f := centre - widthMHz/2 + 10; f < centre+widthMHz/2; f += 20 {
		out = append(out, f)
	}
	return out
}

func isTrellisIfname(name string) bool {
	return name == APIfname || meshLinkOfIfname(name) >= 0
}

func (r *radioReconciler) Reconcile(ctx context.Context) (reconcile.Result, error) {
	d := r.d
	cfg, profile := d.cfg, d.profile
	var res reconcile.Result
	warn := func(level, code, format string, args ...any) {
		res.Warnings = append(res.Warnings, reconcile.Warning{Level: level, Code: code, Message: fmt.Sprintf(format, args...)})
	}

	if d.nl == nil {
		transport := "none"
		if len(cfg.MeshPorts()) > 0 {
			transport = "wired"
		}
		d.facts.Update(func(f *Facts) { f.Transport = transport; f.Radios = nil; f.Plan = nil })
		if cfg.Radios.Roles.Mesh.Mode == config.RoleDisabled {
			return res, nil
		}
		return res, errors.New("nl80211 is unavailable, so no Wi-Fi radio can carry the mesh (set radios.roles.mesh: disabled for a wired-only node)")
	}

	// Regulatory domain first (architecture §5.1): it does not depend on
	// the inventory, and channel flags in the inventory depend on it.
	reg, changed := r.regulatory(d.nl, warn)
	d.facts.Update(func(f *Facts) { f.Regulatory = reg })

	radios, err := radio.Inventory(d.nl, d.opts.SysRoot, d.hw, cfg.Radios)
	if err != nil {
		return res, fmt.Errorf("radio inventory: %w", err)
	}

	// Radio memory.
	now := time.Now()
	memChanged := false
	present := map[string]bool{}
	for _, rd := range radios {
		present[rd.Fingerprint] = true
		rec := d.radioMem.Get(rd.Fingerprint)
		if rec.KernelIfname == "" {
			for _, in := range rd.Interfaces {
				if !isTrellisIfname(in.Name) && in.Ifindex != 0 {
					rec.KernelIfname = in.Name
					memChanged = true
					break
				}
			}
		}
		if rec.Port != rd.Device.Port || rec.Driver != rd.Device.Driver {
			rec.Port, rec.Driver = rd.Device.Port, rd.Device.Driver
			memChanged = true
		}
		if now.Sub(rec.LastSeen) > time.Minute {
			rec.LastSeen = now.UTC().Truncate(time.Second)
			memChanged = true
		}
	}

	// Debounce removals of role-holding radios.
	grace := false
	var requeue time.Duration
	for fp, role := range r.active {
		if present[fp] {
			delete(r.missingSince, fp)
			continue
		}
		since, ok := r.missingSince[fp]
		if !ok {
			since = now
			r.missingSince[fp] = now
			d.log.Warn("radio disappeared; holding its role briefly in case it returns", "fingerprint", fp, "role", role, "grace", removalGrace)
		}
		if left := removalGrace - now.Sub(since); left > 0 {
			grace = true
			if requeue == 0 || left < requeue {
				requeue = left + 100*time.Millisecond
			}
		}
	}
	if grace {
		d.facts.Update(func(f *Facts) { f.Radios = radios; f.GraceActive = true })
		res.RequeueAfter = requeue
		return res, nil
	}
	for fp := range r.active {
		if !present[fp] {
			d.log.Warn("radio gone; reassigning roles", "fingerprint", fp, "role", r.active[fp])
			delete(r.active, fp)
			delete(r.missingSince, fp)
		}
	}

	// Roles stay put while the node runs, so that plugging a radio in never
	// drops a link. That is for radios coming and going: when the operator
	// changes what the roles should be (a pin set or removed, a radio
	// ignored, the AP switched, the profile's links on other bands), the
	// plan is made afresh, as at start. Otherwise a role set back to
	// automatic would stay where the pin had put it until the next restart.
	key := planKey(cfg, profile)
	var current map[string]radio.Role
	if r.booted && key == r.planKey {
		current = r.active
	}
	r.planKey = key
	plan := radio.MakePlan(radio.Input{
		Radios: radios, MeshBand: profile.Radio.Band, MoreBands: linkBands(profile), LinkNames: profile.LinkNames(), APEnabled: cfg.APEnabled(), Roles: cfg.Radios.Roles, Current: current,
	})
	r.booted = true

	// Interfaces that no longer belong where they are.
	for _, rd := range radios {
		role := plan.Roles[rd.Fingerprint]
		wantLink, wantAP := role.Link(), role.HasAP()
		for i, in := range rd.Interfaces {
			// A mesh interface of a link this radio does not carry (any
			// more), or an AP interface on a radio without the AP.
			if l := meshLinkOfIfname(in.Name); (l >= 0 && l != wantLink) || (in.Name == APIfname && !wantAP) {
				if l >= 0 {
					d.leaveLink(l, "this radio no longer carries the link")
				}
				if err := d.nl.DelInterface(in.Ifindex); err != nil {
					if !cannotDelete(err) {
						return res, err
					}
					kname, perm := r.kernelIfname(rd.PhyIndex)
					if err := r.releaseInterface(in, kname, perm); err != nil {
						return res, err
					}
					d.log.Info("handed interface back as a station; the driver cannot delete it", "name", in.Name, "now", kname, "phy", rd.Phy, "reason", "role no longer held")
					// Keep the snapshot true to the kernel: ensureInterface
					// below skips Trellis names, and must see this one as the
					// kernel's interface again so a new role can take it over.
					rd.Interfaces[i].Name, rd.Interfaces[i].Type = kname, nl80211.NL80211_IFTYPE_STATION
					if len(perm) == 6 {
						rd.Interfaces[i].MAC = perm
					}
					changed = true
					continue
				}
				d.log.Info("removed interface", "name", in.Name, "phy", rd.Phy, "reason", "role no longer held")
				changed = true
			}
		}
	}
	for i, rd := range plan.Links {
		if rd == nil {
			continue
		}
		ch, err := r.ensureInterface(rd, meshIfname(i), nl80211.NL80211_IFTYPE_MESH_POINT, nil)
		if err != nil {
			return res, err
		}
		changed = changed || ch
	}
	// The mesh radio is Trellis's: any other interface that appears on it
	// later (some images add a p2p or wlan interface to every new radio, and
	// a network manager then scans on it) takes the radio off the mesh
	// channel. Remove it, and say so if it keeps coming back.
	for _, meshRadio := range plan.Links {
		if meshRadio == nil {
			continue
		}
		for _, in := range meshRadio.Interfaces {
			if isTrellisIfname(in.Name) || in.Ifindex == 0 {
				continue
			}
			if err := d.nl.DelInterface(in.Ifindex); err != nil {
				warn("warning", "foreign-interface", "cannot remove %s from the mesh radio %s: %v", in.Name, meshRadio.Phy, err)
				continue
			}
			changed = true
			if r.foreign == nil {
				r.foreign = map[string]int{}
			}
			r.foreign[in.Name]++
			d.log.Warn("removed an interface something else added to the mesh radio", "name", in.Name, "type", in.TypeName(), "phy", meshRadio.Phy, "times", r.foreign[in.Name])
			if r.foreign[in.Name] >= 3 {
				warn("warning", "foreign-interface", "%s keeps being added to the mesh radio %s (%d times); a udev rule or service on this node creates it. "+
					"Look for it with: grep -rl '%s\\|interface add' /etc/udev/rules.d /lib/udev/rules.d /etc/systemd/system", in.Name, meshRadio.Phy, r.foreign[in.Name], strings.TrimRight(in.Name, "0123456789"))
			}
		}
	}

	for _, rd := range append(append([]*radio.Radio{}, plan.Links...), plan.AP) {
		if rd == nil || rd.HWDB == nil || (rd == plan.AP && plan.APLink >= 0) {
			continue // no radio, nothing known about it, or already seen as a link's
		}
		for k, want := range rd.HWDB.ModuleParams {
			have, err := os.ReadFile(filepath.Join(d.opts.SysRoot, "module", rd.Device.Driver, "parameters", k))
			if err != nil || hwdb.ParamMatches(string(have), want) {
				continue
			}
			warn("error", "driver-params", "%s needs the %s module loaded with %s=%s (it has %s): without it, unicast over the mesh fails. "+
				"make deploy sets it; by hand: echo 'options %s %s=%s' | sudo tee /etc/modprobe.d/trellis.conf, then unplug and replug the adapter",
				rd.Describe(), rd.Device.Driver, k, want, strings.TrimSpace(string(have)), rd.Device.Driver, k, want)
		}
	}
	if plan.AP != nil {
		var mac net.HardwareAddr
		if plan.APLink >= 0 {
			mac = apMAC(plan.AP.PermAddr, plan.AP.Device.Driver)
		}
		ch, err := r.ensureInterface(plan.AP, APIfname, nl80211.NL80211_IFTYPE_AP, mac)
		if err != nil {
			return res, err
		}
		changed = changed || ch
	}
	if changed {
		if ifaces, err := d.nl.Interfaces(); err == nil {
			for _, rd := range radios {
				rd.Interfaces = rd.Interfaces[:0]
				for _, in := range ifaces {
					if in.Wiphy == rd.PhyIndex {
						rd.Interfaces = append(rd.Interfaces, in)
					}
				}
			}
		}
	}

	// Remember roles.
	r.active = map[string]radio.Role{}
	for _, rd := range radios {
		role := plan.Roles[rd.Fingerprint]
		r.active[rd.Fingerprint] = role
		rec := d.radioMem.Get(rd.Fingerprint)
		if rec.LastRole != string(role) {
			rec.LastRole = string(role)
			memChanged = true
		}
	}
	if memChanged {
		if err := d.radioMem.Save(d.opts.StateDir); err != nil {
			warn("warning", "state-write", "cannot save radio memory: %v", err)
		}
	}

	// mesh0 facts, MTU and rfkill.
	var apIf *nl80211.Interface
	if plan.AP != nil {
		for i := range plan.AP.Interfaces {
			if plan.AP.Interfaces[i].Name == APIfname {
				apIf = &plan.AP.Interfaces[i]
			}
		}
	}
	// An AP-only radio needs its soft block cleared too (meshLinkSetup does
	// it for mesh radios): Raspberry Pi OS soft-blocks the onboard Wi-Fi
	// until a country is set in raspi-config.
	if plan.AP != nil && plan.APLink < 0 {
		if unblocked, err := sysfs.RFKillUnblock(d.opts.SysRoot, plan.AP.Phy); err != nil {
			warn("warning", "rfkill", "%v", err)
		} else if unblocked {
			d.log.Info("cleared soft rf-kill block", "phy", plan.AP.Phy)
			changed = true
		}
	}
	meshIf, meshMTU, ch1 := r.meshLinkSetup(plan.Mesh, MeshIfname, profile.Radio, reg.KernelAlpha2, warn)
	changed = changed || ch1
	// The further links, in profile order.
	type linkIf struct {
		in  *nl80211.Interface
		mtu int
	}
	more := make([]linkIf, len(profile.Links))
	for i, rp := range profile.Links {
		in, mtu, ch := r.meshLinkSetup(plan.Link(i+1), meshIfname(i+1), rp, reg.KernelAlpha2, warn)
		more[i] = linkIf{in, mtu}
		changed = changed || ch
	}

	// The node's mesh transport, over all its links: a node whose only
	// radio carries the second link is on Wi-Fi as much as any other.
	transport := "none"
	for _, lr := range plan.Links {
		if lr != nil {
			transport = "wifi"
		}
	}
	if len(cfg.MeshPorts()) > 0 {
		if transport == "wifi" {
			transport = "wifi+wired"
		} else {
			transport = "wired"
		}
	}

	active := r.active
	d.facts.Update(func(f *Facts) {
		f.Radios, f.Plan, f.GraceActive = radios, plan, false
		f.Roles = map[string]radio.Role{}
		for k, v := range active {
			f.Roles[k] = v
		}
		f.MeshIfname, f.MeshIfindex, f.MeshWiphy, f.MeshMTU = "", 0, 0, 0
		if meshIf != nil {
			f.MeshIfname, f.MeshIfindex, f.MeshWiphy, f.MeshMTU = MeshIfname, meshIf.Ifindex, meshIf.Wiphy, meshMTU
		}
		// A link the profile no longer has keeps no facts: its reconciler
		// stops when the interface name is empty.
		for i := range f.MoreLinks {
			l := &f.MoreLinks[i]
			l.Ifname, l.Ifindex, l.Wiphy, l.MTU = "", 0, 0, 0
		}
		for i, li := range more {
			if li.in != nil {
				l := f.moreLink(i + 1)
				l.Ifname, l.Ifindex, l.Wiphy, l.MTU = meshIfname(i+1), li.in.Ifindex, li.in.Wiphy, li.mtu
			}
		}
		f.APIfname, f.APIfindex, f.APWiphy, f.APShared, f.APLink = "", 0, 0, false, 0
		if apIf != nil {
			f.APIfname, f.APIfindex, f.APWiphy = APIfname, apIf.Ifindex, apIf.Wiphy
			if plan.APLink >= 0 {
				f.APShared, f.APLink = true, plan.APLink
			}
		}
		f.Transport = transport
	})
	res.Changed = changed
	return res, nil
}

// ensureInterface makes sure the radio has exactly one interface with the
// given name and type, removing the kernel-created interfaces first.
func (r *radioReconciler) ensureInterface(rd *radio.Radio, name string, iftype uint32, mac net.HardwareAddr) (bool, error) {
	d := r.d
	for i, in := range rd.Interfaces {
		if in.Name == name {
			if in.Type == iftype && (mac == nil || in.MAC.String() == mac.String()) {
				return false, nil
			}
			if err := d.nl.DelInterface(in.Ifindex); err != nil {
				if !cannotDelete(err) {
					return false, err
				}
				if err := r.convertInterface(in, name, iftype, mac); err != nil {
					return false, err
				}
				d.log.Info("changed interface in place; the driver cannot delete it", "name", name, "type", nl80211.IftypeName(iftype), "phy", rd.Phy)
				rd.Interfaces[i].Type = iftype
				if mac != nil {
					rd.Interfaces[i].MAC = mac
				}
				return true, nil
			}
			if in.Type != iftype {
				d.log.Info("removed interface with the wrong type", "name", name, "type", in.TypeName())
			} else {
				d.log.Info("removed interface with the wrong address", "name", name, "had", in.MAC.String(), "want", mac.String())
			}
			break
		}
	}
	// The name may exist on another radio (role moved while we were down).
	if l, err := netlink.LinkByName(name); err == nil {
		if in, err := d.nl.InterfaceByIndex(l.Attrs().Index); err == nil && in.Wiphy != rd.PhyIndex {
			if err := d.nl.DelInterface(in.Ifindex); err != nil {
				return false, err
			}
			d.log.Info("removed interface from another radio", "name", name, "phy", in.Wiphy)
		}
	}
	// An interface of the right type under another name is most likely ours,
	// renamed by udev (Debian/Ubuntu 73-usb-net-by-mac.rules renames new
	// interfaces on USB adapters to wlx<mac>). Take it back rather than
	// deleting and recreating it on every pass.
	for i, in := range rd.Interfaces {
		if isTrellisIfname(in.Name) || in.Ifindex == 0 || in.Type != iftype {
			continue
		}
		l, err := netlink.LinkByIndex(in.Ifindex)
		if err != nil {
			break
		}
		if err := netlink.LinkSetDown(l); err != nil {
			return false, fmt.Errorf("take down %s to rename it: %w", in.Name, err)
		}
		if err := netlink.LinkSetName(l, name); err != nil {
			return false, fmt.Errorf("rename %s to %s: %w", in.Name, name, err)
		}
		d.log.Warn("renamed an interface back to its Trellis name; is /etc/udev/rules.d/70-trellis-names.rules installed?",
			"was", in.Name, "now", name, "type", nl80211.IftypeName(iftype), "phy", rd.Phy)
		rd.Interfaces[i].Name = name
		return true, nil
	}
	// Remove kernel-created interfaces, and drop them from rd.Interfaces: on
	// a radio with two roles (mesh+ap) this runs twice per pass on the same
	// snapshot, and the second call must not delete them again. One that is
	// already gone (ENODEV) is what we wanted.
	//
	// Some drivers will not delete the interface they created: brcmfmac
	// (Raspberry Pi onboard Wi-Fi) answers ENOTSUPP for its primary wlan0.
	// They do let its type and name change, so that interface becomes ours
	// in place of a new one.
	changed := false
	adopted := false
	kept := make([]nl80211.Interface, 0, len(rd.Interfaces))
	for _, in := range rd.Interfaces {
		if isTrellisIfname(in.Name) || in.Ifindex == 0 {
			kept = append(kept, in)
			continue
		}
		err := d.nl.DelInterface(in.Ifindex)
		switch {
		case err == nil, errors.Is(err, unix.ENODEV):
			d.log.Info("removed kernel-created interface", "name", in.Name, "phy", rd.Phy)
			changed = true
		case cannotDelete(err) && !adopted:
			if err := r.checkChurn(name); err != nil {
				return changed, err
			}
			if cerr := r.convertInterface(in, name, iftype, mac); cerr != nil {
				return changed, fmt.Errorf("%s on %s cannot be deleted (%v) or converted: %w", in.Name, rd.Phy, err, cerr)
			}
			d.log.Info("took over kernel-created interface; the driver cannot delete it", "was", in.Name, "now", name,
				"type", nl80211.IftypeName(iftype), "phy", rd.Phy, "radio", rd.Describe())
			in.Name, in.Type = name, iftype
			if mac != nil {
				in.MAC = mac
			}
			kept = append(kept, in)
			adopted, changed = true, true
		default:
			return changed, fmt.Errorf("remove kernel interface %s on %s: %w", in.Name, rd.Phy, err)
		}
	}
	rd.Interfaces = kept
	if adopted {
		return true, nil
	}
	if err := r.checkChurn(name); err != nil {
		return changed, err
	}
	newIf, err := d.nl.NewInterface(rd.PhyIndex, name, iftype, mac)
	if err != nil {
		return changed, err
	}
	d.log.Info("created interface", "name", name, "type", nl80211.IftypeName(iftype), "phy", rd.Phy, "radio", rd.Describe())
	rd.Interfaces = append(rd.Interfaces, *newIf)
	return true, nil
}

// cannotDelete reports whether a DEL_INTERFACE failure means the driver keeps
// the interface for good: brcmfmac answers ENOTSUPP (524) for its primary
// interface, other FullMAC drivers EOPNOTSUPP.
func cannotDelete(err error) bool {
	const enotsupp = unix.Errno(524) // kernel-internal, not exported by x/sys/unix
	return errors.Is(err, enotsupp) || errors.Is(err, unix.EOPNOTSUPP)
}

// convertInterface changes an existing interface's type, address and name in
// place, for drivers that will not delete it.
func (r *radioReconciler) convertInterface(in nl80211.Interface, name string, iftype uint32, mac net.HardwareAddr) error {
	l, err := netlink.LinkByIndex(in.Ifindex)
	if err != nil {
		return fmt.Errorf("find %s: %w", in.Name, err)
	}
	if err := netlink.LinkSetDown(l); err != nil {
		return fmt.Errorf("take down %s: %w", in.Name, err)
	}
	if in.Type != iftype {
		if err := r.d.nl.SetInterfaceType(in.Ifindex, iftype); err != nil {
			return fmt.Errorf("change %s from %s to %s: %w", in.Name, in.TypeName(), nl80211.IftypeName(iftype), err)
		}
	}
	if mac != nil && in.MAC.String() != mac.String() {
		if err := netlink.LinkSetHardwareAddr(l, mac); err != nil {
			return fmt.Errorf("set the address of %s to %s: %w", in.Name, mac, err)
		}
	}
	if in.Name != name {
		if err := netlink.LinkSetName(l, name); err != nil {
			return fmt.Errorf("rename %s to %s: %w", in.Name, name, err)
		}
	}
	return nil
}

// releaseInterface turns a Trellis interface the driver cannot delete back
// into the station interface the kernel created.
func (r *radioReconciler) releaseInterface(in nl80211.Interface, kernelName string, perm net.HardwareAddr) error {
	if len(perm) != 6 {
		perm = nil
	}
	return r.convertInterface(in, kernelName, nl80211.NL80211_IFTYPE_STATION, perm)
}

// kernelIfname returns the name of the interface the kernel created on a
// radio (remembered from when meshd first saw it; wlan0 if unknown) and the
// radio's permanent address.
func (r *radioReconciler) kernelIfname(wiphy uint32) (string, net.HardwareAddr) {
	var rd *radio.Radio
	r.d.facts.Read(func(f *Facts) {
		for _, x := range f.Radios {
			if x.PhyIndex == wiphy {
				rd = x
			}
		}
	})
	if rd == nil {
		return "wlan0", nil
	}
	name := "wlan0"
	if rec := r.d.radioMem.Radios[rd.Fingerprint]; rec != nil && rec.KernelIfname != "" && !isTrellisIfname(rec.KernelIfname) {
		name = rec.KernelIfname
	}
	return freeIfname(name), rd.PermAddr
}

// freeIfname is want when no interface has that name, else the first free
// wlanN. The name a radio's kernel interface had is remembered across
// boots, but the kernel numbers interfaces in the order radios appear: the
// remembered name may be another radio's now, and the radio would be left
// without an interface when meshd stops.
func freeIfname(want string) string {
	if _, err := netlink.LinkByName(want); err != nil {
		return want
	}
	for i := 0; i < 32; i++ {
		name := "wlan" + strconv.Itoa(i)
		if _, err := netlink.LinkByName(name); err != nil {
			return name
		}
	}
	return want
}

// apMAC derives the AP interface's address from the radio's permanent one
// when the AP shares the mesh radio: two interfaces on one phy need
// different addresses, and drivers disagree on which differences their
// hardware can filter:
//
//   - mt76x0/mt76x2 (MT7610U, MT7612U) take the hardware slot for a second
//     interface from bits 1–4 of the FIRST byte and refuse the interface
//     (EBUSY on up) when that byte equals the first interface's;
//   - rt2800 (RT5572) and ath9k take it from the low 3 bits of the LAST byte
//     and silently break the first interface when those collide.
//
// So mt76x* gets the locally administered bit set in the first byte, and
// everything else the lowest bit of the last byte flipped.
func apMAC(perm net.HardwareAddr, driver string) net.HardwareAddr {
	if len(perm) != 6 {
		return nil
	}
	m := make(net.HardwareAddr, 6)
	copy(m, perm)
	if strings.HasPrefix(driver, "mt76x") {
		if m[0]&0x02 == 0 {
			m[0] |= 0x02
		} else {
			m[0] ^= 0x04
		}
		return m
	}
	m[5] ^= 0x01
	return m
}

// Teardown removes Trellis interfaces and gives radios their kernel
// interface back.
func (r *radioReconciler) Teardown(ctx context.Context) error {
	d := r.d
	if d.nl == nil {
		return nil
	}
	ifaces, err := d.nl.Interfaces()
	if err != nil {
		return err
	}
	var errs []error
	touched := map[uint32]bool{}
	for _, in := range ifaces {
		if isTrellisIfname(in.Name) && in.Ifindex != 0 {
			if err := d.nl.DelInterface(in.Ifindex); err != nil {
				if !cannotDelete(err) {
					errs = append(errs, err)
					continue
				}
				kname, perm := r.kernelIfname(in.Wiphy)
				if err := r.releaseInterface(in, kname, perm); err != nil {
					errs = append(errs, err)
					continue
				}
				d.log.Info("handed interface back as a station; the driver cannot delete it", "name", in.Name, "now", kname)
				continue
			}
			touched[in.Wiphy] = true
			d.log.Info("removed interface", "name", in.Name)
		}
	}
	// Restore a managed interface where we removed the kernel's one.
	remaining := map[uint32]int{}
	if ifaces, err = d.nl.Interfaces(); err == nil {
		for _, in := range ifaces {
			if in.Ifindex != 0 {
				remaining[in.Wiphy]++
			}
		}
	}
	for fp, role := range r.active {
		if role == radio.RoleUnused {
			continue
		}
		rec := d.radioMem.Radios[fp]
		if rec == nil || rec.KernelIfname == "" {
			continue
		}
		var rd *radio.Radio
		d.facts.Read(func(f *Facts) { rd = radio.Find(f.Radios, fp) })
		if rd == nil || !touched[rd.PhyIndex] || remaining[rd.PhyIndex] > 0 {
			continue
		}
		name := freeIfname(rec.KernelIfname)
		if _, err := d.nl.NewInterface(rd.PhyIndex, name, nl80211.NL80211_IFTYPE_STATION, nil); err != nil {
			d.log.Warn("could not restore kernel interface", "name", name, "phy", rd.Phy, "err", err)
		} else {
			d.log.Info("restored kernel interface", "name", name, "phy", rd.Phy)
		}
	}
	return errors.Join(errs...)
}

// checkChurn records a (re)creation of name and refuses when it has happened
// too often recently: something else keeps changing or deleting it.
func (r *radioReconciler) checkChurn(name string) error {
	if r.created == nil {
		r.created = map[string][]time.Time{}
	}
	now := time.Now()
	recent := r.created[name][:0]
	for _, t := range r.created[name] {
		if now.Sub(t) < churnWindow {
			recent = append(recent, t)
		}
	}
	if len(recent) >= churnLimit {
		r.created[name] = recent
		return fmt.Errorf("%s had to be recreated %d times in %s: something else on this node keeps changing it, "+
			"most likely NetworkManager, systemd-networkd/netplan, iwd or a system wpa_supplicant. Check: nmcli device; "+
			"networkctl; ps aux | grep -E 'wpa_supplicant|iwd'. meshd stops recreating it until %s",
			name, len(recent), churnWindow, recent[0].Add(churnWindow).Format("15:04:05"))
	}
	if n := len(recent); n > 0 {
		r.d.log.Warn("recreating an interface meshd created earlier; something removed or changed it",
			"name", name, "previous_created_ago", now.Sub(recent[n-1]).Round(time.Second), "recreations_in_window", n)
	}
	r.created[name] = append(recent, now)
	return nil
}

// planKey is everything in the configuration that says which radio should
// do what: the role selections, the per-radio rules that rank or exclude a
// radio (ignore, a power figure), whether there is an AP, and the band of
// each mesh link. Two configurations with the same key ask for the same
// plan. A label is left out: renaming a radio must not move a link.
func planKey(n *config.Node, p *config.Profile) string {
	rs := n.Radios
	rs.Devices = []config.DeviceRule{}
	for _, d := range n.Radios.Devices {
		if d.Ignore || d.MaxPowerDBm != nil { // a rule that is only a label plans nothing
			d.Label = ""
			rs.Devices = append(rs.Devices, d)
		}
	}
	roles, _ := json.Marshal(rs)
	return fmt.Sprintf("%s ap=%v bands=%s,%s", roles, n.APEnabled(), p.Radio.Band, strings.Join(linkBands(p), ","))
}

// linkBands lists the bands of the profile's links after the first, for the
// planner.
func linkBands(p *config.Profile) []string {
	var out []string
	for _, l := range p.Links {
		out = append(out, l.Band)
	}
	return out
}
