package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"meridian/trellis/internal/config"
	"meridian/trellis/internal/status"
)

// Client talks to meshd over its socket.
type Client struct {
	Socket string
	http   *http.Client
}

// NewClient creates a client for the socket ("" means the default one).
func NewClient(socket string) *Client {
	if socket == "" {
		socket = filepath.Join(config.DefaultRunDir, "meshd.sock")
	}
	return &Client{
		Socket: socket,
		// No fixed timeout: the caller's context sets it (defaultTimeout
		// when it has none), so that a long request such as a package
		// upload can be given longer.
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socket)
				},
				MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second,
			},
		},
	}
}

// defaultTimeout bounds a request whose context has no deadline.
const defaultTimeout = 30 * time.Second

// bounded gives ctx the default deadline when it has none.
func bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, defaultTimeout)
}

// ErrNotRunning means the socket is absent or refuses connections.
var ErrNotRunning = errors.New("meshd is not running")

// ErrPermission means the socket exists but this user may not open it.
var ErrPermission = errors.New("permission denied")

// Raw sends one request and returns the response as it is: the status
// code, the content type and the body. path starts after the version, for
// example "/status". Transport failures are errors; API errors are not.
func (c *Client) Raw(ctx context.Context, method, path, query string, body []byte) (int, string, []byte, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	u := "http://meshd/" + Version + path
	if query != "" {
		u += "?" + query
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return 0, "", nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return 0, "", nil, fmt.Errorf("%w: cannot open %s; run as root or add your user to the trellis group", ErrPermission, c.Socket)
		}
		var opErr *net.OpError
		if errors.As(err, &opErr) || errors.Is(err, os.ErrNotExist) {
			return 0, "", nil, fmt.Errorf("%w (socket %s: %v)", ErrNotRunning, c.Socket, err)
		}
		return 0, "", nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, "", nil, err
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), data, nil
}

// do sends in (when not nil) as JSON and decodes the answer into out (when
// not nil). An API error comes back as *Error.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		if raw, ok := in.(json.RawMessage); ok {
			body = raw
		} else {
			var err error
			if body, err = json.Marshal(in); err != nil {
				return err
			}
		}
	}
	path, query, _ := strings.Cut(path, "?")
	code, _, data, err := c.Raw(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	if code >= 400 {
		e := &Error{Status: code}
		if json.Unmarshal(data, e) != nil || e.Message == "" {
			e.Message = fmt.Sprintf("%s %s: HTTP %d", method, path, code)
		}
		return e
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// Status fetches the node status.
func (c *Client) Status(ctx context.Context) (*status.Status, error) {
	var st status.Status
	if err := c.do(ctx, http.MethodGet, "/status", nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// Health pings meshd.
func (c *Client) Health(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/health", nil, nil)
}

// Reconcile asks for a pass.
func (c *Client) Reconcile(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/reconcile", nil, nil)
}

// Reload asks meshd to re-read its configuration.
func (c *Client) Reload(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/reload", nil, nil)
}

// APJoin fetches the client-AP credentials.
func (c *Client) APJoin(ctx context.Context) (*status.APJoin, error) {
	var out status.APJoin
	if err := c.do(ctx, http.MethodGet, "/ap", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Config fetches the redacted configuration.
func (c *Client) Config(ctx context.Context) (*ConfigResult, error) {
	var out ConfigResult
	if err := c.do(ctx, http.MethodGet, "/config", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetNode replaces the node configuration, or with dryRun only reports
// what that would do.
func (c *Client) SetNode(ctx context.Context, node config.Node, dryRun bool) (*ApplyResult, error) {
	var out ApplyResult
	path := "/config/node"
	if dryRun {
		path += "?dry_run=1"
	}
	if err := c.do(ctx, http.MethodPut, path, node, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetMeshProfile sends a profile to every node of the mesh, or with dryRun
// only reports what that would do.
func (c *Client) SetMeshProfile(ctx context.Context, in MeshProfileInput, dryRun bool) (*ApplyResult, error) {
	var out ApplyResult
	path := "/profile/mesh"
	if dryRun {
		path += "?dry_run=1"
	}
	if err := c.do(ctx, http.MethodPost, path, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Survey fetches the last mesh-wide channel survey (nil before the first).
func (c *Client) Survey(ctx context.Context) (*status.Survey, error) {
	var out *status.Survey
	if err := c.do(ctx, http.MethodGet, "/survey", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// StartSurvey begins a mesh-wide channel survey.
func (c *Client) StartSurvey(ctx context.Context) (*status.Survey, error) {
	var out status.Survey
	if err := c.do(ctx, http.MethodPost, "/survey", struct{}{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetProfile installs a mesh profile.
func (c *Client) SetProfile(ctx context.Context, in ProfileInput) (*ApplyResult, error) {
	var out ApplyResult
	if err := c.do(ctx, http.MethodPut, "/profile", in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ProfileExport fetches the mesh profile with its secret.
func (c *Client) ProfileExport(ctx context.Context) (*ProfileExport, error) {
	var out ProfileExport
	if err := c.do(ctx, http.MethodGet, "/profile/export", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Topology fetches the mesh graph.
func (c *Client) Topology(ctx context.Context) (*status.Topology, error) {
	var out status.Topology
	if err := c.do(ctx, http.MethodGet, "/topology", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// WebInfo fetches what webd needs to serve the UI.
func (c *Client) WebInfo(ctx context.Context) (*WebInfo, error) {
	var out WebInfo
	if err := c.do(ctx, http.MethodGet, "/web", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// WebLogin checks the admin password.
func (c *Client) WebLogin(ctx context.Context, password string) error {
	return c.do(ctx, http.MethodPost, "/web/login", LoginInput{Password: password}, nil)
}

// WebSetPassword sets the admin password.
func (c *Client) WebSetPassword(ctx context.Context, current, new string) error {
	return c.do(ctx, http.MethodPut, "/web/password", PasswordInput{Current: current, New: new}, nil)
}
