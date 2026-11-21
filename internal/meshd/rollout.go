package meshd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"meridian/trellis/internal/gossip"
	"meridian/trellis/internal/reconcile"
	"meridian/trellis/internal/release"
	"meridian/trellis/internal/rollout"
	"meridian/trellis/internal/status"
	"meridian/trellis/internal/upgrade"
)

// Rolling mesh-wide upgrades (internal/rollout). The node the operator
// gives the release to (the origin) sends every node the rollout; each
// node keeps it on disk, fetches and checks its package, waits for its
// turn by what the others say in gossip, upgrades, and says how it went.
// The origin sends again to a node until that node's gossip names the
// rollout; a node that sees a rollout in another node's gossip that it
// does not know asks that node for it.

const (
	rolloutFile = "rollout.json"
	// The origin sends every rolloutResend while the rollout is new (or
	// newly cancelled), then every rolloutResendSlow to a node still silent.
	rolloutResend     = 3 * time.Second
	rolloutResendSlow = 30 * time.Second
	rolloutResendFast = 2 * time.Minute
	rolloutAskEvery   = 10 * time.Second
	rolloutPass       = 3 * time.Second // reconcile pass while a rollout is on
	// rolloutKeep: a rollout that has not ended this long after it started
	// is dropped (but not while this node upgrades).
	rolloutKeep = 24 * time.Hour
	// rolloutShowFor: an ended rollout stays in status, in gossip and on
	// disk this long, then is dropped and its packages removed.
	rolloutShowFor = time.Hour
	// rolloutStaged: a package uploaded for a rollout that was never
	// started is kept this long.
	rolloutStaged     = time.Hour
	rolloutFetchRetry = 15 * time.Second
	// rolloutNoResult: an upgrade that is not running and left no result
	// this long after it was started has failed.
	rolloutNoResult = 2 * time.Minute
)

// rolloutRecord is this node's part in a rollout, kept on disk.
type rolloutRecord struct {
	Rollout rollout.Rollout `json:"rollout"`
	Phase   rollout.Phase   `json:"phase,omitempty"`
	Reason  string          `json:"reason,omitempty"`
	Since   time.Time       `json:"since"`
	// To is the package version this node installs; UpgradeAt when it
	// started installing it.
	To        string    `json:"to,omitempty"`
	UpgradeAt time.Time `json:"upgrade_at,omitzero"`
	// Sender: this node is the origin.
	Sender bool `json:"sender,omitempty"`
	// Ended: every node in the order finished, as this node saw it.
	Ended time.Time `json:"ended,omitzero"`
	// Dropped: over; kept so that the same rollout is not taken again.
	Dropped bool `json:"dropped,omitempty"`
	// Received is when this node got the rollout, by its own clock (the
	// origin's start time is by the origin's): turns and expiry count from
	// it. Zero in a record kept by an earlier build: Started is used.
	Received time.Time `json:"received,omitzero"`
}

// received is when this node got the rollout.
func (c *rolloutRecord) received() time.Time {
	if c.Received.IsZero() {
		return c.Rollout.Started
	}
	return c.Received
}

type rolloutState struct {
	mu  sync.Mutex
	cur *rolloutRecord
	// others is the other nodes' progress in cur, by identity MAC, as last
	// heard (a node upgrading goes silent for a while).
	others   map[string]rollout.State
	wait     string // why this node waits, while it does
	fetchErr string
	fetching bool
	// notMine: rollouts this node is not in, or too old, by ID.
	notMine  map[string]bool
	asked    time.Time
	answered map[string]time.Time // asks answered, by asker
	sent     map[string]time.Time // the origin's last send, by MAC
	fastTill time.Time            // the origin sends every rolloutResend until then

	srv     *http.Server
	srvAddr string

	// names: callsigns of the nodes seen in gossip, kept while one is away
	// (upgrading, restarting) so that progress still names it.
	names map[string]string

	upOnce sync.Once
	up     rolloutUpgrader

	// loadedAt: when this meshd took the rollout up again from disk. Its
	// view of the others starts empty then, so stalls count from here.
	loadedAt time.Time
}

// viewFrom is when this node could first have heard the others' progress
// in c: when it got the rollout, or, after a restart, when it took it up
// again (s.mu held). Stalls and the end count from it: a node that has
// not heard the others yet must not take them for silent.
func (s *rolloutState) viewFrom(c *rolloutRecord) time.Time {
	t := c.received()
	if s.loadedAt.After(t) {
		t = s.loadedAt
	}
	return t
}

// rolloutCheckEvery is how often the node re-reads whether it takes
// rollouts: a release key may be added, the package installed. A "no" is
// read again sooner: meshd restarts inside dpkg's configure step of an
// upgrade, when the package is "half-configured" for a few seconds.
const (
	rolloutCheckEvery = 5 * time.Minute
	rolloutRecheckNo  = 20 * time.Second
)

type rolloutSelf struct {
	mu        sync.Mutex
	arch      string
	accepts   bool
	why       string // why not, for status
	checkedAt time.Time
}

// debArch maps Go's architecture to Debian's, for a node without dpkg.
func debArch() string {
	switch runtime.GOARCH {
	case "arm":
		return "armhf"
	case "386":
		return "i386"
	}
	return runtime.GOARCH
}

// rolloutSelfCheck returns this node's Debian architecture and whether it
// takes mesh-wide upgrades (with why not), re-read at most every
// rolloutCheckEvery.
func (d *Daemon) rolloutSelfCheck(ctx context.Context) (arch string, accepts bool, why string) {
	if d.opts.TestUpgraderDir != "" {
		return d.rolloutUpgrader().SelfCheck(ctx)
	}
	s := &d.rolloutSelf
	s.mu.Lock()
	defer s.mu.Unlock()
	every := rolloutCheckEvery
	if !s.accepts {
		every = rolloutRecheckNo
	}
	if !s.checkedAt.IsZero() && time.Since(s.checkedAt) < every {
		return s.arch, s.accepts, s.why
	}
	s.checkedAt = time.Now()
	sys := d.upgradeSys()
	if s.arch == "" {
		s.arch = debArch()
		if out, err := sys.Run(ctx, "dpkg", "--print-architecture"); err == nil && strings.TrimSpace(out) != "" {
			s.arch = strings.TrimSpace(out)
		}
	}
	s.accepts, s.why = false, ""
	switch keys, err := release.TrustedKeys(release.KeyDir); {
	case d.packagedLayout() != nil:
		s.why = "meshd does not run with the installed package's paths"
	case !upgrade.QueryPackage(ctx, sys).Installed():
		s.why = "Trellis is not installed from its package"
	case err != nil || len(keys) == 0:
		s.why = "this node trusts no release key"
	default:
		s.accepts = true
	}
	return s.arch, s.accepts, s.why
}

func (d *Daemon) releasesDir() string { return filepath.Join(d.upgradeDir(), "releases") }

func (d *Daemon) saveRollout(c *rolloutRecord) error {
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(d.opts.StateDir, rolloutFile), data, 0o600)
}

// loadRollout picks up the rollout this node had before a restart (the
// upgrade itself restarts meshd).
func (d *Daemon) loadRollout() {
	data, err := os.ReadFile(filepath.Join(d.opts.StateDir, rolloutFile))
	if err != nil {
		return
	}
	var c rolloutRecord
	if err := json.Unmarshal(data, &c); err != nil || c.Rollout.Validate() != nil {
		d.log.Warn("the kept mesh-wide upgrade cannot be read; dropped", "err", err)
		_ = os.Remove(filepath.Join(d.opts.StateDir, rolloutFile))
		return
	}
	s := &d.rollouts
	s.mu.Lock()
	s.cur, s.others, s.loadedAt = &c, map[string]rollout.State{}, time.Now()
	if c.Sender {
		s.fastTill = time.Now().Add(rolloutResendFast)
	}
	s.mu.Unlock()
	if !c.Dropped {
		d.log.Info("mesh-wide upgrade kept from before the restart", "id", c.Rollout.ID, "version", c.Rollout.Version, "phase", c.Phase)
	}
}

// setPhase moves this node to phase p (s.mu held) and keeps it on disk.
func (d *Daemon) setPhase(c *rolloutRecord, p rollout.Phase, reason string) {
	if c.Phase == p && c.Reason == reason {
		return
	}
	c.Phase, c.Reason, c.Since = p, gossip.TrimErr(reason), time.Now()
	if err := d.saveRollout(c); err != nil {
		d.log.Error("cannot keep the mesh-wide upgrade's state", "err", err)
	}
	d.log.Info("mesh-wide upgrade", "id", c.Rollout.ID, "version", c.Rollout.Version, "phase", p, "reason", reason)
	d.Kick("mesh-wide upgrade: " + string(p))
}

// rolloutGossip is what this node says about the rollout in gossip.
func (d *Daemon) rolloutGossip() *gossip.RolloutState {
	s := &d.rollouts
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.cur
	if c == nil || c.Dropped || c.Phase == "" {
		return nil
	}
	reason := c.Reason
	if len(reason) > gossip.MaxRolloutReason {
		reason = reason[:gossip.MaxRolloutReason-3] + "..."
	}
	return &gossip.RolloutState{ID: c.Rollout.ID, Phase: string(c.Phase), Reason: reason}
}

// rolloutActive reports whether a rollout is under way (not ended).
func (d *Daemon) rolloutActive() bool {
	s := &d.rollouts
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur != nil && !s.cur.Dropped && s.cur.Ended.IsZero()
}

// rolloutReconciler drives this node's part in a rollout. It runs after
// gossip, and every rolloutPass while a rollout is on.
type rolloutReconciler struct{ d *Daemon }

func (r *rolloutReconciler) Name() string { return "rollout" }

func (r *rolloutReconciler) Teardown(ctx context.Context) error {
	r.d.stopRolloutServer()
	return nil
}

func (r *rolloutReconciler) Reconcile(ctx context.Context) (reconcile.Result, error) {
	d := r.d
	var res reconcile.Result
	if d.gossip == nil || d.peers == nil {
		return res, nil
	}
	key := d.gossip.currentKey()
	me := d.identity()
	if key == nil || me == nil {
		d.stopRolloutServer()
		return res, nil
	}
	mac := strings.ToLower(me.MAC)
	d.askForRollout(key, mac)
	s := &d.rollouts
	s.mu.Lock()
	c := s.cur
	if c == nil || c.Dropped {
		s.mu.Unlock()
		d.stopRolloutServer()
		d.pruneReleases()
		return res, nil
	}
	now := time.Now()
	d.noteOthers(c, now)
	d.rolloutStep(ctx, c, mac, now)
	// Over: every node in the order finished, or it stopped and the nodes
	// still out have been silent for the stall time.
	if c.Ended.IsZero() && rollout.Over(&c.Rollout, mac, c.Phase, s.viewFrom(c), s.others, now) {
		c.Ended = now
		_ = d.saveRollout(c)
		d.log.Info("mesh-wide upgrade over", "id", c.Rollout.ID, "version", c.Rollout.Version)
	}
	drop := (!c.Ended.IsZero() && now.Sub(c.Ended) > rolloutShowFor) ||
		(now.Sub(c.received()) > rolloutKeep && c.Phase != rollout.Upgrading)
	if drop {
		c.Dropped = true
		_ = d.saveRollout(c)
		d.log.Info("mesh-wide upgrade dropped", "id", c.Rollout.ID)
	}
	sender := c.Sender && !c.Dropped && c.Ended.IsZero()
	var send *rolloutRecord
	if sender {
		cp := *c
		send = &cp
	}
	s.mu.Unlock()
	if send != nil {
		d.sendRollout(key, send, mac)
	}
	if drop {
		d.stopRolloutServer()
		d.pruneReleases()
		return res, nil
	}
	if err := d.ensureRolloutServer(); err != nil {
		res.Warnings = append(res.Warnings, reconcile.Warning{Level: "warning", Code: "rollout-serve",
			Message: fmt.Sprintf("Cannot serve the mesh upgrade package to other nodes: %v", err)})
	}
	// Quick passes while it runs; once over, it is only shown.
	s.mu.Lock()
	running := s.cur != nil && !s.cur.Dropped && s.cur.Ended.IsZero()
	s.mu.Unlock()
	if running {
		res.RequeueAfter = rolloutPass
	}
	return res, nil
}

// noteOthers reads the other nodes' progress from gossip (s.mu held).
func (d *Daemon) noteOthers(c *rolloutRecord, now time.Time) {
	s := &d.rollouts
	if s.others == nil {
		s.others = map[string]rollout.State{}
	}
	for _, p := range d.peers.Peers() {
		ro := p.Record.Rollout
		if ro == nil || ro.ID != c.Rollout.ID {
			continue
		}
		m := strings.ToLower(p.Record.MAC)
		st := s.others[m]
		if string(st.Phase) != ro.Phase || st.Reason != ro.Reason {
			s.others[m] = rollout.State{Phase: rollout.Phase(ro.Phase), Reason: ro.Reason, Since: now}
		}
	}
}

// rolloutName is a node's callsign for the reasons, or its MAC (s.mu
// held).
func (d *Daemon) rolloutName(mac string) string {
	s := &d.rollouts
	if p, ok := d.peers.ByMAC(mac); ok && p.Record.Callsign != "" {
		if s.names == nil {
			s.names = map[string]string{}
		}
		s.names[strings.ToLower(mac)] = p.Record.Callsign
		return p.Record.Callsign
	}
	if n, ok := s.names[strings.ToLower(mac)]; ok {
		return n
	}
	if me := d.identity(); me != nil && strings.EqualFold(me.MAC, mac) {
		cur, _ := d.conf()
		return cur.Callsign
	}
	return mac
}

// rolloutStep moves this node's part on (s.mu held; let go around the
// upgrade's start).
func (d *Daemon) rolloutStep(ctx context.Context, c *rolloutRecord, me string, now time.Time) {
	s := &d.rollouts
	up := d.rolloutUpgrader()
	if !slices.Contains(c.Rollout.Order, me) {
		// The origin, when it cannot take the release itself: it still
		// sends and serves it.
		if c.Phase == "" {
			d.setPhase(c, rollout.Skipped, "this node does not take the release itself")
		}
		return
	}
	switch c.Phase {
	case "":
		arch, accepts, why := up.SelfCheck(ctx)
		switch {
		case !accepts:
			d.setPhase(c, rollout.Skipped, why)
		case c.Rollout.Package(arch) == nil:
			d.setPhase(c, rollout.Skipped, "no package for "+arch)
		default:
			d.setPhase(c, rollout.Fetching, "")
		}
	case rollout.Fetching, rollout.Ready:
		dec := rollout.Turn(&c.Rollout, me, s.viewFrom(c), s.others, d.rolloutName, now)
		if dec.Action == rollout.Halt {
			d.setPhase(c, rollout.Halted, dec.Why)
			return
		}
		if c.Phase == rollout.Fetching {
			if !s.fetching {
				s.fetching = true
				arch, _, _ := up.SelfCheck(ctx)
				go d.fetchRollout(c.Rollout, *c.Rollout.Package(arch), me)
			}
			s.wait = ""
			return
		}
		if dec.Action == rollout.Wait {
			s.wait = dec.Why
			return
		}
		// Start only on what the others say now. A node back in reach can
		// get the rollout (a resend held while it was away) before it hears
		// the others again, and a node restarted while ready has heard no
		// one: its view then cannot show that the rollout halted, and,
		// first in the order, it would upgrade by itself (lab R6.2).
		if who := d.rolloutUnheard(c, me, now); who != "" {
			s.wait = "Awaiting current state from " + who
			return
		}
		s.wait = ""
		arch, _, _ := up.SelfCheck(ctx)
		p := c.Rollout.Package(arch)
		if p == nil {
			d.setPhase(c, rollout.Failed, "no package for "+arch)
			return
		}
		c.UpgradeAt = now
		d.setPhase(c, rollout.Upgrading, "")
		id := c.Rollout.ID
		s.mu.Unlock()
		err := up.Start(ctx, rollout.File(d.releasesDir(), p.SHA256))
		s.mu.Lock()
		if err != nil && s.cur != nil && s.cur.Rollout.ID == id {
			d.setPhase(s.cur, rollout.Failed, "the upgrade did not start: "+err.Error())
		} else if err == nil {
			d.log.Info("mesh-wide upgrade: this node's turn; upgrading", "id", id, "to", c.To)
		}
	case rollout.Upgrading:
		running, r := up.Outcome(ctx, c.UpgradeAt)
		switch {
		case running:
		case r == nil:
			if now.Sub(c.UpgradeAt) > rolloutNoResult {
				d.setPhase(c, rollout.Failed, "the upgrade ended without a result")
			}
		case r.Result == upgrade.ResultUpgraded:
			d.setPhase(c, rollout.Done, "")
		case r.Result == upgrade.ResultRolledBack:
			d.setPhase(c, rollout.RolledBack, r.Reason)
		default:
			d.setPhase(c, rollout.Failed, strings.TrimSpace(r.Result+": "+r.Reason))
		}
	}
}

// rolloutHearWithin is how long after its view starts (viewFrom) a node
// waits to hear each other node in the order before it starts its own
// upgrade: every node in reach sends its record within a refresh. One not
// heard by then is taken as away.
const rolloutHearWithin = gossipRefresh + gossipJitter + 10*time.Second

// rolloutUnheard names a node in the rollout's order, other than this one,
// not heard since this node's view of the rollout began (viewFrom), or "".
// A node missing from the gossip table counts as not heard: the table is
// empty after a restart, and a node away longer than gossip.Expiry has
// forgotten everyone. Each blocks for rolloutHearWithin at most; a node
// whose old record is still in the table blocks until it is heard again
// or its record expires. (s.mu held.)
func (d *Daemon) rolloutUnheard(c *rolloutRecord, me string, now time.Time) string {
	from := d.rollouts.viewFrom(c)
	heard := map[string]time.Time{}
	for _, p := range d.peers.Peers() {
		heard[strings.ToLower(p.Record.MAC)] = p.LastHeard
	}
	waiting := now.Sub(from) < rolloutHearWithin
	for _, m := range c.Rollout.Order {
		if m == me {
			continue
		}
		at, inTable := heard[m]
		switch {
		case inTable && at.Before(from):
			return d.rolloutName(m)
		case !inTable && waiting:
			return d.rolloutName(m)
		}
	}
	return ""
}

// fetchRollout gets this node's package, from the origin or a node of the
// same architecture that has it, and checks it. It retries until it has
// it, or the rollout is no longer this node's to fetch.
func (d *Daemon) fetchRollout(r rollout.Rollout, p rollout.Package, me string) {
	s := &d.rollouts
	defer func() {
		s.mu.Lock()
		s.fetching = false
		s.mu.Unlock()
	}()
	// Straight to the node: never through a proxy from the environment.
	client := &http.Client{Timeout: 10 * time.Minute, Transport: &http.Transport{Proxy: nil, ResponseHeaderTimeout: 30 * time.Second}}
	up := d.rolloutUpgrader()
	for {
		s.mu.Lock()
		c := s.cur
		if c == nil || c.Rollout.ID != r.ID || c.Phase != rollout.Fetching {
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
		var errs []string
		for _, addr := range d.rolloutSources(r, p.Arch, me) {
			path, err := rollout.Fetch(d.ctx, client, addr, p, d.releasesDir())
			if err != nil {
				errs = append(errs, err.Error())
				continue
			}
			pkg, err := os.ReadFile(path)
			if err != nil {
				errs = append(errs, err.Error())
				continue
			}
			to, cerr := up.Check(d.ctx, pkg)
			s.mu.Lock()
			if c := s.cur; c != nil && c.Rollout.ID == r.ID && c.Phase == rollout.Fetching {
				s.fetchErr = ""
				if cerr != nil {
					d.setPhase(c, rollout.Skipped, cerr.Error())
				} else {
					c.To = to
					d.setPhase(c, rollout.Ready, "")
				}
			}
			s.mu.Unlock()
			return
		}
		s.mu.Lock()
		was := s.fetchErr
		s.fetchErr = "no node has the package yet"
		if len(errs) > 0 {
			s.fetchErr = errs[len(errs)-1]
		}
		if s.fetchErr != was {
			d.log.Warn("mesh-wide upgrade: cannot get this node's package yet; trying again", "err", s.fetchErr)
		}
		s.mu.Unlock()
		select {
		case <-d.ctx.Done():
			return
		case <-time.After(rolloutFetchRetry):
		}
	}
}

// rolloutSources are the addresses a package may come from: this node
// (the origin keeps them all), the origin, then nodes of the architecture
// that have it.
func (d *Daemon) rolloutSources(r rollout.Rollout, arch, me string) []string {
	var out []string
	if strings.EqualFold(r.Origin, me) {
		return []string{"127.0.0.1"} // Fetch finds it kept and goes no further
	}
	if r.OriginAddr != "" {
		out = append(out, r.OriginAddr)
	}
	for _, p := range d.peers.Peers() {
		ro := p.Record.Rollout
		if ro == nil || ro.ID != r.ID || p.Record.Arch != arch || p.Record.Addr == r.OriginAddr || net.ParseIP(p.Record.Addr) == nil {
			continue
		}
		switch rollout.Phase(ro.Phase) {
		case rollout.Ready, rollout.Upgrading, rollout.Done:
			out = append(out, p.Record.Addr)
		}
	}
	return out
}

// sendRollout, on the origin, sends the rollout to every node in the order
// whose gossip does not show it yet (or, cancelled, that has not stopped).
func (d *Daemon) sendRollout(key []byte, c *rolloutRecord, me string) {
	s := &d.rollouts
	raw, err := json.Marshal(c.Rollout)
	if err != nil {
		return
	}
	cur, _ := d.conf()
	pkt, err := gossip.SealRollout(key, &gossip.RolloutMsg{V: 1, MAC: me, Callsign: cur.Callsign, Rollout: raw})
	if err != nil {
		d.log.Error("cannot send the mesh-wide upgrade", "err", err)
		return
	}
	now := time.Now()
	s.mu.Lock()
	every := rolloutResendSlow
	if now.Before(s.fastTill) {
		every = rolloutResend
	}
	if s.sent == nil {
		s.sent = map[string]time.Time{}
	}
	var to []net.IP
	for _, m := range c.Rollout.Order {
		if m == me {
			continue
		}
		p, ok := d.peers.ByMAC(m)
		if !ok {
			continue
		}
		ip := net.ParseIP(p.Record.Addr)
		ro := p.Record.Rollout
		has := ro != nil && ro.ID == c.Rollout.ID && (!c.Rollout.Cancelled || rollout.Phase(ro.Phase).Final())
		if ip == nil || has || now.Sub(s.sent[m]) < every {
			continue
		}
		s.sent[m] = now
		to = append(to, ip)
	}
	s.mu.Unlock()
	for _, ip := range to {
		_ = d.gossip.sendTo(pkt, ip)
	}
}

// askForRollout asks another node for a rollout it names in gossip that
// this node does not know: it was away when the origin sent it.
func (d *Daemon) askForRollout(key []byte, me string) {
	s := &d.rollouts
	s.mu.Lock()
	if s.cur != nil && !s.cur.Dropped && s.cur.Phase == rollout.Upgrading || time.Since(s.asked) < rolloutAskEvery {
		s.mu.Unlock()
		return
	}
	// Any node that has it, at random: one may not get its answer through.
	var can []gossip.Peer
	for _, p := range d.peers.Peers() {
		ro := p.Record.Rollout
		if ro == nil || s.notMine[ro.ID] || (s.cur != nil && s.cur.Rollout.ID == ro.ID) || net.ParseIP(p.Record.Addr) == nil {
			continue
		}
		can = append(can, p)
	}
	if len(can) == 0 {
		s.mu.Unlock()
		return
	}
	ask := can[rand.IntN(len(can))]
	s.asked = time.Now()
	id := ask.Record.Rollout.ID
	s.mu.Unlock()
	pkt, err := gossip.SealRolloutAsk(key, &gossip.RolloutAsk{V: 1, MAC: me, ID: id})
	if err != nil {
		return
	}
	d.log.Info("another node takes part in a mesh-wide upgrade this node does not know; asking it", "node", ask.Record.Callsign, "id", id)
	_ = d.gossip.sendTo(pkt, net.ParseIP(ask.Record.Addr))
}

// handleRollout takes a rollout from another node.
func (d *Daemon) handleRollout(key, pkt []byte) error {
	m, err := gossip.OpenRollout(key, pkt)
	if err != nil {
		return err
	}
	me := d.identity()
	if me == nil || strings.EqualFold(m.MAC, me.MAC) {
		return nil
	}
	var r rollout.Rollout
	if err := json.Unmarshal(m.Rollout, &r); err != nil {
		return fmt.Errorf("%w: %v", gossip.ErrInvalid, err)
	}
	if err := r.Validate(); err != nil {
		return fmt.Errorf("%w: %v", gossip.ErrInvalid, err)
	}
	mac := strings.ToLower(me.MAC)
	s := &d.rollouts
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.notMine == nil {
		s.notMine = map[string]bool{}
	}
	c := s.cur
	if c != nil && c.Rollout.ID == r.ID {
		if r.Cancelled && !c.Rollout.Cancelled && !c.Dropped {
			c.Rollout.Cancelled = true
			switch c.Phase {
			case "", rollout.Fetching, rollout.Ready:
				d.setPhase(c, rollout.Halted, "the rollout was cancelled")
			default:
				_ = d.saveRollout(c)
			}
		}
		return nil
	}
	switch {
	case s.notMine[r.ID]:
		return nil
	case time.Since(r.Started) > rolloutKeep:
		s.notMine[r.ID] = true
		return nil
	case !slices.Contains(r.Order, mac):
		s.notMine[r.ID] = true
		d.log.Info("a mesh-wide upgrade without this node in its order", "id", r.ID, "from", m.Callsign)
		return nil
	case c != nil && !c.Dropped && c.Phase == rollout.Upgrading:
		return nil // its own upgrade first; it asks again once done
	case c != nil && !c.Dropped && c.Rollout.Started.After(r.Started):
		return nil // a later one is under way
	case r.Cancelled:
		s.notMine[r.ID] = true
		return nil
	}
	n := &rolloutRecord{Rollout: r, Since: time.Now(), Received: time.Now()}
	if err := d.saveRollout(n); err != nil {
		return err
	}
	s.cur, s.others, s.wait, s.fetchErr = n, map[string]rollout.State{}, "", ""
	d.log.Info("mesh-wide upgrade received", "id", r.ID, "version", r.Version, "from", m.Callsign, "nodes", len(r.Order))
	d.Kick("mesh-wide upgrade received")
	return nil
}

// handleRolloutAsk answers a node that asks for the rollout this node
// takes part in.
func (d *Daemon) handleRolloutAsk(key, pkt []byte, from *net.UDPAddr) error {
	a, err := gossip.OpenRolloutAsk(key, pkt)
	if err != nil {
		return err
	}
	me := d.identity()
	if me == nil {
		return nil
	}
	s := &d.rollouts
	s.mu.Lock()
	c := s.cur
	if c == nil || c.Dropped || c.Rollout.ID != a.ID {
		s.mu.Unlock()
		return nil
	}
	if s.answered == nil {
		s.answered = map[string]time.Time{}
	}
	who := from.IP.String()
	if time.Since(s.answered[who]) < 5*time.Second {
		s.mu.Unlock()
		return nil
	}
	s.answered[who] = time.Now()
	raw, err := json.Marshal(c.Rollout)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	cur, _ := d.conf()
	out, err := gossip.SealRollout(key, &gossip.RolloutMsg{V: 1, MAC: strings.ToLower(me.MAC), Callsign: cur.Callsign, Rollout: raw})
	if err != nil {
		return err
	}
	return d.gossip.sendTo(out, from.IP)
}

// ensureRolloutServer serves the rollout's packages on this node's mesh
// address while a rollout is on.
func (d *Daemon) ensureRolloutServer() error {
	addr, _, _ := strings.Cut(d.facts.bridgeAddress(), "/")
	if net.ParseIP(addr) == nil {
		d.stopRolloutServer()
		return nil
	}
	s := &d.rollouts
	s.mu.Lock()
	if s.srv != nil && s.srvAddr == addr {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	d.stopRolloutServer()
	ln, err := net.Listen("tcp", net.JoinHostPort(addr, fmt.Sprint(rollout.Port)))
	if err != nil {
		return err
	}
	wanted := func(sha string) bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.cur == nil || s.cur.Dropped {
			return false
		}
		for _, p := range s.cur.Rollout.Packages {
			if p.SHA256 == sha {
				return true
			}
		}
		return false
	}
	allowed := func(ip net.IP) bool {
		for _, p := range d.peers.Peers() {
			if a := net.ParseIP(p.Record.Addr); a != nil && a.Equal(ip) {
				return true
			}
		}
		return false
	}
	srv := rollout.Server(rollout.Handler(d.releasesDir(), wanted, allowed))
	s.mu.Lock()
	s.srv, s.srvAddr = srv, addr
	s.mu.Unlock()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			d.log.Warn("mesh-wide upgrade: package server stopped", "err", err)
		}
	}()
	d.log.Info("serving the mesh-wide upgrade's packages", "address", addr, "port", rollout.Port)
	return nil
}

func (d *Daemon) stopRolloutServer() {
	s := &d.rollouts
	s.mu.Lock()
	srv := s.srv
	s.srv, s.srvAddr = nil, ""
	s.mu.Unlock()
	if srv != nil {
		_ = srv.Close()
	}
}

// pruneReleases removes the packages no rollout needs: those of a rollout
// under way stay, and those uploaded for one in the last rolloutStaged.
func (d *Daemon) pruneReleases() {
	s := &d.rollouts
	s.mu.Lock()
	keep := map[string]bool{}
	if c := s.cur; c != nil && !c.Dropped {
		for _, p := range c.Rollout.Packages {
			keep[p.SHA256] = true
		}
	}
	s.mu.Unlock()
	dir := d.releasesDir()
	rollout.Prune(dir, func(sha string) bool {
		if keep[sha] {
			return true
		}
		fi, err := os.Stat(rollout.File(dir, sha))
		return err == nil && time.Since(fi.ModTime()) < rolloutStaged
	})
}

// rolloutStatus is the rollout for status: under way, or ended in the last
// rolloutShowFor.
func (d *Daemon) rolloutStatus() *status.Rollout {
	s := &d.rollouts
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.cur
	if c == nil || c.Dropped {
		return nil
	}
	me := d.identity()
	st := &status.Rollout{
		ID: c.Rollout.ID, Version: c.Rollout.Version, Origin: d.rolloutName(c.Rollout.Origin), Sender: c.Sender,
		Started: c.Rollout.Started, Cancelled: c.Rollout.Cancelled, Phase: string(c.Phase), Reason: c.Reason, To: c.To,
	}
	if !c.Ended.IsZero() {
		e := c.Ended
		st.Ended = &e
	}
	switch c.Phase {
	case rollout.Ready:
		st.Waiting = s.wait
	case rollout.Fetching:
		st.Waiting = s.fetchErr
	}
	for _, p := range c.Rollout.Packages {
		st.Packages = append(st.Packages, status.RolloutPackage{Arch: p.Arch, SHA256: p.SHA256, Size: p.Size})
	}
	for i, m := range c.Rollout.Order {
		n := status.RolloutNode{Position: i + 1, MAC: m, Callsign: d.rolloutName(m)}
		if p, ok := d.peers.ByMAC(m); ok {
			n.NodeID, n.Arch, n.Version = p.Record.ID, p.Record.Arch, p.Record.Version
		}
		if me != nil && strings.EqualFold(m, me.MAC) {
			n.Self, n.NodeID, n.Version = true, me.NodeID, d.opts.Version
			n.Phase, n.Reason = string(c.Phase), c.Reason
			n.Since = c.Since
		} else if o, ok := s.others[m]; ok {
			n.Phase, n.Reason, n.Since = string(o.Phase), o.Reason, o.Since
		}
		st.Nodes = append(st.Nodes, n)
	}
	return st
}
