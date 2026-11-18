package doctor

import (
	"testing"
	"time"
)

// A command that outlives cmdTimeout is cut off with its children, even
// when they hold its output open (dkms is a shell script).
func TestCommandTimeoutKillsChildren(t *testing.T) {
	old := cmdTimeout
	cmdTimeout = 200 * time.Millisecond
	defer func() { cmdTimeout = old }()
	start := time.Now()
	_, err := output("sh", "-c", "sleep 30 & sleep 30")
	if err == nil {
		t.Error("no error from a command that timed out")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("took %v", d)
	}
}
