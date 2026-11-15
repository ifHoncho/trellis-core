package meshd

import (
	"fmt"
	"net"
	"path/filepath"
	"time"

	"meridian/trellis/internal/config"
	"meridian/trellis/internal/gossip"
)

// Two parts of a mesh that each changed the profile while apart.
//
// Every mesh-wide change carries an ID and the IDs of the changes before it
// (config.ChangeInfo), and gossip carries each node's current change ID. A
// node that hears of a change it does not know asks for that profile. If
// neither profile knows the other's change, the mesh was changed apart:
// config.Reconcile keeps the later change as a new revision above both,
// which every node makes the same from the two profiles. The node that
// found the conflict sends the result to every node it can reach, as a
// mesh-wide change that switches shortly; nodes that get it take it as an
// ordinary newer change. The dropped profile is kept on the node, and an
// alert says what was dropped, so the operator can send it again.

const (
	// meshConflictSwitch is how soon the reconciled profile takes effect:
	// time for the nodes in reach to get it, short because the two parts
	// may be meeting only through a node that is visiting (findmesh.go).
	meshConflictSwitch = 15 * time.Second
	// meshConflictShowFor is how long the alert stays.
	meshConflictShowFor = 24 * time.Hour
	// meshConflictFile keeps the dropped profile (it holds the secret).
	meshConflictFile = "profile-dropped.yaml"
)

// meshConflict is the last conflict this node settled or took part in.
type meshConflict struct {
	at            time.Time
	kept, dropped *config.Profile
	file          string
}

// latestProfile is the newest profile this node has: a mesh-wide change
// waiting to switch, or the one it runs.
func (d *Daemon) latestProfile() *config.Profile {
	_, cur := d.conf()
	m := &d.meshChanges
	m.mu.Lock()
	c := m.cur
	m.mu.Unlock()
	if c != nil && c.State == "pending" && c.Rev > cur.Revision {
		if p, err := config.ParseProfile([]byte(c.YAML)); err == nil {
			return p
		}
	}
	return cur
}

// askHoldoff is how long a node waits before asking about a change it does
// not know: a few seconds, different per node, so that when two parts of a
// mesh meet the first to settle it tells the others, rather than every node
// settling it and telling everyone.
func askHoldoff(nodeID int) time.Duration {
	return 2*time.Second + time.Duration(nodeID%8)*time.Second
}

// reconcileProfiles settles a conflict between this node's newest profile
// and theirs, from the node callsign, and sends the result to every node
// in reach.
func (d *Daemon) reconcileProfiles(mine, theirs *config.Profile, from string) error {
	merged, kept, dropped, err := config.Reconcile(mine, theirs)
	if err != nil {
		return err
	}
	if l := d.latestProfile(); l.ChangeID() == merged.ChangeID() {
		return nil // settled already, here or by another node
	}
	data, err := merged.YAML("")
	if err != nil {
		return err
	}
	cur, _ := d.conf()
	file := filepath.Join(d.opts.StateDir, meshConflictFile)
	hdr := fmt.Sprintf("Dropped %s: two parts of the mesh changed the profile while apart.\nThis is the change from %s at %s (revision %d); the one from %s at %s was kept.\nTo use it after all, send it again: Settings → Mesh profile, or meshctl profile push.",
		stamp(), changeBy(dropped), dropped.Change.At.Format(time.RFC3339), dropped.Revision, changeBy(kept), kept.Change.At.Format(time.RFC3339))
	if y, err := dropped.YAML(hdr); err == nil {
		if err := writeFileAtomic(file, y, 0o600); err != nil {
			d.log.Warn("cannot keep the dropped profile", "err", err)
			file = ""
		}
	}
	d.log.Warn("two parts of the mesh changed the profile while apart: the later change is kept, as a new revision for every node",
		"kept_from", changeBy(kept), "kept_at", kept.Change.At.Format(time.RFC3339), "dropped_from", changeBy(dropped), "dropped_at", dropped.Change.At.Format(time.RFC3339),
		"revision", merged.Revision, "found_through", from)

	c := &meshChange{Rev: merged.Revision, ApplyAt: time.Now().Add(meshConflictSwitch).Truncate(time.Second), From: cur.Callsign, Sender: true, YAML: string(data), State: "pending"}
	if err := d.acceptMeshChange(c); err != nil {
		return err
	}
	m := &d.meshChanges
	m.mu.Lock()
	m.conflict = &meshConflict{at: time.Now(), kept: kept, dropped: dropped, file: file}
	m.targets = nil
	if d.peers != nil {
		for _, peer := range d.peers.Peers() {
			r := peer.Record
			if ip := net.ParseIP(r.Addr); ip != nil && meshChangeFromPeer(r) {
				m.targets = append(m.targets, &meshChangeTarget{callsign: r.Callsign, mac: r.MAC, ip: ip, state: "sent"})
			}
		}
	}
	if m.stop != nil {
		close(m.stop)
	}
	stop := make(chan struct{})
	m.stop = stop
	m.mu.Unlock()
	if d.gossip != nil {
		go d.sendMeshChange(c, stop)
	}
	return nil
}

// changeBy names who made a change, for messages.
func changeBy(p *config.Profile) string {
	if p.Change != nil && p.Change.By != "" {
		return p.Change.By
	}
	return "another node"
}

// meshConflictWarning is the alert after a conflict was settled.
func (d *Daemon) meshConflictWarning() (string, bool) {
	m := &d.meshChanges
	m.mu.Lock()
	c := m.conflict
	m.mu.Unlock()
	if c == nil || time.Since(c.at) > meshConflictShowFor {
		return "", false
	}
	msg := fmt.Sprintf("two parts of the mesh changed its profile while apart: %s's change (%s) and %s's (%s). The later one, %s's, is in force on every node that was in reach; %s's settings were dropped",
		changeBy(c.kept), c.kept.Change.At.Local().Format("Jan 2 15:04"), changeBy(c.dropped), c.dropped.Change.At.Local().Format("Jan 2 15:04"),
		changeBy(c.kept), changeBy(c.dropped))
	if c.file != "" {
		msg += fmt.Sprintf(" and are kept in %s: send them again from Settings → Mesh profile if they are still wanted", c.file)
	}
	return msg, true
}

// unknownChange reports whether a peer runs a mesh-wide change this node
// does not know (ahead of it, or made apart from it).
func unknownChange(mine *config.Profile, r gossip.Record) bool {
	return r.ProfileChange != "" && mine.ChangeID() != "" && !mine.Knows(r.ProfileChange)
}
