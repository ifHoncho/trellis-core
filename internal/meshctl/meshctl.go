// Package meshctl is the operator CLI: status, radios, doctor, config
// validation and profile generation.
package meshctl

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"meridian/trellis/internal/api"
	"meridian/trellis/internal/config"
	"meridian/trellis/internal/doctor"
	"meridian/trellis/internal/fsutil"
	"meridian/trellis/internal/hoststat"
	"meridian/trellis/internal/hwdb"
	"meridian/trellis/internal/qr"
	"meridian/trellis/internal/status"

	"gopkg.in/yaml.v3"
)

// Main runs meshctl with the given arguments (without the program name).
func Main(version string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("meshctl", flag.ContinueOnError)
	fs.SetOutput(stderr)
	socket := fs.String("socket", "", "meshd API socket (default "+config.DefaultRunDir+"/meshd.sock)")
	fs.Usage = func() {
		fmt.Fprintf(stderr, `Usage: meshctl [--socket PATH] <command> [args]

Commands:
  status [--json] [--watch]      node status from meshd
  radios [--json]                radios, capabilities and roles
  doctor [--json] [--node PATH] [--profile PATH]
                                 host preflight checks; paste the output when asking for help
  prepare [--no-doctor]          after installing the packages (root): build and load the pinned
                                 batman-adv, the MT7915/MT7916 driver where the kernel lacks it,
                                 radio driver options, regulatory rules; says if a reboot is needed
  config validate [node.yaml] [profile.yaml]
                                 validate configuration files without applying them
  profile new --name NAME [--band 5g] [--channel 149] [--country US] [--mode country|unrestricted] [-o FILE [--force]]
                                 new mesh profile with a random passphrase (stdout, or FILE with mode 0600)
  profile show [FILE]            print this node's mesh profile (to copy it to other nodes)
  profile band 2g|5g|6g [--channel N] [--width MHz] [FILE]
                                 switch a profile file to another band (default file: mesh-profile.yaml)
  profile install [FILE|-]       validate a profile, install it on this node and apply it live
  survey [--start]               the last mesh-wide channel survey; --start runs one (each radio
                                 leaves its channel briefly) and waits for the answers
  profile push [--in S] [--yes] [--replace] FILE
                                 send a profile file to every node: all switch to it in S seconds (default 60);
                                 without --yes, only show what it would do. While another change is on
                                 its way it is refused, unless --replace (that change is dropped)
  profile edit [--in S] [--yes] [--replace]
                                 change the profile the mesh runs, in $EDITOR, and send it to every node;
                                 refused if the profile changed on the node while it was being edited
  profile same FILE1 FILE2       exit 0 if the two profiles mean the same, else name the settings that differ
  node new --callsign NAME [--id N] [-o FILE [--force]]
                                 node config skeleton (stdout, or FILE)
  profile share                  this node's mesh profile as a profile code and QR code, for a new node's setup page
  ap [--json]                    client AP name and passphrase, with the QR code a phone joins by
  web                            where the web UI answers, and whether it has an admin password
  web password [--stdin|--clear] set (or remove) the web UI's admin password
  web wan-access on|off          also serve the web UI on the node's uplink network
  clients [--json]               devices joined to this node: AP clients and DHCP leases
  voice [--json]                 push-to-talk: talk groups, who is talking, this node's channels
  site [--json]                  the site link: this node's key, and its tunnels to other sites
  site --new-key [--force]       give this node a new site link key
  upgrade FILE.deb               install a release; the node goes back to the old one if it is not healthy after
  upgrade --status|--rollback|--resume
  trial                          new driver modules, firmware or board files on trial, and the last
                                 trial of each
  trial begin NAME --file PLACE=NEWFILE [--module MOD] [--reboot]
                                 try new files (root): kept once the node has run well on them,
                                 undone if it restarts first. Also: trial confirm|revert|rearm NAME
  reconcile                      ask meshd for a pass now
  reload                         ask meshd to re-read its configuration
  version

`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return 2
	}
	// No deadline for the whole command: each API request has the
	// client's own (30 s), and some commands wait on the operator (a
	// password typed twice) or run until stopped (status --watch).
	ctx := context.Background()
	client := api.NewClient(*socket)
	var err error
	switch rest[0] {
	case "status":
		err = cmdStatus(ctx, client, rest[1:], stdout, stderr)
	case "radios":
		err = cmdRadios(ctx, client, rest[1:], stdout)
	case "doctor":
		err = cmdDoctor(version, client.Socket, rest[1:], stdout)
	case "prepare":
		err = cmdPrepare(version, client.Socket, rest[1:], stdout)
	case "config":
		err = cmdConfig(rest[1:], stdout)
	case "profile":
		err = cmdProfile(ctx, client, rest[1:], stdout)
	case "survey":
		err = cmdSurvey(ctx, client, rest[1:], stdout)
	case "node":
		err = cmdNode(rest[1:], stdout)
	case "ap":
		err = cmdAP(ctx, client, rest[1:], stdout)
	case "clients":
		err = cmdClients(ctx, client, rest[1:], stdout)
	case "voice":
		err = cmdVoice(ctx, client, rest[1:], stdout)
	case "site":
		err = cmdSite(ctx, client, rest[1:], stdout)
	case "web":
		err = cmdWeb(ctx, client, rest[1:], stdout, stderr)
	case "upgrade":
		err = cmdUpgrade(client, rest[1:], stdout)
	case "trial":
		err = cmdTrial(ctx, client, rest[1:], stdout)
	case "reconcile":
		if err = client.Reconcile(ctx); err == nil {
			fmt.Fprintln(stdout, "reconcile requested")
		}
	case "reload":
		if err = client.Reload(ctx); err == nil {
			fmt.Fprintln(stdout, "configuration reloaded")
		}
	case "driver-options":
		// modprobe.d lines from the hardware database, for scripts.
		var db *hwdb.DB
		if db, err = hwdb.Load(); err == nil {
			for _, l := range db.ModprobeOptions() {
				fmt.Fprintln(stdout, l)
			}
		}
	case "version":
		fmt.Fprintln(stdout, version)
	case "batman", "trellis":
		err = cmdEgg(rest[0], stdout)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", rest[0])
		fs.Usage()
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		if errors.Is(err, api.ErrNotRunning) {
			fmt.Fprintln(stderr, "hint: systemctl start trellis-meshd, or run meshctl doctor")
		}
		return 1
	}
	return 0
}

func cmdStatus(ctx context.Context, c *api.Client, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print JSON")
	watch := fs.Bool("watch", false, "refresh every 2 seconds")
	if err := fs.Parse(args); err != nil {
		return err
	}
	for {
		st, err := c.Status(ctx)
		if err != nil {
			return err
		}
		if *asJSON {
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(st)
		}
		if *watch {
			fmt.Fprint(stdout, "\033[H\033[2J")
		}
		PrintStatus(stdout, st)
		if !*watch {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
}

// PrintStatus renders the human-readable status.
func PrintStatus(w io.Writer, st *status.Status) {
	fmt.Fprintf(w, "%s  node %d (%s)  mac %s  trellis %s  up %s\n", st.Node.Callsign, st.Node.NodeID, st.Node.NodeIDSource, st.Node.MAC, st.Version, time.Since(st.StartedAt).Round(time.Second))
	fmt.Fprintf(w, "mesh %q  %s ch %d (%g MHz, %d MHz wide)  prefix %s  address %s\n", st.Profile.MeshID, st.Profile.Band, st.Profile.Channel, st.Profile.FreqMHz, st.Profile.WidthMHz, st.Profile.Prefix, st.Node.Address)
	kdom := st.Regulatory.KernelAlpha2
	if kdom == "" {
		kdom = "unknown"
	}
	src := map[string]string{"trellis": "rules from trellis crda", "kernel": "rules from the kernel's regulatory.db"}[st.Regulatory.Source]
	switch {
	case st.Regulatory.ActiveMode == config.RegModeUnrestricted:
		fmt.Fprintf(w, "regulatory: UNRESTRICTED (no regulatory limits), country %s, kernel domain %s, %s\n", st.Regulatory.Country, kdom, src)
	case st.Regulatory.RequestedMode == config.RegModeUnrestricted:
		fmt.Fprintf(w, "regulatory: unrestricted requested, NOT ACTIVE: %s rules, kernel domain %s, %s\n", st.Regulatory.Country, kdom, src)
	default:
		fmt.Fprintf(w, "regulatory: country %s, kernel domain %s, %s\n", st.Regulatory.Country, kdom, src)
	}
	fmt.Fprintf(w, "time: %s\n", clockLine(st.Clock))
	if mc := st.MeshChange; mc != nil {
		fmt.Fprintf(w, "mesh change: %s\n", meshChangeLine(mc, time.Now()))
	}
	if r := st.Rollout; r != nil {
		fmt.Fprintf(w, "mesh upgrade: %s\n", rolloutLine(r))
	}
	if st.GNSS.Receiver {
		fmt.Fprintf(w, "gnss: %s\n", gnssLine(st.GNSS))
	}
	for _, t := range st.Trials {
		what := strings.TrimSpace(t.Name + " " + t.Version)
		switch {
		case t.State == "pending" && t.ConfirmAfterS > 0:
			fmt.Fprintf(w, "trial: %s pending, confirms in %s if healthy\n", what, (time.Duration(t.ConfirmAfterS) * time.Second).String())
		case t.State == "pending":
			fmt.Fprintf(w, "trial: %s pending, not healthy yet\n", what)
		case t.State == "armed":
			fmt.Fprintf(w, "trial: %s loads at the next boot\n", what)
		case t.State == "reverted" && t.Latched:
			fmt.Fprintf(w, "trial: %s UNDONE and off until re-armed: %s\n", what, t.Reason)
		}
	}
	if l := hostLine(st.Host); l != "" {
		fmt.Fprintf(w, "host: %s\n", l)
	}
	if len(st.Resets) > 0 {
		last := st.Resets[0]
		more := ""
		if n := len(st.Resets); n > 1 {
			more = fmt.Sprintf(" (%d on record: meshctl status --json)", n)
		}
		at := last.Time.Local().Format("2006-01-02 15:04")
		if last.TimeUnknown {
			at = "time unknown"
		}
		fmt.Fprintf(w, "last reset: %s, %s%s\n", at, last.Summary, more)
	}
	if l := positionsLine(st.Positions, st.GNSS.Receiver); l != "" {
		fmt.Fprintf(w, "positions: %s\n", l)
	}

	fmt.Fprintf(w, "\nRadios (%d):\n", len(st.Radios))
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  PHY\tROLE\tREC\tCHIPSET/DRIVER\tBUS\tPORT\tBANDS\tSTREAMS\tPOWER\tCAPS\tINTERFACES\tFINGERPRINT")
	for _, r := range st.Radios {
		name := r.Chipset
		if name == "" {
			name = r.Driver
		}
		if r.Label != "" {
			name = r.Label + " (" + name + ")"
		}
		bus := r.Bus
		if r.USBSpeedMbps > 0 {
			bus = fmt.Sprintf("%s %d", bus, r.USBSpeedMbps)
		}
		var powers []string
		bands := append([]string(nil), r.Bands...)
		sort.Strings(bands)
		for _, b := range bands {
			if p, ok := r.Power[b]; ok {
				powers = append(powers, fmt.Sprintf("%s %.0f(%s)", b, p.DBm, p.Source[:1]))
			}
		}
		var caps []string
		if r.MeshPoint {
			caps = append(caps, "mesh")
		}
		if r.AP {
			caps = append(caps, "ap")
		}
		if r.MeshAndAP {
			caps = append(caps, "both")
		}
		if r.Ignored {
			caps = append(caps, "IGNORED")
		}
		if r.Broken != "" {
			caps = append(caps, "no-mesh")
		}
		if r.InUse != "" {
			caps = append(caps, "SYSTEM-IN-USE")
		}
		rec := r.Recommended
		if rec == r.Role {
			rec = "="
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n", r.Phy, r.Role, rec, name, bus, r.Port, strings.Join(r.Bands, "/"), r.Streams,
			strings.Join(powers, " "), strings.Join(caps, ","), strings.Join(r.Interfaces, ","), r.Fingerprint)
	}
	tw.Flush()
	if st.Plan.GraceActive {
		fmt.Fprintln(w, "  (a radio disappeared; roles are held for a few seconds in case it returns)")
	}

	printMeshLink(w, linkTitle(st.MeshLink), st.MeshLink, st, true)
	for _, ml := range st.MoreLinks {
		printMeshLink(w, linkTitle(ml), ml, st, false)
	}

	b := st.Batman
	fmt.Fprintf(w, "\nbatman-adv: ")
	if !b.Present {
		fmt.Fprintf(w, "%s not present; module %q\n", b.Interface, b.ModuleVersion)
	} else {
		mismatch := ""
		if b.PinnedMismatch {
			mismatch = " (differs from pinned " + st.Profile.PinnedBatman + ")"
		}
		fmt.Fprintf(w, "%s %s mac %s mtu %d, module %s%s\n", b.Interface, b.Algo, b.MAC, b.MTU, b.ModuleVersion, mismatch)
		var hifs []string
		for _, h := range b.HardIfs {
			state := "inactive"
			if h.Active {
				state = "active"
			}
			hifs = append(hifs, fmt.Sprintf("%s(%s, mtu %d)", h.Name, state, h.MTU))
		}
		if len(hifs) == 0 {
			hifs = []string{"none"}
		}
		fmt.Fprintf(w, "  hard interfaces: %s\n", strings.Join(hifs, " "))
		// Peers of every mesh link: batman-adv routes over any of them.
		peers := append([]status.Peer{}, st.MeshLink.Peers...)
		for _, ml := range st.MoreLinks {
			peers = append(peers, ml.Peers...)
		}
		printRoutes(w, b, peers, nodeNames(st), nodeOfAddr(st))
		if gs := st.Gossip; gs.Sent > 0 || gs.Received > 0 {
			fmt.Fprintf(w, "  gossip: %d nodes known; sent %d records (%s, last %d bytes), received %d (%s)", len(st.Nodes), gs.Sent, humanBytes(gs.BytesSent), gs.LastRecordBytes, gs.Received, humanBytes(gs.BytesReceived))
			if gs.AuthFailed > 0 {
				fmt.Fprintf(w, ", %d failed authentication", gs.AuthFailed)
			}
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "  multicast: %s; gateways: %d\n", mcastSummary(b.Multicast), len(b.Gateways))
	}

	br := st.Bridge
	if br.Present {
		fmt.Fprintf(w, "bridge: %s %s ports %s mtu %d\n", br.Name, br.Address, portList(br.Ports), br.MTU)
	}
	printPorts(w, st)
	printClientNet(w, st)
	printGateway(w, st)
	if st.SiteLink.Member {
		fmt.Fprintln(w)
		printSiteLink(w, st.SiteLink)
	}

	if st.Reconcile.LastPass.IsZero() {
		fmt.Fprintf(w, "\nreconcile: first pass still running\n")
	} else {
		fmt.Fprintf(w, "\nreconcile: %d passes, last %s ago took %s, converged=%v; meshd pid %d, %d open files\n", st.Reconcile.Passes, time.Since(st.Reconcile.LastPass).Round(time.Second), st.Reconcile.LastDuration, st.Reconcile.Converged, st.Process.PID, st.Process.OpenFiles)
	}
	if len(st.Warnings) > 0 {
		fmt.Fprintf(w, "\nWarnings (%d):\n", len(st.Warnings))
		for _, wn := range st.Warnings {
			fmt.Fprintf(w, "  [%s] %s: %s\n", strings.ToUpper(wn.Level), wn.Code, wn.Message)
		}
	}
}

func cmdRadios(ctx context.Context, c *api.Client, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("radios", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := c.Status(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"radios": st.Radios, "plan": st.Plan})
	}
	for _, r := range st.Radios {
		fmt.Fprintf(stdout, "%s  role=%s recommended=%s\n", r.Phy, r.Role, r.Recommended)
		fmt.Fprintf(stdout, "  fingerprint: %s (%s)\n", r.Fingerprint, r.FingerprintSource)
		fmt.Fprintf(stdout, "  hardware: %s driver %s, %s %s, id %s, perm mac %s\n", r.Chipset, r.Driver, r.Bus, r.Port, r.DeviceID, r.PermAddr)
		fmt.Fprintf(stdout, "  capabilities: mesh=%v ap=%v both=%v bands=%s streams=%d\n", r.MeshPoint, r.AP, r.MeshAndAP, strings.Join(r.Bands, "/"), r.Streams)
		for b, p := range r.Power {
			fmt.Fprintf(stdout, "  max power %s: %.1f dBm (%s)\n", b, p.DBm, p.Source)
		}
		if len(r.Interfaces) > 0 {
			fmt.Fprintf(stdout, "  interfaces: %s\n", strings.Join(r.Interfaces, ", "))
		}
		if r.PCIe != nil {
			fmt.Fprintf(stdout, "  pcie link %s: %s\n", r.PCIe.Device, r.PCIe)
		}
		if r.Thermal != nil {
			fmt.Fprintf(stdout, "  temperature: %s\n", r.Thermal)
		}
		for _, q := range r.Quirks {
			fmt.Fprintf(stdout, "  quirk: %s\n", q)
		}
		if r.Broken != "" {
			fmt.Fprintf(stdout, "  broken: %s\n", r.Broken)
		}
		if r.InUse != "" {
			fmt.Fprintf(stdout, "  in use by the system, left alone: %s\n", r.InUse)
		}
	}
	for _, n := range st.Plan.Notices {
		fmt.Fprintf(stdout, "[%s] %s: %s\n", strings.ToUpper(n.Level), n.Code, n.Message)
	}
	return nil
}

func cmdDoctor(version, socket string, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	nodePath := fs.String("node", config.DefaultNodePath, "node config to validate")
	profilePath := fs.String("profile", "", "mesh profile to validate (default: from the node config)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rep := doctor.Run(doctor.Options{Version: version, NodePath: *nodePath, ProfilePath: *profilePath, Socket: socket})
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	fmt.Fprint(stdout, rep.Text())
	if rep.Fail > 0 {
		return fmt.Errorf("%d check(s) failed", rep.Fail)
	}
	return nil
}

func cmdConfig(args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "validate" {
		return errors.New("usage: meshctl config validate [node.yaml] [profile.yaml]")
	}
	nodePath, profilePath := config.DefaultNodePath, ""
	if len(args) > 1 {
		nodePath = args[1]
	}
	if len(args) > 2 {
		profilePath = args[2]
	}
	failed := false
	node, err := config.LoadNode(nodePath)
	if err != nil {
		fmt.Fprintf(stdout, "node config %s: INVALID\n  %s\n", nodePath, strings.ReplaceAll(err.Error(), "\n", "\n  "))
		failed = true
	} else {
		fmt.Fprintf(stdout, "node config %s: OK (callsign %s)\n", nodePath, node.Callsign)
		if profilePath == "" {
			profilePath = node.ProfilePath
		}
	}
	if profilePath != "" {
		if p, err := config.LoadProfile(profilePath); err != nil {
			fmt.Fprintf(stdout, "mesh profile %s: INVALID\n  %s\n", profilePath, strings.ReplaceAll(err.Error(), "\n", "\n  "))
			failed = true
		} else {
			fmt.Fprintf(stdout, "mesh profile %s: OK (mesh %q, %s channel %d)\n", profilePath, p.MeshID, p.Radio.Band, p.Radio.Channel)
		}
	}
	if failed {
		return errors.New("configuration is invalid")
	}
	return nil
}

const passphraseAlphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func randomPassphrase(n int) (string, error) {
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 && i%6 == 0 {
			b.WriteByte('-')
		}
		idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(passphraseAlphabet))))
		if err != nil {
			return "", err
		}
		b.WriteByte(passphraseAlphabet[idx.Int64()])
	}
	return b.String(), nil
}

// DefaultChannel is the channel a band switch picks when none is given:
// non-DFS, usable in the US and most of Europe. HaLow's is the middle of the
// US band at its default width of 2 MHz (915 MHz).
var DefaultChannel = map[string]int{config.Band2G: 6, config.Band5G: 149, config.Band6G: 37, config.BandS1G: 26}

func cmdProfile(ctx context.Context, client *api.Client, args []string, stdout io.Writer) error {
	if len(args) > 0 && args[0] == "band" {
		return cmdProfileBand(args[1:], stdout)
	}
	if len(args) > 0 && args[0] == "install" {
		return cmdProfileInstall(ctx, client, args[1:], stdout)
	}
	if len(args) > 0 && args[0] == "push" {
		return cmdProfilePush(ctx, client, args[1:], stdout)
	}
	if len(args) > 0 && args[0] == "edit" {
		return cmdProfileEdit(client, args[1:], stdout)
	}
	if len(args) > 0 && args[0] == "same" {
		return cmdProfileSame(args[1:], stdout)
	}
	if len(args) > 0 && args[0] == "share" {
		ex, err := client.ProfileExport(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Mesh %q: %s channel %d, %d MHz. Contains the mesh secret: authorized personnel only.\n\n", ex.Profile.Name, ex.Profile.Radio.Band, ex.Profile.Radio.Channel, ex.Profile.Radio.Width)
		if c, err := qr.Encode(ex.Code, qr.M); err == nil {
			io.WriteString(stdout, c.TerminalANSI())
		}
		fmt.Fprintf(stdout, "\nProfile code (paste it into the new node's setup page, or: meshctl profile install):\n%s\n", ex.Code)
		return nil
	}
	if len(args) > 0 && args[0] == "show" {
		path := config.DefaultProfilePath
		if len(args) > 1 {
			path = args[1]
		}
		if _, err := config.LoadProfile(path); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		_, err = stdout.Write(data)
		return err
	}
	if len(args) == 0 || args[0] != "new" {
		return errors.New("usage: meshctl profile new --name NAME [--band 5g] [--channel 149] [--width MHz] [--country US] [--mode country]")
	}
	fs := flag.NewFlagSet("profile new", flag.ContinueOnError)
	name := fs.String("name", "", "mesh name (required)")
	band := fs.String("band", "5g", "mesh band: 2g, 5g, 6g or s1g (HaLow)")
	channel := fs.Int("channel", 0, "channel (default 149 on 5g, 6 on 2g, 37 on 6g, 26 on s1g)")
	country := fs.String("country", "US", "ISO country code")
	mode := fs.String("mode", config.RegModeCountry, "regulatory mode: country or unrestricted")
	width := fs.Int("width", 0, "channel width in MHz (default 20; on s1g 2, of 1, 2, 4 or 8)")
	out := fs.String("o", "", "write to this file (mode 0600) instead of stdout")
	force := fs.Bool("force", false, "overwrite an existing file")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *name == "" {
		return errors.New("--name is required")
	}
	if *channel == 0 {
		*channel = DefaultChannel[*band]
	}
	pass, err := randomPassphrase(24)
	if err != nil {
		return err
	}
	p := config.Profile{
		Schema: config.SchemaVersion, Name: *name, MeshID: "trellis-" + *name, SAEPassphrase: pass,
		Regulatory: config.Regulatory{Mode: *mode, Country: strings.ToUpper(*country)},
		Radio:      config.RadioPlan{Band: *band, Channel: *channel, Width: *width},
	}
	p.ApplyDefaults()
	if err := p.Validate(); err != nil {
		return err
	}
	return emit(stdout, *out, *force, 0o600, renderProfile(&p))
}

// renderProfile writes a profile as commented YAML.
func renderProfile(p *config.Profile) string {
	var buf strings.Builder
	fmt.Fprintf(&buf, `# Trellis mesh profile. Share this file (or its QR code) with every node in
# the mesh. It holds the mesh secret: keep it private.
schema: %d
name: %s
mesh_id: %s
sae_passphrase: %s
regulatory:
  mode: %s          # country: %s's rules | unrestricted: no regulatory limits (scripts/install-regulatory.sh)
  country: %s
radio:
  band: %s
  channel: %d
  width: %d
`, p.Schema, p.Name, p.MeshID, p.SAEPassphrase, p.Regulatory.Mode, p.Regulatory.Country, p.Regulatory.Country,
		p.Radio.Band, p.Radio.Channel, p.Radio.Width)
	if len(p.Links) > 0 {
		buf.WriteString("links:             # more mesh links, each on its own radio; a node runs those it has radios for\n")
		for _, l := range p.Links {
			fmt.Fprintf(&buf, "  - {band: %s, channel: %d, width: %d}\n", l.Band, l.Channel, l.Width)
		}
	}
	fmt.Fprintf(&buf, `prefix: %s
batman:
  routing_algo: %s
  orig_interval_ms: %d
  min_version: "%s"
  pinned_version: "%s"
`, p.Prefix, p.Batman.RoutingAlgo, p.Batman.OrigIntervalMS, p.Batman.MinVersion, p.Batman.PinnedVersion)
	return buf.String()
}

// emit prints content, or writes it to path atomically with the given mode.
// An existing file is only replaced with force.
func emit(stdout io.Writer, path string, force bool, mode os.FileMode, content string) error {
	if path == "" {
		_, err := io.WriteString(stdout, content)
		return err
	}
	if _, err := os.Stat(path); err == nil && !force {
		return fmt.Errorf("%s already exists; not overwriting it (use --force to replace it)", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// Durable before it replaces anything: deploys write node.yaml this
	// way, and a node may lose power soon after.
	if err := fsutil.WriteFile(path, []byte(content), mode); err != nil {
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("%w (run with sudo)", err)
		}
		return err
	}
	fmt.Fprintf(stdout, "wrote %s\n", path)
	return nil
}

func cmdNode(args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "new" {
		return errors.New("usage: meshctl node new --callsign NAME [--id N]")
	}
	fs := flag.NewFlagSet("node new", flag.ContinueOnError)
	callsign := fs.String("callsign", "", "node callsign, a DNS label (required)")
	id := fs.Int("id", 0, "pinned node id 1–254 (0 = automatic)")
	out := fs.String("o", "", "write to this file instead of stdout")
	force := fs.Bool("force", false, "overwrite an existing file")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *callsign == "" {
		return errors.New("--callsign is required")
	}
	n := config.Node{Schema: config.SchemaVersion, Callsign: *callsign, NodeID: *id}
	n.ApplyDefaults()
	if err := n.Validate(); err != nil {
		return err
	}
	var buf strings.Builder
	fmt.Fprintf(&buf, `# Trellis node configuration for one node.
schema: %d
callsign: %s
node_id: %d            # 0 = claim automatically (derived from the node MAC until gossip lands)
profile: %s

radios:
  roles:
    mesh: auto         # auto | disabled | {radio: <fingerprint>} | {port: usb:1-1.2}
    ap: auto
  devices: []          # per-radio: {match: {fingerprint: ...}, label: ..., ignore: false, max_power_dbm: 20}

ports: []              # - {name: eth0, role: auto}   roles: auto | wan | client | mesh (wired backhaul)

ap:
  enabled: true
  country: US
  band: 5g

mesh_mtu: %d
allow_old_batman: false

firewall:
  wan: closed          # from outside the mesh: closed (replies, ICMP, DHCP, SSH, allow) or open
  ssh: true            # port 22 stays open while closed
  allow: []            # further ports open while closed: tcp/5201, udp/51820
`, n.Schema, n.Callsign, n.NodeID, n.ProfilePath, n.MeshMTU)
	return emit(stdout, *out, *force, 0o644, buf.String())
}

// cmdProfileSame compares two profile files by what they say, not byte for
// byte: comments, spelling (radio2 or links), defaults written out or left
// to be filled in, and key order do not count.
func cmdProfileSame(args []string, stdout io.Writer) error {
	if len(args) != 2 {
		return errors.New("usage: meshctl profile same FILE1 FILE2")
	}
	var docs [2]map[string]any
	for i, path := range args {
		p, err := config.LoadProfile(path)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		// The revision counts changes; it is not a setting.
		p.Revision = 0
		data, err := p.YAML("")
		if err != nil {
			return err
		}
		if err := yaml.Unmarshal(data, &docs[i]); err != nil {
			return err
		}
	}
	if diff := yamlDiff("", docs[0], docs[1]); len(diff) > 0 {
		return fmt.Errorf("the profiles differ in %s", strings.Join(diff, ", "))
	}
	fmt.Fprintln(stdout, "the profiles are the same")
	return nil
}

// yamlDiff names the settings that differ between two decoded documents,
// descending into maps.
func yamlDiff(prefix string, a, b map[string]any) []string {
	var out []string
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	for _, k := range slices.Sorted(maps.Keys(keys)) {
		am, aok := a[k].(map[string]any)
		bm, bok := b[k].(map[string]any)
		switch {
		case aok && bok:
			out = append(out, yamlDiff(prefix+k+".", am, bm)...)
		case !reflect.DeepEqual(a[k], b[k]):
			out = append(out, prefix+k)
		}
	}
	return out
}

// cmdProfileBand switches a profile file to another band.
func cmdProfileBand(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("profile band", flag.ContinueOnError)
	channel := fs.Int("channel", 0, "channel (default: 6 on 2g, 149 on 5g, 37 on 6g, 26 on s1g)")
	width := fs.Int("width", 0, "channel width in MHz (default: keep, or the band's default if it does not allow it)")
	// Accept "band 2g [flags] [FILE]" in any order.
	var pos []string
	rest := args
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	if len(pos) == 0 || len(pos) > 2 {
		return errors.New("usage: meshctl profile band 2g|5g|6g|s1g [--channel N] [--width MHz] [FILE (default mesh-profile.yaml)]")
	}
	band, path := pos[0], "mesh-profile.yaml"
	if len(pos) == 2 {
		path = pos[1]
	}
	p, err := config.LoadProfile(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	old := p.Radio
	p.Radio.Band = band
	p.Radio.Channel = *channel
	if p.Radio.Channel == 0 {
		p.Radio.Channel = DefaultChannel[band]
	}
	if *width != 0 {
		p.Radio.Width = *width
	} else if (band == config.Band2G && p.Radio.Width > 40) || (band == config.BandS1G) != (old.Band == config.BandS1G) {
		// Wi-Fi and HaLow widths have nothing in common.
		p.Radio.Width = config.DefaultWidth(band)
	}
	if err := p.Validate(); err != nil {
		return err
	}
	if p.Radio == old {
		fmt.Fprintf(stdout, "%s already uses %s channel %d (%d MHz wide)\n", path, band, p.Radio.Channel, p.Radio.Width)
		return nil
	}
	// Every setting of the file kept (talk groups, site link, batman
	// options), not only those of a new profile.
	data, err := p.YAML("Trellis mesh profile. Share it with every node in the mesh.\nIt holds the mesh secret: keep it private.")
	if err != nil {
		return err
	}
	if err := emit(io.Discard, path, true, 0o600, string(data)); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s: %s ch %d (%d MHz) -> %s ch %d (%d MHz)\n", path, old.Band, old.Channel, old.Width, p.Radio.Band, p.Radio.Channel, p.Radio.Width)
	return nil
}

// cmdProfileInstall validates a profile and installs it on this node, then
// asks meshd to apply it.
func cmdProfileInstall(ctx context.Context, client *api.Client, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("profile install", flag.ContinueOnError)
	noReload := fs.Bool("no-reload", false, "only write the file")
	target := fs.String("target", "", "where to install (default: the profile path in node.yaml)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	src := "-"
	if fs.NArg() > 0 {
		src = fs.Arg(0)
	}
	var data []byte
	var err error
	if src == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(src)
	}
	if err != nil {
		return err
	}
	var p *config.Profile
	if config.IsProfileCode(string(data)) {
		// A profile code (meshctl profile share, or the web UI): install
		// it as the file it stands for.
		if p, err = config.DecodeProfileCode(string(data)); err == nil {
			// A code for the mesh the node is on keeps the talk groups
			// and site link of the profile in place (a code has neither).
			if cur, cerr := config.LoadProfile(profileTarget(*target)); cerr == nil {
				p.KeepUncoded(cur)
				err = p.Validate()
			}
		}
		if err == nil {
			data, err = p.YAML("Trellis mesh profile, installed from a profile code. It holds the mesh secret: keep it private.")
		}
	} else {
		p, err = config.ParseProfile(data)
	}
	if err != nil {
		return fmt.Errorf("profile rejected, nothing installed: %w", err)
	}
	dst := profileTarget(*target)
	if cur, err := os.ReadFile(dst); err == nil && bytes.Equal(cur, data) {
		fmt.Fprintf(stdout, "%s unchanged (%s ch %d)\n", dst, p.Radio.Band, p.Radio.Channel)
		return nil
	}
	if err := emit(io.Discard, dst, true, 0o600, string(data)); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "installed %s (mesh %q, %s ch %d, %d MHz)\n", dst, p.MeshID, p.Radio.Band, p.Radio.Channel, p.Radio.Width)
	if *noReload {
		return nil
	}
	if err := client.Reload(ctx); err != nil {
		if errors.Is(err, api.ErrNotRunning) {
			fmt.Fprintln(stdout, "meshd not running; profile applies on start")
			return nil
		}
		return fmt.Errorf("installed, but meshd rejected the reload: %w", err)
	}
	fmt.Fprintln(stdout, "meshd is applying it now")
	return nil
}

// profileTarget is the file profile install writes: the one given, else
// the node configuration's, else the default.
func profileTarget(target string) string {
	if target != "" {
		return target
	}
	if n, err := config.LoadNode(config.DefaultNodePath); err == nil && n.ProfilePath != "" {
		return n.ProfilePath
	}
	return config.DefaultProfilePath
}

// meshRadioMaxDBm returns the known maximum transmit power of the mesh radio
// on the mesh band.
func meshRadioMaxDBm(st *status.Status) (float64, bool) {
	for _, r := range st.Radios {
		if !strings.Contains(r.Role, "mesh") {
			continue
		}
		if pf, ok := r.Power[st.Profile.Band]; ok && pf.DBm > 0 {
			return pf.DBm, true
		}
	}
	return 0, false
}

// saeSummary condenses SAE events to one line per peer. Events from before a
// peer's current established link are dropped: they are history, not a
// problem.
func saeSummary(events []status.SAEEvent, peers []status.Peer, now time.Time) []string {
	estabSince := map[string]time.Time{}
	for _, p := range peers {
		if p.Established {
			estabSince[p.MAC] = now.Add(-time.Duration(p.ConnectedSeconds) * time.Second)
		}
	}
	type agg struct {
		failures    int
		last        time.Time
		blocked     bool
		blockedTime time.Time
	}
	var order []string
	byMAC := map[string]*agg{}
	for _, e := range events {
		if since, ok := estabSince[e.MAC]; ok && !e.Time.After(since) {
			continue
		}
		a := byMAC[e.MAC]
		if a == nil {
			a = &agg{}
			byMAC[e.MAC] = a
			order = append(order, e.MAC)
		}
		if e.Kind == "blocked" {
			a.blocked, a.blockedTime = true, e.Time
		} else {
			a.failures++
			if e.Time.After(a.last) {
				a.last = e.Time
			}
		}
	}
	ago := func(t time.Time) string { return now.Sub(t).Round(time.Second).String() + " ago" }
	var out []string
	for _, mac := range order {
		a := byMAC[mac]
		line := "SAE " + mac + ":"
		if a.failures > 0 {
			noun := "failure"
			if a.failures > 1 {
				noun = "failures"
			}
			line += fmt.Sprintf(" %d authentication %s, last %s", a.failures, noun, ago(a.last))
		}
		if a.blocked {
			if a.failures > 0 {
				line += ";"
			}
			line += " blocked " + ago(a.blockedTime)
		}
		// A failure is a peer that did not answer as often as a wrong
		// passphrase: one that left the channel, or restarted, mid-way.
		line += ". The peer did not answer (it changed channel or restarted: this clears by itself), or it has a different passphrase (that node needs this mesh profile)"
		out = append(out, line)
	}
	return out
}

func portList(ports []string) string {
	if len(ports) == 0 {
		return "none"
	}
	return strings.Join(ports, ",")
}

// printLinks shows each direct link once: the 802.11s peer link (radio
// layer) next to batman-adv's estimate of what it can carry. batman
// neighbours without a radio peer (wired ports, or a peer that has just
// gone) are listed with blank radio fields.
func printLinks(w io.Writer, peers []status.Peer, neighbors []status.Neighbor, names map[string]string) {
	if len(peers) == 0 && len(neighbors) == 0 {
		return
	}
	est := map[string]status.Neighbor{}
	for _, n := range neighbors {
		est[n.MAC] = n
	}
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  DIRECT LINK\tNODE\tSTATE\tSIGNAL\tRADIO TX/RX\tBATMAN EST.\tINACTIVE\tCONNECTED")
	seen := map[string]bool{}
	for _, p := range peers {
		seen[p.MAC] = true
		sig := "-"
		if p.SignalDBm != 0 {
			sig = fmt.Sprintf("%d dBm", p.SignalDBm)
		}
		inactive := fmt.Sprintf("%d ms", p.InactiveMS)
		if p.InactiveMS >= 10000 {
			inactive = (time.Duration(p.InactiveMS) * time.Millisecond).Round(time.Second).String()
		}
		bat := "-"
		if n, ok := est[p.MAC]; ok {
			bat = fmt.Sprintf("%.1f Mbit/s", n.ThroughputMbps)
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%.0f/%.0f Mbit/s\t%s\t%s\t%s\n", p.MAC, nameOf(names, p.MAC), p.PlinkState, sig, p.TXMbps, p.RXMbps, bat, inactive, (time.Duration(p.ConnectedSeconds) * time.Second).String())
	}
	for _, n := range neighbors {
		if seen[n.MAC] {
			continue
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t-\t-\t%.1f Mbit/s\t%d ms\t-\n", n.MAC, nameOf(names, n.MAC), "batman only ("+n.Via+")", n.ThroughputMbps, n.LastSeenMS)
	}
	tw.Flush()
}

// printRoutes shows how traffic reaches every other node: straight to it, or
// relayed through a neighbour. batman-adv only knows the first hop, so a
// relayed route may be longer than two hops; batctl tr shows the full path.
func printRoutes(w io.Writer, b status.Batman, peers []status.Peer, names map[string]string, node map[string]string) {
	var routes []status.Originator
	for _, o := range b.Originators {
		if o.Best {
			routes = append(routes, o)
		}
	}
	if len(routes) == 0 {
		fmt.Fprintln(w, "  no other nodes seen")
		return
	}
	// same reports whether two addresses are one node's. With several mesh
	// links a node has one address per link; batman-adv names it by the
	// first (the originator) and may reach it over another.
	same := func(a, b string) bool {
		if strings.EqualFold(a, b) {
			return true
		}
		na, nb := node[strings.ToLower(a)], node[strings.ToLower(b)]
		return na != "" && na == nb
	}
	var estab []string
	for _, p := range peers {
		if p.Established {
			estab = append(estab, p.MAC)
		}
	}
	direct := func(o status.Originator) bool { return same(o.NextHop, o.MAC) }
	sort.SliceStable(routes, func(i, j int) bool {
		if di, dj := direct(routes[i]), direct(routes[j]); di != dj {
			return di
		}
		return routes[i].MAC < routes[j].MAC
	})
	multiIf := len(b.HardIfs) > 1
	relayed := 0
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  ROUTE TO\tNODE\tPATH\tPATH THROUGHPUT\tLAST SEEN")
	for _, o := range routes {
		path := "direct"
		if !direct(o) {
			relayed++
			path = "relayed via " + o.NextHop
			if n := names[strings.ToLower(o.NextHop)]; n != "" {
				path = "relayed via " + n
			}
			for _, e := range estab {
				if same(e, o.MAC) {
					path += " (beats the direct link)"
					break
				}
			}
		}
		if multiIf {
			path += " on " + o.Via
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%.1f Mbit/s\t%d ms\n", o.MAC, nameOf(names, o.MAC), path, o.ThroughputMbps, o.LastSeenMS)
	}
	tw.Flush()
	fmt.Fprintf(w, "  %d other nodes: %d direct, %d relayed (batctl tr <mac> shows every hop)\n", len(routes), len(routes)-relayed, relayed)
}

// printClientNet renders the anycast gateway, filters, DHCP and the client
// AP with its clients.
func printClientNet(w io.Writer, st *status.Status) {
	cn, ap := st.ClientNet, st.AP
	var parts []string
	if cn.Anycast.Present {
		parts = append(parts, "gateway "+cn.Anycast.Address+" on "+cn.Anycast.Interface)
	} else {
		parts = append(parts, "gateway not up")
	}
	switch {
	case cn.Filters.Applied:
		var dropped uint64
		for _, n := range cn.Filters.Drops {
			dropped += n
		}
		parts = append(parts, fmt.Sprintf("bat0 filters on (%d rules, %d frames dropped)", cn.Filters.Rules, dropped))
	case cn.Filters.Error != "":
		parts = append(parts, "bat0 filters FAILED")
	default:
		parts = append(parts, "bat0 filters off")
	}
	if cn.DHCP.Dnsmasq.Running {
		parts = append(parts, fmt.Sprintf("dnsmasq pid %d (DHCP %s–%s, DNS .%s)", cn.DHCP.Dnsmasq.PID, cn.DHCP.PoolStart, cn.DHCP.PoolEnd, cn.DHCP.Domain))
	} else {
		parts = append(parts, "dnsmasq not running")
	}
	fmt.Fprintf(w, "client network: %s\n", strings.Join(parts, "; "))

	switch {
	case !ap.Enabled:
		fmt.Fprintln(w, "Client AP: disabled in node.yaml")
	case ap.Interface == "":
		fmt.Fprintln(w, "Client AP: no radio available (see warnings)")
	case ap.Channel == 0:
		fmt.Fprintf(w, "Client AP: %s not started (see warnings)\n", ap.Interface)
	default:
		shared := ""
		if ap.SharesMeshRadio {
			shared = ", shares the mesh radio"
		}
		state := "hostapd " + orDash(ap.Hostapd.State)
		if !ap.Hostapd.Running {
			state = "hostapd not running"
			if ap.Hostapd.Restarts > 0 {
				state = fmt.Sprintf("hostapd keeps exiting (%d times, see the journal)", ap.Hostapd.Restarts)
			}
		}
		fmt.Fprintf(w, "Client AP: %s %q %s ch %d (%d MHz)%s, %s, %d clients", ap.Interface, ap.SSID, ap.Band, ap.Channel, ap.FreqMHz, shared, state, len(ap.Clients))
		if ap.TXPowerDBm > 0 {
			fmt.Fprintf(w, ", tx %.0f dBm", ap.TXPowerDBm)
		}
		fmt.Fprintln(w)
	}
	if len(ap.Clients) > 0 {
		tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "  CLIENT\tSIGNAL\tTX/RX Mbps\tCONNECTED\tADDRESS\tHOSTNAME")
		for _, c := range ap.Clients {
			fmt.Fprintf(tw, "  %s\t%d dBm\t%.0f/%.0f\t%s\t%s\t%s\n", c.MAC, c.SignalDBm, c.TXMbps, c.RXMbps,
				(time.Duration(c.ConnectedSeconds) * time.Second).String(), orDash(c.IP), orDash(c.Hostname))
		}
		tw.Flush()
	}
	wired := wiredLeases(st)
	if len(wired) > 0 {
		tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "  OTHER LEASE\tADDRESS\tHOSTNAME\tEXPIRES")
		for _, l := range wired {
			fmt.Fprintf(tw, "  %s\t%s\t%s\tin %s\n", l.MAC, l.IP, orDash(l.Hostname), time.Until(l.Expires).Round(time.Minute))
		}
		tw.Flush()
	}
}

// wiredLeases returns leases whose holder is not an AP client: wired
// devices, or phones that have left but still hold an address.
func wiredLeases(st *status.Status) []status.Lease {
	joined := map[string]bool{}
	for _, c := range st.AP.Clients {
		joined[strings.ToLower(c.MAC)] = true
	}
	var out []status.Lease
	for _, l := range st.ClientNet.Leases {
		if !joined[strings.ToLower(l.MAC)] && time.Until(l.Expires) > 0 {
			out = append(out, l)
		}
	}
	return out
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func cmdAP(ctx context.Context, c *api.Client, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("ap", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	j, err := c.APJoin(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(j)
	}
	fmt.Fprintf(stdout, "SSID:       %s\n", j.SSID)
	fmt.Fprintf(stdout, "passphrase: %s\n", j.Passphrase)
	if j.Channel != 0 {
		fmt.Fprintf(stdout, "channel:    %s ch %d\n", j.Band, j.Channel)
	}
	if j.Note != "" {
		fmt.Fprintf(stdout, "note:       %s\n", j.Note)
	}
	if c, err := qr.Encode(j.WiFiQR, qr.M); err == nil {
		fmt.Fprintln(stdout)
		io.WriteString(stdout, c.TerminalANSI())
	}
	return nil
}

func cmdClients(ctx context.Context, c *api.Client, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("clients", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := c.Status(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"ap_clients": st.AP.Clients, "leases": st.ClientNet.Leases})
	}
	if len(st.AP.Clients) == 0 && len(wiredLeases(st)) == 0 {
		fmt.Fprintln(stdout, "no clients joined to this node")
		return nil
	}
	printClientNet(stdout, st)
	return nil
}

// nodeNames maps mesh-interface MACs to callsigns, from gossip.
func nodeNames(st *status.Status) map[string]string {
	m := map[string]string{}
	for _, n := range st.Nodes {
		label := n.Callsign
		if label == "" {
			label = fmt.Sprintf("node %d", n.NodeID)
		}
		for _, o := range n.Originators {
			m[strings.ToLower(o)] = label
		}
		m[strings.ToLower(n.MAC)] = label
	}
	return m
}

// nodeOfAddr maps every address a node goes by (identity MAC and one per
// mesh link, from gossip) to its identity MAC.
func nodeOfAddr(st *status.Status) map[string]string {
	m := map[string]string{}
	for _, n := range st.Nodes {
		id := strings.ToLower(n.MAC)
		m[id] = id
		for _, o := range n.Originators {
			m[strings.ToLower(o)] = id
		}
	}
	return m
}

func nameOf(names map[string]string, mac string) string {
	if n := names[strings.ToLower(mac)]; n != "" {
		return n
	}
	return "?"
}

// otherLinkUp reports whether a mesh link after the first has an interface
// on this node.
func otherLinkUp(st *status.Status) bool {
	for _, l := range st.MoreLinks {
		if l.Interface != "" {
			return true
		}
	}
	return false
}

// linkNeighbors are the batman-adv neighbours that belong under one mesh
// link: those reached over its interface. The first link also lists those
// reached over wired mesh ports, which belong to no link. A link this node
// has no radio for has none: its neighbours on another link are that
// link's.
func linkNeighbors(st *status.Status, ml status.MeshLink, first bool) []status.Neighbor {
	other := map[string]bool{} // the other links' interfaces
	for _, l := range append([]status.MeshLink{st.MeshLink}, st.MoreLinks...) {
		if l.Interface != "" && l.Interface != ml.Interface {
			other[l.Interface] = true
		}
	}
	var out []status.Neighbor
	for _, n := range st.Batman.Neighbors {
		if (ml.Interface != "" && n.Via == ml.Interface) || (first && n.Via != ml.Interface && !other[n.Via]) {
			out = append(out, n)
		}
	}
	return out
}

// printMeshLink renders one mesh link: interface, supplicant, channel,
// power and its peers. first is the profile's radio (mesh0), the link the
// driver-power hint is about.
// linkTitle names a mesh link by its band and channel, as the web UI does:
// "Mesh link 5 GHz · ch 149".
func linkTitle(ml status.MeshLink) string {
	if ml.Band == "" {
		return "Mesh link"
	}
	return "Mesh link " + config.RadioPlan{Band: ml.Band, Channel: ml.Channel}.Name()
}

func printMeshLink(w io.Writer, title string, ml status.MeshLink, st *status.Status, first bool) {
	fmt.Fprintf(w, "\n%s: transport %s", title, ml.Transport)
	if v := ml.Visit; v != nil {
		fmt.Fprintf(w, ", visiting %s at %d MHz for now (looking for the rest of the mesh)", config.RadioPlan{Band: v.Band, Channel: v.Channel}.Name(), v.WidthMHz)
	}
	if ml.Interface != "" {
		fmt.Fprintf(w, ", %s mtu %d", ml.Interface, ml.MTU)
		sup := "wpa_supplicant stopped"
		if ml.Supplicant.Running {
			sup = fmt.Sprintf("wpa_supplicant pid %d state %s", ml.Supplicant.PID, ml.Supplicant.State)
		}
		fmt.Fprintf(w, ", %s", sup)
		if ml.FreqMHz > 0 {
			if ml.WidthMHz > 0 {
				fmt.Fprintf(w, ", %g MHz, %d MHz wide", ml.FreqMHz, ml.WidthMHz)
			} else {
				fmt.Fprintf(w, ", %g MHz", ml.FreqMHz)
			}
			if ml.RunningWidthMHz != 0 && ml.RunningWidthMHz != ml.WidthMHz {
				fmt.Fprintf(w, ", running %d MHz wide", ml.RunningWidthMHz)
			}
		}
		if ml.TXPowerDBm > 0 {
			fmt.Fprintf(w, ", tx %.0f dBm", ml.TXPowerDBm)
			// Some drivers (rt2800usb, for one) report the regulatory limit
			// rather than what the hardware transmits.
			if hw, ok := meshRadioMaxDBm(st); first && ok && ml.TXPowerDBm > hw {
				fmt.Fprintf(w, " (driver figure; radio max %.0f)", hw)
			}
		}
	} else if !first || otherLinkUp(st) {
		// The first link without a radio is only worth this note when the
		// node meshes over another one; alone, the warnings say why.
		fmt.Fprintf(w, " (no radio here for this link; see the plan notices)")
	}
	if len(ml.WiredPorts) > 0 {
		fmt.Fprintf(w, ", wired %s", strings.Join(ml.WiredPorts, ","))
	}
	fmt.Fprintln(w)
	if ml.WidthCost != nil {
		fmt.Fprintf(w, "  width: %s\n", ml.WidthCost)
	}
	names := nodeNames(st)
	printLinks(w, ml.Peers, linkNeighbors(st, ml, first), names)
	for _, line := range saeSummary(ml.SAEFailures, ml.Peers, time.Now()) {
		fmt.Fprintf(w, "  %s\n", line)
	}
}

// printPorts renders the wired ports: configured roles, and what auto
// ports became (architecture §7.1).
func printPorts(w io.Writer, st *status.Status) {
	if len(st.Ports) == 0 {
		return
	}
	fmt.Fprintf(w, "Ethernet ports (%d):\n", len(st.Ports))
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  PORT\tCONFIG\tROLE\tLINK\tDETAIL")
	for _, p := range st.Ports {
		cfg := p.Configured
		if cfg == "" {
			cfg = "(auto)"
		}
		link := "down"
		if p.Carrier {
			link = "up"
		}
		detail := p.Why
		if l := p.Lease; l != nil {
			detail = fmt.Sprintf("lease %s via %s from %s, expires in %s", l.Address, orDash(l.Router), l.Server, time.Until(l.Expires).Round(time.Second))
		}
		if !p.Since.IsZero() && p.Role != "in-use" {
			detail += fmt.Sprintf(" (%s for %s)", p.Role, time.Since(p.Since).Round(time.Second))
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", p.Name, cfg, p.Role, link, strings.TrimSpace(detail))
	}
	tw.Flush()
}

// printGateway renders the Internet section: this node's WAN, the selected
// gateway, and the gateways batman-adv knows.
func printGateway(w io.Writer, st *status.Status) {
	g := st.Gateway
	if g.Mode == "" {
		return
	}
	wan := "no uplink of its own"
	if g.WAN != "" {
		switch {
		case g.WebCheck == "portal":
			wan = fmt.Sprintf("WAN %s BEHIND A LOGIN PAGE (%s), not shared", g.WAN, orDash(g.WebCheckDetail))
		case g.WANHealthy:
			wan = fmt.Sprintf("WAN %s healthy (%.0f ms, %d%% loss)", g.WAN, g.RTTms, g.LossPct)
		case !g.WANKnown:
			wan = fmt.Sprintf("WAN %s being checked", g.WAN)
		default:
			wan = fmt.Sprintf("WAN %s DOWN (%s)", g.WAN, orDash(g.ProbeError))
		}
	}
	if g.Mode == "off" {
		wan += ", sharing off"
	}
	sel := "NONE: clients reach the mesh only"
	if s := g.Selected; s != nil {
		switch {
		case s.Self && g.WebCheck == "portal":
			sel = "this node, for its own clients (to sign in)"
		case s.Self:
			sel = "this node (sharing it, announced " + g.Announced + ")"
		default:
			sel = fmt.Sprintf("%s %s, %.1f Mbit/s path", s.Callsign, s.Address, s.ThroughputMbps)
		}
		sel += fmt.Sprintf(" for %s", time.Since(s.Since).Round(time.Second))
	}
	fmt.Fprintf(w, "Internet: %s; gateway in use: %s", wan, sel)
	if len(g.DNS) > 0 {
		fmt.Fprintf(w, "; DNS via %s", strings.Join(g.DNS, ","))
	}
	fmt.Fprintln(w)
	switch g.Firewall {
	case "closed":
		open := "no ports"
		if len(g.FirewallOpen) > 0 {
			open = strings.Join(g.FirewallOpen, " ")
		}
		fmt.Fprintf(w, "Firewall: closed on non-mesh interfaces (open: %s, + replies, ICMP, DHCP), %d dropped\n", open, g.FirewallDrop)
	case "open":
		fmt.Fprintln(w, "Firewall: off, no Trellis rules on non-mesh interfaces (firewall.wan)")
	case "failed":
		fmt.Fprintln(w, "Firewall: set closed, rules failed to apply: OPEN (see warnings)")
	}
	if len(g.Candidates) > 0 {
		tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "  GATEWAY\tNODE\tADDRESS\tPATH THROUGHPUT\tLAST SEEN\tUSABLE")
		for _, c := range g.Candidates {
			usable := "yes"
			if !c.Usable {
				usable = "no: " + c.Why
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%.1f Mbit/s\t%d ms\t%s\n", c.Originator, orDash(c.Callsign), orDash(c.Address), c.ThroughputMbps, c.LastSeenMS, usable)
		}
		tw.Flush()
	}
}

// humanBytes renders a byte count with a binary unit.
func humanBytes(n uint64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// mcastSummary explains the multicast path in one line (architecture §9).
func mcastSummary(m status.Multicast) string {
	querier := func(exists, shadowing bool) string {
		switch {
		case !exists:
			return "none"
		case shadowing:
			return "shadowed"
		}
		return "ok"
	}
	path := m.Path
	if path == "" {
		path = "unknown"
	}
	var parts []string
	switch path {
	case "efficient":
		parts = append(parts, "efficient (sent only to nodes with listeners)")
	case "partial":
		n := m.PeersWantAllIPv4
		if m.WantAllIPv4 {
			n++
		}
		parts = append(parts, fmt.Sprintf("partial (%d nodes receive every group)", n))
	default:
		parts = append(parts, path)
	}
	parts = append(parts, fmt.Sprintf("IGMP querier %s, MLD querier %s", querier(m.QuerierIPv4, m.QuerierIPv4Shadowing), querier(m.QuerierIPv6, m.QuerierIPv6Shadowing)))
	if m.LegacyPeers > 0 {
		parts = append(parts, fmt.Sprintf("%d peers without optimisations", m.LegacyPeers))
	}
	if m.PeersNoPtype > 0 {
		parts = append(parts, fmt.Sprintf("%d peers without the multicast packet type", m.PeersNoPtype))
	}
	c := m.Counters
	parts = append(parts, fmt.Sprintf("packet type tx %d rx %d fwd %d", c.TX, c.RX, c.Fwd))
	return strings.Join(parts, "; ")
}

// clockLine says how the node keeps time: "synchronised to GNSS receiver
// (stratum 1, offset 0.4 ms), serving the mesh".
func clockLine(c status.Clock) string {
	if c.Service != "chrony" {
		return "not kept by Trellis: " + c.Note
	}
	if !c.Synced {
		return "chrony, NOT synchronised: " + c.Note
	}
	line := fmt.Sprintf("synchronised to %s (stratum %d, offset %.1f ms)", c.Source, c.Stratum, c.OffsetMS)
	if c.Serving {
		line += ", serving the mesh"
	}
	if len(c.MeshServers) > 0 {
		line += "; mesh time servers " + strings.Join(c.MeshServers, ", ")
	}
	return line
}

// gnssLine describes the receiver: "u-blox on /dev/ttyAMA0, 3D fix, 9 of
// 14 satellites, ±4 m, 30.12346 -97.65432, PPS".
func gnssLine(g status.GNSS) string {
	parts := []string{strings.TrimSpace(g.Driver + " on " + g.Device)}
	if g.Fix == "none" {
		parts = append(parts, "no fix")
	} else {
		parts = append(parts, strings.ToUpper(g.Fix)+" fix")
	}
	if g.SatsSeen > 0 {
		parts = append(parts, fmt.Sprintf("%d of %d satellites", g.SatsUsed, g.SatsSeen))
	}
	if len(g.Signals) > 0 && g.Fix == "none" {
		parts = append(parts, fmt.Sprintf("strongest %d dB-Hz", g.Signals[0]))
	}
	if g.Lat != nil && g.Lon != nil {
		if g.HAccM > 0 {
			parts = append(parts, fmt.Sprintf("±%.0f m", g.HAccM))
		}
		parts = append(parts, fmt.Sprintf("%.5f %.5f", *g.Lat, *g.Lon))
	}
	if g.PPS {
		parts = append(parts, "PPS")
	}
	return strings.Join(parts, ", ")
}

// cmdProfilePush sends a profile to every node of the mesh, from this one:
// they all switch to it at one moment. It always shows what the change
// does first, and sends only with --yes.
func cmdProfilePush(ctx context.Context, client *api.Client, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("profile push", flag.ContinueOnError)
	in := fs.Int("in", 60, "seconds from now at which every node switches (15 to 900)")
	yes := fs.Bool("yes", false, "send it (without: only show what it would do)")
	replace := fs.Bool("replace", false, "send even with a mesh change pending (supersedes it)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: meshctl profile push [--in SECONDS] [--yes] [--replace] FILE")
	}
	data, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	input := api.MeshProfileInput{ProfileInput: api.ProfileInput{YAML: string(data)}, DelaySeconds: *in, Replace: *replace}
	if config.IsProfileCode(strings.TrimSpace(string(data))) {
		input.ProfileInput = api.ProfileInput{Code: strings.TrimSpace(string(data))}
	}
	res, err := client.SetMeshProfile(ctx, input, !*yes)
	if err != nil {
		return err
	}
	if nothingToSend(res, stdout) {
		return nil
	}
	if !*yes {
		fmt.Fprintln(stdout, "\nNothing sent. "+howToSend(res, *replace))
		return nil
	}
	sentLine(res, stdout)
	return nil
}

// nothingToSend prints the review of a mesh-wide change, and reports
// whether there is nothing to send: the mesh runs this profile, and no
// change on its way would be cancelled by sending it.
func nothingToSend(res *api.ApplyResult, stdout io.Writer) bool {
	if !res.Changed && !hasEffectCode(res, "mesh-change-cancelled") {
		fmt.Fprintln(stdout, "nothing to change: the mesh runs this profile")
		return true
	}
	for _, e := range res.Effects {
		fmt.Fprintf(stdout, "  [%s] %s\n", strings.ToUpper(e.Level), e.Message)
	}
	return false
}

func hasEffectCode(res *api.ApplyResult, code string) bool {
	for _, e := range res.Effects {
		if e.Code == code {
			return true
		}
	}
	return false
}

// howToSend says what a review needs to become a change: --yes, and
// --replace too when another change is on its way.
func howToSend(res *api.ApplyResult, replace bool) string {
	if hasEffectCode(res, "mesh-change-replaced") && !replace {
		return "Mesh change pending: add --yes --replace to supersede it, or wait for switchover."
	}
	return "Add --yes to send it to every node."
}

func sentLine(res *api.ApplyResult, stdout io.Writer) {
	fmt.Fprintf(stdout, "\nsent as revision %d; every node switches at %s. Follow it with: meshctl status\n", res.Revision, res.ApplyAt.Local().Format("15:04:05"))
}

// cmdProfileEdit changes the profile the mesh runs: the profile in force
// in the operator's editor, reviewed, and sent to every node with the
// version it was made from, so that it is refused when the profile changed
// on the node meanwhile (as a web UI editor's is). profile push sends a
// file as it is; this starts from the mesh.
func cmdProfileEdit(client *api.Client, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("profile edit", flag.ContinueOnError)
	in := fs.Int("in", 60, "seconds from now at which every node switches (15 to 900)")
	yes := fs.Bool("yes", false, "send it without asking")
	replace := fs.Bool("replace", false, "send even with a mesh change pending (supersedes it)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: meshctl profile edit [--in SECONDS] [--yes] [--replace]")
	}
	// Each request has its own time limit: the editor is open for as long
	// as the operator likes.
	call := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), 30*time.Second)
	}
	ctx, cancel := call()
	cfg, err := client.Config(ctx)
	cancel()
	if err != nil {
		return err
	}
	if cfg.Setup.Needed {
		return errors.New("No mesh profile: install one first (meshctl profile install)")
	}
	orig, err := cfg.Profile.YAML(fmt.Sprintf("The profile mesh %q runs, revision %d. Change it, save and quit: the change\nis reviewed, and sent to every node once you confirm.\nThe secret is not shown: leave sae_passphrase as it is.", cfg.Profile.Name, cfg.Profile.Revision))
	if err != nil {
		return err
	}
	f, err := os.CreateTemp("", "trellis-profile-*.yaml")
	if err != nil {
		return err
	}
	path := f.Name()
	keep := false
	defer func() {
		if !keep {
			os.Remove(path)
		}
	}()
	if _, err = f.Write(orig); err == nil {
		err = f.Close()
	}
	if err != nil {
		return err
	}
	stdin := bufio.NewReader(os.Stdin)
	var input api.MeshProfileInput
	var res *api.ApplyResult
	for {
		if err := runEditor(path); err != nil {
			return err
		}
		edited, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Equal(edited, orig) {
			fmt.Fprintln(stdout, "no changes made: nothing sent")
			return nil
		}
		p, err := config.ParseProfile(edited)
		if err == nil {
			input = api.MeshProfileInput{ProfileInput: api.ProfileInput{Profile: p, Base: cfg.ProfileDigest}, DelaySeconds: *in, Replace: *replace}
			ctx, cancel := call()
			res, err = client.SetMeshProfile(ctx, input, true)
			cancel()
		}
		if err == nil {
			break
		}
		// Made from a profile the node no longer has, or while another
		// change is on its way: editing again would not help.
		var ae *api.Error
		if errors.As(err, &ae) && ae.Status == http.StatusConflict {
			keep = true
			return fmt.Errorf("%w\nNothing sent. Your edits are kept in %s", err, path)
		}
		fmt.Fprintf(stdout, "error: %v\n", err)
		if *yes || !ask(stdin, stdout, "Edit again?", true) {
			keep = true
			return fmt.Errorf("nothing sent; your edits are kept in %s", path)
		}
	}
	if nothingToSend(res, stdout) {
		return nil
	}
	if !*yes && !ask(stdin, stdout, "\nSend this to every node?", false) {
		keep = true
		fmt.Fprintf(stdout, "Nothing sent. Your edits are kept in %s\n", path)
		return nil
	}
	ctx, cancel = call()
	res, err = client.SetMeshProfile(ctx, input, false)
	cancel()
	if err != nil {
		keep = true
		return fmt.Errorf("%w\nNothing sent. Your edits are kept in %s", err, path)
	}
	sentLine(res, stdout)
	return nil
}

// runEditor opens path in the operator's editor ($VISUAL, $EDITOR, else
// the first of sensible-editor, nano, vi) on this terminal, and waits.
func runEditor(path string) error {
	var argv []string
	for _, v := range []string{"VISUAL", "EDITOR"} {
		if argv = strings.Fields(os.Getenv(v)); len(argv) > 0 {
			break
		}
	}
	if len(argv) == 0 {
		for _, e := range []string{"sensible-editor", "nano", "vi"} {
			if _, err := exec.LookPath(e); err == nil {
				argv = []string{e}
				break
			}
		}
	}
	if len(argv) == 0 {
		return errors.New("No editor: set EDITOR (e.g. EDITOR=nano meshctl profile edit)")
	}
	cmd := exec.Command(argv[0], append(argv[1:], path)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("the editor (%s) failed: %w", argv[0], err)
	}
	return nil
}

// ask puts a yes/no question on the terminal; an empty answer is def. No
// answer at all (the input has ended: no terminal) is no, whatever def
// is, so that a question asked again and again comes to an end.
func ask(in *bufio.Reader, out io.Writer, question string, def bool) bool {
	fmt.Fprintf(out, "%s %s ", question, map[bool]string{true: "[Y/n]", false: "[y/N]"}[def])
	line, err := in.ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		fmt.Fprintln(out)
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	case "n", "no":
		return false
	}
	return def
}

// meshChangeLine describes a mesh-wide change: "revision 4 from relay-1:
// 5 GHz · ch 149 at 80 MHz; switches at 14:05:00 (in 42s); node-2 has it,
// node-3 sent".
func meshChangeLine(mc *status.MeshChange, now time.Time) string {
	line := fmt.Sprintf("revision %d from %s", mc.Revision, mc.From)
	if mc.Summary != "" {
		line += ": " + mc.Summary
	}
	switch mc.State {
	case "pending":
		line += fmt.Sprintf("; switches at %s (in %s)", mc.ApplyAt.Local().Format("15:04:05"), mc.ApplyAt.Sub(now).Round(time.Second))
	case "applied":
		line += fmt.Sprintf("; in force since %s", mc.ApplyAt.Local().Format("15:04:05"))
	default:
		line += "; FAILED: " + mc.Error
	}
	var nodes []string
	for _, n := range mc.Nodes {
		nodes = append(nodes, n.Callsign+" "+n.State)
	}
	if len(nodes) > 0 {
		line += "; " + strings.Join(nodes, ", ")
	}
	return line
}

// surveyWait bounds survey --start's wait for the round to end.
var surveyWait = 2 * time.Minute

// cmdSurvey shows the last mesh-wide channel survey, or runs one.
func cmdSurvey(ctx context.Context, client *api.Client, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("survey", flag.ContinueOnError)
	start := fs.Bool("start", false, "run a survey now and wait for the nodes to answer")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var sv *status.Survey
	var err error
	if *start {
		if sv, err = client.StartSurvey(ctx); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "Surveying: all nodes scanning their link bands…")
		// meshd ends a round within a minute (45 s for answers, asking
		// again every 5 s); a meshd restarted meanwhile has none (nil).
		ctx, cancel := context.WithTimeout(ctx, surveyWait)
		defer cancel()
		for sv != nil && !sv.Done {
			select {
			case <-ctx.Done():
				return fmt.Errorf("the survey did not finish in %v", surveyWait)
			case <-time.After(2 * time.Second):
			}
			if sv, err = client.Survey(ctx); err != nil {
				if ctx.Err() != nil {
					return fmt.Errorf("the survey did not finish in %v", surveyWait)
				}
				return err
			}
		}
	} else if sv, err = client.Survey(ctx); err != nil {
		return err
	}
	if sv == nil {
		fmt.Fprintln(stdout, "no survey yet: meshctl survey --start runs one")
		return nil
	}
	answered := 0
	var missing []string
	for _, n := range sv.Nodes {
		if n.State == "answered" {
			answered++
		} else {
			missing = append(missing, n.Callsign+" "+n.State)
		}
	}
	fmt.Fprintf(stdout, "survey of %s: %d of %d nodes answered", sv.Started.Local().Format("15:04:05"), answered, len(sv.Nodes))
	if len(missing) > 0 {
		fmt.Fprintf(stdout, " (%s)", strings.Join(missing, ", "))
	}
	fmt.Fprintln(stdout)
	for _, s := range sv.Suggestions {
		fmt.Fprintf(stdout, "\n%s at %d MHz: %s\n", s.Name, s.Width, s.Note)
		if len(s.Ranked) == 0 {
			continue
		}
		tw := tabwriter.NewWriter(stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "  CHANNEL\tBUSY AT WORST\tAT\tOTHER NETWORKS\t")
		rows := s.Ranked
		if s.Current != nil && !slices.ContainsFunc(rows, func(r status.ChannelScore) bool { return r.Ch == s.Current.Ch }) {
			rows = append(rows, *s.Current)
		}
		for _, r := range rows {
			busy := "-"
			if r.WorstBusy >= 0 {
				busy = fmt.Sprintf("%d%%", r.WorstBusy)
			}
			mark := ""
			if s.Current != nil && r.Ch == s.Current.Ch {
				mark = " (now)"
			}
			if r.DFS {
				mark += " radar"
			}
			fmt.Fprintf(tw, "  %d%s\t%s\t%s\t%d\t\n", r.Ch, mark, busy, r.WorstNode, r.Nets)
		}
		tw.Flush()
	}
	return nil
}

// hostLine is the computer the node runs on in a line: board, uptime, its
// hottest sensor, CPU, memory, each filesystem, and its supply when the
// board reports it. Empty when meshd reports nothing (an older version).
func hostLine(h status.Host) string {
	if h.CPUs == 0 && h.MemTotal == 0 {
		return ""
	}
	var parts []string
	first := h.Model
	if h.UptimeS > 0 {
		sec := int(h.UptimeS)
		up := fmt.Sprintf("%dm", sec/60)
		switch {
		case sec >= 86400:
			up = fmt.Sprintf("%dd %dh", sec/86400, sec%86400/3600)
		case sec >= 3600:
			up = fmt.Sprintf("%dh %02dm", sec/3600, sec%3600/60)
		}
		if first != "" {
			first += ", "
		}
		first += "up " + up
	}
	if first != "" {
		parts = append(parts, first)
	}
	if len(h.Temps) > 0 {
		t := h.Temps[0]
		s := fmt.Sprintf("%s %.0f °C", t.Name, t.TempC)
		if t.TempC >= t.HotC {
			s += fmt.Sprintf(" HIGH (throttle at %.0f °C)", t.HotC)
		}
		parts = append(parts, s)
	}
	cpu := "CPU –"
	if h.CPUPct >= 0 {
		cpu = fmt.Sprintf("CPU %.0f%%", h.CPUPct)
	}
	parts = append(parts, fmt.Sprintf("%s (load %.2f, %d CPUs)", cpu, h.Load[0], h.CPUs))
	if h.MemTotal > 0 {
		parts = append(parts, fmt.Sprintf("memory %.0f%% used of %s", 100*(1-float64(h.MemAvailable)/float64(h.MemTotal)), hoststat.Bytes(h.MemTotal)))
	}
	var fs []string
	for _, f := range h.Filesystems {
		fs = append(fs, fmt.Sprintf("%s %.0f%% of %s", f.Mount, f.UsedPct(), hoststat.Bytes(f.Size)))
	}
	if len(fs) > 0 {
		parts = append(parts, strings.Join(fs, ", "))
	}
	switch {
	case h.Undervoltage == nil:
	case *h.Undervoltage:
		parts = append(parts, "supply TOO LOW")
	case h.UndervoltageAt != nil:
		parts = append(parts, "supply low at "+h.UndervoltageAt.Local().Format("15:04:05"))
	default:
		parts = append(parts, "supply ok")
	}
	return strings.Join(parts, "; ")
}

// positionsLine says whether this node is on the ATAK map, and which nodes
// that left the mesh it keeps at their last known position. Empty for a
// node with nothing to say (no receiver, sharing on, nothing kept).
func positionsLine(p status.Positions, receiver bool) string {
	var parts []string
	switch {
	case p.Shared:
		s := "this node is on the ATAK map"
		if p.LastSentAgoMS >= 0 {
			s += fmt.Sprintf(" (last sent %s ago, %d reports)", time.Duration(p.LastSentAgoMS)*time.Millisecond/time.Second*time.Second, p.Sent)
		}
		if p.Device != "" {
			s += ", position from the device at " + p.Device
		}
		parts = append(parts, s)
	case p.Off != "" || receiver:
		parts = append(parts, "not on the ATAK map: "+p.Note)
	}
	if p.NMEADevices > 0 {
		parts = append(parts, fmt.Sprintf("external GPS for %d device(s) on this node (NMEA, UDP 4349)", p.NMEADevices))
	} else if strings.HasPrefix(p.NMEAHeld, "cannot send") {
		parts = append(parts, "external GPS: "+p.NMEAHeld)
	}
	if p.DeviceNote != "" {
		parts = append(parts, "device positions: "+p.DeviceNote)
	}
	if n := len(p.LastKnown); n > 0 {
		var names []string
		for _, k := range p.LastKnown {
			names = append(names, fmt.Sprintf("%s (gone %s)", k.Callsign, time.Since(k.Heard).Round(time.Minute)))
		}
		s := "last known: " + strings.Join(names, ", ")
		if p.Announcing {
			s += "; this node re-announces them"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "; ")
}
