package wpa

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRender(t *testing.T) {
	out := Render(MeshConfig{CtrlDir: "/run/trellis/wpa", MeshID: "trellis-alpha", Passphrase: "correct horse", FreqMHz: 5745, WidthMHz: 80})
	for _, want := range []string{
		"ctrl_interface=DIR=/run/trellis/wpa",
		"ssid=7472656c6c69732d616c706861",
		"mode=5", "frequency=5745", "key_mgmt=SAE", `sae_password="correct horse"`,
		"ieee80211w=2", "mesh_fwding=0", "ht40=1", "vht=1", "max_oper_chwidth=1", "vht_center_freq1=5775",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// 160 MHz: wpa_supplicant places the block (max_oper_chwidth 2 is
	// CONF_OPER_CHWIDTH_160MHZ), on 5 and 6 GHz alike.
	for _, f := range []int{5180, 5745, 6135} {
		out160 := Render(MeshConfig{MeshID: "x", Passphrase: "y", FreqMHz: f, WidthMHz: 160})
		if !strings.Contains(out160, "\tmax_oper_chwidth=2\n") || !strings.Contains(out160, "\tvht=1\n") || !strings.Contains(out160, "\tht40=1\n") ||
			strings.Contains(out160, "disable_ht40") || strings.Contains(out160, "vht_center_freq1") {
			t.Errorf("160 MHz at %d MHz wrong:\n%s", f, out160)
		}
	}
	// The country makes wpa_supplicant handle radar for the mesh.
	if out := Render(MeshConfig{MeshID: "x", Passphrase: "y", FreqMHz: 5500, WidthMHz: 160, Country: "US"}); !strings.Contains(out, "\ncountry=US\n") {
		t.Errorf("no global country line:\n%s", out)
	}
	if out := Render(MeshConfig{MeshID: "x", Passphrase: "y", FreqMHz: 2437, WidthMHz: 20}); strings.Contains(out, "country=") {
		t.Errorf("country line without a country:\n%s", out)
	}
	out20 := Render(MeshConfig{MeshID: "x", Passphrase: "y", FreqMHz: 2437, WidthMHz: 20})
	if !strings.Contains(out20, "disable_ht40=1") || strings.Contains(out20, "vht=1") || strings.Contains(out20, "max_oper_chwidth") {
		t.Errorf("20 MHz config wrong:\n%s", out20)
	}
	// 5 GHz at 20 and 40 MHz: VHT on, held to the HT width. The kernel then
	// reports 20 and 40 MHz on an 802.11ax radio; without the two settings
	// the 20 MHz mesh does not start and the 40 MHz one runs at 80.
	for width, first := range map[int]string{20: "disable_ht40=1", 40: "ht40=1"} {
		out5 := Render(MeshConfig{MeshID: "x", Passphrase: "y", FreqMHz: 5745, WidthMHz: width})
		for _, want := range []string{first, "\tvht=1\n", "\tmax_oper_chwidth=0\n"} {
			if !strings.Contains(out5, want) {
				t.Errorf("5 GHz at %d MHz lacks %q:\n%s", width, want, out5)
			}
		}
		if strings.Contains(out5, "vht_center_freq1") || strings.Contains(out5, "max_oper_chwidth=1") {
			t.Errorf("5 GHz at %d MHz asks for 80 MHz:\n%s", width, out5)
		}
		// 2.4 GHz has no VHT to hold.
		if out2 := Render(MeshConfig{MeshID: "x", Passphrase: "y", FreqMHz: 2437, WidthMHz: width}); strings.Contains(out2, "vht") || strings.Contains(out2, "max_oper_chwidth") {
			t.Errorf("2.4 GHz at %d MHz has VHT settings:\n%s", width, out2)
		}
	}
	// A build without HT overrides rejects the whole file on disable_ht40.
	outNo := Render(MeshConfig{MeshID: "x", Passphrase: "y", FreqMHz: 5180, WidthMHz: 20, NoHTOverrides: true})
	if strings.Contains(outNo, "disable_ht40") || !strings.Contains(outNo, "max_oper_chwidth=0") {
		t.Errorf("20 MHz config without HT overrides wrong:\n%s", outNo)
	}
}

// A HaLow mesh, for Morse Micro's wpa_supplicant_s1g: a block known to start
// a mesh.
func TestRenderS1G(t *testing.T) {
	pw, idx := S1GPrimary(2)
	out := Render(MeshConfig{CtrlDir: "/run/trellis/wpa", MeshID: "trellis-alpha", Passphrase: "correct horse", BeaconIntTU: 1000,
		FreqMHz: 5745, WidthMHz: 80, // ignored for S1G
		S1G: &S1GParams{Country: "US", OpClass: 69, Channel: 26, PrimaryWidthMHz: pw, Primary1MHzIdx: idx}})
	for _, want := range []string{
		"\ncountry=US\n", "\nsae_pwe=1\n", "mode=5", "key_mgmt=SAE", `sae_password="correct horse"`, "ieee80211w=2", "mesh_fwding=0",
		"\tcountry=\"US\"\n", "\top_class=69\n", "\tchannel=26\n", "\ts1g_prim_chwidth=1\n", "\ts1g_prim_1mhz_chan_index=0\n", "\tbeacon_int=1000\n", "\tdtim_period=1\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, not := range []string{"frequency=", "ht40", "vht", "max_oper_chwidth", "disable_ht40"} {
		if strings.Contains(out, not) {
			t.Errorf("a HaLow config must not carry %q:\n%s", not, out)
		}
	}
	// A 1 MHz channel is its own primary channel.
	pw, idx = S1GPrimary(1)
	out = Render(MeshConfig{MeshID: "x", Passphrase: "y", S1G: &S1GParams{Country: "US", OpClass: 68, Channel: 27, PrimaryWidthMHz: pw, Primary1MHzIdx: idx}})
	if !strings.Contains(out, "s1g_prim_chwidth=0\n") {
		t.Errorf("1 MHz config:\n%s", out)
	}
	// Wi-Fi configs carry none of it.
	if wifi := Render(MeshConfig{MeshID: "x", Passphrase: "y", FreqMHz: 2437, WidthMHz: 20}); strings.Contains(wifi, "country") || strings.Contains(wifi, "sae_pwe") || strings.Contains(wifi, "op_class") || strings.Contains(wifi, "dtim_period") {
		t.Errorf("Wi-Fi config has HaLow settings:\n%s", wifi)
	}
}

func TestHTOverridesFromProbe(t *testing.T) {
	// Fedora 44, wpa_supplicant v2.11 (built without CONFIG_HT_OVERRIDES).
	fedora := "Successfully initialized wpa_supplicant\nLine 13: unknown network field 'disable_ht40'.\n" +
		"Line 15: failed to parse network block.\nFailed to read or parse configuration '/tmp/p.conf'.\n"
	if htOverridesFromProbe(fedora) {
		t.Error("Fedora's output means no HT overrides")
	}
	debian := "Successfully initialized wpa_supplicant\nCould not read interface trellis-probe0 flags: No such device\n"
	if !htOverridesFromProbe(debian) {
		t.Error("a parsed file means HT overrides are there")
	}
}

func TestCenterFreq80(t *testing.T) {
	for f, want := range map[int]int{5180: 5210, 5200: 5210, 5240: 5210, 5260: 5290, 5500: 5530, 5745: 5775, 5805: 5775, 5825: 5855, 5955: 0} {
		if got := CenterFreq80(f); got != want {
			t.Errorf("CenterFreq80(%d) = %d, want %d", f, got, want)
		}
	}
}

func TestPassphraseOK(t *testing.T) {
	if err := PassphraseOK("good-passphrase 123"); err != nil {
		t.Error(err)
	}
	for _, bad := range []string{`has"quote`, `back\slash`, "tab\there", "ünïcode"} {
		if PassphraseOK(bad) == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestParseEvent(t *testing.T) {
	e, ok := ParseEvent("<3>MESH-PEER-CONNECTED aa:bb:cc:dd:ee:ff")
	if !ok || e.Level != 3 || e.Name != EvMeshPeerConnected || e.Arg() != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("got %+v", e)
	}
	e, _ = ParseEvent(`<3>MESH-SAE-AUTH-BLOCKED addr=aa:bb:cc:dd:ee:ff duration=10`)
	if e.KV["addr"] != "aa:bb:cc:dd:ee:ff" || e.KV["duration"] != "10" {
		t.Errorf("kv: %+v", e)
	}
	e, _ = ParseEvent(`<3>MESH-GROUP-STARTED ssid="trellis" id=0`)
	if e.KV["ssid"] != "trellis" {
		t.Errorf("quoted kv: %+v", e)
	}
}

// fakeSupplicant answers PING/STATUS/ATTACH on a unixgram socket.
func fakeSupplicant(t *testing.T, path string) (events func(string)) {
	t.Helper()
	addr := &net.UnixAddr{Name: path, Net: "unixgram"}
	conn, err := net.ListenUnixgram("unixgram", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	// attached is set by the reader goroutine and read by the returned
	// sender, so it is guarded (go test -race caught it unguarded).
	var mu sync.Mutex
	var attached *net.UnixAddr
	go func() {
		buf := make([]byte, 4096)
		for {
			n, from, err := conn.ReadFromUnix(buf)
			if err != nil {
				return
			}
			cmd := string(buf[:n])
			var reply string
			switch cmd {
			case "PING":
				reply = "PONG\n"
			case "STATUS":
				reply = "bssid=00:11:22:33:44:55\nfreq=5745\nssid=trellis\nmode=mesh\nwpa_state=COMPLETED\n"
			case "ATTACH":
				a := *from
				mu.Lock()
				attached = &a
				mu.Unlock()
				reply = "OK\n"
			case "DETACH":
				reply = "OK\n"
			default:
				reply = "UNKNOWN COMMAND\n"
			}
			conn.WriteToUnix([]byte(reply), from)
		}
	}()
	return func(ev string) {
		mu.Lock()
		to := attached
		mu.Unlock()
		if to != nil {
			conn.WriteToUnix([]byte(ev), to)
		}
	}
}

func TestCtrlAgainstFake(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "mesh0")
	send := fakeSupplicant(t, sock)

	c, err := DialCtrl(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := c.GetStatus(ctx)
	if err != nil || !st.Joined() || st.Freq() != 5745 {
		t.Fatalf("status: %v %v", st, err)
	}

	ev, err := DialEvents(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ev.Close()
	time.Sleep(20 * time.Millisecond)
	send("<3>MESH-PEER-CONNECTED aa:bb:cc:dd:ee:ff")
	e, err := ev.Next(time.Second)
	if err != nil || e.Name != EvMeshPeerConnected {
		t.Fatalf("event: %+v %v", e, err)
	}
	if _, err := ev.Next(50 * time.Millisecond); !IsTimeout(err) {
		t.Errorf("expected timeout, got %v", err)
	}
}

// TestHTOverridesProbe runs the real probe against an installed
// wpa_supplicant, which needs no root (it fails before touching a driver).
func TestHTOverridesProbe(t *testing.T) {
	path := "/usr/sbin/wpa_supplicant"
	if _, err := os.Stat(path); err != nil {
		t.Skip("no wpa_supplicant installed")
	}
	ok, err := HTOverrides(context.Background(), path, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s HT overrides: %v", path, ok)
}
