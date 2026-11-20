package meshd

import (
	"testing"
	"time"
)

func TestSpoken(t *testing.T) {
	for d, want := range map[time.Duration]string{
		40 * time.Second: "40 s", 61 * time.Second: "1 min", 47 * time.Minute: "47 min",
		2 * time.Hour: "2 h", 125 * time.Minute: "2 h 5 min", 73 * time.Hour: "3 d",
	} {
		if got := spoken(d); got != want {
			t.Errorf("spoken(%v) = %q, want %q", d, got, want)
		}
	}
}
