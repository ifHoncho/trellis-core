package meshd

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"meridian/trellis/internal/halowfiles"
	"meridian/trellis/internal/reconcile"
	"meridian/trellis/internal/rollout"
)

// HaLow files (internal/halowfiles): a node with a HaLow module and
// without Morse's firmware or BCF gets them from a node of the mesh that
// holds them, else over its uplink from Morse's repository, each checked
// against its pinned hash; a node that holds them says so in gossip and
// serves them on the package server.

type halowState struct {
	mu       sync.Mutex
	held     bool
	fetching bool          // a fetch runs, outside the reconcile pass
	done     chan struct{} // closed when the fetch under way ends
	next     time.Time
	backoff  time.Duration
	lastErr  string
}

const (
	halowRetryMin = time.Minute
	halowRetryMax = 30 * time.Minute
	halowRecheck  = 30 * time.Minute
	// halowFetchLimit bounds one fetch and install, every source tried.
	halowFetchLimit = 10 * time.Minute
)

func (d *Daemon) halowPins() *halowfiles.Pins {
	path := d.opts.HaLowPins
	if path == "" {
		path = halowfiles.DefaultPinsFile
	}
	p, err := halowfiles.LoadPins(path)
	if err != nil {
		return nil
	}
	return p
}

func (d *Daemon) halowFirmwareDir() string {
	if d.opts.HaLowFirmwareDir != "" {
		return d.opts.HaLowFirmwareDir
	}
	return halowfiles.FirmwareDir
}

// halowHeld says whether this node holds every pinned file (gossip "hf").
func (d *Daemon) halowHeld() bool {
	d.halow.mu.Lock()
	defer d.halow.mu.Unlock()
	return d.halow.held
}

type halowReconciler struct{ d *Daemon }

func (r *halowReconciler) Name() string                       { return "halow-files" }
func (r *halowReconciler) Teardown(ctx context.Context) error { return nil }

// Reconcile reports what the node holds and starts a fetch of what it
// lacks. The fetch runs outside the pass (a source that does not answer
// takes its whole timeout); the pass that follows it sees the result.
func (r *halowReconciler) Reconcile(ctx context.Context) (reconcile.Result, error) {
	d := r.d
	res := reconcile.Result{RequeueAfter: halowRecheck}
	pins := d.halowPins()
	if pins == nil || d.opts.NoHostConfig {
		return res, nil
	}
	dir := d.halowFirmwareDir()
	held := len(pins.Missing(dir)) == 0
	s := &d.halow
	s.mu.Lock()
	wasHeld := s.held
	s.held = held
	s.mu.Unlock()
	if held {
		if !wasHeld {
			d.log.Info("HaLow firmware and BCF present: served to the mesh")
			res.Changed = true
		}
		if err := d.ensureRolloutServer(); err != nil {
			d.log.Debug("package server", "err", err)
		}
		return res, nil
	}
	// Only what the node has no file for is fetched: a file that is there
	// under a pinned name is the operator's, and stays.
	absent := pins.Absent(dir)
	if len(absent) == 0 || !halowfiles.ModulePresent(d.opts.ProcRoot) {
		return res, nil
	}
	var names []string
	for _, f := range absent {
		names = append(names, f.Name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	msg := fmt.Sprintf("HaLow module files missing: %s. Fetching from mesh nodes and uplink; or write a card: trellis-deploy --card --halow.", strings.Join(names, ", "))
	if s.lastErr != "" {
		msg += " Last try: " + s.lastErr
	}
	res.Warnings = append(res.Warnings, reconcile.Warning{Level: "warning", Code: "halow-files-missing", Message: msg})
	if s.fetching {
		return res, nil // its end asks for a pass
	}
	if wait := time.Until(s.next); wait > 0 {
		res.RequeueAfter = wait
		return res, nil
	}
	s.fetching, s.done = true, make(chan struct{})
	go d.runHaLowFetch(pins, absent, dir, names, s.done)
	return res, nil
}

// runHaLowFetch is one try at the absent files, in its own goroutine: it
// keeps the retry state and asks for a pass when it ends.
func (d *Daemon) runHaLowFetch(pins *halowfiles.Pins, absent []halowfiles.File, dir string, names []string, done chan struct{}) {
	parent := d.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, halowFetchLimit)
	err := d.fetchHaLowFiles(ctx, pins, absent, dir)
	cancel()
	s := &d.halow
	s.mu.Lock()
	if err != nil {
		if s.backoff < halowRetryMin {
			s.backoff = halowRetryMin
		} else if s.backoff *= 2; s.backoff > halowRetryMax {
			s.backoff = halowRetryMax
		}
		s.next, s.lastErr = time.Now().Add(s.backoff), err.Error()
	} else {
		s.backoff, s.lastErr, s.next = 0, "", time.Time{}
	}
	s.fetching = false
	s.mu.Unlock()
	if err != nil {
		d.log.Info("HaLow files not fetched yet", "missing", strings.Join(names, ","), "err", err)
	} else {
		d.log.Info("HaLow files installed", "files", strings.Join(names, ","))
	}
	close(done)
	d.Kick("halow files")
}

// fetchHaLowFiles gets the absent files (and the licence) into the state
// directory, from a node that holds them or from Morse's repository, then
// installs them.
func (d *Daemon) fetchHaLowFiles(ctx context.Context, pins *halowfiles.Pins, missing []halowfiles.File, fwDir string) error {
	tmp := filepath.Join(d.opts.StateDir, "halow")
	_ = os.RemoveAll(tmp)
	defer os.RemoveAll(tmp)
	client := &http.Client{Timeout: 30 * time.Second}
	port := rollout.Port
	if d.opts.HaLowMeshPort != 0 {
		port = d.opts.HaLowMeshPort
	}
	var holders []string
	if d.peers != nil {
		for _, p := range d.peers.Peers() {
			if p.Record.HaLowFiles && p.Record.Addr != "" {
				holders = append(holders, p.Record.Addr)
			}
		}
	}
	rand.Shuffle(len(holders), func(i, j int) { holders[i], holders[j] = holders[j], holders[i] })
	get := func(meshName, upstreamPath, name, sum string) error {
		var errs []string
		for _, addr := range holders {
			if _, err := halowfiles.Get(ctx, client, halowfiles.MeshURL(addr, port, meshName), tmp, name, sum); err == nil {
				return nil
			} else {
				errs = append(errs, err.Error())
			}
		}
		if _, err := halowfiles.Get(ctx, client, pins.UpstreamURL(upstreamPath), tmp, name, sum); err != nil {
			errs = append(errs, "uplink: "+err.Error())
			if len(holders) == 0 {
				return fmt.Errorf("no mesh node holds it; uplink: %v", err)
			}
			return fmt.Errorf("%s", strings.Join(errs, "; "))
		}
		return nil
	}
	for _, f := range missing {
		if err := get(f.SHA256, f.Path, f.Name, f.SHA256); err != nil {
			return fmt.Errorf("%s: %w", f.Name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(fwDir, halowfiles.LicenseName)); err != nil && pins.License != "" {
		_ = get(halowfiles.LicenseName, pins.License, halowfiles.LicenseName, "")
	}
	if d.opts.HaLowInstall != nil {
		return d.opts.HaLowInstall(ctx, tmp)
	}
	bin, err := os.Executable()
	if err != nil {
		bin = ""
	}
	return halowfiles.RunInstall(ctx, bin, tmp)
}
