package webd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"meridian/trellis/internal/api"
)

// The hub feeds live status to every open page. It asks meshd for the
// status on a timer, only while somebody is listening, and hands the same
// bytes to every listener: ten open pages cost meshd what one does.

const (
	pollEvery   = 2 * time.Second
	heartbeat   = 15 * time.Second // comment lines that keep idle streams open
	subscriberQ = 4                // events buffered per listener; a slow one skips ahead
)

// event is one server-sent event, already compacted to a single line.
type event struct {
	name string // "status" or "problem"
	data []byte
}

type hub struct {
	meshd *api.Client
	log   *slog.Logger

	mu   sync.Mutex
	subs map[chan event]struct{}
	last event     // the latest event, for new listeners
	at   time.Time // when last was made
	wake chan struct{}
}

func newHub(meshd *api.Client, log *slog.Logger) *hub {
	return &hub{meshd: meshd, log: log, subs: map[chan event]struct{}{}, wake: make(chan struct{}, 1)}
}

func (h *hub) subscribe() chan event {
	ch := make(chan event, subscriberQ)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	if h.last.data != nil && time.Since(h.at) < 2*pollEvery {
		ch <- h.last
	}
	h.mu.Unlock()
	select {
	case h.wake <- struct{}{}:
	default:
	}
	return ch
}

func (h *hub) unsubscribe(ch chan event) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

func (h *hub) listeners() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

func (h *hub) publish(ev event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.last, h.at = ev, time.Now()
	for ch := range h.subs {
		select {
		case ch <- ev:
		default:
			// The listener is behind: drop its oldest event for this one.
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- ev:
			default:
			}
		}
	}
}

// poll fetches the status once and turns it into an event.
func (h *hub) poll(ctx context.Context) event {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	code, _, body, err := h.meshd.Raw(ctx, http.MethodGet, "/status", "", nil)
	problem := func(msg string) event {
		data, _ := json.Marshal(map[string]string{"error": msg})
		return event{name: "problem", data: data}
	}
	switch {
	case errors.Is(err, api.ErrNotRunning):
		return problem("meshd is not running on this node")
	case err != nil:
		return problem("cannot reach meshd: " + err.Error())
	case code != http.StatusOK:
		return problem("meshd could not report its status")
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, body); err != nil {
		return problem("meshd sent a status that is not JSON")
	}
	return event{name: "status", data: buf.Bytes()}
}

func (h *hub) run(ctx context.Context) {
	t := time.NewTicker(pollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-h.wake:
		}
		if h.listeners() == 0 {
			continue
		}
		h.publish(h.poll(ctx))
	}
}
