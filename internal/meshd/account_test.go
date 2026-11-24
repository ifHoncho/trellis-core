package meshd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"meridian/trellis/internal/account"
	"meridian/trellis/internal/api"
	"meridian/trellis/internal/config"
	"meridian/trellis/internal/status"
)

const testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAILhNVh+5m5DDjNfMtEp20Yqp3LnpdpNB3trND13c2OoB op@laptop"

// fakeAccountSystem is a machine in a directory whose commands are only
// recorded (useradd adds the passwd line).
func fakeAccountSystem(t *testing.T) (*account.System, *[]string) {
	root := t.TempDir()
	for p, c := range map[string]string{
		"etc/passwd":    "root:x:0:0:root:/root:/bin/bash\n",
		"etc/group":     "sudo:x:27:\ntrellis:x:998:\n",
		"etc/shadow":    "root:*:1:0:99999:7:::\n",
		"usr/sbin/sshd": "",
	} {
		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, p), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var cmds []string // what changes the machine: reads are not recorded
	run := func(ctx context.Context, stdin, name string, args ...string) ([]byte, error) {
		if name == "systemctl" && args[0] == "is-active" {
			return []byte("inactive\n"), nil
		}
		cmds = append(cmds, strings.TrimSpace(name+" "+strings.Join(args, " ")))
		if name == "useradd" {
			f, err := os.OpenFile(filepath.Join(root, "etc/passwd"), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				return nil, err
			}
			defer f.Close()
			_, err = f.WriteString("trellis:x:1001:998::/home/trellis:/bin/bash\n")
			return nil, err
		}
		return nil, nil
	}
	return &account.System{Root: root, Run: run, Chown: func(string, int, int) error { return nil }}, &cmds
}

func TestSetupAccess(t *testing.T) {
	d, dir := testDaemon(t, nil)
	sys, cmds := fakeAccountSystem(t)
	d.opts.Account = sys
	ctx := context.Background()
	on, off := true, false
	in := api.SetupInput{Callsign: "kit-1", ProfileInput: api.ProfileInput{New: &api.NewProfile{Name: "alpha", Band: "5g"}},
		WebUplink: &on, SSHUplink: &off}

	// A key that does not parse: refused, nothing written or run.
	bad := in
	bad.Account = &api.AccountInput{Password: "correct-horse", Keys: &[]string{"not a key"}}
	if _, err := d.Setup(ctx, bad, false); err == nil || !strings.Contains(err.Error(), "key 1") {
		t.Fatalf("bad key: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "node.yaml")); err == nil || len(*cmds) > 0 {
		t.Fatalf("a refused setup wrote or ran something: %v", *cmds)
	}
	// Key login only without a key: refused by the account's check.
	bad.Account = &api.AccountInput{Password: "correct-horse", KeyOnly: &on}
	if _, err := d.Setup(ctx, bad, false); err == nil {
		t.Fatal("key login only without a key was accepted")
	}

	in.Account = &api.AccountInput{Password: "correct-horse", Keys: &[]string{testKey, testKey}}
	if res, err := d.Setup(ctx, in, true); err != nil || res.Applied || len(*cmds) > 0 {
		t.Fatalf("dry run: %v %v", err, *cmds)
	}
	if _, err := d.Setup(ctx, in, false); err != nil {
		t.Fatal(err)
	}
	n, err := config.LoadNode(filepath.Join(dir, "node.yaml"))
	if err != nil || !n.Web.WANAccess || n.Firewall.SSHOpen() {
		t.Fatalf("uplink switches not in node.yaml: %+v %v", n, err)
	}
	joined := strings.Join(*cmds, "\n")
	if !strings.Contains(joined, "useradd") || !strings.Contains(joined, "chpasswd") || strings.Contains(joined, "correct-horse") {
		t.Fatalf("commands: %s", joined)
	}
	st, err := d.Account(ctx)
	if err != nil || !st.Exists || len(st.Keys) != 1 {
		t.Fatalf("account after setup (one key, the duplicate merged): %+v %v", st, err)
	}
}

func TestAccountOffInLabs(t *testing.T) {
	d, _ := testDaemon(t, nil)
	d.opts.NoHostConfig = true
	if _, err := d.Account(context.Background()); err == nil {
		t.Fatal("a lab meshd reads the host's accounts")
	}
	if _, err := d.SetAccount(context.Background(), api.AccountInput{Password: "correct-horse"}, false); err == nil {
		t.Fatal("a lab meshd changes the host's accounts")
	}
}

func TestSetupEffects(t *testing.T) {
	in := []status.Warning{
		{Level: "info", Code: "joins-mesh"}, {Level: "warning", Code: "ap-credentials"},
		{Level: "warning", Code: "mesh-link-moves"}, {Level: "warning", Code: "ap-moves"},
		{Level: "warning", Code: "regulatory"}, {Level: "error", Code: "leaves-mesh"}, {Level: "info", Code: "ap-up"},
	}
	var got []string
	for _, e := range setupEffects(in) {
		got = append(got, e.Code)
	}
	if strings.Join(got, ",") != "regulatory,leaves-mesh" {
		t.Fatalf("kept %v", got)
	}
}
