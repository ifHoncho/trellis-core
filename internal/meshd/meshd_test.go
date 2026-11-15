package meshd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"meridian/trellis/internal/config"
)

func TestLockInstanceExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meshd.lock")
	unlock, err := lockInstance(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockInstance(path); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second lock: want ErrAlreadyRunning, got %v", err)
	}
	unlock()
	unlock2, err := lockInstance(path)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	unlock2()
}

func TestChurnGuard(t *testing.T) {
	r := &radioReconciler{d: &Daemon{log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	for i := 0; i < churnLimit; i++ {
		if err := r.checkChurn("mesh0"); err != nil {
			t.Fatalf("creation %d refused: %v", i+1, err)
		}
	}
	err := r.checkChurn("mesh0")
	if err == nil || !strings.Contains(err.Error(), "NetworkManager") {
		t.Fatalf("expected a churn error naming the likely culprits, got %v", err)
	}
	if err := r.checkChurn("ap0"); err != nil {
		t.Fatalf("other interfaces are tracked separately: %v", err)
	}
	// Old creations age out of the window.
	old := time.Now().Add(-churnWindow - time.Second)
	r.created["mesh0"] = []time.Time{old, old, old}
	if err := r.checkChurn("mesh0"); err != nil {
		t.Fatalf("aged-out creations must not count: %v", err)
	}
}

func TestClientNetworkAddressing(t *testing.T) {
	if a, err := anycastAddress("10.73.0.0/16"); err != nil || a != "10.73.0.1" {
		t.Errorf("anycast = %q, %v", a, err)
	}
	s, e, m, err := dhcpPool("10.73.0.0/16", 7)
	if err != nil || s != "10.73.7.100" || e != "10.73.7.199" || m != "255.255.0.0" {
		t.Errorf("pool = %s %s %s %v", s, e, m, err)
	}
	// MT7610U: second interface differs in the first byte.
	perm, _ := net.ParseMAC("a0:b0:c0:00:01:f1")
	if got := apMAC(perm, "mt76x0u").String(); got != "a2:b0:c0:00:01:f1" {
		t.Errorf("apMAC mt76x0u = %s", got)
	}
	// RT5572: low bits of the last byte, so the hardware BSS slots differ.
	perm2, _ := net.ParseMAC("a0:b0:c0:00:81:ea")
	got := apMAC(perm2, "rt2800usb")
	if got.String() != "a0:b0:c0:00:81:eb" || got[5]&7 == perm2[5]&7 {
		t.Errorf("apMAC rt2800usb = %s", got)
	}
	local, _ := net.ParseMAC("02:11:22:33:44:55")
	if got := apMAC(local, "mt76x2u").String(); got != "06:11:22:33:44:55" {
		t.Errorf("apMAC mt76x2u of a local address = %s", got)
	}
}

func TestWarmupFrame(t *testing.T) {
	src := net.HardwareAddr{0xa0, 0xb0, 0xc0, 0x00, 0x00, 0x01}
	f := warmupFrame(src, 300)
	if len(f) != 60 || !bytes.Equal(f[:6], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}) || !bytes.Equal(f[6:12], src) {
		t.Fatalf("frame header: % x", f[:14])
	}
	if f[12] != 0x88 || f[13] != 0xb5 {
		t.Fatalf("ethertype %x%x, want 88b5 (the one the bridge filter drops)", f[12], f[13])
	}
	if f[40] != 0x01 || f[41] != 0x2c {
		t.Fatalf("sequence field % x", f[40:42])
	}
	// The number of frames must cover batman-adv's broadcast window.
	if warmupCount < 64 {
		t.Fatal("warmupCount below batman-adv's 64-number broadcast window")
	}
}

func TestInputPolicy(t *testing.T) {
	n := &config.Node{}
	n.ApplyDefaults()
	if inputPolicy(n) != nil {
		t.Fatal("a node without a firewall setting is closed")
	}
	no := false
	n.Firewall = config.FirewallConfig{WAN: config.FirewallClosed, Allow: []string{"tcp/5201", "udp/51820", "tcp/22"}}
	n.Web.WANAccess = true
	in := inputPolicy(n)
	if in == nil || fmt.Sprint(in.TCP) != "[22 80 443 5201]" || fmt.Sprint(in.UDP) != "[51820]" {
		t.Fatalf("policy %+v", in)
	}
	n.Firewall.SSH, n.Web.WANAccess, n.Firewall.Allow = &no, false, nil
	if in := inputPolicy(n); len(in.TCP) != 0 || len(in.UDP) != 0 {
		t.Fatalf("closed with nothing open: %+v", in)
	}
}
