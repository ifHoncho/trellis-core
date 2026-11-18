package meshd

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"meridian/trellis/internal/config"
	"meridian/trellis/internal/probe"
)

func TestWebCheckGates(t *testing.T) {
	var verdict atomic.Value
	verdict.Store(probe.WebPortal)
	calls := make(chan struct{}, 10)
	release := make(chan struct{})
	g := &gatewayReconciler{d: &Daemon{log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	g.webCheckFn = func(ctx context.Context, ifname string, urls, resolvers []string) probe.WebResult {
		<-release
		calls <- struct{}{}
		return probe.WebResult{Verdict: verdict.Load().(string), Detail: "302 to http://login.example/", At: time.Now()}
	}
	cfg := config.GatewayConfig{Mode: config.GatewayAuto, WebCheckURLs: []string{"http://192.0.2.1/generate_204"}}
	g.ensureWebCheck("wan0", cfg, nil)
	defer g.stopWebCheck()

	// Until the first check is back, the WAN waits.
	if _, on, portal, waiting := g.webState(); !on || portal || !waiting {
		t.Fatalf("before the first check: on=%v portal=%v waiting=%v", on, portal, waiting)
	}
	release <- struct{}{}
	<-calls
	waitFor(t, "portal", func() bool { _, _, portal, _ := g.webState(); return portal })

	// Signed in: a check asked for now (as after an ICMP change) clears it.
	verdict.Store(probe.WebOK)
	g.web.soon()
	release <- struct{}{}
	<-calls
	waitFor(t, "signed in", func() bool {
		r, _, portal, waiting := g.webState()
		return !portal && !waiting && r.Verdict == probe.WebOK
	})

	// A reset (the carrier changed): the verdict is forgotten until a new
	// check is back.
	g.web.reset()
	if _, _, _, waiting := g.webState(); !waiting {
		t.Error("a reset kept the old verdict")
	}
	release <- struct{}{}
	<-calls
	waitFor(t, "checked again", func() bool { _, _, _, waiting := g.webState(); return !waiting })

	// The same settings keep the checker; web_check: off stops it.
	w := g.web
	g.ensureWebCheck("wan0", cfg, nil)
	if g.web != w {
		t.Error("the checker was restarted with unchanged settings")
	}
	off := false
	cfg.WebCheck = &off
	close(release)
	g.ensureWebCheck("wan0", cfg, nil)
	if _, on, _, _ := g.webState(); on {
		t.Error("web_check: off left the checker running")
	}
}
