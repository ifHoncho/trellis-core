// Package deb writes and reads Debian binary packages, and signs them.
//
// It exists so that a release can be built on any machine (no dpkg), so
// that a node can check a package before installing it (no gpg), and so
// that an upgrade can rebuild the package of the version it replaces, to
// go back to.
//
// A package is an ar archive of three members: "debian-binary",
// "control.tar.gz" and "data.tar.gz". A signed one has a fourth,
// "_trellis-sig": dpkg skips members whose names start with an underscore,
// so a signed package installs like any other.
package deb

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// File is one entry of a package's payload. Path is absolute on the
// installed system ("/usr/bin/trellis"). Mode carries the type: a
// directory (fs.ModeDir), a symlink (fs.ModeSymlink, with Link) or a
// regular file (with Body).
type File struct {
	Path string
	Mode fs.FileMode
	Body []byte
	Link string
}

// Field is one control field, in the order it is written.
type Field struct{ Name, Value string }

// Package is a binary package to write.
type Package struct {
	// Control holds every field but Installed-Size, which Write computes.
	// A multi-line value (Description) has its continuation lines already
	// indented by one space.
	Control []Field
	// Scripts are the maintainer scripts by name: preinst, postinst,
	// prerm, postrm.
	Scripts map[string][]byte
	// Conffiles are paths dpkg must never overwrite once changed.
	Conffiles []string
	Files     []File
	// Time stamps every entry, so the same input gives the same bytes.
	Time time.Time
}

// Get returns a control field's value.
func (p *Package) Get(name string) string {
	for _, f := range p.Control {
		if strings.EqualFold(f.Name, name) {
			return f.Value
		}
	}
	return ""
}

// Filename is the conventional name: package_version_architecture.deb.
func (p *Package) Filename() string {
	return fmt.Sprintf("%s_%s_%s.deb", p.Get("Package"), p.Get("Version"), p.Get("Architecture"))
}

// Bytes returns the package as an unsigned .deb.
func (p *Package) Bytes() ([]byte, error) {
	for _, need := range []string{"Package", "Version", "Architecture", "Maintainer", "Description"} {
		if p.Get(need) == "" {
			return nil, fmt.Errorf("control field %s is missing", need)
		}
	}
	files, err := p.payload()
	if err != nil {
		return nil, err
	}
	data, err := tarball(files, p.Time)
	if err != nil {
		return nil, err
	}
	ctl, err := tarball(p.controlFiles(files), p.Time)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.WriteString(arMagic)
	arMember(&out, "debian-binary", []byte("2.0\n"), p.Time)
	arMember(&out, "control.tar.gz", ctl, p.Time)
	arMember(&out, "data.tar.gz", data, p.Time)
	return out.Bytes(), nil
}

// payload returns the files sorted, with every parent directory present.
func (p *Package) payload() ([]File, error) {
	seen := map[string]bool{}
	var out []File
	for _, f := range p.Files {
		clean := path.Clean(f.Path)
		if !strings.HasPrefix(clean, "/") || clean == "/" {
			return nil, fmt.Errorf("payload path %q is not absolute", f.Path)
		}
		if seen[clean] {
			return nil, fmt.Errorf("payload path %q is listed twice", clean)
		}
		seen[clean] = true
		f.Path = clean
		out = append(out, f)
	}
	for _, f := range append([]File(nil), out...) {
		for d := path.Dir(f.Path); d != "/"; d = path.Dir(d) {
			if !seen[d] {
				seen[d] = true
				out = append(out, File{Path: d, Mode: fs.ModeDir | 0o755})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// controlFiles builds the control archive: control, md5sums, conffiles and
// the maintainer scripts.
func (p *Package) controlFiles(files []File) []File {
	var kib int64
	var sums bytes.Buffer
	for _, f := range files {
		if f.Mode.IsRegular() {
			kib += (int64(len(f.Body)) + 1023) / 1024
			fmt.Fprintf(&sums, "%x  %s\n", md5.Sum(f.Body), strings.TrimPrefix(f.Path, "/"))
		} else {
			kib++
		}
	}
	var ctl bytes.Buffer
	for _, f := range p.Control {
		if strings.EqualFold(f.Name, "Installed-Size") {
			continue
		}
		fmt.Fprintf(&ctl, "%s: %s\n", f.Name, f.Value)
		if strings.EqualFold(f.Name, "Architecture") {
			fmt.Fprintf(&ctl, "Installed-Size: %d\n", kib)
		}
	}
	out := []File{
		{Path: "/control", Mode: 0o644, Body: ctl.Bytes()},
		{Path: "/md5sums", Mode: 0o644, Body: sums.Bytes()},
	}
	if len(p.Conffiles) > 0 {
		out = append(out, File{Path: "/conffiles", Mode: 0o644, Body: []byte(strings.Join(p.Conffiles, "\n") + "\n")})
	}
	names := make([]string, 0, len(p.Scripts))
	for n := range p.Scripts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		out = append(out, File{Path: "/" + n, Mode: 0o755, Body: p.Scripts[n]})
	}
	return out
}

// tarball writes files as a gzip-compressed tar, every entry owned by root.
func tarball(files []File, when time.Time) ([]byte, error) {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	tw := tar.NewWriter(zw)
	for _, f := range files {
		h := &tar.Header{
			Name: "." + f.Path, Mode: int64(f.Mode.Perm()), ModTime: when,
			Uname: "root", Gname: "root", Format: tar.FormatGNU,
		}
		switch {
		case f.Mode.IsDir():
			h.Typeflag, h.Name = tar.TypeDir, h.Name+"/"
		case f.Mode&fs.ModeSymlink != 0:
			h.Typeflag, h.Linkname, h.Mode = tar.TypeSymlink, f.Link, 0o777
		case f.Mode.IsRegular():
			h.Typeflag, h.Size = tar.TypeReg, int64(len(f.Body))
		default:
			return nil, fmt.Errorf("%s: only files, directories and symlinks can be packaged", f.Path)
		}
		if err := tw.WriteHeader(h); err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write(f.Body); err != nil {
				return nil, err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

const (
	arMagic     = "!<arch>\n"
	arHeaderLen = 60
)

// arMember appends one member, padded to an even length.
func arMember(w *bytes.Buffer, name string, body []byte, when time.Time) {
	fmt.Fprintf(w, "%-16s%-12d%-6d%-6d%-8s%-10d`\n", name, when.Unix(), 0, 0, "100644", len(body))
	w.Write(body)
	if len(body)%2 == 1 {
		w.WriteByte('\n')
	}
}

// member is one ar member of a package that was read.
type member struct {
	name   string
	offset int // of its header in the archive
	body   []byte
}

// members splits an ar archive.
func members(b []byte) ([]member, error) {
	if !bytes.HasPrefix(b, []byte(arMagic)) {
		return nil, errors.New("not a Debian package (no ar archive header)")
	}
	var out []member
	for off := len(arMagic); off < len(b); {
		if off+arHeaderLen > len(b) {
			return nil, errors.New("package is cut short (member header)")
		}
		h := b[off : off+arHeaderLen]
		if h[58] != '`' || h[59] != '\n' {
			return nil, errors.New("package is damaged (member header)")
		}
		size, err := strconv.Atoi(strings.TrimSpace(string(h[48:58])))
		if err != nil || size < 0 || off+arHeaderLen+size > len(b) {
			return nil, errors.New("package is cut short (member body)")
		}
		name := strings.TrimSuffix(strings.TrimSpace(string(h[:16])), "/")
		out = append(out, member{name: name, offset: off, body: b[off+arHeaderLen : off+arHeaderLen+size]})
		off += arHeaderLen + size + size%2
	}
	return out, nil
}

// Limits on what is unpacked from a package.
const (
	maxControl = 4 << 20   // the control archive, unpacked
	maxPayload = 512 << 20 // the files, unpacked (Payload)
)

// Info is what a node needs to know about a package before installing it.
type Info struct {
	Control []Field
	// signed is the part of the file a signature covers, and sig the
	// signature member's text; both empty for an unsigned package.
	signed []byte
	sig    string
}

// Get returns a control field's value.
func (i *Info) Get(name string) string {
	for _, f := range i.Control {
		if strings.EqualFold(f.Name, name) {
			return f.Value
		}
	}
	return ""
}

// Read parses a .deb far enough to identify it: its control fields, and
// its signature if it has one.
func Read(b []byte) (*Info, error) {
	ms, err := members(b)
	if err != nil {
		return nil, err
	}
	if len(ms) < 3 || ms[0].name != "debian-binary" || !bytes.HasPrefix(ms[0].body, []byte("2.")) {
		return nil, errors.New("not a Debian package (no debian-binary member)")
	}
	info := &Info{}
	var ctl []byte
	for i, m := range ms {
		switch {
		case m.name == "control.tar.gz":
			ctl = m.body
		case strings.HasPrefix(m.name, "control.tar"):
			return nil, fmt.Errorf("control archive %s: only gzip is read here", m.name)
		case m.name == SigMember:
			if i != len(ms)-1 {
				return nil, errors.New("the signature is not the package's last member")
			}
			info.signed, info.sig = b[:m.offset], string(m.body)
		}
	}
	if ctl == nil {
		return nil, errors.New("package has no control archive")
	}
	zr, err := gzip.NewReader(bytes.NewReader(ctl))
	if err != nil {
		return nil, fmt.Errorf("control archive: %w", err)
	}
	// A control archive is a few kilobytes. Reading stops well past that,
	// so a small file that unpacks to gigabytes costs nothing (packages
	// come from browsers, to a daemon running as root).
	tr := tar.NewReader(io.LimitReader(zr, maxControl))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, errors.New("package has no control file")
		}
		if err != nil {
			return nil, fmt.Errorf("control archive: %w", err)
		}
		if path.Clean("/"+h.Name) != "/control" {
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(tr, 1<<20))
		if err != nil {
			return nil, fmt.Errorf("control file: %w", err)
		}
		info.Control = ParseControl(string(raw))
		if info.Get("Package") == "" || info.Get("Version") == "" {
			return nil, errors.New("control file names no package or version")
		}
		return info, nil
	}
}

// ParseControl reads one control stanza. Continuation lines stay part of
// their field's value, indented as they were.
func ParseControl(s string) []Field {
	var out []Field
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if line == "" {
			break
		}
		if (line[0] == ' ' || line[0] == '\t') && len(out) > 0 {
			out[len(out)-1].Value += "\n" + line
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		out = append(out, Field{Name: name, Value: strings.TrimSpace(value)})
	}
	return out
}

// Payload lists the files a package installs, with their contents.
func Payload(b []byte) ([]File, error) {
	ms, err := members(b)
	if err != nil {
		return nil, err
	}
	for _, m := range ms {
		if m.name != "data.tar.gz" {
			continue
		}
		zr, err := gzip.NewReader(bytes.NewReader(m.body))
		if err != nil {
			return nil, err
		}
		var out []File
		lim := &io.LimitedReader{R: zr, N: maxPayload}
		tr := tar.NewReader(lim)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				return out, nil
			}
			if err != nil {
				if lim.N <= 0 {
					return nil, fmt.Errorf("the package's files unpack to more than %d MB", maxPayload>>20)
				}
				return nil, err
			}
			f := File{Path: path.Clean("/" + h.Name), Mode: fs.FileMode(h.Mode).Perm()}
			switch h.Typeflag {
			case tar.TypeDir:
				f.Mode |= fs.ModeDir
			case tar.TypeSymlink:
				f.Mode, f.Link = fs.ModeSymlink|0o777, h.Linkname
			case tar.TypeReg:
				if f.Body, err = io.ReadAll(tr); err != nil {
					return nil, err
				}
			default:
				continue
			}
			out = append(out, f)
		}
	}
	return nil, errors.New("package has no data.tar.gz")
}
