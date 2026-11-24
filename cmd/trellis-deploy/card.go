package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"meridian/trellis/internal/account"
	"meridian/trellis/internal/card"
	"meridian/trellis/internal/config"
)

// cardOptions are --card's: what goes on one card (internal/card).
type cardOptions struct {
	dir, profile, callsign, sshKey, github string
	webUplink, sshUplink, halow, noHalow   bool
}

// runCard writes the trellis/ folder onto a card flashed with a Trellis
// image: the node comes up on the mesh, named, with its admin password
// and operator account, and nothing to do on it.
func runCard(ctx context.Context, o cardOptions) int {
	fail := func(format string, args ...any) int {
		fmt.Fprintf(os.Stderr, "trellis-deploy: "+format+"\n", args...)
		return 1
	}
	if o.callsign == "" {
		return fail("--card needs --callsign: each node's own name")
	}
	c := card.Card{Callsign: o.callsign, WebUplink: o.webUplink, SSHUplink: o.sshUplink}
	if o.profile != "" {
		data, err := os.ReadFile(o.profile)
		if err != nil {
			return fail("%v", err)
		}
		p, err := config.ParseProfile(data)
		if err != nil {
			return fail("%s is not a mesh profile: %v", o.profile, err)
		}
		c.Profile = data
		fmt.Printf("Mesh profile: %s (%s)\n", p.Name, p.MeshID)
	} else {
		fmt.Println("No --profile: the node starts in setup mode (its setup Wi-Fi and web UI).")
	}
	if o.sshKey != "" {
		data, err := os.ReadFile(o.sshKey)
		if err != nil {
			return fail("%v", err)
		}
		keys, err := account.ParseKeys(string(data))
		if err != nil {
			return fail("%s: %v", o.sshKey, err)
		}
		c.Keys = account.MergeKeys(c.Keys, keys...)
	}
	if o.github != "" {
		keys, err := account.GitHubKeys(ctx, nil, o.github)
		if err != nil {
			return fail("%v", err)
		}
		c.Keys = account.MergeKeys(c.Keys, keys...)
	}
	for _, k := range c.Keys {
		fmt.Printf("SSH key: %s %s %s\n", k.Fingerprint, k.Type, k.Comment)
	}
	// A HaLow board's card gets Morse's files unless told not to:
	// the BCF cannot ship in the image.
	if !o.halow && !o.noHalow && card.HaLowBoard(o.dir) {
		fmt.Println("HaLow board (config.txt enables the WM6108): Morse's files go on the card (--no-halow to skip).")
		o.halow = true
	}
	if o.halow {
		fmt.Println("Fetching Morse Micro's firmware and BCF (pinned, checked)…")
		m, err := card.FetchMorse(ctx, nil)
		if err != nil {
			return fail("%v", err)
		}
		c.Morse = m
	}

	fmt.Printf("\nPassword for the web UI admin and the operator account %s (8 to 128 characters)\n", account.Name)
	for {
		a, err := secret(ctx, "  password: ")
		if err != nil {
			return fail("%v", err)
		}
		b, _ := secret(ctx, "  again: ")
		if a == b && account.CheckPassword(a) == nil {
			c.Password = a
			break
		}
		fmt.Printf("  They differ, or not %d to %d characters; again.\n", account.MinPassword, account.MaxPassword)
	}

	written, err := c.Write(o.dir)
	if err != nil {
		return fail("%v", err)
	}
	sort.Strings(written)
	fmt.Printf("\nWritten to %s:\n  %s\n", o.dir, strings.Join(written, "\n  "))
	fmt.Printf("Web UI from uplink: %s. SSH from uplink: %s.\n", onOff(o.webUplink), onOff(o.sshUplink))
	fmt.Println("Eject the card and boot the node: it installs these files and removes them from the card.")
	fmt.Println("The files hold the mesh secret until then: keep the card safe.")
	return 0
}

func onOff(v bool) string {
	if v {
		return "on"
	}
	return "off"
}
