# Security model and threat model

What Trellis protects, from whom, how, and what it does not protect yet.
The design is in [architecture.md](architecture.md) §11; this document is
the threat model that design is checked against, and the record of that
check.

## What is worth protecting

| Asset | Why it matters |
|---|---|
| **The mesh secret** (the SAE passphrase in the mesh profile) | Whoever has it can join a node to the mesh, read nothing on its own (links are encrypted per hop) but take part as a member: route traffic, see every client's traffic that crosses it, and send authenticated control data. |
| **Traffic on the mesh** | ATAK situational awareness, chat, voice, and whatever the team's devices send. |
| **Positions** | Every node with a GNSS fix reports where it is. Positions are the most sensitive data the mesh carries. |
| **Control of a node** | The web UI and API change radios, links, the mesh profile and the software. |
| **The software itself** | A node installs releases. A forged release runs as root on every node that takes it. |
| **Availability** | The mesh must keep carrying traffic while nodes move, fail, restart and upgrade. |

## Who it is protected from

| | Adversary | Can reach |
|---|---|---|
| **A1** | Someone in radio range without the mesh secret | The air: every mesh link and every node's client Wi-Fi |
| **A2** | A device on a node's client Wi-Fi or Ethernet port (a team member's phone, or a compromised one) | The client network: every device and node on the mesh, by design |
| **A3** | A host on a gateway's uplink network (the LAN, an LTE router's network) | The node's uplink address |
| **A4** | A mesh member: a node with the mesh secret that is buggy, out of date, or captured and reprogrammed | Everything a node can send: frames, routes, gossip, mesh-wide changes |
| **A5** | Someone with a node in their hands | Its storage: configuration, mesh secret, identity, admin password hash |
| **A6** | Someone who tampers with a release on its way to a node | A package file handed to `meshctl upgrade` or the web UI |

Jamming the radio, or the GNSS signal, is out of reach of any software
measure: it is noted where it changes what a node believes, not where it
stops the radio.

## What stops them

### A1: radio range, no secret

- **Mesh links** are 802.11s with SAE: a node without the passphrase
  cannot peer, and every link is encrypted per hop with management frame
  protection.
- **Client Wi-Fi** is WPA2-PSK with a passphrase of its own per node,
  separate from the mesh secret: knowing a node's Wi-Fi passphrase gives
  the client network (A2), not mesh membership.
- **What remains:** deauthentication and jamming deny service. Repeated
  failed SAE attempts are throttled by wpa_supplicant; meshd lifts a block
  early only for peers that were members on that link moments before.
  wpa_supplicant and hostapd parse frames from anyone in range and run as
  root inside meshd's sandbox (below).

### A2: a device on the client network

- **The client network is flat on purpose** (requirement R2): a device
  reaches every other device on the mesh. A compromised phone can scan
  and attack the team's other devices; Trellis does not isolate clients
  from each other.
- **Node services on the client network:** DHCP and DNS (dnsmasq, on mesh
  addresses only), the web UI (sign-in required), NTP (chrony serves the
  mesh prefix). meshd's own sockets (gossip, link queries) are on the
  mesh bridge, and **client ports drop the gossip port in both
  directions**, so a device can neither read nor inject control data.
  The same port over TCP serves a mesh-wide upgrade's packages while one
  is on (to node addresses known in gossip only); client ports drop it
  too. A package is named by its SHA-256 and checked against it, and
  each node checks its signature with its own release keys.
- **Rogue services** from a device are contained to its port: DHCP-server
  replies, router advertisements and IGMP queries arriving from client
  ports are dropped.
- **Broadcast floods** from a device are limited where they would cost
  every node airtime: IPv4 broadcast other than ARP and allow-listed ports
  is dropped before the mesh, and link-local multicast is rate-limited.
  A device can still send unicast as fast as its link allows.
- **The web UI:** a signed-in admin only; password stored as a salted
  PBKDF2-SHA256 hash readable by root; five wrong passwords from one
  address (an IPv6 /64) lock that address out, doubling up to a minute,
  while other addresses can still sign in; password checks run one at a
  time, each checking the lockout first, so parallel guesses gain nothing
  (about half a second each on a Cortex-A53); webd bounds each request's time,
  the connections per address (32) and its own memory; session cookies are
  `SameSite=Strict`,
  `HttpOnly`, `Secure`, and every change needs a header no other site can
  set. The certificate is the node's own, self-signed: a browser has to be
  told once to trust it, and nothing stops a man in the middle on the
  client Wi-Fi from presenting another the first time.
- **Setup mode** (a node without a mesh profile) runs an open-passphrase
  Wi-Fi, `trellis-setup`, and whoever reaches it first sets the admin
  password. It ends as soon as the node joins a mesh.

### A3: the uplink network

- **The web UI refuses connections whose either end is outside the mesh
  prefix** unless `web.wan_access` is set: a host on the uplink network can
  address a node's mesh address through the gateway's routing, but cannot
  have one.
- **dnsmasq** listens on mesh addresses only. **chrony** listens on all
  addresses but serves only the mesh prefix.
- **SSH** is the operating system's, as the operator configured it.
- **The firewall** (`firewall.wan: closed` in the node configuration, or
  Settings → Access): from anything that is not the mesh side (its uplink
  network, any other interface), the node takes only replies to its own
  traffic, ICMP, DHCP replies, SSH (unless turned off), the web UI where
  it is served beyond the mesh, and the ports its operator lists. Nor
  does it forward connections into the mesh from outside: a host on a
  gateway's uplink network that routes the mesh prefix through it reaches
  no node or device (replies, ICMP and the operator's own port forwards
  pass). The mesh, the node's Wi-Fi and device ports, and the internet it
  forwards for its clients are not filtered. A node set up by Trellis starts
  closed; a node upgraded from an earlier release keeps what it had
  (open) until its operator closes it, and says so under Alerts.
- **What remains:** an open node takes whatever its system serves.
  A closed node stays closed while meshd is stopped (an upgrade, a
  restart, a crash); removing Trellis removes the firewall.
  Positions never leave through a gateway (a lab test checks the uplink
  side), and gossip is never routed. What a gateway itself sends out of
  its uplink is its WAN checks: pings to the probe targets and a plain
  HTTP request to each connectivity-check address, with no cookies and
  nothing about the node (`gateway.web_check: false` stops the requests).
  A network that fakes the answers can at worst get a WAN shared that
  carries no traffic, as the pings alone would.

### A4: a mesh member

Every member is trusted: membership is the mesh secret, and the secret is
shared. What limits a buggy or hostile member:

- **Gossip is authenticated** with a key derived from the mesh secret
  (HMAC-SHA256, replay-checked by boot ID and sequence number), and
  **every record is validated** before use: addresses, IDs and callsigns
  must be well formed, so a member cannot write lines into another node's
  DNS or firewall configuration, or control characters onto an operator's
  terminal. The decoders are fuzzed.
- **Mesh-wide profile changes** come from any member, are authenticated
  and offered with a switch time; each node checks the profile as strictly
  as one installed by hand. A change cannot replace the mesh ID or secret.
  When two parts of a mesh changed it apart, the change that took effect
  later is kept; a member can date its change ahead to win, which gives it
  nothing it could not already do by sending a change.
- **Mesh-wide upgrades** come from any member too, but carry nothing a
  node trusts on the member's word: each node installs a package only if
  it is signed by a release key that node trusts and is newer than the
  one installed, as `meshctl upgrade` does. A hostile member can start,
  stall (a node that does not move on its turn halts the rest after 15
  minutes) or cancel one, and send a node an older signed release that is
  still newer than what it runs.
- **The site link** joins parts of the mesh over the internet with
  WireGuard: a member is admitted by its own key, listed in the profile,
  and each tunnel accepts only what its peer's key sends from that peer's
  tunnel address. A table of the member's own keeps that true on every
  interface: what is addressed to, or sent from, its tunnel address
  anywhere but inside WireGuard is dropped (uplink network and client
  network alike, firewall open or closed), and nothing is routed through
  the WireGuard interface. Every node tells the mesh its public key (gossip); the
  private key never leaves its identity file. What crosses is the mesh itself (batman-adv frames,
  encrypted by WireGuard on the internet, where the radio links' SAE does
  not reach). A member has to be reachable from the internet, or reach
  out to one that is: its UDP port is open on its uplink, answered only
  for a known key (WireGuard says nothing to anyone else). Anyone who can
  change the profile (any member, A4) can add a member, or a key of their
  own.
- **What remains:** a captured node holds the mesh secret, and with it
  everything above: it can join, route, read the traffic it routes, and
  push mesh-wide changes (other links, other channels, unrestricted
  regulatory mode). **Re-keying the mesh** from one node, and per-node keys
  with revocation, are planned; until then a captured node means setting
  every node up with a new profile.

### A5: a node in someone's hands

- The mesh profile, node identity and admin password hash are files
  readable by root only. They are not encrypted at rest: whoever can read
  the storage has the mesh secret (see A4) and the admin password hash.
- **What remains:** the mesh secret of a lost node must be treated as
  known. Storage encryption is up to the image (planned) or the operator.

### A6: tampered releases

- **A release package carries an Ed25519 signature** inside it. A node
  installs only packages signed by a key it trusts: the release keys built
  into the version it runs, and keys its operator added under
  `/etc/trellis/release-keys/`. Through the web UI and API the signature
  is mandatory and a package may not be older than the one installed;
  overriding either is for root on the node (`meshctl upgrade
  --allow-untrusted`, `--allow-downgrade`).
- Unpacking a package is bounded (a crafted archive cannot make the root
  daemon unpack gigabytes), and the package parser is fuzzed.
- **An upgrade checks the node afterwards and goes back** when it is not
  healthy, so a broken release does not strand a node.
- **`trellis-deploy`** checks a release (checksums, and every package's
  signature against its built-in keys) before anything reaches a device.
  It logs in with the operator's SSH key and works through `sudo`; the
  sudo passwords and the mesh secret are never written to its logs. It
  does not give `sudo` without a password unless asked to
  (`--setup --sudo-nopasswd`), because that makes the SSH key alone
  enough for root on the device.
- **What remains:** the release keys are the keys to every node. Two are
  built in, either of which signs a release, each kept offline and apart.
  A lost or stolen key is dropped by the next release, signed with the
  other one, and every node must be upgraded to it; until then the stolen
  key still signs packages the nodes take.

### Positions

- Sent only inside the mesh, to the mesh's ATAK group, with a time-to-live
  of one hop on the bridge; never through a gateway.
- One switch in the mesh profile stops every node reporting (emissions
  control), and each node can be stopped on its own.
- **What remains:** anyone on the client network (A2) receives them, as
  ATAK's own reports. GNSS can be spoofed: a node reports where its
  receiver says it is.

## Privileges on a node

| Process | Runs as | Can |
|---|---|---|
| `meshd` | root, inside a systemd sandbox: only the capabilities networking needs, no new privileges, the file system read-only except its own directories and the network managers' configuration | everything about the node's network, its radios and helper processes, and starting an upgrade |
| `hostapd`, `wpa_supplicant`, `dnsmasq` | children of meshd, inside its sandbox; dnsmasq drops to an unprivileged user | their own jobs |
| `webd` | a user that exists only while it runs, no capabilities, no network namespace of its own, unix sockets only | serve pages and pass a signed-in admin's requests to meshd's socket |
| `trellis crda` | root, run by udev for a moment | answer the kernel's regulatory requests |
| `meshctl` | the operator | talk to meshd's socket (root and the `trellis` group) |

Membership of the `trellis` group is admin access: the API socket needs no
password.

## Review against the implementation

The model above, checked against the code and a running node. Each finding
is either fixed, or planned with its reason.

| | Finding | Status |
|---|---|---|
| R1 | meshd ran as unrestricted root, and hostapd and wpa_supplicant (which parse frames from anyone in radio range) with it; the architecture claimed network capabilities only. | **Fixed:** meshd's unit bounds the capabilities to those networking needs and sandboxes the file system; its children inherit the bounds. |
| R2 | No input firewall on the uplink. | **Fixed:** `firewall.wan: closed`, the default for nodes Trellis sets up; nodes upgraded from earlier releases stay open until their operator closes them (an upgrade never closes ports by surprise), with a notice. A lab test (F1) checks it on a real kernel. |
| R3 | A captured node cannot be shut out. | Planned: re-keying from one node. It needs per-node keys first: the new secret must reach the other nodes in a form the captured node cannot read, although it is a member and may relay the very packets that carry it. |
| R4 | Setup mode's Wi-Fi passphrase is public, and the first visitor sets the admin password. | By design for self-installs; images may carry a per-device passphrase. |
| R5 | The web UI's certificate is trusted on first use. | Accepted for now; a certificate fingerprint in the node's QR code would close it. |
| R6 | Secrets are not encrypted at rest. | Accepted: a node's own storage; image-level encryption planned. |
| R7 | Gossip, packages, profiles, profile codes, configuration, the regulatory database, supervisor events, dnsmasq's lease file, DHCP answers from the uplink, versions, rollouts from other nodes and the CoT writer: parsers of untrusted input. | Fuzzed (`make fuzz`); seeds run in every test run. Findings fixed: a link entry with a negative channel was accepted; a DHCP lease time near 2³² s overflowed the rebinding time. Leases are also bounded (a mask of /8 or longer, at least 10 s; a short lease is kept as granted, never stretched). |
| R8 | A release could be signed with the production key during a trial build, and would then be accepted everywhere. | **Fixed:** development builds are refused a release key unless forced, and trial releases refuse a trusted key. |
| R9 | Client devices are not isolated from each other. | By design (R2); a per-port option to isolate a client could come later. |
| R10 | Voice: trellis-voiced parses RTP and RTCP from any device on the mesh's client network and hands the audio to libopus (C), and a client can talk on any group. | It runs without capabilities in a sandbox (devices: ALSA, hidraw, input, USB serial ports and GPIO chips), fuzzed (`rtp`, `voice`); a CNAME is bounded and filtered to printable ASCII. It has no access to meshd's API: it is not in the `trellis` group, and reads its configuration (no secret in it) from a file meshd keeps for it. Its `input` group lets it read every input device of the node, a keyboard included. Talk groups are not authenticated: anyone on the client network can join or talk, as with the ATAK groups, and can also keep a group busy (a few packets a second refuse every PTT on it), end a talk with a forged RTCP BYE, or take every group over by claiming an all-call. The browser console (any signed-in admin) monitors and talks through the node: its WebSocket accepts only this UI's own origin with a session, ends with that session, and its frames are fuzzed (`webd:FuzzWSRead`). |
| R11 | The site link puts the whole mesh's traffic on the internet between sites, and its members' UDP port on their uplinks. | WireGuard (Noise IK, a key per node) encrypts and authenticates it; an unknown key gets no answer. The port is opened only on members. A member drops tunnel traffic that is not inside WireGuard, on every interface (`inet trellis_site`), so neither its uplink network nor a client device can put frames into a tunnel. A captured member's key is removed from the profile like any member; a site's broadcasts reach every other site's radios, up to a budget per tunnel (`site_link.multicast_kbit`). |
