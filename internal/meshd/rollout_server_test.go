package meshd

import (
	"fmt"
	"net"
	"testing"
	"time"

	"meridian/trellis/internal/rollout"
)

// A node that holds the HaLow files keeps its package server with no
// rollout on, and still closes it when its mesh address changes and when
// meshd stops.
func TestPackageServerFollowsTheAddress(t *testing.T) {
	d, _ := testDaemon(t, nil)
	listening := func(addr string) bool {
		c, err := net.DialTimeout("tcp", net.JoinHostPort(addr, fmt.Sprint(rollout.Port)), time.Second)
		if err != nil {
			return false
		}
		c.Close()
		return true
	}
	setAddr := func(a string) {
		d.facts.mu.Lock()
		d.facts.BridgeAddress = a
		d.facts.mu.Unlock()
	}
	const first, second = "127.73.73.1", "127.73.73.2"
	if listening(first) || listening(second) {
		t.Skipf("port %d is in use on this machine", rollout.Port)
	}
	d.halow.held = true
	setAddr(first + "/16")
	if err := d.ensureRolloutServer(); err != nil || !listening(first) {
		t.Fatalf("first address: %v, listening %v", err, listening(first))
	}
	// No rollout on: the server stays for the HaLow files.
	d.stopRolloutServer()
	if !listening(first) {
		t.Fatal("stopped while the node holds the HaLow files")
	}
	setAddr(second + "/16")
	if err := d.ensureRolloutServer(); err != nil || !listening(second) {
		t.Fatalf("second address: %v, listening %v", err, listening(second))
	}
	if listening(first) {
		t.Fatal("the old address is still served")
	}
	// Back to the first: its port is free again.
	setAddr(first + "/16")
	if err := d.ensureRolloutServer(); err != nil || !listening(first) || listening(second) {
		t.Fatalf("back to the first address: %v", err)
	}
	if err := (&rolloutReconciler{d: d}).Teardown(t.Context()); err != nil || listening(first) {
		t.Fatalf("after teardown: %v, listening %v", err, listening(first))
	}
}
