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
	// The operator's options first: ssh keeps the first value it is given
	// for each, so they override the defaults below.
	var a []string
	for _, o := range s.Options {
		a = append(a, "-o", o)
	}
	// A device's host key is taken the first time it is seen (a new
	// device has never been logged in to), and refused if it changes;
	// StrictHostKeyChecking=yes as an option requires known hosts instead.
	a = append(a, "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new", "-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=10", "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=4")
	if s.ControlDir != "" {
		a = append(a, "-o", "ControlMaster=auto", "-o", "ControlPath="+filepath.Join(s.ControlDir, "%C"), "-o", "ControlPersist=120")
	}
	if r.Port != 0 {
		a = append(a, "-p", fmt.Sprint(r.Port))
	}
	if r.Via != "" {
		a = append(a, "-o", "ProxyCommand="+s.proxy(r), "-o", "HostKeyAlias="+r.Alias)
	}
	return append(a, r.Target)
}

// proxy is the command that carries a connection through the node r goes
// via. A ProxyCommand rather than -J: ssh gives a -J hop none of the -o
// options, so the operator's key and the batch settings would not reach
// it. An operator's own ProxyCommand (--ssh-option) comes first and wins.
func (s *SSH) proxy(r Route) string {
	// ssh runs a ProxyCommand with the system's shell: sh, or cmd.exe on
	// Windows.
	q := quote
	if proxyCmdExe {
		q = quoteCmd
	}
	parts := []string{q(s.Binary)}
	for _, o := range s.Options {
		// ssh expands % tokens in a ProxyCommand: the option's own must
		// reach the inner ssh as they were written.
		parts = append(parts, "-o", q(strings.ReplaceAll(o, "%", "%%")))
	}
	parts = append(parts, "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new", "-o", "LogLevel=ERROR", "-o", "ConnectTimeout=10")
	if r.ViaPort != 0 {
		parts = append(parts, "-p", fmt.Sprint(r.ViaPort))
	}
	return strings.Join(append(parts, "-W", "%h:%p", q(r.Via)), " ")
}

// proxyCmdExe: ssh runs a ProxyCommand with cmd.exe (Windows' OpenSSH).
var proxyCmdExe = runtime.GOOS == "windows"

// quoteCmd makes s one argument on a cmd.exe command line, as Windows
// programs split it: in double quotes, a quote inside escaped with a
// backslash.
func quoteCmd(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"&|<>^()") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for _, c := range s {
		switch c {
		case '\\':
			slashes++
			continue
		case '"':
			b.WriteString(strings.Repeat(`\`, 2*slashes+1))
			b.WriteByte('"')
		default:
			b.WriteString(strings.Repeat(`\`, slashes))
			b.WriteRune(c)
		}
		slashes = 0
	}
	b.WriteString(strings.Repeat(`\`, 2*slashes))
	b.WriteByte('"')
	return b.String()
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
