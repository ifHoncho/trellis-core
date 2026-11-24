# Reference

Options and behaviour in full. Start with the [guide](guide.md).

- [trellis-deploy](#trellis-deploy)
- [Passwords and sudo](#passwords-and-sudo)
- [Installing from an image](#installing-from-an-image)
- [Installing one device by hand](#installing-one-device-by-hand)
- [Setting a device up from a phone](#setting-a-device-up-from-a-phone)
- [The operator account](#the-operator-account)
- [Setting up a card](#setting-up-a-card)
- [Changing the whole mesh](#changing-the-whole-mesh)
- [Radios and mesh links](#radios-and-mesh-links)
- [Sharing an uplink](#sharing-an-uplink)
- [Linking sites over the internet](#linking-sites-over-the-internet)
- [Access from outside the mesh](#access-from-outside-the-mesh)
- [Regulatory modes](#regulatory-modes)
- [OS updates](#os-updates)
- [HaLow firmware and BCF](#halow-firmware-and-bcf)
- [Upgrading a device by itself](#upgrading-a-device-by-itself)
- [Upgrading the whole mesh from one node](#upgrading-the-whole-mesh-from-one-node)
- [The web UI](#the-web-ui)
- [Push-to-talk voice](#push-to-talk-voice)
- [Trying a new driver, firmware or board file](#trying-a-new-driver-firmware-or-board-file)
- [Command line (meshctl)](#command-line-meshctl)
- [Configuration files](#configuration-files)
- [When something is wrong](#when-something-is-wrong)

## trellis-deploy

### The devices file

One device per line; `#` starts a comment.

```
pi@192.0.2.11                            user@address, as ssh takes it
pi@192.0.2.12  callsign=alpha-2 id=12    the callsign and node ID it gets
pi@192.0.2.13  port=2222                 its SSH port, when not 22 (on its mesh address too)
pi@192.0.2.14  mesh=10.73.14.1           its mesh address (see below)
```

- `callsign` and `id` apply to devices installed by the run. Defaults:
  callsign from the hostname, ID chosen by the node. Unique per file.
- `mesh`: the node's mesh address (listed by `meshctl status`). Used when
  the LAN address does not respond (node not wired to the LAN), via the
  first responding device.

### What a run does

1. **Fetch and verify the release** (latest; `--release 0.2.0`, or
   `--from DIR`): checksums, and each package signature against the
   release keys built into `trellis-deploy`. Any failure stops the run.
2. **Plan per device**: install, upgrade, no change ("up to date"), or
   skip with the reason (no response, not Debian-family, no package for
   the architecture, newer version installed). Lists devices whose `sudo`
   needs a password, then one confirmation (`--yes`: none; `--dry-run`:
   stop here).
3. **New devices**, in parallel (`--jobs`, default 4): packages, `meshctl
   prepare`, mesh profile (`--profile`; an existing profile is kept),
   callsign and node ID, admin password (prompted once), reboot, wait for
   Trellis (`--no-reboot` skips the reboot).
4. **Upgrades**, one device at a time with `meshctl upgrade`: each
   self-tests and rolls back on failure; the first rollback halts the run
   (`--keep-going` overrides). Devices reached over the mesh go first.
   Connections dropped during a mesh restart are re-established and the
   result queried.
5. **Summary**: result per device, `meshctl doctor` findings, client AP
   SSID and passphrase of new devices. Full output per device in
   `trellis-deploy-logs/<time>/`; passwords and the mesh secret are never
   logged.

### Options

| Option | |
|---|---|
| `--setup` | Only put your SSH key on each device it does not log in to yet (`ssh-copy-id` asks for that device's login password; on Windows, which has none, `trellis-deploy` sends the key over `ssh` itself); `--ssh-key FILE.pub` chooses the key (default without `ssh-copy-id`: `id_ed25519.pub`, `id_ecdsa.pub` or `id_rsa.pub`) |
| `--setup --sudo-nopasswd` | Also let the login user use `sudo` without a password: see [below](#passwords-and-sudo) |
| `--new-mesh NAME --country CC` | Make a new mesh's profile with a fresh secret, in `mesh.yaml` (`--out` for another file; an existing one is never overwritten). `--band 5g` for 5 GHz instead of 2.4 GHz |
| `--profile FILE` | The mesh profile for devices that have none |
| `--release X.Y.Z`, `--from DIR` | Another release, or release files you have |
| `--dry-run`, `--yes` | Only show the plan; do not ask before going ahead |
| `--web-password`, `--no-web-password` | Ask for the web UI's admin password even with `--yes`; never ask |
| `--jobs N`, `--no-reboot`, `--keep-going`, `--force` | New installs at once; no reboot after them (the admin password is then set later, on the node, with `sudo meshctl web password`); go on after an upgrade goes back; reinstall devices that run the release already |
| `--ssh-option Name=value` | An option for every ssh connection (repeatable), as `IdentityFile=~/.ssh/other`; yours override the tool's defaults. On Windows, for a device reached through another, the Windows command shell replaces `%NAME%` in a value when NAME is an environment variable |
| `--logs DIR` | Where the logs go |
| `--repo OWNER/NAME` | The GitHub repository releases are downloaded from (default `ifHoncho/trellis-core`) |
| `--trust`, `--node-key`, `--any-version`, `--no-health-check` | For development builds ([building.md](dev/building.md)) |

SSH host keys: accepted on first contact, refused if changed
(`StrictHostKeyChecking=accept-new`). `--ssh-option
StrictHostKeyChecking=yes` requires keys already in `known_hosts`. An
existing mesh profile is kept; the run warns if it differs from
`--profile`.

## Passwords and sudo

`trellis-deploy` logs in with your SSH key and runs as root via `sudo`.
Where `sudo` needs a password, the run prompts once, tries it on every
such device, and prompts again by name only where it fails (Enter skips
the device). Passwords are held for the run only.

`--setup --sudo-nopasswd` installs `/etc/sudoers.d/trellis-deploy` on
each device: passwordless `sudo` for the login user. Security impact:
**any login as that user is root without further authentication**; the
SSH key and the machine holding it become the only barrier. In order of
preference:

1. the password prompt (default; a few seconds per run);
2. with the rule: an SSH key passphrase, unlocked in `ssh-agent` for the
   session;
3. the rule only on physically controlled devices, removed afterwards
   (`sudo rm /etc/sudoers.d/trellis-deploy`).

A narrower rule is no safer: package installation is root regardless of
the command named.

## Installing from an image

A release includes disk images for some boards. An image is the board's
operating system with Trellis installed and the radio drivers built for
its kernel. A node written from an image needs no further installation.

Each image is three files on the release:

| File | |
|---|---|
| `trellis-X.Y.Z-BOARD.img.xz` | The image, compressed with xz |
| `trellis-X.Y.Z-BOARD.img.xz.sha256` | Its SHA-256 checksum |
| `trellis-X.Y.Z-BOARD.manifest.txt` | The board it is built for (first line), and its packages with their versions |

### Images

| Image (`BOARD`) | Board | Storage | Radios and GPS |
|---|---|---|---|
| `cm4-reference` | Raspberry Pi Compute Module 4 (CM4) | microSD card (CM4 Lite) or eMMC, 4 GB or more | MediaTek MT7915 or MT7916 (PCIe, M.2); Seeed Wio-WM6108 HaLow in a WM1302 Pi HAT (SPI); GPS receiver on the primary UART |

The `cm4-reference` image turns Bluetooth and the serial console off. Its
GPS receiver uses the primary UART.

Every image differs from a standard installation of its operating system:

- SSH is on. Each node generates its own SSH host keys at first boot.
- The kernel packages are held (`apt-mark showhold` lists them).
- The image has no login account. The setup wizard creates the
  [operator account](#the-operator-account).

**Requirements:**

- A board from the table above.
- Storage for the image, as the table gives.
- A computer with an image writer, and a card reader for a microSD card.
- A phone or laptop with Wi-Fi, for setup.

**Procedure:**

1. From the newest
   [release](https://github.com/ifHoncho/trellis-core/releases), download
   the board's `trellis-X.Y.Z-BOARD.img.xz` and
   `trellis-X.Y.Z-BOARD.img.xz.sha256` into one directory.
2. In that directory, verify the image:

   ```sh
   sha256sum -c trellis-X.Y.Z-BOARD.img.xz.sha256        # Linux
   shasum -a 256 -c trellis-X.Y.Z-BOARD.img.xz.sha256    # macOS
   ```

   The command prints `trellis-X.Y.Z-BOARD.img.xz: OK`. On Windows, run
   `Get-FileHash trellis-X.Y.Z-BOARD.img.xz` in PowerShell and compare the
   hash with the first field of the `.sha256` file. Letter case does not
   matter. If the check fails, download the image again.
3. For a board that boots from eMMC, connect the eMMC to the computer as a
   USB mass storage device, as the board's maker documents. For a
   Raspberry Pi Compute Module, the procedure uses `rpiboot`:
   [Flash an image to a Compute Module](https://www.raspberrypi.com/documentation/computers/compute-module.html#flash-compute-module-emmc).
4. Write the image to the microSD card or the eMMC with an image writer,
   for example Raspberry Pi Imager (**Use custom**) or, on Linux,
   Impression. An image writer that does not read `.xz` files needs the
   image decompressed first: `xz -dk trellis-X.Y.Z-BOARD.img.xz`.
5. Do not apply the image writer's OS customisation (hostname, user
   account, Wi-Fi, SSH keys). The setup wizard sets the node's callsign,
   operator account and SSH keys.
6. For a microSD card, insert the card in the board. For eMMC, disconnect
   the USB cable and set the board back to eMMC boot.
7. Power the node on.

   The node has no mesh profile and starts in setup mode. Its setup AP,
   `trellis-setup-XXXX`, appears.
8. Set the node up from a phone or laptop:
   [Setting a device up from a phone](#setting-a-device-up-from-a-phone).

To set a node up from files on the card instead of from a phone, see
[Setting up a card](#setting-up-a-card).

## Installing one device by hand

Requirements: a device and radios per [hardware.md](hardware.md); Debian,
Ubuntu or Raspberry Pi OS; two packages from a release: `trellis` for the
architecture (`arm64` for most current boards on a 64-bit OS, `armhf` for
32-bit, `amd64` for PCs) and `trellis-batman-adv-dkms`. To build them:
`make deb` (output in `dist/deb/`). On the device:

```sh
sudo apt install ./trellis_*_arm64.deb ./trellis-batman-adv-dkms_*_all.deb
sudo meshctl prepare
sudo reboot
```

`apt` installs the dependencies (wpa_supplicant, hostapd, dnsmasq,
nftables, chrony). `meshctl prepare` then completes the device, skipping
steps already done:

- builds and loads the pinned batman-adv release (DKMS rebuilds it after
  each kernel update; requires kernel headers, fetched where available);
- builds the MediaTek MT7915/MT7916 driver on kernels that lack it
  (e.g. Raspberry Pi OS) or whose driver predates MT7916 5/6 GHz support
  (before Linux 6.14);
- sets driver options required by some radios for an encrypted mesh;
- hands the kernel's regulatory rules to Trellis.

It finishes with `meshctl doctor` and reports whether a reboot is
required. After the reboot, a device without a mesh profile starts in
setup mode (next section).

Install, upgrade and removal never modify configuration, mesh profile or
identity (`/etc/trellis`, `/var/lib/trellis`); only `apt purge trellis`
deletes them.

## Setting a device up from a phone

A device with no mesh profile starts in **setup mode**: setup AP
`trellis-setup-XXXX` (passphrase `trellis-setup`), setup page on the AP
and on its Ethernet ports.

1. Join `trellis-setup-XXXX` and open **https://10.73.0.1**. Accept the
   self-signed certificate.
2. Set the web UI **admin password**.
3. **Join a mesh**: scan the profile QR code of a mesh node (Settings →
   Mesh profile → **Share profile**), paste its profile code, or upload its
   profile file. Or **start a new mesh**: name, and band, channel and width
   per link (one link per mesh radio of the device, each on its best band;
   secret generated).
4. **Callsign**.
5. **Access**: the [operator account](#the-operator-account) `trellis`
   (password: the admin password, or another; SSH keys pasted or from a
   GitHub user; **Password authentication**), **Web UI from uplink**
   (tcp/80, tcp/443) and **SSH from uplink** (tcp/22), both on.
6. **Review**: the radio for each mesh link and the client AP, the access
   settings, **OS updates after setup** (on when the uplink is up; see
   [OS updates](#os-updates)), and alerts that need action. Apply. The
   setup AP is replaced by the client AP; its SSID and passphrase are
   shown with a QR code.

A provisioned node without an admin password (installed by
`trellis-deploy` without one) shows "No admin password set". Set it on the
node: `sudo meshctl web password`.

## The operator account

The Linux account `trellis`: SSH login, `sudo` (asks for the account's
password) and `meshctl` without sudo (primary group `trellis`). SSH is
always on. A node installed by `trellis-deploy` has no operator account
until one is set up in Settings → Access → **Operator account**.

- **Set up** in the setup wizard's Access step, from a card
  ([below](#setting-up-a-card)), or in Settings → Access → **Operator
  account**.
- **Password:** 8 to 128 characters. Default in the wizard: the web UI
  admin password.
- **SSH keys:** `/home/trellis/.ssh/authorized_keys`. Added by pasting a
  public key, or by a GitHub user's keys (`github.com/USER.keys`, fetched
  by the node over its uplink). Fingerprints (SHA256) are shown before
  saving. Keys added to the file by hand are kept and shown; other lines
  (comments, certificates) are kept as they are.
- **Password authentication:** On by default. Off: SSH login with a key
  only, for `trellis` alone (`/etc/ssh/sshd_config.d/50-trellis.conf`);
  other accounts unchanged. Can be turned off only when the account has
  a key; removing the last key turns it on.
- A system without `openssh-server` shows SSH server: Not installed.

Login: `ssh trellis@10.73.0.1` from a client, `ssh trellis@CALLSIGN.mesh`,
or the node's uplink address while SSH from uplink is on.

## Setting up a card

A card flashed with a Trellis image takes its setup from the computer
that flashed it: `trellis-deploy --card`, with the card's boot partition
mounted.

```sh
trellis-deploy --card /media/$USER/bootfs --profile mesh.yaml --callsign kit-07 \
  --ssh-key ~/.ssh/id_ed25519.pub --halow
```

| Option | |
|---|---|
| `--card DIR` | the boot partition (it holds `config.txt`) |
| `--profile FILE` | the mesh profile; without it the node starts in setup mode |
| `--callsign NAME` | required |
| `--ssh-key FILE`, `--github USER` | public keys for the operator account |
| `--no-web-uplink`, `--no-ssh-uplink` | access from the uplink network off |
| `--halow`, `--no-halow` | Morse Micro's firmware and BCF for a Wio-WM6108, fetched at the pinned commit and checked; the default when the card's `config.txt` enables the WM6108 overlay |

It asks for one password: the web UI admin's and the operator
account's. The files go in `trellis/` on the card; at first boot the node
installs them and removes them from the card (overwritten first), and
writes what it did to `trellis/firstboot.txt`. A file that fails its check
stays, with the reason in that file. Until then the card holds the mesh
secret.

## Changing the whole mesh

All nodes of a mesh need the same **mesh profile**: links, channels,
secret, regulatory mode. To change it: edit on any node (Settings → Mesh
profile), select **All nodes**. The node sends the change to all others,
shows delivery per node, and all nodes switch over together (default: 1
min later). **This node only**: single-node testing.

- The review lists nodes that cannot follow (no radio for the new links,
  older version), nodes whose regulatory mode would change, and nodes off
  or out of range that will miss the change.
- Out-of-range nodes take the change on next contact with a node that has
  it, if they still share a link.
- Nodes off during the change come back on the old channel and find few
  peers. After 1 min they scan their radios' other channels and bands,
  visit where the mesh is heard (shown in Alerts) and take the new profile
  there. Not possible on HaLow: a HaLow-only node needs the profile
  reinstalled manually.
- Partitioned groups that both change the mesh reconcile on merge: the
  later change wins everywhere; the node that detects the conflict saves
  the other to a file and raises an alert, so it can be resent.

Send changes with all nodes up and in range, from one location at a time.

No change silently overrides another:

- While a change is pending, every page shows it, and no second mesh-wide
  change is sent before switchover (it would supersede the first).
- A pending change can be corrected before switchover. Banner →
  **Replace…** opens the editors on the pending profile; edit and send to
  supersede it. **Cancel…** sends the current profile instead: no change
  at switchover. CLI: `meshctl profile push --replace FILE` (the mesh's
  current profile file cancels).
- A web UI change based on a profile or node settings changed on the node
  since the page loaded them (another session, another node, `meshctl`)
  is refused; the page reloads and says so. Unsaved edits stay on the page.
- `meshctl profile edit` opens the current mesh profile in `$EDITOR`,
  shows the impact, and sends on confirmation. Refused if the profile
  changed on the node during editing; edits are saved to a file.
- `meshctl profile push FILE` sends the file as is. The review flags a
  file older than the mesh's current profile (start from `meshctl profile
  edit` or `meshctl profile show`); refused while a change is pending
  unless `--replace` is given.

## Radios and mesh links

A mesh profile can define several **links**, each on its own band and
channel (e.g. 5 GHz for throughput, 2.4 GHz and HaLow for range). Each
node runs every link it has a radio for, concurrently; routing takes the
fastest path per destination. Nodes with different radios mesh over the
links they share.

- One link per radio. A dual-band USB adapter: one band at a time. A card
  with two radios: two links.
- A node with fewer radios than links takes links in profile order: put
  the link most nodes should use first.
- **Radios** page: pin a link to a radio, disable a link on one node, or
  ignore a radio. "Automatic": node decides.
- **Channel load:** Settings → Channels shows channel utilization per
  link per node. **Survey channels**: all nodes scan their link bands for
  a few seconds (receive only). Where a channel is clearly better, one
  button moves the link on all nodes (mesh-wide change).
- **Channel busy alert:** a link whose channel stays at 60 % busy or more
  at any node for 10 minutes (dips that stay above 50 % do not break the
  count) raises `channel-busy`, with the node and figure and a link to
  the survey; it clears after 2 minutes under 50 %, or with no node
  measuring. The figure includes the mesh's own traffic: a link carrying
  heavy traffic can raise it on a clean channel (the survey then shows
  no better one). Nothing moves by itself.
- **Off-channel guard:** a mesh radio follows a peer's channel switch
  announcement in the kernel (a peer that hears radar moves the mesh
  this way; a faulty driver can announce one it never makes). A link
  left on another channel than the profile's for 30 seconds raises
  `mesh-off-channel` and is restarted on its own channel, at most once
  in 10 minutes per link. On a radar (DFS) channel plan it is flagged
  only, not moved back. Trellis announces no switch itself: a mesh-wide
  channel change restarts the link on every node at the same moment
  (seconds without it).
- Higher bands: more throughput, less range. A link that is established
  but cannot carry traffic (one end too weak) raises an alert: reduce
  distance, use a narrower width, or use a longer-range band.
- **Width:** wider channels: higher throughput at short range, less
  range. Noise rises (+6 dB at 80 MHz vs 20 MHz) and Tx power drops.
  Where the hardware database has per-width power, the Radios page and the
  profile editor show the link budget for this node's radio: MT7916 at
  20 dBm (20 MHz) and 13.5 dBm (80 MHz), so 80 MHz costs 12.5 dB (about a
  quarter of the open-field range). Both ends run one width: size it for
  the link's longest hop.
- **Temperature:** radios with a driver sensor show temperature on the
  Radios page. Thermal throttling raises an alert; the Mesh page shows
  hot nodes.
- **PCIe radios:** the Radios page shows the trained link (Gen, lanes)
  and error state. "Marginal": the card and slot (or adapter) corrupt
  data and retransmit; under load the card can hang and the node resets.
  Reseat the card, or change adapter or slot.
- A radio carrying a link is exclusive to Trellis: meshd removes other
  interfaces on it (e.g. `p2p` or `wlan` created by a network manager),
  except **monitor** interfaces on the link's channel. Capture: `sudo iw
  phy PHY interface add mon0 type monitor && sudo ip link set mon0 up`,
  then `tcpdump -i mon0`.

## Sharing an uplink

A node with a local uplink (default route interface, a port with role
`wan`, or `gateway.wan`) acts as a gateway (`gateway.mode: auto`; `off`:
never shares, still uses other gateways). Two checks, both via the uplink,
decide sharing:

- **Ping** to `gateway.probe_targets` (default `1.1.1.1`, `8.8.8.8`,
  `9.9.9.9`) every `gateway.probe_interval_ms` (1000). 3 missed rounds:
  uplink withdrawn; 3 good rounds: restored.
- **Web check** (`gateway.web_check`, default on): HTTP request to each
  `gateway.web_check_urls` for the empty "204" response used for
  captive-portal detection. Defaults: Google, Cloudflare and Ubuntu
  connectivity-check URLs. Runs every minute and on ping state changes.
  Any other response (redirect, page): **captive portal**.

An uplink behind a captive portal is not shared, even if ping succeeds.
While no other gateway is available, local clients still use it, so the
portal can be reached from any `http://` page and signed in. The check
then runs every 10 s; sharing starts automatically when it passes. The
Internet page and `meshctl status` show "captive portal" with the
response seen. If no web check URL responds (blocked), ping alone
decides. `gateway.web_check: false` disables the web check.

Requests carry no cookies or node details; the checked sites see the
uplink's public address, as do the ping targets.

## Linking sites over the internet

Joins parts of one mesh at different locations, each with internet, into
one mesh: one client network, DNS names, gateways, ATAK and voice. All
parts share one mesh profile. One or two uplink-equipped nodes per site
carry the link (members); two provide redundancy.

1. Get each member's key: `meshctl site`, or Internet → Site link in the
   web UI. `meshctl site` prints the profile line to add.
2. Add members to the profile: Internet → Members (web UI), or the file.
   The web UI adds this node and heard nodes by name; nodes at sites not
   yet linked are added by key.

   ```yaml
   site_link:
     members:
       - {name: hq-1, site: hq, public_key: "…", endpoint: "hq.example.org:51873"}
       - {name: hq-2, site: hq, public_key: "…", endpoint: "203.0.113.7:51873"}
       - {name: field-1, site: field, public_key: "…"}
   ```

   `endpoint`: public address or name and the forwarded UDP port. Omit for
   members behind NAT (outbound only); their peers then need an endpoint.
   Members link to the members of other sites.
3. Send the profile to all nodes at one site (**All nodes**, or `meshctl
   profile push`).
4. At each other site, install the same profile on one node: `meshctl
   profile show > p.yaml` on a node that has it, then `meshctl profile
   install p.yaml` there. The rest of that site takes it from that node.
   (Pushing again at the second site creates a second change; reconciled
   when the sites connect, with an alert.)
5. Verify: `meshctl site` on a member, or Internet → Site link: each peer
   with last handshake and tunnel state. `meshctl status` lists the nodes
   at other sites. A down tunnel states the cause: no endpoint at either
   end, unresolvable name, no local uplink, or no response.

To remove a member, or the whole link: remove from the list and resend
the profile the same way (from one node while the link is up, or per
site). Peers stay listed on a member while the profile lists them.

The review flags sites that cannot connect (no endpoint at either), and
members at different sites that are already connected without a link
(same location: use one site name; members of one site do not link).

| Setting | Default | |
|---|---|---|
| `site_link.port` | 51873 | UDP listen port on every member; opened automatically when the firewall is closed |
| `site_link.mbit` | 10 | Throughput figure batman-adv uses to rank a tunnel; not a limit or measurement. Leave as is: higher makes gateways at other sites compete with local ones. Also sets a hop penalty so local traffic never transits another site |
| `site_link.mtu` | 1420 | WireGuard MTU; inner tunnels carry 38 bytes less. Larger frames pass: batman-adv fragments unicast; broadcast is fragmented inside WireGuard |
| `site_link.multicast_kbit` | 1000 | Broadcast and multicast per tunnel per direction (kbit/s); excess dropped at the tunnel |

- Tunnels use the member's own uplink only, never the mesh. A member
  without an uplink retries until it has one.
- Members require `nft` (`nftables`, installed with Trellis): traffic
  addressed as tunnel traffic that did not arrive via WireGuard (from the
  uplink network or the client network) is dropped. `meshctl site` shows
  the count when non-zero.
- A profile is passed between nodes whole, in one ~8 kB message. Member
  lists too long for that (about 40 members with names and endpoints) are
  refused at review, before sending.
- While a reachable member reports a site link tunnel down that was up in
  the last day, nodes do not scan other channels for the missing part of
  the mesh (see [Changing the whole mesh](#changing-the-whole-mesh)): the
  missing nodes are behind the link; alert raised. Nodes that hear no
  member still scan; tunnels never established do not count.
- `meshctl site --new-key`, or **New key…** on the Internet page:
  generates a new node key (if compromised). The member drops off the link
  until the profile lists the new key; the web UI adds it to the member
  list, ready to send.
- Link state is visible on every node, member or not: `meshctl site`
  lists every member with its reported tunnels; also on the Overview and
  Internet pages.
- Broadcast, and multicast flooded by batman-adv, reaches every node at
  other sites, up to `site_link.multicast_kbit` per tunnel: sufficient for
  ATAK and voice, not video. Multicast to few subscribers goes as unicast
  copies (not counted against the budget).
- Releases before 0.3.0 cannot read a profile with a site link and refuse
  it: upgrade all nodes first.

## Access from outside the mesh

Settings → Access → **Firewall** (`firewall.*` in [node.yaml](#configuration-files)): nftables
table `inet trellis_fw`. `lo`, `br-mesh` and `anycast0` (mesh, client AP,
client ports) are trusted and not filtered; rules apply to all other
interfaces: uplink, VPN, container links.

**Drop new connections from filtered interfaces** on (`firewall.wan:
closed`; default for nodes set up by Trellis):

- **Input:** accept established/related, ICMP/ICMPv6, DHCP replies
  (udp/68, udp/546), SSH (tcp/22, unless `firewall.ssh: false`; max. 10
  new connections/min per address, burst 10), web UI (tcp/80, tcp/443)
  while **Web UI from uplink** is on, and **Also accept**
  ports (`firewall.allow`, e.g. `tcp/5201`); drop all else.
- **Forward:** new connections from filtered interfaces into the mesh
  dropped; DNAT port forwards pass. Outbound traffic, including shared
  internet, unaffected.

The setup wizard and `trellis-deploy --card` set web UI and SSH from the
uplink (`web.wan_access`, `firewall.ssh`), both on by default. A node in
setup mode, which has no admin password yet, does not serve its web UI on
the uplink.

Off (`firewall.wan: open`): no Trellis rules; host firewall applies.
Nodes upgraded from a release without this firewall stay open until it
is enabled; an alert is raised.

## Regulatory modes

Set per mesh profile:

- **Country**: national licence-exempt channels and power limits.
- **Unrestricted**: all channels, widths and power the hardware supports.
  Requires frequency authorization; unlawful without it in most
  countries.

The active mode is always shown (Overview, Settings → System, `meshctl
status`), with a banner while unrestricted. **Dismiss** minimizes the
banner to a "U" corner marker in that browser (warning on hover or tap);
the full banner returns the next time the mesh enters unrestricted mode.

## OS updates

Settings → System → **OS updates**: the operating system's packages
(apt). **Check** fetches the package lists and counts pending updates;
**Update now** installs them. Held packages stay as they are: images hold
the kernel and its headers (the drivers are built for that kernel).
Trellis's own packages are not updated here (see below). Packages are
upgraded, never removed or added; changed configuration files keep the
node's version. Needs internet (the node's uplink, or a gateway of the
mesh). **Reboot: Required** after some updates; reboot when convenient.

## HaLow firmware and BCF

A Wio-WM6108 HaLow module needs Morse Micro's firmware (`mm6108.bin`)
and the module's board configuration file (`bcf_fgh100mhaamd.bin`) in
`/lib/firmware/morse`. Images carry the firmware; the BCF reaches a node
with no step by hand:

- from the card (`trellis-deploy --card`, the default for a HaLow
  board's card);
- from another node of the mesh that holds both files (served on the
  mesh only, port 7373);
- from Morse Micro's repository over the node's uplink.

Each file fetched must match the SHA-256 pinned in
`/usr/share/trellis/packaging/dkms-morse/firmware.pins`, whatever its
source. While a file is absent, the alert `halow-files-missing` names
it. A node with both pinned files holds and serves them to other nodes.

Only an absent file is fetched. A file already in `/lib/firmware/morse`
under that name is never replaced, whatever it holds (another BCF, a
[trial](#trying-a-new-driver-firmware-or-board-file)'s); such a node
does not serve the files.

## Upgrading a device by itself

`trellis-deploy` upgrades every listed device. Single device: web UI,
Settings → System → Software → **Choose a release file** (the `trellis`
package for its architecture), or on the device:

```sh
sudo meshctl upgrade trellis_0.4.0_arm64.deb
```

- The package must be signed with a trusted release key: keys built into
  the running version, plus `/etc/trellis/release-keys/NAME.pub`.
  `--check`: verify only.
- Before installing, the node records its health baseline and keeps the
  running version, configuration, mesh profile and identity. After
  installing, it self-tests for up to 3 min (meshd on the new version, no
  new failures, mesh peer restored if it had one, web UI responding) and
  rolls back version and configuration on failure, with the reason.
- Independent of the session: continues if the session drops; after power
  loss mid-upgrade it completes or rolls back on next start. An
  unfinished upgrade (`meshctl upgrade --status` or `meshctl doctor`):
  `sudo systemctl start trellis-upgrade-resume`.
- Last result: Software panel or `meshctl upgrade --status`. **Go back**
  there, or `meshctl upgrade --rollback`, returns to the previous version
  (`--restore-config` also restores the pre-upgrade configuration).

## Upgrading the whole mesh from one node

A node distributes a release through the mesh: every eligible node
upgrades, one at a time, farthest first, the sending node last. Web UI:
Settings → System → **Mesh upgrade**, select the `trellis` package for
each architecture in the mesh. Or on the node:

```sh
meshctl upgrade --mesh trellis_0.4.0_arm64.deb trellis_0.4.0_armhf.deb
meshctl upgrade --mesh --yes --follow trellis_0.4.0_arm64.deb trellis_0.4.0_armhf.deb
```

- Without `--yes`: plan only (order; skipped nodes with reason: no
  package for the architecture, not installed from the package, release
  too old).
- Each node fetches its package from the sender over the mesh, verifies
  the signature with its own release keys, and waits for its turn. Then
  it upgrades as `meshctl upgrade` does: health check, automatic rollback
  on failure.
- **The first rollback halts the upgrade**, as does a node not
  progressing on its turn for 15 min. Remaining nodes keep their release.
  A halted upgrade ends once outstanding nodes have been silent for 15
  min; a node returning later halts instead of upgrading.
- The sender may go offline or restart: nodes continue on their own. A
  node out of range when it was sent fetches it from another node.
- Progress: `meshctl upgrade --mesh`; summary line in `meshctl status`;
  Overview while active. Cancel on the sending node (`meshctl upgrade
  --mesh --cancel`, or **Cancel the upgrade**): nodes not yet started
  stop.

Run the same release on all nodes. Differences raise an alert; a warning
when a node is a release series apart (first number, or first two below
1.0): such nodes may not follow each other's mesh changes.

## The web UI

**https://10.73.0.1** (or `https://node.mesh`) from a client on a node's
AP or Ethernet ports; from anywhere in the mesh,
`https://<callsign>.mesh`. `.mesh` names require the node's DNS: disable
Private DNS on phones and secure DNS in browsers if they do not resolve.

| Page | Content |
|---|---|
| Overview | Node state and clock, mesh links, nodes, internet, site link, alerts |
| Mesh | Live topology with link rates (View: all links, or one band, the site link or wired); select a node for its path from this node; direct links, routes |
| Internet | Local uplink, gateway in use, all gateways; site link and members |
| Devices | Clients on the AP and ports; AP QR code |
| Radios | Radios, capabilities, role assignment |
| Multicast | Multicast path state and causes |
| Voice | Talk groups on the air; this node's channels, audio device, PTT and headset; talk groups for every node; console (monitor and PTT from the browser) |
| Alerts | Faults, warnings and notes, most severe first |
| Settings | Node, Wi-Fi, internet, ports, mesh profile, channels, access, system, appearance |

Changes are validated before apply; any service impact (link, AP, the
current session) is shown for review. Pages with unsaved or unsent edits
are marked in the menu; the browser warns before closing them.

Times are shown in the viewing device's zone where the browser reports
one; Settings → Appearance selects another zone or Zulu (UTC, also the
fallback). The Overview shows the node clock in that zone, with Zulu and
the time source; an unsynced clock is marked. The theme button cycles
dark, light and red night themes; Settings → Appearance selects the dark
and light themes (Trellis defaults, or editor themes such as Nord,
Gruvbox, Dracula, Catppuccin, Solarized). Everything in the UI is also
available in `meshctl` and the [HTTP API](api.md).

## Push-to-talk voice

Preview: lab-tested; not yet field-tested with headsets on nodes.
Voice is part of the `trellis` package on arm64 and amd64 (not armhf
yet), off until `voice.enable` is set (or the Voice page). It loads
`libopus0` and `libasound2t64` (`libasound2` on older releases), which the
package recommends: `trellis-deploy` and `apt install` bring them; on a
node upgraded with `meshctl upgrade` and no network, install them before
turning voice on (the node says so). A node that had the separate
`trellis-voice` package of earlier builds loses it at the upgrade: the
`trellis` package takes its place.

The talk groups are mesh-wide, in the profile:

```yaml
voice:
  groups:
    - {number: 1, name: Command}
    - {number: 2, name: Alpha}
    - {number: 3, name: Bravo, address: 239.73.80.3, port: 38805}  # the defaults
  bitrate: 32000          # Opus, bit/s (6000–128000)
```

A group is a multicast address and port: by default `239.73.80.n`, port
`38799 + 2n` (38801, 38803 …); RTCP uses the next port. To talk with the
ATAK Voice plugin, set its Multicast channel to the group's address and
port, protocol RTP.

Each node chooses what it hears and how, in [node.yaml](#configuration-files):

```yaml
voice:
  enable: true
  device: auto            # or usb:0d8c:0012@1-1.3, onboard:<ALSA card id>
  ptt: auto               # or none, vox, cm108:<pin 1–4>, serial:<tty>:<cts|dsr|dcd>, gpio:<chip>:<line>
  channels:
    a: {group: 1, ear: left, volume: 80}
    b: {group: 2, ear: right, volume: 80}
  listen: [3]             # heard in both ears, at listen_volume (default 60); a channel's group is not listened to twice
  tones: true
```

- **Device `auto`:** an OpenVLM first, then any CM108-family adapter with
  its HID interface, then any USB audio that records, then onboard audio.
  Devices are named by USB vendor, product and port, or by ALSA card id,
  never by card number. `meshctl voice` and the service's log show the one
  in use.
- **PTT `auto`:** an OpenVLM's PTT (GPIO3, pressed = high) and its volume
  buttons; else the adapter's headset button (channel A). Others:
  - `cm108:<pin>`: a button between that GPIO pin and 3.3 V on any CM108
    adapter;
  - `serial:ttyACM0:cts`: a modem-status line of a USB serial port
    asserted (a serial PTT cable). The service may open USB serial ports
    (`ttyUSB`, `ttyACM`); another port needs a `DeviceAllow=` in
    `systemctl edit trellis-voiced`. A cable plugged back in is found
    again;
  - `gpio:gpiochip0:17`: a button from that line to ground (its pull-up
    is turned on). On Raspberry Pi OS add the service to the gpio group:
    `systemctl edit trellis-voiced`, `[Service]`
    `SupplementaryGroups=gpio`;
  - `button`: a plain USB audio adapter's headset button. A phone-style
    PTT adapter (3.5 mm to a Kenwood PTT) presses it: the adapter reports
    it as Play/Pause, held as long as the PTT. `auto` takes it when the
    adapter has no CM108 pins; its volume keys step the channel;
  - `vox`: speech above -30 dBFS keys channel A, held 0.8 s after. The
    level is the microphone's after `mic_gain` and before the automatic
    gain. A VOX talk is released after 60 s, and VOX keys again once the
    microphone has fallen quiet. It is the node's PTT only
    (`voice.ptt`), not a channel's.

  `ptt_mode`: `hold` (talk while pressed; the default for every PTT) or
  `toggle` (press to start, press to stop; released after 60 s).

**PTT adapters.** A CM108 adapter with its PTT on a GPIO pin (an OpenVLM)
reads the PTT apart from the microphone: hold to talk works. A
phone-style adapter (3.5 mm to a Kenwood PTT, for Zello and the like)
keys by shorting the microphone line, as a phone headset's button does:
while it is held the microphone is silent (measured: about -78 dBFS,
against -48 for a quiet room). Use it with **press to start, press to
stop**, or with VOX. Mismatch detection:
- a headset-button PTT with hold to talk is flagged in the change's
  review, on the Voice page and in Alerts, with one-click fixes;
- with a headset-button PTT in hold to talk, a talk is stopped when the
  press itself silenced the microphone: a second in, the raw microphone
  (before any gain) is at least 20 dB below its level at rest and under
  -65 dBFS. Nothing more is sent, the operator hears a low
  long-short-long tone (even with tones off), and Alerts and the Voice
  page say why. A PTT read apart from the microphone (a CM108 pin,
  GPIO, serial, the browser) is never watched, and a microphone as quiet
  at rest as when keyed (a gated headset, a silent room) is never cut;
- **PTT test** (Voice page, Headset): hold PTT, speak, release: playback
  of the would-be transmission, mic level keyed and idle, and a verdict
  (OK, mic muted by PTT, too quiet, released immediately, no press).
  Nothing transmitted. With press to start, press to stop: press, speak,
  press again.

  The node's `ptt` keys channel A; `channels.b.ptt` gives channel B one
  of its own (two CM108 pins, or a second line): dual PTT. With
  `channels.b.device` naming a second adapter, channel B has its own
  headset too: its mic, its earpiece, and that adapter's PTT and volume
  buttons (two OpenVLMs: one channel each).
- **Priority groups:** while one talks, every other group is 12 dB
  quieter, and its talk starts with the override tone (three short
  beeps).
- **A radio bridged into a group (RoIP):** connect a land-mobile radio's
  speaker and mic to the node's audio, its carrier-sense output to the
  channel's PTT input, and its PTT to an output named by
  `channels.a.key` (`cm108:<pin>`, `serial:<tty>:<rts|dtr>`,
  `gpio:<chip>:<line>`). Radio Rx is transmitted on the group; group
  audio keys the radio (0.3 s hang time; released after 3 min continuous,
  until the group is idle).
- **FEC:** Opus in-band FEC adapts to measured loss: 10 % on a clean
  mesh, up to 40 %.
- **Busy:** a group that carried a packet in the last 250 ms refuses PTT,
  with the busy tone; release and press again.
- **Levels:** voiced sets the adapter's own mixer at start: its outputs
  to 0 dB (many adapters start lower: a KT USB Audio at -20 dB), its
  microphone raised to 0 dB if below, any path that plays the
  microphone straight into the earpiece off, and the adapter's own
  automatic gain off. Unknown controls are left unchanged. The Headset
  panel lists the settings applied. **Automatic microphone gain**
  (`mic_agc`, default on) normalizes speech to about -18 dBFS (up to +30
  dB), matching speaker-mics to headsets; `mic_gain` adds fixed gain
  before it.
- **Tones** (local only): PTT granted (two beeps), released, busy, volume.
- A mono device (OpenVLM's one earpiece) hears both channels.

`meshctl voice` shows each group, who is talking and the packets heard
and lost, and both channels.

**Console (TOC):** Voice page → Console: browser monitor and PTT
through the node, for any signed-in user. Start it (browsers require a
click before audio), select groups (volume, pan, mute each), hold the PTT
button (or Space/Enter on it) to transmit on the selected group. Mic
permission requested on first transmit; requires HTTPS (accept the node
certificate once). Headset rules apply: busy groups refuse (three tones);
60 s transmit limit; monitored audio muted while transmitting (feedback
prevention). PTT releases on button release, loss of focus, or page
hidden; a press released during the mic permission prompt keys nothing.
Console ID: `<node callsign>/<console name>`.

**All call:** the console's second button transmits on all talk groups
(priority override). Not refused on busy groups: Trellis nodes switch to
it immediately, mark it ALL CALL, sound the override tone and duck other
groups, then return to the previous talker when it ends. Marked in RTCP
(SDES NOTE `all-call`); other RTP receivers (ATAK Voice plugin) ignore
the mark and mix both streams on a busy group. One stream per group on
the mesh while active; 60 s limit. Opus runs on the node, so any current
browser works (Firefox, Chrome, Edge, Safari, mobile); each monitored
group uses about 256 kbit/s between node and browser while active
(client AP or LAN). Consoles require voice enabled (`voice.enable`); no
headset required.

**Reset to defaults** (Voice page, This node): device, PTT, volumes,
gain and options to defaults; voice on/off and talk groups unchanged.
Applied on save.

**Supported adapters:** OpenVLM and other CM108 adapters. Other adapters
and onboard jacks may lack features; the Voice page flags devices without
a microphone or PTT.

**Hot-plug:** voiced polls sound cards every second; plug, unplug or
replug restarts audio and re-detects the adapter (about 1 s gap, no
configuration).

**Onboard audio jacks** may have no microphone and no button detection
(some boards' 3.5 mm jack is line out only, or line out and video):
receive only. The Voice page flags it (no PTT found; no signal
on the mic input), and Alerts lists nodes whose device has no PTT. A
USB audio adapter provides both.

**Voice** page: live state (active talkers per group, channel state and
levels), node settings (saved like any node setting), headset tests
(tone per ear, 10 s mic loopback; nothing transmitted), and talk groups
(sent as a mesh-wide profile change).

## Trying a new driver, firmware or board file

A faulty driver, firmware or board configuration file can disable a node.
`meshctl trial` installs new files with automatic recovery:

```sh
sudo meshctl trial begin halow-bcf --kind bcf --version "27 dBm" --module morse \
     --file /lib/firmware/morse/bcf_board.bin=./bcf_27dbm.bin
```

- The replaced files are backed up and the trial recorded before the new
  files are installed and the module reloaded (`--reboot`: no reload;
  loads at next boot).
- meshd confirms the trial after 5 min of continuous health on that
  driver's radios: present, mesh up on their links, peer restored if one
  existed at trial start. Ignored radios are not checked. Not healthy
  15 min after the new files load (`--reboot`: 15 min after that boot):
  reverted. Healthy, then not healthy for 5 min: reverted. After a
  restart of meshd: at least 5 min from the restart.
- One open trial per file: a trial of a file another pending or armed
  trial replaces is refused.
- A reboot while the trial is pending restores the previous files at
  next boot, before any driver loads (`trellis-trial.service`). The trial
  is then **locked out**: the same files are refused until `sudo meshctl
  trial rearm NAME`.
- `meshctl trial`: list. `confirm NAME`: confirm now. `revert NAME`:
  restore the previous files, also after confirmation. Shown in Settings →
  System and `meshctl status`; reverts raise an alert.

The boot hook requires the state directory on the root filesystem.
Modules loaded by the initramfs load before it.

## Command line (meshctl)

`meshctl` runs on the node. The operator account and root can use it.
Commands marked **Root** require `sudo`. Options shown as `--json` print
the same information as JSON.

### Status and diagnostics

| Command | Function | Root |
|---|---|---|
| `meshctl status [--json] [--watch]` | Shows the node's state: mesh links and neighbours, routes, client network, client AP, internet, site link and alerts. `--watch` refreshes every 2 seconds. | |
| `meshctl radios [--json]` | Lists the radios, their capabilities and the role of each. | |
| `meshctl clients [--json]` | Lists the clients on this node: AP clients and DHCP leases. | |
| `meshctl doctor [--json] [--node PATH] [--profile PATH]` | Checks the kernel, batman-adv, radios, helper programs and configuration. Include its output in a bug report. | For the mesh profile check |
| `meshctl survey [--start]` | Shows the last mesh-wide channel survey. `--start` runs a new survey; each radio leaves its channel briefly. | |
| `meshctl version` | Prints the version. | |

### Mesh profile

| Command | Function | Root |
|---|---|---|
| `meshctl profile new --name NAME [--band 5g] [--channel 149] [--country US] [--mode country\|unrestricted] [-o FILE [--force]]` | Creates a mesh profile with a random passphrase. Prints it, or writes it to FILE readable by its owner only. | |
| `meshctl profile show [FILE]` | Prints this node's mesh profile, to copy to other nodes. | For this node's profile |
| `meshctl profile share` | Shows this node's mesh profile as a profile code and a QR code, for a new node's setup page. | |
| `meshctl profile band 2g\|5g\|6g [--channel N] [--width MHz] [FILE]` | Changes the band of a profile file (default `mesh-profile.yaml`). | |
| `meshctl profile same FILE1 FILE2` | Compares two profiles. Exits 0 if they are equivalent; otherwise names the settings that differ. | |
| `meshctl profile install [FILE\|-]` | Validates a profile, installs it on this node and applies it. Other nodes are not changed. | |
| `meshctl profile push [--in S] [--yes] [--replace] FILE` | Sends a profile to every node; all nodes switch to it in S seconds (default 60). Without `--yes`, shows the plan only. See [Changing the whole mesh](#changing-the-whole-mesh). | |
| `meshctl profile edit [--in S] [--yes] [--replace]` | Opens the mesh's current profile in `$EDITOR`, then sends it to every node as `profile push` does. | |

### Node

| Command | Function | Root |
|---|---|---|
| `meshctl node new --callsign NAME [--id N] [-o FILE [--force]]` | Creates a node configuration file. | |
| `meshctl config validate [node.yaml] [profile.yaml]` | Validates configuration files without applying them. See [Configuration files](#configuration-files). | To read the mesh profile |
| `meshctl reload` | Makes meshd read its configuration files again. An invalid file is rejected and the running configuration is kept. | |
| `meshctl reconcile` | Makes meshd run a reconcile pass now. | |
| `meshctl prepare [--no-doctor]` | Run after installing the packages by hand: builds and loads the pinned batman-adv module and, where the kernel lacks it, the MT7915/MT7916 driver; sets radio driver options and regulatory rules. Reports whether a reboot is required. | Yes |

### Client AP and web UI

| Command | Function | Root |
|---|---|---|
| `meshctl ap [--json]` | Shows the client AP's name and passphrase, and the QR code a phone joins with. | |
| `meshctl web` | Shows the web UI's addresses and whether an admin password is set. | |
| `meshctl web password [--stdin\|--clear]` | Sets or removes the web UI's admin password. | Yes |
| `meshctl web wan-access on\|off` | Serves the web UI on the node's uplink network, or stops it. | |

### Voice and site link

| Command | Function | Root |
|---|---|---|
| `meshctl voice [--json]` | Shows the talk groups, who is talking and this node's channels. See [Push-to-talk voice](#push-to-talk-voice). | |
| `meshctl site [--json]` | Shows this node's site link key and its tunnels to other sites. See [Linking sites over the internet](#linking-sites-over-the-internet). | |
| `meshctl site --new-key [--force]` | Gives this node a new site link key. | |

### Upgrades and trials

| Command | Function | Root |
|---|---|---|
| `meshctl upgrade FILE.deb` | Installs a release. The node goes back to the previous release if it is not healthy afterwards. See [Upgrading a device by itself](#upgrading-a-device-by-itself). | Yes |
| `meshctl upgrade --check FILE.deb` | Checks a package's signature, architecture and version, and installs nothing. | |
| `meshctl upgrade --status` | Shows the upgrade in progress and the result of the last one. | |
| `meshctl upgrade --rollback [--restore-config]` | Returns to the release before the last upgrade. `--restore-config` also restores the configuration from before the upgrade. | Yes |
| `meshctl upgrade --resume` | Completes or undoes an interrupted upgrade. | Yes |
| `meshctl upgrade --mesh [--yes] [--follow] FILE.deb...` | Upgrades every node of the mesh from this node, one at a time. Without `--yes`, shows the plan only. Without files, shows the mesh-wide upgrade in progress. `meshctl upgrade --mesh --cancel` stops a mesh-wide upgrade started on this node. See [Upgrading the whole mesh from one node](#upgrading-the-whole-mesh-from-one-node). | |
| `meshctl trial` | Lists the driver, firmware and board files on trial, and the last trial of each. | |
| `meshctl trial begin NAME --file PLACE=NEWFILE [--module MOD] [--reboot]` | Installs new files on trial. See [Trying a new driver, firmware or board file](#trying-a-new-driver-firmware-or-board-file). | Yes |
| `meshctl trial confirm\|revert\|rearm NAME` | Confirms a trial now, restores the previous files, or allows files that were reverted to be tried again. | Yes |

## Configuration files

| File | Content |
|---|---|
| `/etc/trellis/node.yaml` | This node's settings: callsign, radio roles, ports, client AP, firewall, voice. Example: [configs/examples/node.yaml](../configs/examples/node.yaml). |
| `/etc/trellis/profile.yaml` | The mesh profile, the same on every node: mesh name, secret, regulatory mode, links, voice talk groups. Readable by root only. Example: [configs/examples/profile.yaml](../configs/examples/profile.yaml). |

The web UI and the API rewrite `node.yaml` when a setting is changed
there. The rewritten file lists only the settings that differ from the
defaults, and comments are not kept.

To change a file by hand:

1. Edit the file.
2. Validate it: `sudo meshctl config validate`.
3. Apply it: `meshctl reload`.

A change to `profile.yaml` on one node applies to that node only. To
change the mesh profile on every node, see
[Changing the whole mesh](#changing-the-whole-mesh).

## When something is wrong

- **Alerts**: cause and corrective action. **Ignore** hides an alert from
  the Overview and alert count in that browser; it stays on the Alerts
  page, and is shown again on higher severity or on return after 1 h
  clear.
- `sudo meshctl doctor`: kernel, batman-adv, radios, helpers and
  configuration checks; report formatted for issues.
- `meshctl status`: full node state as text.
- **Unplanned resets:** Alerts lists restarts without shutdown, with the
  cause from the kernel crash record (card hang, kernel panic, serial
  console SysRq) or, with no record, power loss. An oops the kernel
  recovers from is not a reset. A crash while `meshctl trial` or `meshctl
  prepare` loads a driver is one. Times come from the clock as chrony sets
  it; without an RTC, a reset in a boot whose clock was never set is
  dated by the start of the next boot. With no RTC and no time source in
  reach, the time shows as unknown, and the 24 h alert counts only the
  resets found at this boot. Requires a pstore
  backend; `meshctl doctor` reports whether the board has one (Raspberry
  Pi 4 / CM4: `dtoverlay=ramoops-pi4` in `config.txt`).
- **Hardware:** Settings → System → Hardware: CPU temperature, CPU and
  memory use (3 min history), storage, supply voltage, radio temperatures,
  PCIe links. `meshctl status`: the `host:` line. Alert thresholds:

  | Alert | Warning | Fault |
  |---|---|---|
  | `host-hot` | Throttle point (passive trip; default 80 °C) | 5 °C below shutdown |
  | `undervoltage` | Undervoltage now (Raspberry Pi); note for 10 min after | — |
  | `disk-full` | Filesystem ≥ 90 % | ≥ 97 % |
  | `memory-low` | Free memory < 10 % | < 5 % |

- Bug reports and hardware results: [CONTRIBUTING.md](../CONTRIBUTING.md).

The procedures below start from `meshctl status` on the node. Its
**Warnings** list shows each alert as `[LEVEL] code: message`; the
**Alerts** page shows the same alerts.

### A node does not join the mesh

1. Run `meshctl status`. The mesh link's **DIRECT LINK** table lists the
   nodes this node has a link with.
2. If the warnings include `no-hard-interfaces`, the node has no radio or
   wired port on the mesh. Run `meshctl radios` and check that a radio
   has the role `mesh`, and that its **BANDS** column includes the band
   of the mesh profile.
3. If the mesh link shows an `SAE` line with authentication failures, the
   neighbour did not answer, or the two nodes have different mesh
   profiles. Compare the profiles:
   1. On a node of the mesh, run `sudo meshctl profile show > mesh.yaml`.
   2. Copy `mesh.yaml` to this node.
   3. Run `sudo meshctl profile same mesh.yaml /etc/trellis/profile.yaml`.
      The command names the settings that differ.
   4. If they differ, run `meshctl profile install mesh.yaml`.
4. If the warnings include `mesh-not-joined`, run `sudo meshctl doctor`
   and correct the failed checks it lists.

### Clients have no internet

1. Run `meshctl status` on the node the client is connected to. The
   `Internet:` line names the gateway in use.
2. If the warnings include `no-gateway`, no node of the mesh shares an
   uplink. Connect an uplink to a node; see
   [Sharing an uplink](#sharing-an-uplink).
3. The gateway table's **USABLE** column gives the reason a gateway is
   not used, for example `no: its uplink is down`.
4. If the gateway node's warnings include `wan-portal`, its uplink is
   behind a captive portal and is not shared. Sign in to the portal from
   a client of that node by opening any `http://` page.

### `.mesh` names do not resolve

1. Open the node by its address: `https://10.73.0.1` from a client of
   the node. Addresses work without the node's DNS.
2. On the client, turn off Private DNS (Android), secure DNS in the
   browser, and any VPN. Each of these sends name lookups to another
   server instead of the node.
3. If the node's warnings include `dhcp-no-dnsmasq`, install dnsmasq:
   `sudo apt install dnsmasq-base`.

### The web UI does not load

1. Run `meshctl web` on the node. It lists the web UI's addresses for
   the node's clients and for the mesh, and states whether the web UI
   answers on the uplink network.
2. Use an address from that list with `https://`. On the first
   connection the browser warns about the node's self-signed
   certificate; accept it.
3. From the uplink network, the web UI answers only while **Web UI from
   uplink** is on: Settings → Access, or `meshctl web wan-access on`. A
   node in setup mode does not serve the web UI on its uplink network.
