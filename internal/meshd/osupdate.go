package meshd

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"meridian/trellis/internal/api"
	"meridian/trellis/internal/osupdate"
)

// OS package updates (internal/osupdate): the helper runs apt outside
// meshd's sandbox; meshd starts it and reads its record.

func (d *Daemon) osUpdatePath() string { return filepath.Join(d.opts.StateDir, "os-updates.json") }

// OSUpdates is the record of the last check and update; a run cut short
// (its unit gone) reads as failed.
func (d *Daemon) OSUpdates(ctx context.Context) (*osupdate.State, error) {
	if d.opts.NoHostConfig {
		return nil, api.Errorf(http.StatusConflict, "OS updates: not managed on this node (lab)")
	}
	return osupdate.Current(ctx, d.osUpdatePath())
}

// StartOSUpdate starts a check or an upgrade ("check", "upgrade").
func (d *Daemon) StartOSUpdate(ctx context.Context, op string) (*osupdate.State, error) {
	if d.opts.NoHostConfig {
		return nil, api.Errorf(http.StatusConflict, "OS updates: not managed on this node (lab)")
	}
	if op != "check" && op != "upgrade" {
		return nil, api.Invalid("OS updates: check or upgrade")
	}
	bin, err := os.Executable()
	if err != nil {
		bin = ""
	}
	st, err := osupdate.Start(ctx, d.osUpdatePath(), bin, op)
	if errors.Is(err, osupdate.ErrRunning) {
		return st, api.Errorf(http.StatusConflict, "%v", err)
	}
	if err != nil {
		return nil, err
	}
	d.log.Info("OS package "+op+" started", "unit", st.Unit)
	return st, nil
}

// osUpdateAfterSetup starts the upgrade a setup asked for, once the setup
// is in force: a moment later, so the network the node comes up on is the
// one apt uses.
func (d *Daemon) osUpdateAfterSetup() {
	go func() {
		time.Sleep(30 * time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := d.StartOSUpdate(ctx, "upgrade"); err != nil {
			d.log.Warn("OS update after setup not started", "err", err)
		}
	}()
}
