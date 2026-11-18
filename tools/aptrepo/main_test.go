package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"meridian/trellis/internal/deb"
)

func makeDeb(t *testing.T, dir, pkg, ver, arch, trellisVer string, priv string) string {
	t.Helper()
	p := &deb.Package{Control: []deb.Field{
		{Name: "Package", Value: pkg}, {Name: "Version", Value: ver}, {Name: "Architecture", Value: arch},
		{Name: "Maintainer", Value: "Test <test@example.com>"}, {Name: "Depends", Value: "nftables"},
		{Name: "Description", Value: "a test\n more about it"},
	}, Files: []deb.File{{Path: "/usr/share/doc/" + pkg + "/x", Mode: 0o644, Body: []byte(ver + arch)}}, Time: time.Unix(1700000000, 0)}
	if trellisVer != "" {
		p.Control = append(p.Control, deb.Field{Name: "Trellis-Version", Value: trellisVer})
	}
	b, err := p.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if priv != "" {
		k, err := deb.ParseSecret(priv)
		if err != nil {
			t.Fatal(err)
		}
		if b, err = deb.Sign(b, k, time.Unix(1700000000, 0)); err != nil {
			t.Fatal(err)
		}
	}
	name := filepath.Join(dir, p.Filename())
	if err := os.WriteFile(name, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return name
}

func TestRepository(t *testing.T) {
	src, repo := t.TempDir(), t.TempDir()
	secret, public, err := deb.NewKey("test")
	if err != nil {
		t.Fatal(err)
	}
	trusted := filepath.Join(src, "keys.pub")
	os.WriteFile(trusted, []byte(public), 0o644)
	now := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)

	arm := makeDeb(t, src, "trellis", "0.9.0", "arm64", "0.9.0", secret)
	amd := makeDeb(t, src, "trellis", "0.9.0", "amd64", "0.9.0", secret)
	dkms := makeDeb(t, src, "trellis-batman-adv-dkms", "2026.3-2", "all", "", secret)

	// Packages signed with a key the nodes do not trust are refused, and
	// so is a build that is not a release, outside a trial.
	if err := run(repo, "", false, "", []string{arm}, now); err == nil || !strings.Contains(err.Error(), "key") {
		t.Errorf("an untrusted signature was taken: %v", err)
	}
	dev := makeDeb(t, src, "trellis", "0.9.1~dev", "arm64", "0.9.0-3-gabc", secret)
	if err := run(repo, "", false, "", []string{dev}, now); err == nil || !strings.Contains(err.Error(), "not a release") {
		t.Errorf("a dev build was taken: %v", err)
	}
	unsigned := makeDeb(t, t.TempDir(), "trellis", "0.9.0", "armhf", "0.9.0", "")
	if err := run(repo, "", true, trusted, []string{unsigned}, now); err == nil {
		t.Error("an unsigned package was taken")
	}

	if err := run(repo, "", true, trusted, []string{arm, amd, dkms}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, "pool/main/t/trellis/trellis_0.9.0_arm64.deb")); err != nil {
		t.Error(err)
	}
	pk := read(t, repo, "dists/stable/main/binary-arm64/Packages")
	if !strings.Contains(pk, "Package: trellis\nVersion: 0.9.0\nArchitecture: arm64\n") ||
		!strings.Contains(pk, "Package: trellis-batman-adv-dkms\n") || strings.Contains(pk, "Architecture: amd64") ||
		!strings.Contains(pk, "Description: a test\n more about it\n") || !strings.Contains(pk, "Filename: pool/main/t/trellis/trellis_0.9.0_arm64.deb\n") {
		t.Errorf("arm64 Packages:\n%s", pk)
	}
	if hf := read(t, repo, "dists/stable/main/binary-armhf/Packages"); !strings.Contains(hf, "trellis-batman-adv-dkms") || strings.Contains(hf, "Package: trellis\n") {
		t.Errorf("armhf Packages:\n%s", hf)
	}
	zr, err := gzip.NewReader(bytes.NewReader([]byte(read(t, repo, "dists/stable/main/binary-arm64/Packages.gz"))))
	if err != nil {
		t.Fatal(err)
	}
	if unz, _ := io.ReadAll(zr); string(unz) != pk {
		t.Error("Packages.gz differs from Packages")
	}

	// Release lists every index with its size and hash.
	rel := read(t, repo, "dists/stable/Release")
	if !strings.Contains(rel, "Date: Wed, 02 Jan 2030 03:04:05 UTC\n") || !strings.Contains(rel, "Architectures: amd64 arm64 armhf\n") {
		t.Errorf("Release:\n%s", rel)
	}
	n := 0
	sc := bufio.NewScanner(strings.NewReader(rel[strings.Index(rel, "SHA256:\n")+8:]))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		body := read(t, repo, "dists/stable/"+f[2])
		if f[0] != sha([]byte(body)) || f[1] != strconv.Itoa(len(body)) {
			t.Errorf("Release line %q does not match the file", sc.Text())
		}
		n++
	}
	if n != 6 {
		t.Errorf("Release lists %d files, want 6", n)
	}

	// Adding the same file again is fine; another file under a published
	// name is refused; a new version joins the old one.
	if err := run(repo, "", true, trusted, []string{arm}, now); err != nil {
		t.Errorf("the same file again: %v", err)
	}
	osecret, opublic, _ := deb.NewKey("other")
	os.WriteFile(filepath.Join(src, "other.pub"), []byte(opublic), 0o644)
	other := makeDeb(t, t.TempDir(), "trellis", "0.9.0", "arm64", "0.9.0", osecret)
	if err := run(repo, "", true, filepath.Join(src, "other.pub"), []string{other}, now); err == nil || !strings.Contains(err.Error(), "never replaced") {
		t.Errorf("a published version was replaced: %v", err)
	}
	newer := makeDeb(t, src, "trellis", "0.9.1", "arm64", "0.9.1", secret)
	if err := run(repo, "", true, trusted, []string{newer}, now); err != nil {
		t.Fatal(err)
	}
	if pk := read(t, repo, "dists/stable/main/binary-arm64/Packages"); !strings.Contains(pk, "Version: 0.9.0\n") || !strings.Contains(pk, "Version: 0.9.1\n") {
		t.Errorf("both versions should be listed:\n%s", pk)
	}
}

func read(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// --gpg-key must name exactly one key: a name two keys share is refused,
// a fingerprint is taken.
func TestArchiveKey(t *testing.T) {
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("no gpg")
	}
	home, err := os.MkdirTemp("", "aptrepo-gpg") // short: gpg-agent's socket path has a limit
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(home, 0o700)
	t.Setenv("GNUPGHOME", home)
	t.Cleanup(func() { exec.Command("gpgconf", "--kill", "gpg-agent").Run(); os.RemoveAll(home) })
	for i := 0; i < 2; i++ {
		if out, err := exec.Command("gpg", "--batch", "--quiet", "--passphrase", "", "--quick-gen-key", fmt.Sprintf("Trellis archive (test %d)", i), "ed25519", "sign", "never").CombinedOutput(); err != nil {
			t.Fatalf("gpg: %v %s", err, out)
		}
	}
	if _, err := archiveKey("Trellis archive"); err == nil || !strings.Contains(err.Error(), "matches 2 secret keys") {
		t.Errorf("a name two keys share: %v", err)
	}
	if _, err := archiveKey("nobody"); err == nil {
		t.Error("an unknown key was taken")
	}
	fpr, err := archiveKey("Trellis archive (test 1)")
	if err != nil || len(fpr) != 40 {
		t.Fatalf("one key by name: %q %v", fpr, err)
	}
	t.Logf("fingerprint %s", fpr)
	if got, err := archiveKey(fpr); err != nil || got != fpr {
		t.Errorf("by fingerprint: %q %v", got, err)
	}
}
