package meshd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"meridian/trellis/internal/gossip"
)

// The roster: the other nodes this node has heard lately, kept across
// restarts. Gossip forgets a node within two minutes of last hearing it;
// the roster remembers it for a day, so that a node can tell that most of
// the mesh it was part of is missing (findmesh.go), and a mesh-wide change
// can name the nodes that will miss it.
//
// Ages are kept relative to the moment the file was written, never as
// times of day: a node without a real-time clock starts from a wrong one.
// Time switched off does not count.

const (
	rosterFile      = "known-nodes.json"
	rosterKeep      = 24 * time.Hour  // a node unheard this long is forgotten
	rosterSaveEvery = 5 * time.Minute // the file is written at most this often
	rosterMax       = 256             // nodes kept at most (the mesh is 1–50)
)

type roster struct {
	mu    sync.Mutex
	nodes map[string]rosterNode // by identity MAC
	saved time.Time
}

type rosterNode struct {
	callsign string
	seen     time.Time
}

type rosterEntry struct {
	MAC      string `json:"mac"`
	Callsign string `json:"callsign"`
	AgeS     int64  `json:"age_s"`
}

// loadRoster reads the roster written before the last stop.
func (d *Daemon) loadRoster() {
	r := &d.roster
	data, err := os.ReadFile(filepath.Join(d.opts.StateDir, rosterFile))
	if err != nil {
		return
	}
	var es []rosterEntry
	if json.Unmarshal(data, &es) != nil {
		return
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.nodes == nil {
		r.nodes = map[string]rosterNode{}
	}
	for _, e := range es {
		age := time.Duration(e.AgeS) * time.Second
		if e.MAC == "" || age < 0 || age > rosterKeep || len(r.nodes) >= rosterMax {
			continue
		}
		r.nodes[e.MAC] = rosterNode{callsign: e.Callsign, seen: now.Add(-age)}
	}
	r.saved = now
}

// noteRoster records the nodes gossip hears now, forgets old ones, and
// writes the file when it is due (or when final, at a stop).
func (d *Daemon) noteRoster(peers []gossip.Peer, final bool) {
	r := &d.roster
	now := time.Now()
	r.mu.Lock()
	if r.nodes == nil {
		r.nodes = map[string]rosterNode{}
	}
	for _, p := range peers {
		if _, ok := r.nodes[p.Record.MAC]; ok || len(r.nodes) < rosterMax {
			r.nodes[p.Record.MAC] = rosterNode{callsign: p.Record.Callsign, seen: now}
		}
	}
	for mac, n := range r.nodes {
		if now.Sub(n.seen) > rosterKeep {
			delete(r.nodes, mac)
		}
	}
	due := final || now.Sub(r.saved) >= rosterSaveEvery
	var es []rosterEntry
	if due {
		r.saved = now
		for mac, n := range r.nodes {
			es = append(es, rosterEntry{MAC: mac, Callsign: n.callsign, AgeS: int64(now.Sub(n.seen) / time.Second)})
		}
		sort.Slice(es, func(i, j int) bool { return es[i].MAC < es[j].MAC })
	}
	r.mu.Unlock()
	if !due {
		return
	}
	data, err := json.Marshal(es)
	if err == nil {
		err = writeFileAtomic(filepath.Join(d.opts.StateDir, rosterFile), data, 0o644)
	}
	if err != nil {
		d.log.Debug("cannot write the roster", "err", err)
	}
}

// rosterKnown is the other nodes heard in the last day, by MAC: callsign.
func (d *Daemon) rosterKnown() map[string]string {
	r := &d.roster
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]string{}
	for mac, n := range r.nodes {
		if time.Since(n.seen) <= rosterKeep {
			out[mac] = n.callsign
		}
	}
	return out
}
