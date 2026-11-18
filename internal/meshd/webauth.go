package meshd

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"meridian/trellis/internal/api"
	"meridian/trellis/internal/fsutil"
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
	// After webFreeFailures wrong passwords in a row from one source (an
	// address, as webd reports it), each further attempt from that source
	// must wait: 1 s, 2 s, 4 s … up to webMaxLockout. Other sources are not
	// held up: a lockout shared by all would let any client on the Wi-Fi
	// keep the admin out for as long as it kept guessing. Guessing from many
	// addresses is bounded by the hash: one at a time, about half a second
	// each on a Cortex-A53.
	webFreeFailures = 5
	webMaxLockout   = time.Minute
	webSourcesMax   = 256              // sources tracked; the quietest goes first
	webForgetAfter  = 10 * time.Minute // a source quiet this long starts afresh
)

// webSource is one source's run of wrong passwords.
type webSource struct {
	fails       int
	lockedUntil time.Time
	lastFail    time.Time
}

type webCredential struct {
	Algo       string    `yaml:"algo"`
	Iterations int       `yaml:"iterations"`
	Salt       string    `yaml:"salt"`
	Hash       string    `yaml:"hash"`
	SetAt      time.Time `yaml:"set_at"`
}

type webAuth struct {
	mu      sync.Mutex
	loaded  bool
	cred    *webCredential
	sources map[string]*webSource // by source; "" is a local caller

	// hashMu lets one password hash run at a time, without holding mu:
	// WebInfo, which webd asks for every connection, never waits on a hash.
	hashMu sync.Mutex
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

// sourceKey is the lockout key for an address as webd reports it. An IPv6
// client can take any address in its /64 (SLAAC, privacy addresses), so
// IPv6 sources are keyed by their /64.
func sourceKey(from string) string {
	a, err := netip.ParseAddr(from)
	if err != nil {
		return from
	}
	a = a.Unmap()
	if a.Is6() {
		p, _ := a.WithZone("").Prefix(64)
		return p.String()
	}
	return a.String()
}

// source returns the record for key, making it if needed. Call with
// webAuth.mu held.
func (a *webAuth) source(key string, now time.Time) *webSource {
	if a.sources == nil {
		a.sources = map[string]*webSource{}
	}
	src := a.sources[key]
	if src != nil && now.Sub(src.lastFail) > webForgetAfter && !now.Before(src.lockedUntil) {
		src.fails, src.lockedUntil = 0, time.Time{}
	}
	if src == nil {
		for len(a.sources) >= webSourcesMax {
			delete(a.sources, a.quietest(now))
		}
		src = &webSource{}
		a.sources[key] = src
	}
	return src
}

// quietest picks the source to forget when the table is full: the one
// whose last failure is oldest, preferring one not locked out now, so a
// guesser cannot push its own lockout out by filling the table.
func (a *webAuth) quietest(now time.Time) string {
	oldest, at, locked := "", time.Time{}, true
	for k, v := range a.sources {
		l := now.Before(v.lockedUntil)
		if oldest == "" || (locked && !l) || (locked == l && v.lastFail.Before(at)) {
			oldest, at, locked = k, v.lastFail, l
		}
	}
	return oldest
}

// checkPassword verifies password against the stored credential, with the
// lockout for its source. Call with webAuth.mu held; it is released while
// the hash runs.
//
// Attempts are serialised by hashMu, and each one checks the lockout after
// taking it, so parallel requests from one source cannot all pass the
// check before the first failure is counted.
func (d *Daemon) checkPassword(from, password string) error {
	a := &d.webAuth
	key := sourceKey(from)
	a.mu.Unlock()
	a.hashMu.Lock()
	defer a.hashMu.Unlock()
	a.mu.Lock()
	c := d.webCred()
	if c == nil {
		return api.Forbidden("no admin password is set on this node")
	}
	if src := a.sources[key]; src != nil {
		if wait := time.Until(src.lockedUntil); wait > 0 {
			return api.TooMany(wait, "too many wrong passwords; wait %d s and try again", int(wait/time.Second)+1)
		}
	}
	a.mu.Unlock()
	ok := c.matches(password)
	a.mu.Lock()
	if a.cred != c {
		// The password was removed while the hash ran (meshctl does not
		// take hashMu to remove it).
		return api.Errorf(409, "the admin password changed meanwhile; try again")
	}
	now := time.Now()
	if ok {
		delete(a.sources, key)
		return nil
	}
	src := a.source(key, now)
	src.fails++
	src.lastFail = now
	if src.fails >= webFreeFailures {
		lock := time.Second << min(src.fails-webFreeFailures, 10)
		src.lockedUntil = now.Add(min(lock, webMaxLockout))
	}
	where := from
	if where == "" {
		where = "local"
	}
	d.log.Warn("web UI login failed: wrong admin password", "from", where, "failures_in_a_row", src.fails)
	return api.ErrBadPassword
}

// WebLogin checks the admin password.
func (d *Daemon) WebLogin(from, password string) error {
	d.webAuth.mu.Lock()
	defer d.webAuth.mu.Unlock()
	return d.checkPassword(from, password)
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
func (d *Daemon) WebSetPassword(from, current, new string, privileged bool) error {
	d.webAuth.mu.Lock()
	defer d.webAuth.mu.Unlock()
	a := &d.webAuth
	c := d.webCred()
	switch {
	case privileged:
	case c != nil:
		if err := d.checkPassword(from, current); err != nil {
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
		a.cred, a.loaded, a.sources = nil, true, nil
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
	// Hash with mu released, one hash at a time, as for a login.
	before := a.cred
	a.mu.Unlock()
	a.hashMu.Lock()
	hash, err := hashPassword(new, salt, webIterations)
	a.hashMu.Unlock()
	a.mu.Lock()
	if err != nil {
		return err
	}
	if a.cred != before {
		return api.Errorf(409, "the admin password changed meanwhile; try again")
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
	a.cred, a.loaded, a.sources = nc, true, nil
	d.log.Info("web admin password set", "by_root", privileged)
	return nil
}

// writeFileAtomic is fsutil.WriteFile: whole on disk before it replaces
// the old file.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	return fsutil.WriteFile(path, data, mode)
}
