// Package card writes the files a node's card carries from the computer it
// was flashed on: a folder trellis/ on the boot
// partition, which trellis firstboot installs at the node's first boot and
// removes from the card (internal/firstboot). trellis-deploy --card uses it.
package card

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"meridian/trellis/internal/account"
	"meridian/trellis/internal/config"
)

// Morse Micro's firmware and BCF for the Wio-WM6108, as
// packaging/dkms-morse/firmware.pins pins them (a test keeps the two the
// same). Fetched by the computer: an image carries the firmware, never
// the BCF.
const (
	MorseRef       = "db4b69fd212990c59c5f1712e23060cbb94276ec"
	MorseFW        = "firmware/mm6108.bin"
	MorseFWSHA256  = "aeacb187816b62c55efc362cde256c02b862613ebf136b96e11e4fa05b3e1b03"
	MorseBCF       = "bcf/quectel/bcf_fgh100mhaamd.bin"
	MorseBCFSHA256 = "55089af7d08597a16ddd4002ca4b23f24729aa5bd0f1b75b6e04bc5270d23ba6"
	MorseLicense   = "firmware/LICENSE"
)

// MorseURL is where Morse's files are fetched from: the repository at the
// pinned commit. A variable for tests.
var MorseURL = "https://raw.githubusercontent.com/MorseMicro/morse-firmware/" + MorseRef + "/"

// Card is what goes on one card.
type Card struct {
	Profile   []byte // a mesh profile file; nil: the node starts in setup mode
	Callsign  string
	Password  string // the web UI admin's and the operator account's
	Keys      []account.Key
	WebUplink bool
	SSHUplink bool
	Morse     map[string][]byte // file name -> content (FetchMorse)
}

// Files returns the card's files by their name under trellis/.
func (c *Card) Files() (map[string][]byte, error) {
	files := map[string][]byte{}
	if c.Profile != nil {
		if _, err := config.ParseProfile(c.Profile); err != nil {
			return nil, fmt.Errorf("mesh profile: %v", err)
		}
		files["profile.yaml"] = c.Profile
	}
	ssh := c.SSHUplink
	n := config.Node{Schema: config.SchemaVersion, Callsign: c.Callsign,
		Firewall: config.FirewallConfig{WAN: config.FirewallClosed, SSH: &ssh}}
	n.Web.WANAccess = c.WebUplink
	check := n
	check.ApplyDefaults()
	if err := check.Validate(); err != nil {
		return nil, fmt.Errorf("node: %v", err)
	}
	node, err := n.YAML("Node configuration, written by trellis-deploy --card.")
	if err != nil {
		return nil, err
	}
	files["node.yaml"] = node
	if c.Password != "" {
		if err := account.CheckPassword(c.Password); err != nil {
			return nil, err
		}
		cred, err := WebCredential(c.Password)
		if err != nil {
			return nil, err
		}
		files["web-admin.yaml"] = cred
		h, err := account.HashPassword(c.Password)
		if err != nil {
			return nil, err
		}
		files["password"] = []byte(h + "\n")
	}
	if len(c.Keys) > 0 {
		var b strings.Builder
		b.WriteString("# SSH keys for the operator account trellis (trellis-deploy --card).\n")
		for _, k := range c.Keys {
			b.WriteString(k.Line() + "\n")
		}
		files["authorized_keys"] = []byte(b.String())
	}
	for name, data := range c.Morse {
		files["morse/"+name] = data
	}
	return files, nil
}

// HaLowBoard says whether the card's image is for a board with a HaLow
// module: its config.txt enables the WM6108 overlay.
func HaLowBoard(boot string) bool {
	data, err := os.ReadFile(filepath.Join(boot, "config.txt"))
	if err != nil {
		return false
	}
	for _, l := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(l) == "dtoverlay=trellis-wm6108-spi" {
			return true
		}
	}
	return false
}

// Write puts the card's files in boot/trellis/. boot must be a Raspberry
// Pi boot partition (it holds config.txt): a wrong folder is refused.
func (c *Card) Write(boot string) ([]string, error) {
	if _, err := os.Stat(filepath.Join(boot, "config.txt")); err != nil {
		return nil, fmt.Errorf("%s is not a Raspberry Pi boot partition (no config.txt)", boot)
	}
	files, err := c.Files()
	if err != nil {
		return nil, err
	}
	var written []string
	for name, data := range files {
		p := filepath.Join(boot, "trellis", filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return written, err
		}
		if err := os.WriteFile(p, data, 0o600); err != nil {
			return written, err
		}
		written = append(written, "trellis/"+name)
	}
	// An earlier first boot's report would say what that one did.
	_ = os.Remove(filepath.Join(boot, "trellis", "firstboot.txt"))
	return written, nil
}

// WebCredential is the web UI admin credential file meshd keeps
// (internal/meshd/webauth.go): PBKDF2-HMAC-SHA256, 200,000 iterations.
func WebCredential(password string) ([]byte, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	const iterations = 200_000
	hash, err := pbkdf2.Key(sha256.New, password, salt, iterations, 32)
	if err != nil {
		return nil, err
	}
	c := struct {
		Algo       string    `yaml:"algo"`
		Iterations int       `yaml:"iterations"`
		Salt       string    `yaml:"salt"`
		Hash       string    `yaml:"hash"`
		SetAt      time.Time `yaml:"set_at"`
	}{"pbkdf2-sha256", iterations, base64.StdEncoding.EncodeToString(salt), base64.StdEncoding.EncodeToString(hash), time.Now().UTC().Truncate(time.Millisecond)}
	data, err := yaml.Marshal(c)
	if err != nil {
		return nil, err
	}
	return append([]byte("# Trellis web UI admin credential (a salted hash). Reset: sudo meshctl web password\n"), data...), nil
}

// FetchMorse fetches Morse's firmware, BCF and licence at the pinned
// commit and checks the first two against their pinned SHA-256.
func FetchMorse(ctx context.Context, client *http.Client) (map[string][]byte, error) {
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	out := map[string][]byte{}
	for _, f := range []struct{ path, sum string }{{MorseFW, MorseFWSHA256}, {MorseBCF, MorseBCFSHA256}, {MorseLicense, ""}} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, MorseURL+f.path, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("Morse firmware: %v", err)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("Morse firmware: %s: %s", f.path, resp.Status)
		}
		if f.sum != "" {
			h := sha256.Sum256(data)
			if got := hex.EncodeToString(h[:]); got != f.sum {
				return nil, fmt.Errorf("Morse firmware: %s has SHA-256 %s, not the pinned %s", f.path, got, f.sum)
			}
		}
		out[filepath.Base(f.path)] = data
	}
	if len(out) != 3 {
		return nil, errors.New("Morse firmware: incomplete")
	}
	return out, nil
}
