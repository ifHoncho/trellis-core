# Building

Requirements: Go 1.27 or newer, `make`, and for `make gen` the kernel uapi
headers (`/usr/include/linux/nl80211.h`, `batman_adv.h`) plus Python 3.

```sh
make            # bin/trellis, with bin/meshd and bin/meshctl symlinks
make cross      # dist/trellis-linux-{arm64,armv7,amd64}
make check      # gofmt, go vet, unit tests
```

Binaries are static (`CGO_ENABLED=0`), stripped, about 9 MB. The version
string comes from `git describe`; override with `make VERSION=x.y.z`.

## Deploying to test nodes

One command from the development machine, after a one-time setup:

```sh
cp deploy.hosts.example deploy.hosts     # one user@host per line (git ignores it)
scripts/deploy.sh --setup                # once per node: SSH key + no sudo password prompt
make deploy                              # check, build, update every node, show status
make nodes                               # just show status from every node
make deploy HOSTS=user@192.0.2.11      # one node only
scripts/deploy.sh --no-check             # skip tests when iterating quickly
# a node that may be off your network: give its mesh address in deploy.hosts
#   user@192.0.2.13 mesh=10.73.89.1
# and it is reached over the mesh, through a node that answers, when its LAN
# address does not; or, for every node at once:
TRELLIS_SSH_JUMP=user@192.0.2.10 scripts/deploy.sh user@10.73.129.1
```

Nodes need `wpasupplicant`, `iw`, `batctl`, `hostapd`, `dnsmasq-base` (the
binary only: the `dnsmasq` package starts a system DNS server on port 53 that
blocks Trellis's own) and `nftables`; `install-node.sh` installs whatever is
missing. `meshctl ap` prints the Wi-Fi join QR code in the terminal, and
`meshctl profile share` the mesh profile's; neither needs another package.

`make deploy` runs gofmt, vet and the unit tests, builds the binary for each
node's architecture (asked over SSH), syncs only the binary, `scripts/` and
`packaging/` to `~/trellis-deploy/` on each node, and runs
`scripts/node-update.sh` there with sudo. That script does every node-side
step in the right order and skips what is already done: stop meshd,
install the binary, unit and udev rule, load (or if needed build) the pinned
batman-adv, install the fallback regulatory database only if the kernel
rejects the current one, start meshd and print `meshctl status`. The version
string (`git describe` plus a timestamp) shows which build a node runs.

### Changing the mesh profile (band, channel)

Every node in a mesh must use the same profile. `mesh-profile.yaml` on the
development machine is the copy deploys work from.

```sh
make band BAND=2g                 # 2.4 GHz, channel 6
make band BAND=5g                 # 5 GHz, channel 149
make band BAND=5g CHANNEL=36      # any valid channel
make profile                      # push mesh-profile.yaml as it is (after editing it by hand)
```

`make band` rewrites `mesh-profile.yaml` (`meshctl profile band`), then pushes
it to every node in `deploy.hosts` (`scripts/deploy.sh --profile`). Each node
validates it, installs it (`meshctl profile install`), and meshd applies it
live: wpa_supplicant restarts on the new channel and the mesh radio is
re-chosen for the new band. No build, no service restart; it then shows each
node's mesh link. An invalid profile is refused and the node keeps its old
one. Nodes switch one after another, so the mesh is split for a few seconds;
a node that was offline keeps the old band until the next `make profile`.
New nodes get the current `mesh-profile.yaml` on their first deploy.

### Internet gateways

Every node with an uplink of its own shares it with the mesh by default
(architecture §8): the interface of its default route, probed every second.
That includes bench nodes reached over their LAN or Wi-Fi. In `node.yaml`:

```yaml
gateway:
  mode: auto            # or off: never share, still use other gateways
  wan: eth0             # optional; default: the default route's interface
  probe_targets: [1.1.1.1, 8.8.8.8, 9.9.9.9]
  down_mbit: 20         # announced capacity
  up_mbit: 5
```

`meshctl status` shows the `Internet:` line: this node's WAN, the gateway in
use, and every gateway batman-adv knows, with the reason any is unusable.
Clients reach the internet through the gateway; the node's own traffic still
uses its system routing (meshd never changes the main table).

### The web UI

`make deploy` also installs the web UI (`trellis-webd.socket` and
`.service`): systemd holds ports 443 and 80 and starts `webd` on the first
connection. See [../api.md](../api.md) for where it answers and how it is
protected.

```sh
make web-password              # once: the same admin password on every node
make web-wan WAN=on            # bench: also serve the UI on each node's LAN address
sudo meshctl web               # on a node: where its UI answers, and whether it has a password
```

- Without `make web-wan WAN=on` the UI answers only from the mesh and from
  a node's own Wi-Fi and device ports (`https://10.73.0.1`, or
  `https://node.mesh` where the device uses the node for name lookups; a
  phone with Private DNS, secure DNS or a VPN does not). With it,
  `https://<the node's LAN address>/` works from the development machine.
- The certificate is the node's own, so a browser warns once per node.
- A node with no mesh profile comes up in setup mode and serves the setup
  wizard: join its Wi-Fi `trellis-setup-XXXX` (passphrase `trellis-setup`)
  or plug into one of its Ethernet ports, and open `https://10.73.0.1`.

To work on the UI itself without a mesh: `make ui-dev` (made-up data on
`http://127.0.0.1:8480`, password `trellis-demo`; `make ui-dev SETUP=1` for
the setup wizard, `LINKS=2` for a node with a HaLow radio and a second mesh
link, `LINKS=3` for a third link on 2.4 GHz, `BARE=1` for a
node with no per-radio rules, as freshly installed, `ONE_RADIO=1` for a node
with one dual-band adapter). The UI's files are in `internal/webd/ui/` and
are built into the binary, so run it again after editing them.

`tools/uishot.py` drives a headless Firefox against it and saves
screenshots (`tools/uishot.py go:#/mesh wait:3 shot:mesh`). It reports a UI
that does not start (a module that fails to load, with the module's error)
and any JavaScript error seen while its steps ran, and then exits 1, so it
can check a change as well as picture it.

### Bringing up a new node

1. Flash the OS, enable SSH, note the node's address and user.
2. Add `user@address` to `deploy.hosts`, then `scripts/deploy.sh --setup user@address`
   (asks for the node's SSH password and sudo password once).
3. Once per development machine, fetch the mesh profile from a running node:
   `ssh user@192.0.2.11 sudo meshctl profile show > mesh-profile.yaml`
   (git ignores this file; it holds the mesh secret).
4. `make deploy`. On the new node this installs the helpers, kernel headers,
   the matching compiler and the pinned batman-adv (several minutes on a
   small board), Trellis's regulatory rules, the mesh profile, and a
   `node.yaml` with the hostname as callsign; then starts meshd.
5. Pin a unique `node_id` in `/etc/trellis/node.yaml` and `sudo meshctl reload`.

An existing `/etc/trellis/profile.yaml` or `node.yaml` is never overwritten;
a profile that differs from `mesh-profile.yaml` is reported, because nodes
with different profiles cannot join each other.

## Installing on a node by hand

Copy the binary for the node's architecture (`arm64` for most current
single-board computers on a 64-bit OS) and the repo's `scripts/` and `packaging/`
directories, then as root:

```sh
sudo scripts/install-batman-adv-dkms.sh     # pinned batman-adv 2026.3 via DKMS
sudo scripts/install-node.sh dist/trellis-linux-arm64
meshctl doctor
sudo meshctl profile new --name alpha -o /etc/trellis/profile.yaml   # once per mesh; copy to every node
sudo meshctl node new --callsign node-1 -o /etc/trellis/node.yaml
sudo systemctl enable --now trellis-meshd
sudo meshctl status
```

`install-node.sh` never overwrites an existing configuration.

## Running by hand

```sh
sudo bin/trellis meshd --node /etc/trellis/node.yaml --log-level debug
bin/meshctl status --watch
```

`meshd` tears down everything it created on SIGTERM or Ctrl-C, and reloads
configuration on SIGHUP (`meshctl reload`). An invalid file is rejected with
the offending fields named, and the running configuration is kept.
