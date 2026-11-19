package config

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"time"
)

// ChangeInfo says which mesh-wide change made a profile's revision, and
// which changes came before it. Revision numbers alone cannot tell two
// parts of a mesh that each changed the profile while apart from one that
// is simply behind: both may arrive at the same number. The change IDs can.
//
// A change made on one node only (an experiment) keeps the profile's
// ChangeInfo: it is still based on that change, and is not a branch.
type ChangeInfo struct {
	// ID is random per change, 16 hex digits.
	ID string `yaml:"id" json:"id"`
	// At is when the change took effect: the switch moment it was sent with.
	At time.Time `yaml:"at" json:"at"`
	// By is the callsign of the node it was made on.
	By string `yaml:"by,omitempty" json:"by,omitempty"`
	// Earlier are the IDs of the changes before it, oldest first, at most
	// MaxEarlierChanges.
	Earlier []string `yaml:"earlier,omitempty" json:"earlier,omitempty"`
}

// MaxEarlierChanges is how many earlier changes a profile remembers. Two
// parts of a mesh apart for more changes than this are taken to have
// changed it apart, and are reconciled as such.
const MaxEarlierChanges = 32

var reChangeID = regexp.MustCompile(`^[0-9a-f]{16}$`)

// ValidChangeID reports whether s looks like a change ID.
func ValidChangeID(s string) bool { return reChangeID.MatchString(s) }

// NewChangeID returns a fresh random change ID.
func NewChangeID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand does not fail on Linux
	}
	return hex.EncodeToString(b[:])
}

func (c *ChangeInfo) validate(es *Errors) {
	if !ValidChangeID(c.ID) {
		es.add("change.id", "must be 16 hex digits (got %q)", c.ID)
	}
	if c.At.IsZero() {
		es.add("change.at", "required")
	}
	if c.By != "" && !reCallsign.MatchString(c.By) {
		es.add("change.by", "must be a callsign (got %q)", c.By)
	}
	if len(c.Earlier) > MaxEarlierChanges {
		es.add("change.earlier", "at most %d (got %d)", MaxEarlierChanges, len(c.Earlier))
	}
	for _, id := range c.Earlier {
		if !ValidChangeID(id) {
			es.add("change.earlier", "must be change IDs (got %q)", id)
			break
		}
	}
}

// ChangeID is the ID of the change that made p, or "" for a profile never
// changed mesh-wide (or changed by a version that did not record it).
func (p *Profile) ChangeID() string {
	if p == nil || p.Change == nil {
		return ""
	}
	return p.Change.ID
}

// Knows reports whether p is change id, or came after it.
func (p *Profile) Knows(id string) bool {
	if p.Change == nil || id == "" {
		return false
	}
	return p.Change.ID == id || slices.Contains(p.Change.Earlier, id)
}

// StartChange makes p the mesh-wide change that follows prev: a new ID,
// the moment it takes effect, the node it is made on, and prev's change
// among the earlier ones.
func (p *Profile) StartChange(prev *Profile, at time.Time, by string) {
	var earlier []string
	if prev != nil && prev.Change != nil {
		earlier = append(slices.Clone(prev.Change.Earlier), prev.Change.ID)
	}
	p.Change = &ChangeInfo{ID: NewChangeID(), At: at.UTC().Truncate(time.Second), By: by, Earlier: lastN(earlier, MaxEarlierChanges)}
}

// Relation is how one node's profile stands to another's.
type Relation int

const (
	// RelUnknown: one of them has no change record (a profile never
	// changed mesh-wide, or from an older version): only the revision
	// numbers can be compared.
	RelUnknown  Relation = iota
	RelSame              // the same change
	RelBehind            // the other came after this one
	RelAhead             // this one came after the other
	RelDiverged          // each was changed without the other's change
)

func (r Relation) String() string {
	return [...]string{"unknown", "same", "behind", "ahead", "diverged"}[r]
}

// Compare says how p stands to q.
func Compare(p, q *Profile) Relation {
	a, b := p.ChangeID(), q.ChangeID()
	switch {
	case a == "" || b == "":
		return RelUnknown
	case a == b:
		return RelSame
	case q.Knows(a):
		return RelBehind
	case p.Knows(b):
		return RelAhead
	}
	return RelDiverged
}

// Reconcile settles two profiles that were changed apart (RelDiverged):
// the later change is kept (by the moment each took effect; the higher ID
// on a tie), and the result is a new revision above both that counts both
// changes as earlier ones. Every node that has the two profiles makes the
// same result, byte for byte, whichever it holds itself: the two parts of
// the mesh agree without a round of messages.
func Reconcile(a, b *Profile) (merged, kept, dropped *Profile, err error) {
	if a.Change == nil || b.Change == nil {
		return nil, nil, nil, fmt.Errorf("both profiles need a change record")
	}
	if a.MeshID != b.MeshID || a.SAEPassphrase != b.SAEPassphrase {
		return nil, nil, nil, fmt.Errorf("the profiles are of different meshes")
	}
	kept, dropped = a, b
	if b.Change.At.After(a.Change.At) || (b.Change.At.Equal(a.Change.At) && b.Change.ID > a.Change.ID) {
		kept, dropped = b, a
	}
	merged, err = cloneProfile(kept)
	if err != nil {
		return nil, nil, nil, err
	}
	merged.Revision = max(a.Revision, b.Revision) + 1
	ids := []string{a.Change.ID, b.Change.ID}
	slices.Sort(ids)
	sum := sha256.Sum256([]byte("trellis-reconcile\n" + ids[0] + "\n" + ids[1]))
	var earlier []string
	for _, id := range append(append(slices.Clone(kept.Change.Earlier), dropped.Change.Earlier...), kept.Change.ID, dropped.Change.ID) {
		if !slices.Contains(earlier, id) {
			earlier = append(earlier, id)
		}
	}
	merged.Change = &ChangeInfo{ID: hex.EncodeToString(sum[:8]), At: kept.Change.At, By: kept.Change.By, Earlier: lastN(earlier, MaxEarlierChanges)}
	return merged, kept, dropped, nil
}

func cloneProfile(p *Profile) (*Profile, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	var q Profile
	if err := json.Unmarshal(b, &q); err != nil {
		return nil, err
	}
	return &q, nil
}

func lastN(s []string, n int) []string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// Digest is a short fingerprint of the profile, the secret left out: it
// differs whenever what the profile says does. Status carries it, so that
// whoever holds a copy of the configuration knows when to read it again.
func (p Profile) Digest() string {
	data, err := json.Marshal(p.Redacted())
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:6])
}

// Digest is a short fingerprint of the node configuration, secrets left
// out: it differs whenever what the configuration says does.
func (n Node) Digest() string {
	data, err := json.Marshal(n.Redacted())
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:6])
}
