package meshd

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"meridian/trellis/internal/api"
	"meridian/trellis/internal/config"
	"meridian/trellis/internal/status"
	"meridian/trellis/internal/voice"
)

func TestVoiceConfig(t *testing.T) {
	p := &config.Profile{Voice: config.VoicePlan{Groups: []config.VoiceGroup{{Number: 1, Name: "Command"}, {Number: 2, Name: "Alpha", Address: "239.1.2.3", Port: 5000}}}}
	ninety := 90
	n := &config.Node{Callsign: "relay-7", Voice: config.VoiceNode{Enable: true, PTT: "cm108:3",
		Channels: config.VoiceChannels{A: config.VoiceChannel{Group: 1, Ear: "left", Volume: &ninety}, B: config.VoiceChannel{Group: 5}},
		Listen:   []int{2, 9}}}
	c, problems := voiceConfig(p, n)
	if len(problems) != 2 {
		t.Errorf("problems: %v", problems)
	}
	if c.Callsign != "relay-7" || c.PTT != "cm108:3" || !c.Tones || c.Volume != 60 {
		t.Errorf("config: %+v", c)
	}
	if ch := c.Channels[0]; ch.Group != 1 || ch.Ear != voice.Left || ch.Volume != 90 {
		t.Errorf("channel a: %+v", ch)
	}
	if ch := c.Channels[1]; ch.Group != 0 || ch.Ear != voice.Both || ch.Volume != 80 {
		t.Errorf("channel b (group 5 missing): %+v", ch)
	}
	if len(c.Listen) != 1 || c.Listen[0] != 2 || c.Groups[1].Addr.String() != "239.1.2.3:5000" {
		t.Errorf("listen %v, groups %+v", c.Listen, c.Groups)
	}
	// What meshd sends, voiced reads and accepts.
	b, _ := json.Marshal(c)
	var back voice.Config
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	back.Defaults()
	if err := back.Validate(); err != nil {
		t.Errorf("voiced would refuse it: %v", err)
	}
}

// Voice on where the package carries none (armhf) says so.
func TestVoiceNotInstalled(t *testing.T) {
	d, _ := testDaemon(t, map[string]string{"node.yaml": "schema: 0\ncallsign: node-1\nvoice: {enable: true}\n", "profile.yaml": testProfile})
	d.opts.NoHostConfig = false
	was := voiceUnitFile
	voiceUnitFile = filepath.Join(t.TempDir(), "none.service")
	t.Cleanup(func() { voiceUnitFile = was })
	st, err := d.Voice(context.Background())
	if err != nil || !st.Enabled || st.Installed || st.Running || !strings.Contains(st.Error, "arm64 and amd64 only") {
		t.Errorf("%+v %v", st, err)
	}
}

// Choosing the headset button as PTT with hold to talk is flagged in the
// change's review; with press to start, it is not.
func TestVoiceButtonHoldEffect(t *testing.T) {
	d, _ := testDaemon(t, map[string]string{"node.yaml": "schema: 0\ncallsign: node-1\n", "profile.yaml": testProfile})
	cur, p := d.conf()
	n := *cur
	n.Voice = config.VoiceNode{Enable: true, PTT: "button"}
	has := func(es []status.Warning) bool {
		for _, e := range es {
			if e.Code == "voice-ptt-cuts-mic" {
				return true
			}
		}
		return false
	}
	if !has(d.nodeEffects(cur, &n, p, false)) {
		t.Error("button with hold to talk not flagged")
	}
	n.Voice.PTTMode = "toggle"
	if has(d.nodeEffects(cur, &n, p, false)) {
		t.Error("button with press to start flagged")
	}
}

// Asking voiced for its state, again and again (every reconcile pass),
// leaves no connection open behind (it once leaked one a pass).
func TestVoiceNoLeak(t *testing.T) {
	d, dir := testDaemon(t, map[string]string{"node.yaml": "schema: 0\ncallsign: node-1\nvoice: {enable: true}\n", "profile.yaml": testProfile})
	sock := filepath.Join(dir, "v.sock")
	d.opts.VoiceSocket = sock
	d.opts.NoHostConfig = true // as in a lab: voiced counts as installed
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	open := 0
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("{}")) }),
		ConnState: func(_ net.Conn, s http.ConnState) {
			mu.Lock()
			defer mu.Unlock()
			switch s {
			case http.StateNew:
				open++
			case http.StateClosed, http.StateHijacked:
				open--
			}
		}}
	go srv.Serve(ln)
	defer srv.Close()
	for i := 0; i < 50; i++ {
		if st, err := d.Voice(context.Background()); err != nil || !st.Running {
			t.Fatalf("%+v %v", st, err)
		}
	}
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if open != 0 {
		t.Errorf("%d connections left open after 50 requests", open)
	}
}

// A group on a channel, or named twice, is listened to once.
func TestVoiceConfigListenOnce(t *testing.T) {
	p := &config.Profile{Voice: config.VoicePlan{Groups: []config.VoiceGroup{{Number: 1, Name: "Command"}, {Number: 2, Name: "Alpha"}, {Number: 3, Name: "Bravo"}}}}
	n := &config.Node{Callsign: "relay-7", Voice: config.VoiceNode{Enable: true,
		Channels: config.VoiceChannels{A: config.VoiceChannel{Group: 2}}, Listen: []int{2, 3, 3, 1}}}
	c, _ := voiceConfig(p, n)
	if len(c.Listen) != 2 || c.Listen[0] != 3 || c.Listen[1] != 1 {
		t.Errorf("listen %v, want [3 1]", c.Listen)
	}
}

// meshd keeps voiced's configuration in a file in its run directory:
// written when it changes, and again when the file has gone.
func TestVoiceStateFile(t *testing.T) {
	d, dir := testDaemon(t, map[string]string{"node.yaml": "schema: 0\ncallsign: node-1\nvoice: {enable: true}\n", "profile.yaml": testProfile})
	if err := os.MkdirAll(d.opts.RunDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "run", voiceStateFile)
	state, err := d.writeVoiceState(nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var vc api.VoiceConfig
	if err := json.Unmarshal(raw, &vc); err != nil || !vc.Enable {
		t.Fatalf("%s: %v", raw, err)
	}
	var c voice.Config
	if err := json.Unmarshal(vc.Config, &c); err != nil || c.Callsign != "node-1" {
		t.Fatalf("config %s: %v", vc.Config, err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	// Unchanged: not written again.
	old := time.Now().Add(-time.Hour)
	os.Chtimes(path, old, old)
	if _, err := d.writeVoiceState(state); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); !fi.ModTime().Equal(old) {
		t.Error("an unchanged configuration was written again")
	}
	// Gone: written again.
	os.Remove(path)
	if _, err := d.writeVoiceState(state); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Error("a missing file was not written again")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "run", "*.tmp*")); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
}

func TestBeforeVoice(t *testing.T) {
	for ver, want := range map[string]bool{"0.2.1": true, "0.1.0": true, "0.3.0": false, "0.4.2": false, "0.2.1+1115.102741": false, "0.2.1-dirty+1117.061122": false, "dev": false, "": false} {
		if beforeVoice(ver) != want {
			t.Errorf("%q: %v", ver, !want)
		}
	}
}

// Voice's libraries are looked for where Debian puts them; the missing
// ones are named by package.
func TestVoiceLibsMissing(t *testing.T) {
	was := voiceLibDirs
	t.Cleanup(func() { voiceLibDirs = was })
	root := t.TempDir()
	voiceLibDirs = []string{filepath.Join(root, "usr/lib/*")}
	if m := voiceLibsMissing(); len(m) != 2 {
		t.Fatalf("none there: %v", m)
	}
	dir := filepath.Join(root, "usr/lib/aarch64-linux-gnu")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "libopus.so.0"), nil, 0o644)
	if m := voiceLibsMissing(); len(m) != 1 || !strings.HasPrefix(m[0], "libasound2t64") {
		t.Fatalf("libopus there: %v", m)
	}
	os.WriteFile(filepath.Join(dir, "libasound.so.2"), nil, 0o644)
	if m := voiceLibsMissing(); len(m) != 0 {
		t.Fatalf("both there: %v", m)
	}
}
