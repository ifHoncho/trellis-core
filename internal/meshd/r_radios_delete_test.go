package meshd

import (
	"fmt"
	"testing"

	"github.com/mdlayher/netlink"
	"golang.org/x/sys/unix"
)

func TestCannotDelete(t *testing.T) {
	wrap := func(e error) error {
		return fmt.Errorf("delete interface 3: %w", &netlink.OpError{Op: "receive", Err: e})
	}
	if !cannotDelete(wrap(unix.Errno(524))) {
		t.Error("brcmfmac's ENOTSUPP (524) means the interface cannot be deleted")
	}
	if !cannotDelete(wrap(unix.EOPNOTSUPP)) {
		t.Error("EOPNOTSUPP means the interface cannot be deleted")
	}
	for _, e := range []error{unix.ENODEV, unix.EBUSY, unix.EPERM} {
		if cannotDelete(wrap(e)) {
			t.Errorf("%v is not a driver refusal", e)
		}
	}
}
