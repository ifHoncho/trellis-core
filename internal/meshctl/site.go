package meshctl

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"meridian/trellis/internal/api"
	"meridian/trellis/internal/status"
)

func cmdSite(ctx context.Context, c *api.Client, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("site", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	newKey := fs.Bool("new-key", false, "give this node a new site link key")
	force := fs.Bool("force", false, "with --new-key: even when the node holds the site link under its current key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *newKey {
		k, err := c.SiteLinkNewKey(ctx, *force)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "new site link key: %s\n(was %s)\n", k.PublicKey, k.Was)
		return nil
	}
	st, err := c.Status(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(st.SiteLink)
	}
	printSiteLink(stdout, st.SiteLink)
	if !st.SiteLink.Member {
		fmt.Fprintf(stdout, "\nTo make this node a member, add it to the profile's site_link.members and push the profile:\n")
		fmt.Fprintf(stdout, "  - {name: %s, site: <its site>, public_key: %q, endpoint: \"<host:port the other sites reach it at; leave out behind NAT>\"}\n", st.Node.Callsign, st.SiteLink.PublicKey)
	}
	return nil
}

// printSiteLink writes the site link section.
func printSiteLink(w io.Writer, sl status.SiteLink) {
	switch {
	case sl.Member:
		fmt.Fprintf(w, "site link: member for site %q, tunnel address %s, UDP port %d\n", sl.Site, sl.Address, sl.Port)
	case sl.Members > 0:
		fmt.Fprintf(w, "site link: this node is not a member (the profile lists %d)\n", sl.Members)
	default:
		fmt.Fprintf(w, "site link: none in the profile\n")
	}
	fmt.Fprintf(w, "  this node's key: %s\n", sl.PublicKey)
	if sl.Error != "" {
		fmt.Fprintf(w, "  ERROR: %s\n", sl.Error)
	}
	if sl.GuardDropped > 0 {
		fmt.Fprintf(w, "  dropped outside the tunnels: %d packets addressed as a tunnel's that did not come through WireGuard\n", sl.GuardDropped)
	}
	defer printSiteRoster(w, sl)
	if !sl.Member {
		return
	}
	if len(sl.Peers) == 0 {
		fmt.Fprintf(w, "  no member of another site in the profile\n")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  PEER\tSITE\tENDPOINT\tHANDSHAKE\tRX\tTX\tTUNNEL\tBATMAN\n")
	for _, p := range sl.Peers {
		ep := p.Current
		switch {
		case ep == "" && p.Endpoint == "":
			ep = "(waits for it to dial in)"
		case ep == "":
			ep = p.Endpoint + " (not reached)"
		case p.Endpoint != "" && p.Resolved == "" && p.Endpoint != ep:
			ep += " (profile: " + p.Endpoint + ")"
		}
		hs := "never"
		if !p.LastHandshake.IsZero() {
			hs = time.Since(p.LastHandshake).Round(time.Second).String() + " ago"
			if !p.Up {
				hs += " (DOWN)"
			}
		}
		bat := "inactive"
		if p.Batman {
			bat = fmt.Sprintf("active, %d neighbour", p.Neighbors)
			if p.Neighbors != 1 {
				bat += "s"
			}
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", sitePeerName(p), p.Site, ep, hs, humanBytes(p.RxBytes), humanBytes(p.TxBytes), p.Interface, bat)
	}
	tw.Flush()
	for _, p := range sl.Peers {
		if p.Note != "" {
			fmt.Fprintf(w, "  %s: %s\n", sitePeerName(p), p.Note)
		}
	}
}

// sitePeerName is what a peer goes by: the profile's label, the node's
// callsign, or the start of its key.
func sitePeerName(p status.SitePeer) string {
	switch {
	case p.Name != "":
		return p.Name
	case p.Callsign != "":
		return p.Callsign
	case len(p.PublicKey) >= 8:
		return p.PublicKey[:8] + "…"
	}
	return "-"
}

// printSiteRoster lists every member the profile names, with what this
// node knows of it: the link's state from any node.
func printSiteRoster(w io.Writer, sl status.SiteLink) {
	if len(sl.Roster) == 0 {
		return
	}
	fmt.Fprintf(w, "  members across the mesh:\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "    MEMBER\tSITE\tNODE\tTUNNELS\n")
	for _, m := range sl.Roster {
		name := m.Name
		if name == "" && len(m.PublicKey) >= 8 {
			name = m.PublicKey[:8] + "…"
		}
		node, tunnels := "not heard", "-"
		switch {
		case m.Self:
			node = m.Callsign + " (this node)"
		case m.Heard:
			node = m.Callsign
		}
		switch {
		case m.Reported && m.Peers == 0:
			tunnels = "none to hold"
		case m.Reported:
			tunnels = fmt.Sprintf("%d of %d up", m.Up, m.Peers)
		case m.Heard:
			tunnels = "not reported"
		}
		fmt.Fprintf(tw, "    %s\t%s\t%s\t%s\n", orDash(name), m.Site, node, tunnels)
	}
	tw.Flush()
}
