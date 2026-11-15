package meshd

import (
	"context"
	"fmt"
	"time"

	"meridian/trellis/internal/config"
	"meridian/trellis/internal/nft"
	"meridian/trellis/internal/proc"
	"meridian/trellis/internal/reconcile"
)

// firewallOpenNotice is the status line of a node open from outside.
const firewallOpenNotice = "this node takes connections from outside the mesh to every service its system runs. Closed, it takes SSH, the web UI where it is served beyond the mesh, and the ports you list (firewall.wan: closed; Settings → Access)"

// firewallReconciler owns the firewall towards everything outside the mesh
// (firewall.* in node.yaml, docs/security.md R2): the nftables table
// trellis_fw, with an input chain while the node is closed. It runs early
// and on its own, so that a node is closed whatever state its mesh is in
// (setup mode, no radio, bat0 not up), and it checks the table on every
// pass: if something flushes nftables, the next pass puts it back. What it
// reports is what the kernel holds, not what the file asks for.
type firewallReconciler struct {
	d       *Daemon
	nftPath string
	applied string // script in force
	// checked is when the table was last read: a pass between checks (the
	// loop runs often on busy moments) does not fork nft again.
	checked time.Time
}

func newFirewallReconciler(d *Daemon) *firewallReconciler { return &firewallReconciler{d: d} }

func (f *firewallReconciler) Name() string { return "firewall" }

// firewallCheckEvery is how often a closed node checks its table is there.
const firewallCheckEvery = 15 * time.Second

func (f *firewallReconciler) Reconcile(ctx context.Context) (reconcile.Result, error) {
	d := f.d
	var res reconcile.Result
	warn := func(level, code, format string, args ...any) {
		res.Warnings = append(res.Warnings, reconcile.Warning{Level: level, Code: code, Message: fmt.Sprintf(format, args...)})
	}
	set := func(fw firewallFacts) { d.facts.Update(func(fa *Facts) { fa.Firewall = fw }) }
	in := inputPolicy(d.cfg)
	if f.nftPath == "" {
		p, err := proc.LookPath("nft")
		if err != nil {
			if in != nil {
				set(firewallFacts{State: "failed", Error: "nft is not installed"})
				warn("error", "firewall-failed", "the node is set to be closed from outside the mesh, but nft is not installed: it is open. apt install nftables")
			} else {
				set(firewallFacts{State: config.FirewallOpen})
			}
			res.RequeueAfter = 30 * time.Second
			return res, nil
		}
		f.nftPath = p
	}
	want := ""
	if in != nil {
		want = in.Render()
	}
	if want == f.applied && !f.checked.IsZero() && time.Since(f.checked) < firewallCheckEvery {
		res.RequeueAfter = firewallCheckEvery - time.Since(f.checked)
		if in == nil {
			warn("info", "firewall-open", firewallOpenNotice)
		}
		return res, nil
	}
	f.checked = time.Now()
	present, counters, err := nft.FirewallState(ctx, f.nftPath)
	if err != nil {
		set(firewallFacts{State: "failed", Error: err.Error()})
		warn("error", "firewall-failed", "cannot read the firewall: %v", err)
		res.RequeueAfter = 10 * time.Second
		return res, nil
	}
	if in == nil {
		if present {
			if err := nft.DeleteTable(ctx, f.nftPath, "inet", nft.FirewallTable); err != nil {
				return res, err
			}
			d.log.Info("firewall opened: the node takes everything from outside the mesh")
			res.Changed = true
		}
		f.applied = ""
		set(firewallFacts{State: config.FirewallOpen})
		warn("info", "firewall-open", firewallOpenNotice)
		return res, nil
	}
	script := want
	if !present || script != f.applied {
		if err := nft.Apply(ctx, f.nftPath, script); err != nil {
			f.applied = ""
			set(firewallFacts{State: "failed", Error: err.Error()})
			warn("error", "firewall-failed", "the node is set to be closed from outside the mesh, but the firewall cannot be applied: %v", err)
			res.RequeueAfter = 10 * time.Second
			return res, nil
		}
		if f.applied != "" && !present {
			d.log.Warn("the firewall table had gone (nftables flushed?); put back")
		} else {
			d.log.Info("firewall closed: from outside the mesh only replies, ICMP, DHCP and the ports listed", "open", in.Ports())
		}
		f.applied = script
		res.Changed = true
		counters = nil
	}
	set(firewallFacts{State: config.FirewallClosed, Open: in.Ports(), Dropped: counters[nft.RuleWANClosed] + counters[nft.RuleIntoMesh]})
	res.RequeueAfter = firewallCheckEvery
	return res, nil
}

// Teardown leaves a closed node closed: stopping meshd (an upgrade, a
// deploy, a crash) must not open it. The table stays until meshd opens the
// node again, or the package is removed (its prerm deletes it). An open
// node has no table to remove.
func (f *firewallReconciler) Teardown(ctx context.Context) error {
	if f.applied != "" {
		f.d.log.Info("firewall stays in place while meshd is stopped")
	}
	return nil
}

// firewallFacts is what the firewall reconciler found, for status.
type firewallFacts struct {
	State   string // open, closed, failed
	Open    []string
	Dropped uint64
	Error   string
}

// inputPolicy is the firewall the node configuration asks for (nil: open).
// The mesh side (loopback, the bridge, the anycast gateway) is never
// filtered; from elsewhere: SSH unless turned off, the web UI where it is
// served beyond the mesh (web.wan_access), and the ports listed.
func inputPolicy(cfg *config.Node) *nft.Input {
	fw := cfg.Firewall
	if !fw.Closed() {
		return nil
	}
	in := &nft.Input{Inside: []string{"lo", BridgeIfname, AnycastIfname}}
	seen := map[string]bool{}
	add := func(proto string, port int) {
		k := fmt.Sprintf("%s/%d", proto, port)
		if seen[k] {
			return
		}
		seen[k] = true
		if proto == "tcp" {
			in.TCP = append(in.TCP, port)
		} else {
			in.UDP = append(in.UDP, port)
		}
	}
	if fw.SSHOpen() {
		add("tcp", 22)
	}
	if cfg.Web.WANAccess {
		add("tcp", 80)
		add("tcp", 443)
	}
	for _, a := range fw.Allow {
		if proto, port, err := config.ParsePort(a); err == nil {
			add(proto, port)
		}
	}
	return in
}
