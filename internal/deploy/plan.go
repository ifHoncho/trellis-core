package deploy

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// State is what a node is like before the run.
type State struct {
	Route     Route
	Reachable bool
	Err       string // why it is not reachable
	Refused   bool   // ssh reached it and was turned away (key, host key)
	Arch      string // Debian architecture
	OS        string
	Hostname  string
	User      string // the login user
	Trellis   string // installed trellis package version; empty: none
	Batman    string // installed trellis-batman-adv-dkms version
	Voice     string // installed trellis-voice version
	Profile   bool   // /etc/trellis/profile.yaml exists
	NodeConf  bool   // /etc/trellis/node.yaml exists
	NoPass    bool   // sudo works without a password
	Apt       bool
	Cmp       string // the installed trellis against the release: older, same or newer
	Reboot    bool   // a reboot is pending already
}

// probe is the read-only command that describes a node; $T is the
// release's Debian version.
const probe = `T=%s
echo "arch=$(dpkg --print-architecture 2>/dev/null)"
. /etc/os-release 2>/dev/null; echo "os=${PRETTY_NAME:-unknown}"
echo "hostname=$(hostname)"
echo "user=$(id -un)"
v=$(dpkg-query -W -f='${db:Status-Abbrev}|${Version}' trellis 2>/dev/null)
case "$v" in ii*) v=${v#*|}; echo "trellis=$v"
  if dpkg --compare-versions "$v" lt "$T"; then echo cmp=older
  elif dpkg --compare-versions "$v" eq "$T"; then echo cmp=same
  else echo cmp=newer; fi ;;
esac
b=$(dpkg-query -W -f='${db:Status-Abbrev}|${Version}' trellis-batman-adv-dkms 2>/dev/null)
case "$b" in ii*) echo "batman=${b#*|}" ;; esac
b=$(dpkg-query -W -f='${db:Status-Abbrev}|${Version}' trellis-voice 2>/dev/null)
case "$b" in ii*) echo "voice=${b#*|}" ;; esac
[ -e /etc/trellis/profile.yaml ] && echo profile=yes
[ -e /etc/trellis/node.yaml ] && echo node=yes
sudo -n true 2>/dev/null && echo sudo=nopass
command -v apt-get >/dev/null 2>&1 && echo apt=yes
[ -e /run/reboot-required ] && echo reboot=yes
true`

// ParseState reads the probe's output.
func ParseState(out string) State {
	var s State
	for _, l := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(l), "=")
		if !ok {
			continue
		}
		switch k {
		case "arch":
			s.Arch = v
		case "os":
			s.OS = v
		case "hostname":
			s.Hostname = v
		case "user":
			s.User = v
		case "trellis":
			s.Trellis = v
		case "cmp":
			s.Cmp = v
		case "batman":
			s.Batman = v
		case "voice":
			s.Voice = v
		case "profile":
			s.Profile = v == "yes"
		case "node":
			s.NodeConf = v == "yes"
		case "sudo":
			s.NoPass = v == "nopass"
		case "apt":
			s.Apt = v == "yes"
		case "reboot":
			s.Reboot = v == "yes"
		}
	}
	return s
}

// Inspect reaches every node, eight at a time, and reads its state. A node
// with a mesh address that does not answer at its own is tried through the
// first node that does.
func Inspect(ctx context.Context, s Remote, hosts []Host, debVersion string) []State {
	states := make([]State, len(hosts))
	probeCmd := fmt.Sprintf(probe, quote(debVersion))
	look := func(i int, r Route) {
		out, err := s.Output(ctx, r, probeCmd)
		st := ParseState(out)
		st.Route = r
		if err != nil {
			// ssh's verdict is its last line; the lines before it (a
			// banner of @ signs, warnings) are not the reason.
			st.Err, st.Refused = lastLine(out, err), refused(out)
		} else {
			st.Reachable = true
		}
		states[i] = st
	}
	run := func(idx []int, route func(int) (Route, bool)) {
		var wg sync.WaitGroup
		sem := make(chan struct{}, 8)
		for _, i := range idx {
			r, ok := route(i)
			if !ok {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				look(i, r)
			}()
		}
		wg.Wait()
	}
	all := make([]int, len(hosts))
	for i := range hosts {
		all[i] = i
	}
	run(all, func(i int) (Route, bool) { return Route{Target: hosts[i].Target, Port: hosts[i].Port}, true })

	// Through the mesh: the first node reached directly carries the rest.
	via, viaPort := "", 0
	for i, st := range states {
		if st.Reachable && st.Route.Via == "" {
			via, viaPort = hosts[i].Target, hosts[i].Port
			break
		}
	}
	// Only a node that could not be connected to: one that answered and
	// refused (the key, its host key) is where it should be.
	var again []int
	for i, st := range states {
		if !st.Reachable && hosts[i].Mesh != "" && via != "" && !st.Refused {
			again = append(again, i)
		}
	}
	run(again, func(i int) (Route, bool) {
		h := hosts[i]
		t := h.Mesh
		if h.User != "" {
			t = h.User + "@" + h.Mesh
		}
		return Route{Target: t, Port: h.Port, Via: via, ViaPort: viaPort, Alias: h.Addr}, true
	})
	return states
}

// lastLine is the last non-empty line of out, or err's text.
func lastLine(out string, err error) string {
	ls := strings.Split(strings.TrimSpace(out), "\n")
	if l := strings.TrimSpace(ls[len(ls)-1]); l != "" {
		return l
	}
	if err == nil {
		return ""
	}
	return err.Error()
}

// refused reports whether ssh's output says it reached the node and was
// turned away: the key, or the node's host key.
func refused(e string) bool {
	return strings.Contains(e, "Permission denied") || strings.Contains(e, "Host key verification failed") ||
		strings.Contains(e, "REMOTE HOST IDENTIFICATION HAS CHANGED") || strings.Contains(e, "host key is known")
}

func firstLine(out string, err error) string {
	for _, l := range strings.Split(out, "\n") {
		// ssh's own notes ("Warning: Permanently added …") are not the
		// answer.
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "Warning: ") {
			return l
		}
	}
	if err == nil {
		return ""
	}
	return err.Error()
}

// Action is what the run does to a node.
type Action int

const (
	Skip    Action = iota // cannot, or must not
	Install               // no Trellis yet
	Upgrade               // an older Trellis
	Current               // runs this release already
	Setup                 // --setup: SSH key and sudo for later runs
)

func (a Action) String() string {
	return [...]string{"skip", "new install", "upgrade", "up to date", "setup"}[a]
}

// Step is one node's part of the run.
type Step struct {
	Host     Host
	State    State
	Action   Action
	Why      string // for Skip, or a note
	Callsign string // for Install: the callsign it gets, if it gets a configuration
	Package  string // the trellis package for its architecture
	Label    string // how the run calls it: unique among the steps
}

// PlanOptions change what the plan does.
type PlanOptions struct {
	Profile bool // a mesh profile is given for nodes without one
	Force   bool // reinstall nodes that run this release already
	// AnyVersion installs over a newer version too: development builds do
	// not sort.
	AnyVersion bool
}

// Plan decides each node's step, in the order they run: new installs first
// (they disturb nobody, and go in parallel), then upgrades one at a time,
// nodes reached through the mesh first, so that the way to the others
// stays up the longest.
func Plan(hosts []Host, states []State, rel *Release, o PlanOptions) []Step {
	var steps []Step
	for i, h := range hosts {
		st := states[i]
		s := Step{Host: h, State: st, Package: rel.Arches[st.Arch]}
		switch {
		case !st.Reachable:
			s.Action, s.Why = Skip, "unreachable: "+st.Err
		case !st.Apt || st.Arch == "":
			s.Action, s.Why = Skip, "not a Debian-family system ("+st.OS+")"
		case s.Package == "":
			s.Action, s.Why = Skip, fmt.Sprintf("no %s package in this release (%s)", st.Arch, strings.Join(arches(rel), ", "))
		case st.Trellis == "":
			s.Action = Install
			s.Callsign = h.Callsign
			if s.Callsign == "" {
				s.Callsign = CallsignFrom(st.Hostname)
			}
			switch {
			case st.NodeConf:
				s.Why = "keeps its configuration"
			case st.Profile || o.Profile:
				s.Why = "callsign " + s.Callsign
			default:
				s.Why = "waits in setup mode (no mesh profile given)"
			}
		case st.Cmp == "older":
			s.Action, s.Why = Upgrade, st.Trellis+" → "+rel.Debian
		case st.Cmp == "same" && o.Force:
			s.Action, s.Why = Upgrade, "reinstall "+rel.Debian+" (--force)"
		case st.Cmp == "same":
			s.Action, s.Why = Current, rel.Debian
		case st.Cmp == "newer" && o.AnyVersion:
			s.Action, s.Why = Upgrade, st.Trellis+" → "+rel.Debian+" (older; --any-version)"
		default:
			s.Action, s.Why = Skip, "runs "+st.Trellis+", newer than this release"
		}
		steps = append(steps, s)
	}
	rank := func(s Step) int {
		switch {
		case s.Action == Install:
			return 0
		case s.Action == Upgrade && s.State.Route.Via != "":
			return 1
		case s.Action == Upgrade:
			return 2
		}
		return 3
	}
	sort.SliceStable(steps, func(i, j int) bool { return rank(steps[i]) < rank(steps[j]) })
	label(steps)
	return steps
}

// label names each step: its callsign or hostname, with its address where
// two nodes share a name (images and fresh installs often do).
func label(steps []Step) {
	count := map[string]int{}
	for _, s := range steps {
		count[s.baseName()]++
	}
	for i := range steps {
		n := steps[i].baseName()
		if count[n] > 1 && n != steps[i].Host.Key() {
			n += "@" + steps[i].Host.Key()
		}
		steps[i].Label = n
	}
}

func arches(rel *Release) []string {
	var out []string
	for a := range rel.Arches {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}
