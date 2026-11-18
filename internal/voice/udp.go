package voice

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// DSCP marks voice packets. EF (46) is voice's code point. batman-adv
// derives a frame's 802.11 priority from the top three bits of the TOS
// byte, which puts EF in the video access category; see architecture §13, Voice.
const DSCP = 46

// UDP is the multicast transport on one interface: a socket per address
// listened to, bound to that group address, and one socket to send from.
// Packets go out with TTL 1 (the mesh's client network is one layer 2),
// without loopback, marked with DSCP.
type UDP struct {
	ifname  string
	ifi     *net.Interface
	send    *net.UDPConn
	mu      sync.Mutex
	recv    map[netip.AddrPort]*net.UDPConn
	deliver func(to netip.AddrPort, b []byte)
	closed  bool
	wg      sync.WaitGroup
}

func control(ifname string, f func(s int) error) func(string, string, syscall.RawConn) error {
	return func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) { serr = f(int(fd)) }); err != nil {
			return err
		}
		return serr
	}
}

// OpenUDP opens the sending socket on ifname.
func OpenUDP(ifname string) (*UDP, error) {
	ifi, err := net.InterfaceByName(ifname)
	if err != nil {
		return nil, err
	}
	lc := net.ListenConfig{Control: control(ifname, func(s int) error {
		if err := unix.BindToDevice(s, ifname); err != nil {
			return fmt.Errorf("bind to %s: %w", ifname, err)
		}
		if err := unix.SetsockoptIPMreqn(s, unix.IPPROTO_IP, unix.IP_MULTICAST_IF, &unix.IPMreqn{Ifindex: int32(ifi.Index)}); err != nil {
			return err
		}
		if err := unix.SetsockoptInt(s, unix.IPPROTO_IP, unix.IP_MULTICAST_TTL, 1); err != nil {
			return err
		}
		if err := unix.SetsockoptInt(s, unix.IPPROTO_IP, unix.IP_MULTICAST_LOOP, 0); err != nil {
			return err
		}
		return unix.SetsockoptInt(s, unix.IPPROTO_IP, unix.IP_TOS, DSCP<<2)
	})}
	c, err := lc.ListenPacket(context.Background(), "udp4", "0.0.0.0:0")
	if err != nil {
		return nil, err
	}
	return &UDP{ifname: ifname, ifi: ifi, send: c.(*net.UDPConn)}, nil
}

// Send sends one packet to a group address.
func (u *UDP) Send(to netip.AddrPort, b []byte) error {
	_, err := u.send.WriteToUDPAddrPort(b, to)
	return err
}

// Listen joins each address on the interface and passes what arrives to
// deliver, until Close.
func (u *UDP) Listen(addrs []netip.AddrPort, deliver func(to netip.AddrPort, b []byte)) error {
	u.mu.Lock()
	u.deliver = deliver
	u.mu.Unlock()
	return u.Join(addrs)
}

// Join listens to more addresses, delivering to Listen's function; one
// already listened to is skipped.
func (u *UDP) Join(addrs []netip.AddrPort) error {
	u.mu.Lock()
	deliver := u.deliver
	if u.recv == nil {
		u.recv = map[netip.AddrPort]*net.UDPConn{}
	}
	closed := u.closed
	u.mu.Unlock()
	if closed || deliver == nil {
		return errors.New("voice: the transport is closed, or not listening yet")
	}
	for _, a := range addrs {
		u.mu.Lock()
		_, have := u.recv[a]
		u.mu.Unlock()
		if have {
			continue
		}
		g := a.Addr().As4()
		lc := net.ListenConfig{Control: control(u.ifname, func(s int) error {
			if err := unix.SetsockoptInt(s, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
				return err
			}
			if err := unix.BindToDevice(s, u.ifname); err != nil {
				return fmt.Errorf("bind to %s: %w", u.ifname, err)
			}
			if err := unix.SetsockoptInt(s, unix.IPPROTO_IP, unix.IP_MULTICAST_ALL, 0); err != nil {
				return err
			}
			mreq := &unix.IPMreqn{Ifindex: int32(u.ifi.Index), Multiaddr: g}
			if err := unix.SetsockoptIPMreqn(s, unix.IPPROTO_IP, unix.IP_ADD_MEMBERSHIP, mreq); err != nil {
				return fmt.Errorf("join %v on %s: %w", a.Addr(), u.ifname, err)
			}
			return nil
		})}
		// Bound to the group address: this socket gets that group's
		// packets only.
		c, err := lc.ListenPacket(context.Background(), "udp4", a.String())
		if err != nil {
			return err
		}
		conn := c.(*net.UDPConn)
		u.mu.Lock()
		if u.closed { // closed meanwhile: nobody would close this one
			u.mu.Unlock()
			conn.Close()
			return errors.New("voice: the transport is closed")
		}
		u.recv[a] = conn
		u.wg.Add(1)
		u.mu.Unlock()
		go func(a netip.AddrPort) {
			defer u.wg.Done()
			buf := make([]byte, 1500)
			for {
				n, _, err := conn.ReadFromUDPAddrPort(buf)
				if err != nil {
					if errors.Is(err, net.ErrClosed) {
						return
					}
					// An error that stays (the interface gone) must not
					// spin: the engine is restarted on a new interface.
					time.Sleep(100 * time.Millisecond)
					continue
				}
				deliver(a, buf[:n])
			}
		}(a)
	}
	return nil
}

// Leave stops listening to addresses (the kernel leaves the group with
// the socket).
func (u *UDP) Leave(addrs []netip.AddrPort) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, a := range addrs {
		if c := u.recv[a]; c != nil {
			c.Close()
			delete(u.recv, a)
		}
	}
}

// Close closes every socket and waits for the readers.
func (u *UDP) Close() error {
	u.mu.Lock()
	u.closed = true
	for _, c := range u.recv {
		c.Close()
	}
	u.recv = nil
	u.mu.Unlock()
	u.wg.Wait()
	return u.send.Close()
}
