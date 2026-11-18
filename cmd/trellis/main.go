// Command trellis is the single Trellis binary. It runs as meshd, meshctl,
// webd or crda depending on the first argument, or as meshd, meshctl or
// webd by the name it is invoked by (symlinks), as architecture §12.2
// describes.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"meridian/trellis/internal/api"
	"meridian/trellis/internal/config"
	"meridian/trellis/internal/meshctl"
	"meridian/trellis/internal/meshd"
	"meridian/trellis/internal/webd"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run(os.Args))
}

func run(argv []string) int {
	prog := filepath.Base(argv[0])
	args := argv[1:]
	var sub string
	switch prog {
	case "meshd", "meshctl", "webd":
		sub = prog
	default:
		if len(args) == 0 {
			usage()
			return 2
		}
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "meshd":
		return runMeshd(args)
	case "meshctl":
		return meshctl.Main(version, args, os.Stdout, os.Stderr)
	case "webd":
		return runWebd(args)
	case "crda":
		return runCRDA(args)
	case "version", "--version", "-v":
		fmt.Println("trellis", version)
		return 0
	case "help", "--help", "-h":
		usage()
		return 0
	}
	fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", sub)
	usage()
	return 2
}

func usage() {
	fmt.Fprintf(os.Stderr, `trellis %s — Meridian Trellis mesh node

Usage: trellis <subcommand> [flags]

  meshd     the privileged network daemon (run as root, under systemd)
  meshctl   operator CLI: status, radios, doctor, config validate, profile new, web
  webd      the web UI and network API (unprivileged, under systemd)
  crda      answer the kernel's regulatory request (run by udev; --print to show the answer)
  version

The binary may also be installed as symlinks named meshd, meshctl and webd.
`, version)
}

func runMeshd(args []string) int {
	fs := flag.NewFlagSet("meshd", flag.ContinueOnError)
	node := fs.String("node", config.DefaultNodePath, "node configuration file")
	profile := fs.String("profile", "", "mesh profile (default: the path in the node configuration)")
	stateDir := fs.String("state-dir", config.DefaultStateDir, "persistent state directory")
	runDir := fs.String("run-dir", config.DefaultRunDir, "runtime directory (sockets, generated configs)")
	socket := fs.String("socket", "", "API socket (default <run-dir>/meshd.sock)")
	logLevel := fs.String("log-level", "info", "debug, info, warn or error")
	logFormat := fs.String("log-format", "text", "text or json")
	interval := fs.Duration("interval", 15*time.Second, "slow reconcile interval")
	voiceSocket := fs.String("voice-socket", "", "trellis-voiced's control socket (default /run/trellis-voice/control.sock)")
	noHostConfig := fs.Bool("no-host-config", false, "don't write NetworkManager or systemd-networkd configuration on the host, or reload them (for labs, where meshd runs in a namespace of a host it doesn't own)")
	testPortManager := fs.String("test-port-manager", "", "lab only: replace NetworkManager and systemd-networkd with a stand-in driven by files in this directory")
	testUpgrader := fs.String("test-upgrader", "", "lab only: replace the upgrade a mesh-wide rollout makes with a stand-in driven by files in this directory")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "meshd is the daemon and takes no arguments (got %q).\n", strings.Join(fs.Args(), " "))
		fmt.Fprintln(os.Stderr, "For status use: sudo meshctl status. To restart the daemon: sudo systemctl restart trellis-meshd")
		return 2
	}
	log, err := newLogger(*logLevel, *logFormat)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	d, err := meshd.New(meshd.Options{
		Version: version, NodePath: *node, ProfilePath: *profile, StateDir: *stateDir, RunDir: *runDir,
		Socket: *socket, Log: log, Interval: *interval, NoHostConfig: *noHostConfig, VoiceSocket: *voiceSocket,
		TestPortManagerDir: *testPortManager, TestUpgraderDir: *testUpgrader,
	})
	if err != nil {
		log.Error("meshd cannot start", "err", err)
		return 1
	}
	sock := *socket
	if sock == "" {
		sock = filepath.Join(*runDir, "meshd.sock")
	}
	serve := func(ctx context.Context, d *meshd.Daemon) error {
		srv := &api.Server{Provider: d, Log: log.With("component", "api"), Socket: sock}
		return srv.Serve(ctx)
	}
	if err := d.Run(context.Background(), serve); err != nil {
		log.Error("meshd exited with error", "err", err)
		return 1
	}
	return 0
}

// newLogger builds the process logger and makes it the default.
func newLogger(logLevel, logFormat string) (*slog.Logger, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(strings.ToUpper(logLevel))); err != nil {
		return nil, fmt.Errorf("bad --log-level: %w", err)
	}
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if logFormat == "json" {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		// Under systemd the journal adds timestamps; keep lines compact.
		if os.Getenv("JOURNAL_STREAM") != "" {
			opts.ReplaceAttr = func(groups []string, a slog.Attr) slog.Attr {
				if a.Key == slog.TimeKey && len(groups) == 0 {
					return slog.Attr{}
				}
				return a
			}
		}
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	log := slog.New(h)
	slog.SetDefault(log)
	return log, nil
}

func runWebd(args []string) int {
	fs := flag.NewFlagSet("webd", flag.ContinueOnError)
	socket := fs.String("socket", "", "meshd's API socket (default "+filepath.Join(config.DefaultRunDir, "meshd.sock")+")")
	listen := fs.String("listen", ":443", "HTTPS address, when systemd passes no sockets")
	httpListen := fs.String("http-listen", ":80", "plain-HTTP address that redirects to HTTPS, when systemd passes no sockets (\"\" for none)")
	// systemd sets STATE_DIRECTORY for a unit with StateDirectory=.
	stateDir := fs.String("state-dir", os.Getenv("STATE_DIRECTORY"), "where the TLS certificate is kept (default: none, a new certificate at every start)")
	plain := fs.Bool("plain-http", false, "serve the UI without TLS on --listen (development only)")
	voiceConsole := fs.String("voice-console", "", "trellis-voiced's console socket (default /run/trellis-voice/console.sock)")
	logLevel := fs.String("log-level", "info", "debug, info, warn or error")
	logFormat := fs.String("log-format", "text", "text or json")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "webd takes no arguments (got %q).\n", strings.Join(fs.Args(), " "))
		return 2
	}
	log, err := newLogger(*logLevel, *logFormat)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	srv, err := webd.New(webd.Options{
		Version: version, Socket: *socket, Listen: *listen, HTTPListen: *httpListen, StateDir: *stateDir,
		PlainHTTP: *plain, VoiceConsole: *voiceConsole, Log: log.With("component", "webd"),
	})
	if err != nil {
		log.Error("webd cannot start", "err", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := srv.Run(ctx); err != nil {
		log.Error("webd exited with error", "err", err)
		return 1
	}
	return 0
}
