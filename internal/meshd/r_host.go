package meshd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"meridian/trellis/internal/config"
	"meridian/trellis/internal/reconcile"
)

// hostReconciler prepares the host: directories, and telling
// NetworkManager to leave Trellis interfaces alone (architecture §12.4).
type hostReconciler struct {
	d *Daemon
}

func newHostReconciler(d *Daemon) *hostReconciler { return &hostReconciler{d: d} }

func (h *hostReconciler) Name() string { return "host" }

const nmConfPath = "/etc/NetworkManager/conf.d/90-trellis.conf"

// trellisIfnames are the interfaces Trellis creates on a node whose profile
// has the given number of mesh links. mesh0 and mesh1 are always listed, so
// that the files below do not change (and NetworkManager and networkd are
// not reloaded) on a node with one or two links; a third link and up add
// mesh2, mesh3, …. A member of the site link adds site0 and its tunnels
// (site-* matches every one).
func trellisIfnames(links int, site bool) []string {
	names := []string{meshIfname(0), meshIfname(1), APIfname, BatIfname, BridgeIfname, "anycast0"}
	for i := 2; i < links && i < config.MaxMeshLinks; i++ {
		names = append(names, meshIfname(i))
	}
	if site {
		names = append(names, SiteIfname, siteIfPrefix+"*")
	}
	return names
}

// nmConf tells NetworkManager to leave Trellis interfaces alone, and the
// wired ports node.yaml gives to Trellis (client, mesh, auto).
func nmConf(links int, site bool, ports []string) string {
	var specs []string
	for _, n := range append(trellisIfnames(links, site), ports...) {
		specs = append(specs, "interface-name:"+n)
	}
	return "# Written by trellis meshd. Interfaces owned by Trellis must not be managed\n# by NetworkManager.\n[keyfile]\nunmanaged-devices=" +
		strings.Join(specs, ";") + "\n"
}

// networkdConfPath sorts before netplan's 10-netplan-*.network files, so
// systemd-networkd never configures Trellis interfaces even when a netplan
// rule matches by wildcard.
const networkdConfPath = "/etc/systemd/network/00-trellis-unmanaged.network"

func networkdConf(links int, site bool, ports []string) string {
	return "# Written by trellis meshd. Interfaces owned by Trellis must not be managed\n# by systemd-networkd.\n[Match]\nName=" +
		strings.Join(append(trellisIfnames(links, site), ports...), " ") + "\n\n[Link]\nUnmanaged=yes\n"
}

func (h *hostReconciler) Reconcile(ctx context.Context) (reconcile.Result, error) {
	var res reconcile.Result
	// A reloaded configuration takes effect here, before any other
	// reconciler runs, so a pass never mixes old and new settings.
	if h.d.applyPending() {
		res.Changed = true
	}
	for _, dir := range []string{h.d.opts.StateDir, h.d.opts.RunDir, filepath.Join(h.d.opts.RunDir, "wpa")} {
		if _, err := os.Stat(dir); err != nil {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return res, err
			}
			res.Changed = true
		}
	}
	if h.d.opts.NoHostConfig {
		return res, nil
	}
	if st, err := os.Stat(filepath.Dir(nmConfPath)); err == nil && st.IsDir() {
		cur, _ := os.ReadFile(nmConfPath)
		want := nmConf(1+len(h.d.profile.Links), h.d.siteLinkPort(h.d.profile) != 0, h.d.cfg.OwnedPorts())
		if string(cur) != want {
			if err := os.WriteFile(nmConfPath, []byte(want), 0o644); err != nil {
				res.Warnings = append(res.Warnings, reconcile.Warning{Level: "warning", Code: "nm-conf", Message: "cannot write " + nmConfPath + ": " + err.Error()})
			} else {
				res.Changed = true
				h.d.log.Info("NetworkManager told to ignore Trellis interfaces", "path", nmConfPath)
				// Takes effect on a configuration reload; harmless if NM is not running.
				if err := exec.CommandContext(ctx, "nmcli", "general", "reload", "conf").Run(); err != nil {
					_ = exec.CommandContext(ctx, "systemctl", "try-reload-or-restart", "NetworkManager").Run()
				}
			}
		}
	}
	if st, err := os.Stat(filepath.Dir(networkdConfPath)); err == nil && st.IsDir() {
		cur, _ := os.ReadFile(networkdConfPath)
		want := networkdConf(1+len(h.d.profile.Links), h.d.siteLinkPort(h.d.profile) != 0, h.d.cfg.OwnedPorts())
		if string(cur) != want {
			if err := os.WriteFile(networkdConfPath, []byte(want), 0o644); err != nil {
				res.Warnings = append(res.Warnings, reconcile.Warning{Level: "warning", Code: "networkd-conf", Message: "cannot write " + networkdConfPath + ": " + err.Error()})
			} else {
				res.Changed = true
				h.d.log.Info("systemd-networkd told to ignore Trellis interfaces", "path", networkdConfPath)
				// Only reloads if networkd is running; harmless otherwise.
				_ = exec.CommandContext(ctx, "networkctl", "reload").Run()
			}
		}
	}
	return res, nil
}

func (h *hostReconciler) Teardown(ctx context.Context) error { return nil }
