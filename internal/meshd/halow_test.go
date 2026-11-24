package meshd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"meridian/trellis/internal/gossip"
	"meridian/trellis/internal/halowfiles"
)

func halowSum(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

// halowFetched waits for the fetch a pass started, if one runs.
func halowFetched(t *testing.T, d *Daemon) {
	t.Helper()
	d.halow.mu.Lock()
	done, fetching := d.halow.done, d.halow.fetching
	d.halow.mu.Unlock()
	if !fetching {
		return
	}
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the fetch did not end")
	}
}

// A node with a HaLow module and no files gets them from a node of the
// mesh that holds them, installs them, and then holds them itself.
func TestHaLowFilesFromMesh(t *testing.T) {
	d, dir := testDaemon(t, nil)
	pins := filepath.Join(dir, "firmware.pins")
	os.WriteFile(pins, []byte("FW_REPO=https://github.com/MorseMicro/morse-firmware.git\nFW_REF=abc\nFW_FILE=firmware/mm6108.bin\nFW_SHA256="+halowSum("fw")+
		"\nBCF_FILE=bcf/quectel/bcf_x.bin\nBCF_SHA256="+halowSum("bcf")+"\nLICENSE_FILE=firmware/LICENSE\n"), 0o644)
	fwDir := filepath.Join(dir, "lib-firmware-morse")
	proc := filepath.Join(dir, "root")
	d.opts.HaLowPins, d.opts.HaLowFirmwareDir, d.opts.ProcRoot = pins, fwDir, proc

	// The holder: another node's package server.
	holder := t.TempDir()
	for n, c := range map[string]string{"mm6108.bin": "fw", "bcf_x.bin": "bcf", "LICENSE": "licence"} {
		os.WriteFile(filepath.Join(holder, n), []byte(c), 0o644)
	}
	p, _ := halowfiles.LoadPins(pins)
	srv := httptest.NewServer(halowfiles.Handler(func() *halowfiles.Pins { return p }, holder, func(net.IP) bool { return true }))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	d.opts.HaLowMeshPort = port
	d.peers = gossip.NewTable("02:00:00:00:00:01")
	d.peers.Update(&gossip.Record{V: 1, MAC: "02:00:00:00:00:02", ID: 2, Callsign: "node-2", Addr: "127.0.0.1", Boot: 1, Seq: 1, HaLowFiles: true}, "127.0.0.1")
	var installed []string
	d.opts.HaLowInstall = func(ctx context.Context, from string) error {
		os.MkdirAll(fwDir, 0o755)
		for _, n := range []string{"mm6108.bin", "bcf_x.bin", "LICENSE"} {
			data, err := os.ReadFile(filepath.Join(from, n))
			if err != nil {
				return err
			}
			installed = append(installed, n)
			os.WriteFile(filepath.Join(fwDir, n), data, 0o644)
		}
		return nil
	}
	r := &halowReconciler{d: d}
	ctx := context.Background()

	// No module: nothing to do, nothing said.
	res, err := r.Reconcile(ctx)
	if err != nil || len(res.Warnings) != 0 || installed != nil || d.halowHeld() {
		t.Fatalf("no module: %+v %v %v", res, err, installed)
	}

	os.MkdirAll(filepath.Join(proc, "proc/device-tree/soc/spi@7e204000/mm6108@0"), 0o755)
	// The pass starts the fetch and says what is missing; the fetch runs
	// outside it.
	if res, err := r.Reconcile(ctx); err != nil || len(res.Warnings) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	halowFetched(t, d)
	if len(installed) != 3 {
		t.Fatalf("installed %v", installed)
	}
	if res, _ := r.Reconcile(ctx); !d.halowHeld() || len(res.Warnings) != 0 {
		t.Fatalf("after install: held %v, %+v", d.halowHeld(), res.Warnings)
	}
}

// No holder and no uplink: an alert names the file, and retries back off.
func TestHaLowFilesMissingAlert(t *testing.T) {
	d, dir := testDaemon(t, nil)
	pins := filepath.Join(dir, "firmware.pins")
	os.WriteFile(pins, []byte("FW_REPO=https://invalid.invalid/x.git\nFW_REF=abc\nFW_FILE=firmware/mm6108.bin\nFW_SHA256="+halowSum("fw")+
		"\nBCF_FILE=bcf/quectel/bcf_x.bin\nBCF_SHA256="+halowSum("bcf")+"\n"), 0o644)
	fwDir := filepath.Join(dir, "fw")
	os.MkdirAll(fwDir, 0o755)
	os.WriteFile(filepath.Join(fwDir, "mm6108.bin"), []byte("fw"), 0o644) // the image's
	proc := filepath.Join(dir, "root")
	os.MkdirAll(filepath.Join(proc, "proc/device-tree/soc/spi@7e204000/mm6108@0"), 0o755)
	d.opts.HaLowPins, d.opts.HaLowFirmwareDir, d.opts.ProcRoot = pins, fwDir, proc
	d.opts.HaLowInstall = func(context.Context, string) error { t.Error("installed with nothing fetched"); return nil }
	r := &halowReconciler{d: d}
	res, err := r.Reconcile(context.Background())
	if err != nil || len(res.Warnings) != 1 || res.Warnings[0].Code != "halow-files-missing" {
		t.Fatalf("%+v %v", res, err)
	}
	if w := res.Warnings[0].Message; !contains(w, "bcf_x.bin") || contains(w, "mm6108.bin") {
		t.Fatalf("alert: %s", w)
	}
	halowFetched(t, d)
	res, err = r.Reconcile(context.Background())
	if err != nil || len(res.Warnings) != 1 || !contains(res.Warnings[0].Message, "Last try:") {
		t.Fatalf("after the try: %+v %v", res, err)
	}
	if res.RequeueAfter < halowRetryMin-10*time.Second || res.RequeueAfter > halowRetryMin {
		t.Fatalf("retry in %v", res.RequeueAfter)
	}
}

// A file that is there under a pinned name is the operator's (their own
// BCF, a trial's): never fetched over, no alert; the node does not say it
// holds the pinned files.
func TestHaLowOperatorFileStays(t *testing.T) {
	d, dir := testDaemon(t, nil)
	pins := filepath.Join(dir, "firmware.pins")
	os.WriteFile(pins, []byte("FW_REPO=https://invalid.invalid/x.git\nFW_REF=abc\nFW_FILE=firmware/mm6108.bin\nFW_SHA256="+halowSum("fw")+
		"\nBCF_FILE=bcf/quectel/bcf_x.bin\nBCF_SHA256="+halowSum("bcf")+"\n"), 0o644)
	fwDir := filepath.Join(dir, "fw")
	os.MkdirAll(fwDir, 0o755)
	os.WriteFile(filepath.Join(fwDir, "mm6108.bin"), []byte("fw"), 0o644)
	os.WriteFile(filepath.Join(fwDir, "bcf_x.bin"), []byte("the operator's own"), 0o644)
	proc := filepath.Join(dir, "root")
	os.MkdirAll(filepath.Join(proc, "proc/device-tree/soc/spi@7e204000/mm6108@0"), 0o755)
	d.opts.HaLowPins, d.opts.HaLowFirmwareDir, d.opts.ProcRoot = pins, fwDir, proc
	d.opts.HaLowInstall = func(context.Context, string) error { t.Error("installed over the operator's file"); return nil }
	res, err := (&halowReconciler{d: d}).Reconcile(context.Background())
	halowFetched(t, d)
	if err != nil || len(res.Warnings) != 0 || d.halowHeld() {
		t.Fatalf("%+v %v held %v", res, err, d.halowHeld())
	}
	if got, _ := os.ReadFile(filepath.Join(fwDir, "bcf_x.bin")); string(got) != "the operator's own" {
		t.Fatalf("the BCF is now %q", got)
	}
}

// A source that takes the connection and never answers does not hold the
// reconcile pass.
func TestHaLowFetchOutsideThePass(t *testing.T) {
	d, dir := testDaemon(t, nil)
	pins := filepath.Join(dir, "firmware.pins")
	os.WriteFile(pins, []byte("FW_REPO=https://invalid.invalid/x.git\nFW_REF=abc\nFW_FILE=firmware/mm6108.bin\nFW_SHA256="+halowSum("fw")+
		"\nBCF_FILE=bcf/quectel/bcf_x.bin\nBCF_SHA256="+halowSum("bcf")+"\n"), 0o644)
	proc := filepath.Join(dir, "root")
	os.MkdirAll(filepath.Join(proc, "proc/device-tree/soc/spi@7e204000/mm6108@0"), 0o755)
	d.opts.HaLowPins, d.opts.HaLowFirmwareDir, d.opts.ProcRoot = pins, filepath.Join(dir, "fw"), proc
	ctx, cancel := context.WithCancel(context.Background())
	d.ctx = ctx
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer hang.Close()
	u, _ := url.Parse(hang.URL)
	d.opts.HaLowMeshPort, _ = strconv.Atoi(u.Port())
	d.peers = gossip.NewTable("02:00:00:00:00:01")
	d.peers.Update(&gossip.Record{V: 1, MAC: "02:00:00:00:00:02", ID: 2, Callsign: "node-2", Addr: "127.0.0.1", Boot: 1, Seq: 1, HaLowFiles: true}, "127.0.0.1")
	r := &halowReconciler{d: d}
	start := time.Now()
	for range 2 { // the second pass finds the fetch still running
		if res, err := r.Reconcile(context.Background()); err != nil || len(res.Warnings) != 1 {
			t.Fatalf("%+v %v", res, err)
		}
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("two passes took %v", took)
	}
	cancel() // meshd stopping ends the fetch
	halowFetched(t, d)
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
