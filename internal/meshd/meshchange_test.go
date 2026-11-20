package meshd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"meridian/trellis/internal/api"
	"meridian/trellis/internal/config"
	"meridian/trellis/internal/gossip"
	"meridian/trellis/internal/nodestate"
)

func meshChangeDaemon(t *testing.T) (*Daemon, string, []byte) {
	t.Helper()
	d, dir := testDaemon(t, map[string]string{"node.yaml": "schema: 0\ncallsign: node-1\n", "profile.yaml": testProfile})
	d.ident = &nodestate.Identity{MAC: "02:00:00:00:00:01", NodeID: 1}
	d.peers = gossip.NewTable(d.ident.MAC)
	key, _ := gossip.Key("test-passphrase-not-secret", "trellis-alpha")
	return d, dir, key
}

// profileAt is the test profile with the first link at another width and a
// revision.
func profileAt(t *testing.T, width int, rev uint64, mutate func(*config.Profile)) string {
	t.Helper()
	p, err := config.ParseProfile([]byte(testProfile))
	if err != nil {
		t.Fatal(err)
	}
	p.Radio.Width, p.Revision = width, rev
	if mutate != nil {
		mutate(p)
	}
	data, err := p.YAML("")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func offer(t *testing.T, key []byte, o gossip.Offer) []byte {
	t.Helper()
	o.V, o.MAC, o.Callsign = 1, "02:00:00:00:00:02", "picker"
	pkt, err := gossip.SealOffer(key, &o)
	if err != nil {
		t.Fatal(err)
	}
	return pkt
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// An offer is kept, and switched to at its moment; the file then has the
// new profile and its revision.
func TestMeshChangeOffer(t *testing.T) {
	d, dir, key := meshChangeDaemon(t)
	from := &net.UDPAddr{IP: net.ParseIP("10.73.2.1"), Port: gossip.Port}
	at := time.Now().Add(300 * time.Millisecond)
	if err := d.handleProfileOffer(key, offer(t, key, gossip.Offer{Rev: 3, ApplyAtMS: at.UnixMilli(), Profile: profileAt(t, 40, 3, nil)}), from); err != nil {
		t.Fatal(err)
	}
	if c := d.meshChangeState(); c == nil || c.State != "pending" || c.Rev != 3 || c.From != "picker" {
		t.Fatalf("pending change: %+v", c)
	}
	if _, err := os.Stat(filepath.Join(dir, "state", meshChangeFile)); err != nil {
		t.Errorf("the pending change is not kept on disk: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "profile.yaml"))
	if strings.Contains(string(data), "revision") {
		t.Fatal("switched before the moment")
	}
	waitFor(t, "the switch", func() bool { c := d.meshChangeState(); return c != nil && c.State != "pending" })
	if c := d.meshChangeState(); c.State != "applied" {
		t.Fatalf("after the switch: %+v", c)
	}
	p, err := config.LoadProfile(filepath.Join(dir, "profile.yaml"))
	if err != nil || p.Revision != 3 || p.Radio.Width != 40 {
		t.Fatalf("installed profile: %+v %v", p, err)
	}
	if st := d.meshChangeStatus(); st == nil || st.Revision != 3 || !strings.Contains(st.Summary, "at 40 MHz") {
		t.Errorf("status: %+v", st)
	}
	if _, err := os.Stat(filepath.Join(dir, "state", meshChangeFile)); err == nil {
		t.Error("the change is still kept after it is in force")
	}

	// Refused: another secret (a re-key), an older revision. Ignored:
	// another key altogether.
	d.pending = nil // as if the reload had been applied
	d.profile = p
	other := profileAt(t, 20, 5, func(p *config.Profile) { p.SAEPassphrase = "another-secret-1234" })
	_ = d.handleProfileOffer(key, offer(t, key, gossip.Offer{Rev: 5, Profile: other}), from)
	_ = d.handleProfileOffer(key, offer(t, key, gossip.Offer{Rev: 2, Profile: profileAt(t, 20, 2, nil)}), from)
	if c := d.meshChangeState(); c.Rev != 3 {
		t.Errorf("a refused offer replaced the change: %+v", c)
	}
	wrong, _ := gossip.Key("another-secret-1234", "trellis-alpha")
	if err := d.handleProfileOffer(key, offer(t, wrong, gossip.Offer{Rev: 9, Profile: profileAt(t, 20, 9, nil)}), from); !errors.Is(err, gossip.ErrAuth) {
		t.Errorf("an offer under another key: %v", err)
	}
}

// A change accepted before a restart is still due after it, at its moment
// when the clock can be believed; at once when it cannot: when it is not
// synchronised (a node without an RTC starts from a stale time and would
// otherwise wait on the old settings, cut off from the mesh that would set
// its clock), or when the switch seems further away than it was when the
// change arrived (the clock went back).
func TestMeshChangeSurvivesRestart(t *testing.T) {
	synced := clockSynced
	t.Cleanup(func() { clockSynced = synced })
	for _, c := range []struct {
		name   string
		sync   bool
		edit   func(m map[string]any, at time.Time) // the kept file, as a restart finds it
		atOnce bool
	}{
		{"synchronised clock", true, nil, false},
		{"clock not synchronised", false, nil, true},
		{"clock a day behind, though synchronised", true, func(m map[string]any, at time.Time) {
			m["apply_at"] = at.Add(24 * time.Hour).Format(time.RFC3339Nano)
		}, true},
		{"clock a little behind", true, func(m map[string]any, at time.Time) {
			m["apply_at"] = at.Add(20 * time.Second).Format(time.RFC3339Nano)
		}, false},
		{"kept by an older version, further than any delay", true, func(m map[string]any, at time.Time) {
			delete(m, "wait_ms")
			m["apply_at"] = time.Now().Add(meshChangeMax + 5*time.Minute).Format(time.RFC3339Nano)
		}, true},
		{"kept by an older version, within the longest delay", true, func(m map[string]any, at time.Time) {
			delete(m, "wait_ms")
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			clockSynced = func() bool { return c.sync }
			d, dir, key := meshChangeDaemon(t)
			from := &net.UDPAddr{IP: net.ParseIP("10.73.2.1")}
			at := time.Now().Add(10 * time.Minute)
			_ = d.handleProfileOffer(key, offer(t, key, gossip.Offer{Rev: 4, ApplyAtMS: at.UnixMilli(), Profile: profileAt(t, 40, 4, nil)}), from)
			d.meshChanges.timer.Stop()
			kept := filepath.Join(dir, "state", meshChangeFile)
			if c.edit != nil {
				var m map[string]any
				b, err := os.ReadFile(kept)
				if err != nil || json.Unmarshal(b, &m) != nil {
					t.Fatalf("kept change: %s %v", b, err)
				}
				if w, _ := m["wait_ms"].(float64); w < float64((9 * time.Minute).Milliseconds()) {
					t.Fatalf("the wait was not kept: %v", m["wait_ms"])
				}
				c.edit(m, at)
				b, _ = json.Marshal(m)
				if err := os.WriteFile(kept, b, 0o600); err != nil {
					t.Fatal(err)
				}
			}

			d2, err := New(Options{NodePath: filepath.Join(dir, "node.yaml"), ProfilePath: filepath.Join(dir, "profile.yaml"),
				StateDir: filepath.Join(dir, "state"), RunDir: filepath.Join(dir, "run"), SysRoot: filepath.Join(dir, "sys"), Log: d.log, Version: "test"})
			if err != nil {
				t.Fatal(err)
			}
			d2.loadMeshChange()
			if !c.atOnce {
				if m := d2.meshChangeState(); m == nil || m.Rev != 4 || m.State != "pending" || time.Until(m.ApplyAt) < 5*time.Minute {
					t.Fatalf("after a restart: %+v", m)
				}
				d2.meshChanges.timer.Stop()
				return
			}
			waitFor(t, "the switch at once", func() bool { m := d2.meshChangeState(); return m != nil && m.State == "applied" })
			p, err := config.LoadProfile(filepath.Join(dir, "profile.yaml"))
			if err != nil || p.Revision != 4 || p.Radio.Width != 40 {
				t.Fatalf("installed profile: %+v %v", p, err)
			}
		})
	}
}

// The sending side: what a dry run says, and what is refused.
func TestSetMeshProfile(t *testing.T) {
	d, _, _ := meshChangeDaemon(t)
	d.time = newTimeReconciler(d)
	d.peers.Update(&gossip.Record{V: 1, MAC: "02:00:00:00:00:02", ID: 2, Callsign: "node-2", Addr: "10.73.2.1", Boot: 1, Seq: 1, MeshChange: true, Regulatory: "unrestricted"}, "10.73.2.1")
	d.peers.Update(&gossip.Record{V: 1, MAC: "02:00:00:00:00:03", ID: 3, Callsign: "old-3", Addr: "10.73.3.1", Boot: 1, Seq: 1}, "10.73.3.1")
	ctx := context.Background()
	in := api.MeshProfileInput{ProfileInput: api.ProfileInput{YAML: profileAt(t, 40, 0, nil)}}
	res, err := d.SetMeshProfile(ctx, in, true)
	if err != nil || !res.Changed || res.Applied || res.ApplyAt == nil || len(res.Nodes) != 1 || res.Nodes[0] != "node-2" {
		t.Fatalf("dry run: %+v %v", res, err)
	}
	if !hasEffect(res.Effects, "info", "mesh-change") || !hasEffect(res.Effects, "warning", "mesh-change-old") || hasEffect(res.Effects, "warning", "this-node-only") {
		t.Errorf("effects: %+v", res.Effects)
	}
	// Every node switches together: nothing drops for good, the changed
	// link restarts.
	if hasEffect(res.Effects, "error", "leaves-mesh") || hasEffect(res.Effects, "warning", "links-drop") || !strings.Contains(effectText(res.Effects, "mesh-change-restart"), "the 5 GHz · ch 149 link") {
		t.Errorf("mesh-wide framing: %+v", res.Effects)
	}
	// node-2 runs unrestricted; the profile sent is in country mode.
	if msg := effectText(res.Effects, "mesh-change-regulatory"); !strings.Contains(msg, "node-2: unrestricted → country mode (US)") {
		t.Errorf("regulatory change on another node: %q", msg)
	}
	if until := time.Until(*res.ApplyAt); until < 55*time.Second || until > 61*time.Second {
		t.Errorf("default switch in %v", until)
	}
	rekey := api.MeshProfileInput{ProfileInput: api.ProfileInput{YAML: profileAt(t, 20, 0, func(p *config.Profile) { p.SAEPassphrase = "another-secret-1234" })}}
	if _, err := d.SetMeshProfile(ctx, rekey, true); err == nil || !strings.Contains(err.Error(), "re-key") {
		t.Errorf("a secret change: %v", err)
	}
	in.DelaySeconds = 5
	if _, err := d.SetMeshProfile(ctx, in, true); err == nil {
		t.Error("a switch 5 s away was accepted")
	}
}

// A profile with a site link is refused by releases before 0.3.0: the
// review names them.
func TestSetMeshProfileSiteLinkOld(t *testing.T) {
	d, _, _ := meshChangeDaemon(t)
	d.time = newTimeReconciler(d)
	d.peers.Update(&gossip.Record{V: 1, MAC: "02:00:00:00:00:02", ID: 2, Callsign: "node-2", Addr: "10.73.2.1", Boot: 1, Seq: 1, MeshChange: true, Version: "0.2.1"}, "10.73.2.1")
	d.peers.Update(&gossip.Record{V: 1, MAC: "02:00:00:00:00:03", ID: 3, Callsign: "node-3", Addr: "10.73.3.1", Boot: 1, Seq: 1, MeshChange: true, Version: "0.3.0"}, "10.73.3.1")
	key := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	in := api.MeshProfileInput{ProfileInput: api.ProfileInput{YAML: profileAt(t, 40, 0, func(p *config.Profile) {
		p.SiteLink.Members = []config.SiteMember{{Site: "hq", PublicKey: key}}
	})}}
	res, err := d.SetMeshProfile(context.Background(), in, true)
	if err != nil {
		t.Fatal(err)
	}
	if msg := effectText(res.Effects, "mesh-change-voice-old"); !strings.Contains(msg, "node-2: release before 0.3.0") || !strings.Contains(msg, "a site link") || strings.Contains(msg, "node-3") {
		t.Errorf("old releases: %q", msg)
	}
	plain := api.MeshProfileInput{ProfileInput: api.ProfileInput{YAML: profileAt(t, 40, 0, nil)}}
	if res, _ := d.SetMeshProfile(context.Background(), plain, true); hasEffect(res.Effects, "warning", "mesh-change-voice-old") {
		t.Error("warned without voice or a site link")
	}
}

// A profile too large for one offer reaches no other node: refused at the
// review, with the field to shorten, rather than "sent" and lost.
func TestProfileFits(t *testing.T) {
	d, _, _ := meshChangeDaemon(t)
	d.time = newTimeReconciler(d)
	ctx := context.Background()
	members := func(n int) func(*config.Profile) {
		return func(p *config.Profile) {
			// Keys whose tunnel addresses and interface names differ: two
			// random keys now and then share one, which validation
			// refuses (and this test failed on, a few runs in a hundred).
			used := map[string]bool{}
			for i := 0; i < n; i++ {
				_, pub := siteTestKey(t)
				for used[config.SiteAddr(pub).String()] || used[config.SiteIfname(pub)] {
					_, pub = siteTestKey(t)
				}
				used[config.SiteAddr(pub).String()], used[config.SiteIfname(pub)] = true, true
				p.SiteLink.Members = append(p.SiteLink.Members, config.SiteMember{
					Name: fmt.Sprintf("relay-station-%02d", i), Site: fmt.Sprintf("site-%02d", i%32), PublicKey: pub,
					Endpoint: fmt.Sprintf("relay-station-%02d.example.org:51873", i)})
			}
		}
	}
	// A list of ordinary size passes, and is sealed for real.
	res, err := d.SetMeshProfile(ctx, api.MeshProfileInput{ProfileInput: api.ProfileInput{YAML: profileAt(t, 40, 0, members(12))}}, true)
	if err != nil || !res.Changed {
		t.Fatalf("12 members: %+v %v", res, err)
	}
	// The limit validation allows does not fit: said at the dry run, on
	// the members.
	big := profileAt(t, 40, 0, members(config.SiteLinkMaxMembers))
	for name, try := range map[string]func() error{
		"every node": func() error {
			_, err := d.SetMeshProfile(ctx, api.MeshProfileInput{ProfileInput: api.ProfileInput{YAML: big}}, true)
			return err
		},
		"this node": func() error {
			_, err := d.SetProfile(ctx, api.ProfileInput{YAML: big}, true)
			return err
		},
	} {
		err := try()
		var es config.Errors
		if !errors.As(err, &es) || len(es) != 1 || es[0].Field != "site_link.members" || !strings.Contains(err.Error(), "too large to pass from node to node") {
			t.Errorf("%s: 64 members: %v", name, err)
		}
	}
	// What passes the check seals with the longest change record.
	p, err := config.ParseProfile([]byte(profileAt(t, 40, 0, members(30))))
	if err != nil {
		t.Fatal(err)
	}
	if err := profileFits(p); err != nil {
		t.Fatalf("30 members: %v", err)
	}
}

// A change that has just switched is in the file, and accepted, a pass
// before it is in force. A change sent in that moment follows it: built on
// the profile still in force, it would not count the change just made
// among its earlier ones, and be taken for one made apart (lab P4 found
// it: a cancel sent right after a switch became a conflict).
func TestLatestProfileAfterSwitch(t *testing.T) {
	d, _, key := meshChangeDaemon(t)
	from := &net.UDPAddr{IP: net.ParseIP("10.73.2.1"), Port: gossip.Port}
	id := config.NewChangeID()
	at := time.Now().Add(100 * time.Millisecond)
	switched := profileAt(t, 40, 3, func(p *config.Profile) {
		p.Change = &config.ChangeInfo{ID: id, At: at.UTC().Truncate(time.Second), By: "picker"}
	})
	if err := d.handleProfileOffer(key, offer(t, key, gossip.Offer{Rev: 3, ApplyAtMS: at.UnixMilli(), Profile: switched}), from); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the switch", func() bool { c := d.meshChangeState(); return c != nil && c.State == "applied" })
	if _, inForce := d.conf(); inForce.Revision == 3 {
		t.Skip("the change is in force already: nothing between the file and the pass to test")
	}
	latest := d.latestProfile()
	if latest.Revision != 3 || latest.ChangeID() != id {
		t.Fatalf("the newest profile after a switch: revision %d, change %q; want 3, %q", latest.Revision, latest.ChangeID(), id)
	}
	next := *latest
	next.StartChange(d.latestProfile(), time.Now().Add(time.Minute), "relay-1")
	if !next.Knows(id) {
		t.Error("a change made right after the switch does not count that change among its earlier ones")
	}
}
