package gnss

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// Reports as gpsd 3.25 sends them for a u-blox receiver on a UART.
const (
	devices = `{"class":"DEVICES","devices":[{"class":"DEVICE","path":"/dev/ttyAMA0","driver":"u-blox","activated":"2026-01-02T06:33:40.000Z","flags":1,"native":1,"bps":9600}]}`
	noFix   = `{"class":"TPV","device":"/dev/ttyAMA0","mode":1,"time":"2026-01-02T06:33:47.000Z","ept":0.005}`
	fix3D   = `{"class":"TPV","device":"/dev/ttyAMA0","mode":3,"time":"2026-01-02T06:40:01.000Z","ept":0.005,"lat":52.123456,"lon":4.654321,"altHAE":180.2,"eph":4.1,"epv":7.3,"speed":0.1}`
	sky     = `{"class":"SKY","device":"/dev/ttyAMA0","nSat":14,"uSat":9,"satellites":[{"PRN":2,"used":true},{"PRN":5,"used":false}]}`
	skyPart = `{"class":"SKY","device":"/dev/ttyAMA0","hdop":0.9}`
	pps     = `{"class":"PPS","device":"/dev/ttyAMA0","real_sec":1794645601,"real_nsec":0,"clock_sec":1794645601,"clock_nsec":120,"precision":-20}`
)

func TestApply(t *testing.T) {
	now := time.Date(2026, 1, 2, 6, 40, 1, 0, time.UTC)
	var st State
	for _, l := range []string{devices, noFix} {
		apply(&st, []byte(l), now)
	}
	if !st.Receiver || st.Device != "/dev/ttyAMA0" || st.Driver != "u-blox" || st.Mode != ModeNoFix || Fix(st.Mode) != "no fix" || st.HAccM != 0 {
		t.Fatalf("no fix: %+v", st)
	}
	for _, l := range []string{fix3D, sky, skyPart, pps, "not json", `{"class":"WATCH"}`} {
		apply(&st, []byte(l), now)
	}
	switch {
	case st.Mode != Mode3D || st.Lat != 52.123456 || st.Lon != 4.654321 || !st.HasAlt || st.AltM != 180.2:
		t.Errorf("3D fix: %+v", st)
	case st.HAccM != 4.1 || st.VAccM != 7.3 || st.SatsUsed != 9 || st.SatsSeen != 14:
		t.Errorf("accuracy and satellites (a partial SKY must not clear them): %+v", st)
	case !st.Time.Equal(time.Date(2026, 1, 2, 6, 40, 1, 0, time.UTC)) || !st.Heard.Equal(now) || !st.PPS.Equal(now):
		t.Errorf("times: %+v", st)
	}
	// Losing the fix clears the position: a stale one is never shown as
	// current.
	apply(&st, []byte(noFix), now)
	if st.Lat != 0 || st.Lon != 0 || st.HasAlt || st.HAccM != 0 {
		t.Errorf("position kept after the fix was lost: %+v", st)
	}
}

// The watcher follows a gpsd, and says so when there is none.
func TestWatcher(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	watch := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		line, _ := bufio.NewReader(c).ReadString('\n')
		watch <- line
		for _, l := range []string{`{"class":"VERSION","release":"3.25"}`, devices, fix3D, sky} {
			c.Write([]byte(l + "\n"))
		}
		time.Sleep(2 * time.Second)
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &Watcher{Addr: ln.Addr().String()}
	go w.Run(ctx)
	if got := <-watch; !strings.HasPrefix(got, `?WATCH={"enable":true,"json":true`) {
		t.Fatalf("watch request %q", got)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && w.State().SatsUsed == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if st := w.State(); !st.Running || !st.Receiver || st.Mode != Mode3D || st.SatsUsed != 9 {
		t.Errorf("following gpsd: %+v", st)
	}

	none := &Watcher{Addr: "127.0.0.1:1"}
	nctx, ncancel := context.WithCancel(context.Background())
	go none.Run(nctx)
	time.Sleep(100 * time.Millisecond)
	ncancel()
	if st := none.State(); st.Running || st.Receiver || st.Err == "" {
		t.Errorf("no gpsd: %+v", st)
	}
}
