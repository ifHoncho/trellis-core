// Command mkdeb builds Trellis's Debian packages and signs them. It needs
// no dpkg, so a release can be built on any machine with Go.
//
//	mkdeb trellis --version V --arch arm64 --binary dist/trellis-linux-arm64 [--voiced dist/trellis-voiced-arm64] --out dist/deb
//	mkdeb batman-adv --source batman-adv-2026.3.tar.gz --out dist/deb
//	mkdeb keygen --out release.key [--comment TEXT]     (prints the public line)
//	mkdeb pubkey --key release.key                      (prints it again)
//	mkdeb sign --key release.key FILE.deb...
//	mkdeb verify --pub KEYS.pub FILE.deb...
//
// --key signs the packages as they are built: releases only, unless
// --sign-dev.
// --epoch (or SOURCE_DATE_EPOCH) stamps every file, so the same source
// gives the same package.
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"meridian/trellis/internal/deb"
	"meridian/trellis/internal/version"
)

const (
	maintainer = "Eunoia IT Solutions LLC <license@eunoiacyber.com>"
	homepage   = "https://github.com/ifHoncho/trellis-core"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: mkdeb trellis|batman-adv|keygen|pubkey|sign|verify [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "trellis":
		err = cmdTrellis(os.Args[2:])
	case "batman-adv":
		err = cmdBatman(os.Args[2:])
	case "keygen":
		err = cmdKeygen(os.Args[2:])
	case "pubkey":
		err = cmdPubkey(os.Args[2:])
	case "sign":
		err = cmdSign(os.Args[2:])
	case "verify":
		err = cmdVerify(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "mkdeb:", err)
		os.Exit(1)
	}
}

// common are the flags every build command takes.
type common struct {
	root, out, key string
	epoch          int64
	// signDev lets a build that is not an exact release be signed.
	signDev bool
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.root, "root", ".", "the repository")
	fs.StringVar(&c.out, "out", "dist/deb", "where the package is written")
	fs.StringVar(&c.key, "key", "", "release secret key file; the package is signed with it")
	fs.BoolVar(&c.signDev, "sign-dev", false, "sign even a version that is not an exact release (for testing upgrades); a node trusting the key would install it")
	var def int64
	if s := os.Getenv("SOURCE_DATE_EPOCH"); s != "" {
		def, _ = strconv.ParseInt(s, 10, 64)
	}
	fs.Int64Var(&c.epoch, "epoch", def, "timestamp of every file in the package (default: now)")
}

func (c *common) when() time.Time {
	if c.epoch > 0 {
		return time.Unix(c.epoch, 0).UTC()
	}
	return time.Now().UTC().Truncate(time.Second)
}

// write signs the package if a key was given and writes it to the output
// directory.
func (c *common) write(p *deb.Package) error {
	b, err := p.Bytes()
	if err != nil {
		return err
	}
	signed := ""
	if c.key != "" && p.Get("Package") == "trellis" && !c.signDev {
		if v := version.Parse(p.Get("Trellis-Version")); !v.Numbered || !v.Exact {
			return fmt.Errorf("refusing to sign %s with a release key: it is not a release (a tag vX.Y.Z on a clean tree); --sign-dev to sign it anyway", p.Get("Trellis-Version"))
		}
	}
	if c.key != "" {
		priv, err := readSecret(c.key)
		if err != nil {
			return err
		}
		if b, err = deb.Sign(b, priv, p.Time); err != nil {
			return err
		}
		signed = ", signed"
	}
	if err := os.MkdirAll(c.out, 0o755); err != nil {
		return err
	}
	name := filepath.Join(c.out, p.Filename())
	if err := os.WriteFile(name, b, 0o644); err != nil {
		return err
	}
	fmt.Printf("%s (%d kB%s)\n", name, len(b)/1024, signed)
	return nil
}

func readSecret(file string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	// A warning, not a refusal: a key kept on removable media often has
	// the modes its filesystem gives every file.
	if st, err := os.Stat(file); err == nil && st.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(os.Stderr, "warning: %s can be read by other users (mode %04o); chmod 600 it\n", file, st.Mode().Perm())
	}
	return deb.ParseSecret(string(b))
}

// tree adds every file under root/src to the payload at dst, keeping the
// executable bit.
func tree(files *[]deb.File, root, src, dst string, skip func(rel string) bool) error {
	base := filepath.Join(root, src)
	return filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(base, p)
		if d.IsDir() || (skip != nil && skip(filepath.ToSlash(rel))) {
			return nil
		}
		return file(files, p, path.Join(dst, filepath.ToSlash(rel)), 0)
	})
}

// file adds one file. Mode 0 keeps 0755 for executables and 0644 for the
// rest.
func file(files *[]deb.File, src, dst string, mode fs.FileMode) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if mode == 0 {
		mode = 0o644
		if st, err := os.Stat(src); err == nil && st.Mode()&0o111 != 0 {
			mode = 0o755
		}
	}
	*files = append(*files, deb.File{Path: dst, Mode: mode, Body: b})
	return nil
}

func scripts(root string, prefix string) (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, n := range []string{"preinst", "postinst", "prerm", "postrm"} {
		b, err := os.ReadFile(filepath.Join(root, "packaging/deb", prefix+"."+n))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[n] = b
	}
	return out, nil
}

func copyright(pkg, licence string) deb.File {
	return deb.File{Path: "/usr/share/doc/" + pkg + "/copyright", Mode: 0o644, Body: []byte(licence)}
}

func cmdTrellis(args []string) error {
	fl := flag.NewFlagSet("trellis", flag.ExitOnError)
	var c common
	c.register(fl)
	ver := fl.String("version", "", "the version the binary reports (as built with -X main.version=)")
	arch := fl.String("arch", "", "Debian architecture: arm64, armhf or amd64")
	bin := fl.String("binary", "", "the trellis binary for that architecture")
	voiced := fl.String("voiced", "", "the trellis-voiced binary for that architecture (push-to-talk voice; arm64 and amd64)")
	fl.Parse(args)
	if *ver == "" || *arch == "" || *bin == "" {
		return errors.New("trellis needs --version, --arch and --binary")
	}
	if *voiced != "" && *arch != "arm64" && *arch != "amd64" {
		return fmt.Errorf("voice is built for arm64 and amd64 only (got %q)", *arch)
	}
	switch *arch {
	case "arm64", "armhf", "amd64":
	default:
		return fmt.Errorf("unknown architecture %q", *arch)
	}
	pin, err := readPin(c.root)
	if err != nil {
		return err
	}
	var files []deb.File
	if err := file(&files, *bin, "/usr/bin/trellis", 0o755); err != nil {
		return err
	}
	for _, l := range []string{"meshd", "meshctl", "webd"} {
		files = append(files, deb.File{Path: "/usr/bin/" + l, Mode: fs.ModeSymlink | 0o777, Link: "trellis"})
	}
	for _, f := range [][2]string{
		{"packaging/systemd/trellis-meshd.service", "/usr/lib/systemd/system/trellis-meshd.service"},
		{"packaging/systemd/trellis-webd.service", "/usr/lib/systemd/system/trellis-webd.service"},
		{"packaging/systemd/trellis-webd.socket", "/usr/lib/systemd/system/trellis-webd.socket"},
		{"packaging/udev/70-trellis-names.rules", "/usr/lib/udev/rules.d/70-trellis-names.rules"},
		{"packaging/tmpfiles/trellis.conf", "/usr/lib/tmpfiles.d/trellis.conf"},
		{"packaging/chrony/trellis.conf", "/etc/chrony/conf.d/trellis.conf"},
	} {
		if err := file(&files, filepath.Join(c.root, f[0]), f[1], 0o644); err != nil {
			return err
		}
	}
	// Push-to-talk voice: trellis-voiced, its unit and the udev rule for
	// CM108 adapters. meshd starts it when voice.enable is set. It loads
	// libopus and libasound at run time: recommended, not required, so
	// that meshctl upgrade (dpkg alone, often with no network) never needs
	// a package the node lacks.
	recommends := fmt.Sprintf("trellis-batman-adv-dkms (>= %s), batctl", pin.version)
	var takeover []deb.Field
	about := " This package holds the single trellis binary (meshd, meshctl, webd), its\n" +
		" services, and the scripts that set up radios it supports."
	if *voiced != "" {
		if err := file(&files, *voiced, "/usr/bin/trellis-voiced", 0o755); err != nil {
			return err
		}
		for _, f := range [][2]string{
			{"packaging/systemd/trellis-voiced.service", "/usr/lib/systemd/system/trellis-voiced.service"},
			{"packaging/udev/70-trellis-voice.rules", "/usr/lib/udev/rules.d/70-trellis-voice.rules"},
		} {
			if err := file(&files, filepath.Join(c.root, f[0]), f[1], 0o644); err != nil {
				return err
			}
		}
		// libasound2t64 first: on Ubuntu 24.04 the virtual libasound2 can be
		// met by liboss4-salsa-asound2, which lacks the PCM API.
		recommends += ", libopus0, libasound2t64 | libasound2"
		// Voice was a package of its own before 0.3: this one takes its
		// files, and dpkg removes it in the same install.
		takeover = []deb.Field{{Name: "Conflicts", Value: "trellis-voice"}, {Name: "Replaces", Value: "trellis-voice"}}
		about = " This package holds the single trellis binary (meshd, meshctl, webd),\n" +
			" push-to-talk voice (trellis-voiced, off until voice.enable), their\n" +
			" services, and the scripts that set up radios it supports."
	}
	// The hardware scripts and what they install, laid out as in the
	// repository so they find each other. Only the scripts a node runs: the
	// development machine's (deploy, release, tests) stay behind.
	nodeScript := func(rel string) bool {
		return !strings.Contains(rel, "/") && (strings.HasPrefix(rel, "install-") || strings.HasPrefix(rel, "repair-"))
	}
	if err := tree(&files, c.root, "scripts", "/usr/share/trellis/scripts", func(rel string) bool { return !nodeScript(rel) }); err != nil {
		return err
	}
	for _, d := range []string{"dkms", "dkms-mt7915e", "dkms-morse", "overlays", "regdb"} {
		if err := tree(&files, c.root, "packaging/"+d, "/usr/share/trellis/packaging/"+d, nil); err != nil {
			return err
		}
	}
	files = append(files, copyright("trellis", `Meridian Trellis
Copyright (C) 2026 Eunoia IT Solutions LLC.

Licensed under the GNU General Public License, version 3 only; on Debian
systems its text is in /usr/share/common-licenses/GPL-3. A commercial
licence is also available: see LICENSING.md in the source.

The regulatory database under /usr/share/trellis/packaging/regdb is
wireless-regdb's, under the ISC licence beside it.

Source: `+homepage+"\n"))
	sc, err := scripts(c.root, "trellis")
	if err != nil {
		return err
	}
	control := []deb.Field{
		{Name: "Package", Value: "trellis"},
		{Name: "Version", Value: version.Debian(*ver)},
		{Name: "Architecture", Value: *arch},
		{Name: "Maintainer", Value: maintainer},
		{Name: "Section", Value: "net"},
		{Name: "Priority", Value: "optional"},
		{Name: "Homepage", Value: homepage},
		{Name: "Depends", Value: "wpasupplicant, hostapd, dnsmasq-base, nftables, iw, wireless-regdb, iproute2, kmod, udev, chrony"},
		{Name: "Recommends", Value: recommends},
	}
	control = append(control, takeover...)
	control = append(control, []deb.Field{
		{Name: "Trellis-Version", Value: *ver},
		{Name: "Description", Value: "Meridian Trellis mesh node\n" +
			" A self-forming, encrypted IP mesh: 802.11s and SAE radio links with\n" +
			" batman-adv routing, one flat client network, shared internet with\n" +
			" failover, efficient multicast for ATAK, and a web UI.\n" +
			" .\n" + about + " It never\n" +
			" ships or overwrites a node's configuration, mesh profile or identity."},
	}...)
	return c.write(&deb.Package{
		Control:   control,
		Scripts:   sc,
		Conffiles: []string{"/etc/chrony/conf.d/trellis.conf"},
		Files:     files,
		Time:      c.when(),
	})
}

// pin is packaging/dkms/source: the batman-adv release and its checksum.
type pin struct{ version, url, sha256, revision string }

func readPin(root string) (pin, error) {
	b, err := os.ReadFile(filepath.Join(root, "packaging/dkms/source"))
	if err != nil {
		return pin{}, err
	}
	var p pin
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || strings.HasPrefix(k, "#") {
			continue
		}
		switch k {
		case "BATADV_VERSION":
			p.version = v
		case "BATADV_URL":
			p.url = v
		case "BATADV_SHA256":
			p.sha256 = v
		case "REVISION":
			p.revision = v
		}
	}
	if p.version == "" || p.sha256 == "" || p.revision == "" {
		return pin{}, errors.New("packaging/dkms/source is incomplete")
	}
	return p, nil
}

func cmdBatman(args []string) error {
	fl := flag.NewFlagSet("batman-adv", flag.ExitOnError)
	var c common
	c.register(fl)
	src := fl.String("source", "", "the batman-adv release tarball named in packaging/dkms/source")
	fl.Parse(args)
	p, err := readPin(c.root)
	if err != nil {
		return err
	}
	if *src == "" {
		return fmt.Errorf("batman-adv needs --source: the release tarball from %s", p.url)
	}
	raw, err := os.ReadFile(*src)
	if err != nil {
		return err
	}
	if sum := sha256.Sum256(raw); hex.EncodeToString(sum[:]) != p.sha256 {
		return fmt.Errorf("%s is not the pinned batman-adv %s (SHA-256 %x, want %s)", *src, p.version, sum, p.sha256)
	}
	// The DKMS version carries the packaging revision: a new revision
	// (new patches) is a new DKMS module, built before the old one goes.
	dkmsVersion := p.version + "-" + p.revision
	dst := "/usr/src/trellis-batman-adv-" + dkmsVersion
	var files []deb.File
	f, err := os.Open(*src)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		// Drop the tarball's top directory.
		_, rel, ok := strings.Cut(path.Clean(h.Name), "/")
		if !ok || h.Typeflag != tar.TypeReg {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		mode := fs.FileMode(0o644)
		if h.Mode&0o111 != 0 {
			mode = 0o755
		}
		files = append(files, deb.File{Path: path.Join(dst, rel), Mode: mode, Body: body})
	}
	conf, err := os.ReadFile(filepath.Join(c.root, "packaging/dkms/dkms.conf"))
	if err != nil {
		return err
	}
	files = append(files, deb.File{Path: dst + "/dkms.conf", Mode: 0o644,
		Body: []byte(strings.ReplaceAll(string(conf), "@VERSION@", dkmsVersion))})
	if err := file(&files, filepath.Join(c.root, "packaging/dkms/trellis-build.sh"), dst+"/trellis-build.sh", 0o755); err != nil {
		return err
	}
	patches, err := filepath.Glob(filepath.Join(c.root, "packaging/dkms/patches/*.patch"))
	if err != nil {
		return err
	}
	for _, p := range patches {
		if err := file(&files, p, dst+"/patches/"+filepath.Base(p), 0o644); err != nil {
			return err
		}
	}
	files = append(files, copyright("trellis-batman-adv-dkms", `batman-adv `+p.version+`
Copyright (C) B.A.T.M.A.N. contributors; see the source files.
Licensed under the GNU General Public License, version 2; on Debian
systems its text is in /usr/share/common-licenses/GPL-2.
Source: `+p.url+`

The DKMS packaging is part of Meridian Trellis,
Copyright (C) 2026 Eunoia IT Solutions LLC, GPL-3.0-only.
`))
	sc, err := scripts(c.root, "dkms")
	if err != nil {
		return err
	}
	for n, body := range sc {
		sc[n] = []byte(strings.NewReplacer("@BATADV_VERSION@", p.version, "@DKMS_VERSION@", dkmsVersion).Replace(string(body)))
	}
	return c.write(&deb.Package{
		Control: []deb.Field{
			{Name: "Package", Value: "trellis-batman-adv-dkms"},
			{Name: "Version", Value: p.version + "-" + p.revision},
			{Name: "Architecture", Value: "all"},
			{Name: "Maintainer", Value: maintainer},
			{Name: "Section", Value: "kernel"},
			{Name: "Priority", Value: "optional"},
			{Name: "Homepage", Value: homepage},
			// trellis: the build script and its helpers live there.
			{Name: "Depends", Value: "trellis, dkms, make, gcc"},
			{Name: "Description", Value: "batman-adv " + p.version + " for Trellis, built by DKMS\n" +
				" Every node of a Trellis mesh runs the same batman-adv release, whatever\n" +
				" its kernel. This package holds that release's source; DKMS builds it for\n" +
				" the running kernel when the package is installed and again after every\n" +
				" kernel update. The module takes precedence over the kernel's own."},
		},
		Scripts: sc,
		Files:   files,
		Time:    c.when(),
	})
}

func cmdKeygen(args []string) error {
	fl := flag.NewFlagSet("keygen", flag.ExitOnError)
	out := fl.String("out", "", "where the secret key is written (mode 0600; never overwritten)")
	comment := fl.String("comment", "", "text kept with the public key, such as who holds it")
	fl.Parse(args)
	if *out == "" {
		return errors.New("keygen needs --out")
	}
	secret, public, err := deb.NewKey(*comment)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(secret); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "secret key written to %s; keep it off the network. The public key:\n", *out)
	fmt.Print(public)
	return nil
}

// cmdPubkey prints the public line of a secret key.
func cmdPubkey(args []string) error {
	fl := flag.NewFlagSet("pubkey", flag.ExitOnError)
	key := fl.String("key", "", "release secret key file")
	comment := fl.String("comment", "", "text kept with the public key")
	fl.Parse(args)
	if *key == "" {
		return errors.New("usage: mkdeb pubkey --key FILE")
	}
	priv, err := readSecret(*key)
	if err != nil {
		return err
	}
	fmt.Print(deb.Public(priv, *comment))
	return nil
}

func cmdSign(args []string) error {
	fl := flag.NewFlagSet("sign", flag.ExitOnError)
	key := fl.String("key", "", "release secret key file")
	dev := fl.Bool("sign-dev", false, "sign even a version that is not an exact release")
	fl.Parse(args)
	if *key == "" || fl.NArg() == 0 {
		return errors.New("usage: mkdeb sign --key FILE PACKAGE.deb...")
	}
	priv, err := readSecret(*key)
	if err != nil {
		return err
	}
	for _, name := range fl.Args() {
		b, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		info, err := deb.Read(b)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if v := version.Parse(info.Get("Trellis-Version")); info.Get("Package") == "trellis" && (!v.Numbered || !v.Exact) && !*dev {
			return fmt.Errorf("%s: refusing to sign %s, which is not a release; --sign-dev to sign it anyway", name, info.Get("Trellis-Version"))
		}
		signed, err := deb.Sign(b, priv, time.Now().UTC())
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := os.WriteFile(name, signed, 0o644); err != nil {
			return err
		}
		fmt.Printf("%s: signed\n", name)
	}
	return nil
}

func cmdVerify(args []string) error {
	fl := flag.NewFlagSet("verify", flag.ExitOnError)
	pub := fl.String("pub", "", "file of public keys to trust")
	fl.Parse(args)
	if *pub == "" || fl.NArg() == 0 {
		return errors.New("usage: mkdeb verify --pub KEYS PACKAGE.deb...")
	}
	raw, err := os.ReadFile(*pub)
	if err != nil {
		return err
	}
	keys, err := deb.ParsePublicKeys(string(raw))
	if err != nil {
		return err
	}
	for _, name := range fl.Args() {
		b, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		info, err := deb.Read(b)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		k, err := info.Verify(keys)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		fmt.Printf("%s: %s %s (%s), signed by %s %s\n", name, info.Get("Package"), info.Get("Version"),
			info.Get("Architecture"), k.ID(), k.Comment)
	}
	return nil
}
