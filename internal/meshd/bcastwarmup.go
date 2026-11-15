package meshd

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Broadcast sequence numbers after bat0 is recreated.
//
// batman-adv numbers the broadcasts a node floods, and every other node
// drops a number it has already seen within a window of 64. A new bat0
// starts again at 1. Its broadcasts are then dropped until its numbers
// pass the highest the others saw from the old one, unless that is 64 or
// more ahead (then the first new one resets the window, at most once per
// 30 s). A node whose previous bat0 sent between a dozen and 64 broadcasts
// (a restart soon after another, as an upgrade or its rollback does) is
// deaf to ARP and every other broadcast until it has sent as many again,
// which on a quiet node takes minutes.
//
// So meshd sends warmupCount small broadcasts on each new bat0 as soon as
// batman-adv has a neighbour (a link that has peered: before that, frames
// reach nobody). Afterwards its numbers are past any old count
// below 65, and for a higher one the first of them resets the window; and
// since every bat0 now sends at least that many, the next restart always
// finds the others' window reset at once. The frames carry a local
// experimental ethertype and every node's bridge filter drops them as they
// come in from the mesh (nft RuleWarmup), so they reach no device.
//
// Every node floods each broadcast, so the warm-up costs as much airtime as
// 64 ARP requests. Over a HaLow link, sent at a few hundred kbit/s, that is
// seconds of the channel: there the frames are spread out, at the same
// total cost, so they never hold it for long at a time.
const (
	warmupEtherType = 0x88b5 // IEEE 802 local experimental ethertype 1
	warmupCount     = 64     // batman-adv's broadcast window (BATADV_BCAST_MAX_AGE)
	warmupGap       = 50 * time.Millisecond
	warmupGapHaLow  = 250 * time.Millisecond
	// warmupRetry is the wait before trying again after a failed warm-up.
	warmupRetry = 10 * time.Second
	// warmupLimit bounds a warm-up that batman-adv keeps holding back.
	warmupLimit = 2 * time.Minute
	// warmupHeldBack is the pause after a frame batman-adv dropped.
	warmupHeldBack = time.Second
)

// warmupFrame is one warm-up frame: broadcast, from src, numbered n.
func warmupFrame(src net.HardwareAddr, n int) []byte {
	f := make([]byte, 60) // the Ethernet minimum, without the checksum
	for i := 0; i < 6; i++ {
		f[i] = 0xff
	}
	copy(f[6:12], src)
	binary.BigEndian.PutUint16(f[12:14], warmupEtherType)
	copy(f[14:], "trellis broadcast warm-up")
	binary.BigEndian.PutUint16(f[40:42], uint16(n))
	return f
}

// sendWarmup sends the warm-up broadcasts on the interface, gap apart,
// until warmupCount of them were numbered by batman-adv. A frame it drops
// before numbering it (seen right after a start on radio links) shows in
// the interface's transmit-dropped counter and is not counted: the numbers
// did not move. It stops when ctx ends, the interface goes away, or
// warmupLimit passes; it returns the frames numbered and those held back.
func sendWarmup(ctx context.Context, ifindex int, ifname string, src net.HardwareAddr, gap time.Duration) (numbered, held int, err error) {
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return 0, 0, fmt.Errorf("packet socket: %w", err)
	}
	defer unix.Close(fd)
	sa := &unix.SockaddrLinklayer{Ifindex: ifindex, Halen: 6, Addr: [8]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}}
	stat := func(name string) uint64 {
		b, err := os.ReadFile("/sys/class/net/" + ifname + "/statistics/" + name)
		if err != nil {
			return 0
		}
		v, _ := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
		return v
	}
	deadline := time.Now().Add(warmupLimit)
	for n := 0; numbered < warmupCount; n++ {
		if time.Now().After(deadline) {
			return numbered, held, fmt.Errorf("batman-adv numbered %d of %d in %v (%d held back)", numbered, warmupCount, warmupLimit, held)
		}
		// bat0 transmits synchronously: a frame batman-adv sent raises
		// tx_packets before Sendto returns, one it dropped tx_dropped.
		// Other traffic may move either; a frame counts as held back only
		// when nothing was sent and something was dropped.
		dropBefore, sentBefore := stat("tx_dropped"), stat("tx_packets")
		if err := unix.Sendto(fd, warmupFrame(src, n), 0, sa); err != nil {
			return numbered, held, err
		}
		wait := gap
		if stat("tx_dropped") > dropBefore && stat("tx_packets") == sentBefore {
			held++
			wait = warmupHeldBack
		} else {
			numbered++
		}
		select {
		case <-ctx.Done():
			return numbered, held, ctx.Err()
		case <-time.After(wait):
		}
	}
	return numbered, held, nil
}
