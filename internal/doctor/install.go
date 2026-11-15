package doctor

import (
	"context"
	"fmt"
	"os"

	"meridian/trellis/internal/release"
	"meridian/trellis/internal/upgrade"
)

// checkInstall says how Trellis got onto this node and whether an upgrade
// is left unfinished.
func checkInstall(r *Report) {
	ps := upgrade.QueryPackage(context.Background(), upgrade.HostSystem{})
	_, localErr := os.Lstat("/usr/local/bin/trellis")
	packaged := ps.Installed()
	r.checkInstallKind(ps, localErr == nil)

	dir := upgrade.DefaultDir
	if p, err := upgrade.ReadPending(dir); err == nil && p != nil {
		r.add(Warn, "upgrade", fmt.Sprintf("an upgrade from %s to %s was left unfinished (%s)", p.From, p.To, p.Phase), upgrade.ResumeHint)
	} else if last, err := upgrade.ReadResult(dir); err == nil && last != nil && last.Result != upgrade.ResultUpgraded && last.Result != upgrade.ResultUnchanged {
		r.add(Info, "upgrade", fmt.Sprintf("the last upgrade (%s to %s) ended as %s: %s", last.From, last.To, last.Result, last.Reason), "")
	}
	if packaged {
		keys, err := release.TrustedKeys(release.KeyDir)
		switch {
		case err != nil:
			r.add(Warn, "release keys", err.Error(), "fix or remove the file under "+release.KeyDir)
		case len(keys) == 0:
			r.add(Info, "release keys", "none trusted: meshctl upgrade takes a package only with --allow-untrusted", "put the release's public key in "+release.KeyDir+"/NAME.pub")
		default:
			r.add(Pass, "release keys", fmt.Sprintf("%d trusted", len(keys)), "")
		}
	}
}

func (r *Report) checkInstallKind(ps upgrade.PackageState, local bool) {
	switch {
	case ps.Installed() && local:
		r.add(Warn, "install", fmt.Sprintf("the trellis package %s is installed, and an older copy from the install scripts sits in /usr/local/bin, where it is found first", ps.Version),
			"sudo rm /usr/local/bin/trellis /usr/local/bin/meshd /usr/local/bin/meshctl /usr/local/bin/webd")
	case ps.Installed():
		r.add(Pass, "install", "trellis package "+ps.Version, "")
	case ps.HalfDone():
		r.add(Fail, "install", fmt.Sprintf("the trellis package is not cleanly installed (dpkg state %q)", ps.State), "sudo dpkg --configure -a")
	case local:
		r.add(Info, "install", "installed by the scripts, not from a package: upgrades that can go back need the package", "sudo apt install ./trellis_VERSION_ARCH.deb (it takes the script install over)")
	default:
		r.add(Info, "install", "not installed from a package (run from a build directory?)", "")
	}
}
