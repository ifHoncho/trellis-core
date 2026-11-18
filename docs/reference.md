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
- [Push-to-talk voice](#push-to-talk-voice)
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

### Installing from the apt repository

An apt repository of the packages, built with `make apt-repo`
([building.md](dev/building.md)), can be served from any web server. The
project does not publish one yet. On the device, with the repository's
address in `REPO`:

```sh
curl -fsSL "$REPO/trellis-archive-keyring.gpg" | sudo tee /usr/share/keyrings/trellis-archive-keyring.gpg > /dev/null
echo "deb [signed-by=/usr/share/keyrings/trellis-archive-keyring.gpg] $REPO stable main" | sudo tee /etc/apt/sources.list.d/trellis.list
sudo apt update
sudo apt install trellis trellis-batman-adv-dkms
sudo meshctl prepare
```

Check the key's fingerprint (`gpg --show-keys
/usr/share/keyrings/trellis-archive-keyring.gpg`) against the one its
publisher gives before you trust it. `apt upgrade` then brings new releases,
but it installs them directly, without the health check and rollback of
`meshctl upgrade`. For a node in the field, use `meshctl upgrade`, or the
mesh-wide upgrade.

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
- A radio that carries a link is Trellis's alone: meshd removes any other
  interface that appears on it (a `p2p` or `wlan` one that a network
  manager would scan on), except a **monitor** interface, which listens
  on the link's channel. To capture a link: `sudo iw phy PHY interface add
  mon0 type monitor && sudo ip link set mon0 up`, then `tcpdump -i mon0`.

## Sharing an uplink

A node with an uplink of its own (the interface of its default route, a
port with role `wan`, or `gateway.wan`) shares it with the mesh
(`gateway.mode: auto`; `off` never shares and still uses other nodes'
gateways). Two checks decide whether the uplink is shared, both out of
the uplink itself:

- **Pings** to `gateway.probe_targets` (default `1.1.1.1`, `8.8.8.8`,
  `9.9.9.9`) every `gateway.probe_interval_ms` (1000). Three missed rounds
  withdraw the uplink; three good ones bring it back.
- **A web check** (`gateway.web_check`, on by default): a request to
  each of `gateway.web_check_urls` for the empty "204" answer phones and
  laptops use to spot a login page. The defaults are Google's, Cloudflare's
  and Ubuntu's connectivity-check addresses. The check runs every minute,
  and again when the pings start or stop passing. Any other answer, such as
  a redirect or a page, means a **login page** (a hotel or café network)
  sits in front of the internet.

An uplink behind a login page is not offered to the mesh, even when it
answers pings. While no other gateway is usable, the node's own devices
still go out through it, so someone can open any `http://` page there,
reach the login page and sign in. The check then runs every 10 s, and
sharing starts on its own once it passes. The Internet page and `meshctl
status` say "behind a login page", with the answer seen. When none of the
web addresses answer (a network that blocks them), the pings alone decide.
`gateway.web_check: false` turns the web check off.

The requests carry no cookies and no details about the node; the sites
asked see the uplink's public address, as the ping targets do.

## Access from outside the mesh

Settings → Access → **Firewall** (`firewall.*` in node.yaml) is an nftables
table, `inet trellis_fw`. `lo`, `br-mesh` and `anycast0` (the mesh, the
node's Wi-Fi and device ports) are trusted and never filtered; the rules
apply to every other interface: the uplink, and any VPN or container
link on the host.

With **Drop new connections from filtered interfaces** on
(`firewall.wan: closed`, the default for nodes Trellis sets up):

- **Input:** established and related, ICMP and ICMPv6, DHCP replies
  (udp/68, udp/546), SSH (tcp/22, unless `firewall.ssh: false`), the web
  UI (tcp/80, tcp/443) while "Also on the node's uplink network" is on,
  and the ports under **Also accept** (`firewall.allow`, e.g. `tcp/5201`)
  are accepted; the rest is dropped.
- **Forward:** new connections from filtered interfaces into the mesh are
  dropped; your own DNAT port forwards pass. Traffic leaving the mesh,
  including the internet the node shares, is not touched.

Off (`firewall.wan: open`), Trellis installs no rules and the host's own
firewall, if any, applies. A node upgraded from a release without this
firewall stays open until you turn it on; Alerts reminds you.

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

## Upgrading the whole mesh from one node

A node can send a release through the mesh: every node that can takes
it, one at a time, farthest from that node first and that node last. In
its web UI, Settings → System → **Every node**, choose the `trellis`
package for each architecture in the mesh; or on the node:

```sh
meshctl upgrade --mesh trellis_0.4.0_arm64.deb trellis_0.4.0_armhf.deb
meshctl upgrade --mesh --yes --follow trellis_0.4.0_arm64.deb trellis_0.4.0_armhf.deb
```

- Without `--yes`, nothing starts: it shows the order, and which nodes are
  left out and why (no package for their architecture, not installed
  from the package, a release too old to take part).
- Each node fetches its package from the sending node over the mesh,
  checks the signature with its own release keys, then waits for its
  turn. On its turn it upgrades as `meshctl upgrade` does: health check,
  and back to the old version by itself if it is not healthy.
- **The first node that goes back halts the rest,** as does a node that
  does not move on its turn for 15 minutes. The others keep the release
  they run. A halted upgrade is over once the nodes still out have been
  silent for 15 minutes; one that was away halts too when it is back,
  instead of upgrading by itself.
- The sending node may go away, or restart: the nodes go on by
  themselves. A node that was out of reach when it was sent asks another
  node for it.
- `meshctl upgrade --mesh` shows the progress, `meshctl status` a line;
  the overview shows it while it runs. `meshctl upgrade --mesh --cancel`,
  or **Cancel the upgrade**, on the sending node, stops the nodes that
  have not begun.

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

## Push-to-talk voice

A preview: built and tested in the lab, not yet with headsets on nodes.
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

Each node chooses what it hears and how, in node.yaml:

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
stop**, or with VOX. Trellis catches the mismatch three ways:
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
- **Check my PTT** (Voice page, Headset): hold the PTT, say a sentence,
  release; you hear what would have been sent, with the microphone's
  level held and at rest, and a verdict: works, cuts the microphone,
  too quiet, released at once, or no press. Nothing goes on the air.
  With press to start, press to stop: press, say the sentence, press
  again.

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
  `gpio:<chip>:<line>`). What the radio receives goes out on the group;
  what the group says keys the radio (held 0.3 s through gaps, and let
  go after 3 minutes in one go until the group is quiet).
- **Redundancy:** Opus's in-band FEC follows the loss the node hears:
  planned for 10 % loss on a clean mesh, up to 40 %.
- **Busy:** a group that carried a packet in the last 250 ms refuses PTT,
  with the busy tone; release and press again.
- **Levels:** voiced sets the adapter's own mixer at start: its outputs
  to 0 dB (many adapters start lower: a KT USB Audio at -20 dB), its
  microphone raised to 0 dB if below, any path that plays the
  microphone straight into the earpiece off, and the adapter's own
  automatic gain off. A control it does not know by name is left as it
  is. The Headset panel lists what it set. **Automatic microphone gain** (`mic_agc`, on by default) brings
  speech to about -18 dBFS by up to +30 dB, so a radio speaker-mic is as
  loud as a headset; `mic_gain` adds a fixed gain before it.
- **Tones** (local only): PTT granted (two beeps), released, busy, volume.
- A mono device (OpenVLM's one earpiece) hears both channels.

`meshctl voice` shows each group, who is talking and the packets heard
and lost, and both channels.

**Console (a TOC):** the Voice page's Console panel lets this browser
monitor talk groups and talk on one, through the node, for anyone signed
in to the UI. Start it (the browser needs a click before it plays sound),
choose the groups to hear, each with its volume, pan and mute, and hold
the PTT button (or Space or Enter on it) to talk on the chosen group. The
browser asks for the microphone the first time; it needs the UI over
HTTPS (accept the node's certificate once). The same rules as a headset:
a busy group refuses, with three tones; talk stops after 60 s; while you
talk, what you monitor is muted so the speakers do not feed the
microphone. A button is released when it is let go, and also when the
page loses the keyboard or goes out of sight; a press let go while the
browser asks for the microphone keys nothing. Listeners see you as `<node callsign>/<console name>`.

**All call:** the console's second button talks on every talk group at
once, for urgent messages. It is not refused where someone is talking:
Trellis nodes switch to it at once, mark it ALL CALL, sound the override
tone and duck their other groups; when it ends they go back to whoever
was talking. It is marked in RTCP (an SDES NOTE, `all-call`), which
other RTP receivers (the ATAK Voice plugin) do not read: on a busy group
they hear both at once. It costs a stream per group on the mesh while it
lasts, and stops after 60 s like any talk. The
node does the Opus work, so any current browser works (Firefox, Chrome,
Edge, Safari, phones); each group a console hears takes about 256 kbit/s
between the node and the browser while someone talks, which suits the
node's own Wi-Fi or LAN. A node needs voice on (`voice.enable`) to serve
consoles; it needs no headset.

**Reset to defaults** (Voice page, This node) puts the device, PTT,
volumes, gain and switches back to their defaults; voice on or off and
the node's talk groups stay. It takes effect when saved.

**Supported adapters** are an OpenVLM and other CM108 adapters. Other
audio adapters and onboard jacks may not support every feature: the
Voice page says when a device shows no microphone or no PTT.

**Plugging in:** voiced looks at the sound cards every second; an
adapter plugged in, unplugged or replugged restarts its audio, which
finds it again (a second's gap, no configuration needed).

**Onboard audio jacks** may have no microphone and no button detection
(some boards' 3.5 mm jack is line out only, or line out and video): such
a device listens only. The Voice page says so (no PTT found; no signal
at all from the microphone input), and Alerts lists a node whose device
offers no PTT. A USB audio adapter adds both.

The web UI's **Voice** page shows the same live (who is on the air on
each group, each channel's state and level), edits this node's settings
(saved like any node setting), checks the headset (a tone in either
channel's ear, or the microphone played back for 10 s; nothing goes on
the air), and edits the talk groups, which it sends to every node as a
mesh-wide profile change.

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
