package upgrade

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"meridian/trellis/internal/status"
)

// RunUnit is the transient systemd unit an upgrade runs in, so that it
// outlives whoever started it: the operator's session, or meshd and webd,
// which the upgrade itself restarts.
const RunUnit = "trellis-upgrade-run"

// DefaultDir is where an upgrade keeps its files on a node.
const DefaultDir = "/var/lib/trellis/upgrade"

// Incoming is the copy of the package a detached run installs: the file
// given may sit on a stick that is pulled, or in a temporary directory.
const Incoming = "incoming.deb"

// HostSystem is the real node: commands run here, and meshd's status comes
// from StatusFn.
type HostSystem struct {
	StatusFn func(ctx context.Context) (*status.Status, error)
}

func (HostSystem) Now() time.Time        { return time.Now() }
func (HostSystem) Sleep(d time.Duration) { time.Sleep(d) }

func (s HostSystem) Status(ctx context.Context) (*status.Status, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return s.StatusFn(ctx)
}

func (HostSystem) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive", "LC_ALL=C",
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(errb.String()); msg != "" {
			return out.String(), fmt.Errorf("%s: %s", name, msg)
		}
		return out.String(), fmt.Errorf("%s: %w", name, err)
	}
	return out.String(), nil
}

// UnitActive says whether an upgrade's unit, or the boot-time resume, is
// running now.
func UnitActive(ctx context.Context, sys System) bool {
	for _, unit := range []string{RunUnit + ".service", ResumeUnit} {
		out, _ := sys.Run(ctx, "systemctl", "is-active", unit)
		if s := strings.TrimSpace(out); s == "active" || s == "activating" {
			return true
		}
	}
	return false
}

// StartDetached copies pkg (nil for --resume and --rollback) to the upgrade
// directory and starts exe ("meshctl upgrade" args…, the package's path
// replaced by the copy) in RunUnit. The log is emptied first: it then holds
// this run only.
func StartDetached(ctx context.Context, sys System, dir, exe string, pkg []byte, args []string, file string) error {
	if UnitActive(ctx, sys) {
		return errors.New("an upgrade is running already (meshctl upgrade --status)")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if pkg != nil {
		incoming := filepath.Join(dir, Incoming)
		if err := writeFileSync(incoming, pkg, 0o600); err != nil {
			return err
		}
		replaced := false
		for i, a := range args {
			if a == file {
				args = append(append(append([]string(nil), args[:i]...), incoming), args[i+1:]...)
				replaced = true
				break
			}
		}
		if !replaced {
			args = append(append([]string(nil), args...), incoming)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "log"), nil, 0o600); err != nil {
		return err
	}
	cmd := append([]string{"--quiet", "--collect", "--unit=" + RunUnit,
		"--description=Trellis upgrade", exe, "meshctl", "upgrade"}, args...)
	if _, err := sys.Run(ctx, "systemd-run", cmd...); err != nil {
		return fmt.Errorf("cannot start the upgrade: %w", err)
	}
	return nil
}

// LogTail returns the last n lines of the upgrade's log.
func LogTail(dir string, n int) []string {
	f, err := os.Open(filepath.Join(dir, "log"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if len(lines) > n {
			lines = lines[1:]
		}
	}
	return lines
}
