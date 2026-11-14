package webd

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

// The UI is built into the binary: plain HTML, CSS
// and JavaScript modules, no build step and nothing fetched from outside
// the node, because a mesh node often has no internet.

//go:embed ui
var uiFS embed.FS

type asset struct {
	ctype string
	etag  string
	raw   []byte
	gz    []byte // nil when compression does not help
}

type assets struct {
	files map[string]*asset // by URL path, e.g. "/app.css"
}

func loadAssets() (*assets, error) {
	as := &assets{files: map[string]*asset{}}
	root, err := fs.Sub(uiFS, "ui")
	if err != nil {
		return nil, err
	}
	err = fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := fs.ReadFile(root, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		a := &asset{ctype: contentType(p), etag: `"` + hex.EncodeToString(sum[:8]) + `"`, raw: raw}
		if compressible(p) {
			var buf bytes.Buffer
			zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
			zw.Write(raw)
			zw.Close()
			if buf.Len() < len(raw) {
				a.gz = buf.Bytes()
			}
		}
		as.files["/"+p] = a
		return nil
	})
	return as, err
}

func contentType(p string) string {
	switch path.Ext(p) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".otf":
		return "font/otf"
	case ".ttf":
		return "font/ttf"
	case ".woff2":
		return "font/woff2"
	case ".txt", ".md":
		return "text/plain; charset=utf-8"
	case ".json", ".webmanifest":
		return "application/json"
	}
	if t := mime.TypeByExtension(path.Ext(p)); t != "" {
		return t
	}
	return "application/octet-stream"
}

func compressible(p string) bool {
	switch path.Ext(p) {
	case ".woff2", ".png", ".jpg":
		return false
	}
	return true
}

// serve answers a request for a UI file. Files are revalidated on every
// load (ETag), so an upgraded node never serves a page from before.
func (as *assets) serve(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if p == "/" {
		p = "/index.html"
	}
	a, ok := as.files[p]
	if !ok {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", a.ctype)
	h.Set("ETag", a.etag)
	h.Set("Cache-Control", "no-cache")
	h.Add("Vary", "Accept-Encoding")
	if r.Header.Get("If-None-Match") == a.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	body := a.raw
	if a.gz != nil && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		h.Set("Content-Encoding", "gzip")
		body = a.gz
	}
	if r.Method == http.MethodHead {
		return
	}
	w.Write(body)
}
