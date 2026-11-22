# ATAK on a Trellis mesh — recommended settings

ATAK works on Trellis without a TAK Server: situational awareness (SA) and
chat travel as multicast, and Trellis carries multicast only to the nodes
with subscribed phones (architecture §9). Phone settings and rationale.

Menu names follow recent ATAK-CIV releases and vary by version; search
ATAK settings for the quoted name if not found.

## 1. Joining the mesh

- Join the phone to **its own node's access point**. `meshctl ap` on the
  node shows the SSID, passphrase and a QR code.
- Leave the phone's network settings automatic (DHCP). Every node hands out
  the same gateway and DNS server, `10.73.0.1`, so nothing changes when the
  phone ends up on another node.
- No TAK Server or VPN is needed for SA and chat between phones on the mesh.
- **Disable any VPN on the phone**, or exclude local networks. With a VPN
  active, outbound reports reach the mesh but inbound multicast does not
  traverse the tunnel: map and chat stay empty.

## 2. Multicast groups

Use the ATAK defaults:

| Traffic | Default group | Notes |
|---|---|---|
| SA (position reports) | `239.2.3.1:6969` | Delivered only to nodes with listeners |
| GeoChat | `224.10.10.1:17012` | Not link-local, so it takes the efficient path too |
| Plugin traffic (voice etc.) | plugin-specific | Unknown groups are **not** filtered and pass normally |

Do not move ATAK or a plugin onto these groups:

- `239.255.255.250` (SSDP) and `224.0.0.252` (LLMNR) are dropped at every
  node as chatter.
- `224.0.0.0/24` is link-local multicast. It is flooded and rate-limited to
  20 packets per second per node.

## 3. Packet format: TAK Protocol version 1 (protobuf)

In ATAK's network preferences, set the TAK protocol used for mesh
(multicast) SA to **version 1** (protobuf), not the XML format (version 0).

Why it matters:

- **Size.** batman-adv's multicast packet type, the cheapest way to reach
  many nodes, carries the packet plus 6 bytes per destination node and must
  fit in 1280 bytes. With 50 nodes listening that leaves about 980 bytes for
  the packet. A protobuf position report is a few hundred bytes. An XML one,
  with a callsign, group and a few details, can exceed that. Then
  batman-adv falls back to separate unicasts to each listener node (up to
  16), and to flooding beyond that.
- **Airtime.** Smaller packets are shorter transmissions on every hop.

All phones on a mesh should use the same protocol version.

## 4. Reporting rate

Every SA report is delivered to every node with an ATAK phone, so the
reporting rate multiplies across the whole mesh. Use **dynamic reporting**
(the rate follows movement) and keep it modest:

| Setting | Recommended | Why |
|---|---|---|
| Reporting strategy | Dynamic | Stationary phones report rarely |
| Stationary rate | 30–60 s | Enough to show a stationary phone is alive |
| Moving rate | 5–10 s | Smooth tracks without saturating the mesh |
| Constant rate (if used) | ≥ 5 s | Never faster on a radio mesh |

Rough cost: 50 phones moving at one report per 5 s, about 300 bytes each,
is about 3 kB/s mesh-wide before per-hop delivery. That is comfortable on
standard Wi-Fi links and the main cost to watch on HaLow.

## 5. Power saving

The access point delivers multicast to each phone as unicast
(`multicast_to_unicast`). A phone in power save therefore receives SA and the
mesh's IGMP queries reliably, and its group memberships don't time out.
Battery optimisation can still stop ATAK in the background, so exempt ATAK
from it in Android's settings.

## 6. Checking that it works

On any node:

```
meshctl status
```

The `multicast:` line under batman-adv should read **efficient** with
`IGMP querier ok`. Partial or flooding means some node asks for all
multicast. The node itself also shows a `multicast-want-all` warning that
says why (usually: no querier yet in the first minute after a node starts,
or an IGMP querier on a client device).

The `packet type tx/rx/fwd` counters on the same line grow as batman-adv
uses its multicast packet type, which it prefers for every packet that fits.
A packet too large for it goes as separate unicasts to up to 16 listener
nodes (`batman.multicast_fanout`). Both are acknowledged and use fast rates;
only beyond that does it flood.

## 7. Nodes on the map

A node with a GNSS receiver puts itself on the map. It appears under its
callsign, drawn as equipment (`a-f-G-E-S`, a friendly ground-equipment
frame), not as a person, and its remarks give the fix, the number of
satellites and the accuracy. It reports once a minute while still, at once
when it moves, and goes stale on the map 3 minutes after its last report.

When a node leaves the mesh, another node keeps re-announcing its last
known position, so the marker stays on the map, labelled with the node's
callsign and "(last known)", and with the remarks "last known position: not
on the mesh since HH:MM UTC". When the node returns, its own reports put
the plain callsign back within a minute or two.

A node without a receiver takes its position from a device on its own AP
or Ethernet ports that runs ATAK (its SA, XML or TAK protocol), so it is
still on the map; the remarks say so. Only the device's own position
report counts, not a marker it shares, and only a GPS fix (CoT how
`m-g`): ATAK without a fix reports its last or a hand-set point, which
is not used. Not used either: a device this node fed its own fix in the
last two minutes (section 8), and a source that reports several units
(a TAK gateway passing reports on). Settings → System says which holds a
device back. Other nodes see such a position marked as a device's.
Devices on other nodes are never used. A node's own receiver always
wins.

Nothing needs setting on the phones. Positions stay inside the mesh: they
are never routed to an uplink. To stop all nodes from appearing (emissions
control), turn off **Node positions to ATAK** in
Settings → Mesh profile and send it to every node; one node is turned off
in its own Settings → Node. Neither affects the phones' own reports, which
are set in ATAK.

## 8. The node as the phone's GPS

A node with a GNSS fix sends it once a second, as NMEA (GGA and RMC), to
UDP port 4349 of every device on its own AP or Ethernet ports that runs
ATAK (it sent its own position report in the last two minutes): ATAK's
external GPS input. Other devices get nothing, so their radios can
sleep, and nor does a TAK gateway passing on other units' reports. A
receiver with a good antenna on the node then stands in for a phone's
weaker one (inside a vehicle, a phone in a pocket). In ATAK's GPS
preferences, choose the external GPS (4349 is ATAK's default port for
it). Nothing goes to devices on other nodes. It stops when node
positions are off or the node has no current fix. Settings → System
shows how many devices get it, or why none do.
