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
	"runtime"
	"strings"
)

// SSH runs commands on nodes with the system's ssh client.
type SSH struct {
	// Binary is the ssh program ("ssh").
	Binary string
	// ControlDir holds the shared connections (ssh's ControlMaster); empty
	// for none (Windows' OpenSSH has no connection sharing).
	ControlDir string
	// Options are more ssh options, as "-o Name=value" pairs' values.
	Options []string
}

// NewSSH makes the runner, with a directory for shared connections.
func NewSSH(options []string) (*SSH, error) {
	s := &SSH{Binary: "ssh", Options: options}
	if runtime.GOOS != "windows" {
		// Short: a control socket's path is limited to about 100 bytes.
		dir, err := os.MkdirTemp("/tmp", "tdssh-")
		if err != nil {
			return nil, err
		}
		s.ControlDir = dir
	}
	return s, nil
}

// Close ends the shared connections.
func (s *SSH) Close() {
	if s.ControlDir != "" {
		_ = os.RemoveAll(s.ControlDir)
	}
}

// Route is how a node is reached: directly, or through another node of
// the list at its mesh address.
type Route struct {
	Target string // user@address for ssh
	Via    string // user@host of the node it goes through; empty: direct
	Alias  string // the host key's name when Via is set: the node's own address
	Port   int    // the node's ssh port; 0: the default
	// ViaPort is the port of the node it goes through.
	ViaPort int
}

func (s *SSH) args(r Route) []string {
	a := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=4"}
	if s.ControlDir != "" {
		a = append(a, "-o", "ControlMaster=auto", "-o", "ControlPath="+filepath.Join(s.ControlDir, "%C"), "-o", "ControlPersist=120")
	}
	for _, o := range s.Options {
		a = append(a, "-o", o)
	}
	if r.Port != 0 {
		a = append(a, "-p", fmt.Sprint(r.Port))
	}
	if r.Via != "" {
		via := r.Via
		if r.ViaPort != 0 {
			via += fmt.Sprintf(":%d", r.ViaPort)
		}
		a = append(a, "-J", via, "-o", "HostKeyAlias="+r.Alias)
	}
	return append(a, r.Target)
}

// Run runs a shell command on the node, with stdin, writing its output to
// out. An error means ssh failed (exit 255) or the command did.
func (s *SSH) Run(ctx context.Context, r Route, cmd string, stdin io.Reader, out io.Writer) error {
	c := exec.CommandContext(ctx, s.Binary, append(s.args(r), cmd)...)
	c.Stdin, c.Stdout, c.Stderr = stdin, out, out
	return c.Run()
}

// Output runs a command quietly and returns what it printed.
func (s *SSH) Output(ctx context.Context, r Route, cmd string) (string, error) {
	var b bytes.Buffer
	err := s.Run(ctx, r, cmd, nil, &b)
	return b.String(), err
}

// Put copies a local file into dir on the node (made private to the user),
// under name.
func (s *SSH) Put(ctx context.Context, r Route, local, dir, name string) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()
	return s.PutBytes(ctx, r, f, dir, name)
}

// PutBytes writes data to dir/name on the node, readable by the user only.
func (s *SSH) PutBytes(ctx context.Context, r Route, data io.Reader, dir, name string) error {
	var errb bytes.Buffer
	cmd := fmt.Sprintf("umask 077 && mkdir -p %s && cat > %s/%s", dir, dir, quote(name))
	if err := s.Run(ctx, r, cmd, data, &errb); err != nil {
		return fmt.Errorf("copying %s: %v %s", name, err, strings.TrimSpace(errb.String()))
	}
	return nil
}

// Disconnect ends the shared connection to a node: after its reboot the
// old one is dead.
func (s *SSH) Disconnect(r Route) {
	if s.ControlDir == "" {
		return
	}
	a := append(s.args(r)[:len(s.args(r))-1], "-O", "exit", r.Target)
	_ = exec.Command(s.Binary, a...).Run()
}

// Remote runs commands on nodes: SSH, or a stand-in in tests.
type Remote interface {
	Run(ctx context.Context, r Route, cmd string, stdin io.Reader, out io.Writer) error
	Output(ctx context.Context, r Route, cmd string) (string, error)
	Put(ctx context.Context, r Route, local, dir, name string) error
	PutBytes(ctx context.Context, r Route, data io.Reader, dir, name string) error
	Disconnect(r Route)
}

// Unreachable reports whether an error is ssh's own (exit 255): no
// connection, or the connection dropped.
func Unreachable(err error) bool {
	var ee interface{ ExitCode() int }
	return errors.As(err, &ee) && ee.ExitCode() == 255
}

// quote makes s one word for a POSIX shell.
func quote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("@%+=:,./_-", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
