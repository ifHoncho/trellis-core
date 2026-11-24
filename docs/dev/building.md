# Building

Requirements: Go 1.27 or newer, `make`, and for `make gen` the kernel uapi
headers (`/usr/include/linux/nl80211.h`, `batman_adv.h`) plus Python 3.

```sh
make            # bin/trellis, with bin/meshd and bin/meshctl symlinks
make cross      # dist/trellis-linux-{arm64,armv7,amd64}
make deb        # dist/deb/: trellis_VERSION_{arm64,armhf,amd64}.deb (voice in arm64 and amd64), trellis-batman-adv-dkms
make check      # gofmt, go vet, the static-binary check, unit tests
```

Binaries are static (`CGO_ENABLED=0`), stripped, about 12 MB. The version
string comes from `git describe`; override with `make VERSION=x.y.z`. A
release is a tag named `vX.Y.Z`, and its version is `X.Y.Z`.

## Packages and releases

`make deb` needs nothing but Go: `tools/mkdeb` writes the packages itself.
The first run downloads the pinned batman-adv release
(`packaging/dkms/source` names it and its checksum) into `dist/cache/`.

| Package | Contents |
|---|---|
| `trellis` | `/usr/bin/trellis` (and `meshd`, `meshctl`, `webd`), the systemd units, udev rule, tmpfiles and chrony drop-in, and under `/usr/share/trellis` the scripts for the radios Trellis supports. Its install scripts are `packaging/deb/trellis.*` |
| `trellis-batman-adv-dkms` | the batman-adv source under `/usr/src`; installing it builds the module with DKMS (`packaging/deb/dkms.*`) |

Every file in a package carries the last commit's time, so the same commit
gives the same packages. `make deb-test` installs, upgrades, removes and
purges them in Debian and Ubuntu containers
([testing.md](testing.md#packages-in-containers)).

**Signing.** A release key is an Ed25519 key pair:

```sh
go run ./tools/mkdeb keygen --out ~/keys/trellis-release.key --comment "who holds it"
```

It prints the public line. Put that line in
`internal/release/trusted.keys` and commit it: nodes trust the keys built
into the version they run, so the key must be in a release before it can
sign the next one. Keep the secret file off the network.

**A release**, from start to finish:

1. Write its section in `CHANGELOG.md`, headed `## X.Y.Z: …`, above the
   previous release's, and commit. The release refuses to build without
   one: it becomes the notes of the release page.
2. Tag the commit and push the tag:

   ```sh
   git tag -s v0.4.0 -m "Trellis 0.4.0"
   git push origin v0.4.0
   git describe --tags --dirty      # exactly v0.4.0: tagged, nothing modified
   ```

3. Build and sign:

   ```sh
   TRELLIS_RELEASE_KEY=~/keys/trellis-release.key GPG_KEY=<gpg key id> make release
   ```

   It writes `dist/release/0.4.0/`: the signed packages, `trellis-deploy`
   for Linux, macOS and Windows (`make deploy-tool`), the public key, `SHA256SUMS`
   over all of them and, with `GPG_KEY`, `SHA256SUMS.asc`. It refuses an
   untagged or modified tree, a version with no changelog section, and a
   key that is not in `trusted.keys`; `ALLOW_DEV=1` and `UNSIGNED=1` make
   a trial run. `go run ./tools/mkdeb verify --pub KEYS FILE.deb` checks a
   package by hand.
4. Try it: `sudo meshctl upgrade dist/release/0.4.0/trellis_0.4.0_<arch>.deb`
   on a test node.
5. Publish it on GitHub (the GitHub CLI, logged in once with `gh auth
   login`):

   ```sh
   DRY_RUN=1 make publish     # the checks, the notes, the command; nothing published
   make publish
   ```

   It checks that the tag is on GitHub at this commit, that every package
   is signed by a key in `trusted.keys`, the checksums and their gpg
   signature, and that the release is not published already; then it
   publishes the files with the changelog section as the notes. While the
   major number is 0 a release is a pre-release titled "(alpha)";
   `STAGE=beta` names another stage, `STAGE=final` publishes a full
   release. `scripts/release-notes.sh 0.4.0` prints the notes alone.

6. Build its images (below).

**Images.** A board's image is Raspberry Pi OS Lite with the release's
packages, the board's drivers built for its kernel, and its overlays and
boot settings. Each board is one file in `packaging/image/boards/`, which
also pins the base image by SHA-256. The image holds no mesh profile, no
node identity and no BCF: those go on the card when it is flashed, in a
folder `trellis/` on the boot partition (`profile.yaml`, `node.yaml`,
`web-admin.yaml`, `authorized_keys`, `password`, `morse/`). At boot, `trellis firstboot`
checks them, installs them and removes them from the card, and writes
what it did to `trellis/firstboot.txt`.

The build runs as root on an arm64 Linux machine, natively (its drivers
are compiled inside the image), with e2fsprogs 1.47 or newer (Ubuntu 24.04,
Debian 13: the image's ext4 uses `orphan_file`), in 30 to 60 minutes:

```sh
sudo make image BOARD=cm4-reference DEBS=dist/release/0.4.0
```

It writes `dist/image/`: the `.img.xz`, its `.sha256` and a manifest of the
image's packages. `IMAGE_XZ=0` leaves the image uncompressed, for a build
host that cannot take a long compression on all its cores. A HaLow
board's image carries Morse's firmware and licence, fetched at the pinned
commit and checked; never the BCF (licensing: it reaches a node from the
card, the node's uplink or another node). For a release, the `image` workflow builds it on
GitHub's arm64 runner (Actions → image → Run workflow, with the tag and
the board). It checks the release's packages against `trusted.keys`,
holds no key, and leaves the image as an artifact for 14 days.

**The apt repository** tooling is kept, though no public repository is
run: releases go out as packages, images and mesh upgrades. A repository
is a folder of static files that any web server can serve as it is. `tools/aptrepo` adds packages to its `pool/`, then writes
`dists/stable/` again from everything the pool holds. It signs `Release`
with a gpg key, the **archive key** (separate from the release key and
from anyone's commit key), and writes the key's public half beside it as
`trellis-archive-keyring.gpg`:

```sh
make apt-repo APT_REPO=~/trellis-apt APT_GPG_KEY=<archive key fingerprint>
```

The key must name exactly one gpg secret key (a name two keys share is
refused); it prints the fingerprint, which goes beside the repository for
people to check. It takes only release packages signed with a key in `trusted.keys`, and
never replaces a published file with different contents. Older versions
stay listed. `make apt-test` builds a repository with throwaway keys and
checks, in Debian and Ubuntu containers, that apt installs and upgrades
from it, and that it refuses the repository when an index is changed or
another key signed it. A machine uses one with the keyring in
`/usr/share/keyrings/` and `deb [signed-by=…] URL stable main`; `apt
upgrade` then installs releases without `meshctl upgrade`'s health check
and rollback.

A node takes a release with `sudo meshctl upgrade FILE.deb`
([reference](../reference.md#upgrading-a-device-by-itself)). To try one that no built-in key signed,
put the public line in `/etc/trellis/release-keys/NAME.pub` on the node, or
pass `--allow-untrusted`.

## Deploying to test nodes

One command from the development machine, after a one-time setup:

```sh
cp deploy.hosts.example deploy.hosts     # one user@host per line (git ignores it)
go run ./tools/mkdeb keygen --out ~/keys/trellis-dev.key --comment dev > dev_key.pub
export TRELLIS_DEV_KEY=~/keys/trellis-dev.key   # keep it set (shell profile)
scripts/deploy.sh --setup                # once per node: your SSH key
scripts/deploy.sh --setup --sudo-nopasswd  # and sudo without a password (see reference.md first)
make deploy                              # check, build, upgrade every node (health-checked)
make deploy FAST=1                       # the same with apt, no health check: quicker
make deploy DRY_RUN=1                    # build and show what it would do; nothing changes
make nodes                               # just show status from every node
make deploy HOSTS=user@192.0.2.11      # one node only
scripts/deploy.sh --no-check             # skip tests when iterating quickly
# a node that may be off your network: give its mesh address in deploy.hosts
#   user@192.0.2.13 mesh=10.73.89.1
# and it is reached over the mesh, through a node that answers, when its LAN
# address does not; or, for every node at once:
TRELLIS_SSH_JUMP=user@192.0.2.10 scripts/deploy.sh user@10.73.129.1
```

Nodes need `wpasupplicant`, `iw`, `hostapd`, `dnsmasq-base` (the binary
only: the `dnsmasq` package starts a system DNS server on port 53 that
blocks Trellis's own), `nftables` and `chrony`; the `trellis` package
depends on them, so `apt` installs whatever is missing. `meshctl ap` prints the Wi-Fi join QR code in the terminal, and
`meshctl profile share` the mesh profile's; neither needs another package.

`make deploy` runs gofmt, vet and the unit tests, asks each node its
architecture, builds the `trellis` package for those (and the batman-adv
package), signed with the development key, and hands them to
`trellis-deploy`, as a user would deploy a release
([reference](../reference.md#trellis-deploy)), with:

- `--trust dev_key.pub --node-key dev_key.pub`: the tool accepts the
  development key, and every node is made to trust it
  (`/etc/trellis/release-keys/deploy-dev.pub`), so `meshctl upgrade`, the
  web UI and the API take these builds there too. A `trellis-deploy` run
  on the built-in release keys alone (no `--trust`, no `--node-key`: a
  real release) removes that file again once the node took the release;
- `--any-version`: development builds do not sort, so a node gets the one
  just built whatever it runs;
- `--profile mesh-profile.yaml` when that file exists: a node without a
  profile gets it;
- `--no-health-check` with `FAST=1`: `apt` instead of `meshctl upgrade`,
  then `meshctl prepare`: no wait for the health check, but nothing goes
  back on its own if the build is broken;
- `--dry-run` with `DRY_RUN=1`: build, check, show what would be done.

Nodes are upgraded one at a time, nodes reached over the mesh first, and
the first one that does not come up healthy stops the rest. The version
string (`git describe` plus a timestamp) shows which build a node runs.
The deploy refuses a development key that is in `trusted.keys`: every
node would take its builds.

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
  web_check: true       # a WAN behind a login page is not shared
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
   (asks for the node's SSH password once; add `--sudo-nopasswd` to stop
   deploys asking for its sudo password, after reading
   [reference.md](../reference.md#passwords-and-sudo)).
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

Copy the packages for the node's architecture (`arm64` for most current
single-board computers on a 64-bit OS), then:

```sh
sudo apt install ./trellis_*_arm64.deb ./trellis-batman-adv-dkms_*_all.deb
sudo meshctl doctor
sudo reboot        # the node comes up in setup mode: the guide's step 2
```

or, instead of the setup page:

```sh
sudo meshctl profile new --name alpha -o /etc/trellis/profile.yaml   # once per mesh; copy to every node
sudo meshctl node new --callsign node-1 -o /etc/trellis/node.yaml
sudo systemctl restart trellis-meshd
sudo meshctl status
```

The packages never overwrite an existing configuration. Where the
batman-adv module could not be built at install (no kernel headers yet),
`sudo /usr/share/trellis/scripts/install-batman-adv-dkms.sh` fetches the
headers and builds it.

## Running by hand

```sh
sudo bin/trellis meshd --node /etc/trellis/node.yaml --log-level debug
bin/meshctl status --watch
```

`meshd` tears down everything it created on SIGTERM or Ctrl-C, and reloads
configuration on SIGHUP (`meshctl reload`). An invalid file is rejected with
the offending fields named, and the running configuration is kept.
