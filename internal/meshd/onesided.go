package meshd

import (
	"fmt"
	"time"

	"meridian/trellis/internal/nl80211"
)

// A mesh peer link can be established and still carry nothing: this node
// hears the peer, the peer barely hears this node. Peering frames get
// through on retries, data does not. The usual cause is a pair out of range
// on that band in one direction, typically where one end transmits much
// more strongly (30 dBm against 16 dBm leaves the weaker end heard near the
// noise floor while it hears the other well). meshd reports such a link
// rather than show it as fine.

// oneSidedMinFrames is how many frames, sent since the last look, it takes
// to judge a link by its failures: fewer say nothing. Failure counts are
// not to be relied on alone: some USB adapters (mt76x0u) report frames as
// sent that never arrived.
const oneSidedMinFrames = 20

// signalFloorDBm is the weakest average signal at which a link of this
// width still carries data at its lowest rate, with a few dB to spare:
// about -92 dBm on 20 MHz, 3 dB less for each doubling of the width, as
// the noise grows with it.
func signalFloorDBm(widthMHz int) int {
	floor := -92
	for w := 20; w < widthMHz; w *= 2 {
		floor += 3
	}
	return floor
}

// peerTX is a peer's transmit counters at the last look.
type peerTX struct {
	packets, failed uint32
	at              time.Time
}

// oneSided judges one peer from its counters now and at the last look:
// whether most frames sent to it failed.
func oneSided(prev peerTX, st nl80211.Station) (bad bool, sent, failed uint32) {
	if st.TXPackets < prev.packets || st.TXFailed < prev.failed {
		return false, 0, 0 // counters reset: the peer entry is new
	}
	sent, failed = st.TXPackets-prev.packets, st.TXFailed-prev.failed
	return sent >= oneSidedMinFrames && failed*2 >= sent, sent, failed
}

// tooWeak reports whether this node hears a peer below the floor for the
// link's width: then the link cannot carry data, whatever it shows.
func tooWeak(st nl80211.Station, widthMHz int) bool {
	return st.HasSignal && st.SignalAvgDBm != 0 && int(st.SignalAvgDBm) < signalFloorDBm(widthMHz)
}

// checkOneSided looks at every established peer on the link and warns for
// those that fail more often than not. name gives a peer's callsign.
func (m *meshLinkReconciler) checkOneSided(ifindex int, ifname, band string, widthMHz int, warn func(level, code, format string, args ...any)) {
	d := m.d
	if d.nl == nil || ifindex == 0 {
		return
	}
	stations, err := d.nl.Stations(ifindex)
	if err != nil {
		return
	}
	if m.peerTX == nil {
		m.peerTX = map[string]peerTX{}
	}
	now := time.Now()
	seen := map[string]bool{}
	for _, st := range stations {
		mac := st.MAC.String()
		seen[mac] = true
		if !st.Established() {
			delete(m.peerTX, mac)
			continue
		}
		prev, ok := m.peerTX[mac]
		// Compare over half a minute or more, so a burst of retries while
		// a peer moves or restarts does not count.
		if ok && now.Sub(prev.at) < 30*time.Second {
			if m.oneSided[mac] {
				warn("warning", "mesh-link-one-sided", "%s", m.oneSidedMsg[mac])
			}
			continue
		}
		m.peerTX[mac] = peerTX{packets: st.TXPackets, failed: st.TXFailed, at: now}
		if !ok {
			continue
		}
		failing, sent, failed := oneSided(prev, st)
		weak := tooWeak(st, widthMHz)
		bad := failing || weak
		if m.oneSided == nil {
			m.oneSided, m.oneSidedMsg = map[string]bool{}, map[string]string{}
		}
		was := m.oneSided[mac]
		m.oneSided[mac] = bad
		if !bad {
			if was {
				d.log.Info("mesh link carries traffic again", "interface", ifname, "peer", d.peerName(mac))
			}
			continue
		}
		var why string
		if weak {
			why = fmt.Sprintf("this node hears it at %d dBm on average, below the %d dBm needed to carry data on %d MHz", st.SignalAvgDBm, signalFloorDBm(widthMHz), widthMHz)
		} else {
			why = fmt.Sprintf("%d of %d frames sent to it in the last %s failed", failed, sent, now.Sub(prev.at).Round(time.Second))
			if st.HasSignal {
				why += fmt.Sprintf(" (this node hears it at %d dBm)", st.SignalAvgDBm)
			}
		}
		msg := fmt.Sprintf("the %s link to %s on %s is established but cannot carry traffic: %s. "+
			"The two are at the edge of range on this band, often in one direction only, when one transmits much more strongly than the other. "+
			"Traffic goes around this link where it can. Move them closer, use a narrower width or better antennas, or let this pair meet on a band that reaches further (2.4 GHz, HaLow)",
			bandName(band), d.peerName(mac), ifname, why)
		m.oneSidedMsg[mac] = msg
		if !was {
			d.log.Warn("mesh link established but cannot carry traffic", "interface", ifname, "peer", d.peerName(mac), "signal_avg_dbm", st.SignalAvgDBm, "width_mhz", widthMHz, "failed", failed, "sent", sent)
		}
		warn("warning", "mesh-link-one-sided", "%s", msg)
	}
	for mac := range m.peerTX {
		if !seen[mac] {
			delete(m.peerTX, mac)
			delete(m.oneSided, mac)
			delete(m.oneSidedMsg, mac)
		}
	}
}

// peerName is a peer's callsign where gossip knows it, else its address.
func (d *Daemon) peerName(mac string) string {
	if d.peers != nil {
		if p, ok := d.peers.ByOriginator(mac); ok && p.Record.Callsign != "" {
			return p.Record.Callsign
		}
	}
	return mac
}
