package meshctl

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"meridian/trellis/internal/config"
	"meridian/trellis/internal/doctor"
	"meridian/trellis/internal/hwdb"
	"meridian/trellis/internal/prepare"
)

// cmdPrepare readies the node's system once the packages are installed
// (internal/prepare), then shows what doctor makes of it.
func cmdPrepare(version, socket string, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("prepare", flag.ContinueOnError)
	dir := fs.String("dir", prepare.DefaultDir, "where the package put its scripts")
	noDoctor := fs.Bool("no-doctor", false, "skip the checks at the end")
	stateDir := fs.String("state-dir", config.DefaultStateDir, "the node's state directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return errors.New("run as root: sudo meshctl prepare")
	}
	var opts []string
	if db, err := hwdb.Load(); err == nil {
		opts = db.ModprobeOptions()
	}
	// The builds take minutes on a small board; the scripts end on their own.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	// Ctrl-C or a stop ends the step under way, and prepare starts meshd
	// again before it exits. A second one kills it at once.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	context.AfterFunc(ctx, stop)
	res, err := prepare.Run(ctx, prepare.Options{Dir: *dir, Host: prepare.System{}, Out: stdout, DriverOptions: opts, StateDir: *stateDir})
	if err != nil {
		return err
	}

	fmt.Fprintln(stdout, "\n== Summary")
	if !*noDoctor {
		rep := doctor.Run(doctor.Options{Version: version, Socket: socket})
		fmt.Fprintf(stdout, "doctor: %d passed, %d warnings, %d failed\n", rep.Pass, rep.Warn, rep.Fail)
		for _, c := range rep.Checks {
			if c.Status == doctor.Pass {
				continue
			}
			fmt.Fprintf(stdout, "  [%s] %s: %s\n", c.Status, c.Name, c.Detail)
			if c.Fix != "" {
				fmt.Fprintf(stdout, "         fix: %s\n", c.Fix)
			}
		}
	}
	for _, a := range res.Advice {
		fmt.Fprintf(stdout, "to do: %s\n", a)
	}
	if len(res.Reboot) > 0 {
		fmt.Fprintf(stdout, "REBOOT to finish: %s\n", strings.Join(res.Reboot, "; "))
	}
	if len(res.Problems) > 0 {
		return fmt.Errorf("%d step(s) failed: %s", len(res.Problems), strings.Join(res.Problems, "; "))
	}
	fmt.Fprintln(stdout, "prepared")
	return nil
}
