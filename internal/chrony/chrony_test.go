package chrony

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseTracking(t *testing.T) {
	for name, c := range map[string]struct {
		out              string
		ref              string
		synced, fromGNSS bool
		offset           time.Duration
	}{
		// chronyc 4.x on a desktop following an internet server.
		"server": {out: "C755082E,199.85.8.46,3,1794637769.144725369,-0.000090921,0.000045180,0.000131108,2.937,0.004,0.038,0.013318513,0.000686926,1031.0,Normal\n",
			ref: "199.85.8.46", synced: true, offset: -90921 * time.Nanosecond},
		"gnss": {out: "474E5353,GNSS,1,1794637769.0,0.012500000,0,0,0,0,0,0,0,16.0,Normal\n",
			ref: "GNSS", synced: true, fromGNSS: true, offset: 12500 * time.Microsecond},
		"unsynced": {out: "00000000,,0,0.000000000,0.000000000,0.000000000,0.000000000,0.000,0.000,0.000,1.000000000,1.000000000,0.0,Not synchronised\n"},
	} {
		tr, err := ParseTracking([]byte(c.out))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if tr.RefName != c.ref || tr.Synced() != c.synced || tr.FromGNSS() != c.fromGNSS || tr.Offset != c.offset {
			t.Errorf("%s: %+v synced=%v gnss=%v", name, tr, tr.Synced(), tr.FromGNSS())
		}
	}
	if _, err := ParseTracking([]byte("506 Cannot talk to daemon\n")); err == nil {
		t.Error("an error message parsed as tracking")
	}
}

func TestParseSources(t *testing.T) {
	out := "^,-,23.186.168.132,2,10,377,404,0.006523680,0.006523680,0.045791719\n" +
		"^,*,10.73.89.1,2,6,377,415,-0.001002068,-0.000956888,0.007556613\n" +
		"#,?,GNSS,0,4,0,-,0,0,0\n"
	srcs, err := ParseSources([]byte(out))
	if err != nil || len(srcs) != 3 {
		t.Fatalf("%+v %v", srcs, err)
	}
	if s := srcs[1]; s.Mode != '^' || s.State != '*' || s.Name != "10.73.89.1" || s.Stratum != 2 || s.Reach != 0o377 {
		t.Errorf("selected server: %+v", s)
	}
	if s := srcs[2]; s.Mode != '#' || s.State != '?' || s.Reach != 0 {
		t.Errorf("unreachable refclock: %+v", s)
	}
}

func TestWriteSources(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "trellis-chrony")
	changed, err := WriteSources(dir, []string{"10.73.89.1", "10.73.12.1"})
	if err != nil || !changed {
		t.Fatalf("first write: %v %v", changed, err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, SourceFile))
	if !strings.Contains(string(data), "server 10.73.89.1 iburst minpoll 4 maxpoll 6\nserver 10.73.12.1 iburst") {
		t.Errorf("file:\n%s", data)
	}
	if changed, err := WriteSources(dir, []string{"10.73.89.1", "10.73.12.1"}); err != nil || changed {
		t.Errorf("same list rewrote the file: %v %v", changed, err)
	}
	if changed, _ := WriteSources(dir, nil); !changed {
		t.Error("an emptied list did not change the file")
	}
}
