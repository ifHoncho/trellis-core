# Roadmap

Where Trellis is, and what comes next. Each milestone ends in something that
runs and is tested, not a layer of half-built parts.

## Done

- **Foundations.** One Go binary (`meshd`, `meshctl`, `webd`), a declarative
  configuration, a reconcile loop that owns all network state, and a test
  lab that runs real multi-node meshes in network namespaces.
- **Encrypted mesh.** 802.11s with SAE on every radio link, batman-adv
  (BATMAN_V) routing on top, automatic radio detection and role assignment.
- **Flat client network.** Every device on every node is on one IPv4
  network; names under `.mesh` resolve everywhere; each node is the same
  gateway address for its own devices; roaming devices keep their address.
  Tested at 50 nodes in the lab.
- **Internet sharing and failover.** Any node with an uplink shares it; when
  a gateway fails, new connections flow again within 5 seconds, and
  connections to a gateway that is still healthy stay on it.
- **Efficient multicast.** ATAK's multicast reaches only the nodes with
  listeners, with the measured counters to prove it.
- **Web UI and API.** A node is set up and run from a phone's browser: a
  setup wizard (join a mesh by scanning a QR code, or start one), a live
  map of the mesh, radios, devices, internet and settings. Everything in
  the UI is in a documented HTTP/JSON API.
- **Several mesh links at once.** A node meshes on every band it has a
  radio for (2.4 GHz, 5 GHz, 6 GHz, Wi-Fi HaLow) at the same time, and
  traffic takes the fastest path.
- **Regulatory modes.** Country rules, or unrestricted operation for
  operators with their own frequency authorization, on stock distribution
  kernels.
- **Mesh-wide changes from one node.** Change the mesh's links, channels or
  regulatory mode once; every node switches at the same moment, and a node
  that was away catches up when it returns.
- **Mesh time.** Nodes keep one clock without internet, from a node's GNSS
  receiver where there is one.
- **Channel survey.** How busy each link's channel is at every node, and a
  survey that ranks each link's channels across the mesh and moves a link
  to a clearer one in one step.
- **Nodes on the ATAK map.** A node with a GNSS receiver appears on every
  phone's map, apart from the people; when it leaves the mesh, the others
  keep its last known position there, marked stale. One switch stops it on
  every node.

## In progress

- **Wi-Fi HaLow** links for long range, alongside Wi-Fi.
- **Field testing** with phones running ATAK: a walking test over three or
  more hops, gateways pulled mid-test.

## Next

- **Packages and images:** a `.deb` that never overwrites configuration,
  prebuilt images that boot into a working node, signed releases, and
  upgrades that keep a node's identity and roll back on failure.
- **Security hardening:** a threat model, re-keying a whole mesh without
  touching each node, fuzz testing of everything that parses input.

## Later

- **Team awareness with GNSS:** nodes record where the mesh reaches, for
  range figures and a coverage map, and link their marker to their user's.
  Positions stay inside the mesh unless the operator chooses otherwise.
- **A Trellis plugin for ATAK:** the mesh on ATAK's map with live link
  quality, the state of your own node at a glance, and position reporting
  that adapts to how busy the mesh is. ATAK keeps working fully without
  it.
- **Automatic channel selection:** mesh links that move to a clearer
  channel together on their own when theirs stays congested.
- Push-to-talk voice.
- Linking sites over the internet.
- IPv6 for clients.
