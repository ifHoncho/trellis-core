package card

import (
	"context"
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"meridian/trellis/internal/account"
	"meridian/trellis/internal/config"
)

// The pins here and the file the install script and firstboot read must
// be the same.
func TestPinsMatchPackaging(t *testing.T) {
	data, err := os.ReadFile("../../packaging/dkms-morse/firmware.pins")
	if err != nil {
		t.Fatal(err)
	}
	kv := map[string]string{}
	for _, l := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok && !strings.HasPrefix(l, "#") {
			kv[k] = v
		}
	}
	for k, v := range map[string]string{"FW_REF": MorseRef, "FW_FILE": MorseFW, "FW_SHA256": MorseFWSHA256,
		"BCF_FILE": MorseBCF, "BCF_SHA256": MorseBCFSHA256, "LICENSE_FILE": MorseLicense} {
		if kv[k] != v {
			t.Errorf("%s: firmware.pins has %q, card.go %q", k, kv[k], v)
		}
	}
}

const testProfile = "schema: 0\nname: alpha\nsae_passphrase: correct-horse-battery\nregulatory:\n  mode: country\n  country: us\nradio:\n  band: 2g\n  channel: 1\n"

func TestFiles(t *testing.T) {
	k, err := account.ParseKey("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAILhNVh+5m5DDjNfMtEp20Yqp3LnpdpNB3trND13c2OoB op@laptop")
	if err != nil {
		t.Fatal(err)
	}
	c := Card{Profile: []byte(testProfile), Callsign: "kit-07", Password: "correct-horse", Keys: []account.Key{k},
		WebUplink: true, SSHUplink: false, Morse: map[string][]byte{"mm6108.bin": []byte("fw")}}
	files, err := c.Files()
	if err != nil {
		t.Fatal(err)
	}
	n, err := config.ParseNode(files["node.yaml"])
	if err != nil {
		t.Fatalf("node.yaml: %v\n%s", err, files["node.yaml"])
	}
	if n.Callsign != "kit-07" || !n.Firewall.Closed() || n.Firewall.SSHOpen() || !n.Web.WANAccess {
		t.Fatalf("node.yaml: %+v\n%s", n, files["node.yaml"])
	}
	// The firewall is written out: absent would mean open (upgraded nodes).
	if !strings.Contains(string(files["node.yaml"]), "wan: closed") {
		t.Fatalf("firewall.wan not explicit:\n%s", files["node.yaml"])
	}
	if !account.IsHash(strings.TrimSpace(string(files["password"]))) || strings.Contains(string(files["password"]), "correct-horse") {
		t.Fatalf("password: %q", files["password"])
	}
	// The credential is meshd's: the password checks against it.
	var cred struct {
		Algo       string `yaml:"algo"`
		Iterations int    `yaml:"iterations"`
		Salt, Hash string
	}
	if err := yaml.Unmarshal(files["web-admin.yaml"], &cred); err != nil {
		t.Fatal(err)
	}
	salt, _ := base64.StdEncoding.DecodeString(cred.Salt)
	want, _ := pbkdf2.Key(sha256.New, "correct-horse", salt, cred.Iterations, 32)
	if cred.Algo != "pbkdf2-sha256" || cred.Hash != base64.StdEncoding.EncodeToString(want) {
		t.Fatalf("credential: %+v", cred)
	}
	if !strings.Contains(string(files["authorized_keys"]), k.Line()) || string(files["morse/mm6108.bin"]) != "fw" {
		t.Fatal("keys or Morse files missing")
	}

	// Refusals.
	if _, err := (&Card{Callsign: "Bad Name"}).Files(); err == nil {
		t.Error("a bad callsign was taken")
	}
	if _, err := (&Card{Callsign: "a", Profile: []byte("schema: 0\n")}).Files(); err == nil {
		t.Error("a bad profile was taken")
	}
	if _, err := (&Card{Callsign: "a", Password: "short"}).Files(); err == nil {
		t.Error("a short password was taken")
	}
}

func TestWrite(t *testing.T) {
	boot := t.TempDir()
	c := Card{Callsign: "kit-07", Password: "correct-horse", WebUplink: true, SSHUplink: true}
	if _, err := c.Write(boot); err == nil {
		t.Fatal("wrote to a folder that is no boot partition")
	}
	if err := os.WriteFile(filepath.Join(boot, "config.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(boot, "trellis"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(boot, "trellis", "firstboot.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	written, err := c.Write(boot)
	if err != nil || len(written) != 3 {
		t.Fatalf("%v %v", written, err)
	}
	if _, err := os.Stat(filepath.Join(boot, "trellis", "firstboot.txt")); err == nil {
		t.Fatal("an old report was left")
	}
}

func TestFetchMorseChecks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not the pinned firmware"))
	}))
	defer srv.Close()
	old := MorseURL
	MorseURL = srv.URL + "/"
	defer func() { MorseURL = old }()
	if _, err := FetchMorse(context.Background(), srv.Client()); err == nil || !strings.Contains(err.Error(), "not the pinned") {
		t.Fatalf("a file with the wrong hash was taken: %v", err)
	}
}

func TestHaLowBoard(t *testing.T) {
	boot := t.TempDir()
	if HaLowBoard(boot) {
		t.Fatal("no config.txt")
	}
	os.WriteFile(filepath.Join(boot, "config.txt"), []byte("[all]\ndtoverlay=pcie-32bit-dma\n#dtoverlay=trellis-wm6108-spi\n"), 0o644)
	if HaLowBoard(boot) {
		t.Fatal("a commented overlay counted")
	}
	os.WriteFile(filepath.Join(boot, "config.txt"), []byte("[all]\ndtoverlay=trellis-wm6108-spi\n"), 0o644)
	if !HaLowBoard(boot) {
		t.Fatal("the WM6108 overlay not seen")
	}
}
