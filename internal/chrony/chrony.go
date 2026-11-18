// Package chrony reads chronyd's state through chronyc and keeps the list
// of the mesh's time servers it follows. Trellis installs
// chrony on every node: a node with GNSS takes its time from gpsd and serves
// it, the others follow the nodes that serve it, and with no GNSS and no
// internet the nodes still agree among themselves (orphan mode). The static
// part is packaging/chrony/trellis.conf.
package chrony

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"meridian/trellis/internal/fsutil"
)

// SourceDir is where meshd keeps the mesh's servers for chronyd (a
// "sourcedir" in trellis.conf): under /run, outside meshd's own runtime
// directory, which systemd empties whenever meshd stops.
const SourceDir = "/run/trellis-chrony"

// SourceFile is the file meshd writes in SourceDir.
const SourceFile = "mesh.sources"

// Refclock IDs set in trellis.conf for the GNSS receiver.
const (
	RefGNSS = "GNSS"
	RefPPS  = "PPS"
)

// Tracking is chronyd's view of the system clock (chronyc tracking).
type Tracking struct {
	RefID   string // hex
	RefName string // a refclock's ID ("GNSS"), or a server's address
	Stratum int
	// Offset is how far the system clock is from true time by chronyd's
	// estimate (positive: fast).
	Offset time.Duration
	Leap   string // "Normal", "Not synchronised", …
}

// Synced reports whether chronyd follows a source.
func (t Tracking) Synced() bool {
	return t.Leap != "" && !strings.HasPrefix(t.Leap, "Not sync") && t.Stratum > 0
}

// FromGNSS reports whether the clock follows this node's GNSS receiver.
func (t Tracking) FromGNSS() bool {
	return t.Synced() && (t.RefName == RefGNSS || t.RefName == RefPPS)
}

// Source is one of chronyd's sources (chronyc sources).
type Source struct {
	Mode    byte // '^' server, '=' peer, '#' refclock
	State   byte // '*' selected, '+' combined, '-', '?' unreachable, 'x', '~'
	Name    string
	Stratum int
	Reach   int
}

// ParseTracking reads chronyc -c tracking.
func ParseTracking(out []byte) (Tracking, error) {
	recs, err := csv.NewReader(bytes.NewReader(out)).ReadAll()
	if err != nil || len(recs) == 0 || len(recs[0]) < 14 {
		return Tracking{}, fmt.Errorf("unexpected chronyc tracking output %q", strings.TrimSpace(string(out)))
	}
	f := recs[0]
	t := Tracking{RefID: f[0], RefName: f[1], Leap: f[13]}
	t.Stratum, _ = strconv.Atoi(f[2])
	if s, err := strconv.ParseFloat(f[4], 64); err == nil {
		t.Offset = time.Duration(s * float64(time.Second))
	}
	return t, nil
}

// ParseSources reads chronyc -c sources.
func ParseSources(out []byte) ([]Source, error) {
	recs, err := csv.NewReader(bytes.NewReader(out)).ReadAll()
	if err != nil {
		return nil, err
	}
	var srcs []Source
	for _, f := range recs {
		if len(f) < 6 || len(f[0]) != 1 || len(f[1]) != 1 {
			continue
		}
		s := Source{Mode: f[0][0], State: f[1][0], Name: f[2]}
		s.Stratum, _ = strconv.Atoi(f[3])
		r, _ := strconv.ParseInt(f[5], 8, 32) // reach is an octal register
		s.Reach = int(r)
		srcs = append(srcs, s)
	}
	return srcs, nil
}

// Client runs chronyc.
type Client struct {
	Path string // chronyc; empty: found on PATH
}

// ErrNotInstalled is returned when chronyc is not on the node.
var ErrNotInstalled = errors.New("chrony is not installed")

func (c *Client) run(ctx context.Context, args ...string) ([]byte, error) {
	path := c.Path
	if path == "" {
		p, err := exec.LookPath("chronyc")
		if err != nil {
			return nil, ErrNotInstalled
		}
		path = p
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, args...).Output()
	if err != nil {
		return nil, fmt.Errorf("chronyc %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// Tracking asks chronyd how the clock is kept.
func (c *Client) Tracking(ctx context.Context) (Tracking, error) {
	out, err := c.run(ctx, "-n", "-c", "tracking")
	if err != nil {
		return Tracking{}, err
	}
	return ParseTracking(out)
}

// Sources lists chronyd's sources.
func (c *Client) Sources(ctx context.Context) ([]Source, error) {
	out, err := c.run(ctx, "-n", "-c", "sources")
	if err != nil {
		return nil, err
	}
	return ParseSources(out)
}

// ReloadSources makes chronyd read SourceDir again.
func (c *Client) ReloadSources(ctx context.Context) error {
	_, err := c.run(ctx, "reload", "sources")
	return err
}

// SourcesFile renders the servers file for the given addresses.
func SourcesFile(addrs []string) []byte {
	var b strings.Builder
	b.WriteString("# The mesh's time servers, kept by meshd from gossip. Do not edit.\n")
	for _, a := range addrs {
		fmt.Fprintf(&b, "server %s iburst minpoll 4 maxpoll 6\n", a)
	}
	return []byte(b.String())
}

// WriteSources writes the servers file in dir and reports whether it
// changed (then chronyd needs ReloadSources).
func WriteSources(dir string, addrs []string) (bool, error) {
	want := SourcesFile(addrs)
	path := filepath.Join(dir, SourceFile)
	if have, err := os.ReadFile(path); err == nil && bytes.Equal(have, want) {
		return false, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	return true, fsutil.WriteFile(path, want, 0o644)
}
