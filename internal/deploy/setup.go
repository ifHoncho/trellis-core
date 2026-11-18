package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Setting nodes up for deployments from this computer (trellis-deploy
// --setup): the operator's SSH key on each, so later runs log in without a
// password, and sudo without a password for the login user, so they run
// without asking. sudo without a password means that whoever logs in as
// that user, with the key, is root on the node: the deployments need root
// anyway (they install packages), and the key is what guards it.

var reUser = regexp.MustCompile(`^[a-z_][a-z0-9_.-]{0,31}$`)

// SudoersFile is the rule --setup writes.
const SudoersFile = "/etc/sudoers.d/trellis-deploy"

// sudoRule is the rule for a login user.
func sudoRule(user string) (string, error) {
	if !reUser.MatchString(user) {
		return "", fmt.Errorf("%q is not a user name sudo takes here", user)
	}
	return user + " ALL=(ALL) NOPASSWD: ALL", nil
}

// GrantSudo writes the rule on the node, checked by visudo before it
// replaces anything (a broken file in sudoers.d would lock sudo out).
func (r *Runner) GrantSudo(ctx context.Context, s Step, w io.Writer) error {
	rule, err := sudoRule(s.State.User)
	if err != nil {
		return err
	}
	cmd := `sh -c 'umask 0337; f=` + SudoersFile + `; printf "%s\n" "$1" > "$f.new" && visudo -cf "$f.new" > /dev/null && mv "$f.new" "$f" || { rm -f "$f.new"; exit 1; }' sh ` + quote(rule)
	return r.sudo(ctx, s, cmd, "", w)
}

// CopyKey installs the operator's public key (pub; empty: ssh-copy-id's
// choice) on a node with ssh-copy-id, which asks for the login password on
// the terminal.
func (s *SSH) CopyKey(ctx context.Context, r Route, pub string) error {
	var args []string
	if pub != "" {
		args = append(args, "-i", pub)
	}
	if r.Port != 0 {
		args = append(args, "-p", fmt.Sprint(r.Port))
	}
	for _, o := range s.Options {
		args = append(args, "-o", o)
	}
	args = append(args, "-o", "StrictHostKeyChecking=accept-new")
	if r.Via != "" {
		args = append(args, "-o", "ProxyCommand="+s.proxy(r), "-o", "HostKeyAlias="+r.Alias)
	}
	if _, err := exec.LookPath("ssh-copy-id"); err != nil {
		return s.appendKey(ctx, r, pub)
	}
	c := exec.CommandContext(ctx, "ssh-copy-id", append(args, r.Target)...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

// appendKey does ssh-copy-id's job where there is none (Windows): the key
// goes to the node on ssh's input, and ssh asks for the login password on
// the console.
func (s *SSH) appendKey(ctx context.Context, r Route, pub string) error {
	if pub == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		for _, name := range []string{"id_ed25519.pub", "id_ecdsa.pub", "id_rsa.pub"} {
			if p := filepath.Join(home, ".ssh", name); fileExists(p) {
				pub = p
				break
			}
		}
		if pub == "" {
			return errors.New("no public key in your .ssh folder: make one with ssh-keygen, or name it with --ssh-key")
		}
	}
	key, err := os.ReadFile(pub)
	if err != nil {
		return err
	}
	key = []byte(strings.TrimSpace(string(key)) + "\n")
	var a []string
	for _, o := range s.Options {
		a = append(a, "-o", o)
	}
	a = append(a, "-o", "StrictHostKeyChecking=accept-new")
	if r.Port != 0 {
		a = append(a, "-p", fmt.Sprint(r.Port))
	}
	if r.Via != "" {
		a = append(a, "-o", "ProxyCommand="+s.proxy(r), "-o", "HostKeyAlias="+r.Alias)
	}
	// Added once: a key already there is not added again.
	cmd := `umask 077 && mkdir -p ~/.ssh && k=$(cat) && { grep -qxF "$k" ~/.ssh/authorized_keys 2>/dev/null || printf '%s\n' "$k" >> ~/.ssh/authorized_keys; }`
	c := exec.CommandContext(ctx, s.Binary, append(append(a, r.Target), cmd)...)
	c.Stdin, c.Stdout, c.Stderr = bytes.NewReader(key), os.Stdout, os.Stderr
	return c.Run()
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }
