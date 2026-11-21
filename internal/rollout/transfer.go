package rollout

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
	"path/filepath"
	"strings"
	"time"
)

// Packages travel between nodes over plain HTTP on the mesh. The transport
// is not trusted: a package is named by its SHA-256, checked against it on
// arrival, and its signature is checked by each node with its own release
// keys before anything is installed.

// Port is where a node serves the packages of a rollout (TCP; the gossip
// port's number).
const Port = 7373

// MaxPackage bounds a package fetched from another node.
const MaxPackage = 64 << 20

// maxServing bounds the transfers a node serves at once.
const maxServing = 4

// File is where a package is kept, by hash, in dir.
func File(dir, sha string) string { return filepath.Join(dir, sha+".deb") }

// Handler serves GET /release/<sha256> from dir, for the hashes wanted
// says belong to the current rollout, to the remote addresses allowed
// accepts (the nodes known in gossip).
func Handler(dir string, wanted func(sha string) bool, allowed func(ip net.IP) bool) http.Handler {
	slots := make(chan struct{}, maxServing)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if ip := net.ParseIP(host); ip == nil || !allowed(ip) {
			http.Error(w, "not a node of this mesh", http.StatusForbidden)
			return
		}
		sha, ok := strings.CutPrefix(r.URL.Path, "/release/")
		if r.Method != http.MethodGet || !ok || len(sha) != 64 || !isHex(sha) || !wanted(sha) {
			http.NotFound(w, r)
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		f, err := os.Open(File(dir, sha))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.debian.binary-package")
		http.ServeContent(w, r, "", fi.ModTime(), f)
	})
}

// Server is the HTTP server for Handler, with timeouts that let a slow
// mesh link finish a package but not hold a connection open for ever.
func Server(h http.Handler) *http.Server {
	return &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, WriteTimeout: 10 * time.Minute, IdleTimeout: 30 * time.Second}
}

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

// ErrHash: what arrived is not the package asked for.
var ErrHash = errors.New("the package that arrived does not match its hash")

// Fetch gets package p from the node at addr into dir, checks its size and
// hash, and returns its path. A package already there is checked and kept.
func Fetch(ctx context.Context, client *http.Client, addr string, p Package, dir string) (string, error) {
	dest := File(dir, p.SHA256)
	if ok, _ := hasFile(dest, p.SHA256); ok {
		return dest, nil
	}
	if p.Size <= 0 || p.Size > MaxPackage {
		return "", fmt.Errorf("package of %d bytes", p.Size)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	url := "http://" + net.JoinHostPort(addr, fmt.Sprint(Port)) + "/release/" + p.SHA256
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s answered %s", addr, resp.Status)
	}
	tmp, err := os.CreateTemp(dir, ".fetch-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, p.Size+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("from %s: %w", addr, err)
	}
	if n != p.Size || hex.EncodeToString(h.Sum(nil)) != p.SHA256 {
		return "", fmt.Errorf("from %s: %w", addr, ErrHash)
	}
	if err := os.Rename(tmp.Name(), dest); err != nil {
		return "", err
	}
	return dest, nil
}

// Keep stores a package the operator gave this node (the origin) in dir and
// returns its description.
func Keep(dir, arch string, pkg []byte) (Package, error) {
	sum := sha256.Sum256(pkg)
	p := Package{Arch: arch, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(pkg))}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return p, err
	}
	if ok, _ := hasFile(File(dir, p.SHA256), p.SHA256); ok {
		return p, nil
	}
	tmp, err := os.CreateTemp(dir, ".keep-*")
	if err != nil {
		return p, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(pkg); err != nil {
		tmp.Close()
		return p, err
	}
	if err := tmp.Close(); err != nil {
		return p, err
	}
	return p, os.Rename(tmp.Name(), File(dir, p.SHA256))
}

// leftoverAge is how long a temporary file of Fetch or Keep goes unwritten
// before Prune takes it for one a power loss left behind. A transfer still
// running writes to it all the time, and none runs past the server's
// 10 min write timeout.
const leftoverAge = time.Hour

// Prune removes every package in dir that keep does not name, and the
// temporary files of transfers cut short.
func Prune(dir string, keep func(sha string) bool) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if strings.HasPrefix(e.Name(), ".") {
			if fi, err := e.Info(); err == nil && fi.Mode().IsRegular() && time.Since(fi.ModTime()) > leftoverAge {
				_ = os.Remove(p)
			}
			continue
		}
		if sha, ok := strings.CutSuffix(e.Name(), ".deb"); ok && keep(sha) {
			continue
		}
		_ = os.Remove(p)
	}
}

func hasFile(path, sha string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == sha, nil
}
