package config

import (
	"bytes"
	"testing"
	"time"
)

func changeProfile(t *testing.T, channel int, rev uint64) *Profile {
	t.Helper()
	p, err := ParseProfile([]byte("schema: 0\nname: trellis-alpha\nsae_passphrase: correct-horse-battery\nradio: {band: 2g, channel: 6}\n"))
	if err != nil {
		t.Fatal(err)
	}
	p.Radio.Channel = channel
	p.Revision = rev
	return p
}

func TestChangeRelations(t *testing.T) {
	t0 := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	base := changeProfile(t, 6, 0)
	if Compare(base, base) != RelUnknown {
		t.Fatal("profiles never changed mesh-wide compare only by revision")
	}
	r1 := changeProfile(t, 1, 1)
	r1.StartChange(base, t0, "node-1")
	if len(r1.Change.Earlier) != 0 || !ValidChangeID(r1.Change.ID) {
		t.Fatalf("first change: %+v", r1.Change)
	}
	r2 := changeProfile(t, 11, 2)
	r2.StartChange(r1, t0.Add(time.Hour), "node-2")
	if got := Compare(r1, r2); got != RelBehind {
		t.Errorf("r1 to r2: %v", got)
	}
	if got := Compare(r2, r1); got != RelAhead {
		t.Errorf("r2 to r1: %v", got)
	}
	if got := Compare(r2, r2); got != RelSame {
		t.Errorf("r2 to r2: %v", got)
	}
	// Apart: the other part changed r1 too, to the same revision number.
	other := changeProfile(t, 3, 2)
	other.StartChange(r1, t0.Add(30*time.Minute), "node-4")
	if got := Compare(r2, other); got != RelDiverged {
		t.Errorf("changed apart: %v", got)
	}

	// Each part makes the same result from the two profiles.
	m1, kept, dropped, err := Reconcile(r2, other)
	if err != nil {
		t.Fatal(err)
	}
	m2, _, _, err := Reconcile(other, r2)
	if err != nil {
		t.Fatal(err)
	}
	y1, _ := m1.YAML("")
	y2, _ := m2.YAML("")
	if !bytes.Equal(y1, y2) {
		t.Fatalf("the two parts reconcile differently:\n%s\n%s", y1, y2)
	}
	if kept != r2 || dropped != other || m1.Radio.Channel != 11 || m1.Revision != 3 {
		t.Errorf("the later change is kept: channel %d, revision %d", m1.Radio.Channel, m1.Revision)
	}
	for _, p := range []*Profile{r1, r2, other} {
		if Compare(p, m1) != RelBehind {
			t.Errorf("every part is behind the result: %v", Compare(p, m1))
		}
	}
	if err := m1.Validate(); err != nil {
		t.Errorf("result: %v", err)
	}
	// A third part that changed apart too reconciles with the result.
	if Compare(m1, changeProfileAfter(t, r1, t0)) != RelDiverged {
		t.Error("a third branch")
	}

	// A tie on the moment: the higher ID.
	a, b := changeProfile(t, 1, 5), changeProfile(t, 11, 5)
	a.StartChange(r1, t0, "a")
	b.StartChange(r1, t0, "b")
	_, k, _, _ := Reconcile(a, b)
	if want := map[bool]*Profile{true: a, false: b}[a.Change.ID > b.Change.ID]; k != want {
		t.Error("a tie keeps the higher ID")
	}

	// Different meshes are not reconciled.
	x := changeProfile(t, 1, 5)
	x.SAEPassphrase = "another-secret-1234"
	x.StartChange(r1, t0, "x")
	if _, _, _, err := Reconcile(a, x); err == nil {
		t.Error("another secret was reconciled")
	}
}

func changeProfileAfter(t *testing.T, prev *Profile, at time.Time) *Profile {
	p := changeProfile(t, 13, prev.Revision+1)
	p.StartChange(prev, at.Add(2*time.Hour), "node-5")
	return p
}

func TestChangeEarlierIsBounded(t *testing.T) {
	p := changeProfile(t, 6, 0)
	for i := range 3 * MaxEarlierChanges {
		q := changeProfile(t, 6, uint64(i+1))
		q.StartChange(p, time.Now(), "node-1")
		p = q
	}
	if len(p.Change.Earlier) != MaxEarlierChanges {
		t.Errorf("earlier: %d", len(p.Change.Earlier))
	}
	if err := p.Validate(); err != nil {
		t.Error(err)
	}
}

func TestChangeValidation(t *testing.T) {
	p := changeProfile(t, 6, 1)
	p.Change = &ChangeInfo{ID: "not-hex", By: "Bad Callsign!", Earlier: []string{"x"}}
	err := p.Validate()
	for _, f := range []string{"change.id", "change.at", "change.by", "change.earlier"} {
		if err == nil || !bytes.Contains([]byte(err.Error()), []byte(f)) {
			t.Errorf("%s not named: %v", f, err)
		}
	}
	// It survives the profile file.
	q := changeProfile(t, 6, 1)
	q.StartChange(nil, time.Now(), "node-1")
	y, _ := q.YAML("")
	r, err := ParseProfile(y)
	if err != nil || Compare(q, r) != RelSame {
		t.Fatalf("round trip: %v %v", Compare(q, r), err)
	}
}
