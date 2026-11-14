package meshd

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"meridian/trellis/internal/config"
	"meridian/trellis/internal/nl80211"
	"meridian/trellis/internal/proc"
	"meridian/trellis/internal/reconcile"
	"meridian/trellis/internal/status"
	"meridian/trellis/internal/wpa"
)

// meshLinkReconciler runs wpa_supplicant on one mesh link's interface for
// 802.11s + SAE (architecture §5). Link 0 is mesh0 (profile radio); links 1
// and up are mesh1, mesh2, … (the profile's links). One
// instance runs per link (moreLinksReconciler holds those after the first). A HaLow link (band s1g) runs Morse Micro's
// wpa_supplicant_s1g instead, with an S1G channel in place of a frequency.
type meshLinkReconciler struct {
	d          *Daemon
	link       int
	sup        *proc.Supervisor
	supSince   time.Time
	supIfindex int // the interface wpa_supplicant was started on
	ctrl       *wpa.Ctrl
	events     *wpa.EventConn
	evStop     chan struct{}
	wpaPath    string
	wpaBin     string // the binary name wpaPath was looked up for
	// noHTOverrides: this wpa_supplicant lacks CONFIG_HT_OVERRIDES (probed
	// once with the binary; see wpa.HTOverrides).
	noHTOverrides bool
	// blocks are the peers wpa_supplicant blocked after failed SAE
	// attempts, which meshd unblocks early (saeblock.go).
	blocks saeBlocks
	// Per-peer transmit counters and verdicts for one-sided links
	// (onesided.go).
	peerTX      map[string]peerTX
	oneSided    map[string]bool
	oneSidedMsg map[string]string
}

// moreLinksReconciler runs the mesh links after the first (the profile's
// links): one meshLinkReconciler each. It shows as one entry, "meshlinks",
// however many links the profile has; a link that fails leaves the others
// running.
type moreLinksReconciler struct {
	d     *Daemon
	links []*meshLinkReconciler // links[i] runs link i+1
}

func (m *moreLinksReconciler) Name() string { return "meshlinks" }

func (m *moreLinksReconciler) Reconcile(ctx context.Context) (reconcile.Result, error) {
	for len(m.links) < len(m.d.profile.Links) {
		m.links = append(m.links, newMeshLinkReconciler(m.d, len(m.links)+1))
	}
	var res reconcile.Result
	var errs []error
	// Every link there has ever been a reconciler for: one the profile has
	// dropped stops its supplicant in its own Reconcile.
	for _, l := range m.links {
		r, err := l.Reconcile(ctx)
		res.Changed = res.Changed || r.Changed
		res.Warnings = append(res.Warnings, r.Warnings...)
		if r.RequeueAfter > 0 && (res.RequeueAfter == 0 || r.RequeueAfter < res.RequeueAfter) {
			res.RequeueAfter = r.RequeueAfter
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	// A peer that moved its radio from one of this node's links to another
	// leaves an entry behind that stops the new link carrying traffic
	// (peermove.go).
	changed, again := m.d.dropMovedPeers(ctx)
	res.Changed = res.Changed || changed
	if again && (res.RequeueAfter == 0 || res.RequeueAfter > time.Second) {
		res.RequeueAfter = time.Second
	}
	return res, errors.Join(errs...)
}

func (m *moreLinksReconciler) Teardown(ctx context.Context) error {
	var errs []error
	for i := len(m.links) - 1; i >= 0; i-- {
		if err := m.links[i].Teardown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func newMeshLinkReconciler(d *Daemon, link int) *meshLinkReconciler {
	return &meshLinkReconciler{d: d, link: link}
}

func (m *meshLinkReconciler) Name() string {
	if m.link == 0 {
		return "meshlink"
	}
	return "meshlink" + strconv.Itoa(m.link+1)
}

// ifname is the link's interface name.
func (m *meshLinkReconciler) ifname() string {
	return meshIfname(m.link)
}

// radioPlan is the link's band, channel and width from the profile, and
// whether the profile has this link at all.
func (m *meshLinkReconciler) radioPlan(p *config.Profile) (config.RadioPlan, bool) {
	return p.Link(m.link)
}

// linkFacts reads the link's interface.
func (m *meshLinkReconciler) linkFacts(f *Facts) (ifname string, ifindex int) {
	if m.link == 0 {
		return f.MeshIfname, f.MeshIfindex
	}
	if m.link > len(f.MoreLinks) {
		return "", 0
	}
	l := f.MoreLinks[m.link-1]
	return l.Ifname, l.Ifindex
}

// setFacts records the link's supplicant state, frequency, width and power.
func (m *meshLinkReconciler) setFacts(sup status.Supplicant, freq, widthMHz int, txDBm float64) {
	m.d.facts.Update(func(f *Facts) {
		if m.link == 0 {
			f.Supplicant, f.MeshFreqMHz, f.MeshWidthMHz, f.TXPowerDBm = sup, freq, widthMHz, txDBm
			return
		}
		l := f.moreLink(m.link)
		l.Supplicant, l.FreqMHz, l.WidthMHz, l.TXPowerDBm = sup, freq, widthMHz, txDBm
	})
}

func (m *meshLinkReconciler) confPath() string {
	return filepath.Join(m.d.opts.RunDir, "wpa-"+m.ifname()+".conf")
}

func (m *meshLinkReconciler) ctrlPath() string {
	return filepath.Join(m.d.opts.RunDir, "wpa", m.ifname())
}

func (m *meshLinkReconciler) Reconcile(ctx context.Context) (reconcile.Result, error) {
	d := m.d
	var res reconcile.Result
	warn := func(level, code, format string, args ...any) {
		res.Warnings = append(res.Warnings, reconcile.Warning{Level: level, Code: code, Message: fmt.Sprintf(format, args...)})
	}
	var meshIfname string
	var meshIfindex int
	d.facts.Read(func(f *Facts) { meshIfname, meshIfindex = m.linkFacts(f) })
	rp, hasLink := m.radioPlan(d.profile)

	if meshIfname == "" || !hasLink {
		if m.sup != nil {
			m.stop()
			res.Changed = true
		}
		m.setFacts(status.Supplicant{}, 0, 0, 0)
		return res, nil
	}

	halow := rp.Band == config.BandS1G
	bin := "wpa_supplicant"
	if halow {
		bin = wpa.S1GBinary
	}
	if m.wpaPath == "" || m.wpaBin != bin {
		p, err := proc.LookPath(bin)
		if err != nil {
			if halow {
				return res, fmt.Errorf("%s: a HaLow mesh link needs Morse Micro's %s, which is not installed on this node "+
					"(scripts/install-halow-tools.sh builds it)", meshIfname, bin)
			}
			return res, err
		}
		m.wpaPath, m.wpaBin, m.noHTOverrides = p, bin, false
		if !halow {
			ok, err := wpa.HTOverrides(d.ctx, p, d.opts.RunDir)
			if err != nil {
				d.log.Warn("could not probe wpa_supplicant for HT overrides; assuming it has them", "path", p, "err", err)
			}
			m.noHTOverrides = !ok
			if !ok {
				d.log.Info("wpa_supplicant has no HT overrides (CONFIG_HT_OVERRIDES): leaving out disable_ht40", "path", p)
			}
		}
	}
	profile := d.profile
	mc := wpa.MeshConfig{
		CtrlDir: filepath.Join(d.opts.RunDir, "wpa"), MeshID: profile.MeshID, Passphrase: profile.SAEPassphrase,
		NoHTOverrides: m.noHTOverrides, Country: profile.Regulatory.Country,
	}
	freq := 0
	if halow {
		ch, err := config.S1GChannelInfo(profile.Regulatory.Country, rp.Width, rp.Channel)
		if err != nil {
			return res, err
		}
		primWidth, primIdx := wpa.S1GPrimary(rp.Width)
		mc.S1G = &wpa.S1GParams{Country: profile.Regulatory.Country, OpClass: ch.OpClass, Channel: ch.Channel,
			PrimaryWidthMHz: primWidth, Primary1MHzIdx: primIdx}
		// 1000 TU, wpa_supplicant's mesh default and what Morse Micro's own
		// mesh configurations use. A HaLow beacon is slow (it goes out at the
		// lowest rate of a 1 or 2 MHz channel), so ten a second from every
		// node would cost a large mesh much of its airtime; and no client AP
		// shares a HaLow radio, so nothing needs the two intervals to match.
		mc.BeaconIntTU = 1000
	} else {
		f, err := config.ChannelFrequencyMHz(rp.Band, rp.Channel)
		if err != nil {
			return res, err
		}
		freq = f
		// Beacon interval 100 TU, the same as the client AP's: drivers that
		// require all beaconing interfaces on a radio to agree ("STA/AP BI
		// must match" in iw phy) refuse to start the AP next to a mesh
		// beaconing at wpa_supplicant's mesh default of 1000 TU.
		mc.FreqMHz, mc.WidthMHz, mc.BeaconIntTU = f, rp.Width, 100
	}
	conf := wpa.Render(mc)
	if m.noHTOverrides && rp.Width <= 20 && freq > 5000 {
		warn("warning", "mesh-width", "%s: this node's wpa_supplicant (%s) was built without HT overrides (CONFIG_HT_OVERRIDES), so it can't hold "+
			"the %d MHz mesh to the profile's 20 MHz: expect 40 MHz, which nodes running 20 MHz may not peer with. "+
			"Use a wpa_supplicant built with CONFIG_HT_OVERRIDES=y (Debian and Ubuntu packages are), or a 40 MHz profile",
			meshIfname, m.wpaPath, freq)
	}
	restart := false
	if cur, err := os.ReadFile(m.confPath()); err != nil || string(cur) != conf {
		if err := os.WriteFile(m.confPath(), []byte(conf), 0o600); err != nil {
			return res, fmt.Errorf("write %s: %w", m.confPath(), err)
		}
		d.log.Info("wrote wpa_supplicant config", "path", m.confPath(), "mesh_id", profile.MeshID, "band", rp.Band, "channel", rp.Channel, "width", rp.Width)
		restart = m.sup != nil
		res.Changed = true
	}
	argv := []string{m.wpaPath, "-i", meshIfname, "-D", "nl80211", "-c", m.confPath()}
	reason := "configuration changed"
	// A radio that re-enumerates (a USB reset) takes its interfaces with it,
	// and meshd makes them again under the same names. wpa_supplicant
	// usually exits when its interface goes; when the new one appears before
	// it has noticed, it stays attached to nothing and scans for ever.
	if m.sup != nil && !restart && m.supIfindex != 0 && meshIfindex != 0 && meshIfindex != m.supIfindex {
		restart, reason = true, "the interface was created again"
	}
	if m.sup != nil && (restart || !m.sup.SameArgv(argv)) {
		d.log.Info("restarting wpa_supplicant", "interface", meshIfname, "reason", reason)
		m.stop()
		res.Changed = true
	}
	if m.sup == nil {
		m.sup = proc.New(d.log, m.wpaBin, argv...)
		if err := m.sup.Start(d.ctx); err != nil {
			m.sup = nil
			return res, fmt.Errorf("start wpa_supplicant: %w", err)
		}
		m.supSince, m.supIfindex = time.Now(), meshIfindex
		res.Changed = true
	}

	sup := status.Supplicant{Running: m.sup.Alive(), PID: m.sup.PID(), Restarts: m.sup.Restarts(), State: "starting"}
	if m.ctrl == nil {
		c, err := wpa.DialCtrl(m.ctrlPath())
		if err != nil {
			res.RequeueAfter = time.Second
			m.setFacts(sup, 0, 0, 0)
			return res, nil
		}
		m.ctrl = c
	}
	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	st, err := m.ctrl.GetStatus(rctx)
	cancel()
	if err != nil {
		m.ctrl.Close()
		m.ctrl = nil
		m.stopEvents()
		res.RequeueAfter = time.Second
		m.setFacts(sup, 0, 0, 0)
		return res, nil
	}
	sup.State, sup.Joined = st.State(), st.Joined()
	// Lift the SAE blocks that are due: dropping the peer makes
	// wpa_supplicant start afresh with it at its next beacon.
	for _, addr := range m.blocks.due(time.Now()) {
		rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		reply, err := m.ctrl.Request(rctx, "MESH_PEER_REMOVE "+addr)
		cancel()
		if err != nil || reply != "OK" {
			d.log.Info("could not lift the SAE block (the peer may be gone)", "interface", meshIfname, "peer", addr, "reply", reply, "err", err)
			continue
		}
		d.log.Info("lifted the SAE block on a peer; authentication starts again", "interface", meshIfname, "peer", addr)
		res.Changed = true
	}
	if m.events == nil {
		if ec, err := wpa.DialEvents(m.ctrlPath()); err == nil {
			m.events = ec
			m.evStop = make(chan struct{})
			go m.eventLoop(ec, m.evStop)
		}
	}

	txDBm, width := 0.0, 0
	if in, err := d.nl.InterfaceByIndex(meshIfindex); err == nil {
		if in.HasTXPower {
			txDBm = float64(in.TXPowerMBm) / 100
		}
		// HaLow's driver reports the 5 GHz channel it maps the S1G one to,
		// whose width says nothing about the S1G width.
		if in.FreqMHz != 0 && !halow {
			width = nl80211.ChanWidthMHz(in.ChannelWidth)
		}
	}
	if sup.Joined && width != 0 && width != rp.Width {
		why := ""
		switch {
		case width < rp.Width && rp.Width == 160:
			why = ": wpa_supplicant 2.10 builds a 160 MHz mesh only from channel 36 or 104 (2.12 also from other blocks on an 802.11ax radio), " +
				"and only where the radio and regulatory domain " + d.profile.Regulatory.Country + " allow every channel of the block"
		case width < rp.Width:
			why = ": the radio, or the regulatory domain " + d.profile.Regulatory.Country + ", does not allow the wider channel here"
		case m.noHTOverrides:
			why = ": this node's wpa_supplicant has no HT overrides to hold it narrower"
		}
		warn("warning", "mesh-width", "%s runs at %d MHz, not the profile's %d MHz%s", meshIfname, width, rp.Width, why)
	}

	if sup.Joined && !halow {
		// Judged at the width the link runs at: wpa_supplicant may have
		// narrowed it, and a narrower channel needs less signal.
		w := rp.Width
		if width != 0 {
			w = width
		}
		m.checkOneSided(meshIfindex, meshIfname, rp.Band, w, warn)
	}
	if sup.Restarts > 3 {
		warn("warning", "supplicant-restarting", "wpa_supplicant has restarted %d times; see the journal", sup.Restarts)
	}
	if !sup.Joined {
		res.RequeueAfter = 2 * time.Second
		if time.Since(m.supSince) > 20*time.Second {
			warn("warning", "mesh-not-joined", "mesh interface %s is not joined after %s (wpa_state=%s)", meshIfname, time.Since(m.supSince).Round(time.Second), sup.State)
		}
	}
	m.setFacts(sup, st.Freq(), width, txDBm)
	return res, nil
}

func (m *meshLinkReconciler) eventLoop(ec *wpa.EventConn, stop chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		ev, err := ec.Next(2 * time.Second)
		if err != nil {
			if wpa.IsTimeout(err) {
				continue
			}
			return
		}
		switch ev.Name {
		case wpa.EvMeshPeerConnected:
			m.d.log.Info("mesh peer connected", "interface", m.ifname(), "peer", ev.Arg())
			m.blocks.connected(ev.Arg())
			m.d.Kick("peer connected")
		case wpa.EvMeshPeerDisconnected:
			m.d.log.Info("mesh peer disconnected", "interface", m.ifname(), "peer", ev.Arg())
			m.d.Kick("peer disconnected")
		case wpa.EvMeshGroupStarted:
			m.d.log.Info("mesh group started", "interface", m.ifname(), "mesh_id", ev.KV["ssid"])
			m.d.Kick("mesh started")
		case wpa.EvMeshGroupRemoved:
			if m.d.stopping() {
				m.d.log.Info("mesh group removed (meshd stopping)", "interface", m.ifname())
			} else {
				m.d.log.Warn("mesh group removed", "interface", m.ifname())
			}
			m.d.Kick("mesh removed")
		case wpa.EvMeshSAEAuthFailure:
			// Not always a wrong passphrase: a peer that did not answer
			// (off the channel, restarting, or blocking this node) fails
			// the same way.
			m.d.log.Warn("SAE authentication failed: the peer did not answer, or has a different passphrase", "interface", m.ifname(), "peer", ev.KV["addr"])
			m.d.facts.AddSAEFailure(ev.KV["addr"], "failure", "SAE authentication failed (no answer, or another passphrase)")
			// One cause is a leftover entry of this peer on another link
			// (peermove.go): look now, not at the next slow pass.
			m.d.Kick("SAE failure")
		case wpa.EvMeshSAEAuthBlocked:
			addr := ev.KV["addr"]
			if delay, ok := m.blocks.blocked(addr, time.Now(), rand.N(saeLiftSpread)); ok {
				m.d.log.Warn("SAE authentication blocked after repeated failures; meshd lifts the block early", "interface", m.ifname(), "peer", addr, "seconds", ev.KV["duration"], "lift_in", delay.Round(time.Second))
				m.d.facts.AddSAEFailure(addr, "blocked", "SAE blocked after repeated failures; tried again in "+delay.Round(time.Second).String())
				time.AfterFunc(delay+100*time.Millisecond, func() { m.d.Kick("lift SAE block") })
			} else {
				m.d.log.Warn("SAE authentication blocked after repeated failures; left blocked, this peer has failed through every early retry", "interface", m.ifname(), "peer", addr, "seconds", ev.KV["duration"])
				m.d.facts.AddSAEFailure(addr, "blocked", "SAE blocked for "+ev.KV["duration"]+" s after repeated failures")
			}
		}
	}
}

func (m *meshLinkReconciler) stopEvents() {
	if m.events != nil {
		close(m.evStop)
		m.events.Close()
		m.events = nil
	}
}

func (m *meshLinkReconciler) stop() {
	m.stopEvents()
	m.blocks.reset()
	if m.ctrl != nil {
		m.ctrl.Close()
		m.ctrl = nil
	}
	if m.sup != nil {
		_ = m.sup.Stop(5 * time.Second)
		m.sup = nil
	}
}

func (m *meshLinkReconciler) Teardown(ctx context.Context) error {
	m.stop()
	_ = os.Remove(m.confPath())
	return nil
}
