package probe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// The web check: a WAN that answers pings may still not carry traffic to
// the internet. A hotel or café network holds every device behind a login
// page until someone signs in, and many such networks (and routers with no
// service behind them) answer ICMP. The check asks a few well-known
// addresses for an empty "204 No Content" answer, out of the WAN: the
// expected answer means the internet is there; any other HTTP answer (a
// redirect, a login page, 511) means something on the way intercepts the
// traffic. Requests carry nothing about the node: no cookies, a generic
// user agent.

// Web check verdicts.
const (
	WebUnknown = ""          // not checked yet
	WebOK      = "ok"        // a target gave the expected answer
	WebPortal  = "portal"    // an HTTP answer, but not the expected one
	WebNone    = "no-answer" // no target answered over HTTP at all
)

// WebResult is one check's outcome.
type WebResult struct {
	Verdict string
	// Detail says what was seen: the portal's answer ("302 to
	// http://login.example/"), or why nothing answered.
	Detail string
	At     time.Time
}

// WebTimeout bounds one check, every target at once.
const WebTimeout = 6 * time.Second

// CheckWeb asks every URL at once, out of ifname (any interface when
// empty). Names are resolved with resolvers (addresses, asked out of
// ifname), or the system's resolver when there are none.
func CheckWeb(ctx context.Context, ifname string, urls, resolvers []string) WebResult {
	ctx, cancel := context.WithTimeout(ctx, WebTimeout)
	defer cancel()
	type answer struct {
		code   int
		detail string
		err    error
	}
	ans := make([]answer, len(urls))
	c := webClient(ifname, resolvers)
	var wg sync.WaitGroup
	for i, u := range urls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ans[i].code, ans[i].detail, ans[i].err = webGet(ctx, c, u)
		}()
	}
	wg.Wait()
	res := WebResult{Verdict: WebNone, At: time.Now()}
	var portal, errs []string
	for i, a := range ans {
		switch {
		case a.err != nil:
			errs = append(errs, a.err.Error())
		case a.code == http.StatusNoContent || a.code == http.StatusOK && a.detail == "":
			// Some proxies turn an empty 204 into an empty 200.
			res.Verdict, res.Detail = WebOK, ""
			return res
		default:
			portal = append(portal, hostOf(urls[i])+": "+a.detail)
		}
	}
	if len(portal) > 0 {
		res.Verdict, res.Detail = WebPortal, portal[0]
		return res
	}
	if len(errs) > 0 {
		res.Detail = errs[0]
	}
	return res
}

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Host
	}
	return raw
}

// webGet fetches one URL without following redirects. detail is empty for
// an empty body, else what the answer was.
func webGet(ctx context.Context, c *http.Client, u string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("User-Agent", "connectivity-check")
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := c.Do(req)
	if err != nil {
		return 0, "", shortErr(err)
	}
	defer resp.Body.Close()
	n, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400 && resp.Header.Get("Location") != "":
		return resp.StatusCode, fmt.Sprintf("%d to %s", resp.StatusCode, trimURL(resp.Header.Get("Location"))), nil
	case n == 0 && resp.StatusCode == http.StatusOK:
		return resp.StatusCode, "", nil
	case n == 0:
		return resp.StatusCode, fmt.Sprint(resp.StatusCode), nil
	}
	return resp.StatusCode, fmt.Sprintf("%d with a page of %d bytes", resp.StatusCode, n), nil
}

// trimURL keeps a portal's address short enough for a status line.
func trimURL(s string) string {
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		return u.Scheme + "://" + u.Host + "/"
	}
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}

// shortErr drops the URL that net/http puts in front of every error.
func shortErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("no answer in time")
	}
	return err
}

func webClient(ifname string, resolvers []string) *http.Client {
	bind := func(network, address string, c syscall.RawConn) error {
		if ifname == "" {
			return nil
		}
		var serr error
		if err := c.Control(func(fd uintptr) { serr = unix.BindToDevice(int(fd), ifname) }); err != nil {
			return err
		}
		return serr
	}
	d := &net.Dialer{Timeout: WebTimeout, Control: bind}
	if len(resolvers) > 0 {
		var mu sync.Mutex
		next := 0
		d.Resolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			mu.Lock()
			s := resolvers[next%len(resolvers)]
			next++
			mu.Unlock()
			rd := &net.Dialer{Timeout: 2 * time.Second, Control: bind}
			return rd.DialContext(ctx, network, net.JoinHostPort(s, "53"))
		}}
	}
	tr := &http.Transport{
		Proxy:             nil,
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return d.DialContext(ctx, "tcp4", addr)
		},
		ResponseHeaderTimeout: WebTimeout,
	}
	return &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
