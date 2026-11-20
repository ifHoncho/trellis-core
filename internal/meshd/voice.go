package meshd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"meridian/trellis/internal/api"
	"meridian/trellis/internal/config"
	"meridian/trellis/internal/reconcile"
	"meridian/trellis/internal/version"
	"meridian/trellis/internal/voice"
)

// Voice (architecture §13, Voice). trellis-voiced is a service of its own,
// unprivileged, holding the audio devices; meshd starts and stops it as
// the node config says, gives it its configuration (a file in meshd's run
// directory, voiceStateFile), and passes its status on.
//
// voiced takes packets from any device on the client network, so it gets
// nothing of meshd's API: it reads that one file and no socket of meshd's.

// beforeVoice reports whether ver is a release from before the profile
// had talk groups (0.3.0). A development build says nothing either way.
func beforeVoice(ver string) bool {
	v := version.Parse(ver)
	return v.Numbered && v.Exact && v.Compare(version.Parse("0.3.0")) < 0
}

// voiceUnit is trellis-voiced's systemd unit.
const voiceUnit = "trellis-voiced.service"

// voiceStateFile, in the run directory, is what trellis-voiced runs with:
// an api.VoiceConfig as JSON. It holds no secret. voiceGroup, when the
// node has it (the trellis package makes it), is the group that may read
// it: trellis-voiced's own.
const (
	voiceStateFile = "voice.json"
	voiceGroup     = "trellis-voice"
)

// writeVoiceState writes the configuration for trellis-voiced when it
// differs from what was last written (last), and returns what is there now.
func (d *Daemon) writeVoiceState(last []byte) ([]byte, error) {
	data, err := json.Marshal(d.VoiceConfig())
	if err != nil {
		return last, err
	}
	path := filepath.Join(d.opts.RunDir, voiceStateFile)
	if bytes.Equal(data, last) {
		if _, err := os.Stat(path); err == nil {
			return last, nil
		}
	}
	// Whole, and with its group, before it has its name: voiced never
	// reads half of it, or a copy it may not open.
	f, err := os.CreateTemp(d.opts.RunDir, voiceStateFile+".tmp")
	if err != nil {
		return last, err
	}
	tmp := f.Name()
	fail := func(err error) ([]byte, error) {
		f.Close()
		_ = os.Remove(tmp)
		return last, err
	}
	if _, err := f.Write(data); err != nil {
		return fail(err)
	}
	if err := f.Chmod(0o640); err != nil {
		return fail(err)
	}
	if g, err := user.LookupGroup(voiceGroup); err == nil {
		if gid, err := strconv.Atoi(g.Gid); err == nil {
			_ = f.Chown(-1, gid)
		}
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return last, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return last, err
	}
	return data, nil
}

// voiceConfig builds trellis-voiced's configuration from the profile's
// groups and the node's settings. problems names groups the node refers
// to that the profile lacks: those stay off.
func voiceConfig(p *config.Profile, n *config.Node) (voice.Config, []string) {
	vol := func(v *int, def int) int {
		if v == nil {
			return def
		}
		return *v
	}
	ear := func(e string) voice.Ear {
		if e == "" {
			return voice.Both
		}
		return voice.Ear(e)
	}
	c := voice.Config{
		Callsign: n.Callsign, Bitrate: p.Voice.Bitrate, Listen: []int{},
		Volume: vol(n.Voice.ListenVolume, 60), Tones: n.Voice.Tones == nil || *n.Voice.Tones,
		Device: n.Voice.Device, PTT: n.Voice.PTT, MicGain: n.Voice.MicGain, Sidetone: n.Voice.Sidetone,
		MicAGC: n.Voice.MicAGC == nil || *n.Voice.MicAGC, PTTMode: n.Voice.PTTMode,
	}
	have := map[int]bool{}
	for _, g := range p.Voice.Groups {
		ap, _ := g.VoiceAddr()
		c.Groups = append(c.Groups, voice.Group{Number: g.Number, Name: g.Name, Addr: ap, Priority: g.Priority})
		have[g.Number] = true
	}
	known := func(g int) int {
		if have[g] {
			return g
		}
		return 0
	}
	for i, ch := range []config.VoiceChannel{n.Voice.Channels.A, n.Voice.Channels.B} {
		c.Channels[i] = voice.Channel{Group: known(ch.Group), Ear: ear(ch.Ear), Volume: vol(ch.Volume, 80), Device: ch.Device, PTT: ch.PTT, Key: ch.Key}
	}
	// A group is listened to once: one that is a channel's, or is named
	// twice, would otherwise be mixed twice, and into both ears.
	taken := map[int]bool{c.Channels[0].Group: true, c.Channels[1].Group: true}
	for _, g := range n.Voice.Listen {
		if have[g] && !taken[g] {
			c.Listen = append(c.Listen, g)
			taken[g] = true
		}
	}
	return c, config.VoiceMissingGroups(p, n)
}

// VoiceConfig is what trellis-voiced runs with.
func (d *Daemon) VoiceConfig() api.VoiceConfig {
	n, p := d.conf()
	c, problems := voiceConfig(p, n)
	raw, _ := json.Marshal(c)
	return api.VoiceConfig{Enable: n.Voice.Enable, Config: raw, Problems: problems}
}

// Voice is trellis-voiced's state, asked of it over its control socket.
func (d *Daemon) Voice(ctx context.Context) (*api.VoiceState, error) {
	n, _ := d.conf()
	st := &api.VoiceState{Enabled: n.Voice.Enable, Installed: d.voiceInstalled()}
	if !n.Voice.Enable {
		return st, nil
	}
	if !st.Installed {
		st.Error = voiceNotInstalled
		return st, nil
	}
	if m := d.voiceLibsMessage(); m != "" {
		st.Error = m
		return st, nil
	}
	b, answered := d.voiceStatus(ctx, time.Second)
	switch {
	case !answered:
		// A time of its own: the request may have used all of its own up.
		dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		st.Error = d.voiceDown(dctx)
		return st, nil
	case b == nil:
		st.Error = "trellis-voiced gave no status"
		return st, nil
	}
	st.Running, st.Status = true, b
	return st, nil
}

// voiceStatus asks trellis-voiced's control socket for its status, within
// timeout: the JSON, nil when what it gave is not a status; answered is
// false when it did not answer at all.
func (d *Daemon) voiceStatus(ctx context.Context, timeout time.Duration) (status []byte, answered bool) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://voice/status", nil)
	resp, err := d.voiceClient().Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil || resp.StatusCode != http.StatusOK || !json.Valid(b) {
		return nil, true
	}
	return b, true
}

// VoiceTest asks trellis-voiced for a headset check: a test tone in a
// channel's ear, the microphone played back for 10 s, or stop.
func (d *Daemon) VoiceTest(ctx context.Context, in api.VoiceTestInput) error {
	if n, _ := d.conf(); !n.Voice.Enable {
		return api.Errorf(409, "voice is off on this node")
	}
	body, _ := json.Marshal(in)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://voice/test", bytes.NewReader(body))
	resp, err := d.voiceClient().Do(req)
	if err != nil {
		return api.Errorf(503, "trellis-voiced is not answering: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return api.Errorf(resp.StatusCode, "%s", strings.TrimSpace(string(b)))
	}
	return nil
}

// voiceNotInstalled is what voice on a node whose package has none says:
// the trellis package carries voice on arm64 and amd64.
const voiceNotInstalled = "voice is on, but Trellis on this node has no voice: it is built for arm64 and amd64 only"

// voiceLibs are the libraries trellis-voiced loads at run time. The
// package recommends them rather than requiring them: meshctl upgrade
// installs with dpkg alone, often on a node with no network.
var voiceLibs = []struct{ file, pkg string }{
	{"libopus.so.0", "libopus0"},
	{"libasound.so.2", "libasound2t64 (or libasound2)"},
}

// voiceLibDirs are where the libraries are looked for.
var voiceLibDirs = []string{"/usr/lib/*", "/usr/lib", "/lib/*", "/lib", "/usr/lib64", "/usr/local/lib"}

// voiceLibsMissing names the packages of the libraries voice needs that
// are not on this node.
func voiceLibsMissing() []string {
	var out []string
	for _, l := range voiceLibs {
		found := false
		for _, d := range voiceLibDirs {
			if m, _ := filepath.Glob(filepath.Join(d, l.file)); len(m) > 0 {
				found = true
				break
			}
		}
		if !found {
			out = append(out, l.pkg)
		}
	}
	return out
}

// voiceLibsMessage says what to install, or "".
func (d *Daemon) voiceLibsMessage() string {
	if d.opts.NoHostConfig {
		return ""
	}
	if m := voiceLibsMissing(); len(m) > 0 {
		return "voice is on, but the libraries it loads are missing: install " + strings.Join(m, " and ") + " (sudo apt install libopus0 libasound2t64, or libasound2 on older releases; trellis-deploy installs them when the node has network)"
	}
	return ""
}

// voiceUnitFile is where the trellis package puts voice's unit.
var voiceUnitFile = "/usr/lib/systemd/system/" + voiceUnit

// voiceInstalled reports whether trellis-voiced is here: the package's
// unit, or, in a lab (NoHostConfig), a voiced someone runs by hand.
func (d *Daemon) voiceInstalled() bool {
	if d.opts.NoHostConfig {
		return true
	}
	_, err := os.Stat(voiceUnitFile)
	return err == nil
}

// voiceDown says why trellis-voiced does not answer, from systemd.
func (d *Daemon) voiceDown(ctx context.Context) string {
	if d.opts.NoHostConfig {
		return "trellis-voiced is not answering on " + d.voiceSocket()
	}
	out, _ := exec.CommandContext(ctx, "systemctl", "is-active", voiceUnit).Output()
	switch state := strings.TrimSpace(string(out)); state {
	case "activating", "active":
		return "trellis-voiced is starting"
	case "failed":
		return "trellis-voiced failed: see sudo journalctl -u trellis-voiced"
	case "":
		return "trellis-voiced is not answering"
	default:
		return "trellis-voiced is " + state + ": meshd starts it with voice.enable; see sudo journalctl -u trellis-voiced"
	}
}

// voiceClient is the one client meshd asks trellis-voiced with. Without
// keep-alives: a client made per request with idle connections kept open
// leaked a socket and two goroutines on each side every reconcile pass
// (hundreds within hours).
func (d *Daemon) voiceClient() *http.Client {
	d.voiceHTTPOnce.Do(func() {
		d.voiceHTTP = &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", d.voiceSocket())
			},
			DisableKeepAlives: true,
		}}
	})
	return d.voiceHTTP
}

func (d *Daemon) voiceSocket() string {
	if d.opts.VoiceSocket != "" {
		return d.opts.VoiceSocket
	}
	return "/run/trellis-voice/control.sock"
}

// voiceReconciler starts and stops trellis-voiced with voice.enable. Labs
// (NoHostConfig) run it by hand.
type voiceReconciler struct {
	d     *Daemon
	want  *bool  // what was last asked of systemd
	state []byte // the configuration last written for voiced
}

func (v *voiceReconciler) Name() string { return "voice" }

func (v *voiceReconciler) Reconcile(ctx context.Context) (reconcile.Result, error) {
	var res reconcile.Result
	n, p := v.d.conf()
	// The configuration first: voiced reads it as it starts.
	state, err := v.d.writeVoiceState(v.state)
	if err != nil {
		res.Warnings = append(res.Warnings, reconcile.Warning{Level: "warning", Code: "voice-config-not-written",
			Message: "Voice service configuration write failed: " + err.Error()})
	}
	v.state = state
	for _, m := range config.VoiceMissingGroups(p, n) {
		res.Warnings = append(res.Warnings, reconcile.Warning{Level: "warning", Code: "voice-group-missing",
			Message: m + ": the profile has no such talk group; that channel stays off"})
	}
	// What voiced found: a headset-button PTT in hold to talk, or a talk
	// stopped for a dead microphone.
	if n.Voice.Enable {
		res.Warnings = append(res.Warnings, v.d.voiceWarnings(ctx)...)
	}
	if v.d.opts.NoHostConfig {
		return res, nil
	}
	want := n.Voice.Enable
	if v.want != nil && *v.want == want {
		return res, nil
	}
	verb := "stop"
	if want {
		verb = "start"
	}
	if want && !v.d.voiceInstalled() {
		res.Warnings = append(res.Warnings, reconcile.Warning{Level: "warning", Code: "voice-not-installed", Message: voiceNotInstalled})
		return res, nil
	}
	if m := v.d.voiceLibsMessage(); want && m != "" {
		// Checked again every pass: started once they are installed.
		res.Warnings = append(res.Warnings, reconcile.Warning{Level: "warning", Code: "voice-libraries", Message: m})
		return res, nil
	}
	if err := exec.CommandContext(ctx, "systemctl", verb, "--no-block", voiceUnit).Run(); err != nil {
		if want {
			// Tried again next pass, with the warning shown meanwhile.
			res.Warnings = append(res.Warnings, reconcile.Warning{Level: "warning", Code: "voice-not-started",
				Message: "Voice enabled but trellis-voiced failed to start: " + err.Error()})
			return res, nil
		}
		// Not installed, or not running: either way it is stopped.
		v.want = &want
		return res, nil
	}
	v.want = &want
	res.Changed = true
	v.d.log.Info("voice service "+verb+"ed", "unit", voiceUnit)
	return res, nil
}

func (v *voiceReconciler) Teardown(context.Context) error { return nil }

// voiceWarnings turns voiced's state into alerts.
func (d *Daemon) voiceWarnings(ctx context.Context) []reconcile.Warning {
	// Asked every pass, so briefly: a voiced that hangs must not hold the
	// reconcile loop.
	if !d.voiceInstalled() {
		return nil
	}
	b, _ := d.voiceStatus(ctx, 300*time.Millisecond)
	var vs voice.Status
	if b == nil || json.Unmarshal(b, &vs) != nil {
		return nil
	}
	var out []reconcile.Warning
	// The package restarts a running voice service when it is upgraded;
	// one that was not (systemd away, a failed restart) runs the old binary.
	if vs.Version != "" && vs.Version != d.opts.Version {
		out = append(out, reconcile.Warning{Level: "warning", Code: "voice-version",
			Message: "Voice service version " + vs.Version + ", Trellis " + d.opts.Version + ". Restart: sudo systemctl restart trellis-voiced"})
	}
	if vs.Audio != nil {
		for _, ch := range vs.Audio.ButtonHold {
			out = append(out, reconcile.Warning{Level: "warning", Code: "voice-ptt-cuts-mic",
				Message: "voice channel " + strings.ToUpper(ch) + ": PTT is the adapter headset button (mutes mic while held) with hold to talk: transmits silence. Use press to start, press to stop, or VOX (Voice page)"})
		}
	}
	if vs.Audio != nil && vs.Audio.NoMic {
		out = append(out, reconcile.Warning{Level: "warning", Code: "voice-no-mic",
			Message: "Voice: audio device (" + vs.Audio.Device + "): no microphone, receive only. Unsupported adapters (not OpenVLM or CM108) may lack features"})
	}
	if vs.Audio != nil && vs.Audio.NoPTT {
		out = append(out, reconcile.Warning{Level: "info", Code: "voice-no-ptt",
			Message: "Voice: audio device (" + vs.Audio.Device + "): no PTT, receive only. Unsupported adapters (not OpenVLM or CM108) may lack features"})
	}
	for _, c := range vs.Channels {
		if c.MicCut {
			out = append(out, reconcile.Warning{Level: "warning", Code: "voice-mic-cut",
				Message: "voice channel " + strings.ToUpper(c.Name) + ": transmission stopped, no mic audio while PTT held. PTT likely mutes the mic (phone-style adapter): use press to start, press to stop, or VOX, or run PTT test (Voice page)"})
		}
	}
	return out
}
