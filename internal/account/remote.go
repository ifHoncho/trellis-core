package account

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Service is what changes the operator account: the machine itself
// (*System), or a helper outside the caller's sandbox (Remote).
type Service interface {
	Get(ctx context.Context) (*State, error)
	Check(ctx context.Context, ch Change) error
	Apply(ctx context.Context, ch Change) error
}

// InvalidError is a change that is refused as it stands (a missing key, a
// short password), as opposed to a failure of the machine.
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

func invalid(format string, args ...any) error {
	return &InvalidError{Msg: fmt.Sprintf(format, args...)}
}

// Remote runs the account helper (trellis account …) as a transient
// systemd unit. meshd is sandboxed: /etc read-only, /home hidden, few
// capabilities (docs/security.md). Accounts are changed outside that, one
// short unit per request, as upgrades are (trellis-upgrade-run). The
// change travels on stdin, the answer on stdout: never on a command line.
type Remote struct {
	Binary string // the trellis binary; default /usr/bin/trellis
}

// Exit status of the helper for a refused change.
const exitInvalid = 2

func (r *Remote) run(ctx context.Context, op string, in, out any) error {
	bin := r.Binary
	if bin == "" {
		bin = "/usr/bin/trellis"
	}
	var stdin []byte
	if in != nil {
		var err error
		if stdin, err = json.Marshal(in); err != nil {
			return err
		}
	}
	id := make([]byte, 4)
	_, _ = rand.Read(id)
	cmd := exec.CommandContext(ctx, "systemd-run", "--quiet", "--pipe", "--wait", "--collect",
		"--service-type=exec", "--unit=trellis-account-"+hex.EncodeToString(id),
		"--description=Trellis: operator account", bin, "account", op)
	cmd.Stdin = bytes.NewReader(stdin)
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	msg := strings.TrimSpace(se.String())
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == exitInvalid {
		return &InvalidError{Msg: msg}
	}
	if err != nil {
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("account helper: %s", msg)
	}
	if out != nil {
		return json.Unmarshal(so.Bytes(), out)
	}
	return nil
}

func (r *Remote) Get(ctx context.Context) (*State, error) {
	var st State
	if err := r.run(ctx, "get", nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func (r *Remote) Check(ctx context.Context, ch Change) error { return r.run(ctx, "check", ch, nil) }
func (r *Remote) Apply(ctx context.Context, ch Change) error { return r.run(ctx, "apply", ch, nil) }

// Helper is the other end: trellis account get|check|apply, run as root,
// reading a Change on stdin. A refused change exits 2 with the reason on
// stderr; any other failure exits 1.
func Helper(ctx context.Context, op string, stdin io.Reader, stdout, stderr io.Writer) int {
	fail := func(err error) int {
		fmt.Fprintln(stderr, err)
		var inv *InvalidError
		if errors.As(err, &inv) {
			return exitInvalid
		}
		return 1
	}
	if os.Geteuid() != 0 {
		return fail(errors.New("trellis account: run as root (meshd runs it)"))
	}
	s := Host()
	switch op {
	case "get":
		st, err := s.Get(ctx)
		if err != nil {
			return fail(err)
		}
		if err := json.NewEncoder(stdout).Encode(st); err != nil {
			return fail(err)
		}
		return 0
	case "check", "apply":
		var ch Change
		if err := json.NewDecoder(io.LimitReader(stdin, 1<<20)).Decode(&ch); err != nil {
			return fail(fmt.Errorf("trellis account %s: reading the change: %v", op, err))
		}
		if op == "check" {
			if err := s.Check(ctx, ch); err != nil {
				return fail(err)
			}
			return 0
		}
		if err := s.Apply(ctx, ch); err != nil {
			return fail(err)
		}
		return 0
	}
	return fail(fmt.Errorf("trellis account: get, check or apply (got %q)", op))
}
