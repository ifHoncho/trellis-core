package nodestate

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureIdentityStable(t *testing.T) {
	dir := t.TempDir()
	id1, changed, err := EnsureIdentity(dir, 0)
	if err != nil || !changed {
		t.Fatalf("first run: %v changed=%v", err, changed)
	}
	mac := id1.HardwareAddr()
	if mac[0]&0x02 == 0 || mac[0]&0x01 != 0 {
		t.Errorf("MAC must be locally administered unicast: %s", mac)
	}
	if id1.NodeID < 1 || id1.NodeID > 254 || id1.NodeIDSource != "derived" {
		t.Errorf("derived id: %+v", id1)
	}
	id2, changed, err := EnsureIdentity(dir, 0)
	if err != nil || changed || id2.MAC != id1.MAC || id2.NodeID != id1.NodeID {
		t.Errorf("second run must be a no-op: %+v changed=%v err=%v", id2, changed, err)
	}
	id3, changed, _ := EnsureIdentity(dir, 42)
	if !changed || id3.NodeID != 42 || id3.NodeIDSource != "config" || id3.MAC != id1.MAC {
		t.Errorf("pin: %+v changed=%v", id3, changed)
	}
	id4, changed, _ := EnsureIdentity(dir, 0)
	if !changed || id4.NodeID != id1.NodeID || id4.NodeIDSource != "derived" {
		t.Errorf("unpin must re-derive the same id: %+v (want %d)", id4, id1.NodeID)
	}
}

func TestDeriveNodeIDRange(t *testing.T) {
	for i := 0; i < 1000; i++ {
		mac, _ := RandomMAC()
		if n := DeriveNodeID(mac); n < 1 || n > 254 {
			t.Fatalf("id out of range: %d", n)
		}
	}
	// Deterministic: the same MAC always gives the same ID, across runs and
	// builds (a node's derived ID must not move on an upgrade).
	m, _ := net.ParseMAC("02:00:00:00:00:01")
	first := DeriveNodeID(m)
	for i := 0; i < 10; i++ {
		if n := DeriveNodeID(append(net.HardwareAddr{}, m...)); n != first {
			t.Fatalf("not deterministic: %d then %d", first, n)
		}
	}
}

func TestRadiosRoundTrip(t *testing.T) {
	dir := t.TempDir()
	r, err := LoadRadios(dir)
	if err != nil || len(r.Radios) != 0 {
		t.Fatal(err)
	}
	rec := r.Get("mac:00:11:22:33:44:55")
	rec.KernelIfname = "wlan0"
	rec.LastRole = "mesh"
	if err := r.Save(dir); err != nil {
		t.Fatal(err)
	}
	r2, err := LoadRadios(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := r2.Radios["mac:00:11:22:33:44:55"]
	if got == nil || got.KernelIfname != "wlan0" || got.LastRole != "mesh" || got.FirstSeen.IsZero() {
		t.Errorf("round trip: %+v", got)
	}
}

// A damaged identity file must never be replaced by a new identity: the
// backup is used, or meshd refuses to start.
func TestEnsureIdentityRecovers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, IdentityFile)
	id, _, err := EnsureIdentity(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + backupSuffix); err != nil {
		t.Fatalf("no backup after the first save: %v", err)
	}
	for name, damage := range map[string]string{
		"empty":     "",
		"cut short": "mac: \"02:aa",
		"bad mac":   "mac: not-a-mac\nnode_id: 5\n",
	} {
		if err := os.WriteFile(path, []byte(damage), 0o600); err != nil {
			t.Fatal(err)
		}
		got, changed, err := EnsureIdentity(dir, 0)
		if err != nil || !changed || got.MAC != id.MAC || got.NodeID != id.NodeID || got.APPassphrase != id.APPassphrase || got.Recovered == "" {
			t.Errorf("%s: not recovered from the backup: %+v changed=%v err=%v", name, got, changed, err)
		}
		if b, _ := os.ReadFile(path + damagedSuffix); string(b) != damage {
			t.Errorf("%s: the damaged file was not kept: %q", name, b)
		}
		if again, _, _ := EnsureIdentity(dir, 0); again == nil || again.MAC != id.MAC || again.Recovered != "" {
			t.Errorf("%s: the restored file does not hold: %+v", name, again)
		}
	}

	// A missing file with a backup: restored.
	os.Remove(path)
	if got, _, err := EnsureIdentity(dir, 0); err != nil || got.MAC != id.MAC || got.Recovered == "" {
		t.Errorf("missing file: %+v %v", got, err)
	}

	// Damaged, and no usable backup: refused, and nothing is overwritten.
	const damaged = "mac: \"02:aa"
	os.WriteFile(path, []byte(damaged), 0o600)
	os.WriteFile(path+backupSuffix, []byte("mac: nope"), 0o600)
	if got, _, err := EnsureIdentity(dir, 0); err == nil || !strings.Contains(err.Error(), "will not make a new identity") {
		t.Errorf("damaged with no backup: %+v %v", got, err)
	}
	if b, _ := os.ReadFile(path); string(b) != damaged {
		t.Errorf("the damaged file was overwritten: %q", b)
	}

	// Empty, and no backup (a node upgraded from a release that made none):
	// a new identity, as those releases gave it, not a node off the mesh.
	os.WriteFile(path, nil, 0o600)
	os.Remove(path + backupSuffix)
	if got, changed, err := EnsureIdentity(dir, 0); err != nil || !changed || got.MAC == "" || got.Recovered == "" {
		t.Errorf("empty with no backup: %+v %v", got, err)
	}

	// Removing both is how a node gets a new identity.
	os.Remove(path)
	os.Remove(path + backupSuffix)
	if got, changed, err := EnsureIdentity(dir, 0); err != nil || !changed || got.MAC == id.MAC {
		t.Errorf("fresh start: %+v %v", got, err)
	}
}

// A hand edit of an otherwise unchanged file reaches the backup.
func TestEnsureIdentityBackupFollowsEdits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, IdentityFile)
	id, _, err := EnsureIdentity(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	edited := strings.Replace(string(b), id.APPassphrase, "edited-by-hand", 1)
	os.WriteFile(path, []byte(edited), 0o600)
	if _, changed, err := EnsureIdentity(dir, 0); err != nil || changed {
		t.Fatalf("after the edit: changed=%v %v", changed, err)
	}
	if bak, err := readIdentity(path + backupSuffix); err != nil || bak.APPassphrase != "edited-by-hand" {
		t.Errorf("the backup did not follow the edit: %+v %v", bak, err)
	}
}

// A file written by hand without a MAC gets one, as before.
func TestEnsureIdentityHandWritten(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, IdentityFile), []byte("node_id: 77\n"), 0o600)
	got, changed, err := EnsureIdentity(dir, 0)
	if err != nil || !changed || got.MAC == "" || got.Recovered != "" {
		t.Errorf("hand-written identity: %+v %v", got, err)
	}
}

func TestLoadRadiosDamaged(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, RadiosFile), []byte("radios: [this is: not"), 0o644)
	r, err := LoadRadios(dir)
	if err == nil || r == nil || len(r.Radios) != 0 {
		t.Errorf("damaged radio memory: %v %v", r, err)
	}
}
