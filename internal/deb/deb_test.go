package deb

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sample() *Package {
	return &Package{
		Control: []Field{
			{"Package", "trellis"}, {"Version", "0.3.1"}, {"Architecture", "arm64"},
			{"Maintainer", "Example <ops@example.org>"}, {"Depends", "iw, hostapd"},
			{"Description", "mesh node\n A longer text."},
		},
		Scripts:   map[string][]byte{"postinst": []byte("#!/bin/sh\nexit 0\n")},
		Conffiles: []string{"/etc/chrony/conf.d/trellis.conf"},
		Files: []File{
			{Path: "/usr/bin/trellis", Mode: 0o755, Body: []byte("binary")},
			{Path: "/usr/bin/meshctl", Mode: fs.ModeSymlink | 0o777, Link: "trellis"},
			{Path: "/etc/chrony/conf.d/trellis.conf", Mode: 0o644, Body: []byte("allow\n")},
		},
		Time: time.Unix(1_800_000_000, 0),
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	p := sample()
	b, err := p.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	again, _ := p.Bytes()
	if !bytes.Equal(b, again) {
		t.Fatal("the same package gave different bytes")
	}
	info, err := Read(b)
	if err != nil {
		t.Fatal(err)
	}
	if info.Get("Package") != "trellis" || info.Get("version") != "0.3.1" || info.Get("Installed-Size") == "" {
		t.Fatalf("control read back wrong: %+v", info.Control)
	}
	if got := info.Get("Description"); got != "mesh node\n A longer text." {
		t.Fatalf("description = %q", got)
	}
	if info.Signed() {
		t.Fatal("unsigned package reads as signed")
	}
	if _, err := info.Verify(nil); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("Verify on unsigned = %v", err)
	}

	// The payload: parents present, sorted, root-owned, symlink kept.
	ms, _ := members(b)
	if len(ms) != 3 || ms[2].name != "data.tar.gz" {
		t.Fatalf("members = %+v", ms)
	}
	zr, _ := gzip.NewReader(bytes.NewReader(ms[2].body))
	tr := tar.NewReader(zr)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
		if h.Uid != 0 || h.Uname != "root" {
			t.Errorf("%s not owned by root", h.Name)
		}
		if h.Name == "./usr/bin/meshctl" && (h.Typeflag != tar.TypeSymlink || h.Linkname != "trellis") {
			t.Errorf("symlink lost: %+v", h)
		}
	}
	want := "./etc/ ./etc/chrony/ ./etc/chrony/conf.d/ ./etc/chrony/conf.d/trellis.conf ./usr/ ./usr/bin/ ./usr/bin/meshctl ./usr/bin/trellis"
	if got := strings.Join(names, " "); got != want {
		t.Fatalf("payload entries:\n got %s\nwant %s", got, want)
	}
}

func TestRejectsBadInput(t *testing.T) {
	p := sample()
	p.Files = append(p.Files, File{Path: "usr/bin/x", Mode: 0o644})
	if _, err := p.Bytes(); err == nil {
		t.Error("relative payload path accepted")
	}
	p = sample()
	p.Files = append(p.Files, p.Files[0])
	if _, err := p.Bytes(); err == nil {
		t.Error("duplicate payload path accepted")
	}
	p = sample()
	p.Control = p.Control[1:]
	if _, err := p.Bytes(); err == nil {
		t.Error("package without a name accepted")
	}
	for _, junk := range [][]byte{nil, []byte("hello"), []byte(arMagic + "short")} {
		if _, err := Read(junk); err == nil {
			t.Errorf("Read accepted %q", junk)
		}
	}
	b, _ := sample().Bytes()
	if _, err := Read(b[:len(b)-10]); err == nil {
		t.Error("Read accepted a truncated package")
	}
}

func TestSignVerify(t *testing.T) {
	secret, public, err := NewKey("release  2026")
	if err != nil {
		t.Fatal(err)
	}
	priv, err := ParseSecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := ParsePublicKeys("# trusted\n\n" + public)
	if err != nil || len(keys) != 1 || keys[0].Comment != "release 2026" {
		t.Fatalf("keys = %+v, %v", keys, err)
	}
	if Public(priv, "release 2026") != public {
		t.Fatal("Public does not give the key's public line")
	}
	b, _ := sample().Bytes()
	signed, err := Sign(b, priv, time.Unix(1_800_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(signed, b) {
		t.Fatal("signing changed the package's own bytes")
	}
	info, err := Read(signed)
	if err != nil {
		t.Fatal(err)
	}
	if k, err := info.Verify(keys); err != nil || k.ID() != keys[0].ID() {
		t.Fatalf("Verify = %v, %v", k, err)
	}
	if _, err := Sign(signed, priv, time.Now()); err == nil {
		t.Error("signed twice")
	}

	// Another key is not trusted.
	_, otherPub, _ := NewKey("other")
	other, _ := ParsePublicKeys(otherPub)
	if _, err := info.Verify(other); err == nil || !strings.Contains(err.Error(), "does not trust") {
		t.Errorf("untrusted key: %v", err)
	}
	// A changed byte anywhere before the signature is caught.
	for _, at := range []int{len(arMagic) + 70, len(b) / 2, len(b) - 1} {
		bad := append([]byte(nil), signed...)
		bad[at] ^= 0x01
		i2, err := Read(bad)
		if err != nil {
			continue // damaged beyond reading is as good
		}
		if _, err := i2.Verify(keys); err == nil {
			t.Errorf("change at byte %d not caught", at)
		}
	}
	// A member after the signature is refused: it would not be covered.
	extra := bytes.NewBuffer(append([]byte(nil), signed...))
	arMember(extra, "_more", []byte("x"), time.Now())
	if _, err := Read(extra.Bytes()); err == nil {
		t.Error("a member after the signature was accepted")
	}
	if _, err := ParsePublicKeys("ssh-ed25519 AAAA"); err == nil {
		t.Error("foreign key line accepted")
	}
	if _, err := ParseSecret(public); err == nil {
		t.Error("public line accepted as a secret")
	}
}

// With dpkg-deb at hand (a Debian-family build machine), it must agree.
func TestDpkgReadsIt(t *testing.T) {
	dpkg, err := exec.LookPath("dpkg-deb")
	if err != nil {
		t.Skip("no dpkg-deb here")
	}
	secret, _, _ := NewKey("")
	priv, _ := ParseSecret(secret)
	b, _ := sample().Bytes()
	signed, _ := Sign(b, priv, time.Now())
	f := filepath.Join(t.TempDir(), "t.deb")
	if err := os.WriteFile(f, signed, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(dpkg, "--info", f).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "Package: trellis") {
		t.Fatalf("dpkg-deb --info: %v\n%s", err, out)
	}
	out, err = exec.Command(dpkg, "--contents", f).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "./usr/bin/trellis") {
		t.Fatalf("dpkg-deb --contents: %v\n%s", err, out)
	}
}

// A control archive that unpacks to far more than any control file is cut
// off rather than unpacked.
func TestControlBombCutOff(t *testing.T) {
	var big bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&big, gzip.BestCompression)
	tw := tar.NewWriter(zw)
	huge := make([]byte, 64<<20) // zeros: compresses to almost nothing
	tw.WriteHeader(&tar.Header{Name: "./filler", Mode: 0o644, Size: int64(len(huge)), Typeflag: tar.TypeReg})
	tw.Write(huge)
	tw.WriteHeader(&tar.Header{Name: "./control", Mode: 0o644, Size: 30, Typeflag: tar.TypeReg})
	tw.Write([]byte("Package: x\nVersion: 1\n\n\n\n\n\n\n"))
	tw.Close()
	zw.Close()
	var b bytes.Buffer
	b.WriteString(arMagic)
	when := time.Unix(1_800_000_000, 0)
	arMember(&b, "debian-binary", []byte("2.0\n"), when)
	arMember(&b, "control.tar.gz", big.Bytes(), when)
	arMember(&b, "data.tar.gz", []byte{}, when)
	start := time.Now()
	if _, err := Read(b.Bytes()); err == nil {
		t.Error("a control file behind 64 MB of filler was read")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("reading took %v", d)
	}
}
