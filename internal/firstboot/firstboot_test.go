package firstboot

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"meridian/trellis/internal/account"
)

const profile = `schema: 0
name: alpha
sae_passphrase: correct-horse-battery
regulatory:
  mode: country
  country: us
radio:
  band: 2g
  channel: 1
`

const node = `schema: 0
callsign: relay-1
`

const machineIDText = "0123456789abcdef0123456789abcdef\n"

type env struct {
	t        *testing.T
	root     string
	o        Options
	hostname string
	reloads  int
	loaded   bool
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	e := &env{t: t, root: root, hostname: "trellis", loaded: true}
	p := func(rel string) string { return filepath.Join(root, rel) }
	e.o = Options{
		CardDir:      p("boot/trellis"),
		EtcDir:       p("etc/trellis"),
		StateDir:     p("var/lib/trellis"),
		FirmwareDir:  p("lib/firmware/morse"),
		PinsFile:     p("share/firmware.pins"),
		ModprobeConf: p("etc/modprobe.d/trellis-morse.conf"),
		MachineID:    p("etc/machine-id"),
		ImageRelease: p("usr/lib/trellis/image-release"),
		HostnameFile: p("etc/hostname"),
		HostsFile:    p("etc/hosts"),
		Hostname:     func() (string, error) { return e.hostname, nil },
		SetHostname:  func(n string) error { e.hostname = n; return nil },
		ModuleLoaded: func(string) bool { return e.loaded },
		ReloadModule: func(string) error { e.reloads++; return nil },
		Logf:         t.Logf,
	}
	e.write("etc/machine-id", machineIDText)
	return e
}

func (e *env) write(rel, content string) string {
	e.t.Helper()
	path := filepath.Join(e.root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
	return path
}

func (e *env) read(rel string) string {
	e.t.Helper()
	data, err := os.ReadFile(filepath.Join(e.root, rel))
	if err != nil {
		e.t.Fatal(err)
	}
	return string(data)
}

func (e *env) gone(rel string) bool {
	_, err := os.Lstat(filepath.Join(e.root, rel))
	return errors.Is(err, os.ErrNotExist)
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func credential(hashLen int) string {
	return fmt.Sprintf("algo: pbkdf2-sha256\niterations: 200000\nsalt: %s\nhash: %s\nset_at: 2026-11-22T00:00:00Z\n",
		base64.StdEncoding.EncodeToString(make([]byte, 16)), base64.StdEncoding.EncodeToString(make([]byte, hashLen)))
}

func TestNothingToDo(t *testing.T) {
	e := newEnv(t)
	res, err := Run(e.o)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Installed) != 0 || len(res.Rejected) != 0 || res.Hostname != "" || res.MACSuffix != "" || res.Reloaded {
		t.Fatalf("did something: %+v", res)
	}
	if e.hostname != "trellis" {
		t.Fatalf("hostname changed off an image: %s", e.hostname)
	}
}

func TestCardInstalled(t *testing.T) {
	e := newEnv(t)
	e.write("boot/trellis/profile.yaml", profile)
	e.write("boot/trellis/node.yaml", node)
	e.write("boot/trellis/web-admin.yaml", credential(32))
	res, err := Run(e.o)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rejected) != 0 {
		t.Fatalf("rejected: %v", res.Rejected)
	}
	if got := strings.Join(res.Installed, ","); got != "node.yaml,profile.yaml,web-admin.yaml" {
		t.Fatalf("installed %s", got)
	}
	if e.read("etc/trellis/profile.yaml") != profile || e.read("etc/trellis/node.yaml") != node {
		t.Fatal("installed files differ from the card's")
	}
	for rel, mode := range map[string]os.FileMode{"etc/trellis/profile.yaml": 0o600, "var/lib/trellis/web-admin.yaml": 0o600, "etc/trellis/node.yaml": 0o644} {
		st, err := os.Stat(filepath.Join(e.root, rel))
		if err != nil || st.Mode().Perm() != mode {
			t.Fatalf("%s: %v %v, want %v", rel, st.Mode().Perm(), err, mode)
		}
	}
	for _, f := range []string{"profile.yaml", "node.yaml", "web-admin.yaml"} {
		if !e.gone("boot/trellis/" + f) {
			t.Fatalf("%s left on the card", f)
		}
	}
	report := e.read("boot/trellis/" + ReportFile)
	if !strings.Contains(report, "profile.yaml") || strings.Contains(report, "correct-horse") {
		t.Fatalf("report: %q", report)
	}
}

func TestCardRejected(t *testing.T) {
	e := newEnv(t)
	e.write("etc/trellis/profile.yaml", "old")
	e.write("boot/trellis/profile.yaml", "schema: 0\nname: alpha\n") // no passphrase
	e.write("boot/trellis/web-admin.yaml", credential(16))
	res, err := Run(e.o)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Installed) != 0 || len(res.Rejected) != 2 {
		t.Fatalf("%+v", res)
	}
	if e.read("etc/trellis/profile.yaml") != "old" {
		t.Fatal("a rejected profile replaced the node's")
	}
	if e.gone("boot/trellis/profile.yaml") || e.gone("boot/trellis/web-admin.yaml") {
		t.Fatal("a rejected file was removed from the card")
	}
	if r := e.read("boot/trellis/" + ReportFile); !strings.Contains(r, "Not installed") || !strings.Contains(r, "hash:") {
		t.Fatalf("report: %q", r)
	}
}

func TestProfileFollowsNodePath(t *testing.T) {
	e := newEnv(t)
	other := filepath.Join(e.root, "srv/mesh.yaml")
	e.write("boot/trellis/node.yaml", node+"profile: "+other+"\n")
	e.write("boot/trellis/profile.yaml", profile)
	if _, err := Run(e.o); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(other); err != nil || string(data) != profile {
		t.Fatalf("profile not at the node's path: %v", err)
	}
}

func TestMorseFiles(t *testing.T) {
	e := newEnv(t)
	fw, bcf := "firmware bytes", "bcf bytes"
	e.write("share/firmware.pins", "# pins\nFW_FILE=firmware/mm6108.bin\nFW_SHA256="+sum(fw)+
		"\nBCF_FILE=bcf/quectel/bcf_x.bin\nBCF_SHA256="+sum(bcf)+"\n")
	e.write("etc/modprobe.d/trellis-morse.conf", "# Written by Trellis\noptions morse country=US bcf=bcf_x.bin enable_ps=0 macaddr_suffix=aa:bb:cc\n")
	e.write("boot/trellis/morse/mm6108.bin", fw)
	e.write("boot/trellis/morse/bcf_x.bin", "tampered")
	e.write("boot/trellis/morse/LICENSE", "licence")
	e.write("boot/trellis/morse/other.bin", "x")
	res, err := Run(e.o)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(res.Installed, ","); got != "morse/LICENSE,morse/mm6108.bin" {
		t.Fatalf("installed %s", got)
	}
	if _, ok := res.Rejected["morse/bcf_x.bin"]; !ok {
		t.Fatalf("a BCF with the wrong hash was not rejected: %v", res.Rejected)
	}
	if _, ok := res.Rejected["morse/other.bin"]; !ok {
		t.Fatal("an unknown file was not rejected")
	}
	if e.read("lib/firmware/morse/mm6108.bin") != fw || !e.gone("lib/firmware/morse/bcf_x.bin") {
		t.Fatal("firmware directory wrong")
	}
	if !res.Reloaded || e.reloads != 1 {
		t.Fatal("the driver was not reloaded after new firmware")
	}
}

func TestImageHostnameAndSuffix(t *testing.T) {
	e := newEnv(t)
	e.write("usr/lib/trellis/image-release", "IMAGE_NAME=trellis-cm4-reference\nIMAGE_HOSTNAME=trellis\n")
	e.write("etc/hosts", "127.0.0.1\tlocalhost\n127.0.1.1\ttrellis\n")
	e.write("etc/modprobe.d/trellis-morse.conf", "# Written by Trellis\noptions morse country=US bcf=b.bin enable_ps=0\n")
	res, err := Run(e.o)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256([]byte(strings.TrimSpace(machineIDText)))
	want := "trellis-" + hex.EncodeToString(h[:2])
	if res.Hostname != want || e.hostname != want || e.read("etc/hostname") != want+"\n" {
		t.Fatalf("hostname %q, set %q, want %q", res.Hostname, e.hostname, want)
	}
	if hosts := e.read("etc/hosts"); hosts != "127.0.0.1\tlocalhost\n127.0.1.1\t"+want+"\n" {
		t.Fatalf("hosts: %q", hosts)
	}
	// The same derivation as install-wm6108-halow.sh: sha256sum of the file.
	f := sha256.Sum256([]byte(machineIDText))
	suffix := fmt.Sprintf("%02x:%02x:%02x", f[0], f[1], f[2])
	if res.MACSuffix != suffix || !strings.Contains(e.read("etc/modprobe.d/trellis-morse.conf"), "enable_ps=0 macaddr_suffix="+suffix+"\n") {
		t.Fatalf("suffix %q, conf %q", res.MACSuffix, e.read("etc/modprobe.d/trellis-morse.conf"))
	}
	if !res.Reloaded {
		t.Fatal("not reloaded after new options")
	}

	// Second boot: nothing more.
	res, err = Run(e.o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Hostname != "" || res.MACSuffix != "" || res.Reloaded {
		t.Fatalf("second boot did something: %+v", res)
	}
}

func TestHostnameKeptWhenChanged(t *testing.T) {
	e := newEnv(t)
	e.hostname = "relay-1"
	e.write("usr/lib/trellis/image-release", "IMAGE_HOSTNAME=trellis\n")
	if res, err := Run(e.o); err != nil || res.Hostname != "" || e.hostname != "relay-1" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestNoMachineIDYet(t *testing.T) {
	e := newEnv(t)
	e.write("etc/machine-id", "uninitialized\n")
	e.write("usr/lib/trellis/image-release", "IMAGE_HOSTNAME=trellis\n")
	e.write("etc/modprobe.d/trellis-morse.conf", "options morse country=US\n")
	if _, err := Run(e.o); err == nil {
		t.Fatal("no error without a machine id")
	}
	if e.hostname != "trellis" || strings.Contains(e.read("etc/modprobe.d/trellis-morse.conf"), "macaddr") {
		t.Fatal("changed without a machine id")
	}
}

func TestWipeZeroesFirst(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p) // still open: sees the overwrite before the unlink
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := wipe(p); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 6)
	if _, err := f.ReadAt(buf, 0); err != nil || string(buf) != "\x00\x00\x00\x00\x00\x00" {
		t.Fatalf("not zeroed: %q %v", buf, err)
	}
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("not removed")
	}
}

const cardKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAILhNVh+5m5DDjNfMtEp20Yqp3LnpdpNB3trND13c2OoB op@laptop"

// accountSys is a machine in the test's tree whose commands are recorded
// (useradd adds the passwd line; chpasswd -e writes shadow).
func (e *env) accountSys() *[]string {
	e.write("etc/passwd", "root:x:0:0:root:/root:/bin/bash\n")
	e.write("etc/group", "sudo:x:27:\ntrellis:x:998:\n")
	e.write("etc/shadow", "root:*:1:0:99999:7:::\n")
	e.write("usr/sbin/sshd", "")
	e.write("run/systemd/system/.x", "") // systemd runs
	var cmds []string
	e.o.Account = &account.System{Root: e.root, Chown: func(string, int, int) error { return nil },
		Run: func(ctx context.Context, stdin, name string, args ...string) ([]byte, error) {
			if name == "systemctl" && args[0] == "is-active" {
				return []byte("inactive\n"), nil
			}
			cmds = append(cmds, strings.TrimSpace(name+" "+strings.Join(args, " ")))
			switch name {
			case "useradd":
				e.write("etc/passwd", e.read("etc/passwd")+"trellis:x:1001:998::/home/trellis:/bin/bash\n")
			case "chpasswd":
				e.write("etc/shadow", e.read("etc/shadow")+strings.TrimSpace(stdin)+":1:0:99999:7:::\n")
			}
			return nil, nil
		}}
	return &cmds
}

func TestCardAccount(t *testing.T) {
	e := newEnv(t)
	cmds := e.accountSys()
	hash := "$6$saltstring$svn8UoSVapNtMuq1ukKS4tPQd8iKwSMHWjl/O817G3uBnIFNjnQJuesI68u4OTLiBFdcbYEdFCoEOfaS35inz1"
	e.write("boot/trellis/authorized_keys", "# the team's\n"+cardKey+"\n")
	e.write("boot/trellis/password", hash+"\n")
	res, err := Run(e.o)
	if err != nil || len(res.Rejected) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if !e.gone("boot/trellis/authorized_keys") || !e.gone("boot/trellis/password") {
		t.Fatal("account files left on the card")
	}
	joined := strings.Join(*cmds, "\n")
	for _, want := range []string{"useradd", "chpasswd -e", "systemctl enable --now --no-block ssh.service"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no %q in %s", want, joined)
		}
	}
	if !strings.Contains(e.read("etc/shadow"), "trellis:"+hash) || !strings.Contains(e.read("home/trellis/.ssh/authorized_keys"), cardKey) {
		t.Fatal("account not set from the card")
	}
}

func TestCardAccountRejected(t *testing.T) {
	e := newEnv(t)
	cmds := e.accountSys()
	e.write("boot/trellis/authorized_keys", cardKey+"\n")
	e.write("boot/trellis/password", "correct-horse\n") // a password, not a hash
	res, err := Run(e.o)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Rejected["password"]; !ok {
		t.Fatalf("a plain password was taken: %+v", res)
	}
	if _, ok := res.Rejected["authorized_keys"]; !ok || e.gone("boot/trellis/authorized_keys") || e.gone("boot/trellis/password") {
		t.Fatal("with one file wrong, both stay on the card")
	}
	if len(*cmds) != 0 {
		t.Fatalf("ran %v", *cmds)
	}
	if r := e.read("boot/trellis/" + ReportFile); strings.Contains(r, "correct-horse") {
		t.Fatalf("the report holds the password: %q", r)
	}
}
