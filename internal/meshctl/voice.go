package meshctl

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"meridian/trellis/internal/api"
	"meridian/trellis/internal/voice"
)

// cmdVoice prints the push-to-talk state: talk groups, who is talking,
// this node's channels.
func cmdVoice(ctx context.Context, c *api.Client, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("voice", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := c.Voice(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(st)
	}
	switch {
	case !st.Enabled:
		fmt.Fprintln(stdout, "voice: off (voice.enable in node.yaml)")
		return nil
	case !st.Running:
		fmt.Fprintf(stdout, "voice: on, not running: %s\n", st.Error)
		return nil
	}
	var vs voice.Status
	if err := json.Unmarshal(st.Status, &vs); err != nil {
		return fmt.Errorf("trellis-voiced's status: %w", err)
	}
	fmt.Fprintln(stdout, "voice: running")
	fmt.Fprintf(stdout, "\n%-3s %-16s %-22s %-6s %-18s %8s %6s\n", "#", "GROUP", "ADDRESS", "HEARD", "TALKING", "RECEIVED", "LOST")
	for _, g := range vs.Groups {
		heard, talker := "-", g.Talker
		if g.Listening {
			heard = "yes"
		}
		if g.Sending {
			talker = "(this node)"
		}
		if talker == "" {
			talker = "-"
		}
		if g.AllCall {
			talker += " (all call)"
		}
		fmt.Fprintf(stdout, "%-3d %-16s %-22s %-6s %-18s %8d %6d\n", g.Number, g.Name, g.Addr, heard, talker, g.Received, g.Lost)
	}
	fmt.Fprintln(stdout)
	for _, ch := range vs.Channels {
		state := "off"
		switch {
		case ch.Group == 0:
		case ch.Sending:
			state = "talking"
		case ch.PTT:
			state = "PTT held, refused (busy)"
		default:
			state = "listening"
		}
		fmt.Fprintf(stdout, "channel %s: group %d, %s ear, volume %d: %s\n", ch.Name, ch.Group, ch.Ear, ch.Volume, state)
	}
	return nil
}
