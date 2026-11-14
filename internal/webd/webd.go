// Package webd is the web UI and network-facing API (architecture §12.2).
//
// webd is unprivileged and keeps no state but its TLS certificate. It
// serves the UI's files, checks the admin login, and passes API requests
// on to meshd's socket; everything it knows and everything it can change
// goes through that socket (roadmap M5: "webd has no network privileges").
// Under systemd it does not even open its own listening sockets: a socket
// unit binds ports 443 and 80 and hands them over.
package webd

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"meridian/trellis/internal/api"
)

// Options configure webd.
type Options struct {
	Version string
	Socket  string // meshd's API socket
	// Listen and HTTPListen are used when systemd passes no sockets:
	// the HTTPS address, and the plain-HTTP address that only redirects
	// to it ("" for none).
	Listen     string
	HTTPListen string
	// StateDir holds the TLS certificate and key. Empty: a certificate is
	// made at every start and kept in memory only.
	StateDir string
	// PlainHTTP serves the UI without TLS on Listen. For development and
	// tests only: the session cookie then travels in the clear.
	PlainHTTP bool
	Log       *slog.Logger
}

// Server is one webd instance.
type Server struct {
	opts     Options
	log      *slog.Logger
	meshd    *api.Client
	sessions *sessions
	hub      *hub
	assets   *assets

	infoMu   sync.Mutex
	info     api.WebInfo // the last answer from meshd
	infoAt   time.Time
	infoOK   bool
	infoSeen bool // info holds an answer, if an old one
}

// New prepares a server.
func New(opts Options) (*Server, error) {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	as, err := loadAssets()
	if err != nil {
		return nil, err
	}
	s := &Server{opts: opts, log: opts.Log, meshd: api.NewClient(opts.Socket), sessions: newSessions(), assets: as}
	s.hub = newHub(s.meshd, s.log)
	return s, nil
}

// webInfo asks meshd what webd needs to know, at most every few seconds.
// When meshd does not answer, the last answer stands.
func (s *Server) webInfo(ctx context.Context) (api.WebInfo, bool) {
	s.infoMu.Lock()
	defer s.infoMu.Unlock()
	if time.Since(s.infoAt) < 3*time.Second {
		return s.info, s.infoOK
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	wi, err := s.meshd.WebInfo(ctx)
	s.infoAt = time.Now()
	if err != nil {
		s.infoOK = false
		return s.info, false
	}
	// A new admin password (set here, or on the node with meshctl) ends
	// every session opened under the old one.
	if s.infoSeen && !wi.AdminSince.Equal(s.info.AdminSince) {
		s.sessions.dropAll()
	}
	s.info, s.infoOK, s.infoSeen = *wi, true, true
	return s.info, true
}

// forgetInfo makes the next webInfo call ask meshd again: after a change
// that alters the answer.
func (s *Server) forgetInfo() {
	s.infoMu.Lock()
	s.infoAt = time.Time{}
	s.infoMu.Unlock()
}

// Run serves until ctx is done.
func (s *Server) Run(ctx context.Context) error {
	httpsLn, httpLn, err := s.listeners()
	if err != nil {
		return err
	}
	var servers []*http.Server
	errCh := make(chan error, 2)
	serve := func(srv *http.Server, ln net.Listener, useTLS bool) {
		servers = append(servers, srv)
		go func() {
			var err error
			if useTLS {
				err = srv.ServeTLS(ln, "", "")
			} else {
				err = srv.Serve(ln)
			}
			if !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
	}

	main := &http.Server{
		Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute,
		ErrorLog: slog.NewLogLogger(s.log.Handler(), slog.LevelDebug),
	}
	if s.opts.PlainHTTP {
		s.log.Warn("serving the UI over plain HTTP: for development only", "address", httpsLn.Addr().String())
		serve(main, s.guard(httpsLn), false)
	} else {
		cert, err := loadOrCreateCert(s.opts.StateDir, func() ([]string, []net.IP) { return s.certNames(ctx) }, s.log)
		if err != nil {
			return fmt.Errorf("TLS certificate: %w", err)
		}
		main.TLSConfig = &tls.Config{Certificates: []tls.Certificate{*cert}, MinVersion: tls.VersionTLS12}
		s.log.Info("web UI listening", "https", httpsLn.Addr().String())
		serve(main, s.guard(httpsLn), true)
	}
	if httpLn != nil {
		port := 443
		if a, ok := httpsLn.Addr().(*net.TCPAddr); ok {
			port = a.Port
		}
		redirect := &http.Server{Handler: redirectToHTTPS(port), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
		serve(redirect, s.guard(httpLn), false)
	}
	go s.hub.run(ctx)

	select {
	case <-ctx.Done():
	case err = <-errCh:
	}
	sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, srv := range servers {
		_ = srv.Shutdown(sctx)
	}
	return err
}

// listeners returns the HTTPS listener and the optional plain-HTTP one:
// from systemd when it passed sockets, otherwise opened here.
func (s *Server) listeners() (httpsLn, httpLn net.Listener, err error) {
	passed, err := systemdListeners()
	if err != nil {
		return nil, nil, err
	}
	if len(passed) > 0 {
		for _, ln := range passed {
			port := 0
			if a, ok := ln.Addr().(*net.TCPAddr); ok {
				port = a.Port
			}
			switch {
			case port == 80 && httpLn == nil:
				httpLn = ln
			case httpsLn == nil:
				httpsLn = ln
			default:
				ln.Close()
			}
		}
		if httpsLn == nil {
			return nil, nil, errors.New("systemd passed no socket for HTTPS (only port 80)")
		}
		return httpsLn, httpLn, nil
	}
	if s.opts.Listen == "" {
		return nil, nil, errors.New("no listening sockets from systemd and no --listen address")
	}
	if httpsLn, err = net.Listen("tcp", s.opts.Listen); err != nil {
		return nil, nil, fmt.Errorf("listen on %s: %w (ports below 1024 need the systemd socket unit; for development use --listen 127.0.0.1:8443)", s.opts.Listen, err)
	}
	if s.opts.HTTPListen != "" && !s.opts.PlainHTTP {
		if httpLn, err = net.Listen("tcp", s.opts.HTTPListen); err != nil {
			s.log.Warn("cannot listen for plain HTTP; http:// addresses will not redirect", "address", s.opts.HTTPListen, "err", err)
			httpLn = nil
		}
	}
	return httpsLn, httpLn, nil
}

// systemdListeners returns the sockets systemd passed to this process
// (sd_listen_fds): file descriptors 3 and up, when LISTEN_PID names us.
func systemdListeners() ([]net.Listener, error) {
	pid, _ := strconv.Atoi(os.Getenv("LISTEN_PID"))
	n, _ := strconv.Atoi(os.Getenv("LISTEN_FDS"))
	if pid != os.Getpid() || n <= 0 {
		return nil, nil
	}
	os.Unsetenv("LISTEN_PID")
	os.Unsetenv("LISTEN_FDS")
	os.Unsetenv("LISTEN_FDNAMES")
	var out []net.Listener
	for fd := 3; fd < 3+n; fd++ {
		f := os.NewFile(uintptr(fd), "systemd-socket-"+strconv.Itoa(fd))
		ln, err := net.FileListener(f)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("socket %d from systemd: %w", fd, err)
		}
		out = append(out, ln)
	}
	return out, nil
}

// redirectToHTTPS answers every plain-HTTP request with the HTTPS address
// of the same page.
func redirectToHTTPS(port int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if host == "" {
			http.Error(w, "use https://", http.StatusBadRequest)
			return
		}
		if port != 443 {
			host = net.JoinHostPort(host, strconv.Itoa(port))
		}
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	})
}
