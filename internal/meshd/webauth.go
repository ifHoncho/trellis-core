package meshd

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"meridian/trellis/internal/api"
)

// The web UI's admin credential (architecture §11). meshd keeps it, not
// webd: webd holds no state of its own and can change nothing on the node
// except through this API. The password is stored as a salted PBKDF2
// hash, readable by root only.

const (
	webAdminFile   = "web-admin.yaml"
	webIterations  = 200_000 // PBKDF2-HMAC-SHA256; about half a second on a Cortex-A53
	webMinPassword = 8
	webMaxPassword = 128
	// After webFreeFailures wrong passwords in a row, each further attempt
	// must wait: 1 s, 2 s, 4 s … up to webMaxLockout.
	webFreeFailures = 5
	webMaxLockout   = time.Minute
)

type webCredential struct {
	Algo       string    `yaml:"algo"`
	Iterations int       `yaml:"iterations"`
	Salt       string    `yaml:"salt"`
	Hash       string    `yaml:"hash"`
	SetAt      time.Time `yaml:"set_at"`
}

type webAuth struct {
	mu          sync.Mutex
	loaded      bool
	cred        *webCredential
	fails       int
	lockedUntil time.Time
}

func (d *Daemon) webAdminPath() string { return filepath.Join(d.opts.StateDir, webAdminFile) }

// webCred returns the stored credential, reading it once. Call with
// webAuth.mu held.
func (d *Daemon) webCred() *webCredential {
	a := &d.webAuth
	if a.loaded {
		return a.cred
	}
	a.loaded = true
	data, err := os.ReadFile(d.webAdminPath())
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			d.log.Warn("cannot read the web admin credential", "err", err)
		}
		return nil
	}
	var c webCredential
	if err := yaml.Unmarshal(data, &c); err != nil || c.Algo != "pbkdf2-sha256" || c.Iterations < 1000 || c.Salt == "" || c.Hash == "" {
		d.log.Warn("the web admin credential file is damaged; no admin password is set", "path", d.webAdminPath(), "err", err)
		return nil
	}
	a.cred = &c
	return a.cred
}

func hashPassword(password string, salt []byte, iterations int) ([]byte, error) {
	return pbkdf2.Key(sha256.New, password, salt, iterations, 32)
}

func (c *webCredential) matches(password string) bool {
	salt, err1 := base64.StdEncoding.DecodeString(c.Salt)
	want, err2 := base64.StdEncoding.DecodeString(c.Hash)
	if err1 != nil || err2 != nil {
		return false
	}
	got, err := hashPassword(password, salt, c.Iterations)
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

// WebInfo is what webd needs to serve the UI.
func (d *Daemon) WebInfo() api.WebInfo {
	node, profile := d.conf()
	wi := api.WebInfo{Callsign: node.Callsign, Version: d.opts.Version, Setup: d.setupState().Needed, WANAccess: node.Web.WANAccess, Prefix: profile.Prefix}
	d.webAuth.mu.Lock()
	if c := d.webCred(); c != nil {
		wi.AdminSet, wi.AdminSince = true, c.SetAt
	}
	d.webAuth.mu.Unlock()
	return wi
}

// checkPassword verifies password against the stored credential, with the
// lockout. Call with webAuth.mu held.
func (d *Daemon) checkPassword(password string) error {
	a := &d.webAuth
	c := d.webCred()
	if c == nil {
		return api.Forbidden("no admin password is set on this node")
	}
	if wait := time.Until(a.lockedUntil); wait > 0 {
		return api.TooMany(wait, "too many wrong passwords; wait %d s and try again", int(wait/time.Second)+1)
	}
	if c.matches(password) {
		a.fails = 0
		return nil
	}
	a.fails++
	if a.fails >= webFreeFailures {
		lock := time.Second << min(a.fails-webFreeFailures, 10)
		a.lockedUntil = time.Now().Add(min(lock, webMaxLockout))
	}
	d.log.Warn("web UI login failed: wrong admin password", "failures_in_a_row", a.fails)
	return api.ErrBadPassword
}

// WebLogin checks the admin password.
func (d *Daemon) WebLogin(password string) error {
	d.webAuth.mu.Lock()
	defer d.webAuth.mu.Unlock()
	return d.checkPassword(password)
}

// WebSetPassword sets the admin password. Who may:
//
//   - root on the API socket (meshctl web password), always, and with an
//     empty new password to remove it;
//   - anyone with the current password;
//   - anyone at all while the node is in setup mode and has none: whoever
//     sets up a node gives it its password.
//
// A provisioned node with no password takes one only from root, so a
// stranger on the mesh cannot claim its UI.
func (d *Daemon) WebSetPassword(current, new string, privileged bool) error {
	d.webAuth.mu.Lock()
	defer d.webAuth.mu.Unlock()
	a := &d.webAuth
	c := d.webCred()
	switch {
	case privileged:
	case c != nil:
		if err := d.checkPassword(current); err != nil {
			if errors.Is(err, api.ErrBadPassword) {
				return api.Errorf(401, "the current password is wrong")
			}
			return err
		}
	case d.setupState().Needed:
	default:
		return api.Forbidden("no admin password is set on this node, and it is already part of a mesh. Set one on the node itself: sudo meshctl web password")
	}
	if new == "" && privileged {
		if err := os.Remove(d.webAdminPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		a.cred, a.loaded, a.fails, a.lockedUntil = nil, true, 0, time.Time{}
		d.log.Info("web admin password removed")
		return nil
	}
	if l := len(new); l < webMinPassword || l > webMaxPassword {
		return api.Invalid("the password must be %d to %d characters (got %d)", webMinPassword, webMaxPassword, l)
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	hash, err := hashPassword(new, salt, webIterations)
	if err != nil {
		return err
	}
	nc := &webCredential{Algo: "pbkdf2-sha256", Iterations: webIterations, Salt: base64.StdEncoding.EncodeToString(salt),
		Hash: base64.StdEncoding.EncodeToString(hash), SetAt: time.Now().UTC().Truncate(time.Millisecond)}
	data, err := yaml.Marshal(nc)
	if err != nil {
		return err
	}
	data = append([]byte("# Trellis web UI admin credential (a salted hash). Reset: sudo meshctl web password\n"), data...)
	if err := writeFileAtomic(d.webAdminPath(), data, 0o600); err != nil {
		return fmt.Errorf("cannot store the admin password: %w", err)
	}
	a.cred, a.loaded, a.fails, a.lockedUntil = nc, true, 0, time.Time{}
	d.log.Info("web admin password set", "by_root", privileged)
	return nil
}

// writeFileAtomic writes data to path through a temporary file in the same
// directory, so a reader never sees half a file, and a power loss leaves
// either the old file or the new one: the data reaches the disk before the
// rename, and the rename before the function returns. A node loses power
// often, and an empty profile.yaml would keep it off the mesh.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// A name of its own: two writers of the same file must not share one.
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	fail := func(err error) error {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Chmod(mode); err != nil {
		return fail(err)
	}
	if _, err := f.Write(data); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}
