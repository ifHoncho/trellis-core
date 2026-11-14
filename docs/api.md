# Trellis API v1

One HTTP/JSON API serves the web UI, `meshctl` and anything else that
manages a node. Everything the web UI shows or
changes goes through it; the UI has no private channel.

- [Where it is served](#where-it-is-served)
- [Signing in](#signing-in-over-the-network)
- [Conventions](#conventions)
- [Reading](#reading)
- [Changing configuration](#changing-configuration)
- [Setting a node up](#setting-a-node-up)
- [Other actions](#other-actions)
- [Live status](#live-status)
- [The admin password](#the-admin-password)

## Where it is served

| Where | Base | Who may call |
|---|---|---|
| `meshd`'s unix socket, `/run/trellis/meshd.sock` | `http://meshd/v1` | root, and members of the `trellis` group. No login: the socket's file mode is the access control. This is what `meshctl` uses. |
| `webd`, HTTPS port 443 | `https://<node>/api/v1` | a signed-in admin (below). `webd` checks the login and passes the request to the socket unchanged. |

On the network a node answers at:

- `https://10.73.0.1` or `https://node.mesh` from a device on that node (the
  address every node answers on);
- `https://10.73.<id>.1` or `https://<callsign>.mesh` from anywhere on the mesh.

The addresses always work. The `.mesh` names only resolve on a device that
asks the node for names. A phone with Android's Private DNS set to a
provider, a browser with secure DNS (DNS over HTTPS) set to a provider, or a
VPN sends its lookups elsewhere, where `.mesh` does not exist, and the
browser reports a DNS error (`DNS_PROBE_POSSIBLE` in Chrome). Use the
address there.

The certificate is the node's own (self-signed, kept for ten years so a
browser's exception stays valid). Port 80 only redirects to HTTPS.

`webd` serves a connection only when both ends are inside the mesh prefix:
the UI is reachable from the mesh and the node's client ports, not from its
uplink. `web.wan_access: true` in the node config lifts that, for a bench or
a node managed over its uplink (`meshctl web wan-access on`).

```sh
# on the node
sudo curl --unix-socket /run/trellis/meshd.sock http://meshd/v1/status

# from the mesh
curl -k -c jar -H 'X-Trellis-Request: 1' -d '{"password":"…"}' https://node.mesh/api/v1/login
curl -k -b jar https://node.mesh/api/v1/status
```

## Signing in (over the network)

These endpoints exist on `webd` only.

| Request | Body | Answer |
|---|---|---|
| `GET /session` | | `{authenticated, admin_set, setup, callsign, version, meshd_up}`. Needs no login, and tells a stranger nothing else. |
| `POST /login` | `{"password": "…"}` | `200` and a session cookie; `401` wrong password; `429` with `retry_after_s` after five wrong passwords in a row; `403` when the node has no admin password. |
| `POST /logout` | | ends the session |
| `PUT /password` | `{"current": "…", "new": "…"}` | sets the admin password, ends every other session, and starts a new one. `current` is not needed for the first password of a node in setup mode. |

- The session is a cookie (`trellis_session`: `HttpOnly`, `Secure`,
  `SameSite=Strict`). It ends after 12 hours unused, after 14 days in any
  case, when `webd` restarts, and when the admin password changes.
- Every request other than `GET` must carry the header
  `X-Trellis-Request: 1` (any value). A page on another site can make a
  browser send the cookie, but not that header.

## Conventions

- JSON in, JSON out. Unknown fields in a request are refused, not ignored.
- Errors are `{"error": "message"}` with a 4xx or 5xx status. A rejected
  configuration is `422` and names every field:

  ```json
  {"error": "2 problems: …", "fields": [{"field": "callsign", "message": "must be a DNS label…"}]}
  ```

- Secrets (the mesh passphrase, a configured AP passphrase) read as
  `"<redacted>"`. Sent back unchanged in an update, that value means "keep
  the current one".
- `503` from `webd` means `meshd` is not running.

## Reading

| Request | Answer |
|---|---|
| `GET /health` | `{ok, time, api}` |
| `GET /status` | The node's whole state: identity, profile in force, regulatory mode, radios and plan, mesh links and peers, batman-adv tables and multicast path, bridge, client AP and its stations, DHCP leases, gateway state and candidates, how the node keeps time (`clock`) and its GNSS receiver (`gnss`: fix, position, accuracy, satellites, PPS), ports, the other nodes (from gossip), warnings, the control loop. The types are in `internal/status/status.go`; that file is the reference for every field. |
| `GET /radios` | `{radios, plan}`: the radio part of the status |
| `GET /config` | `{node, profile, setup}`: the configuration in force, secrets redacted. `node` is the document `PUT /config/node` takes. |
| `GET /channels` | What a mesh link may ask for, the same on every node: `{widths, max_channel, s1g}`. `widths` per band in MHz (`"5g": [20, 40, 80, 160]`); Wi-Fi channel numbers run from 1 to `max_channel[band]`; `s1g` holds the HaLow channels per country and width (`{"US": {"2": [2, 6, …]}, …}`). A radio may still lack a channel, and the regulatory domain forbid one: the node reports that in its warnings, and a link's `running_width_mhz` in `GET /status` is the width it actually runs. |
| `GET /ap` | How a phone joins this node's Wi-Fi: `{enabled, ssid, passphrase, band, channel, wifi_qr, qr, note}`. `wifi_qr` is the text of the QR code; `qr` is the symbol itself. |
| `GET /topology` | The mesh as a graph (below) |
| `GET /profile/export` | `{profile, yaml, code, qr}`: the mesh profile **with its secret**, as a file, as a profile code, and as that code's QR symbol |
| `GET /doctor` | `{report}`: the host checks, as `meshctl doctor` prints them |

**QR symbols** are `{"size": n, "rows": ["0101…", …]}`: one string per row,
`1` for a dark module, without the quiet zone. Draw them dark on light with
four light modules around.

**Topology.** `{time, interval_ms, nodes, links}`.

- A node: `{id, callsign, node_id, address, self, version, gateway,
  wan_healthy, reported, report_age_ms, clients, selected_gateway}`. `id` is
  the node's identity MAC.
- A link: `{a, b, ab_mbps, ba_mbps, a_signal_dbm, b_signal_dbm, medium,
  a_mac, b_mac, mesh_link}`: throughput as each end estimates it towards the
  other, the signal each end receives, and the mesh link the two meet on,
  numbered from 1 in the profile's order as `mesh_link.index` is (absent
  when wired, or when neither end runs a release that reports it). Two
  nodes that meet on two bands have two links. The UI names a link by its
  band and channel ("5 GHz · ch 149").
- Each node knows only its own links, so the node asked sends every other
  node a small query over the mesh and assembles the answers (architecture
  §12.5). It asks at most every `interval_ms` (5 s, more in a large mesh),
  and only while someone calls this endpoint. `reported: false` marks a node
  that did not answer (an older release): its links appear from the other
  end only, with its own direction at 0.

## Changing configuration

The API never applies a setting by itself. It validates the new document,
writes the file `meshd` reads at start (`node.yaml` or `profile.yaml`) and
reloads. What is on disk and what runs cannot drift apart, and a change
`meshd` refuses is rolled back.

Add `?dry_run=1` to try a change. The answer is the same in both cases:

```json
{
  "applied": false,
  "changed": true,
  "effects": [
    {"level": "warning", "code": "mesh-link-moves", "message": "The mesh link moves from … to … and drops for a few seconds."}
  ],
  "ap": {"ssid": "relay-7", "passphrase": "…", "qr": {…}}
}
```

- `effects` is what the change does to the running node, most serious
  first. `error`: it cuts the node off from the mesh, or the caller from the
  node. `warning`: it interrupts something. `info`: nothing is interrupted.
  The web UI shows them and asks before it applies.
- `ap` is present when the change alters how devices join the node's Wi-Fi.
- **A change applies to this node only.** Nothing carries a profile to the
  other nodes: each has its own copy, and a mesh-wide change means
  installing it on every one. `PUT /profile` therefore answers about this
  node's links to the others, which keep their profile:
  - `leaves-mesh` (error): the node would have no link left that both
    profiles share and that has a neighbour, with the direct links it loses
    counted and named;
  - `links-drop` (warning): it loses those on one link and stays in the
    mesh over another;
  - `link-added` (info), `link-restarts` (warning, when the order of the
    links changes), `link-no-radio` (info);
  - `nodes-cannot-follow` (warning): the nodes that report no radio for any
    link of the new profile, by name; `nodes-unknown-radios` (info) for
    those that do not report their radios;
  - `ap-band` (warning): the client Wi-Fi shares a mesh radio and changes
    band with it;
  - `this-node-only` (info): the other nodes known, which keep their own
    profile.

  Each node's radios are in `GET /status` under `nodes[].mesh_radios`: the
  bands of each radio that can carry a mesh link, `[["2g","5g"],["s1g"]]`.
- `changed: false` means the node already runs this; nothing is written.

| Request | Body |
|---|---|
| `PUT /config/node` | The whole node config, as `GET /config` returns it under `node`. Fields are those of `node.yaml` (`configs/examples/node.yaml`). A file written this way lists only settings that differ from the defaults, and keeps no comments. |
| `PUT /profile` | Exactly one of: `{"code": "TRP1:…"}` (a profile code, from a QR code or pasted), `{"yaml": "…"}` (a profile file's contents), `{"profile": {…}}` (the document; with `sae_passphrase` `"<redacted>"` the secret is kept), `{"new": {"name", "band", "channel", "width", "mode", "country", "links"}}` (a new mesh with a fresh secret; `links` are further mesh links, `[{"band", "channel", "width"}]`, and a channel left at 0 takes the band's usual one). |

Assigning, pinning and ignoring radios, labels, port roles, the AP and the
gateway are all node config: `radios.roles.mesh` is `"auto"`, `"disabled"`,
`{"radio": "<fingerprint>"}` or `{"port": "<port path>"}`, and so on.

A **profile code** is the mesh profile as one line of text
(`TRP1:` + base64url of the deflated document), short enough for a
57-module QR code and safe to paste into a message. It holds the mesh
secret.

## Setting a node up

A node with no mesh profile is in **setup mode**: it runs alone on a
private mesh of its own, so that its Wi-Fi (`trellis-setup-XXXX`, passphrase
`trellis-setup`) and Ethernet ports hand out addresses and serve this API.
`GET /session` then says `setup: true`.

| Request | Body |
|---|---|
| `POST /setup` | `{"callsign": "relay-7", "node_id": 0}` plus the profile in one of `PUT /profile`'s forms. Writes both files and applies them in one step. Takes `?dry_run=1`; the answer's `ap` is the Wi-Fi to join afterwards. |

The first `PUT /password` of a node in setup mode needs no login: whoever
sets a node up gives it its password. A node that is already part of a mesh
takes its first password only on the node itself (`sudo meshctl web
password`).

## Other actions

| Request | Body | Does |
|---|---|---|
| `POST /radios/identify` | `{"fingerprint": "…", "seconds": 10}` | blinks that radio's LED. `409` when the driver exposes none (`led: false` in the status). |
| `POST /reconcile` | | asks the control loop for a pass now |
| `POST /reload` | | re-reads the configuration files (after editing them by hand) |

## Live status

`GET /events` on `webd` is a server-sent event stream (`text/event-stream`,
gzip when the client accepts it):

| Event | Data |
|---|---|
| `status` | the document of `GET /status`, every 2 seconds |
| `problem` | `{"error": "meshd is not running on this node"}` while there is no status |
| `signed-out` | the session ended; the stream closes |

`webd` asks `meshd` once per interval however many streams are open, and
not at all while none is.

## The admin password

`meshd` keeps the admin password (a salted PBKDF2-SHA256 hash in
`/var/lib/trellis/web-admin.yaml`, readable by root only), not `webd`.
On the socket:

| Request | Body | |
|---|---|---|
| `GET /web` | | what `webd` needs: `{callsign, version, admin_set, admin_since, setup, wan_access, prefix}` |
| `POST /web/login` | `{"password"}` | checks it, with the lockout |
| `PUT /web/password` | `{"current", "new"}` | sets it. Root on the socket needs no `current`, and removes the password with an empty `new`. |

`webd` does not pass these through: over the network only its own
`/login` and `/password` reach them. `meshctl web password` is the reset.
