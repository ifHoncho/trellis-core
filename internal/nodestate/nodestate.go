// Package nodestate persists what a node must remember across reboots and
// that is not configuration: its identity (architecture §7.3) and what it
// has learned about its radios (§5.2).
package nodestate

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"meridian/trellis/internal/fsutil"
)

// File names under the state directory.
const (
	IdentityFile = "identity.yaml"
	RadiosFile   = "radios.yaml"
)

// Identity is the node's persistent identity.
type Identity struct {
	MAC          string    `yaml:"mac"`            // locally administered unicast; used for bat0 and br-mesh
	NodeID       int       `yaml:"node_id"`        // 1–254
	NodeIDSource string    `yaml:"node_id_source"` // IDConfig, IDDerived or IDClaimed
	Created      time.Time `yaml:"created"`
	// APPassphrase is the generated client-AP passphrase, used when the node
	// config sets none. Generated once so phones stay joined across restarts.
	APPassphrase string `yaml:"ap_passphrase,omitempty"`

	// Recovered says, when EnsureIdentity had to restore the identity from
	// its backup, what was wrong with the file. Not stored.
	Recovered string `yaml:"-"`
}

// HardwareAddr parses the MAC.
func (id *Identity) HardwareAddr() net.HardwareAddr {
	m, _ := net.ParseMAC(id.MAC)
	return m
}

// RandomMAC returns a locally administered unicast MAC.
func RandomMAC() (net.HardwareAddr, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	b[0] = (b[0] | 0x02) &^ 0x01
	return net.HardwareAddr(b), nil
}

// passphraseAlphabet leaves out characters that are easy to misread when
// typed from a screen (0/O, 1/l/I).
const passphraseAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

// GeneratePassphrase returns n random characters from a typing-friendly
// alphabet: about 4.95 bits each, so 12 characters give 59 bits.
func GeneratePassphrase(n int) (string, error) {
	out := make([]byte, n)
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for i := range out {
		// Rejection-free: 31 letters, bias from 256 % 31 is negligible for
		// an AP passphrase shown to the operator anyway.
		out[i] = passphraseAlphabet[int(buf[i])%len(passphraseAlphabet)]
	}
	return string(out), nil
}

// Node ID sources.
const (
	IDConfig  = "config"  // pinned in node.yaml
	IDDerived = "derived" // hashed from the identity MAC
	IDClaimed = "claimed" // taken after losing a conflict (architecture §7.3)
)

// DeriveNodeID hashes a MAC into 1–254: a node's first choice of ID when
// none is pinned. Collisions are possible; gossip resolves them
// (KeepsID, ClaimNodeID).
func DeriveNodeID(mac net.HardwareAddr) int { return deriveNodeID(mac, 0) }

// deriveNodeID is the attempt-th ID in this MAC's own sequence. Attempt 0
// is DeriveNodeID. Each MAC walks a different sequence, so two nodes that
// lose conflicts at the same moment rarely land on the same new ID.
func deriveNodeID(mac net.HardwareAddr, attempt int) int {
	h := fnv.New32a()
	h.Write(mac)
	if attempt > 0 {
		h.Write([]byte{byte(attempt >> 8), byte(attempt)})
	}
	return int(h.Sum32()%254) + 1
}

// ClaimNodeID picks a new ID for a node that lost its ID: the first ID in
// the MAC's sequence that is not in used and is not current. When the
// sequence finds none, the lowest free ID; ok is false when all 254 are
// taken.
func ClaimNodeID(mac net.HardwareAddr, used map[int]bool, current int) (id int, ok bool) {
	for a := 1; a <= 512; a++ {
		if n := deriveNodeID(mac, a); n != current && !used[n] {
			return n, true
		}
	}
	for n := 1; n <= 254; n++ {
		if n != current && !used[n] {
			return n, true
		}
	}
	return 0, false
}

// KeepsID reports whether node a keeps a node ID that node b also claims
// (architecture §7.3): an ID pinned in configuration beats a derived or
// claimed one, and otherwise the lower identity MAC keeps it. Both nodes
// evaluate the same rule from the same gossip, so exactly one of them
// moves. Identical MACs (a cloned identity) never keep: CloneKeeps decides.
func KeepsID(aSource, aMAC, bSource, bMAC string) bool {
	ac, bc := aSource == IDConfig, bSource == IDConfig
	if ac != bc {
		return ac
	}
	return macLess(aMAC, bMAC)
}

// CloneKeeps reports whether node a keeps an identity MAC that node b also
// uses (a cloned SD card): the node with the lower mesh hard-interface MAC
// keeps it; those are burnt into the radios and differ. a and b are each
// node's hard-interface MACs. With none to compare, a tie-breaker the two
// sides also see the same way (their meshd boot values) decides.
func CloneKeeps(aOrig, bOrig []string, aBoot, bBoot uint64) bool {
	am, bm := lowestMAC(aOrig), lowestMAC(bOrig)
	if am != "" && bm != "" && am != bm {
		return macLess(am, bm)
	}
	return aBoot < bBoot
}

func lowestMAC(macs []string) string {
	low := ""
	for _, m := range macs {
		if low == "" || macLess(m, low) {
			low = m
		}
	}
	return low
}

// macLess compares MACs numerically; unparsable ones sort last.
func macLess(a, b string) bool {
	ma, ea := net.ParseMAC(a)
	mb, eb := net.ParseMAC(b)
	if ea != nil || eb != nil {
		return ea == nil && eb != nil
	}
	return bytes.Compare(ma, mb) < 0
}

// SaveIdentity writes the identity file, and then its backup.
func SaveIdentity(dir string, id *Identity) error {
	path := filepath.Join(dir, IdentityFile)
	if err := writeAtomic(path, id, 0o600); err != nil {
		return err
	}
	// The backup is what EnsureIdentity falls back on when the file itself
	// is damaged; a failure to write it does not fail the save.
	_ = writeAtomic(path+backupSuffix, id, 0o600)
	return nil
}

// backupSuffix names the identity's backup; damagedSuffix the damaged file
// a recovery set aside.
const (
	backupSuffix  = ".bak"
	damagedSuffix = ".bad"
)

// errEmptyIdentity is an identity file with nothing in it: a write that
// never reached the disk (releases before 0.2.2 did not sync it).
var errEmptyIdentity = errors.New("the file is empty")

// readIdentity reads an identity file. It reports an existing file that
// is damaged (empty, cut short, a MAC that is not one) as an error; a
// missing one as os.ErrNotExist. A file written by hand without a MAC is
// fine: EnsureIdentity gives it one.
func readIdentity(path string) (*Identity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	id := &Identity{}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errEmptyIdentity
	}
	if err := yaml.Unmarshal(data, id); err != nil {
		return nil, err
	}
	if id.MAC != "" {
		m, err := net.ParseMAC(id.MAC)
		if err != nil || len(m) != 6 {
			return nil, fmt.Errorf("mac %q is not a MAC address", id.MAC)
		}
		id.MAC = m.String() // the form gossip uses (lower case, colons)
	}
	return id, nil
}

// EnsureIdentity loads the identity file, creating it on first run, and
// reconciles the node ID with the pinned value from config (0 = none).
// It returns the identity and whether the file was written.
//
// A damaged file (a power loss while it was written, a bad edit) is never
// replaced by a new identity, which would quietly give the node a new MAC,
// node ID and Wi-Fi passphrase: the backup written with every save is
// used, and the damaged file kept beside it. With no usable backup either,
// EnsureIdentity fails and says what to do.
func EnsureIdentity(dir string, pinnedID int) (*Identity, bool, error) {
	path := filepath.Join(dir, IdentityFile)
	changed := false
	id, err := readIdentity(path)
	switch {
	case err == nil:
	case errors.Is(err, os.ErrNotExist):
		if bak, berr := readIdentity(path + backupSuffix); berr == nil {
			id, changed = bak, true
			id.Recovered = "the identity file was missing; restored from " + IdentityFile + backupSuffix
		} else {
			id, changed = &Identity{}, true // first run
		}
	default:
		bak, berr := readIdentity(path + backupSuffix)
		if berr != nil && errors.Is(err, errEmptyIdentity) {
			// An empty file holds nothing to protect: it is a first save
			// that never reached the disk, or one from a release that did
			// not sync it and made no backup. Start afresh, as those
			// releases did, rather than keep the node off the mesh.
			if rerr := os.Rename(path, path+damagedSuffix); rerr != nil {
				return nil, false, fmt.Errorf("%s is empty; cannot set it aside: %w", path, rerr)
			}
			id, changed = &Identity{}, true
			id.Recovered = "the identity file was empty and there was no backup; this node has a new identity, the empty file kept as " + IdentityFile + damagedSuffix
			break
		}
		if berr != nil {
			return nil, false, fmt.Errorf("%s is damaged (%v), and there is no usable backup (%s: %v). "+
				"meshd will not make a new identity over it: that would give this node a new MAC, node ID and Wi-Fi passphrase. "+
				"Restore the file, or remove it to start this node with a new identity", path, err, IdentityFile+backupSuffix, berr)
		}
		if rerr := os.Rename(path, path+damagedSuffix); rerr != nil {
			return nil, false, fmt.Errorf("%s is damaged (%v); cannot set it aside: %w", path, err, rerr)
		}
		id, changed = bak, true
		id.Recovered = fmt.Sprintf("the identity file was damaged (%v); restored from %s, the damaged one kept as %s", err, IdentityFile+backupSuffix, IdentityFile+damagedSuffix)
	}
	if id.MAC == "" {
		mac, err := RandomMAC()
		if err != nil {
			return nil, false, err
		}
		id.MAC = mac.String()
		id.Created = time.Now().UTC().Truncate(time.Second)
		changed = true
	}
	if id.APPassphrase == "" {
		p, err := GeneratePassphrase(12)
		if err != nil {
			return nil, false, err
		}
		id.APPassphrase, changed = p, true
	}
	switch {
	case pinnedID != 0:
		if id.NodeID != pinnedID || id.NodeIDSource != IDConfig {
			id.NodeID, id.NodeIDSource, changed = pinnedID, IDConfig, true
		}
	case id.NodeID < 1 || id.NodeID > 254 || id.NodeIDSource == IDConfig:
		// No pin (any more): derive from the MAC so it stays stable. A
		// claimed ID (won after a conflict) is kept across restarts.
		id.NodeID, id.NodeIDSource, changed = DeriveNodeID(id.HardwareAddr()), IDDerived, true
	}
	if !changed {
		// Nodes set up before the backup existed get one now, and a hand
		// edit of the file reaches the backup.
		if bak, err := readIdentity(path + backupSuffix); err != nil || !sameIdentity(bak, id) {
			_ = writeAtomic(path+backupSuffix, id, 0o600)
		}
		return id, false, nil
	}
	if err := SaveIdentity(dir, id); err != nil {
		return nil, false, err
	}
	return id, true, nil
}

func sameIdentity(a, b *Identity) bool {
	return a.MAC == b.MAC && a.NodeID == b.NodeID && a.NodeIDSource == b.NodeIDSource &&
		a.Created.Equal(b.Created) && a.APPassphrase == b.APPassphrase
}

// RadioRecord is what the node remembers about one radio.
type RadioRecord struct {
	Fingerprint  string    `yaml:"fingerprint"`
	KernelIfname string    `yaml:"kernel_ifname,omitempty"` // interface the kernel created, removed by Trellis
	LastRole     string    `yaml:"last_role,omitempty"`
	FirstSeen    time.Time `yaml:"first_seen"`
	LastSeen     time.Time `yaml:"last_seen"`
	Port         string    `yaml:"port,omitempty"`
	Driver       string    `yaml:"driver,omitempty"`
}

// Radios is the radio memory.
type Radios struct {
	Radios map[string]*RadioRecord `yaml:"radios"`
}

// LoadRadios reads the radio memory; a missing file is an empty memory.
func LoadRadios(dir string) (*Radios, error) {
	r := &Radios{Radios: map[string]*RadioRecord{}}
	data, err := os.ReadFile(filepath.Join(dir, RadiosFile))
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(data, r); err != nil {
		// Only what the node learned about its radios: it learns it again.
		// The memory comes back empty, with the error to report.
		return &Radios{Radios: map[string]*RadioRecord{}}, fmt.Errorf("%s is damaged, starting with an empty radio memory: %w", RadiosFile, err)
	}
	if r.Radios == nil {
		r.Radios = map[string]*RadioRecord{}
	}
	return r, nil
}

// Save writes the radio memory.
func (r *Radios) Save(dir string) error {
	return writeAtomic(filepath.Join(dir, RadiosFile), r, 0o644)
}

// Get returns the record for a fingerprint, creating it if needed.
func (r *Radios) Get(fingerprint string) *RadioRecord {
	rec := r.Radios[fingerprint]
	if rec == nil {
		rec = &RadioRecord{Fingerprint: fingerprint, FirstSeen: time.Now().UTC().Truncate(time.Second)}
		r.Radios[fingerprint] = rec
	}
	return rec
}

func writeAtomic(path string, v any, mode os.FileMode) error {
	data, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	return fsutil.WriteFile(path, data, mode)
}

// FormatMAC lower-cases a MAC string.
func FormatMAC(s string) string { return strings.ToLower(s) }
