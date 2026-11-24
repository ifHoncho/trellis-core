package account

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The reference hashes are openssl passwd -6 -salt SALT 'Hello world!'.
func TestSHA512Crypt(t *testing.T) {
	for salt, want := range map[string]string{
		"saltstring":           "$6$saltstring$svn8UoSVapNtMuq1ukKS4tPQd8iKwSMHWjl/O817G3uBnIFNjnQJuesI68u4OTLiBFdcbYEdFCoEOfaS35inz1",
		"saltstringsaltstring": "$6$saltstringsaltst$e.3mR68CqZEpesEX1HlFZT6sEanSOjM/b5UoDyDo00a8syek2cJldMjrbtKP86.FJvzluVR7nc3DNzelAwTxj.",
	} {
		if got := sha512Crypt([]byte("Hello world!"), []byte(salt)); got != want {
			t.Errorf("salt %s:\n got %s\nwant %s", salt, got, want)
		}
	}
	// A password longer than a SHA-512 block takes the other branch.
	long := strings.Repeat("x", 150)
	if h := sha512Crypt([]byte(long), []byte("ab")); !strings.HasPrefix(h, "$6$ab$") || len(h) != len("$6$ab$")+86 {
		t.Fatalf("long password: %s", h)
	}
	h, err := HashPassword("correct-horse")
	if err != nil || !IsHash(h) || len(h) != 3+16+1+86 {
		t.Fatalf("HashPassword: %q %v", h, err)
	}
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("a short password was hashed")
	}
}

// A real key (ssh-keygen -t ed25519) and its fingerprint (ssh-keygen -lf).
const (
	edKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAILhNVh+5m5DDjNfMtEp20Yqp3LnpdpNB3trND13c2OoB op@laptop"
	edFP  = "SHA256:CjQ085rxNtmkv/eK31LswZp+z1q2bofHp3ZCWuRbQH4"
)

func TestParseKey(t *testing.T) {
	k, err := ParseKey(edKey)
	if err != nil {
		t.Fatal(err)
	}
	if k.Type != "ssh-ed25519" || k.Comment != "op@laptop" || k.Line() != edKey {
		t.Fatalf("%+v", k)
	}
	if k.Fingerprint != edFP {
		t.Fatalf("fingerprint %s, want %s", k.Fingerprint, edFP)
	}
	opt := `from="10.0.0.0/8",command="echo a b" ` + edKey
	k, err = ParseKey(opt)
	if err != nil || k.Options != `from="10.0.0.0/8",command="echo a b"` || k.Line() != opt {
		t.Fatalf("options: %+v %v", k, err)
	}
	for _, bad := range []string{
		"", "hello", "ssh-dss AAAAB3NzaC1kc3M=",
		"ssh-ed25519 !!!notbase64",
		"ssh-rsa AAAAC3NzaC1lZDI1NTE5AAAAILhNVh+5m5DDjNfMtEp20Yqp3LnpdpNB3trND13c2OoB", // an ed25519 blob called rsa
	} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	keys, err := ParseKeys("# mine\n\n" + edKey + "\n")
	if err != nil || len(keys) != 1 {
		t.Fatalf("%v %v", keys, err)
	}
	if _, err := ParseKeys(edKey + "\nnonsense\n"); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("error does not name the line: %v", err)
	}
	if got := MergeKeys(keys, keys[0]); len(got) != 1 {
		t.Fatal("MergeKeys added a key twice")
	}
}

type fakeSys struct {
	t       *testing.T
	root    string
	cmds    []string
	active  bool
	sshdBad bool
}

func newFakeSys(t *testing.T) (*fakeSys, *System) {
	f := &fakeSys{t: t, root: t.TempDir()}
	for p, c := range map[string]string{
		"etc/passwd":            "root:x:0:0:root:/root:/bin/bash\n",
		"etc/group":             "root:x:0:\nsudo:x:27:\ntrellis:x:998:\n",
		"etc/shadow":            "root:*:20000:0:99999:7:::\n",
		"usr/sbin/sshd":         "",
		"run/systemd/system/.x": "", // systemd runs
	} {
		f.write(p, c)
	}
	return f, &System{Root: f.root, Run: f.run, Chown: func(string, int, int) error { return nil }}
}

func (f *fakeSys) write(rel, content string) {
	p := filepath.Join(f.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeSys) append(rel, line string) {
	p := filepath.Join(f.root, rel)
	data, _ := os.ReadFile(p)
	f.write(rel, string(data)+line+"\n")
}

func (f *fakeSys) run(ctx context.Context, stdin, name string, args ...string) ([]byte, error) {
	cmd := strings.TrimSpace(name + " " + strings.Join(args, " "))
	f.cmds = append(f.cmds, cmd)
	switch name {
	case "useradd":
		f.append("etc/passwd", "trellis:x:1001:998:Trellis operator:/home/trellis:/bin/bash")
		f.append("etc/shadow", "trellis:!:20000:0:99999:7:::")
		if err := os.MkdirAll(filepath.Join(f.root, "home/trellis"), 0o755); err != nil {
			return nil, err
		}
	case "chpasswd":
		user, pw, _ := strings.Cut(strings.TrimSpace(stdin), ":")
		if len(args) == 0 {
			pw = "$y$hashed"
		}
		data, _ := os.ReadFile(filepath.Join(f.root, "etc/shadow"))
		var out []string
		for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if p := strings.SplitN(l, ":", 3); p[0] == user {
				l = user + ":" + pw + ":" + p[2]
			}
			out = append(out, l)
		}
		f.write("etc/shadow", strings.Join(out, "\n")+"\n")
	case "systemctl":
		if args[0] == "is-active" {
			if f.active && args[1] == "ssh.service" {
				return []byte("active\n"), nil
			}
			return []byte("inactive\n"), errors.New("exit 3")
		}
		if args[0] == "enable" {
			f.active = true
		}
	case "sshd":
		if f.sshdBad {
			return nil, errors.New("sshd -t: bad configuration")
		}
	}
	return nil, nil
}

func (f *fakeSys) ran(prefix string) bool {
	for _, c := range f.cmds {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func TestApplyNewAccount(t *testing.T) {
	f, s := newFakeSys(t)
	ctx := context.Background()
	st, err := s.Get(ctx)
	if err != nil || st.Exists || st.SSH != SSHInactive {
		t.Fatalf("%+v %v", st, err)
	}
	k, _ := ParseKey(edKey)
	keys, on := []Key{k}, true
	if err := s.Apply(ctx, Change{Password: "correct-horse", Keys: &keys, KeyOnly: &on}); err != nil {
		t.Fatal(err)
	}
	if !f.ran("useradd --create-home --gid trellis --shell /bin/bash --comment Trellis operator --groups sudo trellis") {
		t.Fatalf("useradd: %v", f.cmds)
	}
	if !f.ran("systemctl enable --now --no-block ssh.service") {
		t.Fatalf("ssh not enabled: %v", f.cmds)
	}
	for _, c := range f.cmds {
		if strings.Contains(c, "correct-horse") {
			t.Fatalf("the password is on a command line: %s", c)
		}
	}
	st, err = s.Get(ctx)
	if err != nil || !st.Exists || !st.Password || !st.KeyOnly || len(st.Keys) != 1 || st.Keys[0].Fingerprint != edFP || st.SSH != SSHActive {
		t.Fatalf("%+v %v", st, err)
	}
	fi, err := os.Stat(filepath.Join(f.root, "home/trellis/.ssh/authorized_keys"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("authorized_keys: %v %v", fi, err)
	}
	conf, _ := os.ReadFile(filepath.Join(f.root, SSHDropIn))
	if !strings.Contains(string(conf), "Match User trellis") || !strings.HasSuffix(string(conf), "Match all\n") {
		t.Fatalf("drop-in: %s", conf)
	}

	// Key login only off again: the drop-in goes; nothing else changes.
	off := false
	f.cmds = nil
	if err := s.Apply(ctx, Change{KeyOnly: &off}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.root, SSHDropIn)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("drop-in still there")
	}
	if f.ran("useradd") || !f.ran("systemctl --no-block try-reload-or-restart ssh.service") {
		t.Fatalf("%v", f.cmds)
	}
}

func TestApplyRefusals(t *testing.T) {
	f, s := newFakeSys(t)
	ctx := context.Background()
	on := true
	if err := s.Apply(ctx, Change{KeyOnly: &on}); err == nil {
		t.Fatal("key login only without a key")
	}
	if err := s.Apply(ctx, Change{Password: "short"}); err == nil {
		t.Fatal("short password")
	}
	if err := s.Apply(ctx, Change{PasswordHash: "plain"}); err == nil {
		t.Fatal("not a hash")
	}
	if len(f.cmds) > 0 && f.ran("useradd") {
		t.Fatal("refused changes created the account")
	}
	// sshd rejects the drop-in: it is taken back.
	k, _ := ParseKey(edKey)
	keys := []Key{k}
	f.sshdBad = true
	if err := s.Apply(ctx, Change{Keys: &keys, KeyOnly: &on}); err == nil {
		t.Fatal("sshd -t failure not reported")
	}
	if _, err := os.Stat(filepath.Join(f.root, SSHDropIn)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a drop-in sshd rejects was left")
	}
}

// Saving the keys keeps the lines of authorized_keys that are not keys
// Trellis reads, and drops a removed key.
func TestWriteKeysKeepsOtherLines(t *testing.T) {
	f, s := newFakeSys(t)
	ctx := context.Background()
	k, _ := ParseKey(edKey)
	keys := []Key{k}
	if err := s.Apply(ctx, Change{Keys: &keys}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.root, "home/trellis/.ssh/authorized_keys")
	other := []string{"# the team's CA", "ssh-ed25519-cert-v01@openssh.com AAAAcert op@cert"}
	data, _ := os.ReadFile(path)
	f.write("home/trellis/.ssh/authorized_keys", string(data)+strings.Join(other, "\n")+"\n")
	none := []Key{}
	if err := s.Apply(ctx, Change{Keys: &none}); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	for _, l := range other {
		if !strings.Contains(string(data), l+"\n") {
			t.Fatalf("%q dropped:\n%s", l, data)
		}
	}
	if strings.Contains(string(data), k.Data) || strings.Count(string(data), keysHeader) != 1 {
		t.Fatalf("removed key kept, or header twice:\n%s", data)
	}
}

// Without systemd (a chroot), SSH is only enabled, and the change holds.
func TestApplyWithoutSystemd(t *testing.T) {
	f, s := newFakeSys(t)
	if err := os.RemoveAll(filepath.Join(f.root, "run")); err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(context.Background(), Change{Password: "correct-horse"}); err != nil {
		t.Fatal(err)
	}
	if !f.ran("systemctl enable ssh.service") || f.ran("systemctl enable --now") {
		t.Fatalf("%v", f.cmds)
	}
}

func TestHashFromCard(t *testing.T) {
	f, s := newFakeSys(t)
	h, _ := HashPassword("correct-horse")
	if err := s.Apply(context.Background(), Change{PasswordHash: h}); err != nil {
		t.Fatal(err)
	}
	if !f.ran("chpasswd -e") {
		t.Fatalf("%v", f.cmds)
	}
	shadow, _ := os.ReadFile(filepath.Join(f.root, "etc/shadow"))
	if !strings.Contains(string(shadow), "trellis:"+h+":") {
		t.Fatalf("shadow: %s", shadow)
	}
}

func TestGitHubKeys(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/op.keys":
			fmt.Fprintln(w, strings.Fields(edKey)[0]+" "+strings.Fields(edKey)[1])
		case "/empty.keys":
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	old := GitHubKeysURL
	GitHubKeysURL = srv.URL + "/%s.keys"
	defer func() { GitHubKeysURL = old }()
	ctx := context.Background()
	keys, err := GitHubKeys(ctx, srv.Client(), "op")
	if err != nil || len(keys) != 1 || keys[0].Comment != "github:op" || keys[0].Fingerprint != edFP {
		t.Fatalf("%+v %v", keys, err)
	}
	if _, err := GitHubKeys(ctx, srv.Client(), "nobody"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("404: %v", err)
	}
	if _, err := GitHubKeys(ctx, srv.Client(), "empty"); err == nil || !strings.Contains(err.Error(), "no public SSH keys") {
		t.Fatalf("empty: %v", err)
	}
	for _, bad := range []string{"", "-x", "a--b", "a/b", strings.Repeat("a", 40), "../etc"} {
		if _, err := GitHubKeys(ctx, srv.Client(), bad); err == nil {
			t.Errorf("username %q accepted", bad)
		}
	}
}
