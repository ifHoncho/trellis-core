package meshd

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"time"

	"meridian/trellis/internal/release"
	"meridian/trellis/internal/upgrade"
)

// Rolling mesh-wide upgrades (internal/rollout): what this node says about
// itself in gossip.

// rolloutCheckEvery is how often the node re-reads whether it takes
// rollouts: a release key may be added, the package installed.
const rolloutCheckEvery = 5 * time.Minute

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
	s := &d.rolloutSelf
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.checkedAt.IsZero() && time.Since(s.checkedAt) < rolloutCheckEvery {
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
