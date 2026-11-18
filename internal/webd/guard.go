package webd

import (
	"net"
	"net/netip"
	"sync"
)

// The UI is reachable only from the mesh and this node's client ports
// (architecture §11): a connection is served when both its ends are
// inside the mesh prefix, or both are on the loopback. Checking the far
// end as well as the near one matters while the node has no firewall
// of its own: a host on the uplink network can address the node's mesh
// address, but it cannot have a mesh address of its own.
//
// node.yaml's web.wan_access lifts the restriction, for a bench or a node
// managed over its uplink.

// allowed decides whether a connection may be served.
func allowed(local, remote netip.Addr, prefix netip.Prefix, wanAccess bool) bool {
	local, remote = local.Unmap(), remote.Unmap()
	if local.IsLoopback() && remote.IsLoopback() {
		return true
	}
	if wanAccess {
		return true
	}
	return prefix.IsValid() && prefix.Contains(local) && prefix.Contains(remote)
}

// defaultPrefix is the mesh prefix assumed until meshd has answered once.
var defaultPrefix = netip.MustParsePrefix("10.73.0.0/16")

type guardedListener struct {
	net.Listener
	s *Server
}

// guard wraps a listener so that it only hands out allowed connections.
func (s *Server) guard(ln net.Listener) net.Listener { return &guardedListener{Listener: ln, s: s} }

func (g *guardedListener) Accept() (net.Conn, error) {
	for {
		c, err := g.Listener.Accept()
		if err != nil {
			return nil, err
		}
		la, lok := c.LocalAddr().(*net.TCPAddr)
		ra, rok := c.RemoteAddr().(*net.TCPAddr)
		if !lok || !rok {
			c.Close()
			continue
		}
		info, _ := g.s.cachedInfo()
		prefix, err := netip.ParsePrefix(info.Prefix)
		if err != nil {
			prefix = defaultPrefix
		}
		if !allowed(la.AddrPort().Addr(), ra.AddrPort().Addr(), prefix, info.WANAccess) {
			g.s.log.Debug("refused a connection from outside the mesh", "local", la.String(), "remote", ra.String())
			c.Close()
			continue
		}
		if cc := g.s.track(c, ra.AddrPort().Addr().Unmap()); cc != nil {
			return cc, nil
		}
		g.s.log.Debug("refused a connection: too many open from one address", "remote", ra.String(), "limit", connsPerAddr)
		c.Close()
	}
}

// connsPerAddr bounds the connections one address may hold open: a browser
// uses a handful, and a client of the node's Wi-Fi must not be able to
// take all of webd's.
const connsPerAddr = 32

// track counts c against its remote address, or returns nil when that
// address has too many open already.
func (s *Server) track(c net.Conn, addr netip.Addr) net.Conn {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.conns == nil {
		s.conns = map[netip.Addr]int{}
	}
	if s.conns[addr] >= connsPerAddr {
		return nil
	}
	s.conns[addr]++
	return &trackedConn{Conn: c, release: func() {
		s.connMu.Lock()
		defer s.connMu.Unlock()
		if s.conns[addr]--; s.conns[addr] <= 0 {
			delete(s.conns, addr)
		}
	}}
}

// trackedConn gives its address's count back once, when closed.
type trackedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

// CloseWrite lets net/http half-close a plain TCP connection, as it does
// for one that is not wrapped.
func (c *trackedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}
