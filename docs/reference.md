# Reference

The details behind the [guide](guide.md): every option, and how each part
behaves. Start with the guide; come here when you need to know more.

- [trellis-deploy](#trellis-deploy)
- [Passwords and sudo](#passwords-and-sudo)
- [Installing one device by hand](#installing-one-device-by-hand)
- [Setting a device up from a phone](#setting-a-device-up-from-a-phone)
- [Changing the whole mesh](#changing-the-whole-mesh)
- [Radios and mesh links](#radios-and-mesh-links)
- [Access from outside the mesh](#access-from-outside-the-mesh)
- [Regulatory modes](#regulatory-modes)
- [Upgrading a device by itself](#upgrading-a-device-by-itself)
- [The web UI](#the-web-ui)
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

- `callsign` and `id` apply to a device that is installed by the run: by
  default the callsign comes from its hostname and the ID is chosen by the
  node. Each must be unique in the file.
- `mesh` is the node's address on the mesh (`meshctl status` on any node
  lists them). When its own address does not answer (a node with no cable
  to your network), it is reached there, through the first listed device
  that does answer.

### What a run does

1. **Gets the release** (the newest published, `--release 0.2.0` for
   another, or the files in `--from DIR`) and **checks it**: the
   checksums, and every package's signature against the release keys
   built into `trellis-deploy`. Nothing reaches a device unless all of it
   checks out.
2. **Looks at every device** and shows what it will do to each: a new
   install, an upgrade, nothing ("up to date"), or why it skips one (it
   does not answer, it is not Debian-family, the release has no package
   for its architecture, it runs a newer version). It names the devices
   whose `sudo` asks for a password, then asks once before doing anything
   (`--yes` does not ask; `--dry-run` stops here).
3. **New devices**, a few at a time (`--jobs`, default 4): the packages,
   `meshctl prepare`, the mesh profile (`--profile`; a device that has one
   keeps it), the callsign and node ID, the web UI's admin password (asked
   once for all of them), then a reboot, waiting until each is back with
   Trellis running (`--no-reboot` skips that).
4. **Devices on an older release**, one at a time, with `meshctl
   upgrade`: each checks itself and goes back to its old version on its
   own if it is not healthy, and the first that goes back stops the rest
   (`--keep-going` does not). Devices reached over the mesh go first, so
   the way to the others stays up the longest. A device whose connection
   drops while its mesh restarts is reconnected to and asked how it ended.
5. **A summary**: each device's result, what `meshctl doctor` finds wrong
   there, and the Wi-Fi name and passphrase of each new device. Each
   device's whole output is in `trellis-deploy-logs/<time>/`; the
   passwords and the mesh's secret never are.

### Options

| Option | |
|---|---|
| `--setup` | Only put your SSH key on each device it does not log in to yet (`ssh-copy-id` asks for that device's login password); `--ssh-key FILE.pub` chooses the key |
| `--setup --sudo-nopasswd` | Also let the login user use `sudo` without a password: see [below](#passwords-and-sudo) |
| `--new-mesh NAME --country CC` | Make a new mesh's profile with a fresh secret, in `mesh.yaml` (`--out` for another file; an existing one is never overwritten). `--band 5g` for 5 GHz instead of 2.4 GHz |
| `--profile FILE` | The mesh profile for devices that have none |
| `--release X.Y.Z`, `--from DIR` | Another release, or release files you have |
| `--dry-run`, `--yes` | Only show the plan; do not ask before going ahead |
| `--web-password`, `--no-web-password` | Ask for the web UI's admin password even with `--yes`; never ask |
| `--jobs N`, `--no-reboot`, `--keep-going`, `--force` | New installs at once; no reboot after them (the admin password is then set later, on the node, with `sudo meshctl web password`); go on after an upgrade goes back; reinstall devices that run the release already |
| `--ssh-option Name=value` | An option for every ssh connection (repeatable), as `IdentityFile=~/.ssh/other`; yours override the tool's defaults |
| `--logs DIR` | Where the logs go |
| `--trust`, `--node-key`, `--any-version`, `--no-health-check` | For development builds ([building.md](dev/building.md)) |

A device's SSH host key is accepted the first time the tool meets it, and
refused if it changes later (`StrictHostKeyChecking=accept-new`); pass
`--ssh-option StrictHostKeyChecking=yes` to require devices already in
your `known_hosts`. A device that already has a mesh profile keeps it, and
the run warns if it differs from `--profile`'s.

## Passwords and sudo

`trellis-deploy` logs in with your SSH key, and runs its work as root with
`sudo`. Where `sudo` asks for a password, the run asks you once, tries
that password on every device that needs one, and asks again, by name,
only for a device it does not open (Enter leaves that device out). The
passwords are used for that run only.

`--setup --sudo-nopasswd` writes a rule on each device
(`/etc/sudoers.d/trellis-deploy`) that lets the login user use `sudo`
without a password, so runs stop asking. Think about it first: from then
on, **anyone who can log in as that user is root on the device without
another question**, so your SSH key (and the computer that holds it)
becomes the only thing between an attacker and the device. Prefer, in
order:

1. answering the password prompt (the default): it costs a few seconds a
   run;
2. a passphrase on your SSH key, kept unlocked in `ssh-agent` for the
   session, if you do use the rule;
3. the rule only on devices you control physically, and removing it
   (`sudo rm /etc/sudoers.d/trellis-deploy`) when you are done.

A narrower rule would not be safer: installing packages is root, whatever
command the rule names.

## Installing one device by hand

You need a device and radios that meet [hardware.md](hardware.md), running
Debian, Ubuntu or Raspberry Pi OS, and the two packages from a release:
`trellis` for the device's architecture (`arm64` for most current boards
on a 64-bit OS, `armhf` for 32-bit ones, `amd64` for PCs) and
`trellis-batman-adv-dkms`. Or build them from this repository with `make
deb` (into `dist/deb/`). On the device:

```sh
sudo apt install ./trellis_*_arm64.deb ./trellis-batman-adv-dkms_*_all.deb
sudo meshctl prepare
sudo reboot
```

`apt` brings in what Trellis needs (wpa_supplicant, hostapd, dnsmasq,
nftables, chrony). `meshctl prepare` does what the device needs on top,
skipping what is done already:

- builds and loads the batman-adv release every node runs (DKMS rebuilds
  it after every kernel update; that needs the kernel's headers, which it
  fetches when it can);
- builds the driver for MediaTek MT7915/MT7916 cards on kernels that lack
  it, such as Raspberry Pi OS's;
- sets the driver options some radios need for an encrypted mesh;
- puts the kernel's regulatory rules in Trellis's hands.

It ends with `meshctl doctor`'s checks, and says when a reboot is needed.
After the reboot, a device without a mesh profile waits in setup mode
(next section).

Installing, upgrading and removing never touch a device's configuration,
mesh profile or identity (`/etc/trellis`, `/var/lib/trellis`); only `apt
purge trellis` deletes them.

## Setting a device up from a phone

A device with no mesh profile starts in **setup mode**: it runs its own
Wi-Fi, `trellis-setup-XXXX` (passphrase `trellis-setup`), and serves the
setup page there and on its Ethernet ports.

1. Join `trellis-setup-XXXX` with a phone or laptop, and open
   **https://10.73.0.1**. The certificate is the device's own: accept it
   once.
2. Choose the web UI's **admin password**.
3. **Join a mesh**, by scanning the profile QR code of a node already in
   it (Settings → Mesh profile → **Share with another node**), pasting its
   profile code, or uploading its profile file; or **start a new one**: a
   name, and a band, channel and width for each link (the device proposes
   links for its radios; the secret is generated).
4. **Name the node** (its callsign), review and apply. The setup Wi-Fi
   gives way to the node's own, whose name and passphrase the page shows as
   a QR code.

A node already set up without an admin password (installed by
`trellis-deploy` with none given) shows "No admin password yet"; set one on
the node with `sudo meshctl web password`.

## Changing the whole mesh

Every node of a mesh needs the same **mesh profile**: its links, channels,
secret and regulatory mode. To change it, edit it on any node (Settings →
Mesh profile) and choose **Every node**. That node sends the change to all
the others, shows who has it, and every node switches at the same moment,
a minute later by default. **This node only** is for experiments on one
node.

- The review before sending names any node that cannot follow (no radio
  for the new links, an older version), any node whose regulatory mode
  would change, and the nodes that are off or out of reach and will miss
  it.
- A node that was out of reach takes the change when it next hears one
  that has it, as long as they still share a link.
- A node, or a group of nodes, that was off while the mesh moved comes
  back on the old channel and hears few of the nodes it knew. After a
  minute it looks for the rest on its radios' other channels and bands,
  pays a short visit where it hears its mesh (Alerts shows it), and takes
  the new profile there. A HaLow link cannot be looked for this way: a node
  whose only link is HaLow needs the profile again by hand.
- If two groups each change the mesh while apart, they reconcile when they
  meet: the later change is kept everywhere, and the node that found the
  conflict keeps the other in a file and says so under Alerts, so you can
  send it again.

To keep the mesh together, send changes when every node is on and in
reach, and from one place at a time.

## Radios and mesh links

A mesh profile can have several **links**, each on its own band and
channel: for example 5 GHz for speed, 2.4 GHz and HaLow for reach. Every
node uses the links it has radios for, all at once, and traffic takes the
fastest path to each node. Nodes with different radios still mesh through
the links they share.

- One radio carries one link. A dual-band USB adapter uses one band at a
  time; a card with two radios carries two links.
- A node with fewer radios than links takes the first links in the
  profile's order that its radios can carry: put the link most nodes
  should use first.
- On the **Radios** page you can pin a link to a radio, turn a link off on
  one node, or ignore a radio. "Automatic" lets the node decide.
- **Busy channels:** Settings → Channels shows how busy each link's channel
  is at every node, and **Survey channels** has every node listen across
  its links' bands for a few seconds. When a channel is clearly better,
  one button moves the link there on every node, as a change to the whole
  mesh.
- Higher bands are faster but reach less far. When a link is established
  but cannot carry traffic (one end hears the other too weakly), Alerts
  says so: move the nodes closer, use a narrower width, or let them meet
  on a band that reaches further.

## Access from outside the mesh

A node Trellis sets up is closed to everything outside the mesh: from the
network its uplink is plugged into, it answers only SSH (port 22), ping,
and the web UI if you let it answer there (Settings → Access → "Also on
the node's uplink network"). Settings → Access → **From outside the mesh**
turns SSH off, opens further ports (`tcp/5201`), or opens the node
altogether. The mesh, the node's Wi-Fi and device ports, and the internet
it shares are never affected. A node upgraded from a release without this
firewall stays open until you close it there; Alerts reminds you.

## Regulatory modes

Each mesh profile sets how the radios are limited:

- **Country**: the selected country's licence-exempt channels and power.
- **Unrestricted**: every channel, width and power the hardware allows,
  for operators who hold their own frequency authorization. Using it
  without one is unlawful in most countries.

The mode in force is always shown (Overview, Settings → System, `meshctl
status`), with a banner while unrestricted mode is on. **Dismiss** shrinks
the banner, in that browser, to a triangle marked U in the corner, which
shows the warning when you point at it or tap it; it comes back in full
the next time the mesh enters unrestricted mode.

## Upgrading a device by itself

`trellis-deploy` upgrades every device on your list. One device can also
take a release by itself: in its web UI, Settings → System → Software,
**Choose a release file** (the `trellis` package for its architecture),
or on the device:

```sh
sudo meshctl upgrade trellis_0.4.0_arm64.deb
```

- The package must be signed with a release key the node trusts: the keys
  built into the version it runs, and any in
  `/etc/trellis/release-keys/NAME.pub`. `--check` only checks the file.
- Before installing, the node notes how healthy it is and keeps the
  version it runs, its configuration, mesh profile and identity. After,
  it checks itself for up to three minutes (`meshd` on the new version,
  nothing failing that worked before, a mesh peer back if it had one, the
  web page answering), and puts the old version and configuration back if
  that does not hold, saying why.
- It does not depend on your connection: it carries on if the session
  drops, and a node that loses power in the middle finishes, or goes back,
  when it next starts. If one is ever left unfinished (`meshctl upgrade
  --status` or `meshctl doctor` says so), `sudo systemctl start
  trellis-upgrade-resume` finishes it.
- The Software panel and `meshctl upgrade --status` show how the last
  upgrade ended; **Go back** there, or `meshctl upgrade --rollback`,
  returns to the version before (`--restore-config` also puts back the
  configuration from before the upgrade).

Upgrade every node of a mesh to the same release. Nodes say so under
Alerts while they differ, with a warning when one is a release series
apart (the first number, or the first two below 1.0): such nodes may not
follow each other's changes to the mesh.

## The web UI

Open **https://10.73.0.1** (or `https://node.mesh`) from a device on a
node's Wi-Fi or Ethernet ports; from anywhere on the mesh, a node answers
at `https://<callsign>.mesh`. The `.mesh` names work where the device asks
the node for names: turn off a phone's Private DNS, or a browser's secure
DNS, if they do not resolve.

| Page | What it shows |
|---|---|
| Overview | The node's state, its mesh links, nodes nearby, internet, warnings |
| Mesh | A live map of the mesh with link speeds, direct links, routes |
| Internet | The node's uplink, the gateway in use, every gateway on the mesh |
| Devices | Who is on the node's Wi-Fi and ports, the Wi-Fi QR code |
| Radios | Every radio, what it can do, and which job it has |
| Multicast | Whether multicast takes the efficient path, and why not |
| Alerts | Everything the node wants you to know, most serious first |
| Settings | Node, Wi-Fi, internet, ports, mesh profile, channels, access, system, appearance |

Every change is checked first: when it would interrupt something (a link,
the Wi-Fi, your own connection), the page says what before it is applied.
The theme button cycles a dark, a light and a red night theme; Settings →
Appearance chooses the dark and light ones (Trellis's own, or editor
themes such as Nord, Gruvbox, Dracula, Catppuccin or Solarized). Everything
in the UI is also in `meshctl` on the node and in the [HTTP API](api.md).

## When something is wrong

- **Alerts** names the problem and what to do about it. **Ignore** on an
  alert leaves it out of the overview and the alert count in that
  browser; it stays on the Alerts page, and shows everywhere again if it
  turns more serious or comes back after being gone for an hour.
- `sudo meshctl doctor` checks the kernel, batman-adv, radios, helpers and
  configuration, and gives a report you can paste into an issue.
- `meshctl status` shows the whole node in text.
- Bug reports and hardware results are welcome: see
  [CONTRIBUTING.md](../CONTRIBUTING.md).
