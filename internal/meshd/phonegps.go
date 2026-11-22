package meshd

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"meridian/trellis/internal/cot"
	"meridian/trellis/internal/gnss"
	"meridian/trellis/internal/status"
)

// The node and its own devices:
//   - a node with a GNSS fix is its devices' GPS: NMEA sentences once a
//     second to ATAK's external GPS port on every device that runs ATAK
//     on this node's AP or client ports;
//   - a node without a fix takes its position from one of those devices'
//     own position reports (SA), so it is still on the map.
//
// A device is one heard sending its own position report on one of this
// node's own ports (not behind bat0) in the last deviceFresh. Three rules
// on top, each with a reason shown in status when it holds a device back:
//   - a source reporting more than one unit is a gateway passing reports
//     on, not a device: not fed, not a position;
//   - only a GPS fix is a position (CoT how "m-g"): ATAK without a fix
//     reports its last or a hand-set point with another how;
//   - a device this node fed its own fix in the last fedHold is not a
//     position: ATAK holds the external fix for a while after it stops.

const (
	nmeaPort  = 4349        // ATAK's external GPS input
	nmeaEvery = time.Second // a receiver's usual rate
	// deviceFresh: a device not heard this long is forgotten. ATAK reports
	// every 30 s at its slowest default; a device that left for another
	// node is heard behind bat0, which does not count.
	deviceFresh = 2 * time.Minute
	fedHold     = 2 * time.Minute // a device fed this node's fix is no position source this long
	maxDevices  = 64              // devices remembered, a bound on the memory
	maxUnits    = 4               // units remembered per source: two make a gateway
	portsEvery  = 5 * time.Second // how often the listener reads this node's ports again
)

// phoneGPS is the state of both directions.
type phoneGPS struct {
	mu       sync.Mutex
	started  bool
	devs     map[string]*device // by IP
	cur      string             // the device (IP) whose position is in use
	nmeaSent uint64
	nmeaTo   int
	nmeaHeld string // why nothing goes out, if not for want of devices
	note     string // why no device's position is used, when one is heard
	saErr    string
}

// device is one source of position reports on this node's own ports.
type device struct {
	ip, mac string
	p       cot.Point            // its last report
	at      time.Time            // when that came
	units   map[string]time.Time // the units (UIDs) it reported, and when last
	fedAt   time.Time            // when this node last sent it NMEA
}

// gateway reports whether the source passes on other units' reports: more
// than one unit heard from it within deviceFresh.
func (v *device) gateway(now time.Time) bool {
	n := 0
	for _, at := range v.units {
		if now.Sub(at) <= deviceFresh {
			n++
		}
	}
	return n > 1
}

// held says why the device's position is not this node's, "" when it is.
func (v *device) held(now time.Time) string {
	switch {
	case v.gateway(now):
		return "reports several units (a gateway)"
	case !strings.HasPrefix(v.p.How, "m-g"):
		return "no GPS fix of its own"
	case now.Sub(v.fedAt) < fedHold:
		return "fed this node's own fix until lately"
	}
	return ""
}

// startPhoneGPS starts the sender and the SA listener once.
func (d *Daemon) startPhoneGPS() {
	s := &d.phone
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || d.ctx == nil {
		return
	}
	s.started = true
	go d.nmeaLoop(d.ctx)
	go d.saLoop(d.ctx)
}

// prune forgets devices not heard for deviceFresh. The caller holds s.mu.
func (s *phoneGPS) prune(now time.Time) {
	for ip, v := range s.devs {
		if now.Sub(v.at) > deviceFresh {
			delete(s.devs, ip)
		}
	}
}

// noteDeviceSA takes one SA datagram that arrived on one of this node's
// own ports, from the device at ip and mac. Only the sender's report of
// its own position counts: not an item it shares from its map, nor a
// Trellis node's report.
func (d *Daemon) noteDeviceSA(ip, mac string, b []byte, now time.Time) {
	p, err := cot.ParseSA(b)
	if err != nil || !p.Self || !strings.HasPrefix(p.Type, "a-") || strings.HasPrefix(p.UID, "trellis-node-") {
		return
	}
	mac = strings.ToLower(mac)
	s := &d.phone
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(now)
	v := s.devs[ip]
	if v == nil || v.mac != mac { // new, or the address went to another device
		if v == nil && len(s.devs) >= maxDevices {
			return
		}
		if s.devs == nil {
			s.devs = map[string]*device{}
		}
		v = &device{ip: ip, mac: mac, units: map[string]time.Time{}}
		s.devs[ip] = v
	}
	for uid, at := range v.units {
		if now.Sub(at) > deviceFresh {
			delete(v.units, uid) // a unit gone quiet makes room
		}
	}
	if _, known := v.units[p.UID]; known || len(v.units) < maxUnits {
		v.units[p.UID] = now
	}
	v.p, v.at = p, now
}

// nmeaLoop sends the node's fix to its devices once a second.
func (d *Daemon) nmeaLoop(ctx context.Context) {
	var conn *net.UDPConn
	defer func() {
		if conn != nil {
			conn.Close()
		}
	}()
	t := time.NewTicker(nmeaEvery)
	defer t.Stop()
	s := &d.phone
	set := func(to int, held string) {
		s.mu.Lock()
		s.nmeaTo, s.nmeaHeld = to, held
		s.mu.Unlock()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now()
		fix, ok := ownFix(d.gnss, now)
		switch {
		case positionsOffIn(d.conf()) != "":
			set(0, "position sharing off")
			continue
		case !ok:
			set(0, "no current GNSS fix")
			continue
		}
		msg := gnss.NMEA(fix)
		s.mu.Lock()
		s.prune(now)
		var to []string
		for ip, v := range s.devs {
			if !v.gateway(now) {
				to = append(to, ip)
			}
		}
		s.mu.Unlock()
		if len(to) == 0 || msg == nil {
			set(0, "")
			continue
		}
		if conn == nil {
			c, err := bridgeConn()
			if err != nil {
				set(0, "cannot send: "+err.Error())
				continue
			}
			conn = c
		}
		var sent []string
		var sendErr error
		for _, ip := range to {
			if _, err := conn.WriteToUDP(msg, &net.UDPAddr{IP: net.ParseIP(ip), Port: nmeaPort}); err != nil {
				sendErr = err
				// The socket's own fault (the bridge made again): a fresh
				// one next second. One device's fault skips that device.
				if errors.Is(err, syscall.ENODEV) || errors.Is(err, syscall.EBADF) || errors.Is(err, net.ErrClosed) {
					conn.Close()
					conn = nil
					break
				}
				continue
			}
			sent = append(sent, ip)
		}
		s.mu.Lock()
		s.nmeaSent += uint64(len(sent))
		s.nmeaTo, s.nmeaHeld = len(sent), ""
		if len(sent) == 0 && sendErr != nil {
			s.nmeaHeld = "cannot send: " + sendErr.Error()
		}
		for _, ip := range sent {
			if v := s.devs[ip]; v != nil {
				v.fedAt = now
			}
		}
		s.mu.Unlock()
	}
}

// positionFix is the position this node shares: its own receiver's fix,
// else a device's. from is "" for its own, or the device's IP; how is
// then the device's CoT how.
func (d *Daemon) positionFix(now time.Time) (fix gnss.State, from, how string, ok bool) {
	s := &d.phone
	s.mu.Lock()
	defer s.mu.Unlock()
	if f, ok := ownFix(d.gnss, now); ok {
		s.cur, s.note = "", ""
		return f, "", "", true
	}
	s.prune(now)
	// One device at a time: the one in use while it can be, else the one
	// heard last that can.
	v := s.devs[s.cur]
	if v == nil || v.held(now) != "" {
		v = nil
		var last *device
		for _, c := range s.devs {
			if last == nil || c.at.After(last.at) {
				last = c
			}
			if c.held(now) == "" && (v == nil || c.at.After(v.at)) {
				v = c
			}
		}
		s.note = ""
		if v == nil && last != nil {
			s.note = fmt.Sprintf("%s not used: %s", last.ip, last.held(now))
		}
	}
	if v == nil {
		s.cur = ""
		return gnss.State{}, "", "", false
	}
	s.cur, s.note = v.ip, ""
	p := v.p
	st := gnss.State{Mode: gnss.Mode2D, Lat: p.Lat, Lon: p.Lon, Time: v.at, Heard: v.at}
	if p.HAE != cot.Unknown {
		st.Mode, st.AltM, st.HasAlt = gnss.Mode3D, p.HAE, true
	}
	if p.CE != cot.Unknown && p.CE > 0 {
		st.HAccM = p.CE
	}
	if st.HasAlt && p.LE != cot.Unknown && p.LE > 0 {
		st.VAccM = p.LE
	}
	return st, v.ip, p.How, true
}

// phoneStatus fills the status's part of both directions.
func (d *Daemon) phoneStatus(st *status.Positions) {
	s := &d.phone
	s.mu.Lock()
	defer s.mu.Unlock()
	st.NMEADevices, st.NMEASent, st.NMEAHeld = s.nmeaTo, s.nmeaSent, s.nmeaHeld
	st.DeviceNote = s.note
	if s.saErr != "" {
		st.DeviceNote = "SA listener: " + s.saErr
	}
}

// bridgeConn is a UDP socket that sends on br-mesh only.
func bridgeConn() (*net.UDPConn, error) {
	lc := net.ListenConfig{Control: func(network, address string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) { serr = unix.BindToDevice(int(fd), BridgeIfname) }); err != nil {
			return err
		}
		return serr
	}}
	c, err := lc.ListenPacket(context.Background(), "udp4", "0.0.0.0:0")
	if err != nil {
		return nil, err
	}
	return c.(*net.UDPConn), nil
}

// saLoop reads the SA that arrives on this node's own AP and client
// ports. A packet socket with a filter, not a member of the SA group: a
// node that joined the group on br-mesh would be a listener in
// batman-adv's eyes, and every phone's SA in the mesh would be sent to it
// over the air.
func (d *Daemon) saLoop(ctx context.Context) {
	for ctx.Err() == nil {
		// Positions not shared: neither direction is used (nmeaLoop and
		// the positions reconciler hold back), so no socket: it would
		// filter every frame the node receives for nothing.
		if positionsOffIn(d.conf()) != "" {
			d.phone.mu.Lock()
			d.phone.saErr = "" // off: no listener, no listener fault to show
			d.phone.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}
		excl := meshIfindexes()
		fd, err := saSocket(excl)
		if err != nil {
			d.phone.mu.Lock()
			d.phone.saErr = err.Error()
			d.phone.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second):
			}
			continue
		}
		d.phone.mu.Lock()
		d.phone.saErr = ""
		d.phone.mu.Unlock()
		d.readSA(ctx, fd, excl)
		unix.Close(fd)
		// The socket failed: a moment before the next, so a fault that
		// persists cannot spin.
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// meshIfindexes is bat0's and br-mesh's interface index (0 for one not
// there): the SA filter drops frames arriving on them, the whole mesh's SA,
// which readSA would throw away (not this node's own ports).
func meshIfindexes() [2]uint32 {
	var out [2]uint32
	for i, name := range []string{BatIfname, BridgeIfname} {
		if l, err := netlink.LinkByName(name); err == nil {
			out[i] = uint32(l.Attrs().Index)
		}
	}
	return out
}

// saFilter is the SA socket's classic BPF program: frames not arriving on
// the excluded interfaces, IPv4 UDP to port 6969, whole datagrams only.
// SOCK_DGRAM: the program sees the packet from its IP header on.
func saFilter(excl [2]uint32) []unix.SockFilter {
	return []unix.SockFilter{
		{Code: 0x20, K: 0xfffff008},               // ld skb->dev->ifindex (SKF_AD_OFF + SKF_AD_IFINDEX)
		{Code: 0x15, Jt: 11, Jf: 0, K: excl[0]},   // jeq bat0: drop
		{Code: 0x15, Jt: 10, Jf: 0, K: excl[1]},   // jeq br-mesh: drop
		{Code: 0x20, K: 0xfffff000},               // ld skb->protocol (SKF_AD_OFF + SKF_AD_PROTOCOL)
		{Code: 0x15, Jt: 0, Jf: 8, K: 0x0800},     // jeq IPv4
		{Code: 0x30, K: 9},                        // ldb [9]: protocol
		{Code: 0x15, Jt: 0, Jf: 6, K: 17},         // jeq UDP
		{Code: 0x28, K: 6},                        // ldh [6]: flags, fragment offset
		{Code: 0x45, Jt: 4, Jf: 0, K: 0x3fff},     // jset more fragments or offset: drop
		{Code: 0xb1, K: 0},                        // ldxb 4*([0]&0xf): header length
		{Code: 0x48, K: 2},                        // ldh [x+2]: destination port
		{Code: 0x15, Jt: 0, Jf: 1, K: cot.SAPort}, // jeq 6969
		{Code: 0x06, K: 0x40000},                  // accept
		{Code: 0x06, K: 0},                        // drop
	}
}

// saSocket is an AF_PACKET socket for frames from any interface,
// filtered in the kernel to IPv4 UDP to port 6969 (cot.SAPort), whole
// datagrams only. ETH_P_ALL, not ETH_P_IP: a bridge port's frames go to
// the bridge before the per-protocol taps see them, and the AP and the
// client ports are bridge ports.
func saSocket(excl [2]uint32) (int, error) {
	// Protocol 0: no frames until bound, after the filter is on (a frame
	// queued before it would reach readSA unfiltered).
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	prog := saFilter(excl)
	fprog := unix.SockFprog{Len: uint16(len(prog)), Filter: &prog[0]}
	if err := unix.SetsockoptSockFprog(fd, unix.SOL_SOCKET, unix.SO_ATTACH_FILTER, &fprog); err != nil {
		unix.Close(fd)
		return -1, err
	}
	// Frames this node sends are not wanted: without this the kernel
	// hands a copy of every one, on every interface, to the socket before
	// the filter drops it (CPU on a forwarding node). Linux 4.20 on; an
	// older kernel leaves them to readSA's check.
	_ = unix.SetsockoptInt(fd, unix.SOL_PACKET, unix.PACKET_IGNORE_OUTGOING, 1)
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_ALL)}); err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

// htons is v in network byte order, as the host reads it.
func htons(v uint16) uint16 {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	return binary.NativeEndian.Uint16(b[:])
}

// readSA reads until ctx ends or the socket fails; a short timeout lets it
// look at ctx.
func (d *Daemon) readSA(ctx context.Context, fd int, excl [2]uint32) {
	_ = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Sec: 1})
	buf := make([]byte, 16<<10)
	var ports map[int]bool
	var portsAt time.Time
	for ctx.Err() == nil {
		n, from, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
				if positionsOffIn(d.conf()) != "" {
					return // turned off: saLoop waits without a socket
				}
				continue
			}
			return
		}
		ll, ok := from.(*unix.SockaddrLinklayer)
		if !ok || ll.Pkttype == unix.PACKET_OUTGOING {
			continue
		}
		now := time.Now()
		if ports == nil || now.Sub(portsAt) > portsEvery {
			if positionsOffIn(d.conf()) != "" {
				return // turned off: saLoop waits without a socket
			}
			if meshIfindexes() != excl {
				return // bat0 or br-mesh made again: a socket with their new indexes
			}
			ports, portsAt = d.ownPortIndexes(), now
		}
		if !ports[ll.Ifindex] {
			continue
		}
		src, payload, ok := udpPayload(buf[:n])
		if !ok || ll.Halen != 6 {
			continue
		}
		d.noteDeviceSA(src, net.HardwareAddr(ll.Addr[:6]).String(), payload, now)
	}
}

// ownPortIndexes are the interfaces devices attach to on this node: the
// AP and the client ports.
func (d *Daemon) ownPortIndexes() map[int]bool {
	out := map[int]bool{}
	var ap int
	d.facts.Read(func(f *Facts) { ap = f.APIfindex })
	if ap != 0 {
		out[ap] = true
	}
	cfg, _ := d.conf()
	var names []string
	if cfg != nil {
		names = append(names, cfg.ClientPorts()...)
	}
	d.facts.Read(func(f *Facts) { names = append(names, f.AutoClients...) })
	for _, n := range names {
		if ifi, err := net.InterfaceByName(n); err == nil {
			out[ifi.Index] = true
		}
	}
	return out
}

// udpPayload takes an IPv4 packet the filter passed: the source address
// and the UDP payload.
func udpPayload(p []byte) (string, []byte, bool) {
	if len(p) < 20 || p[0]>>4 != 4 {
		return "", nil, false
	}
	// Bounded by the IP total length, then the UDP length: a frame may
	// carry padding after the datagram.
	ihl := int(p[0]&0xf) * 4
	total := int(binary.BigEndian.Uint16(p[2:4]))
	if ihl < 20 || total < ihl+8 || total > len(p) {
		return "", nil, false
	}
	p = p[:total]
	udpLen := int(binary.BigEndian.Uint16(p[ihl+4 : ihl+6]))
	if udpLen < 8 || ihl+udpLen > total {
		return "", nil, false
	}
	// UDP to the SA port only (the kernel filter says so too; this holds
	// whatever reaches here).
	if p[9] != unix.IPPROTO_UDP || binary.BigEndian.Uint16(p[ihl+2:ihl+4]) != cot.SAPort {
		return "", nil, false
	}
	src := net.IP(p[12:16]).String()
	return src, p[ihl+8 : ihl+udpLen], true
}
