package rtnl

import (
	"net"
	"testing"

	"github.com/vishvananda/netlink"
)

func TestRuleKeyDestination(t *testing.T) {
	_, a, _ := net.ParseCIDR("10.73.0.0/16")
	_, b, _ := net.ParseCIDR("10.74.0.0/16")
	r := *netlink.NewRule()
	r.Priority, r.IifName, r.Table = 7301, "br-mesh", 254
	plain := RuleKey(r)
	r.Dst = a
	toA := RuleKey(r)
	r.Dst = b
	toB := RuleKey(r)
	if plain == toA || toA == toB {
		t.Errorf("rule keys must differ by destination: %q %q %q", plain, toA, toB)
	}
}
