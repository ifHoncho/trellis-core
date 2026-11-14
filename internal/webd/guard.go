package webd

import (
	"context"
	"net"
	"net/netip"
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
		info, _ := g.s.webInfo(context.Background())
		prefix, err := netip.ParsePrefix(info.Prefix)
		if err != nil {
			prefix = defaultPrefix
		}
		if allowed(la.AddrPort().Addr(), ra.AddrPort().Addr(), prefix, info.WANAccess) {
			return c, nil
		}
		g.s.log.Debug("refused a connection from outside the mesh", "local", la.String(), "remote", ra.String())
		c.Close()
	}
}
