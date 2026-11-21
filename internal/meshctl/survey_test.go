package meshctl

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"meridian/trellis/internal/api"
	"meridian/trellis/internal/status"
)

// fakeSurveyAPI serves POST and GET /survey: get answers what the call
// count picks (nil: "null", as a restarted meshd answers).
func fakeSurveyAPI(t *testing.T, get func(n int) *status.Survey) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "meshd.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	n := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/"+api.Version+"/survey", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			json.NewEncoder(w).Encode(&status.Survey{Started: time.Now(), Nodes: []status.SurveyNode{}})
			return
		}
		n++
		json.NewEncoder(w).Encode(get(n))
	})
	go http.Serve(ln, mux)
	return sock
}

// TestSurveyStartMeshdRestarted: meshd restarted during the survey has
// none; survey --start says so instead of crashing.
func TestSurveyStartMeshdRestarted(t *testing.T) {
	sock := fakeSurveyAPI(t, func(int) *status.Survey { return nil })
	var out, errb bytes.Buffer
	if rc := Main("test", []string{"--socket", sock, "survey", "--start"}, &out, &errb); rc != 0 {
		t.Fatalf("rc %d: %s", rc, errb.String())
	}
	if !strings.Contains(out.String(), "no survey yet") {
		t.Errorf("output: %q", out.String())
	}
}

// TestSurveyStartBounded: a survey that never ends gives up at surveyWait.
func TestSurveyStartBounded(t *testing.T) {
	old := surveyWait
	surveyWait = 3 * time.Second
	defer func() { surveyWait = old }()
	sock := fakeSurveyAPI(t, func(int) *status.Survey {
		return &status.Survey{Started: time.Now(), Nodes: []status.SurveyNode{}}
	})
	var out, errb bytes.Buffer
	if rc := Main("test", []string{"--socket", sock, "survey", "--start"}, &out, &errb); rc != 1 || !strings.Contains(errb.String(), "did not finish") {
		t.Fatalf("rc %d: %q", rc, errb.String())
	}
}
