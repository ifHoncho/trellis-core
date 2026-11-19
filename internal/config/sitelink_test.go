package config

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// testSiteKey is a well-formed public key, different for each n.
func testSiteKey(n int) string {
	h := sha256.Sum256([]byte(fmt.Sprint("site key ", n)))
	return base64.StdEncoding.EncodeToString(h[:])
}

func TestSiteLinkProfile(t *testing.T) {
	a, b, c := testSiteKey(1), testSiteKey(2), testSiteKey(3)
	p, err := ParseProfile([]byte(goodProfile + fmt.Sprintf(`
site_link:
  members:
    - {name: hq-1, site: hq, public_key: %q, endpoint: "hq.example.org:51873"}
    - {name: hq-2, site: hq, public_key: %q, endpoint: "[2001:db8::1]:4000"}
    - {name: field-1, site: field, public_key: %q}
`, a, b, c)))
	if err != nil {
		t.Fatal(err)
	}
	s := p.SiteLink
	if s.ListenPort() != 51873 || s.ThroughputMbit() != 10 || s.LinkMTU() != 1420 || s.MulticastBudgetKbit() != 1000 {
		t.Errorf("defaults %d %d %d", s.ListenPort(), s.ThroughputMbit(), s.LinkMTU())
	}
	names := func(ms []SiteMember) (out []string) {
		for _, m := range ms {
			out = append(out, m.Name)
		}
		return
	}
	if got := names(s.Peers(a)); !slices.Equal(got, []string{"field-1"}) {
		t.Errorf("hq-1's peers %v", got)
	}
	if got := names(s.Peers(c)); !slices.Equal(got, []string{"hq-1", "hq-2"}) {
		t.Errorf("field-1's peers %v", got)
	}
	if got := s.Peers(testSiteKey(9)); got != nil {
		t.Errorf("a non-member has peers %v", got)
	}
	ad := SiteAddr(a)
	if o := ad.As4(); o[0] != 169 || o[1] != 254 || o[2] == 0 || o[2] == 255 || SiteAddr(a) != ad || SiteAddr(b) == ad {
		t.Errorf("address %s", ad)
	}
	if n := SiteIfname(a); len(n) != 11 || !strings.HasPrefix(n, "site-") || n == SiteIfname(b) {
		t.Errorf("ifname %q", n)
	}
	// A profile without the section writes none.
	q, _ := ParseProfile([]byte(goodProfile))
	if out, err := q.YAML(""); err != nil || strings.Contains(string(out), "site_link") {
		t.Errorf("empty section written: %v\n%s", err, out)
	}
}

func TestSiteLinkInvalid(t *testing.T) {
	a, b := testSiteKey(1), testSiteKey(2)
	for bad, field := range map[string]string{
		`site_link: {port: 70000}`:                                       "site_link.port",
		`site_link: {mbit: -1}`:                                          "site_link.mbit",
		`site_link: {mtu: 1300}`:                                         "site_link.mtu",
		`site_link: {multicast_kbit: 8}`:                                 "site_link.multicast_kbit",
		`site_link: {members: [{site: "", public_key: "` + a + `"}]}`:    "site_link.members[0].site",
		`site_link: {members: [{site: "a b", public_key: "` + a + `"}]}`: "site_link.members[0].site",
		`site_link: {members: [{site: hq, public_key: "short"}]}`:        "site_link.members[0].public_key",
		`site_link: {members: [{site: hq, public_key: "` + a + `"}, {site: x, public_key: "` + a + `"}]}`: "site_link.members[1].public_key",
		`site_link: {members: [{site: hq, public_key: "` + a + `", endpoint: "nope"}]}`:                   "site_link.members[0].endpoint",
		`site_link: {members: [{site: hq, public_key: "` + a + `", endpoint: "h:0"}]}`:                    "site_link.members[0].endpoint",
		`site_link: {members: [{site: hq, public_key: "` + a + `", endpoint: "bad_host:1"}]}`:             "site_link.members[0].endpoint",
		`site_link: {members: [{site: hq, name: "a\"b", public_key: "` + b + `"}]}`:                       "site_link.members[0].name",
	} {
		_, err := ParseProfile([]byte(goodProfile + bad + "\n"))
		if !slices.Contains(fieldsOf(err), field) {
			t.Errorf("%s: want an error on %s, got %v", bad, field, err)
		}
	}
	// A key in another base64 spelling of the same bytes (padding bits
	// set) is refused: keys are compared as text.
	odd := []byte(a)
	odd[42] = odd[42] + 1
	if _, err := ParseProfile([]byte(goodProfile + "site_link: {members: [{site: hq, public_key: \"" + string(odd) + "\"}]}\n")); !slices.Contains(fieldsOf(err), "site_link.members[0].public_key") {
		t.Errorf("non-canonical key accepted: %s %v", odd, err)
	}
	// A bad key does not hide a bad endpoint in the same entry.
	if _, err := ParseProfile([]byte(goodProfile + "site_link: {members: [{site: hq, public_key: x, endpoint: nope}]}\n")); !slices.Contains(fieldsOf(err), "site_link.members[0].endpoint") {
		t.Errorf("endpoint not checked: %v", err)
	}
	for _, ep := range []string{"203.0.113.5:51873", "vpn.example.org:1", "[2001:db8::7]:443", "100.101.102.103:41641"} {
		if err := checkEndpoint(ep); err != nil {
			t.Errorf("%s: %v", ep, err)
		}
	}
}

// Two keys whose tunnel addresses collide make the profile invalid.
func TestSiteLinkAddressCollision(t *testing.T) {
	seen := map[string]string{}
	for n := 0; n < 100000; n++ {
		k := testSiteKey(n)
		a := SiteAddr(k).String()
		if o, ok := seen[a]; ok {
			_, err := ParseProfile([]byte(goodProfile + fmt.Sprintf("site_link: {members: [{site: a, public_key: %q}, {site: b, public_key: %q}]}\n", o, k)))
			if !slices.Contains(fieldsOf(err), "site_link.members[1].public_key") || !strings.Contains(err.Error(), "tunnel address") {
				t.Fatalf("collision of %s not refused: %v", a, err)
			}
			return
		}
		seen[a] = k
	}
	t.Fatal("no collision found")
}

// "HQ" and "hq" would be two sites whose members link to each other: a
// slip, refused.
func TestSiteLinkSiteCase(t *testing.T) {
	_, err := ParseProfile([]byte(goodProfile + fmt.Sprintf(`
site_link:
  members:
    - {site: hq, public_key: %q}
    - {site: HQ, public_key: %q}
    - {site: field, public_key: %q, endpoint: "f.example:1"}
`, testSiteKey(1), testSiteKey(2), testSiteKey(3))))
	if err == nil || !strings.Contains(err.Error(), "site_link.members[1].site") || !strings.Contains(err.Error(), "differ only in case") {
		t.Fatalf("got %v", err)
	}
}

// The digest follows what the profile says, and leaves the secret out.
func TestProfileDigest(t *testing.T) {
	p, err := ParseProfile([]byte(goodProfile))
	if err != nil {
		t.Fatal(err)
	}
	base := p.Digest()
	q := *p
	q.SiteLink.Members = []SiteMember{{Site: "hq", PublicKey: testSiteKey(1)}}
	r := *p
	r.SAEPassphrase = "another-secret-entirely"
	if len(base) != 12 || q.Digest() == base || r.Digest() != base || p.Digest() != base {
		t.Fatalf("digests %q %q %q", base, q.Digest(), r.Digest())
	}
}

// A tunnel's address comes from the two keys: the same on every node,
// another for each direction and each peer, and a locally administered
// unicast address.
func TestSiteTunnelMAC(t *testing.T) {
	a, b, c := testSiteKey(1), testSiteKey(2), testSiteKey(3)
	ab := SiteTunnelMAC(a, b)
	if ab.String() != SiteTunnelMAC(a, b).String() {
		t.Error("not stable")
	}
	if ab[0]&0x03 != 0x02 || len(ab) != 6 {
		t.Errorf("%s is not a locally administered unicast address", ab)
	}
	seen := map[string]bool{}
	for _, m := range []string{ab.String(), SiteTunnelMAC(b, a).String(), SiteTunnelMAC(a, c).String(), SiteTunnelMAC(c, a).String()} {
		if seen[m] {
			t.Errorf("%s twice", m)
		}
		seen[m] = true
	}
	plan := SiteLinkPlan{Members: []SiteMember{{Site: "hq", PublicKey: a}, {Site: "hq", PublicKey: b}, {Site: "field", PublicKey: c}}}
	if got := plan.TunnelMACs(a); len(got) != 1 || got[0] != SiteTunnelMAC(a, c).String() {
		t.Errorf("a's tunnels: %v", got)
	}
	if got := plan.TunnelMACs(c); len(got) != 2 {
		t.Errorf("c's tunnels: %v", got)
	}
	if got := plan.TunnelMACs(testSiteKey(9)); got != nil {
		t.Errorf("no member: %v", got)
	}
}

// An address with a zone means something on one node only.
func TestSiteLinkEndpointZone(t *testing.T) {
	_, err := ParseProfile([]byte(goodProfile + fmt.Sprintf("site_link: {members: [{site: a, public_key: %q, endpoint: \"[fe80::1%%eth0]:51873\"}]}\n", testSiteKey(1))))
	if !slices.Contains(fieldsOf(err), "site_link.members[0].endpoint") || !strings.Contains(err.Error(), "zone") {
		t.Errorf("zoned endpoint not refused: %v", err)
	}
}
