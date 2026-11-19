package meshd

import (
	"strings"
	"testing"
)

// With no ports listed, the files are exactly what earlier builds wrote, so
// an upgrade does not make NetworkManager or networkd reload.
func TestUnmanagedConfStable(t *testing.T) {
	const nm = "# Written by trellis meshd. Interfaces owned by Trellis must not be managed\n# by NetworkManager.\n[keyfile]\n" +
		"unmanaged-devices=interface-name:mesh0;interface-name:mesh1;interface-name:ap0;interface-name:bat0;interface-name:br-mesh;interface-name:anycast0\n"
	const nd = "# Written by trellis meshd. Interfaces owned by Trellis must not be managed\n# by systemd-networkd.\n[Match]\n" +
		"Name=mesh0 mesh1 ap0 bat0 br-mesh anycast0\n\n[Link]\nUnmanaged=yes\n"
	if got := nmConf(1, false, nil); got != nm {
		t.Errorf("nmConf(1, false, nil) changed:\n%s", got)
	}
	if got := networkdConf(1, false, nil); got != nd {
		t.Errorf("networkdConf(1, false, nil) changed:\n%s", got)
	}
	if got := nmConf(2, false, []string{"eth1"}); !strings.Contains(got, ";interface-name:eth1\n") {
		t.Errorf("listed port missing:\n%s", got)
	}
	if got := networkdConf(2, false, []string{"eth1"}); !strings.Contains(got, "anycast0 eth1\n") {
		t.Errorf("listed port missing:\n%s", got)
	}
	// A third and fourth mesh link are Trellis's too: NetworkManager must not
	// take mesh2 or mesh3 for its own.
	if got := nmConf(4, false, nil); !strings.Contains(got, ";interface-name:mesh2;interface-name:mesh3\n") {
		t.Errorf("further mesh links missing:\n%s", got)
	}
	if got := networkdConf(4, false, []string{"eth1"}); !strings.Contains(got, "anycast0 mesh2 mesh3 eth1\n") {
		t.Errorf("further mesh links missing:\n%s", got)
	}
	// A site link member: site0 and its tunnels.
	if got := nmConf(1, true, nil); !strings.Contains(got, ";interface-name:site0;interface-name:site-*\n") {
		t.Errorf("site link missing:\n%s", got)
	}
	if got := networkdConf(1, true, nil); !strings.Contains(got, "anycast0 site0 site-*\n") {
		t.Errorf("site link missing:\n%s", got)
	}
}
