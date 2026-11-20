#!/usr/bin/env bash
# Trellis site link lab: two parts of one mesh in different places, joined
# over an "internet" namespace by the site link (WireGuard, a gretap per
# peer, batman-adv over it).
#
#   site hq:    nodes 1-2-3 on one air bridge; nodes 1 and 2 have public
#               uplinks and hold the site link (node 1 with its firewall
#               closed); node 3 has none.
#   site field: nodes 4-5-6 on another; node 4 holds the site link from
#               behind a NAT router (no endpoint: it dials out); 5 and 6
#               have no uplink.
#
# Checks: each site forms alone; the members' keys, read from meshctl,
# listed in the profile make one mesh of six; devices on either side reach
# each other, and ATAK SA multicast crosses; WireGuard's own packets never
# take the mesh; the link survives losing one of hq's members; a profile
# without the members takes it all down.
#
#   lab/sites.sh [--keep] [--verbose]
#
# Needs make build, and root or an unprivileged user namespace, with
# batman_adv and wireguard loaded (sudo modprobe -a batman-adv wireguard).
set -u
KEEP=0; VERBOSE=0; ARGS=("$@")
while [ $# -gt 0 ]; do
  case "$1" in
    --keep) KEEP=1; shift ;;
    --verbose) VERBOSE=1; shift ;;
    -h|--help) sed -n '2,21p' "$0"; exit 0 ;;
    *) echo "unknown argument $1" >&2; exit 2 ;;
  esac
done
NODES=6
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
BIN="${TRELLIS_BIN:-$ROOT/bin/trellis}"
RUN="${LAB_RUN:-$ROOT/lab/run}/sites"
PASS=0; FAIL=0; RESULTS=()
say()  { printf '%s\n' "$*"; }
dbg()  { [ "$VERBOSE" = 1 ] && printf '    %s\n' "$*" >&2; return 0; }
pass() { PASS=$((PASS+1)); RESULTS+=("PASS $1"); say "  PASS  $1"; }
fail() { FAIL=$((FAIL+1)); RESULTS+=("FAIL $1: $2"); say "  FAIL  $1: $2"; }

[ -x "$BIN" ] || { echo "$BIN not found; run make build" >&2; exit 2; }
for m in batman_adv wireguard; do
  if ! [ -e "/sys/module/$m" ]; then
    if [ "$(id -u)" -eq 0 ]; then modprobe "$m" || exit 2
    else echo "$m is not loaded: sudo modprobe -a batman-adv wireguard" >&2; exit 2; fi
  fi
done
if [ "$(id -u)" -ne 0 ] && [ -z "${TRELLIS_LAB_UNSHARED:-}" ]; then
  export TRELLIS_LAB_UNSHARED=1
  exec unshare -Urmn --propagation private bash -c '
    mount -t tmpfs none /run && mkdir -p /run/netns
    exec "$0" "$@"' "$0" "${ARGS[@]}"
fi
export TRELLIS_LAB_UNSHARED=1
rm -rf "$RUN"; mkdir -p "$RUN"

NS() { ip netns exec "$1" "${@:2}"; }
ctl() { "$BIN" meshctl --socket "$RUN/node$1/run/meshd.sock" "${@:2}"; }
jsonq() {
  ctl "$1" status --json 2>/dev/null | python3 -I -c "import json,sys
s=json.load(sys.stdin)
print($2)" 2>/dev/null
}
EUD="python3 -I $HERE/eud.py"
site_of() { [ "$1" -le 3 ] && echo hq || echo field; }

cleanup() {
  [ "$KEEP" = 1 ] && { say "== keeping the sites lab up (--keep); sockets in $RUN/node*/run"; return; }
  for f in "$RUN"/node*/pid "$RUN"/*.pid; do [ -f "$f" ] && kill "$(cat "$f")" 2>/dev/null; done
  sleep 1
  for f in "$RUN"/node*/pid; do [ -f "$f" ] && kill -9 "$(cat "$f")" 2>/dev/null; done
  for i in $(seq 1 $NODES); do ip netns del "node$i" 2>/dev/null; ip netns del "eud$i" 2>/dev/null; done
  for n in airhq airfield inet natfield; do ip netns del "$n" 2>/dev/null; done
}
trap cleanup EXIT

# ---------------------------------------------------------------- topology
say "== building two sites of three nodes, and the internet between them"
for a in airhq airfield; do
  ip netns add "$a" || { echo "cannot create namespaces" >&2; exit 2; }
  NS "$a" ip link add air0 type bridge
  NS "$a" ip link set air0 up
  NS "$a" sh -c 'echo 0 > /sys/class/net/air0/bridge/multicast_snooping' 2>/dev/null
done
write_profile() {  # write_profile [site_link yaml] [more batman settings]
  cat > "$RUN/profile.yaml.new" <<YAML
schema: 0
name: sites-lab
mesh_id: trellis-sites-lab
sae_passphrase: lab-passphrase-not-secret
regulatory: {mode: country, country: US}
radio: {band: 5g, channel: 149}
prefix: 10.73.0.0/16
batman: {orig_interval_ms: 500, bridge_loop_avoidance: false${2:+, $2}}
$1
YAML
  mv "$RUN/profile.yaml.new" "$RUN/profile.yaml"
}
write_profile ""
start_meshd() {
  ip netns exec "node$1" "$BIN" meshd --node "$RUN/node$1/node.yaml" --state-dir "$RUN/node$1/state" \
      --run-dir "$RUN/node$1/run" --interval 2s --log-level debug --no-host-config \
      --test-port-manager "$RUN/node$1/pm" >> "$RUN/node$1.log" 2>&1 &
  echo $! > "$RUN/node$1/pid"
}
for i in $(seq 1 $NODES); do
  air=air$(site_of "$i")
  mkdir -p "$RUN/node$i/state" "$RUN/node$i/run" "$RUN/node$i/pm"
  cp "$RUN/profile.yaml" "$RUN/node$i/profile.yaml"
  printf 'mac: "02:00:00:00:05:%02x"\n' "$i" > "$RUN/node$i/state/identity.yaml"
  fw=""; [ "$i" = 1 ] && fw="firewall: {wan: closed}"
  cat > "$RUN/node$i/node.yaml" <<YAML
schema: 0
callsign: $(site_of "$i")-$i
node_id: $i
profile: $RUN/node$i/profile.yaml
radios:
  roles: {mesh: disabled, ap: disabled}
ports:
  - {name: eth1, role: mesh}
  - {name: eth2, role: client}
ap: {enabled: false}
gateway:
  probe_targets: [198.51.100.1]
  web_check: false
$fw
YAML
  ip netns add "node$i"; ip netns add "eud$i"
  ip link add eth1 netns "node$i" type veth peer name "p$i" netns "$air"
  ip link add eth2 netns "node$i" type veth peer name eth0 netns "eud$i"
  NS "$air" ip link set "p$i" mtu 1600 master air0 up
  NS "node$i" ip link set lo up; NS "node$i" ip link set eth1 up; NS "node$i" ip link set eth2 up
  NS "eud$i" ip link set lo up; NS "eud$i" ip link set eth0 up
done
# The internet: probe target 198.51.100.1, a public uplink to nodes 1 and 2,
# and the field site's NAT router (its public address 100.64.4.2).
ip netns add inet
NS inet ip link set lo up
NS inet ip addr add 198.51.100.1/32 dev lo
NS inet sh -c 'echo 1 > /proc/sys/net/ipv4/ip_forward'
for i in 1 2; do
  ip link add wan0 netns "node$i" type veth peer name "w$i" netns inet
  NS inet ip addr add "100.64.$i.1/24" dev "w$i"; NS inet ip link set "w$i" up
  NS "node$i" ip addr add "100.64.$i.2/24" dev wan0; NS "node$i" ip link set wan0 up
  NS "node$i" ip route add default via "100.64.$i.1"
done
ip netns add natfield
NS natfield ip link set lo up
ip link add up0 netns natfield type veth peer name w4 netns inet
NS inet ip addr add 100.64.4.1/24 dev w4; NS inet ip link set w4 up
NS natfield ip addr add 100.64.4.2/24 dev up0; NS natfield ip link set up0 up
NS natfield ip route add default via 100.64.4.1
ip link add wan0 netns node4 type veth peer name lan0 netns natfield
NS natfield ip addr add 192.168.60.1/24 dev lan0; NS natfield ip link set lan0 up
NS node4 ip addr add 192.168.60.2/24 dev wan0; NS node4 ip link set wan0 up
NS node4 ip route add default via 192.168.60.1
NS natfield sh -c 'echo 1 > /proc/sys/net/ipv4/ip_forward'
NS natfield nft -f - <<'NFT'
table ip nat {
  chain post { type nat hook postrouting priority srcnat; oifname "up0" masquerade; }
}
NFT
for i in $(seq 1 $NODES); do start_meshd "$i"; done

# wait_origs <count> <timeout> <nodes...>: those nodes see count originators
wait_origs() {
  local want="$1" deadline=$((SECONDS + $2)) i ok; shift 2
  while [ $SECONDS -lt $deadline ]; do
    ok=1
    for i in "$@"; do
      [ "$(jsonq "$i" "sum(1 for o in s['batman']['originators'] if o['best'])")" = "$want" ] || ok=0
    done
    [ "$ok" = 1 ] && return 0
    sleep 1
  done
  return 1
}
origs() { for i in $(seq 1 $NODES); do printf 'node%s:%s ' "$i" "$(jsonq "$i" "sum(1 for o in s['batman']['originators'] if o['best'])")"; done; }

# ---------------------------------------------------------------- tests
say "== tests"
if wait_origs 2 90 1 2 3 4 5 6; then
  pass "S0 two separate meshes: every node sees the 2 others of its site"
else
  fail "S0 sites form" "$(origs)"; exit 1
fi

# The members' keys, as an operator reads them.
for i in 1 2 4; do
  eval "key$i=\$(ctl $i site --json | python3 -I -c 'import json,sys; print(json.load(sys.stdin)[\"public_key\"])')"
done
hint=$(ctl 1 site 2>&1)
if [ ${#key1} = 44 ] && [ ${#key2} = 44 ] && [ ${#key4} = 44 ] && echo "$hint" | grep -q "public_key: \"$key1\""; then
  pass "S1.1 meshctl site gives each node's key, and the line to add it to the profile"
else
  fail "S1.1 keys" "key1=$key1 key2=$key2 key4=$key4: $hint"
fi
# A mistake an operator can make: two nodes of one site listed as two
# sites, neither with an endpoint. The review says both before anything
# is sent (nodes tell each other their keys).
write_profile "site_link:
  members:
    - {name: hq-1, site: hq, public_key: \"$key1\"}
    - {name: hq-2, site: elsewhere, public_key: \"$key2\"}"
deadline=$((SECONDS + 30))
while [ $SECONDS -lt $deadline ] && [ "$(jsonq 1 "sum(1 for n in s['nodes'] if n.get('site_key'))")" != 2 ]; do sleep 1; done
out=$(ctl 1 profile push "$RUN/profile.yaml" 2>&1)
if echo "$out" | grep -q "already connected within the mesh without a site link" && echo "$out" | grep -q "sites cannot connect" && echo "$out" | grep -q "Nothing sent"; then
  pass "S1.1b two nodes of one site listed as two sites, without endpoints: the review says they are at one place and cannot dial each other"
else
  fail "S1.1b review of a mistaken list" "$out"
fi

# The operator's way: the members sent to every node at hq from node 1,
# then hq's profile file installed on one node in the field, which the
# rest of the field takes from it.
write_profile "site_link:
  members:
    - {name: hq-1, site: hq, public_key: \"$key1\", endpoint: \"100.64.1.2:51873\"}
    - {name: hq-2, site: hq, public_key: \"$key2\", endpoint: \"100.64.2.2:51873\"}
    - {name: field-4, site: field, public_key: \"$key4\"}"
has_link() { grep -q '^site_link:' "$RUN/node$1/profile.yaml" 2>/dev/null; }
out=$(ctl 1 profile push --yes --in 15 "$RUN/profile.yaml" 2>&1) || fail "S1.2 push at hq" "$out"
deadline=$((SECONDS + 40))
while [ $SECONDS -lt $deadline ] && ! { has_link 1 && has_link 2 && has_link 3; }; do sleep 1; done
rev=$(sed -n 's/^revision: //p' "$RUN/node1/profile.yaml")
# The field does not have the list yet: hq-1 says why its tunnel is quiet
# (after the 20 s a new tunnel is given to answer).
deadline=$((SECONDS + 50))
while [ $SECONDS -lt $deadline ]; do
  note=$(jsonq 1 "s['site_link']['peers'][0].get('note','')")
  [ -n "$note" ] && break
  sleep 1
done
if echo "$note" | grep -q "no handshake yet" && echo "$note" | grep -q "remote has no endpoint" && ctl 1 site | grep -q "field-4: no handshake yet"; then
  pass "S1.2b before the field has the list, hq-1 says what its quiet tunnel waits for (status and meshctl site)"
else
  fail "S1.2b note on a quiet tunnel" "note=[$note] $(ctl 1 site 2>&1)"
fi
out=$(ctl 4 profile install --target "$RUN/node4/profile.yaml" "$RUN/node1/profile.yaml" 2>&1) || fail "S1.2 install in the field" "$out"
deadline=$((SECONDS + 60))
while [ $SECONDS -lt $deadline ] && ! { has_link 5 && has_link 6; }; do sleep 1; done
revs=$(for i in $(seq 1 $NODES); do sed -n 's/^revision: //p' "$RUN/node$i/profile.yaml"; done | sort -u | tr '\n' ' ')
if [ -n "$rev" ] && [ "$revs" = "$rev " ] && has_link 5 && has_link 6; then
  pass "S1.2 members pushed at hq (revision $rev), the file installed on field-4, nodes 5 and 6 took it from there"
else
  fail "S1.2 profile to both sites" "revisions on the six nodes: [$revs]; field nodes with the link: $(for i in 4 5 6; do has_link $i && printf '%s ' $i; done)"
fi

# The tunnels: node 4 to both hq members, each hq member to node 4.
up_peers() { jsonq "$1" "' '.join(sorted(p['name'] for p in s['site_link']['peers'] if p['up'] and p['batman']))"; }
deadline=$((SECONDS + 60))
while [ $SECONDS -lt $deadline ]; do
  [ "$(up_peers 4)" = "hq-1 hq-2" ] && [ "$(up_peers 1)" = field-4 ] && [ "$(up_peers 2)" = field-4 ] && break
  sleep 1
done
p4=$(up_peers 4); p1=$(up_peers 1); p2=$(up_peers 2)
cur4=$(jsonq 1 "s['site_link']['peers'][0].get('current','')")
mem3=$(jsonq 3 "s['site_link']['member']")
if [ "$p4" = "hq-1 hq-2" ] && [ "$p1" = field-4 ] && [ "$p2" = field-4 ] && [ "$mem3" = False ]; then
  pass "S1.3 site link up: field-4 (behind NAT, seen by hq-1 at $cur4) to hq-1 and hq-2, both batman-adv interfaces; node 3 not a member"
else
  fail "S1.3 tunnels" "node4=[$p4] node1=[$p1] node2=[$p2] node3 member=$mem3 (diagnostics: $RUN/s1-diag.txt)"
  { for i in 1 2 4; do echo "== node$i"; ctl "$i" site; NS "node$i" wg show site0; NS "node$i" ip rule; NS "node$i" ip route show table all | grep -E 'table 739|site'; done
    NS inet timeout 5 tcpdump -Z root -ni w4 udp > "$RUN/s1-w4.txt" 2>&1 & c1=$!
    NS inet timeout 5 tcpdump -Z root -ni w1 udp > "$RUN/s1-w1.txt" 2>&1 & c2=$!
    NS node1 timeout 5 tcpdump -Z root -ni wan0 udp > "$RUN/s1-node1-wan0.txt" 2>&1 & c3=$!
    wait $c1 $c2 $c3
    echo "== inet w4"; cat "$RUN/s1-w4.txt"; echo "== inet w1"; cat "$RUN/s1-w1.txt"; echo "== node1 wan0"; cat "$RUN/s1-node1-wan0.txt"
    echo "== node1 nft"; NS node1 nft list ruleset; } > "$RUN/s1-diag.txt" 2>&1
fi

if wait_origs 5 60 1 2 3 4 5 6; then
  pass "S2.1 one mesh of six: every node sees the 5 others"
else
  fail "S2.1 one mesh" "$(origs)"
fi
deadline=$((SECONDS + 60))
while [ $SECONDS -lt $deadline ]; do
  g=$(for i in 3 6; do jsonq "$i" "len(s['nodes'])"; done | sort -u)
  [ "$g" = 5 ] && break
  sleep 1
done
if [ "$g" = 5 ]; then pass "S2.2 gossip crosses: nodes 3 and 6 know all 5 others by name"; else fail "S2.2 gossip" "nodes known: $(echo "$g" | tr '\n' ' ')"; fi
# With the link up: each tunnel names the node at its far end, and has
# nothing to say.
cs=$(jsonq 1 "s['site_link']['peers'][0].get('callsign','')"); want=$(jsonq 4 "s['node']['callsign']")
notes=$(for i in 1 2 4; do jsonq "$i" "sum(1 for p in s['site_link']['peers'] if p.get('note'))"; done | tr '\n' ' ')
if [ -n "$cs" ] && [ "$cs" = "$want" ] && [ "$notes" = "0 0 0 " ]; then
  pass "S2.2b the tunnels name the node at the far end ($cs) and carry no note while up"
else
  fail "S2.2b peer names and notes" "callsign=[$cs] want=[$want] notes per member=[$notes]"
fi

# Any node says how the link stands: node 3, no member, lists the three
# members with the tunnels each reports.
want="field-4:2/2 hq-1:1/1 hq-2:1/1"
deadline=$((SECONDS + 60))
while [ $SECONDS -lt $deadline ]; do
  ros=$(jsonq 3 "' '.join(sorted('%s:%d/%d' % (m.get('name'), m['up'], m['peers']) for m in s['site_link']['roster'] if m['reported']))")
  [ "$ros" = "$want" ] && break
  sleep 1
done
if [ "$ros" = "$want" ] && ctl 3 site | grep -q "members across the mesh"; then
  pass "S2.2c node 3, no member, shows every member's tunnels as the members report them ($ros)"
else
  fail "S2.2c roster on a node that is no member" "roster=[$ros] $(ctl 3 site 2>&1)"
fi

# Devices on node 3 (hq, no uplink) and node 6 (field, no uplink).
lease() {
  local l ip
  l=$(NS "eud$1" $EUD dhcp eth0 2>&1); ip=$(echo "$l" | sed -n 's/^ip=//p')
  [ -n "$ip" ] && { NS "eud$1" ip addr add "$ip/16" dev eth0; NS "eud$1" ip route add default via 10.73.0.1; }
  echo "$ip"
}
eud3=$(lease 3); eud6=$(lease 6)
if [ -n "$eud3" ] && [ -n "$eud6" ] && NS eud3 ping -c 5 -i 0.3 -W 2 "$eud6" 2>/dev/null | grep -q ' [1-9][0-9]* received'; then
  pass "S3.1 a device in hq ($eud3) reaches one in the field ($eud6) across the site link"
else
  fail "S3.1 device to device" "eud3=$eud3 eud6=$eud6"
fi
if [ -n "$eud6" ] && NS eud6 ping -c 3 -s 1400 -W 2 "$eud3" 2>/dev/null | grep -q ' [1-9][0-9]* received'; then
  pass "S3.2 full-size packets cross (1428-byte frames over a 1382-byte tunnel: batman-adv fragments)"
else
  fail "S3.2 full-size packets" "no reply to 1400-byte pings"
fi
# ATAK SA: the device on node 6 listens on 239.2.3.1:6969, the one on node 3 sends.
NS eud6 $EUD mlisten eth0 239.2.3.1 6969 10 > "$RUN/sa-listen.txt" 2>&1 &
lp=$!
sleep 4
NS eud3 $EUD msend eth0 239.2.3.1 6969 50 300
wait $lp
got=$(sed -n 's/^received //p' "$RUN/sa-listen.txt")
if [ "${got:-0}" -ge 45 ]; then
  pass "S3.3 ATAK SA crosses: $got/50 CoT datagrams from hq reached the field's listener"
else
  fail "S3.3 ATAK SA" "received ${got:-nothing} of 50"
fi

# WireGuard's packets: out of the uplink, never into the mesh.
r4=$(NS node4 ip route get 100.64.1.2 mark 0x73530000 2>&1)
NS node4 ip route del default via 192.168.60.1
r4b=$(NS node4 ip route get 100.64.1.2 mark 0x73530000 2>&1)
r4c=$(NS node4 ip route get 100.64.1.2 2>&1)
NS node4 ip route add default via 192.168.60.1
rules=$(NS node4 ip rule | grep -cE '^(7190|32798|32799):')
if echo "$r4" | grep -q 'dev wan0' && echo "$r4b" | grep -qiE 'unreachable|no route' && [ "$rules" -ge 2 ]; then
  pass "S4 WireGuard's packets leave by the uplink, and with the uplink's route gone are unreachable, not sent into the mesh (anything else: $(echo "$r4c" | head -1 | awk '{print $2,$3,$4,$5}'))"
else
  fail "S4 underlay routing" "with route: $r4 | without: $r4b | rules: $rules"
fi
tun=$(NS node1 nft list table inet trellis_fw 2>/dev/null | grep -c '51873\|"site0"')
if [ "$tun" -ge 2 ] && [ "$(up_peers 1)" = field-4 ]; then
  pass "S5 node 1's firewall is closed, and lets the site link in (its port and site0)"
else
  fail "S5 firewall" "rules=$tun peers=[$(up_peers 1)]"
fi

# A member settles: its tunnel routes stay put (they were once deleted and
# added again on every pass, with the loop running twice a second), and
# its loop says converged.
NS node2 timeout 8 ip monitor route > "$RUN/s5b-routes.txt" 2>&1
flap=$(grep -c '169\.254\.' "$RUN/s5b-routes.txt")
conv=""
deadline=$((SECONDS + 20))
while [ $SECONDS -lt $deadline ]; do
  conv=$(for i in 1 2 4; do jsonq "$i" "s['reconcile']['converged']"; done | tr '\n' ' ')
  [ "$conv" = "True True True " ] && break
  sleep 1
done
if [ "$flap" = 0 ] && [ "$conv" = "True True True " ]; then
  pass "S5b the members settle: no tunnel route changed in 8 s on node 2, and nodes 1, 2 and 4 say converged"
else
  fail "S5b members settle" "tunnel route events in 8 s: $flap; converged: [$conv]"
fi

# The guard: a tunnel takes nothing that did not come through WireGuard.
# A host on node 2's uplink network (its firewall is open) sends GRE
# between the tunnel's two addresses, as the tunnel's far end would.
self2=$(jsonq 2 "s['site_link']['address']")
peer2=$(jsonq 2 "s['site_link']['peers'][0]['address']")
guards=$(for i in 1 2 4; do NS "node$i" nft list table inet trellis_site 2>/dev/null | grep -c 'counter packets'; done | tr '\n' ' ')
none3=0; NS node3 nft list table inet trellis_site >/dev/null 2>&1 || none3=1
gdrop() { NS node2 nft list chain inet trellis_site input 2>/dev/null | sed -n 's/.*counter packets \([0-9]*\).*/\1/p' | head -1; }
g0=$(gdrop)
NS inet ip addr add "$peer2/32" dev lo
NS inet ip route add "$self2/32" via 100.64.2.2 dev w2 src "$peer2"
NS inet ip link add fake type gretap local "$peer2" remote "$self2" ttl 64
NS inet ip addr add 10.99.0.2/24 dev fake; NS inet ip link set fake up
NS inet ping -c 3 -i 0.2 -W 1 10.99.0.1 >/dev/null 2>&1
NS inet ip link del fake; NS inet ip route del "$self2/32"; NS inet ip addr del "$peer2/32" dev lo
g1=$(gdrop)
if [ "$guards" = "4 4 4 " ] && [ "$none3" = 1 ] && [ -n "$g0" ] && [ "$((g1 - g0))" -ge 1 ] && [ "$(up_peers 2)" = field-4 ]; then
  pass "S5c the guard stands on the members (not on node 3): GRE sent to node 2's tunnel address from its uplink is dropped ($((g1 - g0)) packets), and its tunnel stays up"
else
  fail "S5c guard" "rules per member [$guards], node 3 without=$none3, self=$self2 peer=$peer2, dropped ${g0:-?} -> ${g1:-?}, peers [$(up_peers 2)]"
fi

# The guard's rules emptied (a tool that flushes rulesets but keeps the
# tables): the table is still there, and guards nothing. Put back.
NS node2 nft flush table inet trellis_site
back=0
for _ in $(seq 1 40); do
  [ "$(NS node2 nft list table inet trellis_site 2>/dev/null | grep -c 'counter packets')" = 4 ] && { back=1; break; }
  sleep 0.5
done
if [ "$back" = 1 ] && [ "$(up_peers 2)" = field-4 ]; then
  pass "S5c2 the guard's rules, flushed out of their table on node 2, are put back, and its tunnel stays up"
else
  fail "S5c2 guard emptied" "rules now: $(NS node2 nft list table inet trellis_site 2>&1 | grep -c 'counter packets'), peers [$(up_peers 2)]"
fi

# Two parts of a site joined by a slow radio only (HaLow-like: 3 Mbit/s
# between hq's two members): node 1 reaches node 2 over it, not out through
# field-4 and back in. Two tunnels at 10 Mbit/s are worth 8.8 to
# batman-adv; the tunnels' own hop penalty brings that under 1. The link's
# estimate comes down from the veth's 10 Gbit/s over some 40 s.
hpen=$(for i in 1 2 4; do for d in $(NS "node$i" ls /sys/class/net | grep '^site-'); do NS "node$i" batctl hardif "$d" hop_penalty; done; done | sort -u | tr '\n' ' ')
orig2=$(NS node2 cat /sys/class/net/eth1/address)
for i in 1 2; do NS "node$i" batctl hardif eth1 throughput_override 3mbit; done
slow=""; deadline=$((SECONDS + 60))
while [ $SECONDS -lt $deadline ]; do
  slow=$(jsonq 1 "next((n['throughput_mbps'] for n in s['batman']['neighbors'] if n['mac']=='$orig2' and n['via']=='eth1'), 99) <= 4")
  [ "$slow" = True ] && break
  sleep 1
done
sleep 6  # OGMs every 500 ms: a round trip through the field would be the best path by now
route12=$(jsonq 1 "next(o['via']+' '+str(o['throughput_mbps']) for o in s['batman']['originators'] if o['best'] and o['mac']=='$orig2')")
for i in 1 2; do NS "node$i" batctl hardif eth1 throughput_override 0; done
if [ "$hpen" = "174 " ] && [ "$slow" = True ] && [ "${route12%% *}" = eth1 ]; then
  pass "S5d hq's members joined by 3 Mbit/s only: node 1 reaches node 2 over it ($route12 Mbit/s), not through the field; tunnels' hop penalty $hpen"
else
  fail "S5d no round trip through another site" "tunnel hop penalties [$hpen], link slowed=$slow, route to node 2 [$route12]"
fi
sleep 5  # the air link back at full speed before S6

# Losing the hq member the field reaches node 3 through: the link stays
# through the other.
# batman-adv names node 3 by its primary interface's MAC.
orig3=$(NS node3 cat /sys/class/net/eth1/address)
via=$(jsonq 4 "next(o['via'] for o in s['batman']['originators'] if o['best'] and o['mac']=='$orig3')")
cut=$(jsonq 4 "next(p['name'] for p in s['site_link']['peers'] if p['interface']=='$via')")
keep=hq-2; w=w1; [ "$cut" = hq-2 ] && { keep=hq-1; w=w2; }
NS eud6 $EUD pinger "$eud3" 40 > "$RUN/s6-pinger.txt" 2>&1 &
pp=$!
sleep 3
t0=$(date +%s.%N)
NS inet ip link set "$w" down
wait $pp
# The longest gap between replies once the member was cut off.
gap=$(python3 -I - "$RUN/s6-pinger.txt" "$t0" <<'PY'
import sys
t0=float(sys.argv[2]); ts=[float(l.split()[1]) for l in open(sys.argv[1]) if l.startswith('reply ')]
after=[t for t in ts if t > t0]
before=[t for t in ts if t <= t0]
if not after: print('none'); sys.exit()
prev=before[-1] if before else t0
g=0
for t in after:
    g=max(g,t-prev); prev=t
print(f"{g*1000:.0f}")
PY
)
via2=$(jsonq 4 "next(o['via'] for o in s['batman']['originators'] if o['best'] and o['mac']=='$orig3')")
now=$(jsonq 4 "next(p['name'] for p in s['site_link']['peers'] if p['interface']=='$via2')")
if [ -n "$cut" ] && [ "$gap" != none ] && [ "${gap:-99999}" -lt 15000 ] && [ "$now" = "$keep" ]; then
  pass "S6 $cut, which carried the field's traffic to node 3, cut off: devices keep talking through $now (longest gap ${gap} ms)"
else
  fail "S6 losing a member" "carried by [$cut] ($via), now [$now] ($via2), gap=${gap} ms"
fi
NS inet ip link set "$w" up

# The broadcast budget: at 100 kbit/s per tunnel and direction, a 520
# kbit/s multicast flood (flood mode: batman-adv broadcasts every
# multicast) is cut at hq's tunnels; routing and unicast are not.
budget_drops() {  # packets the budget dropped on node $1's tunnels, egress
  local i=$1 n=0 d
  for t in $(NS "node$i" ip -o link show | grep -oE 'site-[0-9a-f]{6}' | sort -u); do
    d=$(NS "node$i" tc -s filter show dev "$t" egress 2>/dev/null | sed -n 's/.*(dropped \([0-9]*\),.*/\1/p' | awk '{n+=$1} END {print n+0}')
    n=$((n + ${d:-0}))
  done
  echo "$n"
}
write_profile "site_link:
  multicast_kbit: 100
  members:
    - {name: hq-1, site: hq, public_key: \"$key1\", endpoint: \"100.64.1.2:51873\"}
    - {name: hq-2, site: hq, public_key: \"$key2\", endpoint: \"100.64.2.2:51873\"}
    - {name: field-4, site: field, public_key: \"$key4\"}" "multicast_flood: true"
out=$(ctl 1 profile push --yes --in 15 "$RUN/profile.yaml" 2>&1) || fail "S8 push" "$out"
deadline=$((SECONDS + 45))
while [ $SECONDS -lt $deadline ] && ! grep -q 'multicast_kbit: 100' "$RUN/node4/profile.yaml"; do sleep 1; done
sleep 4
rate=$(NS node1 tc filter show dev "$(jsonq 1 "s['site_link']['peers'][0]['interface']")" egress 2>/dev/null | grep -oE 'rate [0-9]+Kbit' | head -1)
d0=$(( $(budget_drops 1) + $(budget_drops 2) ))
NS eud3 $EUD msend eth0 239.9.9.9 9999 200 1300
d1=$(( $(budget_drops 1) + $(budget_drops 2) ))
ok6=0; NS eud3 ping -c 3 -i 0.3 -W 2 "$eud6" >/dev/null 2>&1 && ok6=1
if [ "$rate" = "rate 100Kbit" ] && [ $((d1 - d0)) -ge 100 ] && [ "$ok6" = 1 ] && [ "$(up_peers 4)" = "hq-1 hq-2" ]; then
  pass "S8 budget at 100 kbit/s: of a 520 kbit/s multicast flood (flood mode), hq's tunnels dropped $((d1 - d0)) of 2×200 copies; tunnels and unicast unaffected"
else
  fail "S8 budget" "policer [$rate], dropped $((d1 - d0)), unicast ok=$ok6, node 4's peers [$(up_peers 4)]"
fi

# Full-size broadcast crosses too. batman-adv splits only unicast: a
# flooded multicast frame of 1492 bytes is larger than a tunnel (1382), and
# gets across as fragments of the tunnel's own packets. Five datagrams,
# within the budget's burst.
sleep 5
NS eud6 $EUD mlisten eth0 239.9.9.8 9998 9 > "$RUN/big-listen.txt" 2>&1 &
lp=$!
sleep 4
NS eud3 $EUD msend eth0 239.9.9.8 9998 5 1450
wait $lp
big=$(sed -n 's/^received //p' "$RUN/big-listen.txt")
if [ "${big:-0}" -ge 4 ]; then
  pass "S8b full-size broadcast crosses: $big/5 flooded multicast datagrams of 1450 bytes from hq reached the field's listener"
else
  fail "S8b full-size broadcast" "received ${big:-nothing} of 5 datagrams of 1450 bytes"
fi

# No profile conflict anywhere: both sites hold the one change.
conf=$(for i in $(seq 1 $NODES); do jsonq "$i" "sum(1 for w in s['warnings'] if w['code']=='mesh-change-conflict')"; done | sort -u | tr '\n' ' ')
if [ "$conf" = "0 " ]; then pass "S2.3 no profile conflict when the sites met"; else fail "S2.3 conflict" "warnings per node: $conf"; fi

# The members taken out of the profile, sent from hq to every node of
# both sites: everything taken down.
write_profile ""
out=$(ctl 1 profile push --yes --in 15 "$RUN/profile.yaml" 2>&1) || fail "S7 push" "$out"
deadline=$((SECONDS + 45))
while [ $SECONDS -lt $deadline ]; do
  n=0; for i in $(seq 1 $NODES); do has_link "$i" && n=$((n+1)); done
  [ "$n" = 0 ] && break
  sleep 1
done
# The interfaces go first, then the rules and the guard: wait for all.
deadline=$((SECONDS + 20))
while [ $SECONDS -lt $deadline ]; do
  left=$(for i in 1 2 4; do NS "node$i" ip -o link show | grep -cE ' (site0|site-[0-9a-f]{6})[@:]'; done | awk '{n+=$1} END {print n+0}')
  rl=$(for i in 1 2 4; do NS "node$i" ip rule | grep -cE '^(7190|32798|32799):'; done | awk '{n+=$1} END {print n+0}')
  gl=$(for i in 1 2 4; do NS "node$i" nft list table inet trellis_site 2>/dev/null | grep -c 'table inet'; done | awk '{n+=$1} END {print n+0}')
  [ "$left" = 0 ] && [ "$rl" = 0 ] && [ "$gl" = 0 ] && break
  sleep 1
done
# batman-adv keeps an originator it no longer hears for up to 200 s: the
# other site's must stop being refreshed, and the devices stop reaching
# each other.
sleep 8
fresh=$(jsonq 3 "sum(1 for o in s['batman']['originators'] if o['best'] and o['last_seen_ms'] < 5000)")
apart=1; NS eud3 ping -c 3 -i 0.3 -W 1 "$eud6" >/dev/null 2>&1 && apart=0
if [ "$left" = 0 ] && [ "$rl" = 0 ] && [ "$gl" = 0 ] && [ "$fresh" = 2 ] && [ "$apart" = 1 ]; then
  pass "S7 site link taken out of the profile: tunnels, site0, rules and the guard gone; node 3 hears only its own site, and the devices are apart again"
else
  fail "S7 teardown" "interfaces left $left, rules left $rl, guard tables left $gl, node 3 hears $fresh originators, devices apart=$apart (diagnostics: $RUN/s7-diag.txt)"
  { for i in 1 4 6; do echo "== node$i"; NS "node$i" batctl meshif bat0 if; NS "node$i" batctl meshif bat0 o; NS "node$i" batctl meshif bat0 n; NS "node$i" ip -br link; done; } > "$RUN/s7-diag.txt" 2>&1
fi

say ""
say "== results: $PASS passed, $FAIL failed"
[ "$FAIL" = 0 ]
