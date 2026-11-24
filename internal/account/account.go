// Package account manages the node's operator account: a Linux user
// named trellis, whose primary group is the trellis group that may use
// meshd's API (meshctl without sudo), in the sudo group (sudo asks for the
// password). SSH is always on; the account's password, its SSH keys and
// whether it may log in with a password at all are set here, from the
// setup wizard, Settings → Access, or a card (trellis firstboot).
//
// authorized_keys is the record of the keys: what an operator adds there
// by hand is kept and shown. Password authentication is on unless turned
// off (KeyOnly), which needs a key: an sshd drop-in that applies to this
// account alone.
package account

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"meridian/trellis/internal/fsutil"
)

// The account and its files.
const (
	Name      = "trellis"
	Group     = "trellis"
	SSHDropIn = "/etc/ssh/sshd_config.d/50-trellis.conf"
)

// System is the machine the account lives on: a root for its files ("" is
// /), and how commands run. Tests give both.
type System struct {
	Root  string
	Run   func(ctx context.Context, stdin string, name string, args ...string) ([]byte, error)
	Chown func(path string, uid, gid int) error
}

// Host is the machine this runs on.
func Host() *System {
	return &System{Run: run, Chown: os.Lchown}
}

func run(ctx context.Context, stdin, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s: %v: %s", name, err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func (s *System) path(p string) string { return filepath.Join(s.Root, p) }

// SSH server states.
const (
	SSHActive       = "active"
	SSHInactive     = "inactive"
	SSHNotInstalled = "not-installed"
)

// State is the account as it is.
type State struct {
	Name     string `json:"name"`
	Exists   bool   `json:"exists"`
	Password bool   `json:"password"` // a password is set (and not locked)
	Keys     []Key  `json:"keys"`
	KeyOnly  bool   `json:"key_only"`
	SSH      string `json:"ssh"` // active, inactive, not-installed
}

// Change is what to set; empty or nil fields stay as they are.
type Change struct {
	Password     string `json:"password,omitempty"`      // in the clear: the system hashes it
	PasswordHash string `json:"password_hash,omitempty"` // a crypt hash ($6$…), from a card
	Keys         *[]Key `json:"keys,omitempty"`          // the whole list
	KeyOnly      *bool  `json:"key_only,omitempty"`
}

type passwdEntry struct {
	uid, gid int
	home     string
}

func (s *System) lookup() (*passwdEntry, error) {
	f, err := os.Open(s.path("/etc/passwd"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		p := strings.Split(sc.Text(), ":")
		if len(p) < 7 || p[0] != Name {
			continue
		}
		uid, err1 := strconv.Atoi(p[2])
		gid, err2 := strconv.Atoi(p[3])
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("/etc/passwd: %s: bad uid or gid", Name)
		}
		return &passwdEntry{uid: uid, gid: gid, home: p[5]}, nil
	}
	return nil, sc.Err()
}

func (s *System) hasGroup(name string) bool {
	data, err := os.ReadFile(s.path("/etc/group"))
	if err != nil {
		return false
	}
	for _, l := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(l, name+":") {
			return true
		}
	}
	return false
}

func (s *System) authorizedKeys(e *passwdEntry) string {
	return s.path(filepath.Join(e.home, ".ssh", "authorized_keys"))
}

// Get reads the account's state.
func (s *System) Get(ctx context.Context) (*State, error) {
	st := &State{Name: Name, Keys: []Key{}, SSH: s.sshState(ctx)}
	e, err := s.lookup()
	if err != nil {
		return nil, err
	}
	if e == nil {
		return st, nil
	}
	st.Exists = true
	if data, err := os.ReadFile(s.path("/etc/shadow")); err == nil {
		for _, l := range strings.Split(string(data), "\n") {
			p := strings.Split(l, ":")
			if len(p) > 1 && p[0] == Name {
				st.Password = p[1] != "" && !strings.HasPrefix(p[1], "!") && !strings.HasPrefix(p[1], "*")
			}
		}
	}
	if data, err := os.ReadFile(s.authorizedKeys(e)); err == nil {
		for _, l := range strings.Split(string(data), "\n") {
			l = strings.TrimSpace(l)
			if l == "" || strings.HasPrefix(l, "#") {
				continue
			}
			if k, err := ParseKey(l); err == nil {
				st.Keys = append(st.Keys, k)
			}
		}
	}
	st.KeyOnly = s.keyOnly()
	return st, nil
}

func (s *System) keyOnly() bool {
	data, err := os.ReadFile(s.path(SSHDropIn))
	return err == nil && bytes.Contains(data, []byte("PasswordAuthentication no"))
}

func (s *System) sshState(ctx context.Context) string {
	if _, err := os.Stat(s.path("/usr/sbin/sshd")); err != nil {
		return SSHNotInstalled
	}
	for _, u := range []string{"ssh.service", "ssh.socket"} {
		if out, _ := s.Run(ctx, "", "systemctl", "is-active", u); strings.TrimSpace(string(out)) == "active" {
			return SSHActive
		}
	}
	return SSHInactive
}

// Check validates a change against the account as it is, without applying it.
func (s *System) Check(ctx context.Context, ch Change) error {
	if ch.Password != "" {
		if err := CheckPassword(ch.Password); err != nil {
			return invalid("%v", err)
		}
	}
	if ch.PasswordHash != "" && !IsHash(ch.PasswordHash) {
		return invalid("password hash: a crypt hash ($6$…) expected")
	}
	if ch.Keys != nil {
		for i, k := range *ch.Keys {
			if _, err := ParseKey(k.Line()); err != nil {
				return invalid("key %d: %v", i+1, err)
			}
		}
	}
	cur, err := s.Get(ctx)
	if err != nil {
		return err
	}
	keys, keyOnly := cur.Keys, cur.KeyOnly
	if ch.Keys != nil {
		keys = *ch.Keys
	}
	if ch.KeyOnly != nil {
		keyOnly = *ch.KeyOnly
	}
	if keyOnly && len(keys) == 0 {
		return invalid("password authentication: off requires an SSH key")
	}
	return nil
}

// Apply makes the account as the change says, creating it if needed, and
// sees that SSH runs.
func (s *System) Apply(ctx context.Context, ch Change) error {
	if err := s.Check(ctx, ch); err != nil {
		return err
	}
	e, err := s.lookup()
	if err != nil {
		return err
	}
	if e == nil {
		if !s.hasGroup(Group) {
			return fmt.Errorf("group %s missing (the trellis package makes it)", Group)
		}
		args := []string{"--create-home", "--gid", Group, "--shell", "/bin/bash", "--comment", "Trellis operator"}
		for _, g := range []string{"sudo", "wheel"} {
			if s.hasGroup(g) {
				args = append(args, "--groups", g)
				break
			}
		}
		if _, err := s.Run(ctx, "", "useradd", append(args, Name)...); err != nil {
			return err
		}
		if e, err = s.lookup(); err != nil || e == nil {
			return fmt.Errorf("account %s not found after useradd: %v", Name, err)
		}
	}
	switch {
	case ch.Password != "":
		if _, err := s.Run(ctx, Name+":"+ch.Password+"\n", "chpasswd"); err != nil {
			return errors.New("chpasswd failed") // the error would repeat the password
		}
	case ch.PasswordHash != "":
		if _, err := s.Run(ctx, Name+":"+ch.PasswordHash+"\n", "chpasswd", "-e"); err != nil {
			return errors.New("chpasswd -e failed")
		}
	}
	if ch.Keys != nil {
		if err := s.writeKeys(e, *ch.Keys); err != nil {
			return err
		}
	}
	if ch.KeyOnly != nil && *ch.KeyOnly != s.keyOnly() {
		if err := s.setKeyOnly(ctx, *ch.KeyOnly); err != nil {
			return err
		}
	}
	return s.ensureSSH(ctx)
}

const keysHeader = "# The trellis account's SSH keys (Trellis: Settings → Access)."

func (s *System) writeKeys(e *passwdEntry, keys []Key) error {
	dir := filepath.Dir(s.authorizedKeys(e))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	path := s.authorizedKeys(e)
	var b strings.Builder
	b.WriteString(keysHeader + "\n")
	for _, k := range keys {
		b.WriteString(k.Line() + "\n")
	}
	// Lines that are not keys Trellis reads (an operator's comments,
	// certificates, other key types) are not in the list: they stay as
	// they are.
	if data, err := os.ReadFile(path); err == nil {
		for _, l := range strings.Split(string(data), "\n") {
			t := strings.TrimSpace(l)
			if t == "" || t == keysHeader {
				continue
			}
			if _, err := ParseKey(t); err != nil {
				b.WriteString(l + "\n")
			}
		}
	}
	if err := fsutil.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return err
	}
	for _, p := range []string{dir, path} {
		if err := s.Chown(p, e.uid, e.gid); err != nil {
			return err
		}
	}
	return nil
}

const keyOnlyConf = `# Written by Trellis (Settings → Access): password authentication off
# for the trellis account. Other accounts are not affected.
Match User trellis
	PasswordAuthentication no
	KbdInteractiveAuthentication no
Match all
`

func (s *System) setKeyOnly(ctx context.Context, on bool) error {
	path := s.path(SSHDropIn)
	old, hadOld := []byte(nil), false
	if data, err := os.ReadFile(path); err == nil {
		old, hadOld = data, true
	}
	if on {
		if err := fsutil.WriteFile(path, []byte(keyOnlyConf), 0o644); err != nil {
			return err
		}
	} else if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// sshd refuses to start on a bad file: check before it reloads.
	if _, err := s.Run(ctx, "", "sshd", "-t"); err != nil {
		if hadOld {
			_ = fsutil.WriteFile(path, old, 0o644)
		} else {
			_ = os.Remove(path)
		}
		return fmt.Errorf("sshd rejects the setting: %v", err)
	}
	if s.sshState(ctx) == SSHActive {
		if _, err := s.Run(ctx, "", "systemctl", "--no-block", "try-reload-or-restart", "ssh.service"); err != nil {
			return err
		}
	}
	return nil
}

// ensureSSH starts the SSH server where it is installed. Where it is not
// (a stock system without openssh-server), the account still works on the
// console, and its state says SSH: not-installed. Where systemd does not
// run (a chroot, an image being made), it is only enabled: it starts at
// the next boot.
func (s *System) ensureSSH(ctx context.Context) error {
	switch s.sshState(ctx) {
	case SSHActive, SSHNotInstalled:
		return nil
	}
	if _, err := os.Stat(s.path("/run/systemd/system")); err != nil {
		_, err := s.Run(ctx, "", "systemctl", "enable", "ssh.service")
		return err
	}
	// Queued, not waited for: at boot (trellis firstboot) ssh waits for the
	// network, which waits for firstboot.
	_, err := s.Run(ctx, "", "systemctl", "enable", "--now", "--no-block", "ssh.service")
	return err
}
