package meshd

import (
	"context"
	"strings"
	"sync"
	"time"

	"meridian/trellis/internal/config"
	"meridian/trellis/internal/probe"
)

// The WAN's web check (probe.CheckWeb): beside the ICMP prober, a WAN is
// asked for the connectivity-check answer every webCheckEvery, and often
// while it sits behind a login page so that a sign-in is seen quickly.
// When the WAN's carrier changes or its probes start passing, the verdict
// is forgotten and asked again before the WAN is announced.
const (
	webCheckEvery  = 60 * time.Second
	webCheckPortal = 10 * time.Second
)

type webChecker struct {
	ifname    string
	urls      []string
	resolvers []string
	onChange  func(probe.WebResult)
	check     func(ctx context.Context, ifname string, urls, resolvers []string) probe.WebResult

	mu   sync.Mutex
	res  probe.WebResult
	gen  int // reset counts up: a check started before a reset is not kept
	now  chan struct{}
	stop context.CancelFunc
	done chan struct{}
}

func (w *webChecker) start() {
	ctx, cancel := context.WithCancel(context.Background())
	w.stop, w.now, w.done = cancel, make(chan struct{}, 1), make(chan struct{})
	if w.check == nil {
		w.check = probe.CheckWeb
	}
	go w.loop(ctx)
}

func (w *webChecker) loop(ctx context.Context) {
	defer close(w.done)
	for {
		w.mu.Lock()
		gen := w.gen
		w.mu.Unlock()
		r := w.check(ctx, w.ifname, w.urls, w.resolvers)
		if ctx.Err() != nil {
			return
		}
		w.mu.Lock()
		if w.gen != gen {
			w.mu.Unlock()
			continue
		}
		prev := w.res
		w.res = r
		w.mu.Unlock()
		if (prev.Verdict != r.Verdict || prev.Detail != r.Detail && r.Verdict == probe.WebPortal) && w.onChange != nil {
			w.onChange(r)
		}
		every := webCheckEvery
		if r.Verdict == probe.WebPortal {
			every = webCheckPortal
		}
		t := time.NewTimer(every)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-w.now:
		case <-t.C:
		}
		t.Stop()
	}
}

// soon asks for a check now (after the ICMP state changed).
func (w *webChecker) soon() {
	select {
	case w.now <- struct{}{}:
	default:
	}
}

// reset forgets the verdict and checks again: the WAN may now be another
// network (a cable moved, a link back up), and must not be announced on
// what the old one answered.
func (w *webChecker) reset() {
	w.mu.Lock()
	w.res = probe.WebResult{}
	w.gen++
	w.mu.Unlock()
	w.soon()
}

func (w *webChecker) result() probe.WebResult {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.res
}

func (w *webChecker) halt() {
	w.stop()
	<-w.done
}

// ensureWebCheck runs the web check for wan, or none (no WAN, sharing off,
// web_check off). An auto WAN's names resolve with its lease's resolvers,
// asked out of the port; any other WAN uses the system's resolver.
func (g *gatewayReconciler) ensureWebCheck(wan string, cfg config.GatewayConfig, auto *autoWAN) {
	var resolvers []string
	if auto != nil {
		for _, s := range auto.DNS {
			resolvers = append(resolvers, s.String())
		}
		if len(resolvers) == 0 && auto.Router != nil {
			resolvers = []string{auto.Router.String()}
		}
	}
	key := ""
	if wan != "" && cfg.Mode == config.GatewayAuto && cfg.WebChecking() {
		key = wan + "|" + strings.Join(cfg.WebCheckURLs, ",") + "|" + strings.Join(resolvers, ",")
	}
	g.mu.Lock()
	same := key == g.webKey
	g.mu.Unlock()
	if same {
		return
	}
	g.stopWebCheck()
	if key == "" {
		return
	}
	w := &webChecker{ifname: wan, urls: cfg.WebCheckURLs, resolvers: resolvers, check: g.webCheckFn,
		onChange: func(r probe.WebResult) {
			switch r.Verdict {
			case probe.WebOK:
				g.d.log.Info("WAN web check passes", "interface", wan)
			case probe.WebPortal:
				g.d.log.Warn("WAN is behind a login page: not sharing it", "interface", wan, "answer", r.Detail)
			case probe.WebNone:
				g.d.log.Info("WAN web check: no answer", "interface", wan, "err", r.Detail)
			}
			g.d.Kick("wan web check changed")
		}}
	w.start()
	g.mu.Lock()
	g.web, g.webKey = w, key
	g.mu.Unlock()
}

func (g *gatewayReconciler) stopWebCheck() {
	g.mu.Lock()
	w := g.web
	g.web, g.webKey = nil, ""
	g.mu.Unlock()
	if w != nil {
		w.halt()
	}
}

// webState is the web check's say in sharing the WAN: portal (behind a
// login page), and waiting (the first check of this WAN not back yet, so
// a WAN behind a login page is never announced for the moment before).
func (g *gatewayReconciler) webState() (res probe.WebResult, on, portal, waiting bool) {
	g.mu.Lock()
	w := g.web
	g.mu.Unlock()
	if w == nil {
		return probe.WebResult{}, false, false, false
	}
	res = w.result()
	return res, true, res.Verdict == probe.WebPortal, res.Verdict == probe.WebUnknown
}
