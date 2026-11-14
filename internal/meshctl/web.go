package meshctl

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"

	"meridian/trellis/internal/api"
)

// cmdWeb is "meshctl web": the web UI's admin password and where the UI
// answers. It goes through meshd's API like everything else; setting a
// password without knowing the current one works because meshd sees that
// the caller on its socket is root.
func cmdWeb(ctx context.Context, c *api.Client, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "status" {
		return webStatus(ctx, c, stdout)
	}
	switch args[0] {
	case "password":
		return webPassword(ctx, c, args[1:], stdout, stderr)
	case "wan-access":
		if len(args) != 2 || (args[1] != "on" && args[1] != "off") {
			return errors.New("usage: meshctl web wan-access on|off")
		}
		return webWANAccess(ctx, c, args[1] == "on", stdout)
	}
	return errors.New("usage: meshctl web [status] | web password [--stdin|--clear] | web wan-access on|off")
}

func webStatus(ctx context.Context, c *api.Client, stdout io.Writer) error {
	wi, err := c.WebInfo(ctx)
	if err != nil {
		return err
	}
	st, err := c.Status(ctx)
	if err != nil {
		return err
	}
	addr, _, _ := strings.Cut(st.Node.Address, "/")
	fmt.Fprintf(stdout, "Web UI of %s\n", wi.Callsign)
	fmt.Fprintf(stdout, "  from its own devices: https://%s  or  https://node.mesh\n", st.ClientNet.Anycast.Address)
	fmt.Fprintf(stdout, "  from the mesh:        https://%s  or  https://%s.mesh\n", addr, wi.Callsign)
	fmt.Fprintln(stdout, "  The addresses always work. The .mesh names need the device to ask this node's DNS: a phone with")
	fmt.Fprintln(stdout, "  Private DNS set to a provider, a browser's secure DNS, or a VPN asks elsewhere and finds no such name.")
	if wi.WANAccess {
		fmt.Fprintln(stdout, "  from its uplink network too (web.wan_access is on): https://<the node's address there>")
	} else {
		fmt.Fprintln(stdout, "  not from its uplink network (meshctl web wan-access on, to allow that)")
	}
	switch {
	case wi.AdminSet:
		fmt.Fprintln(stdout, "  admin password: set")
	case wi.Setup:
		fmt.Fprintln(stdout, "  admin password: not set; the setup page asks for one (the node waits to be set up)")
	default:
		fmt.Fprintln(stdout, "  admin password: NOT SET, so nobody can sign in. Set one: sudo meshctl web password")
	}
	fmt.Fprintln(stdout, "The certificate is the node's own: a browser warns the first time.")
	return nil
}

func webPassword(ctx context.Context, c *api.Client, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("web password", flag.ContinueOnError)
	fromStdin := fs.Bool("stdin", false, "read the new password from standard input (one line), for scripts")
	clear := fs.Bool("clear", false, "remove the admin password: nobody can sign in until a new one is set")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var pw string
	switch {
	case *clear:
	case *fromStdin:
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return errors.New("no password on standard input")
		}
		pw = strings.TrimRight(line, "\r\n")
	default:
		first, err := promptHidden(stderr, "New admin password for the web UI: ")
		if err != nil {
			return err
		}
		again, err := promptHidden(stderr, "Again: ")
		if err != nil {
			return err
		}
		if first != again {
			return errors.New("the two passwords differ; nothing changed")
		}
		pw = first
	}
	if pw == "" && !*clear {
		return errors.New("the password is empty; nothing changed (use --clear to remove it)")
	}
	if err := c.WebSetPassword(ctx, "", pw); err != nil {
		var ae *api.Error
		if errors.As(err, &ae) && (ae.Status == 401 || ae.Status == 403) {
			return fmt.Errorf("%w (run meshctl as root: only root sets the password without the current one)", err)
		}
		return err
	}
	if *clear {
		fmt.Fprintln(stdout, "admin password removed")
	} else {
		fmt.Fprintln(stdout, "admin password set; browsers that were signed in must sign in again")
	}
	return nil
}

// promptHidden reads a line from the terminal without echoing it.
func promptHidden(prompt io.Writer, text string) (string, error) {
	fd := int(os.Stdin.Fd())
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return "", errors.New("standard input is not a terminal: use --stdin to pass the password")
	}
	fmt.Fprint(prompt, text)
	quiet := *old
	quiet.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &quiet); err != nil {
		return "", err
	}
	defer func() {
		_ = unix.IoctlSetTermios(fd, unix.TCSETS, old)
		fmt.Fprintln(prompt)
	}()
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func webWANAccess(ctx context.Context, c *api.Client, on bool, stdout io.Writer) error {
	cfg, err := c.Config(ctx)
	if err != nil {
		return err
	}
	node := cfg.Node // secrets come redacted; sent back so, they are kept
	node.Web.WANAccess = on
	res, err := c.SetNode(ctx, node, false)
	if err != nil {
		return err
	}
	if !res.Changed {
		fmt.Fprintf(stdout, "web.wan_access is already %v\n", on)
		return nil
	}
	fmt.Fprintf(stdout, "web.wan_access: %v (written to the node config and applied)\n", on)
	for _, e := range res.Effects {
		fmt.Fprintf(stdout, "  [%s] %s\n", e.Level, e.Message)
	}
	return nil
}
