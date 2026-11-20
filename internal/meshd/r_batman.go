package meshd

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"github.com/vishvananda/netlink"

	"meridian/trellis/internal/batadv"
	"meridian/trellis/internal/config"
	"meridian/trellis/internal/reconcile"
	"meridian/trellis/internal/rtnl"
)

// batmanReconciler owns bat0: the module version gate, the interface, its
// settings and its hard interfaces (architecture §6).
type batmanReconciler struct {
	d           *Daemon
	modprobeAt  time.Time
	modulePath  string
	modinfoDone bool
	// loggedUnsupported records that unsupported settings were reported.
	loggedUnsupported bool
	wantAllSince      time.Time // when this node started asking for all IPv4 multicast
	// warmedIfindex is the bat0 the broadcast warm-up was started on
	// (bcastwarmup.go): once per bat0, as its numbering starts at 1. The
	// sender sets it back to 0 when it fails, so the next pass tries again.
	warmedIfindex atomic.Int64
	// warmFailedAt is when the last warm-up failed (unix ns): a new try
	// waits warmupRetry after it.
	warmFailedAt atomic.Int64
	// blaIfindex is the bat0 on which bridge loop avoidance was turned on
	// for a wired client port: it stays on for that bat0's life, so that
	// plugging a cable in and out does not start its 30 s start-up wait
	// again each time.
	blaIfindex int
	// siteSet is what each site link tunnel was last told (throughput and
	// hop penalty, by interface index): a module that takes the request
	// and keeps another value is told once, not on every pass.
	siteSet map[int][2]uint32
}

// defaultMcastFanout is batman-adv's own default for multicast_fanout.
const defaultMcastFanout = 16

// mcastGrace is how long a node may want all multicast before it is
// reported: the bridge querier needs a query interval plus its response
// time to be elected and recognised (architecture §9).
const mcastGrace = 45 * time.Second

func newBatmanReconciler(d *Daemon) *batmanReconciler { return &batmanReconciler{d: d} }

func (b *batmanReconciler) Name() string { return "batman" }

func boolp(v bool) *bool    { return &v }
func u32p(v uint32) *uint32 { return &v }

func (b *batmanReconciler) Reconcile(ctx context.Context) (reconcile.Result, error) {
	d := b.d
	cfg, profile := d.cfg, d.profile
	var res reconcile.Result
	warn := func(level, code, format string, args ...any) {
		res.Warnings = append(res.Warnings, reconcile.Warning{Level: level, Code: code, Message: fmt.Sprintf(format, args...)})
	}

	// Module and version gate.
	mi := batadv.ReadModuleInfo()
	if !mi.Loaded {
		if time.Since(b.modprobeAt) > 10*time.Second {
			b.modprobeAt = time.Now()
			if out, err := exec.CommandContext(ctx, "modprobe", "batman-adv").CombinedOutput(); err != nil {
				d.facts.Update(func(f *Facts) { f.BatPresent = false })
				return res, fmt.Errorf("batman_adv module is not loaded and modprobe failed: %v: %s", err, strings.TrimSpace(string(out)))
			}
			res.Changed = true
			mi = batadv.ReadModuleInfo()
		}
		if !mi.Loaded {
			d.facts.Update(func(f *Facts) { f.BatPresent = false })
			return res, fmt.Errorf("batman_adv module is not loaded")
		}
	}
	if !b.modinfoDone {
		b.modinfoDone = true
		if out, err := exec.Command("modinfo", "-n", "batman-adv").Output(); err == nil {
			b.modulePath = strings.TrimSpace(string(out))
		}
	}
	v, inferred := mi.EffectiveRelease()
	minV, _ := batadv.ParseVersion(profile.Batman.MinVersion)
	pinV, _ := batadv.ParseVersion(profile.Batman.PinnedVersion)
	mismatch := inferred || v != pinV
	d.facts.Update(func(f *Facts) {
		f.BatModule, f.BatModulePath, f.BatInferred, f.BatMismatch = mi.Raw, b.modulePath, inferred, mismatch
	})
	if v.Less(minV) {
		if !cfg.AllowOldBatman {
			d.facts.Update(func(f *Facts) { f.BatPresent = false })
			return res, fmt.Errorf("batman-adv %s is below the minimum %s required by the mesh profile; install the pinned %s build (sudo meshctl prepare), or set allow_old_batman: true in node.yaml for bench experiments", v, minV, pinV)
		}
		warn("warning", "batman-old-allowed", "Legacy batman-adv %s (minimum %s) allowed by allow_old_batman: forces mesh-wide multicast flooding", v, minV)
	} else if mismatch {
		if inferred {
			warn("warning", "batman-version-unknown", "batman-adv %q (in-kernel, inferred %s); pinned release %s. Install the DKMS build on all nodes", mi.Raw, v, pinV)
		} else {
			warn("warning", "batman-version-mismatch", "batman-adv %s differs from pinned release %s. Install the pinned build on all nodes", v, pinV)
		}
	}

	if d.bat == nil {
		c, err := batadv.Dial()
		if err != nil {
			return res, err
		}
		d.bat = c
	}

	// bat0.
	mac := d.identity().HardwareAddr()
	link, err := rtnl.LinkByNameOrNil(BatIfname)
	if err != nil {
		return res, err
	}
	if link != nil && link.Type() != "batadv" {
		return res, fmt.Errorf("%s exists but is a %s interface, not batman-adv; remove it", BatIfname, link.Type())
	}
	if link == nil {
		if err := rtnl.CreateBatadv(BatIfname, profile.Batman.RoutingAlgo, mac); err != nil {
			return res, err
		}
		d.log.Info("created batman-adv interface", "name", BatIfname, "algo", profile.Batman.RoutingAlgo, "mac", mac.String())
		res.Changed = true
		if link, err = netlink.LinkByName(BatIfname); err != nil {
			return res, err
		}
	}
	idx := link.Attrs().Index
	mesh, err := d.bat.GetMesh(idx)
	if err != nil {
		return res, fmt.Errorf("read %s: %w", BatIfname, err)
	}
	if mesh.AlgoName != "" && mesh.AlgoName != profile.Batman.RoutingAlgo {
		d.log.Warn("recreating bat0 with the configured routing algorithm", "have", mesh.AlgoName, "want", profile.Batman.RoutingAlgo)
		if err := netlink.LinkDel(link); err != nil {
			return res, err
		}
		if err := rtnl.CreateBatadv(BatIfname, profile.Batman.RoutingAlgo, mac); err != nil {
			return res, err
		}
		res.Changed = true
		if link, err = netlink.LinkByName(BatIfname); err != nil {
			return res, err
		}
		idx = link.Attrs().Index
		if mesh, err = d.bat.GetMesh(idx); err != nil {
			return res, err
		}
	}
	if ch, err := rtnl.EnsureHardwareAddr(link, mac); err != nil {
		return res, fmt.Errorf("set %s MAC: %w", BatIfname, err)
	} else if ch {
		d.log.Info("set interface MAC", "interface", BatIfname, "mac", mac.String())
		res.Changed = true
	}

	// Settings (architecture §6 table). gw_mode and its bandwidth belong to
	// the gateway reconciler (§8), which switches between server and
	// client; it must not be reset here. The distributed ARP table is off:
	// it only learns a client when that client's ARP traffic crosses bat0,
	// and with the gateway answered locally on every node (§7.4) it never
	// does, so other nodes could not resolve the client at all unless its
	// node happened to be one of the three DHT candidates for its address.
	want := batadv.Settings{
		AggregatedOGMs:      boolp(true),
		BridgeLoopAvoidance: boolp(b.blaOn(idx)),
		DistributedARPTable: boolp(false),
		Fragmentation:       boolp(true),
		MulticastForceflood: boolp(d.profile.Batman.MulticastFlood),
		NetworkCoding:       boolp(false),
		Bonding:             boolp(false),
		APIsolation:         boolp(false),
		OrigIntervalMS:      u32p(uint32(profile.Batman.OrigIntervalMS)),
	}
	// Always set: removing the knob from the profile must restore the
	// default, not leave the last value in force.
	fanout := uint32(defaultMcastFanout)
	if f := profile.Batman.MulticastFanout; f != nil {
		fanout = uint32(*f)
	}
	want.MulticastFanout = u32p(fanout)
	if u := batadv.Unsupported(mesh.Settings, want); len(u) > 0 && !b.loggedUnsupported {
		d.log.Info("batman-adv module does not provide these settings; skipping them", "settings", u)
		b.loggedUnsupported = true
	}
	if diff := batadv.Diff(mesh.Settings, want); !diff.IsEmpty() {
		if err := d.bat.SetMesh(idx, diff); err != nil {
			return res, fmt.Errorf("apply %s settings %v: %w", BatIfname, diff.Names(), err)
		}
		d.log.Info("applied batman-adv settings", "changed", diff.Names())
		res.Changed = true
	}

	// Hard interfaces.
	desired, sites := map[string]bool{}, map[string]bool{}
	// Every mesh link is a hard interface: BATMAN_V compares paths across
	// them by throughput and uses the best one per destination.
	d.facts.Read(func(f *Facts) {
		if f.MeshIfname != "" {
			desired[f.MeshIfname] = true
		}
		for _, l := range f.MoreLinks {
			if l.Ifname != "" {
				desired[l.Ifname] = true
			}
		}
		for _, n := range f.siteIfnames() {
			sites[n] = true
			desired[n] = true
		}
	})
	for _, port := range cfg.MeshPorts() {
		l, err := rtnl.LinkByNameOrNil(port)
		if err != nil {
			return res, err
		}
		if l == nil {
			warn("warning", "mesh-port-missing", "wired mesh port %s does not exist", port)
			continue
		}
		desired[port] = true
		if l.Attrs().MTU != cfg.MeshMTU {
			if err := netlink.LinkSetMTU(l, cfg.MeshMTU); err != nil {
				warn("warning", "mesh-mtu", "cannot set %s MTU to %d (%v); keeping %d", port, cfg.MeshMTU, err, l.Attrs().MTU)
			} else {
				d.log.Info("set underlay MTU", "interface", port, "mtu", cfg.MeshMTU)
				res.Changed = true
			}
		}
	}
	for name := range desired {
		l, err := netlink.LinkByName(name)
		if err != nil {
			continue
		}
		if ch, err := rtnl.EnsureMaster(l, link); err != nil {
			return res, fmt.Errorf("attach %s to %s: %w", name, BatIfname, err)
		} else if ch {
			d.log.Info("attached hard interface", "interface", name, "to", BatIfname)
			res.Changed = true
		}
		if ch, err := rtnl.EnsureUp(l); err != nil {
			return res, fmt.Errorf("bring up %s: %w", name, err)
		} else if ch {
			res.Changed = true
		}
	}
	hifs, err := d.bat.HardIfs(idx)
	if err != nil {
		return res, err
	}
	// A site link tunnel reports no speed, and BATMAN_V would take it for
	// 1 Mbit/s: it is told the profile's figure (100 kbit/s units), and a
	// hop penalty of its own, so that a path out of this site by one tunnel
	// and back by another never beats a radio path within the site.
	siteTP := uint32(profile.SiteLink.ThroughputMbit() * 10)
	global := uint8(batadvDefaultHopPenalty)
	if mesh.Settings.HopPenalty != nil {
		global = *mesh.Settings.HopPenalty
	}
	siteHP := siteHopPenalty(siteTP, global)
	told := map[int][2]uint32{}
	for _, h := range hifs {
		if sites[h.Name] && (h.ThroughputOverride != siteTP || h.HopPenalty != siteHP) {
			want := [2]uint32{siteTP, uint32(siteHP)}
			if b.siteSet[h.Ifindex] == want {
				// Taken on an earlier pass, and not what the module
				// reports: asking again changes nothing.
				told[h.Ifindex] = want
				warn("warning", "site-link-throughput", "batman-adv rejected %s settings (throughput %d.%d Mbit/s, hop penalty %d; reports %d.%d and %d). Site link path ranking may be wrong. Update batman-adv (meshctl prepare)",
					h.Name, siteTP/10, siteTP%10, siteHP, h.ThroughputOverride/10, h.ThroughputOverride%10, h.HopPenalty)
			} else if err := d.bat.SetHardIf(idx, h.Ifindex, nil, &siteTP, &siteHP); err != nil {
				warn("warning", "site-link-throughput", "cannot set %s's throughput and hop penalty for batman-adv: %v", h.Name, err)
			} else {
				told[h.Ifindex] = want
				res.Changed = true
			}
		}
		if desired[h.Name] {
			continue
		}
		if l, err := netlink.LinkByIndex(h.Ifindex); err == nil {
			if err := netlink.LinkSetNoMaster(l); err != nil {
				return res, fmt.Errorf("detach %s from %s: %w", h.Name, BatIfname, err)
			}
			d.log.Info("detached hard interface", "interface", h.Name, "from", BatIfname)
			res.Changed = true
		}
	}
	b.siteSet = told
	if len(desired) == 0 {
		warn("error", "no-hard-interfaces", "%s: no hard interfaces (no mesh radio or wired mesh port)", BatIfname)
	}

	if ch, err := rtnl.EnsureUp(link); err != nil {
		return res, fmt.Errorf("bring up %s: %w", BatIfname, err)
	} else if ch {
		res.Changed = true
	}
	if link, err = netlink.LinkByName(BatIfname); err != nil {
		return res, err
	}
	// Only now: a broadcast sent on a bat0 that is not up yet fails.
	if link.Attrs().Flags&net.FlagUp != 0 {
		b.warmUp(idx, hifs, mac, b.carriesHaLow(desired))
	}
	mtu := link.Attrs().MTU
	if mtu < 1280 {
		warn("warning", "bat0-mtu-low", "%s MTU %d below 1280 (required for batman-adv multicast)", BatIfname, mtu)
	}
	d.facts.Update(func(f *Facts) {
		f.BatPresent, f.BatIfindex, f.BatAlgo, f.BatMTU, f.BatMAC = true, idx, mesh.AlgoName, mtu, link.Attrs().HardwareAddr.String()
	})
	if d.profile.Batman.MulticastFlood {
		warn("info", "multicast-flood", "Multicast flooding on (batman.multicast_flood in mesh profile)")
	}
	if mf, err := d.bat.MulticastFlags(idx); err == nil {
		wantAll := mf.Own&batadv.McastWantAllIPv4 != 0
		switch {
		case !wantAll:
			b.wantAllSince = time.Time{}
		case b.wantAllSince.IsZero():
			b.wantAllSince = time.Now()
			res.RequeueAfter = mcastGrace
		case time.Since(b.wantAllSince) >= mcastGrace:
			why := "no IGMP querier is known"
			if mf.OwnPriv&batadv.BATADV_MCAST_FLAGS_QUERIER_IPV4_SHADOWING != 0 {
				why = "an IGMP querier on this node's side of bat0 (a client port or the AP) shadows the mesh's querier"
			}
			warn("warning", "multicast-want-all", "Node requests all IPv4 multicast groups (%s): multicast cost equal to flooding", why)
		}
	}
	return res, nil
}

// warmUp starts the broadcast warm-up on a new bat0 once one of its hard
// interfaces is active: before that batman-adv drops broadcasts without
// numbering them.
func (b *batmanReconciler) warmUp(idx int, hifs []batadv.HardIf, mac net.HardwareAddr, halow bool) {
	if int64(idx) == b.warmedIfindex.Load() || b.d.ctx == nil {
		return
	}
	if t := b.warmFailedAt.Load(); t != 0 && time.Since(time.Unix(0, t)) < warmupRetry {
		return
	}
	active := false
	for _, h := range hifs {
		active = active || h.Active
	}
	if !active {
		return
	}
	// Not before the mesh has peered: on a radio link batman-adv marks the
	// interface active at once, and broadcasts sent before the first peer
	// is established reach nobody. The numbers would be used up and the
	// first one a neighbour sees could fall inside its window. A neighbour
	// in batman-adv's table means a link that carries frames.
	if nb, err := b.d.bat.Neighbors(idx); err != nil || len(nb) == 0 {
		return
	}
	b.warmedIfindex.Store(int64(idx))
	ctx, log := b.d.ctx, b.d.log
	gap := warmupGap
	if halow {
		gap = warmupGapHaLow
	}
	go func() {
		start := time.Now()
		n, held, err := sendWarmup(ctx, idx, BatIfname, mac, gap)
		if err != nil && ctx.Err() == nil {
			log.Warn("broadcast warm-up cut short; tried again shortly", "numbered", n, "of", warmupCount, "held_back", held, "err", err, "retry_in", warmupRetry)
			b.warmFailedAt.Store(time.Now().UnixNano())
			b.warmedIfindex.CompareAndSwap(int64(idx), 0)
			time.AfterFunc(warmupRetry+100*time.Millisecond, func() { b.d.Kick("broadcast warm-up retry") })
			return
		}
		if ctx.Err() != nil {
			log.Debug("broadcast warm-up stopped: meshd is stopping", "frames", n)
			return
		}
		log.Info("broadcast warm-up sent", "frames", n, "held_back", held, "took", time.Since(start).Round(time.Millisecond))
	}()
}

// blaOn decides bridge loop avoidance for bat0 (config BLAOn): when the
// profile leaves it unset, on once the node bridges a wired client port,
// and from then on for the rest of this bat0's life.
func (b *batmanReconciler) blaOn(idx int) bool {
	if len(b.d.clientPorts()) > 0 {
		b.blaIfindex = idx
	}
	return b.d.profile.Batman.BLAOn(b.blaIfindex == idx && idx != 0)
}

// carriesHaLow says whether one of bat0's hard interfaces is a HaLow link.
func (b *batmanReconciler) carriesHaLow(hard map[string]bool) bool {
	for i, l := range b.d.profile.AllLinks() {
		if l.Band == config.BandS1G && hard[config.LinkIfname(i)] {
			return true
		}
	}
	return false
}

func (b *batmanReconciler) Teardown(ctx context.Context) error {
	deleted, err := rtnl.DeleteIfExists(BatIfname)
	if deleted {
		b.d.log.Info("removed interface", "name", BatIfname)
	}
	return err
}

// batadvDefaultHopPenalty is batman-adv's own hop penalty (out of 255,
// mesh-interface.c), which Trellis leaves as it is; used only when the
// module does not report it.
const batadvDefaultHopPenalty = 30

// siteRoundTripMax is the most a path may be worth (100 kbit/s units)
// that leaves a site by one tunnel and comes back to it by another: less
// than the slowest radio link a mesh carries (HaLow at 1 MHz, about
// 0.8 Mbit/s), so that traffic between two parts of a site stays on the
// air rather than crossing the internet twice.
const siteRoundTripMax = 8

// siteHopPenalty is the hop penalty given to every site link tunnel: the
// lowest that keeps a round trip through another site at or below
// siteRoundTripMax, so that one crossing, the only way between sites,
// keeps as much of the figure as it can. At most 254: 255 would make every
// path over a tunnel worth 0, which batman-adv drops.
func siteHopPenalty(tp uint32, global uint8) uint8 {
	for hp := 0; hp < 255; hp++ {
		if siteRoundTrip(tp, uint8(hp), global) <= siteRoundTripMax {
			return uint8(hp)
		}
	}
	return 254
}

// siteCrossing is what a path from a member of another site is worth on
// arrival over the tunnel: batman-adv takes a hard interface's own penalty
// off what arrives on it (bat_v_ogm.c, batadv_v_forward_penalty).
func siteCrossing(tp uint32, hp uint8) uint32 {
	return uint32(uint64(tp) * uint64(255-uint32(hp)) / 255)
}

// siteRoundTrip is what a node's path to another member of its own site is
// worth when it goes through a member of another site: the far member
// takes the tunnel's penalty on arrival and batman-adv's own on passing it
// on; this node takes the tunnel's penalty again. The same integer steps
// as batman-adv's.
func siteRoundTrip(tp uint32, hp, global uint8) uint32 {
	t := siteCrossing(tp, hp)
	t = uint32(uint64(t) * uint64(255-uint32(global)) / 255)
	return siteCrossing(min(t, tp), hp)
}
