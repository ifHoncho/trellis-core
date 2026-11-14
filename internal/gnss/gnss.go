// Package gnss follows a GNSS receiver through gpsd (roadmap track G): its
// fix, position, accuracy, satellites and PPS. gpsd owns the receiver;
// this is a reader of its JSON stream (port 2947), so any receiver gpsd
// supports works, on any port. Without gpsd there is simply no receiver.
package gnss

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"sync"
	"time"
)

// Addr is where gpsd serves its JSON stream.
const Addr = "127.0.0.1:2947"

// Fix modes, as gpsd numbers them.
const (
	ModeUnknown = 0
	ModeNoFix   = 1
	Mode2D      = 2
	Mode3D      = 3
)

// State is what is known of the receiver.
type State struct {
	// Running: gpsd answers. Receiver: it lists a device.
	Running, Receiver bool
	Device, Driver    string
	Mode              int
	// Time is the receiver's time of the last report, and Heard when it
	// arrived here.
	Time, Heard time.Time
	// Position, when Mode is 2D or 3D. AltM is height above the ellipsoid
	// where the receiver gives it.
	Lat, Lon, AltM float64
	HasAlt         bool
	// Estimated errors in metres (95%), 0 when unknown.
	HAccM, VAccM float64
	// Satellites used in the fix, and seen.
	SatsUsed, SatsSeen int
	// PPS is the last pulse-per-second report; zero without PPS.
	PPS time.Time
	Err string
}

// Watcher keeps a State current from gpsd.
type Watcher struct {
	Addr string // default Addr

	mu sync.Mutex
	st State
}

// State returns a copy of the current state.
func (w *Watcher) State() State {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.st
}

// Run follows gpsd until ctx ends, reconnecting with a growing delay while
// gpsd is not there (most nodes have no receiver).
func (w *Watcher) Run(ctx context.Context) {
	addr := w.Addr
	if addr == "" {
		addr = Addr
	}
	delay := 5 * time.Second
	for ctx.Err() == nil {
		err := w.follow(ctx, addr)
		w.mu.Lock()
		w.st = State{Err: errString(err)}
		w.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, time.Minute)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// follow reads one connection's reports until it ends.
func (w *Watcher) follow(ctx context.Context, addr string) error {
	var d net.Dialer
	dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	conn, err := d.DialContext(dctx, "tcp", addr)
	cancel()
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if _, err := conn.Write([]byte(`?WATCH={"enable":true,"json":true,"pps":true};` + "\n")); err != nil {
		return err
	}
	w.mu.Lock()
	w.st = State{Running: true}
	w.mu.Unlock()
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for {
		// gpsd reports every second while a receiver is attached; a
		// silent minute means it has gone.
		_ = conn.SetReadDeadline(time.Now().Add(time.Minute))
		if !sc.Scan() {
			return sc.Err()
		}
		w.mu.Lock()
		apply(&w.st, sc.Bytes(), time.Now())
		w.mu.Unlock()
	}
}

// report is the union of the gpsd reports used here.
type report struct {
	Class   string `json:"class"`
	Devices []struct {
		Path   string `json:"path"`
		Driver string `json:"driver"`
	} `json:"devices"`
	Device string   `json:"device"`
	Driver string   `json:"driver"`
	Mode   int      `json:"mode"`
	Time   string   `json:"time"`
	Lat    *float64 `json:"lat"`
	Lon    *float64 `json:"lon"`
	AltHAE *float64 `json:"altHAE"`
	Alt    *float64 `json:"alt"`
	EPH    float64  `json:"eph"`
	EPV    float64  `json:"epv"`
	NSat   *int     `json:"nSat"`
	USat   *int     `json:"uSat"`
	Sats   []struct {
		Used bool `json:"used"`
	} `json:"satellites"`
	RealSec int64 `json:"real_sec"`
}

// apply folds one line of gpsd's stream into st. Lines it does not know
// are ignored.
func apply(st *State, line []byte, now time.Time) {
	var r report
	if json.Unmarshal(line, &r) != nil {
		return
	}
	switch r.Class {
	case "DEVICES":
		st.Receiver = len(r.Devices) > 0
		st.Device, st.Driver = "", ""
		if st.Receiver {
			st.Device, st.Driver = r.Devices[0].Path, r.Devices[0].Driver
		}
	case "DEVICE":
		if r.Device != "" && (st.Device == "" || st.Device == r.Device) {
			st.Receiver, st.Device = true, r.Device
			if r.Driver != "" {
				st.Driver = r.Driver
			}
		}
	case "TPV":
		st.Receiver = true
		if r.Device != "" {
			st.Device = r.Device
		}
		st.Mode, st.Heard = r.Mode, now
		if t, err := time.Parse(time.RFC3339Nano, r.Time); err == nil {
			st.Time = t
		}
		st.Lat, st.Lon, st.HasAlt, st.AltM, st.HAccM, st.VAccM = 0, 0, false, 0, 0, 0
		if r.Mode >= Mode2D && r.Lat != nil && r.Lon != nil {
			st.Lat, st.Lon, st.HAccM = *r.Lat, *r.Lon, r.EPH
		}
		if r.Mode >= Mode3D {
			switch {
			case r.AltHAE != nil:
				st.AltM, st.HasAlt = *r.AltHAE, true
			case r.Alt != nil:
				st.AltM, st.HasAlt = *r.Alt, true
			}
			st.VAccM = r.EPV
		}
	case "SKY":
		used, seen := 0, len(r.Sats)
		for _, s := range r.Sats {
			if s.Used {
				used++
			}
		}
		if r.USat != nil {
			used = *r.USat
		}
		if r.NSat != nil {
			seen = *r.NSat
		}
		// gpsd also sends partial SKY reports without satellites; they
		// say nothing about the counts.
		if r.Sats != nil || r.NSat != nil {
			st.SatsUsed, st.SatsSeen = used, seen
		}
	case "PPS":
		if r.RealSec > 0 {
			st.PPS = now
		}
	}
}

// Fix names a fix mode for people: "no fix", "2D", "3D".
func Fix(mode int) string {
	switch mode {
	case Mode2D:
		return "2D"
	case Mode3D:
		return "3D"
	}
	return "no fix"
}
