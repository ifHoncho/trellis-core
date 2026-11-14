# User guide

How to put a Trellis node together, set it up from a phone, and run a mesh.

## 1. Install Trellis on a node

You need a board and radios that meet [hardware.md](hardware.md), running
Debian, Ubuntu or Raspberry Pi OS. As root, from a copy of this repository
and the binary for the board (`make cross` builds them into `dist/`):

```sh
sudo scripts/install-batman-adv-dkms.sh               # the batman-adv release Trellis pins
sudo scripts/install-node.sh dist/trellis-linux-arm64  # binary, services, udev rules
sudo scripts/install-regulatory.sh                     # Trellis answers the kernel's regulatory requests
meshctl doctor                                         # checks the kernel, radios and helpers
```

Reboot once (the kernel picks up the regulatory change at boot), then start
the node:

```sh
sudo systemctl enable --now trellis-meshd
```

Installing never overwrites an existing configuration.

## 2. Set the node up from a phone

A node with no mesh profile starts in **setup mode**. It runs its own Wi-Fi,
`trellis-setup-XXXX` (passphrase `trellis-setup`), and serves the setup page
there and on its Ethernet ports.

1. Join `trellis-setup-XXXX` with a phone or laptop.
2. Open **https://10.73.0.1**. The certificate is the node's own, so the
   browser asks you to accept it once.
3. Choose an **admin password**. It protects the node's web page from then on.
4. **Join a mesh** or **start a new one**:
   - *Join:* scan the profile QR code from a node that is already in the
     mesh (Settings → Mesh profile → Share with another node), or paste its
     profile code, or upload its profile file.
   - *Start:* give the mesh a name and choose its links: a band, channel
     and width for each. The node proposes links for the radios it has. The
     mesh secret is generated for you.
5. **Name the node** (its callsign).
6. Review and apply. The setup Wi-Fi goes away and the node's own Wi-Fi
   takes its place; the page shows its name and passphrase as a QR code.

## 3. Add more nodes

Every node of a mesh needs the same **mesh profile**: its links, channels,
secret and regulatory mode. On any node already in the mesh, open
Settings → Mesh profile → **Share with another node**, then set up the new
node and scan that QR code in step 4.

To change the mesh's links, channels or regulatory mode, edit the profile
on any node (Settings → Mesh profile) and choose **Every node**. The node
sends the change to all the others, shows who has it, and every node
switches at the same moment, a minute later by default. A node that was
out of reach takes the change when it next hears one that has it, as long
as they still share a link. The review before sending names any node that
cannot follow and any node whose regulatory mode would change.
**This node only** is there for experiments on one node.

## 4. Connect phones and other devices

Each node runs its own Wi-Fi for the devices its user carries. The
**Devices** page shows its name, passphrase and a QR code that phones scan
to join. A device can also be plugged into one of the node's Ethernet ports.

On any node, devices get an address on the mesh's network, reach every
other device and node by address or by name (`callsign.mesh`), and use
whichever node currently has the best internet uplink. When that uplink
fails, new connections move to another within seconds.

For ATAK, see [atak.md](atak.md).

## 5. The web UI

Open **https://10.73.0.1** (or `https://node.mesh`) from a device on the
node's Wi-Fi or ports. From anywhere on the mesh, a node answers at
`https://<callsign>.mesh`. The `.mesh` names work where the device asks the
node for names: turn off a phone's Private DNS or a browser's secure DNS if
they don't resolve.

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

**Themes.** The button at the foot of the menu (at the top on a phone)
cycles a dark theme, a light theme and a red night theme that keeps night
vision. Settings → Appearance chooses the dark and the light one: Trellis's
own Dusk and Day, or editor themes such as Nord, Gruvbox, Dracula, Tokyo
Night, Catppuccin, One Dark, Solarized, Everforest and Rosé Pine. The
choice is kept in the browser.

Everything in the UI is also in `meshctl` on the node (`meshctl status`,
`meshctl radios`, `meshctl doctor`) and in the [HTTP API](api.md).

## 6. Radios and mesh links

A mesh profile can have several **links**, each on its own band and
channel: for example 5 GHz for speed, 2.4 GHz and HaLow for reach. Every
node uses the links it has radios for, all at once, and traffic takes the
fastest path to each node. Nodes with different radios still mesh through
the links they share.

- One radio carries one link. A dual-band USB adapter uses one band at a
  time; a card with two radios carries two links.
- A node with fewer radios than links takes the first links in the
  profile's order that its radios can carry. Put the link most nodes should
  use first.
- On the **Radios** page you can pin a link to a radio, turn a link off on
  this node, or ignore a radio. "Automatic" lets the node decide.

**Busy channels.** Settings → Channels shows, for each link, how busy its
channel is at every node right now, and after a survey, how busy every
channel of the band was at the busiest node. **Survey channels** has every
node listen across its links' bands for a few seconds. When a channel is
clearly better, one button moves the link there on every node, as a
mesh-wide change.

Higher bands are faster but reach less far, and a link only works if both
ends hear each other well enough. When a link is established but cannot
carry traffic (one end hears the other too weakly), the node says so under
Alerts: move the nodes closer, use a narrower width, or let them meet on a
band that reaches further.

## 7. Regulatory modes

Each mesh profile sets how the radios are limited:

- **Country**: the selected country's licence-exempt channels and power.
- **Unrestricted**: every channel, width and power the hardware allows,
  for operators who hold their own frequency authorization. Using it
  without one is unlawful in most countries.

The mode in force is always shown (Overview, Settings → System,
`meshctl status`), with a notice while unrestricted mode is on.

## 8. When something is wrong

- **Alerts** names the problem and what to do about it.
- `sudo meshctl doctor` checks the kernel, batman-adv, radios, helpers and
  configuration, and gives a report you can paste into an issue.
- `meshctl status` shows the whole node in text.
- Bug reports and hardware results are welcome: see
  [CONTRIBUTING.md](../CONTRIBUTING.md).
