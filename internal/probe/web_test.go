package probe

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCheckWeb(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" || !strings.HasPrefix(r.UserAgent(), "connectivity-check") {
			t.Errorf("request carries %q / %q", r.Header.Get("Cookie"), r.UserAgent())
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ok.Close()
	empty200 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer empty200.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://login.example/portal?mac=a0:b0:c0:00:00:01", http.StatusFound)
	}))
	defer redirect.Close()
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>sign in</html>"))
	}))
	defer page.Close()
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNetworkAuthenticationRequired)
	}))
	defer auth.Close()
	// A port nothing listens on.
	l, _ := net.Listen("tcp4", "127.0.0.1:0")
	dead := "http://" + l.Addr().String() + "/generate_204"
	l.Close()

	ctx := context.Background()
	for _, c := range []struct {
		name    string
		urls    []string
		verdict string
		detail  string
	}{
		{"204", []string{ok.URL}, WebOK, ""},
		{"empty 200", []string{empty200.URL}, WebOK, ""},
		{"one good of three", []string{dead, redirect.URL, ok.URL}, WebOK, ""},
		{"redirect", []string{redirect.URL, dead}, WebPortal, "302 to http://login.example/"},
		{"login page", []string{page.URL}, WebPortal, "200 with a page of 20 bytes"},
		{"511", []string{auth.URL}, WebPortal, "511"},
		{"nothing", []string{dead}, WebNone, "refused"},
	} {
		r := CheckWeb(ctx, "", c.urls, nil)
		if r.Verdict != c.verdict || !strings.Contains(r.Detail, c.detail) {
			t.Errorf("%s: got %q %q, want %q containing %q", c.name, r.Verdict, r.Detail, c.verdict, c.detail)
		}
	}
}
