// Package deploy installs or upgrades Trellis on many nodes at once from
// an operator's computer (cmd/trellis-deploy): a signed release, a list of
// nodes, and SSH. It runs on Linux and macOS, and uses the system's ssh, so
// the operator's keys, agent and ssh configuration apply.
package deploy

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// Host is one node of the list.
type Host struct {
	Target   string // user@host, as ssh takes it
	Addr     string // the host part
	User     string // empty: ssh's default
	Port     int    // 0: ssh's default
	Callsign string // for a node set up by the run; empty: from its hostname
	NodeID   int    // 0: derived by the node
	// Mesh is the node's mesh address: when Addr does not answer, the node
	// is reached there, through another node of the list that does.
	Mesh string
	Line int
}

var (
	reCallsign = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	// Neither part may start with "-": ssh would take it for an option.
	reTarget = regexp.MustCompile(`^([A-Za-z0-9._][A-Za-z0-9._-]*@)?[A-Za-z0-9.:_\[][A-Za-z0-9.:_\[\]-]*$`)
)

// ParseHosts reads a hosts file: one node per line, "user@host" and then
// any of callsign=NAME, id=N (1–254), mesh=ADDRESS and port=N (ssh's);
// "#" starts a comment. It is the format of the developer's deploy.hosts,
// extended.
func ParseHosts(r io.Reader) ([]Host, error) {
	var out []Host
	seen := map[string]int{}
	callsigns := map[string]int{}
	sc := bufio.NewScanner(r)
	n := 0
	for sc.Scan() {
		n++
		line, _, _ := strings.Cut(sc.Text(), "#")
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if !reTarget.MatchString(f[0]) {
			return nil, fmt.Errorf("line %d: %q is not user@host", n, f[0])
		}
		h := Host{Target: f[0], Addr: f[0], Line: n}
		if u, a, ok := strings.Cut(f[0], "@"); ok {
			h.User, h.Addr = u, a
		}
		for _, kv := range f[1:] {
			k, v, ok := strings.Cut(kv, "=")
			if !ok || v == "" {
				return nil, fmt.Errorf("line %d: %q: expected key=value (callsign, id, mesh or port)", n, kv)
			}
			switch k {
			case "callsign":
				if !reCallsign.MatchString(v) {
					return nil, fmt.Errorf("line %d: callsign %q: lowercase letters, digits and hyphens, at most 63", n, v)
				}
				if first, dup := callsigns[v]; dup {
					return nil, fmt.Errorf("line %d: callsign %s is used already on line %d", n, v, first)
				}
				callsigns[v] = n
				h.Callsign = v
			case "id":
				id, err := strconv.Atoi(v)
				if err != nil || id < 1 || id > 254 {
					return nil, fmt.Errorf("line %d: id %q: a number from 1 to 254", n, v)
				}
				h.NodeID = id
			case "port":
				p, err := strconv.Atoi(v)
				if err != nil || p < 1 || p > 65535 {
					return nil, fmt.Errorf("line %d: port %q: a number from 1 to 65535", n, v)
				}
				h.Port = p
			case "mesh":
				if !reTarget.MatchString(v) || strings.Contains(v, "@") {
					return nil, fmt.Errorf("line %d: mesh %q is not an address", n, v)
				}
				h.Mesh = v
			default:
				return nil, fmt.Errorf("line %d: unknown setting %q (callsign, id, mesh or port)", n, k)
			}
		}
		if first, dup := seen[h.Key()]; dup {
			return nil, fmt.Errorf("line %d: %s is listed already on line %d", n, h.Key(), first)
		}
		seen[h.Key()] = n
		out = append(out, h)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	ids := map[int]int{}
	for _, h := range out {
		if h.NodeID == 0 {
			continue
		}
		if first, dup := ids[h.NodeID]; dup {
			return nil, fmt.Errorf("line %d: id %d is used already on line %d", h.Line, h.NodeID, first)
		}
		ids[h.NodeID] = h.Line
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no nodes listed")
	}
	return out, nil
}

// Key tells nodes apart: the address, and the port when one is given.
func (h Host) Key() string {
	if h.Port != 0 {
		return fmt.Sprintf("%s:%d", h.Addr, h.Port)
	}
	return h.Addr
}

// CallsignFrom makes a callsign from a hostname, as a node set up by hand
// would get: lowercase, letters, digits and hyphens.
func CallsignFrom(hostname string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(hostname) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		case r == '_' || r == '.':
			b.WriteRune('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > 63 {
		s = strings.TrimRight(s[:63], "-")
	}
	if s == "" {
		s = "node"
	}
	return s
}
