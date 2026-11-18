// Command aptrepo keeps a static apt repository of Trellis's packages: a
// directory any web server (or GitHub Pages, or a USB stick) can serve.
//
//	aptrepo --dir REPO [--gpg-key ID] FILE.deb...
//
// Packages are added to REPO/pool/main/ (an added file never replaces a
// different one of the same name, so a published version stays what it
// was), then every index is written again from what the pool holds:
// dists/stable/main/binary-<arch>/Packages{,.gz} and dists/stable/Release.
// With --gpg-key, Release is signed with gpg (InRelease and Release.gpg)
// and the key's public half is written next to it as
// trellis-archive-keyring.gpg, for apt's signed-by. Without it the
// repository is unsigned: for a trial only.
//
// Only release packages go in, signed with a key in
// internal/release/trusted.keys, unless --allow-dev (trials, with
// --trusted naming the trial key's public line).
package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"meridian/trellis/internal/deb"
	"meridian/trellis/internal/release"
	"meridian/trellis/internal/version"
)

const (
	suite     = "stable"
	component = "main"
	// KeyringName is the public key file, for signed-by.
	keyringName = "trellis-archive-keyring.gpg"
)

// Architectures the repository serves; arch-all packages are listed in
// each.
var arches = []string{"amd64", "arm64", "armhf"}

func main() {
	fl := flag.NewFlagSet("aptrepo", flag.ExitOnError)
	dir := fl.String("dir", "", "the repository (created if missing)")
	gpgKey := fl.String("gpg-key", "", "gpg key that signs Release (the archive key)")
	allowDev := fl.Bool("allow-dev", false, "take packages that are not releases (a trial repository)")
	trusted := fl.String("trusted", "", "with --allow-dev: a file of public keys the packages may be signed with instead")
	fl.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: aptrepo --dir REPO [--gpg-key ID] [--allow-dev [--trusted KEYS]] FILE.deb...")
		fl.PrintDefaults()
	}
	fl.Parse(os.Args[1:])
	if *dir == "" {
		fl.Usage()
		os.Exit(2)
	}
	if err := run(*dir, *gpgKey, *allowDev, *trusted, fl.Args(), time.Now().UTC()); err != nil {
		fmt.Fprintln(os.Stderr, "aptrepo:", err)
		os.Exit(1)
	}
}

func run(dir, gpgKey string, allowDev bool, trustedFile string, debs []string, now time.Time) error {
	keys, err := release.Builtin()
	if err != nil {
		return err
	}
	if trustedFile != "" {
		if !allowDev {
			return errors.New("--trusted is for trial repositories (--allow-dev)")
		}
		raw, err := os.ReadFile(trustedFile)
		if err != nil {
			return err
		}
		extra, err := deb.ParsePublicKeys(string(raw))
		if err != nil {
			return err
		}
		keys = append(keys, extra...)
	}
	for _, name := range debs {
		if err := add(dir, name, keys, allowDev); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if err := index(dir, now); err != nil {
		return err
	}
	if gpgKey == "" {
		fmt.Fprintln(os.Stderr, "aptrepo: no --gpg-key: the repository is unsigned (apt takes it only with [trusted=yes])")
		return nil
	}
	return sign(dir, gpgKey)
}

// add checks a package and copies it into the pool.
func add(dir, name string, keys []deb.PublicKey, allowDev bool) error {
	b, err := os.ReadFile(name)
	if err != nil {
		return err
	}
	info, err := deb.Read(b)
	if err != nil {
		return err
	}
	pkg, ver, arch := info.Get("Package"), info.Get("Version"), info.Get("Architecture")
	if !strings.HasPrefix(pkg, "trellis") {
		return fmt.Errorf("%s is not a Trellis package", pkg)
	}
	if pkg == "trellis" {
		if v := version.Parse(info.Get("Trellis-Version")); (!v.Numbered || !v.Exact) && !allowDev {
			return fmt.Errorf("%s %s is not a release (--allow-dev for a trial repository)", pkg, info.Get("Trellis-Version"))
		}
	}
	if _, err := info.Verify(keys); err != nil {
		return fmt.Errorf("%s %s: %w", pkg, ver, err)
	}
	if arch != "all" && !contains(arches, arch) {
		return fmt.Errorf("architecture %s is not one this repository serves (%s)", arch, strings.Join(arches, ", "))
	}
	dst := filepath.Join(dir, poolPath(pkg, ver, arch))
	if old, err := os.ReadFile(dst); err == nil {
		if bytes.Equal(old, b) {
			return nil
		}
		return fmt.Errorf("%s is already in the repository with other contents: a published version is never replaced", filepath.Base(dst))
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return writeFile(dst, b)
}

// poolPath is where a package lives, Debian style: pool/main/t/trellis/.
func poolPath(pkg, ver, arch string) string {
	// An epoch's colon is not kept in file names.
	if _, v, ok := strings.Cut(ver, ":"); ok {
		ver = v
	}
	return path.Join("pool", component, pkg[:1], pkg, fmt.Sprintf("%s_%s_%s.deb", pkg, ver, arch))
}

type entry struct {
	pkg, ver, arch string
	stanza         string
}

// index writes Packages for every architecture and Release, from the pool.
func index(dir string, now time.Time) error {
	var all []entry
	pool := filepath.Join(dir, "pool")
	err := filepath.WalkDir(pool, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".deb") {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		info, err := deb.Read(b)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		rel, _ := filepath.Rel(dir, p)
		all = append(all, entry{info.Get("Package"), info.Get("Version"), info.Get("Architecture"), stanza(info, filepath.ToSlash(rel), b)})
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	sort.Slice(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if a.pkg != b.pkg {
			return a.pkg < b.pkg
		}
		if a.arch != b.arch {
			return a.arch < b.arch
		}
		return a.ver < b.ver
	})
	dists := filepath.Join(dir, "dists", suite)
	var sums []string
	for _, arch := range arches {
		var buf bytes.Buffer
		for _, e := range all {
			if e.arch == arch || e.arch == "all" {
				buf.WriteString(e.stanza)
				buf.WriteString("\n")
			}
		}
		rel := path.Join(component, "binary-"+arch)
		plain := buf.Bytes()
		gz, err := gzipped(plain)
		if err != nil {
			return err
		}
		for name, body := range map[string][]byte{"Packages": plain, "Packages.gz": gz} {
			p := filepath.Join(dists, filepath.FromSlash(rel), name)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			if err := writeFile(p, body); err != nil {
				return err
			}
			sums = append(sums, fmt.Sprintf(" %s %d %s", sha(body), len(body), path.Join(rel, name)))
		}
	}
	sort.Strings(sums)
	var r strings.Builder
	fmt.Fprintf(&r, "Origin: Trellis\nLabel: Trellis\nSuite: %s\nCodename: %s\n", suite, suite)
	fmt.Fprintf(&r, "Date: %s\n", now.Format("Mon, 02 Jan 2006 15:04:05 UTC"))
	fmt.Fprintf(&r, "Architectures: %s\nComponents: %s\n", strings.Join(arches, " "), component)
	r.WriteString("Description: Meridian Trellis mesh networking\nAcquire-By-Hash: no\nSHA256:\n")
	r.WriteString(strings.Join(sums, "\n") + "\n")
	// Signatures of an earlier Release no longer match: sign writes new ones.
	for _, n := range []string{"InRelease", "Release.gpg"} {
		if err := os.Remove(filepath.Join(dists, n)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return writeFile(filepath.Join(dists, "Release"), []byte(r.String()))
}

// stanza is a package's Packages entry: its control fields (minus the
// ones that describe the file, which the repository states), then the
// file's place, size and hash.
func stanza(info *deb.Info, rel string, b []byte) string {
	var s strings.Builder
	for _, f := range info.Control {
		switch strings.ToLower(f.Name) {
		case "filename", "size", "md5sum", "sha1", "sha256", "sha512":
			continue
		}
		fmt.Fprintf(&s, "%s: %s\n", f.Name, f.Value)
	}
	fmt.Fprintf(&s, "Filename: %s\nSize: %d\nSHA256: %s\n", rel, len(b), sha(b))
	return s.String()
}

// archiveKey resolves --gpg-key to the one secret key it names, by
// fingerprint: a name or address can match several keys, and every key
// exported would be trusted by the machines that add the repository.
func archiveKey(key string) (string, error) {
	out, err := exec.Command("gpg", "--batch", "--with-colons", "--list-secret-keys", "--", key).Output()
	if err != nil {
		return "", fmt.Errorf("gpg has no secret key %q", key)
	}
	var fprs []string
	primary := false
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Split(line, ":")
		switch {
		case f[0] == "sec":
			primary = true
		case f[0] == "ssb":
			primary = false
		case f[0] == "fpr" && primary && len(f) > 9:
			fprs = append(fprs, f[9])
			primary = false
		}
	}
	switch len(fprs) {
	case 0:
		return "", fmt.Errorf("gpg has no secret key %q", key)
	case 1:
		return fprs[0], nil
	}
	return "", fmt.Errorf("%q matches %d secret keys (%s): name the archive key by its fingerprint", key, len(fprs), strings.Join(fprs, ", "))
}

// sign makes InRelease (clearsigned) and Release.gpg (detached), and
// writes the key's public half for signed-by.
func sign(dir, name string) error {
	key, err := archiveKey(name)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "aptrepo: signed with %s; publish this fingerprint beside the repository\n", key)
	dists := filepath.Join(dir, "dists", suite)
	rel := filepath.Join(dists, "Release")
	if out, err := exec.Command("gpg", "--batch", "--yes", "--local-user", key, "--digest-algo", "SHA512",
		"--clearsign", "--output", filepath.Join(dists, "InRelease"), rel).CombinedOutput(); err != nil {
		return fmt.Errorf("gpg --clearsign: %v: %s", err, out)
	}
	if out, err := exec.Command("gpg", "--batch", "--yes", "--local-user", key, "--digest-algo", "SHA512",
		"--armor", "--detach-sign", "--output", filepath.Join(dists, "Release.gpg"), rel).CombinedOutput(); err != nil {
		return fmt.Errorf("gpg --detach-sign: %v: %s", err, out)
	}
	pub, err := exec.Command("gpg", "--batch", "--export", key).Output()
	if err != nil || len(pub) == 0 {
		return fmt.Errorf("gpg --export %s: %v", key, err)
	}
	if err := writeFile(filepath.Join(dir, keyringName), pub); err != nil {
		return err
	}
	asc, err := exec.Command("gpg", "--batch", "--armor", "--export", key).Output()
	if err != nil {
		return fmt.Errorf("gpg --armor --export %s: %v", key, err)
	}
	return writeFile(filepath.Join(dir, strings.TrimSuffix(keyringName, ".gpg")+".asc"), asc)
}

func gzipped(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	// No name and no time in the header: the same pool gives the same files.
	if _, err := zw.Write(b); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func sha(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// writeFile replaces p whole: a web server never serves half a file.
func writeFile(p string, b []byte) error {
	tmp := p + ".new"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
