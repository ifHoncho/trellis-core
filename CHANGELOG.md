# Changelog

## 0.2.1: fixes from a code review

Fixes from a code review of 0.2.0 and the rest of the code.

- **Firewall:** a gateway set to closed (`firewall.wan: closed`, the
  default for new nodes) still forwarded new connections from its uplink
  network into the mesh, so a host there that routed the mesh prefix
  through it reached every node and device. A closed node now forwards
  only replies, ICMP and your own port forwards into the mesh.
- **Go 1.27.4:** builds use it, for a fix to a crash in Go's HTTP server
  (GO-2026-6859) that the web UI runs on.
- **Configuration files** (profile, node configuration, admin password)
  are written to the disk before they replace the old ones, so a power
  loss leaves the old file or the new one, never an empty one.

Fixes to `trellis-deploy`:

- **Upgrades over the mesh:** an upgrade whose SSH session dropped while
  the node restarted its mesh was read back against the wrong version
  string. For a development build it waited ten minutes and reported a
  failure, which stopped the remaining upgrades.
- **`--setup` on new devices:** a device ssh had never connected to was
  taken for unreachable, so its key was never installed. Host keys are now
  accepted the first time a device is seen and refused if they change
  (`--ssh-option StrictHostKeyChecking=yes` requires known hosts).
- **Nodes reached over the mesh** are connected to through a command that
  carries your ssh options (key, timeouts) to the hop as well, and work
  with a ProxyCommand of your own. They keep their `port=`. A node that
  answers and refuses the key, or whose host key changed, is reported as
  such rather than tried again over the mesh.
- **Upgrades** now run `meshctl prepare` afterwards, as installs do, so a
  release's driver options and regulatory rules apply; a node that runs
  Trellis but waits in setup mode gets the `--profile` and joins the mesh;
  a node that asks for a reboot afterwards says so in the summary.
- **`--no-health-check`:** an error line in the node's log is no longer
  taken for the install's end while it still runs.
- **The development key** a run gave the nodes (`--node-key`) is removed
  by a run on the built-in release keys alone, once the node took the
  release.
- **Smaller:** your `--ssh-option` values override the tool's defaults;
  Ctrl-C at a prompt stops the program; `--new-mesh` reports a failed
  write; the admin password is not asked for with `--no-reboot`, which
  says so; a node keeping a different mesh profile is warned about; the
  plan names the node a route goes through; no crash on empty output.
- **`make deploy`:** `deploy.hosts` lines reach `trellis-deploy` whole
  (`port=`, `callsign=`, `id=`); every architecture is built, with no
  probing node by node; `deploy.sh --setup` runs `trellis-deploy --setup`.

## 0.2.0: deploying from your computer

Trellis now installs on a set of devices from one
machine: list them, add your SSH key, create a mesh profile, deploy. The
[guide](docs/guide.md) walks through it.

### trellis-deploy

- **Installs or upgrades every node in a list, over SSH,** from Linux or
  macOS. Builds for both, on amd64 and arm64, come with each release. It
  uses the system's `ssh`, so your keys, agent and ssh configuration apply.
- **Checks the release first:** the checksums, and every package's
  signature against the release keys built into it. Nothing reaches a node
  unless all of it checks out.
- **Shows its plan and asks once:** for each node, a new install, an
  upgrade, nothing, or why it skips it (unreachable, not Debian-family, no
  package for its architecture, a newer version). `--dry-run` stops there.
- **New nodes,** a few at a time: the packages, `meshctl prepare`, the
  mesh profile (`--profile`), callsign and node ID from the list, the web
  UI's admin password (asked once), then a reboot, waiting until each is
  back. The summary ends with each new node's Wi-Fi name and passphrase.
- **Upgrades,** one node at a time with `meshctl upgrade`: each checks
  itself and goes back on its own if it is not healthy, and the first that
  goes back stops the rest (`--keep-going`). Nodes reached over the mesh
  go first; a node whose connection drops while its mesh restarts is
  reconnected to and asked how it ended. A node that refuses a package
  says why.
- **The devices file:** `user@address`, with optional `callsign=`, `id=`,
  `port=` (SSH) and `mesh=` (the node's mesh address, to reach it through
  another listed node when it is off the LAN).
- **sudo:** each password given is tried on every node that needs one, and
  a node it does not open is asked for by name; Enter leaves that node
  out. The passwords and the mesh secret are never written to the logs.
- **`--setup`** puts your SSH key on the nodes (`ssh-copy-id`).
  Password-free sudo is a separate, explicit `--sudo-nopasswd`, with its
  risks set out in the [reference](docs/reference.md#passwords-and-sudo).
- **`--new-mesh NAME --country CC`** makes a mesh profile with a fresh
  secret, on 2.4 GHz or `--band 5g`.
- **The summary** gives each node's result and what `meshctl doctor`
  finds there; each node's whole log is in `trellis-deploy-logs/`.
- For development builds: `--trust`, `--node-key` (make the nodes trust a
  key), `--any-version` (install over a newer build) and
  `--no-health-check` (apt, no rollback).

### Nodes

- **`sudo meshctl prepare`** does everything a node needs once the packages
  are in, skipping what is already done: the pinned batman-adv built and
  loaded, the MediaTek MT7915/MT7916 driver where the kernel lacks it, the
  radio driver options, the regulatory rules. It ends with `meshctl
  doctor`'s checks and says when a reboot is needed (also in
  `/run/reboot-required`). A manual install is now `apt install`, `meshctl
  prepare`, reboot, and the repair hints in doctor and the alerts point to
  it.

### Documentation

- The guide is now a short first-time setup with `trellis-deploy`; the
  details are in the new `docs/reference.md`. The security model covers
  the deploy tool.

### Development

- `make deploy` builds the packages, signed with the development key
  (`TRELLIS_DEV_KEY`, now required), and deploys them with
  `trellis-deploy`, health-checked; `FAST=1` installs with apt and skips
  the health check, `DRY_RUN=1` only shows the plan. `scripts/node-update.sh`
  is gone; `deploy.sh --setup` installs the SSH key, and `--sudo-nopasswd`
  also gives sudo without a password.
- `make deploy-test`: `trellis-deploy` against Debian and Ubuntu
  containers running sshd (installs, upgrades, a refused package, a wrong
  sudo password, the development path, `--setup`). `make deb-test` also
  runs `meshctl prepare`.
- Releases: `make publish` checks a release and publishes it on GitHub,
  with its changelog section as the notes; `make release` builds
  `trellis-deploy` into it.

## 0.1.0: first alpha release

The first signed release. It is an alpha: the mesh, the client network,
gateways, multicast, the web UI and the packages are in daily use on test
nodes, but expect rough edges, and upgrade every node of a mesh together.

What follows is everything added or changed since the previous snapshot of
this repository.

### Packages, releases and upgrades

- **Debian packages.** `trellis` for arm64, armhf and amd64 (the binary,
  its systemd units, udev rule and chrony drop-in, and the hardware scripts
  under `/usr/share/trellis`) and `trellis-batman-adv-dkms` (the pinned
  batman-adv release, built by DKMS for the node's kernel and again after
  every kernel update). `apt` brings in what Trellis needs. Installing,
  upgrading and removing never touch `/etc/trellis` or `/var/lib/trellis`.
  A node set up by the earlier install scripts is taken over by the
  package; `scripts/install-node.sh` is gone.
- **Signed releases.** A package carries an Ed25519 signature inside it,
  in an archive member `dpkg` skips, so one file is both installable by
  `apt` and checkable by a node with no other tool. Two release keys are
  built in; an operator can trust more under `/etc/trellis/release-keys/`.
- **Upgrades that go back by themselves.** `meshctl upgrade FILE.deb`
  checks the signature and the version, keeps the running version and the
  configuration, installs, then checks the node: `meshd` on the new
  version, nothing failing that worked before, a mesh peer back if it had
  one, the web page answering. If that does not hold, the old version and
  its configuration go back in. It runs apart from the operator's session,
  and a node that loses power in the middle finishes, or goes back, at its
  next start. `--check`, `--status`, `--resume` and `--rollback`
  (`--restore-config`) round it off.
- **Upgrades from the web UI.** Settings → System → Software: the version
  and package, the trusted keys, the last upgrade and its log; choose a
  release file, review, confirm, and follow it through the restart. "Go
  back" returns to the version before. The API is `GET/POST /v1/upgrade`
  and `POST /v1/upgrade/rollback`; through it a package must be signed by
  a trusted key and newer than the one installed.
- **Mixed releases.** Every node warns while the mesh runs different
  releases: a note for a different version of the same series, a warning
  for a different series.
- `meshctl doctor` checks the install (package or scripts, an upgrade left
  unfinished, the release keys).
- Building: `make deb`, `make deb-test` (install, upgrade, remove and purge
  in Debian and Ubuntu containers) and `make release`.

### Security

- **Threat model** ([docs/security.md](docs/security.md)): what Trellis
  protects, from whom, what stops each adversary and what remains, with
  the review of the code against it.
- **Firewall.** `firewall: {wan: closed|open, ssh, allow}` in the node
  configuration, and Settings → Access → "From outside the mesh". Closed,
  a node answers from outside the mesh only SSH, ping, the web UI where it
  is served there, and the ports listed; the mesh, its devices and the
  internet it shares are untouched. Nodes set up by this release start
  closed; nodes upgraded to it stay open until closed, and say so. The
  firewall stays in place while `meshd` is stopped.
- **`meshd` confined.** Its systemd unit bounds it to the capabilities
  networking needs, with `NoNewPrivileges` and a sandbox; the helpers it
  runs inherit them.
- **Fuzz tests** for every parser of untrusted input: gossip records and
  messages, packages and keys, mesh profiles and profile codes, node
  configuration, `regulatory.db`, wpa_supplicant events, DHCP leases,
  versions and CoT. They run with the unit tests; `make fuzz` runs them
  longer. Unpacking a package is bounded in size.

### Mesh-wide profile changes

- **Nodes that missed a change find the mesh again.** Each node remembers
  the other nodes it heard in the last day. One that now hears half of
  them or fewer is in the smaller part of its mesh: it scans every band its
  Wi-Fi radios have for its mesh ID, pays a short visit where it hears it
  (at the width the mesh runs there), and takes the newer profile from the
  nodes there, or gives them its own. A group left behind together moves
  together. The larger part of a mesh never scans, and a mesh of one node
  never does. Not yet for HaLow links.
- **Changes made apart.** Every mesh-wide change now carries a record of
  its own and of the changes before it. When two parts of a mesh that each
  changed the profile while apart meet, the later change is kept on every
  node; the other is kept in a file on the node that settled it, and an
  alert says how to send it again.
- **Switching on time whatever the clocks say.** An offer gives the switch
  as a delay as well as a time of day, and nodes go by the delay. A node
  that restarts with a change due makes it at once when its clock cannot
  be believed: not yet synchronised (no real-time clock), or gone back.
- The review before sending names the nodes heard in the last day that are
  not in reach now and will miss the change. Settings → Mesh profile
  suggests sending changes when every node is on and in reach, from one
  place at a time.

### Mesh and radios

- **Broadcasts right after a restart.** batman-adv numbers a new `bat0`'s
  broadcasts from the start again, and other nodes dropped them as repeats
  for seconds to minutes; `meshd` now uses up the old numbers with a short
  warm-up that never reaches a device.
- **Bridge loop avoidance only where it is needed.** Unset, it is on only
  on a node that bridges wired client ports: elsewhere it protected
  nothing and held every broadcast back for about 30 seconds after each
  start. An explicit `true` or `false` still applies mesh-wide.
- **Peers that restart.** A peer that restarts is no longer taken for one
  that moved channel, and a peering that only one side holds is reset.
- Times in the status (uptime, "since") stay right after the clock is
  corrected at boot on nodes without a real-time clock.
- The channel survey and the search for the mesh share the radios without
  scanning over each other.

### Web UI

- Settings → System → Software (upgrades, above), and Settings → Access →
  "From outside the mesh" (the firewall).
- **Dismissable unrestricted-mode banner.** It shrinks, in that browser, to
  a triangle marked U in the corner that shows the warning on hover or a
  tap; it returns the next time the mesh enters unrestricted mode.
- **Ignore an alert** on the Alerts page: it leaves the overview and the
  alert count in that browser, stays listed under "Ignored", and shows
  again if it turns more serious or comes back after an hour gone.
- A configuration change says devices must rejoin the Wi-Fi only when it
  changes the Wi-Fi.

### Tools and development

- `make deploy` builds and installs the packages; a node reached over the
  mesh that drops its session during the update gets its outcome read
  back; a failed update starts `meshd` again rather than leave the node
  off the mesh. A development key in `dev_key.pub` (ignored by git) is
  trusted by the nodes a deploy updates, and `TRELLIS_DEV_KEY` signs the
  deploy's packages with it, for trying upgrades; a key built into Trellis
  is refused there.
- The DKMS script leaves a loaded batman-adv in place while a mesh uses
  it, removes old releases only after the new one builds, and handles
  kernel names with `+`.
- Lab: broadcasts after a restart, the firewall, and `LAB_BLA` for bridge
  loop avoidance. `make ui-dev` takes `UNRESTRICTED=1`.
- Groundwork for upgrading a whole mesh from one node: the order nodes go
  in, whose turn it is, and moving packages between nodes. Not usable
  yet.
