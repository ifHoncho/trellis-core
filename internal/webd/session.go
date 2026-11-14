package webd

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"sync"
	"time"
)

// Sessions are kept in memory: a webd restart signs everyone out, which
// is all the state webd would otherwise have to store.

const (
	sessionCookie = "trellis_session"
	sessionIdle   = 12 * time.Hour      // signed out after this long unused
	sessionMax    = 14 * 24 * time.Hour // and after this long in any case
	sessionLimit  = 64                  // sessions kept; the least recently used goes first
)

type session struct {
	created, seen time.Time
}

type sessions struct {
	mu  sync.Mutex
	m   map[string]*session
	now func() time.Time
}

func newSessions() *sessions { return &sessions{m: map[string]*session{}, now: time.Now} }

// create starts a session and returns its token.
func (s *sessions) create() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for len(s.m) >= sessionLimit {
		oldest, at := "", now
		for t, se := range s.m {
			if !se.seen.After(at) {
				oldest, at = t, se.seen
			}
		}
		delete(s.m, oldest)
	}
	s.m[token] = &session{created: now, seen: now}
	return token, nil
}

// valid reports whether the token names a live session, and keeps it alive.
func (s *sessions) valid(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	se, ok := s.m[token]
	if !ok {
		return false
	}
	now := s.now()
	if now.Sub(se.seen) > sessionIdle || now.Sub(se.created) > sessionMax {
		delete(s.m, token)
		return false
	}
	se.seen = now
	return true
}

func (s *sessions) drop(token string) {
	s.mu.Lock()
	delete(s.m, token)
	s.mu.Unlock()
}

// dropAll signs everyone out: after the admin password changed.
func (s *sessions) dropAll() {
	s.mu.Lock()
	s.m = map[string]*session{}
	s.mu.Unlock()
}

func (s *Server) setCookie(w http.ResponseWriter, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: !s.opts.PlainHTTP, SameSite: http.SameSiteStrictMode,
	})
}

func (s *Server) authenticated(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	return err == nil && s.sessions.valid(c.Value)
}
