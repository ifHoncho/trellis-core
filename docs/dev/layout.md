# Code layout

One Go module (`meridian/trellis`), one binary (`cmd/trellis`) with
subcommands `meshd`, `meshctl`, `webd` and `crda`. Everything else is
under `internal/`, except `tools/fakemeshd`, a development stand-in for
`meshd` that serves the API from made-up data (`make ui-dev`).

| Package | What it does | Kernel interface |
|---|---|---|
| `config` | Mesh profile and node config, schema v0, defaults, validation that names fields | — |
| `nodestate` | Persistent identity (bat0 MAC, node ID) and radio memory under `/var/lib/trellis` | — |
| `gnl` | Generic netlink connection that replaces its socket after any failed request and retries interrupted dumps | generic netlink |
| `batadv` | batman-adv client: version gate, settings get/set, hard interfaces, originators, neighbours, gateways, multicast flags | generic netlink `batadv` |
| `nl80211` | Radio capabilities (bands, channels, power, iftypes, combinations), interface create/delete, regulatory domain, TX power, station info, events | generic netlink `nl80211` |
| `rtnl` | Link helpers and batman-adv link creation with `IFLA_BATADV_ALGO_NAME` | rtnetlink |
| `sysfs` | Bus, port, USB/PCI ids, serial, USB speed, rfkill for a wiphy | sysfs |
| `hwdb` | Embedded hardware database (`hwdb.yaml`) | — |
| `regdb` | Reads wireless-regdb's `regulatory.db`; the permissive rule set; what `trellis crda` answers the kernel (architecture §5.1) | — |
| `radio` | Inventory with stable fingerprints; role planner (architecture §5.2) | — (pure logic) |
| `wpa` | wpa_supplicant mesh config rendering; control-socket client and events | unix sockets |
| `proc` | Child process supervisor with restart backoff | — |
| `hostapd` | hostapd config rendering, AP channel choice, Wi-Fi QR payload | — |
| `dnsmasq` | dnsmasq config rendering (DHCP pool, `.mesh` DNS) and lease-file parsing | — |
| `nft` | The bridge-family nftables ruleset for `bat0` and client ports; apply, verify, read counters | exec `nft` |
| `sysctl` | `/proc/sys` get/set with change detection | procfs |
| `gossip` | Node-to-node records (architecture §12.5): format, HMAC, replay-safe table, multicast socket | UDP multicast |
| `probe` | WAN health: ICMP rounds out of one interface, healthy/unhealthy hysteresis | raw ICMP socket |
| `dhcpc` | DHCPv4 client over a link-layer socket: probing auto ports, holding an auto WAN's lease (renew, rebind, expiry) | AF_PACKET socket |
| `reconcile` | The loop: ordered reconcilers, kicks, timer, teardown in reverse | — |
| `meshd` | The daemon: facts, reconcilers (`host`, `radios` with the regulatory domain, `meshlink` and `meshlinks` (one per mesh link, with the moved-peer and one-sided-link checks), `batman`, `ports`, `bridge`, `filters`, `anycast`, `gateway`, `gossip`, `ap`, `dhcp`), events, status assembly; and behind the API: configuration writes with dry runs (`apiconfig.go`), setup mode (`setup.go`), the admin password (`webauth.go`), topology (`topology.go`), radio identify (`identify.go`) | all of the above |
| `api` | API v1 (docs/api.md): the server meshd runs on its unix socket, the request and answer types, and the client meshctl and webd use | — |
| `status` | The status JSON types (the contract between meshd, meshctl and webd) | — |
| `qr` | QR code encoder (byte mode, versions 1–40) for the Wi-Fi join code and the profile code; draws as SVG, as rows for the UI, or in a terminal | — |
| `webd` | The web UI and network API: listeners from systemd, the mesh-only connection guard, TLS certificate, sessions, the proxy to meshd, the live event stream, and the UI's files (`ui/`, built in) | unix socket to meshd |
| `doctor` | Host preflight checks (architecture §6.1) and the paste-able report | sysfs, nl80211, exec |
| `meshctl` | The CLI | — |

Generated code: `internal/batadv/consts_gen.go` and
`internal/nl80211/consts_gen.go` come from the kernel uapi headers via
`tools/genconst/genconst.py` (`make gen`). Never edit them by hand; the
attribute numbers in the kernel are the source of truth.

## Reconcilers

They run in this order on every pass; each is idempotent and reports whether
it changed anything.

1. `host`: directories, NetworkManager unmanaged-devices file.
2. `radios`: inventory, fingerprints, role plan with removal debounce, create
   `mesh0`/`ap0`, remove kernel interfaces on managed radios, rfkill,
   underlay MTU, regulatory domain, channel sanity.
3. `meshlink`: render wpa_supplicant config, supervise it, read its state,
   apply the TX power cap, record SAE failures.
4. `batman`: module version gate, `bat0` (BATMAN_V, identity MAC), settings
   from architecture §6, hard interfaces (`mesh0` and wired `mesh` ports).
5. `bridge`: `br-mesh` with `bat0`, `ap0` and wired client ports, node
   address `10.73.N.1/16`.
6. `filters`: the nftables `bridge trellis` table (architecture §7.7),
   applied atomically with `nft -f` and re-applied if anything removes it.
7. `anycast`: `anycast0`, a macvlan on `br-mesh` with `10.73.0.1/32` and the
   mesh-wide MAC `02:74:72:65:6c:01`, plus the §7.4 sysctls and a permanent
   bridge FDB entry so the bridge hands those frames to it without flooding.
   Withheld while the filters are not in place: it would leak mesh-wide.
8. `gateway`: WAN detection and probing, batman-adv `gw_mode`, gateway
   selection (plus a 500 ms monitor goroutine that kicks the loop on a
   change), policy rules and tables, the `inet trellis` table (pinning marks,
   NAT), IPv4 forwarding, and the DNS upstream for dnsmasq.
9. `gossip`: the gossip socket on `br-mesh`, this node's record (sent on
   change), expiry, and node ID conflict warnings.
10. `ap`: hostapd on `ap0` (WPA2-PSK, SSID = callsign, generated passphrase
   kept in `identity.yaml`), channel chosen away from the mesh frequency, a
   transmit power cap when the AP has its own radio.
11. `dhcp`: dnsmasq serving `10.73.N.100–199` with router and DNS `10.73.0.1`,
   authoritative (foreign leases are NAKed), `.mesh` names for every node
   from gossip, outside names forwarded to the selected gateway.

Teardown runs in reverse and removes everything the reconcilers created,
restoring the kernel's wlan interface on radios Trellis took over.

## Where things live at runtime

| Path | Contents |
|---|---|
| `/etc/trellis/node.yaml`, `/etc/trellis/profile.yaml` | configuration |
| `/var/lib/trellis/identity.yaml` | node MAC, ID and the generated AP passphrase |
| `/var/lib/trellis/web-admin.yaml` | the web UI's admin password, as a salted hash (0600) |
| `/var/lib/trellis/setup-profile.yaml` | the private mesh profile of a node in setup mode; removed once it is provisioned |
| `/var/lib/trellis-web/` | webd's TLS certificate and key (owned by webd's own user) |
| `/var/lib/trellis/dnsmasq.leases` | DHCP leases this node handed out |
| `/run/trellis/hostapd-ap0.conf`, `/run/trellis/hostapd/` | generated hostapd config (0600) and its control socket |
| `/run/trellis/dnsmasq.conf` | generated dnsmasq config |
| `/var/lib/trellis/radios.yaml` | radio memory: kernel interface names, last roles |
| `/run/trellis/meshd.sock` | API socket |
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
for more mesh links, HaLow among them).
`tools/uishot.py` takes screenshots of it with a headless Firefox, at any
window size and in any theme, and reports JavaScript errors:

```sh
tools/uishot.py --theme night --out /tmp/shots go:#/mesh wait:5 shot:mesh-night
tools/uishot.py --size 500x900 --out /tmp/shots go:#/devices wait:2 full:phone-devices
```
