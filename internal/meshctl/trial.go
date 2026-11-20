package meshctl

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"meridian/trellis/internal/api"
	"meridian/trellis/internal/config"
	"meridian/trellis/internal/crashlog"
	"meridian/trellis/internal/status"
	"meridian/trellis/internal/trial"
)

const trialUsage = `usage:
  meshctl trial                    the trials on this node
  meshctl trial begin NAME --file PLACE=NEWFILE [--file …] [--module MOD] [--radio DRIVER]
                     [--kind module|firmware|bcf] [--version V] [--reboot] [--rearm]
                                   try new files: the ones they replace are kept, MOD is reloaded
                                   (or, with --reboot, the files load at the next boot), and meshd
                                   keeps them once the radios on it have run well for a while
  meshctl trial confirm NAME       keep a pending trial now
  meshctl trial revert NAME        put the old files back (also after a trial was kept)
  meshctl trial rearm NAME         allow a version that was undone to be tried again
  meshctl trial boot               the boot hook (trellis-trial.service): undo what the last
                                   boot did not confirm, before drivers load`

// fileFlags collects --file PLACE=NEWFILE.
type fileFlags map[string]string

func (f fileFlags) String() string { return fmt.Sprint(map[string]string(f)) }

func (f fileFlags) Set(v string) error {
	place, src, ok := strings.Cut(v, "=")
	if !ok || place == "" || src == "" {
		return fmt.Errorf("want PLACE=NEWFILE, got %q", v)
	}
	f[place] = src
	return nil
}

// cmdTrial works on the node's trials directly (internal/trial): the boot
// hook runs before meshd does, and putting files under /lib is outside
// meshd's bounds.
func cmdTrial(ctx context.Context, client *api.Client, args []string, stdout io.Writer) error {
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("trial "+sub, flag.ContinueOnError)
	stateDir := fs.String("state-dir", config.DefaultStateDir, "the node's state directory")
	bootID := fs.String("boot-id", "", "lab only: the ID of this boot of the system")
	files := fileFlags{}
	var req trial.Request
	var reason string
	var latch bool
	switch sub {
	case "begin":
		fs.Var(files, "file", "PLACE=NEWFILE: put NEWFILE at PLACE (repeat for more files)")
		fs.StringVar(&req.Module, "module", "", "kernel module that loads the files: reloaded now, unless --reboot")
		fs.StringVar(&req.Radio, "radio", "", "driver name of the radios it runs, when not the module's")
		fs.StringVar(&req.Kind, "kind", "", "module, firmware or bcf")
		fs.StringVar(&req.Version, "version", "", "what the new version is called")
		fs.BoolVar(&req.Reboot, "reboot", false, "the files load at the next boot; reload nothing now")
		fs.BoolVar(&req.Rearm, "rearm", false, "try files that were reverted before")
	case "revert":
		fs.StringVar(&reason, "reason", "manual revert", "why, for the record")
		fs.BoolVar(&latch, "latch", false, "lock out the same files until re-armed")
	}
	// Flags may follow the name: meshctl trial begin NAME --file …
	var names []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			break
		}
		names = append(names, fs.Arg(0))
		args = fs.Args()[1:]
	}
	store := trial.New(*stateDir)
	if *bootID == "" {
		*bootID = crashlog.BootID()
	}
	name := ""
	if sub != "list" && sub != "boot" {
		if len(names) != 1 {
			return errors.New(trialUsage)
		}
		name = names[0]
	} else if len(names) > 0 {
		return errors.New(trialUsage)
	}
	if sub != "list" && os.Geteuid() != 0 {
		return fmt.Errorf("run as root: sudo meshctl trial %s", sub)
	}
	switch sub {
	case "list":
		ts, err := store.List()
		if err != nil {
			return err
		}
		if len(ts) == 0 {
			fmt.Fprintln(stdout, "no trials")
			return nil
		}
		tw := tabwriter.NewWriter(stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tVERSION\tSTATE\tSINCE\tNOTE")
		for _, t := range ts {
			state, since, note := t.State, t.Started, t.Reason
			if t.Latched {
				state += " (latched)"
			}
			if !t.Ended.IsZero() {
				since = t.Ended
			}
			if t.RebootNeeded {
				note = strings.TrimSpace(note + " REBOOT to load the previous version")
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", t.Name, orDash(t.Version), state, since.Local().Format("2006-01-02 15:04"), note)
		}
		return tw.Flush()
	case "begin":
		req.Name, req.Files, req.BootID = name, files, *bootID
		req.HadPeer = trialHadPeer(ctx, client, req)
		t, err := store.Begin(req)
		if err != nil {
			return err
		}
		switch t.State {
		case trial.Armed:
			fmt.Fprintf(stdout, "%s armed: loads at next boot. Reverts on reboot before confirmation.\n", t.Describe())
		default:
			fmt.Fprintf(stdout, "%s loaded, on trial: confirms after a healthy period; reverts on reboot before that. Status: meshctl status\n", t.Describe())
		}
		return nil
	case "confirm":
		t, err := store.Confirm(name)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s confirmed. Previous files kept; revert: meshctl trial revert %s\n", t.Describe(), name)
		return nil
	case "revert":
		t, err := store.Revert(name, reason, latch)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s reverted: previous files restored.\n", t.Describe())
		if t.RebootNeeded {
			fmt.Fprintln(stdout, "REBOOT REQUIRED: module reload failed; previous version loads at next boot.")
		}
		return nil
	case "rearm":
		if err := store.Rearm(name); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s re-armed.\n", name)
		return nil
	case "boot":
		if !store.Any() {
			return nil
		}
		changed, err := store.Boot(*bootID)
		for _, t := range changed {
			switch t.State {
			case trial.Reverted:
				fmt.Fprintf(stdout, "trellis: %s reverted: %s\n", t.Describe(), t.Reason)
			case trial.Pending:
				fmt.Fprintf(stdout, "trellis: %s loads in this boot, on trial\n", t.Describe())
			}
		}
		return err
	}
	return errors.New(trialUsage)
}

// trialHadPeer asks meshd whether a radio the trial concerns has a mesh
// peer now: meshd then wants one back before it keeps the trial. Without
// meshd, or without such a radio, it does not.
func trialHadPeer(ctx context.Context, client *api.Client, req trial.Request) bool {
	driver := req.Radio
	if driver == "" {
		driver = req.Module
	}
	if driver == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	st, err := client.Status(ctx)
	if err != nil {
		return false
	}
	links := append([]status.MeshLink{st.MeshLink}, st.MoreLinks...)
	for _, r := range st.Radios {
		if r.Driver != driver {
			continue
		}
		for _, l := range links {
			if role := roleOfLink(l.Index); l.Interface == "" || (r.Role != role && r.Role != role+"+ap") {
				continue
			}
			for _, p := range l.Peers {
				if p.Established {
					return true
				}
			}
		}
	}
	return false
}

// roleOfLink is the role name of the radio that carries mesh link n (from
// 1): mesh, mesh2, …
func roleOfLink(n int) string {
	if n <= 1 {
		return "mesh"
	}
	return fmt.Sprintf("mesh%d", n)
}
