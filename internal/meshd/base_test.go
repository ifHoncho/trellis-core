package meshd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"meridian/trellis/internal/api"
)

func isConflict(err error) bool {
	var ae *api.Error
	return errors.As(err, &ae) && ae.Status == http.StatusConflict
}

// A change made from an old copy of the configuration is refused: it
// would undo the change made since.
func TestStaleChangeRefused(t *testing.T) {
	d, _ := testDaemon(t, map[string]string{"node.yaml": "schema: 0\ncallsign: node-1\n", "profile.yaml": testProfile})
	ctx := context.Background()
	first := d.Config()
	if first.NodeDigest == "" || first.ProfileDigest == "" || first.NodeDigest == first.ProfileDigest {
		t.Fatalf("digests %q %q", first.NodeDigest, first.ProfileDigest)
	}
	// Two editors open the node's settings; one saves.
	a := first.Node
	a.Callsign = "relay-7"
	raw, _ := json.Marshal(a)
	if res, err := d.SetNode(ctx, raw, first.NodeDigest, false); err != nil || !res.Applied {
		t.Fatalf("first save: %+v %v", res, err)
	}
	// The other, from its old copy: refused, dry run included, and the
	// first one's change stands.
	b := first.Node
	b.Gateway.DownMbit = 5
	raw, _ = json.Marshal(b)
	for _, dry := range []bool{true, false} {
		if _, err := d.SetNode(ctx, raw, first.NodeDigest, dry); !isConflict(err) || !strings.Contains(err.Error(), "Read it again") {
			t.Fatalf("stale save (dry %v): %v", dry, err)
		}
	}
	now := d.Config()
	if now.Node.Callsign != "relay-7" || now.NodeDigest == first.NodeDigest {
		t.Fatalf("after the refusal: %+v", now.Node)
	}
	// From the copy it reads now, and with no base at all (meshctl): taken.
	b = now.Node
	b.Gateway.DownMbit = 5
	raw, _ = json.Marshal(b)
	if res, err := d.SetNode(ctx, raw, now.NodeDigest, false); err != nil || !res.Applied {
		t.Fatalf("save from the current copy: %+v %v", res, err)
	}
	b.Gateway.DownMbit = 6
	raw, _ = json.Marshal(b)
	if res, err := d.SetNode(ctx, raw, "", false); err != nil || !res.Applied {
		t.Fatalf("save with no base: %+v %v", res, err)
	}

	// The profile, the same way.
	p := first.Profile
	p.Radio.Channel = 157
	if res, err := d.SetProfile(ctx, api.ProfileInput{Profile: &p, Base: first.ProfileDigest}, false); err != nil || !res.Applied {
		t.Fatalf("profile: %+v %v", res, err)
	}
	q := first.Profile
	q.Radio.Width = 40
	if _, err := d.SetProfile(ctx, api.ProfileInput{Profile: &q, Base: first.ProfileDigest}, true); !isConflict(err) {
		t.Fatalf("stale profile: %v", err)
	}
	if _, err := d.SetMeshProfile(ctx, api.MeshProfileInput{ProfileInput: api.ProfileInput{Profile: &q, Base: first.ProfileDigest}}, true); !isConflict(err) {
		t.Fatalf("stale mesh-wide change: %v", err)
	}
}

// While a mesh-wide change is on its way, an editor's change is refused:
// it was made from the profile about to be replaced, and would drop what
// the pending change sets. A file pushed by hand still replaces it, and
// says so.
func TestMeshChangePendingRefusesEditors(t *testing.T) {
	d, _, _ := meshChangeDaemon(t)
	d.time = newTimeReconciler(d)
	ctx := context.Background()
	at := time.Now().Add(time.Minute)
	if err := d.acceptMeshChange(&meshChange{Rev: 1, ApplyAt: at, From: "node-2", YAML: profileAt(t, 40, 1, nil), State: "pending"}); err != nil {
		t.Fatal(err)
	}
	cfg := d.Config()
	p := cfg.Profile
	p.Radio.Channel = 157
	_, err := d.SetMeshProfile(ctx, api.MeshProfileInput{ProfileInput: api.ProfileInput{Profile: &p, Base: cfg.ProfileDigest}}, true)
	if !isConflict(err) || !strings.Contains(err.Error(), "from node-2 is on its way") {
		t.Fatalf("an editor's change while one is pending: %v", err)
	}
	res, err := d.SetMeshProfile(ctx, api.MeshProfileInput{ProfileInput: api.ProfileInput{Profile: &p}}, true)
	if err != nil || !strings.Contains(effectText(res.Effects, "mesh-change-replaced"), "is dropped") {
		t.Fatalf("a push with no base: %+v %v", res, err)
	}
	// On this node only: allowed, and told that the pending change
	// replaces it.
	res, err = d.SetProfile(ctx, api.ProfileInput{Profile: &p, Base: cfg.ProfileDigest}, true)
	if err != nil || !hasEffect(res.Effects, "warning", "mesh-change-pending") {
		t.Fatalf("this node only: %+v %v", res, err)
	}
}

// A change on its way is replaced only on purpose: a file's review is
// shown, sending it is refused without replace; with replace an editor's
// change goes too, and the profile in force cancels the one on its way.
func TestMeshChangeReplaceAndCancel(t *testing.T) {
	d, _, _ := meshChangeDaemon(t)
	d.time = newTimeReconciler(d)
	ctx := context.Background()
	at := time.Now().Add(time.Minute)
	if err := d.acceptMeshChange(&meshChange{Rev: 1, ApplyAt: at, From: "node-2", YAML: profileAt(t, 40, 1, nil), State: "pending"}); err != nil {
		t.Fatal(err)
	}
	cfg := d.Config()
	p := cfg.Profile
	p.Radio.Channel = 157
	file := api.ProfileInput{Profile: &p}
	// A file, sent without replace: refused, nothing changed.
	_, err := d.SetMeshProfile(ctx, api.MeshProfileInput{ProfileInput: file}, false)
	if !isConflict(err) || !strings.Contains(err.Error(), "--replace") {
		t.Fatalf("a push while one is pending: %v", err)
	}
	if c := d.meshChangeState(); c == nil || c.Rev != 1 || c.From != "node-2" {
		t.Fatalf("the pending change after a refused push: %+v", c)
	}
	// An editor's, with replace: reviewed, with what it replaces.
	res, err := d.SetMeshProfile(ctx, api.MeshProfileInput{ProfileInput: api.ProfileInput{Profile: &p, Base: cfg.ProfileDigest}, Replace: true}, true)
	if err != nil || !strings.Contains(effectText(res.Effects, "mesh-change-replaced"), "from node-2") || hasEffect(res.Effects, "warning", "mesh-change-cancelled") {
		t.Fatalf("an editor's replacement: %+v %v", res, err)
	}
	// With replace and a base that is no longer the node's: still refused.
	if _, err := d.SetMeshProfile(ctx, api.MeshProfileInput{ProfileInput: api.ProfileInput{Profile: &p, Base: "0000"}, Replace: true}, true); !isConflict(err) {
		t.Fatalf("a replacement from an old copy: %v", err)
	}
	// The profile in force with replace: a cancel, said so, and it would
	// be sent (without a change on its way it is nothing to change).
	same := cfg.Profile
	res, err = d.SetMeshProfile(ctx, api.MeshProfileInput{ProfileInput: api.ProfileInput{Profile: &same, Base: cfg.ProfileDigest}, Replace: true}, true)
	if err != nil || res.Changed || !strings.Contains(effectText(res.Effects, "mesh-change-cancelled"), "every node keeps the profile it runs now") {
		t.Fatalf("a cancel's review: %+v %v", res, err)
	}
	// Sent, it goes on to the offer (this test's daemon has no gossip to
	// send it with; lab P4 sends one): not "nothing to change".
	if _, err = d.SetMeshProfile(ctx, api.MeshProfileInput{ProfileInput: api.ProfileInput{Profile: &same}, Replace: true}, false); err == nil || !strings.Contains(err.Error(), "gossip has not started") {
		t.Fatalf("the cancel, sent: %v", err)
	}
}

// With no change on its way, replace alone sends nothing.
func TestMeshReplaceWithoutPending(t *testing.T) {
	d, _, _ := meshChangeDaemon(t)
	d.time = newTimeReconciler(d)
	same := d.Config().Profile
	res, err := d.SetMeshProfile(context.Background(), api.MeshProfileInput{ProfileInput: api.ProfileInput{Profile: &same}, Replace: true}, false)
	if err != nil || res.Applied || res.Changed || d.meshChangeState() != nil {
		t.Fatalf("replace with nothing to replace: %+v %v", res, err)
	}
}

// A file older than the mesh's profile, pushed to every node, undoes the
// changes since: said before it is sent.
func TestMeshPushOlderProfile(t *testing.T) {
	d, _, _ := meshChangeDaemon(t)
	d.time = newTimeReconciler(d)
	ctx := context.Background()
	d.profile.Revision = 5
	res, err := d.SetMeshProfile(ctx, api.MeshProfileInput{ProfileInput: api.ProfileInput{YAML: profileAt(t, 40, 3, nil)}}, true)
	if err != nil || !strings.Contains(effectText(res.Effects, "profile-older"), "revision 3, and the mesh runs revision 5") {
		t.Fatalf("older file: %+v %v", res, err)
	}
	// The mesh's own revision, and a hand-written file with none: not said.
	for _, rev := range []uint64{5, 0} {
		res, err = d.SetMeshProfile(ctx, api.MeshProfileInput{ProfileInput: api.ProfileInput{YAML: profileAt(t, 40, rev, nil)}}, true)
		if err != nil || effectText(res.Effects, "profile-older") != "" {
			t.Fatalf("revision %d: %+v %v", rev, res, err)
		}
	}
}

// A second save made before the first is swapped in is measured against
// the first, and keeps its secrets: against the configuration in force
// it brought the old passphrase back.
func TestSecondSaveKeepsTheFirst(t *testing.T) {
	d, _ := testDaemon(t, map[string]string{"node.yaml": "schema: 0\ncallsign: node-1\nap:\n  passphrase: first-passphrase\n", "profile.yaml": testProfile})
	ctx := context.Background()
	a := d.Config().Node
	a.AP.Passphrase = "second-passphrase"
	raw, _ := json.Marshal(a)
	if res, err := d.SetNode(ctx, raw, "", false); err != nil || !res.Applied {
		t.Fatalf("first save: %+v %v", res, err)
	}
	// Not swapped in yet (no pass has run). The page reads it back
	// (the passphrase redacted), changes something else and saves.
	b := d.Config().Node
	if b.AP.Passphrase == "second-passphrase" {
		t.Fatal("the passphrase is not redacted")
	}
	b.Gateway.DownMbit = 7
	raw, _ = json.Marshal(b)
	if res, err := d.SetNode(ctx, raw, d.Config().NodeDigest, false); err != nil || !res.Applied {
		t.Fatalf("second save: %+v %v", res, err)
	}
	node, _, _ := d.confWritten()
	if node.AP.Passphrase != "second-passphrase" || node.Gateway.DownMbit != 7 {
		t.Errorf("after the second save: passphrase %q, down %d", node.AP.Passphrase, node.Gateway.DownMbit)
	}
	// Saving the same again: nothing to change.
	if res, err := d.SetNode(ctx, raw, "", false); err != nil || res.Changed {
		t.Errorf("the same again: %+v %v", res, err)
	}
}
