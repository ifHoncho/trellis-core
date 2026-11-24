package main

import (
	"flag"
	"fmt"
	"log/syslog"
	"os"

	"meridian/trellis/internal/firstboot"
)

// runFirstboot applies the files put on the card at flash time, and makes
// an image's node its own (internal/firstboot). trellis-firstboot.service
// runs it at every boot, before meshd.
func runFirstboot(args []string) int {
	fs := flag.NewFlagSet("firstboot", flag.ContinueOnError)
	card := fs.String("card", firstboot.DefaultCardDir, "the card's folder of files put on it at flash time")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: trellis firstboot [--card DIR]\n\n"+
			"Installs the mesh profile, node configuration, admin credential and Morse\n"+
			"firmware put on the card at flash time, and gives an image's node its own\n"+
			"hostname. Run at boot by trellis-firstboot.service. Flags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "trellis firstboot: run as root")
		return 1
	}
	w, serr := syslog.New(syslog.LOG_DAEMON|syslog.LOG_INFO, "trellis-firstboot")
	logf := func(format string, a ...any) {
		msg := fmt.Sprintf(format, a...)
		fmt.Fprintln(os.Stderr, msg)
		if serr == nil {
			w.Info(msg)
		}
	}
	o := firstboot.System(logf)
	o.CardDir = *card
	res, err := firstboot.Run(o)
	if err != nil {
		logf("error: %v", err)
		return 1
	}
	if len(res.Rejected) > 0 {
		// Left on the card and reported there; meshd runs on what it has.
		return 1
	}
	return 0
}
