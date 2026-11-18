package meshctl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"meridian/trellis/internal/api"
	"meridian/trellis/internal/status"
)

// cmdUpgradeMesh sends a release through the whole mesh from this node:
// every node that takes it upgrades in turn, each checking its own health,
// and the first that goes back halts the rest. It always shows the plan
// first, and starts only with --yes; then it follows the progress.
func cmdUpgradeMesh(client *api.Client, files []string, yes, follow, cancel bool, stdout io.Writer) error {
	ctx := context.Background()
	if cancel {
		if err := client.CancelRollout(ctx); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "cancelled: nodes that have not begun upgrading stop; one upgrading now finishes")
		return nil
	}
	if len(files) == 0 {
		st, err := client.Status(ctx)
		if err != nil {
			return err
		}
		if st.Rollout == nil {
			fmt.Fprintln(stdout, "no mesh-wide upgrade is under way")
			return nil
		}
		printRollout(stdout, st.Rollout)
		if follow {
			return followRollout(ctx, client, stdout)
		}
		return nil
	}
	var shas []string
	for _, f := range files {
		pkg, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		p, err := client.RolloutPackage(ctx, pkg)
		cancel()
		if err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		fmt.Fprintf(stdout, "%s: %s for %s, signed by %s\n", f, p.Version, p.Arch, p.SignedBy)
		shas = append(shas, p.SHA256)
	}
	plan, err := client.Rollout(ctx, shas, !yes)
	if err != nil {
		return err
	}
	printPlan(stdout, plan)
	if !yes {
		fmt.Fprintln(stdout, "\nNothing started. Add --yes to upgrade every node above, in this order.")
		return nil
	}
	fmt.Fprintf(stdout, "\nstarted (rollout %s). Follow it with: meshctl upgrade --mesh --follow\n", plan.ID)
	if follow {
		return followRollout(ctx, client, stdout)
	}
	return nil
}

func printPlan(w io.Writer, p *api.RolloutPlan) {
	fmt.Fprintf(w, "\nrelease %s\n", p.Version)
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  #\tNODE\tARCH\tRUNS\tHOPS\t")
	for _, n := range p.Nodes {
		pos, hops := "-", "?"
		if n.Position > 0 {
			pos = fmt.Sprint(n.Position)
		}
		if n.Hops >= 0 {
			hops = fmt.Sprint(n.Hops)
		}
		name := n.Callsign
		if n.Self {
			name += " (this node)"
		}
		line := fmt.Sprintf("  %s\t%s\t%s\t%s\t%s\t", pos, name, orDash(n.Arch), orDash(n.Version), hops)
		if n.Skip != "" {
			line += "skipped: " + n.Skip
		}
		fmt.Fprintln(tw, line)
	}
	tw.Flush()
	for _, warn := range p.Warnings {
		fmt.Fprintf(w, "  [WARNING] %s\n", warn)
	}
}

// rolloutLine is the rollout in one line, for status: "to 0.3.1 from
// relay-1: 2 of 5 done; ridge-2 upgrading".
func rolloutLine(r *status.Rollout) string {
	done, total, now := 0, len(r.Nodes), ""
	var stopped []string
	halted := ""
	for _, n := range r.Nodes {
		switch n.Phase {
		case "done":
			done++
		case "upgrading":
			now = n.Callsign + " upgrading"
		case "rolled-back", "failed":
			stopped = append(stopped, n.Callsign+" "+n.Phase)
		case "halted":
			if halted == "" && !r.Cancelled {
				halted = n.Reason
			}
		}
	}
	line := fmt.Sprintf("to %s from %s: %d of %d done", r.Version, r.Origin, done, total)
	switch {
	case len(stopped) > 0:
		line += "; HALTED: " + strings.Join(stopped, ", ")
	case halted != "":
		line += "; HALTED: " + halted
	case r.Cancelled:
		line += "; cancelled"
	case now != "":
		line += "; " + now
	}
	if r.Ended != nil {
		line += "; over"
	}
	return line
}

func printRollout(w io.Writer, r *status.Rollout) {
	fmt.Fprintf(w, "mesh-wide upgrade %s\n", rolloutLine(r))
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	for _, n := range r.Nodes {
		name := n.Callsign
		if n.Self {
			name += " (this node)"
		}
		ph := n.Phase
		if ph == "" {
			ph = "not heard yet"
		}
		if n.Reason != "" {
			ph += ": " + n.Reason
		}
		if n.Self && r.Waiting != "" && (n.Phase == "ready" || n.Phase == "fetching") {
			ph += " (" + r.Waiting + ")"
		}
		fmt.Fprintf(tw, "  %d\t%s\t%s\t\n", n.Position, name, ph)
	}
	tw.Flush()
}

// followRollout prints each node's phase as it changes, until the rollout
// is over. Leaving it does not stop the rollout.
func followRollout(ctx context.Context, client *api.Client, stdout io.Writer) error {
	seen := map[string]string{}
	for {
		st, err := client.Status(ctx)
		if err != nil && !errors.Is(err, api.ErrNotRunning) {
			return err
		}
		if err == nil {
			r := st.Rollout
			if r == nil {
				fmt.Fprintln(stdout, "the mesh-wide upgrade is no longer shown on this node")
				return nil
			}
			for _, n := range r.Nodes {
				ph := n.Phase
				if n.Reason != "" {
					ph += ": " + n.Reason
				}
				if ph != "" && seen[n.MAC] != ph {
					seen[n.MAC] = ph
					fmt.Fprintf(stdout, "%s  %s %s\n", time.Now().Format("15:04:05"), n.Callsign, ph)
				}
			}
			if r.Ended != nil {
				fmt.Fprintf(stdout, "over: %s\n", rolloutLine(r))
				for _, n := range r.Nodes {
					if n.Phase == "rolled-back" || n.Phase == "failed" {
						return fmt.Errorf("%s %s: %s", n.Callsign, n.Phase, n.Reason)
					}
				}
				for _, n := range r.Nodes {
					if n.Phase == "halted" && !r.Cancelled {
						return fmt.Errorf("halted: %s", n.Reason)
					}
				}
				return nil
			}
		}
		time.Sleep(2 * time.Second)
	}
}
