// Package halowfiles gets Morse Micro's firmware and board configuration
// file (BCF) onto a node with a HaLow module, with no internet in the
// field and no step by hand. Every file is named by the SHA-256 pinned in
// packaging/dkms-morse/firmware.pins and checked against it, whatever its
// source:
//
//   - the image (the firmware) or the card (trellis firstboot);
//   - another node of the mesh that holds them (D): over the package
//     server on the mesh (port 7373, /halow/<sha256>), which serves only
//     pinned files;
//   - Morse's repository at the pinned commit, over the node's uplink (C).
//
// meshd fetches into its state directory; the files go into
// /lib/firmware/morse through the helper (trellis halow install), outside
// meshd's sandbox, which also restarts the driver.
//
// Only a file the node lacks is fetched. A file that is there under a
// pinned name with other content is the operator's (their own BCF, a
// trial's): it is never replaced, and the node then does not serve the
// pinned set.
package halowfiles

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"meridian/trellis/internal/fsutil"
)

// Paths on a node.
const (
	DefaultPinsFile = "/usr/share/trellis/packaging/dkms-morse/firmware.pins"
	FirmwareDir     = "/lib/firmware/morse"
	LicenseName     = "LICENSE"
	maxFile         = 8 << 20
	maxLicense      = 256 << 10
)

// File is one pinned file.
type File struct {
	Name   string // its name in FirmwareDir: mm6108.bin
	Path   string // its path in Morse's repository: firmware/mm6108.bin
	SHA256 string
}

// Pins is firmware.pins.
type Pins struct {
	Repo, Ref string
	Files     []File // the firmware, then the BCF
	License   string // the licence's path in Morse's repository
}

// LoadPins reads firmware.pins (KEY=VALUE lines).
func LoadPins(path string) (*Pins, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	kv := map[string]string{}
	for _, l := range strings.Split(string(data), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if k, v, ok := strings.Cut(l, "="); ok {
			kv[k] = v
		}
	}
	p := &Pins{Repo: kv["FW_REPO"], Ref: kv["FW_REF"], License: kv["LICENSE_FILE"]}
	for _, k := range []string{"FW", "BCF"} {
		path, sum := kv[k+"_FILE"], kv[k+"_SHA256"]
		if path == "" || len(sum) != 64 {
			return nil, fmt.Errorf("%s: %s_FILE or %s_SHA256 missing", path, k, k)
		}
		p.Files = append(p.Files, File{Name: filepath.Base(path), Path: path, SHA256: sum})
	}
	return p, nil
}

// ByHash finds the pinned file with this hash.
func (p *Pins) ByHash(sum string) (File, bool) {
	for _, f := range p.Files {
		if f.SHA256 == sum {
			return f, true
		}
	}
	return File{}, false
}

func fileSum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, maxFile+1)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Missing lists the pinned files dir lacks or holds with another hash:
// while there are any, the node does not hold the pinned set.
func (p *Pins) Missing(dir string) []File {
	var out []File
	for _, f := range p.Files {
		if sum, err := fileSum(filepath.Join(dir, f.Name)); err != nil || sum != f.SHA256 {
			out = append(out, f)
		}
	}
	return out
}

// Absent lists the pinned files dir has no file for. Only these are
// fetched: a file that is there under a pinned name with another hash is
// the operator's (their own BCF, a trial's) and is never replaced.
func (p *Pins) Absent(dir string) []File {
	var out []File
	for _, f := range p.Files {
		if _, err := os.Lstat(filepath.Join(dir, f.Name)); errors.Is(err, os.ErrNotExist) {
			out = append(out, f)
		}
	}
	return out
}

// ModulePresent says whether the device tree has a Morse SPI module (the
// WM6108 overlay): the node has one whether or not its driver runs.
func ModulePresent(procRoot string) bool {
	if procRoot == "" {
		procRoot = "/"
	}
	m, _ := filepath.Glob(filepath.Join(procRoot, "proc/device-tree/soc/spi@*/mm6108@0"))
	return len(m) > 0
}

// Get fetches url into dir/name, checked against sum ("" for the licence,
// which has no pin), and returns the path.
func Get(ctx context.Context, client *http.Client, url, dir, name, sum string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "trellis")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", url, resp.Status)
	}
	limit := int64(maxFile)
	if sum == "" {
		limit = maxLicense
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return "", err
	}
	if int64(len(data)) > limit {
		return "", fmt.Errorf("%s: too large", url)
	}
	if sum != "" {
		h := sha256.Sum256(data)
		if got := hex.EncodeToString(h[:]); got != sum {
			return "", fmt.Errorf("%s: SHA-256 %s, not the pinned %s", url, got, sum)
		}
	}
	dest := filepath.Join(dir, name)
	return dest, fsutil.WriteFile(dest, data, 0o644)
}

// UpstreamURL is a pinned file's address in Morse's repository.
func (p *Pins) UpstreamURL(path string) string {
	repo := strings.TrimSuffix(strings.TrimPrefix(p.Repo, "https://github.com/"), ".git")
	return "https://raw.githubusercontent.com/" + repo + "/" + p.Ref + "/" + path
}

// MeshURL is a file's address on another node's package server.
func MeshURL(addr string, port int, sumOrLicense string) string {
	return "http://" + net.JoinHostPort(addr, fmt.Sprint(port)) + "/halow/" + sumOrLicense
}

// Handler serves /halow/<sha256> (a pinned file this node holds, checked
// as it is read) and /halow/LICENSE, to the addresses allowed accepts.
func Handler(pins func() *Pins, dir string, allowed func(net.IP) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if ip := net.ParseIP(host); ip == nil || !allowed(ip) {
			http.Error(w, "not a node of this mesh", http.StatusForbidden)
			return
		}
		name, _ := strings.CutPrefix(r.URL.Path, "/halow/")
		p := pins()
		if r.Method != http.MethodGet || p == nil {
			http.NotFound(w, r)
			return
		}
		var path, sum string
		if name == LicenseName {
			path = filepath.Join(dir, LicenseName)
		} else if f, ok := p.ByHash(name); ok {
			path, sum = filepath.Join(dir, f.Name), f.SHA256
		} else {
			http.NotFound(w, r)
			return
		}
		data, err := os.ReadFile(path)
		if err != nil || len(data) > maxFile {
			http.NotFound(w, r)
			return
		}
		if sum != "" {
			if h := sha256.Sum256(data); hex.EncodeToString(h[:]) != sum {
				http.NotFound(w, r) // never pass on a file that is not the pinned one
				return
			}
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(data)
	})
}

// Install is trellis halow install --from DIR, as root: the pinned files
// in DIR (checked again) and the licence go into FirmwareDir, where it has
// no file of that name; then the driver restarts unless a HaLow interface
// is up.
func Install(ctx context.Context, pinsPath, from string, stderr io.Writer) error {
	if os.Geteuid() != 0 {
		return errors.New("trellis halow install: run as root (meshd runs it)")
	}
	p, err := LoadPins(pinsPath)
	if err != nil {
		return err
	}
	if err := installFiles(p, from, FirmwareDir, stderr); err != nil {
		return err
	}
	// The driver read no firmware when it loaded: load it again.
	if up, _ := filepath.Glob("/sys/bus/spi/devices/spi0.0/net/*"); len(up) > 0 {
		fmt.Fprintln(stderr, "a HaLow interface exists: the driver loads the files at its next restart")
		return nil
	}
	_ = exec.CommandContext(ctx, "modprobe", "-r", "morse").Run()
	if out, err := exec.CommandContext(ctx, "modprobe", "morse").CombinedOutput(); err != nil {
		return fmt.Errorf("modprobe morse: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// installFiles copies the pinned files of from (checked again) and the
// licence into dir. A file dir already has under a pinned name stays as
// it is, whatever it holds.
func installFiles(p *Pins, from, dir string, stderr io.Writer) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, f := range p.Files {
		src := filepath.Join(from, f.Name)
		sum, err := fileSum(src)
		if errors.Is(err, os.ErrNotExist) {
			continue // what the node had already
		}
		if err != nil || sum != f.SHA256 {
			return fmt.Errorf("%s: not the pinned file", f.Name)
		}
		dest := filepath.Join(dir, f.Name)
		if _, err := os.Lstat(dest); err == nil {
			fmt.Fprintf(stderr, "%s: a file is there already, left as it is\n", f.Name)
			continue
		}
		data, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		if err := fsutil.WriteFile(dest, data, 0o644); err != nil {
			return err
		}
	}
	if data, err := os.ReadFile(filepath.Join(from, LicenseName)); err == nil {
		_ = fsutil.WriteFile(filepath.Join(dir, LicenseName), data, 0o644)
	}
	if absent := p.Absent(dir); len(absent) > 0 {
		return fmt.Errorf("still missing: %s", absent[0].Name)
	}
	return nil
}

// RunInstall runs the helper as a transient unit from meshd's sandbox and
// waits for it.
func RunInstall(ctx context.Context, bin, from string) error {
	if bin == "" {
		bin = "/usr/bin/trellis"
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemd-run", "--quiet", "--pipe", "--wait", "--collect", "--service-type=exec",
		"--description=Trellis: HaLow firmware and BCF", bin, "halow", "install", "--from", from).CombinedOutput()
	if err != nil {
		return fmt.Errorf("trellis halow install: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
