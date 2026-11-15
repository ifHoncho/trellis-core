package deploy

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"meridian/trellis/internal/deb"
)

// Release is a set of packages checked and ready to deploy.
type Release struct {
	Dir     string
	Version string            // the Trellis version ("0.2.0")
	Debian  string            // the trellis package's Debian version
	Arches  map[string]string // Debian architecture → trellis package file
	Batman  string            // the trellis-batman-adv-dkms package file, if there is one
	// BatmanVersion is its Debian version.
	BatmanVersion string
	SignedBy      string // the key that signed them, as its comment says ("1")
	Checksums     bool   // SHA256SUMS was there and matched
}

// maxPackage bounds a package read into memory: the largest is about 6 MB.
const maxPackage = 64 << 20

// LoadRelease checks the packages in dir: the SHA256SUMS that lists them,
// when it is there, and the signature of each against keys. Every trellis
// package must be the same version; a package that fails a check fails the
// whole release.
func LoadRelease(dir string, keys []deb.PublicKey, requireSums bool) (*Release, error) {
	r := &Release{Dir: dir, Arches: map[string]string{}}
	sums, err := readSums(filepath.Join(dir, "SHA256SUMS"))
	switch {
	case errors.Is(err, os.ErrNotExist):
		if requireSums {
			return nil, fmt.Errorf("%s has no SHA256SUMS", dir)
		}
	case err != nil:
		return nil, err
	default:
		r.Checksums = true
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.deb"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	for _, f := range files {
		b, err := readCapped(f)
		if err != nil {
			return nil, err
		}
		name := filepath.Base(f)
		if r.Checksums {
			want, ok := sums[name]
			sum := sha256.Sum256(b)
			if !ok {
				return nil, fmt.Errorf("%s is not listed in SHA256SUMS", name)
			}
			if hex.EncodeToString(sum[:]) != want {
				return nil, fmt.Errorf("%s does not match SHA256SUMS: damaged, or not the file the release made", name)
			}
		}
		info, err := deb.Read(b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		k, err := info.Verify(keys)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if r.SignedBy == "" {
			r.SignedBy = k.Comment
			if r.SignedBy == "" {
				r.SignedBy = k.ID()
			}
		}
		switch info.Get("Package") {
		case "trellis":
			v := info.Get("Trellis-Version")
			if r.Version != "" && v != r.Version {
				return nil, fmt.Errorf("%s is Trellis %s, the others %s: one release at a time", name, v, r.Version)
			}
			r.Version, r.Debian = v, info.Get("Version")
			r.Arches[info.Get("Architecture")] = f
		case "trellis-batman-adv-dkms":
			r.Batman, r.BatmanVersion = f, info.Get("Version")
		}
	}
	if len(r.Arches) == 0 {
		return nil, fmt.Errorf("no trellis package in %s", dir)
	}
	return r, nil
}

func readCapped(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxPackage+1))
	if err == nil && len(b) > maxPackage {
		err = fmt.Errorf("%s is larger than any Trellis package", path)
	}
	return b, err
}

// readSums reads a sha256sum file: name → hex digest.
func readSums(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		sum, name, ok := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		if !ok || len(sum) != 64 {
			continue
		}
		out[strings.TrimPrefix(strings.TrimSpace(name), "*")] = strings.ToLower(sum)
	}
	return out, sc.Err()
}

// DefaultRepo is where releases are published.
const DefaultRepo = "ifHoncho/trellis-core"

type ghRelease struct {
	Tag        string    `json:"tag_name"`
	Draft      bool      `json:"draft"`
	Prerelease bool      `json:"prerelease"`
	Assets     []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// Fetcher downloads releases from GitHub.
type Fetcher struct {
	Client *http.Client
	API    string // https://api.github.com
	Repo   string // owner/name
}

// Fetch downloads a release's packages and checksums into a directory
// under cache (one per version, kept for the next run) and returns it.
// version "" is the newest release, pre-releases included.
func (f Fetcher) Fetch(ctx context.Context, version, cache string, log io.Writer) (string, error) {
	rel, err := f.find(ctx, version)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cache, strings.TrimPrefix(rel.Tag, "v"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	for _, a := range rel.Assets {
		if !wanted(a.Name) {
			continue
		}
		path := filepath.Join(dir, a.Name)
		if st, err := os.Stat(path); err == nil && st.Size() == a.Size {
			continue // fetched before; checked again below either way
		}
		fmt.Fprintf(log, "-- downloading %s (%d kB)\n", a.Name, a.Size/1024)
		if err := f.download(ctx, a, path); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// wanted: the files a deployment uses. The deploy tool's own builds are
// not.
func wanted(name string) bool {
	return strings.HasSuffix(name, ".deb") || name == "SHA256SUMS" || name == "SHA256SUMS.asc" || name == "release-key.pub"
}

func (f Fetcher) find(ctx context.Context, version string) (*ghRelease, error) {
	url := fmt.Sprintf("%s/repos/%s/releases?per_page=30", f.api(), f.repo())
	var rels []ghRelease
	if err := f.getJSON(ctx, url, &rels); err != nil {
		return nil, err
	}
	for i := range rels {
		r := &rels[i]
		if r.Draft {
			continue
		}
		if version == "" || r.Tag == "v"+strings.TrimPrefix(version, "v") {
			return r, nil
		}
	}
	if version == "" {
		return nil, fmt.Errorf("no release published in %s", f.repo())
	}
	return nil, fmt.Errorf("no release %s in %s", version, f.repo())
}

func (f Fetcher) api() string {
	if f.API != "" {
		return strings.TrimRight(f.API, "/")
	}
	return "https://api.github.com"
}

func (f Fetcher) repo() string {
	if f.Repo != "" {
		return f.Repo
	}
	return DefaultRepo
}

func (f Fetcher) client() *http.Client {
	if f.Client != nil {
		return f.Client
	}
	return http.DefaultClient
}

func (f Fetcher) getJSON(ctx context.Context, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "trellis-deploy")
	resp, err := f.client().Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach GitHub: %w (or use --from DIR with the release's files)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub answered %s for %s", resp.Status, url)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(v)
}

func (f Fetcher) download(ctx context.Context, a ghAsset, path string) error {
	if a.Size > maxPackage {
		return fmt.Errorf("%s is larger than any Trellis release file", a.Name)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "trellis-deploy")
	resp, err := f.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading %s: %s", a.Name, resp.Status)
	}
	tmp := path + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, io.LimitReader(resp.Body, maxPackage+1)); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
