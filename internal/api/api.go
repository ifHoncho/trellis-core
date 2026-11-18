// Package api is meshd's HTTP/JSON API, version 1.
// meshd serves it on a unix socket; meshctl calls it there, and webd puts
// it on the network behind the admin login. docs/api.md describes it.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/sys/unix"

	"meridian/trellis/internal/config"
	"meridian/trellis/internal/status"
)

// Provider is what the server needs from meshd.
type Provider interface {
	Status(ctx context.Context) (*status.Status, error)
	Config() ConfigResult
	Kick(reason string)
	Reload() error
	APJoin() (*status.APJoin, error)

	// SetNode replaces the node configuration with the JSON document raw.
	SetNode(ctx context.Context, raw []byte, dryRun bool) (*ApplyResult, error)
	ProfileExport() (*ProfileExport, error)
	SetProfile(ctx context.Context, in ProfileInput, dryRun bool) (*ApplyResult, error)
	SetMeshProfile(ctx context.Context, in MeshProfileInput, dryRun bool) (*ApplyResult, error)
	// Survey is the last mesh-wide channel survey (nil before the first);
	// StartSurvey begins one, or returns the one under way.
	Survey() *status.Survey
	StartSurvey(ctx context.Context) (*status.Survey, error)
	Setup(ctx context.Context, in SetupInput, dryRun bool) (*ApplyResult, error)

	Topology(ctx context.Context) (*status.Topology, error)
	IdentifyRadio(fingerprint string, seconds int) error
	Doctor(ctx context.Context) (string, error)

	// UpgradeState is the node's software and its upgrades. Upgrade checks
	// a release package (a .deb) and, unless dryRun, starts installing it
	// detached from the caller; the answer comes before meshd restarts.
	// UpgradeRollback goes back to the version before the last upgrade.
	UpgradeState(ctx context.Context) (*UpgradeState, error)
	Upgrade(ctx context.Context, in UpgradeRequest) (*UpgradeCheck, error)
	UpgradeRollback(ctx context.Context) error

	WebInfo() WebInfo
	// WebLogin checks the admin password. from is where the attempt comes
	// from: wrong guesses lock out that source only.
	WebLogin(from, password string) error
	// WebSetPassword sets the admin password. privileged callers (root on
	// the socket) need no current password.
	WebSetPassword(from, current, new string, privileged bool) error

	// VoiceConfig is trellis-voiced's configuration; Voice its state.
	VoiceConfig() VoiceConfig
	Voice(ctx context.Context) (*VoiceState, error)
	VoiceTest(ctx context.Context, in VoiceTestInput) error
}

// Server serves the API.
type Server struct {
	Provider Provider
	Log      *slog.Logger
	Socket   string
	// Mode is the socket's file mode; 0 means 0660.
	Mode os.FileMode
}

// maxBody bounds every request body but a release package: the largest is
// a node configuration. MaxPackage bounds a package (one is some 5 MB).
const (
	maxBody    = 256 << 10
	MaxPackage = 64 << 20
)

type peerKey struct{}

// peerUID is the uid of the process on the other end of the socket, or -1.
func peerUID(r *http.Request) int {
	if uid, ok := r.Context().Value(peerKey{}).(int); ok {
		return uid
	}
	return -1
}

// connContext records the peer's uid for each connection.
func connContext(ctx context.Context, c net.Conn) context.Context {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return ctx
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return ctx
	}
	uid := -1
	_ = raw.Control(func(fd uintptr) {
		if cred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED); err == nil {
			uid = int(cred.Uid)
		}
	})
	return context.WithValue(ctx, peerKey{}, uid)
}

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	p := s.Provider
	get := func(path string, fn func(r *http.Request) (any, error)) {
		mux.HandleFunc("GET /"+Version+path, func(w http.ResponseWriter, r *http.Request) { reply(w, fn, r) })
	}
	// change registers a handler that reads a JSON body into a fresh T.
	dryRun := func(r *http.Request) bool {
		v := r.URL.Query().Get("dry_run")
		return v == "1" || v == "true"
	}

	get("/health", func(r *http.Request) (any, error) {
		return map[string]any{"ok": true, "time": time.Now(), "api": Version}, nil
	})
	get("/status", func(r *http.Request) (any, error) { return p.Status(r.Context()) })
	get("/radios", func(r *http.Request) (any, error) {
		st, err := p.Status(r.Context())
		if err != nil {
			return nil, err
		}
		return map[string]any{"radios": st.Radios, "plan": st.Plan}, nil
	})
	get("/config", func(r *http.Request) (any, error) { return p.Config(), nil })
	get("/channels", func(r *http.Request) (any, error) { return ChannelTable(), nil })
	get("/ap", func(r *http.Request) (any, error) { return p.APJoin() })
	get("/topology", func(r *http.Request) (any, error) { return p.Topology(r.Context()) })
	get("/profile/export", func(r *http.Request) (any, error) { return p.ProfileExport() })
	get("/survey", func(r *http.Request) (any, error) { return p.Survey(), nil })
	get("/voice", func(r *http.Request) (any, error) { return p.Voice(r.Context()) })
	get("/voice/config", func(r *http.Request) (any, error) { return p.VoiceConfig(), nil })
	mux.HandleFunc("POST /"+Version+"/voice/test", func(w http.ResponseWriter, r *http.Request) {
		var in VoiceTestInput
		if !readJSON(w, r, &in) {
			return
		}
		reply(w, func(r *http.Request) (any, error) { return map[string]bool{"ok": true}, p.VoiceTest(r.Context(), in) }, r)
	})
	mux.HandleFunc("POST /"+Version+"/survey", func(w http.ResponseWriter, r *http.Request) {
		reply(w, func(r *http.Request) (any, error) { return p.StartSurvey(r.Context()) }, r)
	})
	get("/doctor", func(r *http.Request) (any, error) {
		text, err := p.Doctor(r.Context())
		if err != nil {
			return nil, err
		}
		return map[string]any{"report": text}, nil
	})
	get("/web", func(r *http.Request) (any, error) { return p.WebInfo(), nil })
	get("/upgrade", func(r *http.Request) (any, error) { return p.UpgradeState(r.Context()) })
	mux.HandleFunc("POST /"+Version+"/upgrade", func(w http.ResponseWriter, r *http.Request) {
		pkg, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxPackage))
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				writeError(w, Errorf(http.StatusRequestEntityTooLarge, "the package is larger than %d MB", MaxPackage>>20))
			} else {
				writeError(w, Errorf(http.StatusBadRequest, "the upload did not arrive whole (%v): try again", err))
			}
			return
		}
		in := UpgradeRequest{Package: pkg, DryRun: dryRun(r), SHA256: r.URL.Query().Get("sha256")}
		if in.SHA256 != "" {
			in.Package = nil // the package kept from the dry run
		}
		reply(w, func(r *http.Request) (any, error) { return p.Upgrade(r.Context(), in) }, r)
	})
	mux.HandleFunc("POST /"+Version+"/upgrade/rollback", func(w http.ResponseWriter, r *http.Request) {
		reply(w, func(r *http.Request) (any, error) { return map[string]any{"ok": true}, p.UpgradeRollback(r.Context()) }, r)
	})

	mux.HandleFunc("PUT /"+Version+"/config/node", func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			writeError(w, Invalid("cannot read the request: %v", err))
			return
		}
		reply(w, func(r *http.Request) (any, error) { return p.SetNode(r.Context(), raw, dryRun(r)) }, r)
	})
	mux.HandleFunc("PUT /"+Version+"/profile", func(w http.ResponseWriter, r *http.Request) {
		var in ProfileInput
		if !readJSON(w, r, &in) {
			return
		}
		reply(w, func(r *http.Request) (any, error) { return p.SetProfile(r.Context(), in, dryRun(r)) }, r)
	})
	mux.HandleFunc("POST /"+Version+"/profile/mesh", func(w http.ResponseWriter, r *http.Request) {
		var in MeshProfileInput
		if !readJSON(w, r, &in) {
			return
		}
		reply(w, func(r *http.Request) (any, error) { return p.SetMeshProfile(r.Context(), in, dryRun(r)) }, r)
	})
	mux.HandleFunc("POST /"+Version+"/setup", func(w http.ResponseWriter, r *http.Request) {
		var in SetupInput
		if !readJSON(w, r, &in) {
			return
		}
		reply(w, func(r *http.Request) (any, error) { return p.Setup(r.Context(), in, dryRun(r)) }, r)
	})
	mux.HandleFunc("POST /"+Version+"/radios/identify", func(w http.ResponseWriter, r *http.Request) {
		var in IdentifyInput
		if !readJSON(w, r, &in) {
			return
		}
		reply(w, func(r *http.Request) (any, error) {
			return map[string]any{"ok": true}, p.IdentifyRadio(in.Fingerprint, in.Seconds)
		}, r)
	})
	mux.HandleFunc("POST /"+Version+"/reconcile", func(w http.ResponseWriter, r *http.Request) {
		p.Kick("api")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /"+Version+"/reload", func(w http.ResponseWriter, r *http.Request) {
		if err := p.Reload(); err != nil {
			e := asError(err)
			if e.Status == http.StatusInternalServerError {
				e.Status = http.StatusBadRequest
			}
			writeError(w, e)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /"+Version+"/web/login", func(w http.ResponseWriter, r *http.Request) {
		var in LoginInput
		if !readJSON(w, r, &in) {
			return
		}
		reply(w, func(r *http.Request) (any, error) {
			return map[string]any{"ok": true}, p.WebLogin(in.From, in.Password)
		}, r)
	})
	mux.HandleFunc("PUT /"+Version+"/web/password", func(w http.ResponseWriter, r *http.Request) {
		var in PasswordInput
		if !readJSON(w, r, &in) {
			return
		}
		reply(w, func(r *http.Request) (any, error) {
			return map[string]any{"ok": true}, p.WebSetPassword(in.From, in.Current, in.New, peerUID(r) == 0)
		}, r)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, Errorf(http.StatusNotFound, "no such API endpoint: %s %s (this is API %s; see docs/api.md)", r.Method, r.URL.Path, Version))
	})
	return mux
}

func reply(w http.ResponseWriter, fn func(r *http.Request) (any, error), r *http.Request) {
	v, err := fn(r)
	if err != nil {
		writeError(w, asError(err))
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// readJSON decodes the request body into v, rejecting unknown fields.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, Invalid("the request body is not the JSON this endpoint takes: %v", err))
		return false
	}
	return true
}

// MaxSocketPath is the longest unix socket path Linux accepts (sun_path
// minus the terminator).
const MaxSocketPath = 107

// Serve listens on the unix socket until ctx is done.
func (s *Server) Serve(ctx context.Context) error {
	if len(s.Socket) > MaxSocketPath {
		return fmt.Errorf("socket path %q is %d bytes; unix sockets allow at most %d. Use a shorter --run-dir or --socket", s.Socket, len(s.Socket), MaxSocketPath)
	}
	if err := os.MkdirAll(filepath.Dir(s.Socket), 0o755); err != nil {
		return err
	}
	// A stale socket from an unclean exit must not block startup.
	if _, err := os.Stat(s.Socket); err == nil {
		if c, err := net.DialTimeout("unix", s.Socket, 500*time.Millisecond); err == nil {
			c.Close()
			return fmt.Errorf("another meshd is already listening on %s", s.Socket)
		}
		_ = os.Remove(s.Socket)
	}
	ln, err := net.Listen("unix", s.Socket)
	if err != nil {
		return err
	}
	mode := s.Mode
	if mode == 0 {
		mode = 0o660
	}
	_ = os.Chmod(s.Socket, mode)
	// Members of the "trellis" group may use the API without root: webd
	// runs in it, and so can an operator's own account for meshctl.
	if g, err := user.LookupGroup("trellis"); err == nil {
		if gid, err := strconv.Atoi(g.Gid); err == nil {
			_ = os.Chown(s.Socket, -1, gid)
		}
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second, ConnContext: connContext}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	if s.Log != nil {
		s.Log.Info("api listening", "socket", s.Socket)
	}
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		_ = os.Remove(s.Socket)
		return ctx.Err()
	case err := <-errCh:
		_ = os.Remove(s.Socket)
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, e *Error) {
	if e.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(e.RetryAfter))
	}
	writeJSON(w, e.Status, e)
}

// Redact blanks the secrets in a configuration for API output.
func Redact(node config.Node, profile config.Profile) (config.Node, config.Profile) {
	return node.Redacted(), profile.Redacted()
}
