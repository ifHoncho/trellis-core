package main

import (
	"flag"
	"fmt"
	"log/syslog"
	"os"
	"strings"
	"time"

	"meridian/trellis/internal/config"
	"meridian/trellis/internal/nl80211"
	"meridian/trellis/internal/regdb"
)

// runCRDA answers one regulatory request from the kernel, the way CRDA did
// (architecture §5.1): udev runs it with COUNTRY=XX when the kernel has no
// regulatory.db to read. In the mesh profile's country mode it sends that
// country's rules from the distribution's database; in unrestricted mode,
// Trellis's permissive rules. It records what it sent for meshd.
func runCRDA(args []string) int {
	fs := flag.NewFlagSet("crda", flag.ContinueOnError)
	country := fs.String("country", os.Getenv("COUNTRY"), "the country the kernel asked for (udev passes COUNTRY)")
	mode := fs.String("mode", "", "country or unrestricted (default: the mesh profile's regulatory.mode)")
	nodePath := fs.String("node", config.DefaultNodePath, "node configuration, which names the mesh profile")
	db := fs.String("db", "", "regulatory.db to read in country mode (default: "+strings.Join(regdb.Paths, ", ")+")")
	print := fs.Bool("print", false, "print the answer instead of sending it")
	runDir := fs.String("state-dir", regdb.StateDir, "where to record what was sent")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: trellis crda [--country XX] [--mode country|unrestricted] [--print]\n\n"+
			"Answers the kernel's regulatory request (run by udev). Flags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	logf := logger()
	if *country == "" {
		logf("no country: run by udev with COUNTRY set, or give --country")
		return 2
	}
	why := ""
	if *mode == "" {
		*mode, why = profileMode(*nodePath)
	}
	paths := regdb.Paths
	if *db != "" {
		paths = []string{*db}
	}
	ans, err := regdb.Decide(*country, *mode, paths)
	if err != nil {
		logf("%s: %v", *country, err)
		return 1
	}
	if *print {
		fmt.Printf("country %s: %s mode, from %s", ans.Domain.Alpha2, ans.Mode, ans.Source)
		if ans.Note != "" {
			fmt.Printf(" (%s)", ans.Note)
		}
		fmt.Println()
		for _, r := range ans.Domain.Rules {
			fmt.Printf("\t(%d - %d @ %d), (%.0f dBm)%s\n", r.StartKHz/1000, r.EndKHz/1000, r.MaxBandwidthKHz/1000, float64(r.MaxEIRPMBm)/100, ruleFlags(r.Flags))
		}
		return 0
	}
	st := regdb.State{Alpha2: ans.Domain.Alpha2, Mode: ans.Mode, Source: ans.Source, Note: strings.TrimSpace(ans.Note + " " + why), Time: time.Now().UTC()}
	nl, err := nl80211.Dial()
	if err == nil {
		err = nl.SetRegdomain(ans.Domain)
		nl.Close()
	}
	if err != nil {
		st.Error = err.Error()
		logf("%s: the kernel refused the %s rules: %v", ans.Domain.Alpha2, ans.Mode, err)
	} else {
		logf("%s: sent the %s rules (%d, from %s)%s", ans.Domain.Alpha2, ans.Mode, len(ans.Domain.Rules), ans.Source, prefixed(st.Note))
	}
	// udev runs helpers in parallel, and at boot one answer may be refused
	// because the kernel's pending request is already another country's.
	// A refusal is recorded only while nothing has
	// succeeded yet, so it cannot hide a later success; the journal keeps
	// every one.
	record := true
	if err != nil {
		if prev, perr := regdb.ReadState(*runDir); perr == nil && prev.Error == "" {
			record = false
		}
	}
	if record {
		if werr := regdb.WriteState(*runDir, st); werr != nil {
			logf("cannot record the answer: %v", werr)
		}
	}
	if err != nil {
		return 1
	}
	return 0
}

// profileMode reads the mesh profile's regulatory mode. Without a profile
// (setup mode) or with one that does not load, it is country mode, and the
// second value says why.
func profileMode(nodePath string) (string, string) {
	profilePath := config.DefaultProfilePath
	if n, err := config.LoadNode(nodePath); err == nil && n.ProfilePath != "" {
		profilePath = n.ProfilePath
	}
	p, err := config.LoadProfile(profilePath)
	if err != nil {
		return regdb.ModeCountry, "(no usable mesh profile, so country mode: " + firstLine(err.Error()) + ")"
	}
	if p.Regulatory.Mode == config.RegModeUnrestricted {
		return regdb.ModeUnrestricted, ""
	}
	return regdb.ModeCountry, ""
}

func ruleFlags(f uint32) string {
	var out []string
	for _, x := range []struct {
		bit  uint32
		name string
	}{
		{nl80211.NL80211_RRF_NO_OFDM, "NO-OFDM"}, {nl80211.NL80211_RRF_NO_OUTDOOR, "NO-OUTDOOR"}, {nl80211.NL80211_RRF_DFS, "DFS"},
		{nl80211.NL80211_RRF_NO_IR, "PASSIVE-SCAN"}, {nl80211.NL80211_RRF_AUTO_BW, "AUTO-BW"},
	} {
		if f&x.bit != 0 {
			out = append(out, x.name)
		}
	}
	if len(out) == 0 {
		return ""
	}
	return ", " + strings.Join(out, ", ")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func prefixed(s string) string {
	if s == "" {
		return ""
	}
	return " " + s
}

// logger writes to syslog (the journal) when it can, since udev keeps a
// helper's output only at debug level, and always to stderr.
func logger() func(format string, args ...any) {
	w, err := syslog.New(syslog.LOG_DAEMON|syslog.LOG_INFO, "trellis-crda")
	return func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		fmt.Fprintln(os.Stderr, msg)
		if err == nil {
			w.Info(msg)
		}
	}
}
