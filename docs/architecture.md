# Meridian Trellis — Architecture

*Meridian Systems. How a Trellis node works, and why it is built that way.*

This document describes what Meridian Trellis ("Trellis") is, how a node works, and why each
major decision was made. Rationale is kept short and marked **Why:**. Where a
decision leans on behaviour in batman-adv or another component, the source
is named so it can be re-checked.

---

## 1. What this is

An open-source tactical mesh radio system: a body-worn node, in the class of
an MPU5, that gives a team a self-forming encrypted IP network with first-class
ATAK support and opportunistic internet sharing, over Wi-Fi and Wi-Fi HaLow
for range.

It ships two ways from one codebase:

- **Self-install** onto a supported Debian-family system.
- **Prebuilt images** for specific hardware.

## 2. Requirements

| ID | Requirement |
|---|---|
| R1 | ATAK works across the whole mesh with no EUD configuration beyond joining the node's Wi-Fi: multicast SA and chat, plus unicast to TAK Servers. |
| R2 | Any EUD or node can reach any other, and any service on them (TAK Server, web, files), with no internet gateway present. |
| R3 | Any node with a WAN uplink shares internet with the whole mesh, automatically. |
| R4 | When a gateway fails and another exists, new connections move to it within **5 s**, invisibly to EUDs. |
| R5 | Mesh links are authenticated and encrypted. |
| R6 | Supports standard Wi-Fi mesh radios and Wi-Fi HaLow (802.11ah). |
| R7 | Web UI and API for management and status. |
| R8 | Optimised for 1–50 nodes, typically one body-worn node per person with their phone attached. |
| R9 | Delivered as a self-installable package and as prebuilt images. |

**Non-goals (v1):** IPv6 for clients; keeping open TCP connections alive across
a gateway failover; meshes of hundreds of nodes; interoperability with
proprietary radios.

## 3. Design principles

1. **The EUD just works.** Join the node's Wi-Fi; nothing else to configure, on any node.
2. **Disconnected first.** Everything inside the mesh works with no WAN. Internet is a bonus.
3. **Airtime is the scarcest resource.** No periodic mesh-wide flooding beyond batman-adv's own routing messages; prefer unicast.
4. **Use the protocol's own mechanism before building one.** batman-adv already does gateway announcement, ARP caching, loop avoidance and efficient multicast.
5. **Reconcile, don't script.** One declarative config; a loop that converges the system to it, safe to rerun at any time.
6. **Own the kernel interface.** Configure through netlink rather than distro tooling, so one binary runs on every platform.
7. **Fail safe.** Never route through an unverified gateway; never lock the operator out.
8. **If it isn't tested, it doesn't work.** Every behaviour in this document has an automated test or a recorded test procedure.

## 4. System overview

```mermaid
flowchart TB
    subgraph node["One node"]
        direction TB
        AP["Client AP<br/>5 GHz, low power"]
        USB["Wired EUD port<br/>(Ethernet / USB)"]
        AC["anycast0<br/>10.73.0.1 + fixed MAC"]
        BR["br-mesh<br/>bridge, 10.73.N.1/16"]
        BAT["bat0<br/>batman-adv BATMAN_V"]
        RADIO["Mesh radio<br/>802.11s + SAE<br/>(Wi-Fi or HaLow)"]
        WAN["WAN (optional)<br/>Ethernet / LTE / Starlink"]
        AP --- BR
        USB --- BR
        AC --- BR
        BR --- BAT
        BAT --- RADIO
        BR -. "routed + NAT<br/>when gateway" .- WAN
    end
    RADIO <-->|"other nodes"| MESH(("Mesh"))
```

A node is a bridge. `bat0` (the mesh) and the client ports are members of one
bridge, so the whole mesh is a single flat IPv4 network. Every node also
answers as the same **anycast gateway** for its own clients and routes their
internet traffic to whichever WAN gateway is currently best.

| Traffic | Path | Depends on a WAN gateway? |
|---|---|---|
| EUD → EUD or node service | Layer 2 across batman-adv | No |
| ATAK multicast | batman-adv multicast to listening nodes | No |
| EUD → internet | EUD → local node (anycast) → best gateway → NAT → WAN | Yes |
| Internet → EUD (replies) | WAN → gateway → layer 2 straight to EUD | Yes |

## 5. Radio link layer

- **802.11s mesh point with SAE (WPA3)**, managed through `wpa_supplicant`
  (upstream, or Morse Micro's `wpa_supplicant_s1g` fork for HaLow).
- **802.11s forwarding off** (`mesh_fwding=0`); batman-adv owns all paths.
- **Regulatory mode** is set in the mesh profile; the default is
  `unrestricted`, at maximum transmit power (§5.1).
- **Underlay MTU** raised to carry batman-adv headers (1500 + overhead), with
  `bat0`'s resulting MTU checked. Every mesh interface must allow at least 1280
  bytes (§9).
- **Open IBSS** is supported only as a legacy fallback for radios without
  mesh-point support. It is unencrypted and is flagged in the UI.

**Why:** SAE gives per-hop authentication and encryption, so only nodes holding
the mesh secret can join. Leaving forwarding to batman-adv avoids two routing
layers competing. On HaLow this is not optional: Morse Micro's OpenWrt scripts
force ad-hoc mode to open authentication, and refuse both mesh and ad-hoc
interfaces in EU/GB regulatory domains
(`lib/netifd/morse/morse_overrides.sh` and `lib/netifd/wireless/morse.sh`
in Morse Micro's `netifd-morse` package, part of its `morse-feed`).

### 5.1 Regulatory mode and transmit power

| Mode | Channels and power | Use |
|---|---|---|
| `unrestricted` (**default**) | Permissive regulatory database: all channels the hardware supports, transmit power at the hardware's maximum | Operators with frequency authorization; maximum range out of the box |
| `country` | The selected country's rules (ISO code, e.g. `US`); power defaults to the maximum that country allows on the channel | Operation under normal licence-exempt rules |

- **Changeable** by the user in the mesh profile (web UI, `meshctl`, or the
  config file). Every node in a mesh should use the same mode, so it lives in
  the shared profile, not per node.
- **No power limits of Trellis's own:** meshd doesn't cap any radio, mesh
  or AP. Radios transmit at what the mode allows and the driver uses.
- **Visible:** the web UI and `meshctl status` always show the active mode,
  and show a persistent notice while `unrestricted` is active.
- **The client AP follows the node's regulatory domain.** Linux applies one regulatory domain to every radio on a node
  (only drivers that manage their own regulation are exceptions), and
  hostapd sets the domain to its `country_code`. So the AP takes the mesh
  profile's country; `ap.country` in `node.yaml` is ignored, with a notice
  when it differs (it would have switched the whole node). There is no
  power cap and no channel list of Trellis's own: the automatic choice
  prefers channels without radar rules that phones expect, and a pinned
  `ap.channel` may be any the radio has and the domain allows, radar
  channels included (hostapd then checks for radar first, `ieee80211h`).
- **No channel or width limits of Trellis's own.** A channel
  number is accepted wherever the kernel can map it to a frequency, and a
  width wherever the band and the software can carry it: 20 or 40 MHz on
  2.4 GHz, up to 160 MHz on 5 and 6 GHz (wpa_supplicant up to 2.11 builds
  no 320 MHz mesh), 1 to 8 MHz on HaLow (no chip runs 16). What the radio
  lacks or the regulatory domain forbids is reported, not refused: a
  channel the radio does not have, or one the domain disables or makes
  passive-only, is an error the link then fails on; a radar (DFS) channel
  is a warning, since the mesh can start after its availability check where
  the driver and wpa_supplicant support radar detection on a mesh point;
  and a link that runs narrower than the profile asks shows its running
  width in status, with a warning that says why. wpa_supplicant places a
  160 MHz mesh itself: version 2.10 only from channel 36 or 104 (both
  blocks need radar detection in the US), later versions from any block an
  802.11ax radio and the domain allow.
- **Radar channels need the country in wpa_supplicant's configuration.**
  wpa_supplicant switches on radar handling for a mesh (`ieee80211h`, the
  channel availability check) only when its global `country` is set;
  without it, it refuses every radar channel ("not allowed for AP mode").
  meshd writes the profile's country into every mesh link's configuration.

**How `unrestricted` is implemented**

Trellis answers the kernel's regulatory requests itself, through the
userspace interface cfg80211 kept from CRDA. No kernel is rebuilt and
nothing is signed.

- **Why it works.** cfg80211 reads `/lib/firmware/regulatory.db` first. When
  that file is missing (or its signature fails), every later request goes
  to userspace instead: a `change` uevent with `COUNTRY=XX`, answered within
  3.1 s by `NL80211_CMD_SET_REG` with the rules for that alpha2 (a pending
  request with the same alpha2 is the only condition; the rules are checked
  for sane frequencies only). `CFG80211_CRDA_SUPPORT` is on by default,
  not deprecated, and enabled on common distribution kernels.
- **What Trellis installs.** The distribution's `regulatory.db` and `.p7s`
  are moved aside (`dpkg-divert`, as the fallback script does); a udev rule
  runs `trellis crda` on each request.
- **What `trellis crda` answers.** In `country` mode: the wireless-regdb
  rules for that country, read from the moved-aside database, unchanged.
  In `unrestricted` mode: one permissive rule set for whatever alpha2 was
  asked (2.4, 4.9–5.9, 5.9–7.1 GHz and 60 GHz at the widest width, no
  radar, passive-only or indoor flags, power above any radio's own limit).
  It records what it answered in `/run/trellis-crda/regdom.json`, which meshd
  reports as the active mode.
- **What it frees on the mesh** (US): 160 MHz in any block without a 60 s
  radar check or a radar shutdown; channels 169–177; 6 GHz mesh at all
  (passive-only under the US rules) at the radio's power, not 12 dBm.
- **Limits it does not lift:** firmware and calibration caps (mt76 power
  tables, ath9k EEPROM), and radios that manage their own regulation
  (Morse's HaLow driver: its board file sets power).
- **Risks.** A kernel without CRDA support needs a kernel built without
  `CFG80211_REQUIRE_SIGNED_REGDB` instead (images). After ten
  unanswered requests the kernel stops asking until reboot, so the helper
  is installed before the database is moved, and `meshctl doctor` checks
  the path. The first request after boot is spent on the failed file load;
  meshd asks again at once.
- **Switching.** Moving a node onto this path takes one reboot (the kernel
  keeps a database it has loaded); moving back takes `iw reg reload`
  (`scripts/install-regulatory.sh --undo`).

| Layer | What it takes |
|---|---|
| Linux (cfg80211) | The CRDA path above: the regulatory database moved aside, a udev rule and `trellis crda`. |
| Driver and firmware | Many chipsets enforce their own limits in firmware or calibration data regardless of the kernel database, so the effective maximum is per radio. The hardware database records known values. |
| HaLow (Morse Micro) | Power comes from the board configuration file (BCF), and Morse's OpenWrt scripts refuse mesh in EU/GB. Unrestricted HaLow needs a BCF chosen for maximum power and those checks bypassed. |

**Why:** the target users (MPU5-class deployments) typically operate under
their own frequency authorization, where licence-exempt limits don't apply and
range matters most.

**What it means for other users:** in most countries, transmitting outside the
local licence-exempt rules without authorization is unlawful, and some
countries regulate products that ship configured that way. Users without
authorization must switch to `country` mode. The docs and the first-run setup
state this plainly.

### 5.2 Radio inventory and role assignment

Nodes will carry a mix of USB and PCIe adapters, and kernel interface names
(`wlan0`, `wlan1`, `phy0`…) depend on probe order, so they change across
reboots. Trellis therefore never trusts kernel names. It identifies each
physical radio by a fingerprint, scores it, gives it a role, and creates its
own interfaces on it.

**Roles**

| Role | Interface Trellis creates | Needs |
|---|---|---|
| `mesh` | `mesh0` (802.11s mesh point) | `mesh point` mode; the mesh profile's band and channel |
| `ap` | `ap0` (client access point) | `AP` mode; the AP band (5 GHz preferred, 2.4 GHz fallback) |
| `mesh+ap` | both, on one radio and one channel | an interface combination allowing mesh point + AP together |
| `mesh2`, `mesh3`, … | `mesh1`, `mesh2`, … (further mesh links) | `mesh point` mode; the band and channel of that link in the profile's `links` |
| `mesh2+ap`, … | that link's interface and `ap0` on one radio | as `mesh+ap`, when the AP has to share that link's radio |
| `unused` | none | — |

**Several mesh links.**
- **What it is for:** the profile's optional `links` list adds mesh links
  after `radio`, each on its own radio and usually one per band: 5 GHz for
  capacity, 2.4 GHz and HaLow for reach. The mesh does not switch bands; it
  runs on all of them at once and batman-adv chooses.
- **How it works:**
  - Link *i* has its own interface (`mesh0`, `mesh1`, … up to `mesh9`),
    wpa_supplicant and channel.
  - Every link is a batman-adv hard interface. BATMAN_V compares paths
    across them by throughput and uses the best one per destination, so
    when one link fades the route moves to another.
  - A node runs the links it has radios for. Nodes with different radios
    still mesh, over the links they share.
- **Planning:**
  - The links are assigned before the AP, one radio per link. First the
    links the node config pins, then links already running on a radio
    (roles don't move while the node runs), then the rest.
  - The rest are matched so that as many links as possible get a radio:
    each takes its best-ranked free radio (table below), and an earlier
    link moves to its next choice when that is the only way a later link
    gets one. The order of the links in the profile therefore never costs
    a node a link.
  - The AP takes a free radio if one is left. Otherwise it shares the radio
    of the first link that can also run an AP on 2.4 or 5 GHz (never a
    HaLow radio).
  - A node with no radio left for a link meshes without it and says so
    (`no-mesh-radio`, `no-mesh2-radio`, `no-mesh3-radio`, …). **Any** link
    may be the one it lacks, the first included: a 2.4 GHz-only adapter in
    a mesh whose first link is 5 GHz, or a node's one radio pinned to the
    second link. While the node carries some link this is information, not
    a warning; with no link at all it is the fault `no-mesh-radio`.
  - One radio carries one link. A node with one dual-band radio and two
    links on its bands keeps the link it already runs (the first, from a
    fresh start); pin the radio to the other link to move it.
  - `radios.roles.mesh2`, `mesh3`, … pin or disable a link's role, like
    `mesh` and `ap`.
- **Cost:** batman-adv's routing messages, and its flooded broadcasts, go out
  on every link. On HaLow that airtime is expensive; per-link tuning is
  not built yet.
- **Status:** `meshctl status` shows "Mesh link 2", "Mesh link 3", …; the
  API has them in `more_links`.
- **Profile spelling:** an older profile's `radio2` is read as the first of
  the further links; files are written with `links`.
- **HaLow (`s1g`) as any link.** A link
  with `band: s1g` takes an S1G channel number and a width of 1, 2 (default),
  4 or 8 MHz. The channel numbers come from the country's plan: meshd
  knows every one Morse Micro's software has (the Americas, Europe, India,
  Japan, Korea, Singapore, Australia's group and New Zealand), with their
  operating classes and frequencies. A module may cover less (many are
  built for one region), and its driver must be loaded for the same
  country, which meshd checks.
  - meshd runs Morse Micro's `wpa_supplicant_s1g` on that link in place of
    `wpa_supplicant`, with the operating class and channel instead of a
    frequency. The primary channel is fixed by the width, so every node
    derives the same one.
  - Beacon interval 1000 TU (Wi-Fi links use 100): a HaLow beacon is slow,
    and no client AP shares the radio.
  - Morse's driver registers its channels with the kernel as 5 GHz ones; the
    hardware database's `bands: [s1g]` tells the planner what the radio is,
    so it carries a HaLow link and never a Wi-Fi role.

**Dual-band concurrent cards.** A dual-band-concurrent (DBDC) card appears
as two radios, one per band, each with its own fingerprint. The planner
treats them like two adapters: two mesh links at once, or a mesh link on
one and the client AP on the other, with no shared airtime. The two radios
share one chip, so a peer that moves its one radio between this node's
links can leave an entry behind on the chip that blocks the new link;
meshd removes such leftovers.

**Identification (fingerprint).** Each radio is identified by the first of
these that is reliable:

1. **Permanent MAC address** (rtnetlink `IFLA_PERM_ADDRESS`), if it is
   globally administered and unique among the radios present. Some cheap
   adapters ship a random or duplicated MAC, so this is checked, not assumed.
2. **Device serial or address:** USB serial number, or PCI bus address.
3. **Port path plus device ID:** where it is plugged in (sysfs path, e.g.
   USB port `1-1.2`) plus USB VID:PID or PCI ID.

The fingerprint, its last role and its measured facts are stored in node
state. Kernel names are never stored.

**Interfaces.** On radios it manages, `meshd` deletes the kernel-created
interface and creates its own (`mesh0`, `ap0`) through nl80211. Names then
never depend on probe order. NetworkManager and similar tools are told to
leave Trellis-managed radios alone.

**Default assignment.** Hard requirements first, then ranking.

| Step | Mesh radio | AP radio |
|---|---|---|
| 1. Eligible | supports `mesh point` and the profile's band; not marked broken in the hardware database | supports `AP`; not the mesh radio |
| 2. Rank by | **highest** transmit power on the mesh channel | **lowest** transmit power |
| 3. Then by | more spatial streams / antennas | supports 5 GHz; on a different band from the mesh |
| 4. Then by | bus: PCIe > USB 3 > USB 2 | — |
| 5. Tie-break | fingerprint order, so the result is identical every boot | same |

For a HaLow mesh profile, only HaLow (S1G) radios are eligible for `mesh`. A
node with HaLow plus Wi-Fi therefore puts HaLow on the mesh and Wi-Fi on the
AP.

**Why highest power for mesh, lowest for AP:** the mesh link sets the team's
range. The AP is a body-area link to the user's own phone and needs little
range (§10). Giving it the weakest radio keeps the strong one for the
backhaul.

**Where "transmit power" comes from.** In order of trust:

1. **Measured** and recorded in the hardware database (below).
2. **Hardware database** value for that chipset or adapter.
3. **Driver-reported** maximum per channel (nl80211 frequency attributes),
   read under the active regulatory mode.

Driver numbers are the weakest source. Many drivers report a generic or
regulatory value. In `country` mode, every radio may show the same capped
figure. The UI always shows which source a number came from.

**Fewer than two radios**

| Radios present | Result |
|---|---|
| One, supports mesh point + AP together | `mesh+ap` on the mesh channel, with a warning: the AP shares airtime and band with the mesh |
| One, mesh only | `mesh`; no AP. The EUD connects by cable; UI warning |
| None mesh-capable | No mesh. The node raises an error and serves only local clients |

**More radios:** further mesh links in the profile use them, one radio
per link; any radio left over stays `unused`.

**Changes while running**

- **Roles are recomputed at boot** from the radios present. The same hardware
  gives the same result every time, because ranking ends in a stable
  tie-break.
- **At runtime, roles only change when a radio is lost.** If the mesh radio
  disappears, mesh moves to the best remaining eligible radio, taking it from
  the AP if necessary (mesh outranks AP). Removals are debounced for a few
  seconds, so a flaky USB connection doesn't thrash roles.
- **A newly plugged radio only fills an empty role.** It never takes the mesh
  from a working radio mid-mission. If it would rank higher, the UI shows the
  recommendation, and the next boot applies it automatically.
- **The operator's changes apply at once.** A pin set or removed, a radio
  ignored or given a power figure, the AP switched, or the profile's links
  on other bands: the plan is made afresh, as at boot. The change's preview
  says which links move before it is applied. A label is not such a change.

**Why:** "the strongest radio carries the mesh" holds at every boot, but an
operator plugging in a cable never drops the mesh link by surprise.

**Overrides** (node config, web UI and API)

| Setting | Values |
|---|---|
| Role assignment, per role | `auto` (default); pinned to a radio (by fingerprint); pinned to a port ("whatever is in USB port 2"); or disabled |
| Per radio | ignore; label; manual power value overriding the database (for ranking) |

Pinning to a port suits fleets of identical builds where adapters get swapped.
Pinning to a radio suits one-off builds.

**Web UI (Radios page).** For every radio, the page lists:
- label, chipset, driver, bus and port;
- bands, maximum power and its source, spatial streams;
- capabilities (mesh, AP, both at once);
- its current role and the recommended role.

Actions: assign, pin, ignore, and **identify** (blink the adapter's LED, where
the driver exposes one). Before a change that drops the mesh link is applied,
the page says so.

**Hardware database.** A small file in the repo, keyed by USB VID:PID, PCI ID
and driver. For each entry it records:
- chipset and maximum transmit power per band;
- whether mesh point and SAE have been verified;
- the kernel versions it was verified on;
- quirks, such as no DFS channels in AP mode, or needing particular firmware.

Adapter testing ([hardware.md](hardware.md)) fills it in, and anyone can
contribute an entry. A radio with no entry still works, on driver-reported
values.

## 6. Mesh layer (batman-adv)

| Setting | Value | Why |
|---|---|---|
| Version | **batman-adv ≥ 2024.0** on every node | Multicast packet type (§9). One older node makes the whole mesh fall back to flooding. |
| `routing_algo` | `BATMAN_V` | Throughput-based metric; much better than IV on mixed-rate radio links. |
| `distributed_arp_table` | **off** | It would answer ARP from a mesh-wide cache instead of flooding, but it only learns a client when that client's ARP crosses `bat0`. With the gateway answered locally on every node (§7.4) a client's ARP never does, so the rest of the mesh could not resolve it unless its node happened to be a DHT candidate for that address. Plain ARP flooding is cheap at the volumes a client network produces. |
| `bridge_loop_avoidance` | on | Required once nodes bridge client ports: two nodes plugged into the same switch would otherwise loop. |
| `fragmentation` | on | Survives underlays that can't carry the full overhead. |
| `multicast_mode` | 1 (optimisations on) | Required for the multicast packet type and listener-only delivery (§9). |
| `gw_mode` | `server` on healthy gateways, `client` elsewhere | Gateway announcement and withdrawal ride on batman's own routing messages (§8). |
| `orig_interval` | 1000 ms (tunable) | Sets detection latency; balanced against airtime. |

**Why batman-adv at all:** it makes the mesh one layer-2 domain with dynamic
multi-hop routing, which is what ATAK multicast, mDNS and moving EUDs want
(§7). The alternative, per-hop IP routing, is costly: layer-3 multicast
echoing can cause exponential storms at three or more nodes.

### 6.1 Getting the same batman-adv onto every kernel

Every node in a mesh runs **one pinned batman-adv release**, whatever its
kernel. The pinned release is currently **v2026.3** (2026-08-31). Its
`README.external.rst` says it builds against **Linux 5.15–7.3**, which
covers:
- Ubuntu 22.04's 5.15 and later;
- the vendor kernels of common single-board computers;
- Debian 13 and current Raspberry Pi OS.

| Path | How batman-adv gets there |
|---|---|
| Images | Built into the image, at the pinned release |
| Self-install | A `trellis-batman-adv-dkms` package. DKMS builds the pinned release against the running kernel at install, and again on every kernel update. The module goes into `updates/`, which takes precedence over the kernel's own copy |
| Kernels below 5.15, or ones failing the checks below | Not supported by self-install. Use an image or a board kernel built by Trellis |

**Host kernel requirements.** The installer and `meshctl doctor` check all of
these:

| Requirement | Why |
|---|---|
| `CONFIG_BATMAN_ADV` is `m` or unset, never `y` | A built-in batman-adv can't be replaced by the pinned module |
| `CONFIG_BRIDGE` and `CONFIG_BRIDGE_IGMP_SNOOPING=y` | batman's multicast listener detection depends on bridge snooping (`multicast.c`). Without it, nodes ask for all multicast and the mesh floods (§9) |
| `CONFIG_MAC80211_MESH=y` | 802.11s mesh point |
| nftables with the bridge family | bat0 filters (§7.7) |
| Headers or a build tree for the running kernel | DKMS needs them. Vendor kernels without headers need a Trellis-built kernel |

**At runtime,** `meshd` reads the loaded module's version
(`/sys/module/batman_adv/version`):
- it refuses to start below 2024.0;
- it shows a mesh-wide warning when any node differs from the pinned
  release.

**Why one pinned release, not "anything ≥ 2024.0":** the multicast packet
type only works when every node supports it (§9). Identical code everywhere
also means identical behaviour, so test results transfer to the field.

**Why DKMS rather than requiring a new kernel:** self-install is a
requirement (R9), and real fleets run whatever kernel the board vendor
ships. Upgrading the kernel is often the hardest part
on such boards.

## 7. Client network

### 7.1 Bridge and ports

`br-mesh` contains `bat0`, the client AP and the wired EUD ports. `anycast0`
sits on top of the bridge (§7.4). IGMP snooping is on (§9).

**Ethernet port role:** `auto`, `wan` or `client`. In `auto`, the port tries
DHCP. A lease from outside the mesh prefix makes it a WAN; otherwise it joins
the bridge as a client port.

**In practice:**
- **Which ports:** those listed with role `auto`, plus every physical,
  wired Ethernet port that node.yaml does not mention.
- **System-in-use ports are left alone**, and shown in status with the
  reason. A port is in use if it is in another bridge or bond, has an
  address, or carries the system's default route.
- **Ports a system network manager runs.** An unlisted
  port that NetworkManager or systemd-networkd runs is the norm on stock
  images (NetworkManager makes a "Wired connection N" DHCP profile for every
  port). meshd shares it with the manager instead of ignoring it:
  - **Idle (no address) with a cable in:** meshd probes as above. Silence
    means a device, not a network: meshd tells the manager to let go *at
    runtime only* (`nmcli device set <port> managed no`, or a
    `/run/systemd/network/00-trellis-claimed.network` with `Unmanaged=yes`
    and `networkctl reload`) and makes the port a client port. An offer from
    outside the mesh means a network: the port stays the manager's, which
    takes the lease itself, so the node keeps its own uplink, DNS and
    management address. That decision holds until the cable changes.
  - **Holding a DHCP lease, and the cable changed** (the carrier change
    counter moved): NetworkManager keeps a lease across a short unplug, so a
    laptop swapped in for the router fast would otherwise find nothing.
    meshd probes again, for 10 s rather than 4 since this is usually the
    uplink, and takes the port only if nothing answers.
  - **Never taken:** a port whose manager profile is not plain DHCP
    (NetworkManager `ipv4.method` manual, shared, link-local or disabled; a
    networkd `.network` with an `Address=` or without DHCPv4). A static or
    shared port is meant for something else. Ports configured by
    `/etc/network/interfaces` or a running dhcpcd are also left alone, since
    meshd cannot release them at runtime; list them in node.yaml instead.
  - **Given back** (`managed yes`; the claim file rewritten, `networkctl
    reload` and `reconfigure`) when a DHCP server answers on the port, when
    the interface goes away, and when meshd stops. A cable pulled from a
    taken port keeps it taken, so a manager bouncing the link cannot make
    the two fight. The claims live in `<run-dir>/claimed-ports`, so a meshd
    restarted after a crash gives them back properly; the managers' runtime
    state, like the file, is gone at reboot.
  - If the manager runs the port again (it restarted), meshd tells it again
    after 15 s.
  - Status shows a taken port's role with "taken over from
    NetworkManager".
  - Labs run meshd with `--no-host-config`, which turns this off (the host's
    managers are not the lab's). `--test-port-manager DIR` replaces them
    with a stand-in driven by files, for the lab tests.
- **Client ports are checked for a DHCP server** every 20 s (3 DISCOVERs
  over 5 s, through a tap socket since the port is in the bridge). One that
  answers means a router was plugged in where a device was, or booted after
  the node. The port leaves the bridge and is decided again: a WAN, or back
  to the manager it was taken from. This also undoes the hazard of taking a
  LAN port while its router is down (after a power cut, for example): the
  port comes back within about 25 s of the router. Meanwhile the LAN's
  devices may lease from the mesh, and they move back at their next rebind.
  An offer from inside the mesh prefix means the cable leads to another
  node's client network, and the port becomes `peer`.
- **Listing a port in node.yaml hands it over** for good. meshd then adds it
  to its NetworkManager and networkd "unmanaged" files (as it does for
  listed `client` and `mesh` ports).
- **Probing:** when a cable is in, meshd sends DISCOVERs (at 0, 1 and
  2.5 s), carrying vendor class `trellis-meshd`, and waits 4 s:
  - an offer from outside the mesh prefix makes the port a **WAN**;
  - silence makes it a **client** port in `br-mesh`;
  - an offer from *inside* the prefix, or another node's `trellis-meshd`
    DISCOVER, means the cable leads to another Trellis node. The port
    becomes **peer** and is not bridged, because bridging two nodes'
    client networks would loop the mesh. Use role `mesh` for a wired link.
- **Re-probing:** losing the link, or a new interface under the same name,
  sends the port back to probing.
- **The auto WAN's lease is meshd's** (`internal/dhcpc`), over a link-layer
  socket, so UDP port 68 is never needed. It renews at T1, rebinds at T2,
  and starts over on expiry or NAK.
- **Auto WAN routing:**
  - The address goes on with `NOPREFIXROUTE`. Its subnet and default route
    go into table `7400+N`, which serves this node's mesh clients (iif and
    pin rules).
  - An `oif <port>` rule (7302) routes sockets bound to the port: the health
    prober, and dnsmasq's upstream `server=<dns>@<port>` from the lease.
  - A last-resort rule after main (32800) makes the reverse-path check
    (including ARP's) pass on a node with no uplink of its own. It also lets
    that node's own traffic out. The main table is never written, and wins
    whenever it has a route.
- **A node with no uplink at all** still reaches the internet itself
  (package updates, anything it fetches): the same last-resort rule points
  at the table of the gateway its clients use, and while the main table has
  no default route meshd points the system's resolver at the node's own
  mesh DNS (dnsmasq on the anycast address), through systemd-resolved (a
  per-link server on `br-mesh`) or NetworkManager (a global DNS setting),
  and puts it back when the node has an uplink again or meshd stops. It
  does so only once dnsmasq forwards to the gateway, never to the system's
  resolver, which would make a loop.
- **Choice of WAN:** `gateway.wan`, then a `wan` port, then an auto WAN, then
  the system's default route.

**Why:** plugging a node into a router or into a laptop should both just work.

### 7.2 Addressing plan

Default mesh prefix `10.73.0.0/16`. It is configurable per mesh profile and
checked against every WAN lease for overlap.

| Range | Use |
|---|---|
| `10.73.0.1` | Anycast gateway and DNS, answered locally by every node |
| `10.73.0.0/24` (rest) | Reserved for future anycast services |
| `10.73.N.1` | Node N's own address (N = 1–254) |
| `10.73.N.2–99` | Static devices attached to node N (cameras, laptops) |
| `10.73.N.100–199` | DHCP pool served by node N |
| `10.73.255.0/24` | Manual and provisioning use; never auto-assigned |

Clients get a `/16` netmask, so everything on the mesh is on-link.

**Why:** ID-derived, non-overlapping pools mean nodes never coordinate leases
(per-node DHCP windows derived from the node ID). 254 IDs leave room well
beyond the 50-node target.

### 7.3 Node identity and ID claims

Each node has a persistent identity: a random locally administered `bat0` MAC,
a hostname or callsign, and a node ID N.

- An unconfigured node claims a free ID from the gossip view (§12.5) and
  probes the address with ARP before using it.
- **Conflicts:** the lowest MAC keeps the ID and the other node re-addresses
  live, through the reconcile loop, with no reboot.
- Operators can pin an ID in config.

**In practice:**
- **The first ID** is the pinned one, else a hash of the identity MAC
  (`derived`). There is no ARP probe before first use: a duplicate lasts until
  the first gossip exchange, which comes within seconds because a new node is
  answered early (§12.5).
- **Who keeps a contested ID:** a pinned ID beats a `derived` or `claimed`
  one. Between equals, the lower identity MAC keeps it. Every node evaluates
  the same rule over the same gossip, so exactly one moves.
- **The loser** takes the first free ID in a sequence hashed from its own MAC
  (so two simultaneous losers rarely collide again). It persists that ID as
  `claimed`, so it survives restarts.
- **What re-addressing changes, live:**
  - the `br-mesh` address (the old one is removed);
  - the DHCP pool (dnsmasq restarts);
  - the gateway marks;
  - the gossip record.
- **The loser's clients:** hostapd restarts and wired client ports drop for
  2 s. Clients then ask for their old lease, the local dnsmasq NAKs it, and
  they lease from the new pool.
- **A pinned loser** keeps the pin in `node.yaml`, so it shows a
  `node-id-taken` error until an operator changes one of the two pins. It
  retries the pin on restart.
- **Cloned identity files** (same identity MAC, different meshd): the node
  whose lowest mesh hard-interface MAC is higher generates a new identity MAC.
  That MAC applies live to `bat0` and `br-mesh`.
- **Reloads:** `meshctl reload` applies a changed `node_id` live, in the same
  way.

**Why:** cloned SD cards and spare nodes are normal in the field, and a
duplicate address looks exactly like an RF fault. The lowest MAC wins, and
the loser re-addresses live rather than needing a reboot.

### 7.4 Anycast gateway

Every node carries `anycast0`: a macvlan on `br-mesh` holding `10.73.0.1` and
one fixed, locally administered MAC that is the same mesh-wide. Bridge-family
nftables rules on the `bat0` port stop it ever leaving the node (§7.7).

- **EUDs always use `10.73.0.1`** as router and DNS, on every node.
- **ICMP redirects are off** (`send_redirects=0`). Otherwise the node would
  tell EUDs to bypass it and go straight to the gateway.
- **ARP flux is suppressed** (`arp_ignore=1`, `arp_announce=2`), because the
  node has two addresses on one layer-2 segment. The node's own traffic
  sources from `10.73.N.1`.
- **Implementation notes:** the MAC is `02:74:72:65:6c:01` on every
  node; `anycast0` holds `10.73.0.1/32` (a `/16` there would compete with
  `br-mesh` for the mesh route); a permanent FDB entry on `br-mesh` makes the
  bridge hand frames for that MAC to `anycast0` instead of flooding them to
  every port; IPv6 is disabled on `anycast0`; and `meshd` refuses to create
  it while the §7.7 filters are not in place.

**Why:** if an EUD's gateway were a specific remote node, failover would wait
for the DHCP lease to renew, which takes minutes. With a gateway on every node,
failover is just the node changing its own route, so EUDs never notice.
Because the MAC is identical on every node, a phone moved to another node, or a
node swapped for a spare, needs no change on the phone. This is the
distributed anycast gateway pattern from EVPN data-centre fabrics, and the
isolation trick Freifunk's Gluon uses for its "next-node" address on large
batman-adv meshes.

### 7.5 DHCP

`dnsmasq` on each node serves only its own clients, from its own pool.

- **Options:** router and DNS = `10.73.0.1`; netmask `/16`; long leases
  (body-worn EUDs rarely move).
- **Containment:** broadcast DHCP never crosses `bat0` (§7.7). Unicast
  renewals to another node's server are allowed, so an EUD that moves keeps its
  lease.
- **When the original node is gone:** the EUD's rebind reaches the local node,
  which NAKs it, and the EUD takes a local lease.
- **Known interaction:** in `gw_mode client`, batman can drop a unicast DHCP
  renewal addressed to a node that is itself a gateway, if that gateway is
  judged worse than the selected one (`batadv_gw_out_of_range` in
  `gateway_client.c`). The EUD then re-leases locally. This is harmless, but
  it is covered by a lab test.

**Why:** no shared lease state, no single DHCP server to lose, and an address
changes only when its home node disappears.

### 7.6 DNS and naming

`dnsmasq` on the anycast IP:

- **Local names:** authoritative for `.mesh`, populated from gossip. That
  covers node names, DHCP hostnames and published services such as
  `tak.mesh`.
- **External names:** forwarded to the current gateway's mesh address, which
  forwards to its WAN resolvers. The forwarder follows failover.
- **No gateway:** external names fail fast (no long timeouts); `.mesh` keeps
  working.

mDNS also works on the flat network, but is rate-limited on `bat0` (§9).

**Why:** a TAK Server must be reachable by name with no internet (R2). Names
don't depend on mDNS support in each EUD app.

### 7.7 Filters on the `bat0` bridge port

All rules use the nftables bridge family, rendered and applied atomically.

| Rule | Direction | Why |
|---|---|---|
| Drop ARP for `10.73.0.1` and frames to or from the anycast MAC | both | Keeps the anycast gateway local (§7.4). |
| Drop broadcast DHCP (UDP 67/68) | both | Each node serves only its own clients. |
| Drop IPv6 router advertisements and DHCPv6 | both | IPv6 is out of scope; stops rogue IPv6 config spreading. |
| Drop known chatter: SSDP and WS-Discovery (`239.255.255.250`), LLMNR (`224.0.0.252`), NetBIOS broadcast | out | Phones and laptops send these constantly; they cost airtime and serve no mission. |
| Rate-limit mDNS (`224.0.0.251`) and other link-local multicast | out | Link-local multicast always floods on bridged nodes (§9). |
| Drop IPv4 broadcast except ARP and allow-listed ports | out | Broadcast is the most expensive frame on a radio mesh. |

On **client ports**, the node also drops DHCP-server replies and router
advertisements arriving *from* EUDs.

**Why:** one phone with its hotspot on, or one laptop sharing its connection,
would otherwise hand out bad leases mesh-wide.

## 8. Internet gateways and failover

### 8.1 Roles

Gateway is a dynamic role, not a node type. A node is a gateway while it has a
healthy WAN:

- it sets batman `gw_mode server` with its measured uplink bandwidth;
- it NATs `10.73.0.0/16` out of the WAN;
- it routes its own clients directly out of the WAN.

When its WAN is unhealthy, it withdraws (`gw_mode client`) and behaves like any
other node.

### 8.2 WAN health

Each gateway probes its WAN every second. Probes are ICMP to several targets
plus a DNS or HTTP check, so one dead target isn't a dead WAN. It withdraws
after 3 consecutive failures and re-announces after sustained success.

**Why:** a peer that is alive and still announcing itself as a gateway, but
not passing traffic, must be caught (lab scenario S4). Probing runs out of the WAN, so it costs no mesh
airtime.

### 8.3 Detection signals

| Failure | Signal | Extra airtime | Target detection |
|---|---|---|---|
| Gateway's WAN dies | Gateway withdraws; its next routing message has no gateway announcement, and batman removes it from every node's gateway list | none | ≤ 4 s (3 s probing + ≤ 1 s propagation) |
| Gateway node dies, or the mesh splits | Originator "last seen" goes stale in batman's originator table, confirmed by one unicast ping | none until suspected | ≤ 5 s |
| Path to gateway degrades | BATMAN_V throughput to that originator drops | none | continuous |

Both behaviours are confirmed in batman-adv source: `gateway_common.c` treats a
missing gateway announcement as bandwidth 0 and removes the gateway, and
`bat_v.c` exports `BATADV_ATTR_LAST_SEEN_MSECS`.

**Why not rely on the gateway list alone:** a dead node's entry lingers until
batman purges its originator, which takes 200 s by default
(`BATADV_PURGE_TIMEOUT`).

**Why no gateway beacons:** at 50 nodes on HaLow, every flooded beacon is about
150 slow transmissions (§9). batman already floods routing messages every
second; we read them instead of adding our own.

### 8.4 Selection

1. **Candidates:** gateways in batman's gateway list whose mesh address is
   known from gossip, that are fresh by last-seen, and that aren't marked
   unhealthy.
2. **Ranking:** batman's own BATMAN_V choice first (path throughput). Ties are
   broken by announced uplink capacity and the WAN quality the gateway reports
   through gossip.
3. **Hysteresis:** switch away from a working gateway only on a large,
   sustained improvement. `gw_sel_class` is tuned for HaLow data rates.
4. **Install:** `default via 10.73.G.1 dev br-mesh src 10.73.N.1`. There is no
   default route when there are no candidates (lab scenario S6: never
   invent one).

**Optional, later:** spread *new* connections across all healthy gateways.

### 8.4.1 Implementation

- **WAN:** `gateway.wan`, else the first port with role `wan`, else the
  interface of the node's own default route (self-install nodes keep their
  OS-managed uplink). Probed every second with ICMP to several targets out of
  that interface; unhealthy after 3 failed rounds, healthy after 3 good ones.
- **Announce:** `gw_mode server` with `gateway.down_mbit/up_mbit` while
  healthy, `gw_mode client` (`gw_sel_class` 2 Mbit/s) otherwise.
- **Selection:** a monitor reads batman-adv's gateway and originator tables
  every 500 ms. A gateway is usable when gossip maps it to an address and
  reports its WAN healthy, and batman-adv heard it within 3 originator
  intervals; the gateway in use gets one unicast ping before being dropped.
- **Unicast reachability:** batman-adv hears a gateway through its
  broadcast routing messages, and those can keep flowing while unicast to
  it is lost: for example a radio link that still acknowledges frames but
  can no longer decrypt them after one end re-peered. So the gateway in
  use is pinged over `br-mesh` every 2 s (1 s timeout). After 3 misses in a
  row it is not usable ("heard by batman-adv, but no answer to unicast
  pings"), the selection moves at once, and it is retried every 15 s.
  Detection takes about 7 s. The cost is one echo pair every 2 s per node
  that uses another node's gateway. Nodes must keep answering ICMP echo on
  `br-mesh` behind any firewall.
  A node with a healthy WAN always uses its own. Otherwise the gateway in use
  is kept unless another is 1.5 times faster and at least 2 Mbit/s faster
  for 10 s.
- **Routing:** `meshd` owns IPv4 rules 7200–7399 and tables 7399–7654.
  - `iif anycast0` (this node's clients) goes to the selected gateway's
    table (`default via 10.73.G.1`), to `main` when this node is the
    gateway, and to table 7399 (`unreachable default`) when there is none.
  - `iif br-mesh` (other nodes' clients arriving at a gateway) goes to
    `main` on a gateway and to 7399 elsewhere, so nodes never relay at IP
    level.
  - `iif br-mesh to <mesh prefix>` goes to `main` (rule 7301, before the
    `iif br-mesh` rule, 7303). Without it, on Linux 6.1 a node that is not
    a gateway drops its clients' internet traffic: the reverse-path check of
    a packet forwarded from `anycast0` to `br-mesh` looks up the source with
    `iif` set to the outgoing interface, `br-mesh`, reaches the unreachable
    table and counts the packet as spoofed (`IPReversePathFilter`). Newer
    kernels don't do this; a lab test checks that the rule exists.
  - New client connections get conntrack mark `0x7300|G`, and a fwmark rule
    per usable gateway keeps them on it. Only new connections follow a
    switch.
- **NAT:** `inet trellis` table, masquerade of the mesh prefix out of the WAN,
  only while this node is a gateway. `nf_conntrack_tcp_be_liberal=1` for the
  one-way TCP that non-gateway nodes see.
- **DNS:** dnsmasq forwards outside names to the selected gateway's address
  (or, on a gateway, the system resolvers), through a servers file re-read on
  SIGHUP. With no gateway it answers REFUSED at once.

### 8.5 Forwarding path and NAT

- **Connection pinning:** each forwarded connection is marked (conntrack mark)
  and routed through a per-gateway routing table, so it stays on the gateway it
  started on. Only new connections follow a gateway switch.
- **Asymmetric return path:** outbound goes EUD → node → gateway; replies go
  gateway → EUD directly over layer 2. The middle node never sees replies, so
  forwarding on that path must not accept or drop by connection state.
  Conntrack is used only for marks, with liberal TCP tracking.
- **How marks survive the one-sided view:**
  - Conntrack on the middle node sees a SYN and then never a SYN-ACK. It
    leaves the entry in `SYN_SENT` and calls every later packet *invalid*,
    which has no mark.
  - So a node whose selected gateway is another node does not track the
    client's bare SYN (`notrack`). The handshake ACK is picked up instead
    (`nf_conntrack_tcp_loose=1`) and gets the mark.
  - These connections stay "new" in conntrack for their whole life, so the
    mark is set only while it is still unset. Otherwise every packet would be
    re-pinned to the current selection.
  - Remaining gap: a switch landing between a SYN and its ACK breaks that one
    handshake. The client retries.
  - A gateway node tracks SYNs normally, because NAT needs them.
- **NAT happens only on the gateway**, never on the client node.

**Why:** stateful `RELATED,ESTABLISHED` forwarding rules would silently
break this path, and per-node NAT would block reachability and multicast
between nodes.

### 8.6 What failover does not preserve

Connections open through a failed gateway break, because the replacement
gateway has a different public IP. That is accepted for v1. In-mesh traffic,
which includes ATAK and TAK Servers hosted on the mesh, never touches a
gateway and is unaffected.

**Possible later option:** route all gateways through a shared WireGuard exit,
so the public IP never changes and connections survive. It is off by default
because it adds a cloud dependency, which conflicts with disconnected-first.

### 8.7 Alternatives considered

| Option | Rejected because |
|---|---|
| DHCP router option points at a gateway, with batman's DHCP steering | Failover waits for lease renewal: minutes, not seconds. |
| VRRP or keepalived between gateways | One active gateway mesh-wide; no per-node choice of the best path. |
| Babel over `bat0` for the default route | Every node is one hop away on `bat0`, so Babel's metric sees nothing useful, and it doesn't check WAN liveness. |
| Routed per-node subnets | ATAK multicast needs TTL rewriting and per-group rules; EUDs change IP when they move. |
| NAT per node | EUDs can't be reached; no multicast between nodes. |

## 9. Multicast and airtime

**The constraint.** In flood mode, every node rebroadcasts every multicast
frame, and on wireless links batman sends each rebroadcast 3 times. That count
is hardcoded: `BATADV_NUM_BCASTS_WIRELESS 3` in `main.h`. At 50 nodes one ATAK
position report becomes about 150 transmissions at the lowest basic rate with
no acknowledgements. HaLow (roughly 1–15 Mbit/s in practice) can't carry that
for 50 EUDs.

**The fix.** batman-adv 2024.0 added a multicast packet type
(`multicast_forw.c`). It sends one packet carrying the list of destination
nodes as unicast along the routing tree, splitting only where paths diverge.
Each hop is acknowledged and uses fast rates, so a 50-node tree costs about 49
such transmissions instead of 150 slow ones. batman uses it only when
(`batadv_mcast_forw_mode_by_count`):

- multicast optimisations are on (`multicast_mode=1`);
- **every** node supports it;
- the packet plus a 6-byte-per-destination list fits in 1280 bytes;
- every mesh interface has an MTU of at least 1280.

Otherwise it falls back to individual unicasts (up to `multicast_fanout`) and
then to flooding.

**What we do:**

- **Querier.** Run IGMP snooping and a querier arrangement that batman can see
  and that doesn't hide listeners behind a node's own bridge. batman marks
  itself "want all IPv4 multicast" when no querier exists or the querier is
  "shadowing" (`batadv_mcast_mla_flags_get`).
- **Link-local multicast.** It always floods on bridged nodes, so it is
  rate-limited or dropped on `bat0` (§7.7). ATAK's groups (`239.2.3.1` SA,
  `224.10.10.1` chat) are *not* link-local and get the efficient path.
- **No multicast allow-list on `bat0`.** ATAK plugins use extra groups (voice
  on `239.255.255.x`, for example). Only known chatter is dropped, so unknown
  ATAK groups keep working.
- **Small CoT packets.** Recommend ATAK's protobuf mesh format (TAK Protocol
  v1) so SA stays under the 1280-byte limit with 50 destinations.
- **Client AP.** Set `multicast_to_unicast=1` in hostapd so phones reliably
  receive multicast.
- **Verify, don't assume.** batman's `MCAST_TX`/`MCAST_FWD` statistics counters
  are checked in tests to prove the efficient path is actually in use.

**In practice:**
- **Querier:** every `br-mesh` runs the bridge's IGMP and MLD querier from
  its own address (`multicast_querier=1`, `multicast_query_use_ifaddr=1`).
  - Queries cross bat0, and the lowest address wins the election.
  - Every node then sees a querier that is either its own bridge or behind
    bat0. Neither counts as shadowing, so no node asks for all multicast.
  - The bridge timers are the kernel defaults (query every 125 s).
- **bat0 is a permanent multicast-router port** (`multicast_router=2`), so
  all multicast from local clients reaches batman-adv. batman-adv knows the
  listeners mesh-wide; the local bridge does not.
- **Filters:**
  - IGMP is exempt from the link-local rate limit on bat0.
  - IGMP and MLD queries from client ports are dropped
    (`rogue-multicast-querier`). A querier on an EUD would shadow the mesh's
    querier, and its node would then want all multicast.
- **Startup window:** a node asks for all IPv4 multicast for its first
  10–50 s. Its own querier counts after 10 s. When it first hears the
  elected querier, the bridge waits one more response interval (10 s) before
  trusting it.
- **Warning:** `multicast-want-all` appears if this lasts more than 45 s.
- **Status:** `meshctl status` names the path (efficient, partial or
  flooding), the querier state and batman-adv's multicast packet-type
  counters (ethtool statistics on bat0).
- **Forwarding order** (batman-adv 2026.3 `batadv_mcast_forw_mode_by_count`,
  confirmed in the lab):
  1. The multicast packet type, whenever every node supports it and the
     packet plus its destination list fits in 1280 bytes.
  2. Otherwise, acknowledged unicasts to up to `multicast_fanout` listener
     nodes (default 16; profile `batman.multicast_fanout`, always applied so
     removing it restores 16).
  3. Otherwise, flooding.
- Recommended phone settings: [atak.md](atak.md).

**Why not flood mode:** some batman-adv deployments run with
`multicast_mode=0` because voice has broken with optimisations on. We take
that as a known risk to test, not a reason to give up the largest airtime
saving available.

## 10. EUD access (body-worn)

- **AP per node:** a unique SSID and passphrase, shown as a QR code in the web
  UI (Android joins Wi-Fi from a QR code).
- **5 GHz,** always a different band from the mesh when the mesh is on
  standard Wi-Fi. Transmit power is not limited by Trellis (§5.1).
- **Wired EUD link** (Ethernet or USB) as a first-class option. It emits no RF
  for the access link.
- **Optional, off by default:** a shared team SSID for fallback.

**Why:** the phone should stay on its owner's node. Unique credentials mean a
teammate's node is never joined by accident, and that teammate will walk away.
With the anycast gateway, the phone's network settings are the
same on any node anyway.

## 11. Security model

- **Mesh membership** = knowing the mesh SAE passphrase (from the mesh
  profile). Links are encrypted per hop.
- **Control traffic** between nodes (gossip, §12.5) is authenticated with a key
  derived from the mesh secret. alfred has no authentication of its own.
- **Trust boundary:** every member node is trusted. A captured node exposes the
  mesh secret, so **key rotation** (re-key every node from a new profile) is a
  roadmap item. Per-node keys and revocation come later.
- **Privilege separation:** only `meshd` has network privileges. The web UI and
  bridges run unprivileged (§12.2).
- **Web UI:** HTTPS (per-node self-signed certificate at first), admin
  credential set at provisioning, reachable only from the mesh and client
  ports.
  - **In practice:**
    - `webd` serves a connection only when both its ends are inside the
      mesh prefix (or both on the loopback). Checking the far end matters
      while there is no firewall: a host on the uplink network can
      address a node's mesh address, but cannot have one itself.
      `web.wan_access` in the node config lifts the restriction.
    - `meshd` keeps the admin password, as a salted PBKDF2-SHA256 hash
      readable by root only. Five wrong passwords in a row start a lockout
      that doubles up to a minute.
    - The first password comes from whoever sets the node up (the setup
      wizard, §12.6). A node that is already part of a mesh takes its
      first password only on the node itself (`meshctl web password`), so
      a stranger on the mesh can't claim its UI.
    - Sessions are a `SameSite=Strict`, `HttpOnly`, `Secure` cookie, and a
      request that changes anything must also carry a header that another
      site can't set. A new password ends every session.
    - The certificate is kept for ten years and never replaced on its own,
      so that the exception a browser remembers stays valid. No HSTS: with
      a self-signed certificate it would lock browsers out.
- **Host:** IPv6 forwarding off; default-deny input from WAN; SSH configurable.

**Why:** this closes the biggest gap of an open mesh (open IBSS, "assume
everyone can see everything") without needing a PKI on day one.

## 12. Software architecture

### 12.1 Language: Go

| Reason | Detail |
|---|---|
| No text parsing | Netlink directly: `mdlayher/genetlink` for batman-adv's own netlink family (gateways with throughput, originators, translation tables, multicast flags), `vishvananda/netlink` for links, routes, rules and bridges, and nl80211 for radios. Removes the `batctl` output-parsing class of bug. |
| Fits the workload | Concurrent long-running loops (probes, netlink events, API, serial) map naturally onto goroutines. |
| Memory-safe | This is a root daemon parsing data from other nodes and serving a web UI. |
| Delivery | One static binary; one-command cross-compile for arm64, armv7, x86-64 and mips; web UI built into the binary. |
| Proven on this hardware | Go cross-compiles cleanly for, and runs well on, these ARM boards. |

### 12.2 Processes

One binary, several subcommands, each run as its own process.

| Process | Privilege | Role |
|---|---|---|
| `meshd` | `CAP_NET_ADMIN`, `CAP_NET_RAW` | Sole owner of network state: radios, batman, bridge, addressing, gateways, firewall, dnsmasq and hostapd config, gossip. Local API on a unix socket. |
| `webd` | none | Web UI and HTTP/JSON API with live updates; talks to `meshd` over its socket. |
| `crda` | root, run by udev | Answers the kernel's regulatory requests with the mesh profile's rules (§5.1). Runs for a moment and exits. |
| `meshctl` | operator | CLI, including `meshctl doctor`. |

**Why:** a bug in the web UI is not a root bug. One binary keeps packaging
simple.

**`webd`.** It has no privilege and no state to protect:

- **No capabilities, no network of its own.** systemd opens ports 443 and
  80 (`trellis-webd.socket`) and passes them in. `webd` runs as a user that
  exists only while it runs, in an empty network namespace, allowed to
  create unix sockets only. It can't see the node's interfaces, let alone
  change them. A lab test proves it.
- **Everything goes through `meshd`'s API** ([api.md](api.md)): `webd`
  checks the login and passes the request to the socket unchanged. `meshctl`
  uses the same API on the same socket, so the UI can do nothing the CLI
  can't, and the other way round.
- **Its only file** is the TLS certificate.
- **Live updates** are server-sent events: `webd` asks `meshd` for the
  status every 2 s while at least one page is open, and hands every page
  the same bytes, compressed.
- **The UI** is plain HTML, CSS and JavaScript
  modules built into the binary: no framework, no build step, nothing
  fetched from outside the node. It redraws a view from the status and
  changes only the parts of the page that differ.

### 12.3 Reconcile loop

`meshd` holds desired state (config plus mesh profile plus learned facts) and
repeatedly converges actual state to it. Every step is idempotent. Netlink
events trigger an immediate pass, and a slow timer catches anything missed.

**Why:** restart-safety and correct teardown come built in. That removes whole
classes of bugs: a dead node after unclean teardown, a stale AP and DHCP after
stop, and boot-order races (a helper starting before its interface exists,
`sleep`-based ordering).

### 12.4 Interfaces to the kernel and helpers

| Component | How `meshd` drives it |
|---|---|
| batman-adv | generic netlink (`batadv` family) |
| Links, addresses, routes, rules, bridge, macvlan | rtnetlink |
| Mesh radio / SAE | `wpa_supplicant` control socket (upstream or `_s1g`) |
| Client AP | generated `hostapd` config plus control socket |
| DHCP and DNS | generated `dnsmasq` config, reloaded on change |
| Firewall and bridge filters | one rendered nftables ruleset applied atomically with `nft -f` |
| Radio inventory (§5.2) | nl80211 for radios, capabilities, interface combinations and per-channel power; rtnetlink `IFLA_PERM_ADDRESS`; sysfs for bus path, USB speed and LEDs |
| Helpers' lifecycle | supervised child processes, restarted by `meshd` |

**Why:** distro network managers (NetworkManager, netifd/UCI, ifupdown) are
told to leave these interfaces alone. That is what makes the same binary
portable between the Debian install and the images.

### 12.5 Node-to-node control data (gossip)

Slow-changing facts:

- node ID claims, hostnames and services;
- each gateway's mesh address, uplink capacity and WAN quality;
- node position and versions.

Each node publishes on change plus a slow refresh (tens of seconds).
Authenticated (§11), versioned, size-capped.

**Transport: custom, built into `meshd`.**

- One JSON record per node: identity MAC, the MACs of its mesh interfaces
  (so batman-adv originators map to nodes), node ID, callsign, address,
  version, gateway state (WAN health, announced capacity, RTT, loss), and
  the bands of each radio that can carry a mesh link (`mr`, one entry per
  radio: `"2g+5g"`, `"s1g"`; about ten bytes a radio). The last is what lets
  a node say, before a profile change, which nodes have no radio for the
  new channel; a node on a version that does not send it is reported as
  unknown.
- Sent to UDP multicast group `239.73.73.73:7373` on `br-mesh`, TTL 1: on
  change, every 30 s ± 5 s, and within a second of hearing a new node (at
  most once per 3 s), so a newcomer learns the mesh in seconds.
- HMAC-SHA256 (truncated to 128 bits) under a key derived with HKDF from the
  mesh passphrase and mesh ID. Replays are refused per sender by a random
  boot ID plus a sequence number. Client ports drop the gossip port in both
  directions (§7.7), so EUDs neither see nor inject it.
- **Airtime:** at 50 nodes about 50 records of roughly 300 bytes per 30 s.
  With batman-adv's multicast packet type that is on the order of 1% of
  Wi-Fi airtime. *Measured* in a 50-node lab: 49 records per 30 s,
  195 bytes each on average (largest 234), 320 B/s mesh-wide before
  batman-adv carries it.
- **Failover does not depend on it:** gossip supplies addresses and WAN
  health, while dead gateways are detected from batman-adv's own tables
  (§8.3).

**Link queries.** Records carry no link state: at 50 nodes it would
multiply their airtime, for a picture nobody may be looking at. A node that
needs the whole mesh graph (its topology page is open) asks instead:

- one small unicast query to each node, on the gossip port, answered with
  that node's direct links (throughput and signal per neighbour, at most
  12), its number of end-user devices and the gateway it uses;
- both packets authenticated like records, under the same key, with a
  nonce so an old answer can't be replayed;
- at most one round every 5 s, growing by half a second per node (25 s at
  50 nodes), and none at all while nobody asks;
- a node on an older release doesn't answer; its links then show from the
  other end only.

**Why not alfred:** an extra daemon and package on every node, no
authentication of its own, and master-to-master unicast sync that grows with
the square of the node count unless most nodes are demoted to slaves.

### 12.6 Configuration and the mesh profile

- **Mesh profile** (shared by every node in a mesh): mesh name and ID, SAE
  passphrase, regulatory mode and country (§5.1), the band, channel and
  width of each mesh link, mesh prefix, batman version requirement. It is distributed by QR code or file.
- **Node config** (per node): callsign, pinned node ID (optional), port roles,
  radio role overrides and per-radio settings (§5.2), AP settings, enabled
  integrations.
- **Format:** both are declarative files (YAML) with a published schema,
  validated before apply. Invalid config is rejected and the last good state is
  kept.

**Why:** provisioning a node becomes "flash, scan profile QR, name it".
Validation-first keeps a bad config from ever reaching a running node.

**In practice**

- **Setup mode.** A node with no mesh profile can't join a mesh, but it
  must still be reachable, or it could only be provisioned over SSH. So it
  runs everything as usual on a private mesh profile of its own, which no
  other node has. Its Wi-Fi (`trellis-setup-XXXX`, passphrase
  `trellis-setup`) and its Ethernet ports come up, hand out addresses and
  serve the web UI, whose wizard asks for an admin password, the mesh (scan
  or paste a profile, or start a new mesh) and the node's name. A node
  with no node config runs on the defaults under its hostname.
  - The setup passphrase is public by design: a headless node has no way
    to show a generated one. Images may carry a per-device one on a label
    instead.
- **Profile codes.** The profile travels as one line of text (`TRP1:…`),
  short enough for a 57-module QR code and safe to paste into a message.
  The web UI shows the QR code; a new node's wizard scans it with the
  phone's camera where the browser can (Chrome on Android), and takes the
  pasted code or the profile file everywhere.
- **Changes through the API** replace the file `meshd` reads at start and
  reload it: what is on disk and what runs can't drift apart, and a change
  the reload refuses is rolled back. A file written this way lists only
  the settings that differ from the defaults, and keeps no comments.
- **Mesh-wide changes.** A profile changed on one node can go to every
  node (`POST /profile/mesh`). The node gives it the next revision and
  sends it to every node it knows, unicast on the gossip port and
  authenticated under the gossip key, so only a node that holds the mesh
  secret can make one; each node keeps it (on disk, so a restart does not
  lose it), answers, and switches at one moment a minute or so ahead, by
  its own clock (the nodes keep one time, §13). Gossip carries every node's
  revision: a node that missed a change asks one that has it, and switches
  at once. The mesh ID and secret cannot change this way, since nodes that
  switched and nodes that did not could no longer hear each other; that is
  a re-key, with its own procedure (roadmap). The dry run speaks for the
  whole mesh: the links that restart at the switch, the nodes that cannot
  follow, and every node whose regulatory mode the change would alter.
- **Dry runs.** Every change can be tried first. The answer lists what it
  would do to the running node (the mesh link moves, the node leaves the
  mesh, devices must rejoin the Wi-Fi), worked out from the radios present,
  so the UI can warn before a change that drops the mesh link (§5.2) or
  the operator's own connection.

### 12.7 Observability

- **Status:** JSON from `meshd` (served by `webd`): neighbours with
  throughput, gateways and the selected one, EUDs per node, multicast path in
  use, warnings.
- **Diagnostics:** `meshctl doctor` gives a single paste-able report.
- **Logs:** structured, to journald or syslog, with RAM-backed volatile state
  to spare SD cards.

## 13. Integrations

- **TAK Server on a node.** Published as `tak.mesh` through gossip.
- **Channel load and surveys.** Each node samples how busy each mesh
  link's channel is where it is (the driver's survey counters, every 10 s;
  free on the air) and carries it in gossip, so every node shows each
  link's load everywhere. On request, a mesh-wide survey: every node scans
  the band of each of its links once, passively, and answers with each
  channel's busy time and the other networks heard; the asking node judges
  every candidate channel by the worst node (interference is local), never
  suggests a radar channel, considers only 1, 6 and 11 on 2.4 GHz, and
  suggests a move only when the current channel is clearly busy and
  another clearly better. A move is a mesh-wide profile change (§12.6).
  Drivers differ: mt76 reports busy times (the MT7610U's are unusable on
  5 GHz during a scan, and are dropped), ath9k_htc and rt2800usb none, so
  their nodes count networks heard only.
- **Time.** chrony on every node (`packaging/chrony/trellis.conf`). A node
  with a GNSS receiver takes its time from gpsd (the receiver's messages, or
  its pulse per second where the board wires it) and serves it to the mesh.
  Each node says in gossip whether it serves time, and whether from GNSS;
  meshd gives chrony the nodes to follow: those on GNSS, or with none, every
  node that serves time, lowest node IDs first so that all pick the same.
  Internet servers through a gateway stay in chrony's own configuration.
  With no GNSS and no internet the nodes still agree among themselves
  (orphan mode). Nodes without a real-time clock otherwise boot with the
  wrong time, which breaks TLS and log ordering, and planned features need
  agreed switch times (a mesh-wide channel change, a re-key).
- **GNSS.** gpsd owns the receiver; meshd reads its JSON stream for the fix,
  position, accuracy, satellites and PPS, and shows them in the status.
- **Node positions on the ATAK map.** A node with a fix sends its own
  position as a CoT event (XML, which ATAK accepts on the SA group whatever
  it sends itself) to `239.2.3.1:6969` on `br-mesh`, with TTL 1: the phones
  on every node show it, as equipment (`a-f-G-E-S`) rather than a person,
  under a UID from the node's identity MAC. It reports every 60 s while
  still, at once on a move of more than 15 m or its fix's error, and every
  15 s for its first 2 minutes. The record carries the position too,
  rounded to about a metre, riding on the refresh; when a node leaves the
  gossip view, the lowest-numbered node on the mesh re-announces its last
  known position every minute, stale and labelled "(last known)" (ATAK
  does not always draw a stale marker differently), for up to 12 hours. Positions
  stay inside the mesh: the SA group is never routed, and gossip is the
  mesh's own. `positions.share: false` in the profile stops every node at
  once and drops the last known positions (emissions control); in
  `node.yaml` it stops one node.
- **Voice (later).** Multicast push-to-talk on its own group.
- **Site linking (later).** WireGuard + VXLAN added to `bat0` as a batman hard
  interface, merging separate meshes into one.

## 14. Platforms and delivery

- **Self-install:** a `.deb` for Debian-family systems on kernel 5.15 or
  newer that meet the §6.1 requirements, plus the `trellis-batman-adv-dkms`
  package and `wpa_supplicant` with mesh support. An installer that never
  overwrites config.
- **Images:** built per hardware target with batman-adv pinned.
  - **Base OS** (Debian-based or OpenWrt) is open. Morse Micro's reference
    stack is OpenWrt; owning the kernel interface (§12.4) keeps both open.
- **Upgrade policy:** all nodes in a mesh run the same major release. `meshd`
  advertises its version in gossip and warns on mismatch.

**Hardware** (details in [hardware.md](hardware.md)):

| Class | What |
|---|---|
| Compute | Single-board computers and small PCs meeting §6.1 (arm64, armv7, x86-64) |
| Wi-Fi mesh radios | USB and PCIe adapters with `mesh point` in `iw list`; dual-band-concurrent cards carry two links |
| HaLow | Modules built on Morse Micro's MM6108 / MM8108, with Morse Micro's Linux driver |

## 15. Risks and open questions

The main ones:

1. HaLow airtime at 50 nodes, even with the multicast packet type.
2. batman multicast optimisations with Android phones in power save (missed
   IGMP queries) and with voice (a known reason for flood mode).
3. Morse HaLow stack on Debian vs OpenWrt.
