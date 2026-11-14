package webd

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"meridian/trellis/internal/api"
)

// The network API (docs/api.md). Under /api/v1:
//
//	session, login, logout, password   webd's own: the admin login
//	events                             live status, as server-sent events
//	anything else                      meshd's API, for a signed-in admin
//
// Everything but session and login needs the session cookie. Requests
// that change something must also carry the X-Trellis-Request header: a
// page on another site can make a browser send the cookie, but not that
// header.

const (
	apiPrefix     = "/api/" + api.Version
	requestHeader = "X-Trellis-Request"
	maxRequest    = 256 << 10
)

// Handler returns webd's routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+apiPrefix+"/session", s.handleSession)
	mux.HandleFunc("POST "+apiPrefix+"/login", s.handleLogin)
	mux.HandleFunc("POST "+apiPrefix+"/logout", s.handleLogout)
	mux.HandleFunc("PUT "+apiPrefix+"/password", s.handlePassword)
	mux.HandleFunc("GET "+apiPrefix+"/events", s.requireAuth(s.handleEvents))
	mux.HandleFunc(apiPrefix+"/", s.requireAuth(s.handleProxy))
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "this node serves API "+api.Version+" under "+apiPrefix+"/")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.assets.serve(w, r)
	})
	return s.secure(mux)
}

// secure adds the headers every response carries and refuses
// state-changing requests that a foreign page could have caused.
func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: blob:; media-src 'self' blob:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", "camera=(self), microphone=(), geolocation=()")
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			if r.Header.Get(requestHeader) == "" {
				writeError(w, http.StatusForbidden, "requests that change something need the "+requestHeader+" header")
				return
			}
			if o := r.Header.Get("Origin"); o != "" {
				if u, err := url.Parse(o); err != nil || u.Host != r.Host {
					writeError(w, http.StatusForbidden, "this request comes from another site")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.webInfo(r.Context()) // notices a password changed behind webd's back
		if !s.authenticated(r) {
			writeError(w, http.StatusUnauthorized, "sign in first")
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// writeAPIError passes an error from meshd on to the browser.
func writeAPIError(w http.ResponseWriter, err error) {
	var ae *api.Error
	switch {
	case errors.As(err, &ae):
		if ae.RetryAfter > 0 {
			w.Header().Set("Retry-After", fmt.Sprint(ae.RetryAfter))
		}
		writeJSON(w, ae.Status, ae)
	case errors.Is(err, api.ErrNotRunning):
		writeError(w, http.StatusServiceUnavailable, "meshd is not running on this node")
	default:
		writeError(w, http.StatusBadGateway, "cannot reach meshd: "+err.Error())
	}
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequest))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "the request body is not the JSON this endpoint takes")
		return false
	}
	return true
}

// SessionInfo is what a page needs before anyone has signed in: nothing
// that tells a stranger about the mesh.
type SessionInfo struct {
	Authenticated bool   `json:"authenticated"`
	AdminSet      bool   `json:"admin_set"` // an admin password exists
	Setup         bool   `json:"setup"`     // the node waits to be provisioned
	Callsign      string `json:"callsign"`
	Version       string `json:"version"`
	MeshdUp       bool   `json:"meshd_up"`
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	info, ok := s.webInfo(r.Context())
	writeJSON(w, http.StatusOK, SessionInfo{
		Authenticated: s.authenticated(r), AdminSet: info.AdminSet, Setup: info.Setup,
		Callsign: info.Callsign, Version: s.opts.Version, MeshdUp: ok,
	})
}

func (s *Server) startSession(w http.ResponseWriter) bool {
	token, err := s.sessions.create()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot start a session")
		return false
	}
	s.setCookie(w, token, int(sessionMax/time.Second))
	return true
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var in api.LoginInput
	if !readJSON(w, r, &in) {
		return
	}
	if err := s.meshd.WebLogin(r.Context(), in.Password); err != nil {
		s.log.Warn("sign-in refused", "from", remoteIP(r), "err", err)
		writeAPIError(w, err)
		return
	}
	// Ask meshd afresh before the session exists: a password set a moment
	// ago (meshctl web password) must end the sessions before this one,
	// not this one.
	s.forgetInfo()
	s.webInfo(r.Context())
	if !s.startSession(w) {
		return
	}
	s.log.Info("admin signed in", "from", remoteIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.drop(c.Value)
	}
	s.setCookie(w, "", -1)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handlePassword sets the admin password: the first one, during setup,
// from anyone; later ones from a signed-in admin who knows the current
// one. meshd enforces both rules; webd only refuses the obvious.
func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request) {
	var in api.PasswordInput
	if !readJSON(w, r, &in) {
		return
	}
	info, ok := s.webInfo(r.Context())
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "meshd is not running on this node")
		return
	}
	if info.AdminSet && !s.authenticated(r) {
		writeError(w, http.StatusUnauthorized, "sign in first")
		return
	}
	if in.New == "" {
		writeError(w, http.StatusBadRequest, "the new password is empty")
		return
	}
	if err := s.meshd.WebSetPassword(r.Context(), in.Current, in.New); err != nil {
		writeAPIError(w, err)
		return
	}
	// Every session ends (webInfo sees the new password's date; dropAll
	// covers a meshd that just went away); this browser gets a fresh one.
	s.forgetInfo()
	s.webInfo(r.Context())
	s.sessions.dropAll()
	if !s.startSession(w) {
		return
	}
	s.log.Info("admin password set", "from", remoteIP(r), "first", !info.AdminSet)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleProxy passes a signed-in admin's request on to meshd.
func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, apiPrefix)
	// meshd's /web endpoints are webd's own business with meshd: the
	// password check must not be reachable around the login handler.
	if path == "/web" || strings.HasPrefix(path, "/web/") {
		writeError(w, http.StatusNotFound, "no such API endpoint")
		return
	}
	var body []byte
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		var err error
		if body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequest)); err != nil {
			writeError(w, http.StatusRequestEntityTooLarge, "the request is too large")
			return
		}
		if body == nil {
			body = []byte{}
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	code, ctype, data, err := s.meshd.Raw(ctx, r.Method, path, r.URL.RawQuery, body)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if r.Method != http.MethodGet {
		s.forgetInfo() // the change may alter what webd itself goes by
		s.log.Info("API change", "method", r.Method, "path", path, "query", r.URL.RawQuery, "status", code, "from", remoteIP(r))
	}
	h := w.Header()
	if ctype == "" {
		ctype = "application/json"
	}
	h.Set("Content-Type", ctype)
	h.Set("Cache-Control", "no-store")
	if len(data) > 1024 && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		h.Set("Content-Encoding", "gzip")
		h.Add("Vary", "Accept-Encoding")
		w.WriteHeader(code)
		zw := gzip.NewWriter(w)
		zw.Write(data)
		zw.Close()
		return
	}
	w.WriteHeader(code)
	w.Write(data)
}

// handleEvents streams live status as server-sent events: "status" with
// the node's status document, or "problem" with {"error": …} while meshd
// cannot be asked. The stream is compressed when the browser takes gzip:
// a status is some 15 kB of JSON every two seconds, a real cost on a
// slow mesh link.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is not supported here")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	var out io.Writer = w
	flush := fl.Flush
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		h.Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		defer zw.Close()
		out = zw
		flush = func() { zw.Flush(); fl.Flush() }
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(out, "retry: 2000\n\n")
	flush()

	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)
	beat := time.NewTicker(heartbeat)
	defer beat.Stop()
	// A session that ends (sign-out, a new password) ends its streams.
	check := time.NewTicker(30 * time.Second)
	defer check.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			if _, err := fmt.Fprintf(out, "event: %s\ndata: %s\n\n", ev.name, ev.data); err != nil {
				return
			}
			flush()
		case <-beat.C:
			if _, err := fmt.Fprint(out, ": keep-alive\n\n"); err != nil {
				return
			}
			flush()
		case <-check.C:
			if !s.authenticated(r) {
				fmt.Fprint(out, "event: signed-out\ndata: {}\n\n")
				flush()
				return
			}
		}
	}
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
