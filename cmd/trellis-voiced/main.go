// Command trellis-voiced is Trellis's push-to-talk voice (architecture
// §13, Voice): talk groups as Opus over multicast RTP on the mesh's client
// network. It is a binary of its own because it loads libopus, which links
// it dynamically; trellis stays static.
//
//	trellis-voiced run [--state voice.json | --config voice.json] [--iface br-mesh] [--control sock]
//	                   [--capture tone:440|silence|file.wav] [--playback out.wav|none]
//	trellis-voiced ptt --control sock a|b down|up
//	trellis-voiced status --control sock
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"meridian/trellis/internal/api"
	"meridian/trellis/internal/opus"
	"meridian/trellis/internal/voice"
	"meridian/trellis/internal/voice/alsa"
	"meridian/trellis/internal/voice/devices"
	"meridian/trellis/internal/voice/vopus"
	"meridian/trellis/internal/voice/wav"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = run(os.Args[2:])
	case "ptt":
		err = ptt(os.Args[2:])
	case "status":
		err = status(os.Args[2:])
	case "version", "--version":
		// The libraries it loads, too: what an install must have brought.
		codec, err1 := opus.Version()
		err2 := alsa.Load()
		fmt.Printf("trellis-voiced %s, %s, libasound %s\n", version, or(codec, "libopus missing"), map[bool]string{true: "loaded", false: "missing"}[err2 == nil])
		if err = errors.Join(err1, err2); err != nil {
			err = fmt.Errorf("libraries: %w", err)
		}
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "trellis-voiced:", err)
		os.Exit(1)
	}
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: trellis-voiced run [--state voice.json | --config voice.json] [--iface br-mesh] [--control sock] [--capture tone:HZ|silence|file.wav] [--playback file.wav|none]
       trellis-voiced ptt --control sock a|b down|up
       trellis-voiced status --control sock`)
}

const (
	defaultControl = "/run/trellis-voice/control.sock"
	defaultConsole = "/run/trellis-voice/console.sock"
	// defaultState is the configuration meshd keeps for voiced: a file,
	// so that voiced, which takes packets from any device on the mesh,
	// needs nothing of meshd's API.
	defaultState = "/run/trellis/voice.json"
)

// runOpts are run's flags.
type runOpts struct {
	cfgPath, state, iface, capture, playback, sys string
	files                                         bool
	log                                           *slog.Logger
	// What was last logged, so that a condition that stays is said once:
	// the configuration's problems, why there is none, why the engine
	// stopped.
	saidProblems, saidNoConfig, saidStopped string
}

func run(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var o runOpts
	fs.StringVar(&o.cfgPath, "config", "", "voice configuration file (JSON), read once; default: follow meshd's (--state)")
	fs.StringVar(&o.state, "state", defaultState, "the configuration meshd keeps for voiced, followed as it changes")
	fs.StringVar(&o.iface, "iface", "br-mesh", "interface of the mesh's client network")
	control := fs.String("control", defaultControl, "control socket")
	consoleSock := fs.String("console", defaultConsole, "socket webd passes browser consoles to")
	fs.StringVar(&o.capture, "capture", "", "file audio in, instead of the device: tone:HZ, silence or a mono 48 kHz WAV file (labs)")
	fs.StringVar(&o.playback, "playback", "", "file audio out, instead of the device: a WAV file to record, or none (labs)")
	fs.StringVar(&o.sys, "sys", "/sys", "where sysfs is (tests)")
	level := fs.String("log-level", "info", "debug, info, warn or error")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(*level)); err != nil {
		return err
	}
	o.log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv}))
	v, err := opus.Version()
	if err != nil {
		return err
	}
	o.log.Info("voice starting", "version", version, "codec", v, "iface", o.iface)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	var cur atomic.Pointer[voice.Engine]
	srv, err := serveControl(*control, &cur)
	if err != nil {
		return err
	}
	defer srv.Close()
	cons, err := serveConsoles(*consoleSock, &cur, o.log)
	if err != nil {
		return err
	}
	defer cons.Close()

	o.files = o.capture != "" || o.playback != ""
	var out voice.Playback = wav.Discard{}
	if o.playback != "" && o.playback != "none" {
		rec, err := wav.NewRecorder(o.playback)
		if err != nil {
			return err
		}
		defer rec.Close()
		out = rec
	}
	// The configuration: from a file, once; or from meshd, followed. A
	// change restarts the engine.
	for ctx.Err() == nil {
		raw, enable, err := o.config()
		if err != nil || !enable {
			if why := fmt.Sprint(err); err != nil && why != o.saidNoConfig {
				o.saidNoConfig = why
				o.log.Warn("no voice configuration yet", "err", err)
			}
			if !sleep(ctx, 3*time.Second) {
				break
			}
			continue
		}
		o.saidNoConfig = ""
		o.cycle(ctx, raw, out, &cur)
	}
	return nil
}

// cycle runs the engine with one configuration until it changes (polled
// every 3 s), the engine fails, or ctx ends.
func (o *runOpts) cycle(ctx context.Context, raw []byte, out voice.Playback, cur *atomic.Pointer[voice.Engine]) {
	engCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// The audio hardware as this engine starts: a change (an adapter
	// plugged in, unplugged, replugged) restarts the engine, which looks
	// again (hot-plug).
	lines := pttLines(raw)
	hw, ifx := o.hardware(lines), o.ifindex()
	done := make(chan error, 1)
	go func() { done <- o.engine(engCtx, raw, out, cur) }()
	for i := 0; ; i++ {
		select {
		case err := <-done:
			if err != nil && ctx.Err() == nil {
				// Said once while it stays the same (the mesh interface
				// missing while meshd is stopped): tried every 2 s.
				if why := err.Error(); why != o.saidStopped {
					o.saidStopped = why
					o.log.Error("voice stopped; trying again every 2 s", "err", err)
				}
				sleep(ctx, 2*time.Second)
			}
			return
		case <-ctx.Done():
			<-done
			return
		case <-time.After(time.Second):
		}
		// meshd recreates br-mesh when it restarts: sockets bound to the
		// old one hear nothing, so the engine starts again on the new one.
		if now := o.ifindex(); now != ifx {
			o.log.Info("voice: the mesh interface changed; restarting the engine", "iface", o.iface, "was", ifx, "now", now)
			cancel()
			<-done
			return
		}
		if now := o.hardware(lines); now != hw {
			o.log.Info("voice: the audio hardware changed; restarting the engine", "was", hw, "now", now)
			cancel()
			<-done
			return
		}
		if o.cfgPath != "" || i%3 != 2 {
			continue // the configuration every 3 s
		}
		if now, en, err := o.config(); err == nil && (!en || !bytes.Equal(now, raw)) {
			o.log.Info("voice configuration changed; restarting the engine")
			cancel()
			<-done
			return
		}
	}
}

// ifindex is the mesh interface's index now, 0 when it is missing.
func (o *runOpts) ifindex() int {
	ifi, err := net.InterfaceByName(o.iface)
	if err != nil {
		return 0
	}
	return ifi.Index
}

// hardware is a fingerprint of the audio hardware: each card's identity
// and what it offers (capture, playback, a CM108 HID node, input
// devices), and whether each serial port or GPIO chip the configuration
// names for a PTT or a radio key is there (a USB serial PTT cable plugged
// back in is opened again). Empty in file mode.
func (o *runOpts) hardware(lines []string) string {
	if o.files {
		return ""
	}
	devs, err := devices.Scan(o.sys)
	if err != nil {
		return "scan: " + err.Error()
	}
	var b strings.Builder
	for _, d := range devs {
		fmt.Fprintf(&b, "%s c%v p%v h%s i%s; ", d.ID, d.Capture, d.Playback, d.HIDRaw, strings.Join(d.Inputs, ","))
	}
	for _, dev := range lines {
		_, err := os.Stat(dev)
		fmt.Fprintf(&b, "%s %v; ", dev, err == nil)
	}
	return b.String()
}

// pttLines are the device files a configuration's PTT and radio key lines
// are on (serial:<device>:<line>, gpio:<chip>:<line>).
func pttLines(raw []byte) []string {
	var cfg voice.Config
	if json.Unmarshal(raw, &cfg) != nil {
		return nil
	}
	var out []string
	for _, spec := range []string{cfg.PTT, cfg.Channels[0].PTT, cfg.Channels[1].PTT, cfg.Channels[0].Key, cfg.Channels[1].Key} {
		kind, rest, _ := strings.Cut(spec, ":")
		i := strings.LastIndex(rest, ":")
		if (kind != "serial" && kind != "gpio") || i <= 0 {
			continue
		}
		dev := rest[:i]
		if !strings.HasPrefix(dev, "/") {
			dev = "/dev/" + dev
		}
		out = append(out, dev)
	}
	return out
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// config reads the configuration: the file given, or the one meshd keeps
// (an api.VoiceConfig: whether voice is on, the engine's configuration,
// and what meshd found wrong with the node's settings).
func (o *runOpts) config() ([]byte, bool, error) {
	if o.cfgPath != "" {
		raw, err := os.ReadFile(o.cfgPath)
		return raw, true, err
	}
	raw, err := os.ReadFile(o.state)
	if err != nil {
		return nil, false, fmt.Errorf("meshd's voice configuration: %w", err)
	}
	var vc api.VoiceConfig
	if err := json.Unmarshal(raw, &vc); err != nil {
		return nil, false, fmt.Errorf("%s: %w", o.state, err)
	}
	if said := strings.Join(vc.Problems, "; "); said != o.saidProblems {
		o.saidProblems = said
		for _, p := range vc.Problems {
			o.log.Warn("voice configuration", "problem", p)
		}
	}
	return vc.Config, vc.Enable, nil
}

// engine runs one engine with one configuration until ctx ends or it fails.
func (o *runOpts) engine(ctx context.Context, raw []byte, out voice.Playback, cur *atomic.Pointer[voice.Engine]) error {
	var cfg voice.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	var a *audio
	if o.files {
		a = &audio{out: [2]voice.Playback{out}, what: "files", info: voice.AudioInfo{Device: "files", PTT: "none"}}
		switch {
		case o.capture == "" || o.capture == "silence":
			a.in[0] = wav.SilenceCapture()
		case strings.HasPrefix(o.capture, "tone:"):
			hz, err := strconv.ParseFloat(strings.TrimPrefix(o.capture, "tone:"), 64)
			if err != nil {
				return fmt.Errorf("--capture: %w", err)
			}
			a.in[0] = wav.ToneCapture(hz, 1)
		default:
			var err error
			if a.in[0], err = wav.FileCapture(o.capture); err != nil {
				return err
			}
		}
	} else {
		var err error
		if a, err = openAudio(o.sys, cfg, o.log); err != nil {
			return fmt.Errorf("audio: %w", err)
		}
		defer a.close()
	}
	udp, err := voice.OpenUDP(o.iface)
	if err != nil {
		return err
	}
	defer udp.Close()
	eng, err := voice.New(cfg, vopus.Codec{LossPerc: voice.InitialLossPerc}, udp, o.log)
	if err != nil {
		return err
	}
	defer eng.Close()
	if err := udp.Listen(eng.Listening(), eng.Deliver); err != nil {
		return err
	}
	eng.SetJoiner(udp)
	eng.SetAudio(a.info)
	eng.SetCutWatch(a.cutWatch)
	cur.Store(eng)
	defer cur.Store(nil)
	o.saidStopped = ""
	o.log.Info("voice running", "audio", a.what, "ptt", a.info.PTT, "groups", len(cfg.Groups), "a", cfg.Channels[0].Group, "b", cfg.Channels[1].Group, "listen", cfg.Listen)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pttErr := make(chan error, 1)
	pttDone := make(chan struct{})
	go func() { defer close(pttDone); watchPTT(ctx, a, eng, o.log, pttErr) }()
	runErr := make(chan error, 1)
	go func() { runErr <- eng.RunDevices(ctx, a.in, a.out) }()
	select {
	case err = <-runErr:
	case err = <-pttErr:
		cancel()
		<-runErr
	}
	// The PTT loop has left before the engine and the devices close: no
	// press reaches a closed engine, no radio key is set after its closer.
	cancel()
	<-pttDone
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func serveControl(path string, cur *atomic.Pointer[voice.Engine]) (*http.Server, error) {
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o660); err != nil {
		ln.Close()
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		eng := cur.Load()
		if eng == nil {
			http.Error(w, "voice is not running", http.StatusServiceUnavailable)
			return
		}
		st := eng.Status()
		st.Version = version
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(st)
	})
	mux.HandleFunc("POST /ptt", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Channel string `json:"channel"`
			Down    bool   `json:"down"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ch := strings.Index("ab", req.Channel)
		if len(req.Channel) != 1 || ch < 0 {
			http.Error(w, "channel is a or b", http.StatusBadRequest)
			return
		}
		eng := cur.Load()
		if eng == nil {
			http.Error(w, "voice is not running", http.StatusServiceUnavailable)
			return
		}
		granted, err := eng.SetPTT(ch, req.Down)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"granted": granted})
	})
	mux.HandleFunc("POST /test", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Kind    string `json:"kind"` // tone, loopback, stop
			Channel string `json:"channel"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		eng := cur.Load()
		if eng == nil {
			http.Error(w, "voice is not running", http.StatusServiceUnavailable)
			return
		}
		switch req.Kind {
		case "tone":
			if len(req.Channel) != 1 || eng.TestTone(strings.Index("ab", req.Channel)) != nil {
				http.Error(w, "channel is a or b", http.StatusBadRequest)
				return
			}
		case "ptt":
			if len(req.Channel) != 1 || eng.StartCheck(strings.Index("ab", req.Channel)) != nil {
				http.Error(w, "channel is a or b, and not talking", http.StatusBadRequest)
				return
			}
		case "loopback":
			eng.Loopback(10 * time.Second)
		case "stop":
			eng.Loopback(0)
		default:
			http.Error(w, "kind is tone, ptt, loopback or stop", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	go srv.Serve(ln)
	return srv, nil
}

func client(path string) *http.Client {
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		}}}
}

func ptt(args []string) error {
	fs := flag.NewFlagSet("ptt", flag.ContinueOnError)
	control := fs.String("control", defaultControl, "control socket")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 || (fs.Arg(1) != "down" && fs.Arg(1) != "up") {
		return errors.New("usage: ptt a|b down|up")
	}
	body, _ := json.Marshal(map[string]any{"channel": fs.Arg(0), "down": fs.Arg(1) == "down"})
	resp, err := client(*control).Post("http://voice/ptt", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s", strings.TrimSpace(string(b)))
	}
	os.Stdout.Write(b)
	return nil
}

func status(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	control := fs.String("control", defaultControl, "control socket")
	if err := fs.Parse(args); err != nil {
		return err
	}
	resp, err := client(*control).Get("http://voice/status")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.Copy(os.Stdout, resp.Body)
	return err
}
