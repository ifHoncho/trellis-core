# Code layout

One Go module (`meridian/trellis`). The main binary (`cmd/trellis`) has
subcommands `meshd`, `meshctl`, `webd` and `crda`, and is static.
Everything else is under `internal/`, except:
- `cmd/trellis-voiced`, push-to-talk voice: a binary of its own, because
  it loads libopus, which links it dynamically (`make static` checks that
  nothing pulls that into `trellis`);
- `cmd/trellis-deploy`, the program operators run on their own computer to
  install or upgrade many nodes over SSH (`make deploy-tool`; Linux, macOS
  and Windows);
- `tools/fakemeshd`, a development stand-in for `meshd` that serves the API
  from made-up data (`make ui-dev`);
- `tools/mkdeb`, which builds and signs the Debian packages (`make deb`);
- `tools/aptrepo`, which keeps the apt repository (`make apt-repo`).

| Package | What it does | Kernel interface |
|---|---|---|
| `config` | Mesh profile and node config, schema v0, defaults, validation that names fields | — |
| `nodestate` | Persistent identity (bat0 MAC, node ID) and radio memory under `/var/lib/trellis` | — |
| `gnl` | Generic netlink connection that replaces its socket after any failed request and retries interrupted dumps | generic netlink |
| `batadv` | batman-adv client: version gate, settings get/set, hard interfaces, originators, neighbours, gateways, multicast flags | generic netlink `batadv` |
| `wg` | WireGuard interfaces: read and configure (key, port, mark, peers changed in place), keys | generic netlink `wireguard` |
| `nl80211` | Radio capabilities (bands, channels, power, iftypes, combinations), interface create/delete, regulatory domain, TX power, station info, events | generic netlink `nl80211` |
| `rtnl` | Link helpers and batman-adv link creation with `IFLA_BATADV_ALGO_NAME` | rtnetlink |
| `sysfs` | Bus, port, USB/PCI ids, serial, USB speed, rfkill for a wiphy | sysfs |
| `hwdb` | Embedded hardware database (`hwdb.yaml`) | — |
| `gnss` | Follows a GNSS receiver through gpsd's JSON stream: fix, position, accuracy, satellites, PPS; writes a fix as NMEA (GGA, RMC) | TCP to gpsd |
| `cot` | Cursor on Target: writes a node's position event (XML), and reads a device's own position report from an SA datagram (XML or TAK protocol version 1) | — |
| `chrony` | Reads chronyd's state through `chronyc` and writes the mesh's time servers for it | exec `chronyc` |
| `regdb` | Reads wireless-regdb's `regulatory.db`; the permissive rule set; what `trellis crda` answers the kernel (architecture §5.1) | — |
| `radio` | Inventory with stable fingerprints; role planner (architecture §5.2) | — (pure logic) |
| `wpa` | wpa_supplicant mesh config rendering; control-socket client and events | unix sockets |
| `proc` | Child process supervisor with restart backoff | — |
| `hostapd` | hostapd config rendering, AP channel choice, Wi-Fi QR payload | — |
| `dnsmasq` | dnsmasq config rendering (DHCP pool, `.mesh` DNS) and lease-file parsing | — |
| `nft` | The bridge-family nftables ruleset for `bat0` and client ports; apply, verify, read counters | exec `nft` |
| `sysctl` | `/proc/sys` get/set with change detection | procfs |
| `gossip` | Node-to-node records (architecture §12.5): format, HMAC, replay-safe table, multicast socket | UDP multicast |
| `probe` | WAN health: ICMP rounds out of one interface, healthy/unhealthy hysteresis; the web check that spots a login page | raw ICMP socket, HTTP out of one interface |
| `dhcpc` | DHCPv4 client over a link-layer socket: probing auto ports, holding an auto WAN's lease (renew, rebind, expiry) | AF_PACKET socket |
| `reconcile` | The loop: ordered reconcilers, kicks, timer, teardown in reverse | — |
| `meshd` | The daemon: facts, reconcilers (below), events, status assembly; and behind the API: configuration writes with dry runs (`apiconfig.go`), mesh-wide profile changes (`meshchange.go`) and settling those made apart (`meshconflict.go`), channel surveys (`survey.go`) and the channel busy alert (`congestion.go`), the checks on a mesh link's channel in the kernel (`meshwatch.go`), node positions (`positions.go`) and the node as its devices' GPS (`phonegps.go`), finding the mesh again (`findmesh.go`) with the nodes heard lately (`roster.go`), setup mode (`setup.go`), the admin password (`webauth.go`), topology (`topology.go`), radio identify (`identify.go`), upgrades through the API (`apiupgrade.go`), mixed-release warnings (`releases.go`) | all of the above |
| `api` | API v1 (docs/api.md): the server meshd runs on its unix socket, the request and answer types, and the client meshctl and webd use | — |
| `status` | The status JSON types (the contract between meshd, meshctl and webd) | — |
| `qr` | QR code encoder (byte mode, versions 1–40) for the Wi-Fi join code and the profile code; draws as SVG, as rows for the UI, or in a terminal | — |
| `webd` | The web UI and network API: listeners from systemd, the mesh-only connection guard, TLS certificate, sessions, the proxy to meshd, the live event stream, the voice console's WebSocket (a server of its own, `websocket.go`) passed to voiced, and the UI's files (`ui/`, built in) | unix socket to meshd |
| `doctor` | Host preflight checks (architecture §6.1) and the paste-able report | sysfs, nl80211, exec |
| `meshctl` | The CLI | — |
| `version` | Version strings: releases, builds between them, development builds; the release series two nodes must share; the Debian form | — |
| `opus` | Opus voice encoder and decoder over the system's libopus, loaded at run time without cgo. A binary that imports it is dynamically linked, so the `trellis` binary does not (for the planned voice service) | dlopen `libopus.so.0` |
| `rtp` | RTP packets and the RTCP voice uses (sender reports, CNAME, BYE): writing, and parsing what comes from the network | — |
| `voice` | The push-to-talk engine of `trellis-voiced`: talk groups, two channels, jitter buffer, half-duplex gate, tones, mixing; the multicast transport. `voice/vopus` is its Opus codec, `voice/alsa` its audio (libasound at run time), `voice/devices` finds sound cards in sysfs by identity, `voice/cm108` reads PTT and buttons from CM108 adapters (OpenVLM), `voice/wav` is file audio for the lab, `voice/pttline` serial and GPIO PTT lines and outputs, `voice/wire` the console's framing between webd and voiced; consoles (`console.go`) serve the browser | UDP multicast |
| `deb` | Writes and reads Debian packages (ar, tar, gzip), and signs them: an Ed25519 signature in a member of the archive that dpkg skips | — |
| `release` | The release keys a node trusts: built in (`trusted.keys`) and the operator's (`/etc/trellis/release-keys`) | — |
| `rollout` | A rolling mesh-wide upgrade: the order nodes take a release in, whose turn it is, halting on a rollback; moving packages between nodes, checked by hash. meshd's side: `rollout.go` (this node's part, sending and asking, the package server), `rollout_api.go` (the origin), `rollout_upgrader.go` (the real upgrade and the lab's stand-in) | — |
| `deploy` | `trellis-deploy`: the hosts file, fetching a release from GitHub and checking it (checksums, signatures against the built-in keys), looking at each node, the plan, and the run (installs in parallel, upgrades one at a time, reboots, a dropped connection); the system's `ssh` runs it all | exec `ssh` |
| `prepare` | `meshctl prepare`: what a node's system needs once the packages are in (the pinned batman-adv, mt7915e, driver options, regulatory rules), through the package's scripts; reports a needed reboot in `/run/reboot-required` | exec `modprobe`, `systemctl`, the scripts |
| `upgrade` | An upgrade that goes back: check, keep the running version and configuration, install, health check, rollback, resume after a power loss; started detached (`trellis-upgrade-run`) by `meshctl` and by meshd's API alike | exec `dpkg`, `systemctl`, `systemd-run`; meshd's API |

Generated code: `internal/batadv/consts_gen.go` and
`internal/nl80211/consts_gen.go` come from the kernel uapi headers via
`tools/genconst/genconst.py` (`make gen`). Never edit them by hand; the
attribute numbers in the kernel are the source of truth.

## Reconcilers

They run in this order on every pass; each is idempotent and reports whether
it changed anything.

1. `host`: directories, NetworkManager unmanaged-devices file.
2. `firewall`: the `inet trellis_fw` table that closes the node to what
   comes from outside the mesh (architecture §11), put back if anything
   removes it; left in place while meshd is stopped.
3. `radios`: inventory, fingerprints, role plan with removal debounce, create
   the mesh and AP interfaces, remove kernel interfaces on managed radios,
   rfkill, underlay MTU, regulatory domain, channel sanity.
4. `meshlink`, `meshlinks`: one per mesh link. Render the wpa_supplicant
   config, supervise it, read its state, record SAE failures; remove a
   peer's leftover entry when it moved to another link, and a peer the
   kernel does not hold as established; restart a link the kernel shows
   out of its mesh or on another channel than the profile's
   (architecture §13, Channel moves).
5. `sitelink`: on a member of the profile's site link, `site0` (WireGuard)
   with a peer per member of another site, a gretap to each, the tunnel
   addresses' table, the rules that keep WireGuard's packets off the
   mesh, and the `inet trellis_site` table that keeps the tunnels'
   packets inside WireGuard (architecture §13, Site link); on any other
   node, none of it.
6. `batman`: module version gate, `bat0` (BATMAN_V, identity MAC), settings
   from architecture §6, hard interfaces (every mesh link and wired `mesh`
   ports; one that will not come up is a warning, not the end of the
   pass), and the broadcast warm-up and the gossip record at `bat0`'s
   first neighbour.
7. `ports`: wired ports and their roles (`auto` probes each one).
8. `bridge`: `br-mesh` with `bat0`, the AP and wired client ports, node
   address `10.73.N.1/16`.
9. `filters`: the nftables `bridge trellis` table (architecture §7.7),
   applied atomically with `nft -f` and re-applied if anything removes it.
10. `anycast`: `anycast0`, a macvlan on `br-mesh` with `10.73.0.1/32` and the
   mesh-wide MAC `02:74:72:65:6c:01`, plus the §7.4 sysctls and a permanent
   bridge FDB entry so the bridge hands those frames to it without flooding.
   Withheld while the filters are not in place: it would leak mesh-wide.
11. `gateway`: WAN detection and probing, batman-adv `gw_mode`, gateway
   selection (plus a 500 ms monitor goroutine that kicks the loop on a
   change), policy rules and tables, the `inet trellis` table (pinning marks,
   NAT), IPv4 forwarding, and the DNS upstream for dnsmasq.
12. `time`: the mesh's time servers for chrony, from gossip.
13. `survey`: each link's channel load, sampled every 10 s, and the
   channel busy alert from every node's.
14. `findmesh`: a node that hears half or fewer of the nodes it knew
   scans for the rest on other channels and visits where it hears them
   (architecture §12.6); also the alert after a conflict was settled.
15. `gossip`: the gossip socket on `br-mesh`, this node's record (sent on
   change), expiry, node ID conflict warnings, and asking for a newer mesh
   profile.
16. `positions`: this node's position on the ATAK map (its receiver's,
   else that of a device on its own ports), the last known positions of
   nodes that left, and NMEA to the devices on its own ports.
17. `ap`: hostapd (WPA2-PSK, SSID = callsign, generated passphrase kept in
   `identity.yaml`), channel chosen away from the mesh frequency, or the
   mesh link's when they share a radio.
18. `dhcp`: dnsmasq serving `10.73.N.100–199` with router and DNS `10.73.0.1`,
   authoritative (foreign leases are NAKed), `.mesh` names for every node
   from gossip, outside names forwarded to the selected gateway.

Teardown runs in reverse and removes everything the reconcilers created,
restoring the kernel's wlan interface on radios Trellis took over.

## Where things live at runtime

| Path | Contents |
|---|---|
| `/etc/trellis/node.yaml`, `/etc/trellis/profile.yaml` | configuration |
| `/var/lib/trellis/identity.yaml` | node MAC, ID, the generated AP passphrase and the site link's private key. `identity.yaml.bak` is its copy, used when the file is damaged or missing (an empty file with no backup gets a new identity); remove both to give a node a new identity |
| `/var/lib/trellis/web-admin.yaml` | the web UI's admin password, as a salted hash (0600) |
| `/var/lib/trellis/setup-profile.yaml` | the private mesh profile of a node in setup mode; removed once it is provisioned |
| `/var/lib/trellis-web/` | webd's TLS certificate and key (owned by webd's own user) |
| `/var/lib/trellis/dnsmasq.leases` | DHCP leases this node handed out |
| `/run/trellis/hostapd-ap0.conf`, `/run/trellis/hostapd/` | generated hostapd config (0600) and its control socket |
| `/run/trellis/dnsmasq.conf` | generated dnsmasq config |
| `/var/lib/trellis/radios.yaml` | radio memory: kernel interface names, last roles |
| `/var/lib/trellis/mesh-change.json` | a mesh-wide profile change waiting for its moment (0600: it holds the mesh secret) |
| `/var/lib/trellis/rollout.json` | the mesh-wide upgrade this node takes part in, and its phase (kept across the restart its own upgrade makes) |
| `/var/lib/trellis/upgrade/releases/` | a mesh-wide upgrade's packages, by SHA-256, until it is over (0700) |
| `/var/lib/trellis/known-nodes.json` | the other nodes heard in the last day, with ages (finding the mesh again) |
| `/var/lib/trellis/profile-dropped.yaml` | the profile dropped when two parts of the mesh had changed it apart (0600) |
| `/run/trellis/meshd.sock` | API socket |
| `/etc/trellis/release-keys/*.pub` | release keys the operator trusts besides the built-in ones |
| `/var/lib/trellis/upgrade/` | the last upgrade: its log and outcome, the previous version's package and configuration (0700); while one is under way also its state, the old binary, and `/etc/systemd/system/trellis-upgrade-resume.service` |
| `/usr/share/trellis/scripts/`, `/usr/share/trellis/packaging/` | the hardware scripts and what they install, as in the repository |
| `/run/trellis/wpa-mesh0.conf`, `/run/trellis/wpa/` | generated wpa_supplicant config and its control sockets |

## The web UI's files

`internal/webd/ui/` is served as it is: there is no build step, so what is
in the folder is what a browser gets.

| File | What it is |
|---|---|
| `index.html`, `app.css`, `favicon.svg` | the page, the three themes (dusk, day, night), the mark |
| `fonts/` | Source Code Pro and Cantarell with their licences (SIL OFL 1.1) |
| `js/theme.js` | sets the theme before the first paint |
| `js/app.js` | session, routing, live data, the frame around every view |
| `js/dom.js` | `h()` to build elements and `morph()` to change only what differs |
| `js/api.js`, `js/state.js`, `js/fmt.js`, `js/ui.js` | the API client, the one state object, formatting, shared components |
| `js/topo.js` | the topology drawing and its layout |
| `js/config.js` | the node-config draft and the dry run, confirm, apply sequence |
| `js/scan.js` | reading a QR code with the camera |
| `js/views/` | one file per page; `gate.js` is signing in and the setup wizard |

Rules the code keeps to (a test checks the first two):

- nothing is loaded from outside the node;
- no `eval`, no inline scripts or styles: the page's content security policy
  forbids them (styles are set through the CSSOM);
- every state is shown by a shape or a word as well as a colour, because
  the night theme has one hue.

To work on it: `make ui-dev` serves the UI on `http://127.0.0.1:8480`
against `tools/fakemeshd` (`SETUP=1` for the setup wizard, `LINKS=2` or `3`
for more mesh links, HaLow among them, `UNRESTRICTED=1` for a mesh in
unrestricted regulatory mode, `PORTAL=1` for an uplink behind a login page).
`tools/uishot.py` takes screenshots of it with a headless Firefox, at any
window size and in any theme, and reports JavaScript errors:

```sh
tools/uishot.py --theme night --out /tmp/shots go:#/mesh wait:5 shot:mesh-night
tools/uishot.py --size 500x900 --out /tmp/shots go:#/devices wait:2 full:phone-devices
```
