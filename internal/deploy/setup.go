package deploy

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
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
	if r.Via != "" {
		via := r.Via
		if r.ViaPort != 0 {
			via += fmt.Sprintf(":%d", r.ViaPort)
		}
		args = append(args, "-o", "ProxyJump="+via, "-o", "HostKeyAlias="+r.Alias)
	}
	c := exec.CommandContext(ctx, "ssh-copy-id", append(args, r.Target)...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}
