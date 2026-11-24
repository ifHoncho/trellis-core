// Package firstboot applies what was put on a node's card when it was
// flashed, and makes an image's node its own.
// It runs at every boot, before meshd (trellis-firstboot.service), and does
// nothing when there is nothing to do.
//
// The card's boot partition may hold a folder trellis/ with:
//
//	profile.yaml     the team's mesh profile
//	node.yaml        this node's configuration (its callsign, at least)
//	web-admin.yaml   the web UI's admin credential, as meshctl web password writes it
//	authorized_keys  SSH public keys for the operator account trellis
//	password         the operator account's password, as a crypt hash ($6$…)
//	morse/           Morse Micro's firmware and BCF for a HaLow module, with Morse's
//	                 licence (an image carries the firmware, never the BCF)
//
// Each file is checked, installed, then overwritten with zeros and removed
// from the card: the profile holds the mesh passphrase, and the card's boot
// partition can be read in any computer. A file that fails its check stays
// on the card. What happened is written to trellis/firstboot.txt, without
// secrets, so it can be read with the card in a computer. Files on the card
// replace the node's own: a card is also how a node is set up again.
//
// On an image (/usr/lib/trellis/image-release), the node also gets a
// hostname of its own in place of the image's, and Morse's driver a MAC
// suffix from the node's machine id: every card flashed from one image
// starts with the same ones.
package firstboot

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"meridian/trellis/internal/account"
	"meridian/trellis/internal/config"
	"meridian/trellis/internal/fsutil"
)

// Defaults on a node.
const (
	DefaultCardDir      = "/boot/firmware/trellis"
	DefaultFirmwareDir  = "/lib/firmware/morse"
	DefaultPinsFile     = "/usr/share/trellis/packaging/dkms-morse/firmware.pins"
	DefaultModprobeConf = "/etc/modprobe.d/trellis-morse.conf"
	DefaultMachineID    = "/etc/machine-id"
	DefaultImageRelease = "/usr/lib/trellis/image-release"
	DefaultHostnameFile = "/etc/hostname"
	DefaultHostsFile    = "/etc/hosts"

	ReportFile   = "firstboot.txt"
	webAdminFile = "web-admin.yaml" // meshd's, in its state directory
)

// Options says where everything is. The zero value of a path means its
// default; the functions must be set (System gives the real ones).
type Options struct {
	CardDir, EtcDir, StateDir, FirmwareDir, PinsFile string
	ModprobeConf, MachineID, ImageRelease            string
	HostnameFile, HostsFile                          string

	// Account changes the operator account (internal/account); nil: card
	// files for it are left on the card.
	Account account.Service

	Hostname     func() (string, error)
	SetHostname  func(string) error
	ModuleLoaded func(name string) bool
	ReloadModule func(name string) error
	Logf         func(format string, args ...any)
}

func (o *Options) defaults() {
	def := func(p *string, v string) {
		if *p == "" {
			*p = v
		}
	}
	def(&o.CardDir, DefaultCardDir)
	def(&o.EtcDir, filepath.Dir(config.DefaultNodePath))
	def(&o.StateDir, config.DefaultStateDir)
	def(&o.FirmwareDir, DefaultFirmwareDir)
	def(&o.PinsFile, DefaultPinsFile)
	def(&o.ModprobeConf, DefaultModprobeConf)
	def(&o.MachineID, DefaultMachineID)
	def(&o.ImageRelease, DefaultImageRelease)
	def(&o.HostnameFile, DefaultHostnameFile)
	def(&o.HostsFile, DefaultHostsFile)
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
}

// Result is what a run did.
type Result struct {
	Installed []string          // card files installed, by their name on the card
	Rejected  map[string]string // card files left on the card, and why
	Hostname  string            // the new hostname, if it was changed
	MACSuffix string            // the suffix added to Morse's driver options, if any
	Reloaded  bool              // Morse's driver was reloaded
}

// Run does what there is to do. Errors that concern one card file are in
// Result.Rejected; the error returned is for the rest (the hostname, the
// driver options, the report).
func Run(o Options) (Result, error) {
	o.defaults()
	res := Result{Rejected: map[string]string{}}
	var errs []error

	cardSeen := false
	if st, err := os.Stat(o.CardDir); err == nil && st.IsDir() {
		cardSeen = true
		installCard(&o, &res)
	}

	morseChanged := false
	for _, f := range res.Installed {
		if strings.HasPrefix(f, "morse/") {
			morseChanged = true
		}
	}

	if image, err := readKV(o.ImageRelease); err == nil {
		if name, err := ownHostname(&o, image["IMAGE_HOSTNAME"]); err != nil {
			errs = append(errs, fmt.Errorf("hostname: %w", err))
		} else if name != "" {
			res.Hostname = name
			o.Logf("hostname set to %s", name)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, fmt.Errorf("%s: %w", o.ImageRelease, err))
	}

	if suffix, err := morseSuffix(&o); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", o.ModprobeConf, err))
	} else if suffix != "" {
		res.MACSuffix = suffix
		morseChanged = true
		o.Logf("Morse driver: macaddr_suffix=%s from this node's machine id", suffix)
	}

	// The driver read its options and firmware when it loaded, which may
	// have been before this ran. Nothing uses the radio yet: meshd starts
	// after this.
	if morseChanged && o.ModuleLoaded != nil && o.ModuleLoaded("morse") && o.ReloadModule != nil {
		if err := o.ReloadModule("morse"); err != nil {
			errs = append(errs, fmt.Errorf("reload morse: %w", err))
		} else {
			res.Reloaded = true
			o.Logf("Morse driver reloaded")
		}
	}

	if cardSeen && (len(res.Installed) > 0 || len(res.Rejected) > 0) {
		if err := writeReport(&o, &res); err != nil {
			errs = append(errs, fmt.Errorf("report: %w", err))
		}
	}
	return res, errors.Join(errs...)
}

// installCard installs the card's files, in an order where each check can
// use what came before: the node's configuration names its profile's path.
func installCard(o *Options, res *Result) {
	reject := func(name string, err error) {
		res.Rejected[name] = err.Error()
		o.Logf("card: %s not installed: %v", name, err)
	}
	done := func(name, src string) {
		res.Installed = append(res.Installed, name)
		o.Logf("card: %s installed", name)
		if err := wipe(src); err != nil {
			o.Logf("card: %s: cannot remove it from the card: %v", name, err)
		}
	}

	nodePath := filepath.Join(o.EtcDir, filepath.Base(config.DefaultNodePath))
	if src := filepath.Join(o.CardDir, "node.yaml"); exists(src) {
		if data, err := os.ReadFile(src); err != nil {
			reject("node.yaml", err)
		} else if _, err := config.ParseNode(data); err != nil {
			reject("node.yaml", err)
		} else if err := fsutil.WriteFile(nodePath, data, 0o644); err != nil {
			reject("node.yaml", err)
		} else {
			done("node.yaml", src)
		}
	}

	if src := filepath.Join(o.CardDir, "profile.yaml"); exists(src) {
		dst := filepath.Join(o.EtcDir, filepath.Base(config.DefaultProfilePath))
		if n, err := config.LoadNode(nodePath); err == nil && n.ProfilePath != config.DefaultProfilePath {
			dst = n.ProfilePath
		}
		if data, err := os.ReadFile(src); err != nil {
			reject("profile.yaml", err)
		} else if _, err := config.ParseProfile(data); err != nil {
			reject("profile.yaml", err)
		} else if err := fsutil.WriteFile(dst, data, 0o600); err != nil {
			reject("profile.yaml", err)
		} else {
			done("profile.yaml", src)
		}
	}

	if src := filepath.Join(o.CardDir, webAdminFile); exists(src) {
		if data, err := os.ReadFile(src); err != nil {
			reject(webAdminFile, err)
		} else if err := checkCredential(data); err != nil {
			reject(webAdminFile, err)
		} else if err := fsutil.WriteFile(filepath.Join(o.StateDir, webAdminFile), data, 0o600); err != nil {
			reject(webAdminFile, err)
		} else {
			done(webAdminFile, src)
		}
	}

	installAccount(o, reject, done)

	morse := filepath.Join(o.CardDir, "morse")
	entries, err := os.ReadDir(morse)
	if err != nil {
		return
	}
	pins, err := readKV(o.PinsFile)
	if err != nil {
		reject("morse/", fmt.Errorf("cannot read the pinned hashes: %w", err))
		return
	}
	want := map[string]string{ // name on the card -> SHA-256 ("" = any)
		filepath.Base(pins["FW_FILE"]):  pins["FW_SHA256"],
		filepath.Base(pins["BCF_FILE"]): pins["BCF_SHA256"],
		"LICENSE":                       "",
	}
	for _, e := range entries {
		name := "morse/" + e.Name()
		src := filepath.Join(morse, e.Name())
		sum, known := want[e.Name()]
		switch {
		case !e.Type().IsRegular():
			reject(name, errors.New("not a file"))
		case !known:
			reject(name, errors.New("not a file Trellis installs (the pinned firmware, BCF and LICENSE only)"))
		default:
			data, err := os.ReadFile(src)
			if err != nil {
				reject(name, err)
				continue
			}
			if sum != "" {
				h := sha256.Sum256(data)
				if got := hex.EncodeToString(h[:]); got != sum {
					reject(name, fmt.Errorf("SHA-256 %s, not the pinned %s", got, sum))
					continue
				}
			}
			if err := fsutil.WriteFile(filepath.Join(o.FirmwareDir, e.Name()), data, 0o644); err != nil {
				reject(name, err)
				continue
			}
			done(name, src)
		}
	}
	_ = os.Remove(morse) // only if empty
}

// installAccount sets up the operator account from the card: its SSH keys
// (added to any it has) and a password hash. Both files go in one change;
// if either is wrong, both stay on the card.
func installAccount(o *Options, reject func(string, error), done func(string, string)) {
	files := map[string]string{}
	for _, n := range []string{"authorized_keys", "password"} {
		if p := filepath.Join(o.CardDir, n); exists(p) {
			files[n] = p
		}
	}
	if len(files) == 0 {
		return
	}
	fail := func(err error) {
		for _, n := range []string{"authorized_keys", "password"} {
			if _, ok := files[n]; ok {
				reject(n, err)
			}
		}
	}
	if o.Account == nil {
		fail(errors.New("operator account not managed here"))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var ch account.Change
	if p, ok := files["authorized_keys"]; ok {
		data, err := os.ReadFile(p)
		if err != nil {
			fail(err)
			return
		}
		keys, err := account.ParseKeys(string(data))
		if err != nil {
			fail(fmt.Errorf("authorized_keys: %v", err))
			return
		}
		if len(keys) == 0 {
			fail(errors.New("authorized_keys: no keys"))
			return
		}
		if cur, err := o.Account.Get(ctx); err == nil {
			keys = account.MergeKeys(cur.Keys, keys...)
		}
		ch.Keys = &keys
	}
	if p, ok := files["password"]; ok {
		data, err := os.ReadFile(p)
		if err != nil {
			fail(err)
			return
		}
		h := strings.TrimSpace(string(data))
		if !account.IsHash(h) {
			fail(errors.New("password: a crypt hash ($6$…) expected, not a password"))
			return
		}
		ch.PasswordHash = h
	}
	if err := o.Account.Apply(ctx, ch); err != nil {
		fail(fmt.Errorf("operator account: %v", err))
		return
	}
	for _, n := range []string{"authorized_keys", "password"} {
		if p, ok := files[n]; ok {
			done(n, p)
		}
	}
}

// checkCredential accepts what meshd's web admin credential file holds.
func checkCredential(data []byte) error {
	var c struct {
		Algo       string `yaml:"algo"`
		Iterations int    `yaml:"iterations"`
		Salt       string `yaml:"salt"`
		Hash       string `yaml:"hash"`
	}
	if err := yaml.Unmarshal(data, &c); err != nil {
		return err
	}
	if c.Algo != "pbkdf2-sha256" {
		return fmt.Errorf("algo %q: pbkdf2-sha256 expected", c.Algo)
	}
	if c.Iterations < 10_000 {
		return fmt.Errorf("iterations %d: at least 10000 expected", c.Iterations)
	}
	if s, err := base64.StdEncoding.DecodeString(c.Salt); err != nil || len(s) < 8 {
		return errors.New("salt: base64 of at least 8 bytes expected")
	}
	if h, err := base64.StdEncoding.DecodeString(c.Hash); err != nil || len(h) != sha256.Size {
		return errors.New("hash: base64 of 32 bytes expected")
	}
	return nil
}

// ownHostname gives the node a hostname of its own while it still has the
// image's: the image's, a dash and four hex digits of its machine id.
func ownHostname(o *Options, imageName string) (string, error) {
	if imageName == "" || o.Hostname == nil {
		return "", nil
	}
	cur, err := o.Hostname()
	if err != nil {
		return "", err
	}
	if cur != imageName {
		return "", nil
	}
	id, err := machineID(o.MachineID)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256([]byte(id))
	name := imageName + "-" + hex.EncodeToString(h[:2])
	if err := fsutil.WriteFile(o.HostnameFile, []byte(name+"\n"), 0o644); err != nil {
		return "", err
	}
	if err := setHostsName(o.HostsFile, name); err != nil {
		return "", err
	}
	if o.SetHostname != nil {
		if err := o.SetHostname(name); err != nil {
			return "", err
		}
	}
	return name, nil
}

// setHostsName points 127.0.1.1 at name, as Debian does for the hostname.
func setHostsName(path, name string) error {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	line := "127.0.1.1\t" + name
	var out []string
	found := false
	for _, l := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if f := strings.Fields(l); len(f) > 0 && f[0] == "127.0.1.1" {
			if !found {
				out = append(out, line)
				found = true
			}
			continue
		}
		if l != "" || len(out) > 0 {
			out = append(out, l)
		}
	}
	if !found {
		out = append(out, line)
	}
	return fsutil.WriteFile(path, []byte(strings.Join(out, "\n")+"\n"), 0o644)
}

var suffixRe = regexp.MustCompile(`\bmacaddr_suffix=`)

// morseSuffix adds macaddr_suffix to Morse's driver options when they have
// none, derived as install-wm6108-halow.sh derives it: the first three
// bytes of the SHA-256 of /etc/machine-id (the file, newline included).
func morseSuffix(o *Options) (string, error) {
	data, err := os.ReadFile(o.ModprobeConf)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if suffixRe.Match(data) {
		return "", nil
	}
	idData, err := os.ReadFile(o.MachineID)
	if err != nil {
		return "", err
	}
	if _, err := machineID(o.MachineID); err != nil {
		return "", err
	}
	h := sha256.Sum256(idData)
	suffix := fmt.Sprintf("%02x:%02x:%02x", h[0], h[1], h[2])
	if suffix == "00:00:00" { // "random" to the driver
		suffix = "00:00:01"
	}
	var out bytes.Buffer
	changed := false
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		l := sc.Text()
		if f := strings.Fields(l); !changed && len(f) >= 2 && f[0] == "options" && f[1] == "morse" {
			l = strings.TrimRight(l, " \t") + " macaddr_suffix=" + suffix
			changed = true
		}
		out.WriteString(l + "\n")
	}
	if !changed {
		return "", nil
	}
	if err := fsutil.WriteFile(o.ModprobeConf, out.Bytes(), 0o644); err != nil {
		return "", err
	}
	return suffix, nil
}

// machineID reads a machine id that systemd has set: 32 hex digits.
func machineID(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(data))
	if len(id) != 32 {
		return "", fmt.Errorf("%s holds no machine id yet (%q)", path, id)
	}
	if _, err := hex.DecodeString(id); err != nil {
		return "", fmt.Errorf("%s: not a machine id", path)
	}
	return id, nil
}

// writeReport writes trellis/firstboot.txt on the card: file names and
// reasons only, never a file's content.
func writeReport(o *Options, res *Result) error {
	var b strings.Builder
	host := res.Hostname
	if host == "" && o.Hostname != nil {
		host, _ = o.Hostname()
	}
	fmt.Fprintf(&b, "Trellis first boot: %s\n\n", host)
	if len(res.Installed) > 0 {
		b.WriteString("Installed, and removed from the card:\n")
		for _, f := range res.Installed {
			fmt.Fprintf(&b, "  %s\n", f)
		}
	}
	if len(res.Rejected) > 0 {
		names := make([]string, 0, len(res.Rejected))
		for f := range res.Rejected {
			names = append(names, f)
		}
		sort.Strings(names)
		b.WriteString("Not installed, left on the card:\n")
		for _, f := range names {
			fmt.Fprintf(&b, "  %s: %s\n", f, res.Rejected[f])
		}
	}
	return os.WriteFile(filepath.Join(o.CardDir, ReportFile), []byte(b.String()), 0o644)
}

// wipe overwrites a file with zeros and removes it. On a FAT boot
// partition the overwrite reuses the file's clusters, so the content is not
// left in free space; flash wear levelling may still keep old blocks.
func wipe(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err == nil {
		_, err = f.Write(make([]byte, st.Size()))
	}
	if err == nil {
		err = f.Sync()
	}
	f.Close()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// readKV reads KEY=VALUE lines; # starts a comment line.
func readKV(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	kv := map[string]string{}
	for _, l := range strings.Split(string(data), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			continue
		}
		kv[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return kv, nil
}
