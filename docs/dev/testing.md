# Testing

Three levels: unit tests, the namespace lab, and real nodes.

## Unit (no root)

```sh
make test
```

Pure logic with fakes: config validation, fingerprints and role planning,
netlink attribute decoding against synthetic messages, the reconcile loop,
the process supervisor, the wpa_supplicant control protocol against a fake
supplicant, the API server and client, the hostapd, dnsmasq and nftables
renderers, AP channel choice, and the client-network addressing.

Fuzzing: the parsers of untrusted input have Go fuzz targets. `make test`
runs their seed inputs every time; `make fuzz` runs each one for
`FUZZTIME` (default 30 s) to look for new failures:

| Package | Targets | Input |
|---|---|---|
| `gossip` | `FuzzOpenRecord`, `FuzzOpenMessages`, `FuzzOpenRaw`, `FuzzParseLinkEntry` | records and messages from other nodes, signed with the mesh key so the decoding behind the authentication is reached; what is accepted must be valid and seal again |
| `deb` | `FuzzRead`, `FuzzParseKeys` | release packages (uploaded through the web UI) and key files |
| `config` | `FuzzParseProfile`, `FuzzDecodeProfileCode`, `FuzzParseNode` | profiles, profile codes from QR codes, node configuration as YAML and JSON; what is accepted must validate |
| `regdb`, `wpa`, `dnsmasq` | `FuzzParse`, `FuzzParseEvent`, `FuzzParseLeases` | the regulatory database, wpa_supplicant's events, DHCP leases (client hostnames) |
| `version`, `cot` | `FuzzVersion`, `FuzzMarshal` | versions from gossip and packages (the Debian form must stay valid), callsigns and remarks in the XML phones receive |
| `rollout` | `FuzzDecodeRollout` | rollouts announced by other nodes; what passes `Validate` must be safe to act on |
| `dhcpc` | `FuzzParse`, `FuzzUDPPayload` | DHCP answers and raw frames from an uplink or client port; a lease that is accepted must stay within bounds (mask /8 or longer, 60 s to a year, T1 < T2 < lease) |

For packages and upgrades:

- `internal/deb`: packages written and read back, byte-identical on a
  second build; signatures (a changed byte, an unknown key, a member after
  the signature); and `dpkg-deb` reading the result where it is installed.
- `internal/upgrade`: whole upgrades against a stand-in for dpkg, systemd
  and meshd: a good one, and releases that crash, do not answer, lose
  their mesh peers, break a reconciler, rewrite the configuration or do not
  install, each going back with the configuration as it was; refusals
  (unsigned, unknown key, other architecture, older); a power loss before
  the install, after it and half-way through; the boot-time resume in
  place while an upgrade is pending and gone after.
- `internal/version`, and the mixed-release warning in `internal/meshd`.

For the web UI and API:

- `internal/qr`: the QR encoder, by reading every symbol back (format
  information, unmasking, de-interleaving, Reed–Solomon syndromes), and
  against libqrencode where that library is installed (whole symbols must
  be identical; the test is skipped without it).
- `internal/meshd`: setup mode and provisioning, configuration writes with
  dry runs and their effects, rollback, the admin password rules and
  lockout, radio identify against a made-up sysfs, topology assembly.
- `internal/webd`: nothing readable without a session, the cookie's flags,
  cross-site requests refused, the password rules as seen from the
  network, sessions ending when the password changes, the event stream,
  the mesh-only connection rule, the certificate surviving restarts. One
  test checks that every file the UI refers to is in the binary and that
  nothing is loaded from outside the node: the UI has no build step to
  catch a mistyped path.

## Lab: network namespaces (root, or an unprivileged user namespace)

```sh
make lab                 # 3 nodes
lab/lab.sh -n 10         # more nodes
lab/lab.sh --keep        # leave it running; sockets in lab/run/node*/run/
```

Real kernel, real batman-adv, no radios: every node is a namespace with a
veth into an "air" bridge, running a real `meshd` with the port as a wired
mesh interface. Tests, each reported by name:

- T1 mesh forms, BATMAN_V, bat0 MTU ≥ 1280, multicast optimisations on;
- T2 every node address answers ping across bat0 and br-mesh;
- T3 `kill -9` mid-run, restart, same identity, converges;
- T4 clean stop removes bat0 and br-mesh; a second start is clean;
- T5 an invalid config is rejected naming the fields, running state untouched;
- T6 the hwsim suite below, or SKIP with the reason.

Without root, the script re-executes itself under `unshare -Urmn`. The
`batman_adv` module must already be loaded in that case
(`sudo modprobe batman-adv` once per boot), because a user namespace cannot
load modules.

A skipped test is printed as `SKIP <name>: <reason>` and counted; "0
failures" never hides a skipped suite.

### Restarts (T3, T4)

T3 kills meshd mid-run and restarts it; T4 stops it cleanly (T4a: nothing
of Trellis's is left) and starts it again (T4b). T4c then checks that the
restarted node's broadcasts reach the others at once: a new `bat0`
numbers its broadcasts from 1 again, and batman-adv on the other nodes
drops numbers it saw from the old one, so before meshd's warm-up an ARP
request from a node restarted soon after an earlier start went unanswered
until it had sent as many broadcasts as before (10 s in the lab, minutes on
a quiet node). T4c also checks that the warm-up frames stop at the other
nodes' bridge.

### Client network (T7)

Every lab node also has a wired client port (`eth2`) with an end-user device
namespace behind it (`eud1`…). `lab/eud.py` is that device: a DHCP client
over raw sockets and a DNS resolver, so no client packages are needed. T7
checks the anycast gateway, the lease (pool, router, DNS, server id), EUD to
EUD reachability across the mesh, `.mesh` names, fast failure for outside
names, the NAK of a foreign lease, and, from the nftables counters, that no
anycast or broadcast-DHCP frame reached the other nodes.

In an unprivileged user namespace the netfilter modules cannot be loaded on
demand, so run once on the host before `make lab`:

```sh
sudo modprobe -a batman-adv nf_tables nft_limit nft_ct nft_masq nft_chain_nat
```

### Gateways and failover (G1–G9)

Nodes 1 and 2 also get a WAN link to an `inet` namespace that holds the
probe targets (`198.51.100.1`, `.2`) and a server (`203.0.113.10`); node 3
has no WAN. The tests are gateway scenarios S2–S6:

| Test | Scenario | Pass |
|---|---|---|
| G1 | Roles | nodes 1 and 2 announce, node 3 sees both as usable |
| G2 | S2 | an EUD on node 3 reaches the server; node 3's DNS forwards to its gateway |
| G3 | S3: the selected gateway's WAN link goes down | outage ≤ 5 s |
| G4 | S4: the WAN stays up but its upstream blackholes everything | outage ≤ 5 s |
| G5 | S5: the gateway node drops off the mesh | outage ≤ 5 s |
| G6 | S6: no gateway anywhere | no route invented, `unreachable` within 2 s, outside DNS fails at once |
| G7 | Connection pinning: node 3 uses node 1, then a much better node 2 appears | a TCP connection opened through node 1 keeps leaving through node 1 after node 3 switches; a new one leaves through node 2 |
| G9 | An EUD on node 1 while node 1's own WAN goes down (runs before G7) | node 1 selects node 2, its EUD reaches the server, and rule 7301 (`iif br-mesh to 10.73.0.0/16 lookup main`) is present. The lab's kernel never showed the 6.1 reverse-path drop that rule prevents (A§8.4.1), so its presence is checked directly |
| G8 | The link between node 3 and its gateway loses every unicast frame but carries broadcast (runs before G7, on the direct link) | batman-adv still hears the gateway, yet node 3 drops it for "no answer to unicast pings" and moves to the other gateway: outage ≤ 10 s. G8b: with the link restored, a retry makes it usable again within 25 s |

**Kernel gap:** the lab runs the development machine's kernel, while nodes
often run older vendor kernels (6.1). Policy-routing and reverse-path
behaviour differ between them (G9). A lab pass on routing changes still
needs a check on real nodes.

For G8, an nft bridge table in `air` drops frames between node 3's port and
the gateway's port unless `meta pkttype` is broadcast or multicast. (Don't
name an nft chain `fwd`: it is a keyword, and the table fails to load.)

For G7, the air bridge drops frames between nodes 1 and 3 (an nft bridge
table in the `air` namespace), so node 1 is two hops away through node 2.
Node 2's mesh port then rates its links at 5 Mbit/s
(`batctl hardif eth1 throughput_override`). The internet server
(`eud.py tcpserve`) answers each line with the source it saw, which is the
NAT address `100.64.<gateway>.2`, so the client knows which gateway carried
each connection.

Outage is measured from the client: the EUD pings the server every 100 ms
and the longest gap between replies counts. Every run appends the figures to
`lab/run/timings.txt`.

### Roaming and rogue DHCP (R1–R3)

Every lab node has a spare client port `eth5`. The test cables a device
namespace (`roam`, with a fixed MAC) to it and moves it between nodes, as a
body-worn EUD walks from one node to another.

| Test | Scenario | Pass |
|---|---|---|
| R1 | Leases on node 3 (no WAN), moves to node 1 | keeps its address and gateway; anycast, node 3's EUD and the internet answer; the unicast renewal (`eud.py renew`) is ACKed by node 3 |
| R2 | Leases on gateway node 1, moves to node 3 | either node 1 ACKs the renewal, or (A§7.5, batman-adv `gw_mode client`) the renewal is lost, node 3 NAKs the rebind and the device re-leases locally. The test says which |
| R3 | A rogue dnsmasq on node 2's client port `eth2` | a second device on node 2 (`eth5`), and node 1's EUD, are served only by their own nodes; no rogue frame reaches `eth5`, and node 2's `rogue-dhcp-server` counter rises |

### Multicast (MC0–MC4)

EUD 3 is the only listener (an ATAK phone on SA, `239.2.3.1:6969`), EUD 1
sends, and node 2 (no listener) is watched with tcpdump on its bat0.

| Test | Pass |
|---|---|
| MC0 | Within 45 s every node sees an IGMP querier, unshadowed, and none wants all IPv4 multicast (status `path` efficient) |
| MC1 | Default settings: EUD 3 gets 50/50 SA datagrams, node 2 sees no UDP to the group, and the multicast packet type carried them (node 1's `mcast_tx` and node 3's `mcast_rx_local` grow by the packet count) |
| MC2 | The fallback order. Datagrams too big for the packet type (1300 bytes) go as unicasts: 20/20 to EUD 3, none to node 2, `mcast_tx` unchanged. After a live profile reload to `batman.multicast_fanout: 0` the same datagrams flood, and node 2 sees them. The profile is then restored, and so is the default fanout |
| MC3 | An unknown group (`239.255.100.1`, like plugin voice) reaches its listener; SSDP and LLMNR reach nobody, counted as `chatter` drops at node 1's bat0 |
| MC4 | IGMP queries sent by an EUD are dropped at its port (`rogue-multicast-querier`) and its node keeps the mesh's querier |

The capture counts UDP only: an IGMPv2 report is addressed to the group
itself and rightly crosses the mesh.

### Node ID conflicts (T8)

The lab nodes have fixed identity MACs (`02:00:00:00:00:0N`), so the outcome
is known in advance. T8 re-pins node 2 to node 3's ID with a live
`meshctl reload`. Both IDs are pinned, so node 2 (lower MAC) keeps ID 3 and
node 3 must re-address live.

- T8a checks node 3's claimed ID and its single `br-mesh` address, node 1's
  gossip view, that no node still flags a conflict, and node 3's
  `node-id-taken` warning.
- T8b checks that node 3's client saw its link drop, that a rebind of its old
  lease is NAKed, and that a fresh lease comes from the new pool.

### Web UI and API (W1–W5), firewall (F1)

`lab/web.py launch` runs `webd` the way its systemd units do: it opens
ports 443 and 80 in the node's namespace, then hands them to a `webd` that
has no capabilities and a network namespace of its own. The other
`web.py` commands are a small HTTPS client, run in a device's namespace.

| Test | Pass |
|---|---|
| W1 | The `webd` process has no capability in any set; its network namespace is not the node's and holds only `lo`. A process with the same restrictions, even inside the node's namespace, can't add a link, an address, a route or an nftables table, and can't take `br-mesh` down |
| W2 | From the EUD on node 1, at `https://10.73.0.1`: the UI loads, `http://` redirects, the API answers 401 before signing in and to a wrong password. Signed in, a setting (`gateway.down_mbit`) is tried, then applied: `meshd` wrote it to `node.yaml` and announces it. An invalid change is a 422 naming its field; `meshd`'s password endpoints are not reachable through `webd`; after signing out the session is dead. Together with W1: `webd` changes the node only through `meshd` |
| W3 | `GET /topology` shows 3 nodes, all of which answered the link query sent over the mesh, and 3 links each reported from both ends. The profile exports as a code and a 57-module QR symbol |
| W4 | From the `inet` namespace, node 1's uplink address refuses a TLS handshake on 443; with `meshctl web wan-access on` it serves, and with `off` it refuses again |
| F1 | Node 1 gets `firewall: {wan: closed, allow: [tcp/5201]}` and runs listeners on 4444 and 5201. From its uplink network only 5201 and ICMP answer, and the drop counter rises; from the mesh both answer; the mesh's clients keep their internet through node 1. Opened again, both answer from the uplink |
| W5 | A fourth node starts with no `node.yaml` and no profile. It is in setup mode on a private mesh, takes over the port a device is plugged into (which the stand-in network manager runs with DHCP, as on a stock image), and leases it an address. Through the web API only, the device sets the admin password, tries and applies the lab's profile with a callsign, then gives the node's cabled port the mesh role. The node leaves setup mode and node 1 sees it by name |

Notes:

- `setpriv` (util-linux) does the sandboxing; without it W is skipped.
- Inside the lab's user namespace every process is uid 0, so `meshd` treats
  `webd` as root on its socket. The rule that only root resets the admin
  password without the current one is covered by the unit tests instead.
- What the lab can't show: the systemd units themselves (`DynamicUser`,
  the socket unit), and a browser. Both need real nodes.

### Mesh-wide profile changes (P1–P2)

Each node first gets its own copy of the lab profile (otherwise the first
node to switch would write the shared file for all of them).

| Test | Pass |
|---|---|
| P1 | Node 1 sends revision 1 (`batman.orig_interval_ms` 500 → 600) to switch in 15 s: every other node (node 9 from W5 included, when it is up) has it before the switch, and all switch to revision 1 with 600 within 3 s of each other |
| P2 | Node 3 is stopped; node 2 sends revision 2 (back to 500). Nodes 1 and 2 switch; node 3, started again on revision 1, asks another node for revision 2 and switches to it |

Not in the lab: nodes left on a channel the mesh has left looking for it
(`findmesh.go`). The lab's links are cables, which have no channels to
scan. Unit tests cover who looks (`TestSearchWanted`), reading a mesh's
width from its beacons (`internal/nl80211`), how a visit ends, the roster
across a restart, settling a profile changed apart and its spreading
(`meshconflict_test.go`), switching by an offer's delay whatever the
clocks say, and the restart rules (`TestMeshChangeSurvivesRestart`); the
rest needs radios: mac80211_hwsim in a VM, or the bench.

### Node positions (L1–L5)

A stand-in gpsd (`lab/eud.py gpsd`) in node 3's namespace gives it a 3D
fix; `lab/eud.py cotwait` is a phone waiting for one marker on ATAK's SA
group.

| Test | Pass |
|---|---|
| L1 | A phone on node 1 receives node 3's CoT event: its UID, type `a-f-G-E-S`, callsign, the fix's position, not stale |
| L2 | Node 1's status shows node 3's position, from gossip |
| L3 | Node 3 is stopped. Node 1 (the lowest-numbered node) keeps it as last known and re-announces it to a phone on node 2, labelled "(last known)" and with "last known position" in its remarks |
| L4 | No packet to port 6969 reaches the internet side of either gateway during L1–L5 |
| L5 | `positions.share: false` sent mesh-wide: node 3 stops reporting, node 1 forgets its last known positions, and a phone hears nothing for 40 s |

### Auto port role (T9)

Node 3 has `eth3` with role `auto`, absent at first. The test plugs things
into it:

| Test | What is plugged in | Pass |
|---|---|---|
| T9a | A "laptop" namespace with no DHCP server | `eth3` becomes a client port in `br-mesh`; the laptop leases from node 3's pool |
| T9b | The cable moves to a "router" namespace (dnsmasq on 192.168.50.0/24, routed to `inet`) | `eth3` becomes an auto WAN with the lease on it. Its routes are only in table `7400+N`, none in main. Node 3 becomes a gateway, its EUD's TCP leaves NATed as the lease address, and outside DNS goes to the router through `eth3` |
| T9c | The router's end goes down (cable pulled) | the port is `no-link`, its address and table are gone, and node 3 uses another gateway |

T9b writes `lab/run/t9-diag.txt` on failure: rules, `rp_filter`, the
neighbour table, and captures on the router and the internet side.

T9d covers unlisted ports that a system network manager runs (A§7.1). The
lab's meshd runs with `--test-port-manager $RUN/nodeN/pm`, a stand-in for
NetworkManager and networkd: a file `pm/<port>` holding `dhcp` or `static`
makes the port managed, and `pm/<port>.released` exists while meshd has
taken it over. Node 3 gets `eth4` (managed, DHCP) and `eth6` (managed,
static), both to a `lap2` namespace:

| Test | What happens | Pass |
|---|---|---|
| T9d1 | A laptop (no DHCP server) on both ports | `eth4` is taken over (`eth4.released` exists), is a client port in `br-mesh`, and the laptop leases from node 3's pool. `eth6` stays `in-use` (static profile), not bridged, not released |
| T9d2 | dnsmasq starts on the laptop's side of `eth4`, without the cable moving | within one client-port check (about 25 s) `eth4` leaves the bridge, `eth4.released` is gone, and the port is `in-use` because a DHCP server answered |

### Scale lab (`make lab-scale`)

`lab/lab.sh --scale` starts 50 nodes (`-n` for another count) with
*derived* node IDs. The identity MACs are pseudo-random but fixed (sha256 of
`2-N`), so, like a real fleet, they collide on derived IDs: at 50 nodes six
nodes must move, one of them in a group of four. Only the scale checks run:

- **N2:** IDs converge. All IDs are unique, every node's gossip view agrees
  with every node's own ID, nothing is flagged, and the nodes that moved are
  exactly the predicted losers (all but the lowest MAC of each group).
- **N3:** gossip volume over 60 s of steady state is within its budget:
  about one record per node per 30 s (20% margin), records of at most
  400 bytes. The startup burst is reported too. Each run appends its figures
  to `lab/run/scale-timings.txt`.

`lab/scale.py` takes one status snapshot of every node per check.

dnsmasq runs in debug mode there (`--no-daemon`), because the namespace
cannot drop privileges; on real nodes it drops to `nobody` as usual.

## Lab: virtual radios (root, in a VM only)

```sh
sudo lab/hwsim.sh          # inside a virtual machine
```

**Run these labs only in a virtual machine.** They load and unload
`mac80211_hwsim` and create and delete radios as root. On a developer's
Fedora desktop (kernel 7.2.15) they have crashed the host's networking:
1. Unloading the module under meshd processes the cleanup had failed to
   stop never finished.
2. Deleting a radio that still had interfaces left an orphaned wireless
   device (a `WARNING` in `wiphy_unregister`). Tearing down its namespace
   later hit a NULL pointer in `nl80211_send_iface` while holding the
   network locks.

Both times NetworkManager, systemd-networkd and systemd-resolved hung until
the machine was powered off. The wired NIC also needed a cold power cycle.
The scripts now refuse to start on a real machine (`systemd-detect-virt`);
`TRELLIS_HWSIM_ON_HOST=1` overrides that, at your own risk. They share
`lab/hwsim-lib.sh`, which does the following:
- starts background processes so their PIDs are right;
- stops meshd and waits for it before cleaning up;
- unloads the module only when nothing is left;
- refuses to run over an earlier run's leftovers.

Every lab meshd runs with `--no-host-config`, so it never writes the host's
NetworkManager or systemd-networkd configuration.

Needs `mac80211_hwsim` (on Fedora it is in `kernel-modules-internal`, which
must match the running kernel; on Debian it is in the main kernel package) and `wpa_supplicant`. Moves one virtual radio
into each namespace and lets `meshd` do everything else: role assignment,
`mesh0`, wpa_supplicant with SAE, batman-adv. Checks that a node with the
wrong passphrase never joins, that peers are visible through nl80211, and
that teardown restores the kernel's `wlan` interface.

### Radio roles

```sh
sudo lab/hwsim-roles.sh    # inside a virtual machine
```

One node. `lab/hwsimctl.py` creates and deletes radios on demand through
hwsim's generic netlink family, with fixed permanent MACs, chosen wiphy names
and, optionally, no AP interface type. Not yet passed: its first run was on
the desktop, before the PID fix, and O2's radio removal is what triggered the
kernel crash above.

| Test | Pass |
|---|---|
| O1 | Three identical radios created in three different orders, under new wiphy names, get the same roles each time |
| O2 | Removing the mesh radio moves the mesh to another radio; the AP keeps its own radio |
| O3 | With only the AP's radio left, the mesh takes it and the AP shares it (`mesh+ap`, `ap-shares-mesh-radio`) |
| O4 | An added radio ranked above the mesh radio takes the shared AP role, never the mesh |
| O5 | A lone radio without the AP interface type carries the mesh only (`no-ap-radio`) |
| O6 | An added AP-capable radio fills the empty AP role; the mesh stays |
| O7 | A radio with a random MAC uses another fingerprint source, and roles survive restarts |

A radio whose interface combinations forbid mesh and AP together can't be
made with hwsim; `TestMeshLossTakesAPRadio` in `internal/radio` covers it.

### Dual backhaul

```sh
sudo lab/hwsim-dual.sh     # inside a virtual machine
```

Two nodes, each given two hwsim radios. The profile has `radio` on 2.4 GHz
(`mesh0`) and one more link (`radio2`, the older spelling of a one-entry
`links`) on 5 GHz (`mesh1`).

| Test | Pass |
|---|---|
| D1 | Each node puts one radio on each link, and both links join with SAE |
| D2 | batman-adv has `mesh0` and `mesh1` as active hard interfaces on both nodes |
| D3 | Node 1 hears node 2 as a neighbour on both interfaces |
| D4 | Node 1 reaches node 2's address across the mesh |
| D5 | batman-adv frames (ethertype 0x4305) are dropped, at netdev ingress on both nodes, on the link that carries the route. The 802.11 peering stays up, so only batman-adv's choice can move the traffic. The route moves to the other link and pings resume within 5 s (the gap is reported) |

Planner cases that need specific hardware (a dual-band-concurrent card
plus HaLow, two radios sharing the AP) are Go unit tests in
`internal/radio`.

## Packages in containers

```sh
make deb-test
```

Builds two versions of the `trellis` package (the newer one signed) and the
batman-adv package, and in a throwaway Debian 13 and Ubuntu 22.04 container
(podman or docker) installs them with the distribution's own `apt` over a
made-up script install, upgrades, downgrades, removes and purges, checking
after each step: the configuration, profile and identity are untouched
until the purge, an edited chrony drop-in survives, the script install is
taken over, units are enabled, the regulatory rules are handed over and
given back. The containers have no systemd and no kernel headers for the
host: no service starts and no module is built, and an install must
survive both. It also runs `meshctl prepare`, which must go through every
step and report the ones a container cannot do.

## trellis-deploy in containers

```sh
make deploy-test
```

Two containers (Debian 13 and Ubuntu 22.04) run sshd, one with `sudo`
that takes no password and one with `sudo` that asks for one, and
`trellis-deploy` on the development machine works on them as on nodes
(`port=` in its hosts file reaches each), with a third listed node that
does not answer. Two releases are built and signed with a throwaway key.

| Round | Pass |
|---|---|
| 1 | Release 0.9.0 installed on both with a mesh profile: the profile is 0600, `node.yaml` has the callsign and node ID from the hosts file, the copied files are gone, the unreachable node is skipped and the run goes on, the plan names the node whose `sudo` asks, and neither the mesh secret nor the sudo password is in any log |
| 2 | 0.9.1, signed by a key the nodes do not trust yet: each node refuses it, and the summary gives its reason. With the key in `/etc/trellis/release-keys/`, both upgrade through `meshctl upgrade`, and the summary carries `meshctl doctor`'s findings |
| 3 | Again: nothing to do |
| 4 | A wrong sudo password three times: that node is left out, the other is reinstalled (`--force`) |
| 5 | `make deploy`'s way: the older 0.9.0 over 0.9.1 (`--any-version`), with `apt` (`--no-health-check`), the key sent along (`--node-key`): installed on both, the key in `/etc/trellis/release-keys/deploy-dev.pub` |
| 6 | `--setup`: the node whose `sudo` asked gets a rule (0440, checked by `visudo`) and asks no more; the other is reported ready; a second `--setup` has nothing to do. (Installing the SSH key needs a person at `ssh-copy-id`'s password prompt, so it is not in the test.) |

There is no systemd in a container, so there is no reboot (`--no-reboot`),
no meshd to start after an `apt` install, and `meshctl prepare` reports
the module it cannot build.

## Real nodes

See [docs/hardware.md](../hardware.md) for what a node needs, and
[building.md](building.md) for deploying to a set of test nodes.

### Reading routes in `meshctl status`

- **DIRECT LINK** lists every node this one hears directly: the 802.11s link
  state, signal and radio rate, next to batman-adv's estimate of what the
  link really carries (`BATMAN EST.`, typically a third of the radio rate).
- **ROUTE TO** lists every other node and how traffic gets there: `direct`,
  or `relayed via <mac>` (the first hop). `(beats the direct link)` means a
  radio link to that node exists but batman-adv judges the relay faster.
  batman-adv only knows the first hop; `sudo batctl tr <mac>` shows them all.
- Nodes are shown by their mesh radio MAC until their names arrive with
  gossip.

### Forcing a relay on a test mesh that is too small

Make two nodes drop each other's batman-adv traffic while their radios stay
linked. On each of the two nodes, with the *other* node's mesh radio MAC:

```sh
sudo nft add table netdev trellis-test
sudo nft add chain netdev trellis-test in '{ type filter hook ingress device "mesh0" priority 0; }'
sudo nft add rule netdev trellis-test in ether saddr <other node's mesh MAC> drop
```

Within seconds the route between them shows `relayed via …`, `ping` keeps
working, and `iperf3` drops to roughly half (the relay sends everything twice
on one channel). Undo with `sudo nft delete table netdev trellis-test` on
both; a reboot also clears it, and so does meshd recreating `mesh0`.

## Known issues found in the lab

- **Bridge loop avoidance holds broadcasts back after a start.** With BLA
  on, batman-adv drops every broadcast and multicast frame (`tx_dropped`
  rises, the claim table stays empty) for its start-up wait, 3 periods of
  10 s, each time `bat0` is created; then they flow. Earlier this looked
  like a kernel fault, because the lab's tests run within that wait.
  Measured on the 7.3 kernel with `LAB_BLA=auto` (BLA on: every lab node
  bridges wired client ports): drops until about 30 s, then claims appear
  and the warm-up completes (30 frames held back); the same as on a bench
  node with DKMS 2026.3. Five tests that act straight after a start fail
  that way (T2, T4c, T7d, MC2, W5), so the lab keeps BLA off by default.
  Real nodes have it on only where they bridge wired client ports.
- **Broadcasts from a freshly restarted node.** Two things hold a
  restarted node's broadcasts back. batman-adv restarts its broadcast
  sequence numbers with `bat0`, and the others discard numbers inside
  their duplicate window: meshd's warm-up uses those numbers up (lab T4c).
  And bridge loop avoidance, where it is on, drops every broadcast and
  multicast frame both ways for its start-up wait (3 periods of 10 s,
  `BATADV_BLA_WAIT_PERIODS`); measured on a bench node: ARP to a neighbour
  answered 33 s after a restart, the moment `bat0`'s `tx_dropped` stopped
  rising. So BLA is on only where the node bridges wired client ports.
  The warm-up counts only frames batman-adv numbered (a dropped one raises
  `tx_dropped`). The lab runs with BLA off and cannot show the wait.
- **Bridge loop avoidance in the lab.** `LAB_BLA=auto make lab` runs the
  lab with the profile's default (on, as every lab node bridges wired
  client ports), `LAB_BLA=on` forces it; the default is off.
- **Debugging the lab.** `LAB_INSPECT=script make lab` runs `script` inside
  the lab's namespaces right after the mesh forms (with `$RUN` and `$BIN`
  set), then stops. tcpdump needs `-Z root` there: Fedora's tcpdump drops to
  its own user by default, which fails silently in the user namespace.
