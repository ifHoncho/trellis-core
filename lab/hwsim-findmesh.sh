#!/usr/bin/env bash
# Finding the mesh again over virtual radios: nodes switched off while the
# mesh moves its channel come back on the old one, hear nobody, look for
# the rest on other channels and follow it (architecture.md, "Finding the
# mesh again"; findmesh.go). Root only, in a VM (lab/hwsim-lib.sh).
#
#   sudo lab/hwsim-findmesh.sh
#
# F1  setup: 5 nodes form an 802.11s + SAE mesh on 5 GHz ch 36
# F2  node 5 off; node 1 moves the mesh to ch 44: nodes 1-4 switch
# F3  the larger part (4 of 5, each hearing 3 of 4) never looks
# F4  node 5, back on ch 36 alone, finds the mesh on ch 44 and joins it on
#     the mesh's revision
# F5  nodes 4 and 5 off; node 1 moves the mesh to ch 149; both, back on
#     ch 44 hearing only each other, find it and join; every node ends on
#     one revision and one channel
set -u
NODES=5
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
BIN="${TRELLIS_BIN:-$ROOT/bin/trellis}"
RUN="${LAB_RUN:-$ROOT/lab/run}/hwsim-findmesh"
PASS=0; FAIL=0
pass() { PASS=$((PASS+1)); echo "  PASS  $1"; }
fail() { FAIL=$((FAIL+1)); echo "  FAIL  $1: $2"; }
[ "$(id -u)" -eq 0 ] || { echo "root required" >&2; exit 2; }
[ -x "$BIN" ] || { echo "binary $BIN missing" >&2; exit 2; }
command -v wpa_supplicant >/dev/null || { echo "wpa_supplicant missing" >&2; exit 2; }

. "$HERE/hwsim-lib.sh"
LAB_NS=$(for i in $(seq 1 "$NODES"); do echo "node$i"; done)
# shellcheck disable=SC2086
hwsim_preflight $LAB_NS
modprobe -r mac80211_hwsim 2>/dev/null
modprobe mac80211_hwsim radios="$NODES" || { echo "cannot load mac80211_hwsim" >&2; exit 2; }
modprobe batman-adv || exit 2
if [ ! -d "$(dirname "$RUN")" ]; then
  mkdir -p "$(dirname "$RUN")"
  [ -n "${SUDO_UID:-}" ] && chown "$SUDO_UID:${SUDO_GID:-$SUDO_UID}" "$(dirname "$RUN")"
fi
rm -rf "$RUN"; mkdir -p "$RUN"

NS() { ip netns exec "$1" "${@:2}"; }
ctl() { "$BIN" meshctl --socket "$RUN/node$1/run/meshd.sock" "${@:2}"; }
jsonq() { ctl "$1" status --json 2>/dev/null | python3 -I -c "import json,sys
s=json.load(sys.stdin)
print($2)" 2>/dev/null; }

# shellcheck disable=SC2086
cleanup() { hwsim_cleanup $LAB_NS; }
trap cleanup EXIT

# Each node has its own profile file: a node that switches rewrites it.
for i in $(seq 1 "$NODES"); do
  mkdir -p "$RUN/node$i/state" "$RUN/node$i/run"
  cat > "$RUN/node$i/profile.yaml" <<YAML
schema: 0
name: hwsim
mesh_id: trellis-hwsim
sae_passphrase: right-passphrase-1234
regulatory: {mode: country, country: US}
radio: {band: 5g, channel: 36, width: 20}
batman: {orig_interval_ms: 500}
YAML
  cat > "$RUN/node$i/node.yaml" <<YAML
schema: 0
callsign: hwsim-$i
node_id: $i
profile: $RUN/node$i/profile.yaml
ap: {enabled: false}
YAML
done

start_node() { # i
  bg_in "node$1" "$BIN" meshd --node "$RUN/node$1/node.yaml" --state-dir "$RUN/node$1/state" \
      --run-dir "$RUN/node$1/run" --interval 2s --log-level debug --no-host-config >> "$RUN/node$1.log" 2>&1
  echo $! > "$RUN/node$1/pid"
}
stop_node() { stop_pid "$(cat "$RUN/node$1/pid")"; rm -f "$RUN/node$1/pid"; }

mapfile -t phys < <(ls /sys/class/ieee80211/ | sort -V | tail -n "$NODES")
for i in $(seq 1 "$NODES"); do
  ip netns add "node$i"
  NS "node$i" ip link set lo up
  iw phy "${phys[$((i-1))]}" set netns name "node$i"
done
for i in $(seq 1 "$NODES"); do start_node "$i"; done

rev() { sed -n 's/^revision: //p' "$RUN/node$1/profile.yaml"; }
chan() { jsonq "$1" "s['mesh_link'].get('channel')"; }
origs() { jsonq "$1" "sum(1 for o in s['batman']['originators'] if o['best'])"; }
warned() { jsonq "$1" "' '.join(w['code'] for w in s.get('warnings') or [] if w['code'] in ('mesh-alone','mesh-visit'))"; }
where() { for i in $(seq 1 "$NODES"); do [ -f "$RUN/node$i/pid" ] && printf 'node%s=rev%s/ch%s/%s ' "$i" "$(rev "$i")" "$(chan "$i")" "$(origs "$i")"; done; }

# meshed NODE... CHANNEL: each listed node is joined on CHANNEL and has at
# least as many originators as there are other listed nodes (batman keeps
# a node switched off as an originator until it times out).
meshed() {
  local ch="${*: -1}" nodes=("${@:1:$#-1}") i o
  for i in "${nodes[@]}"; do
    o=$(origs "$i")
    [ "$(chan "$i")" = "$ch" ] && [ "${o:-0}" -ge "$((${#nodes[@]}-1))" ] || return 1
  done
}
wait_for() { # seconds command...
  local deadline=$((SECONDS + $1)); shift
  while [ $SECONDS -lt $deadline ]; do "$@" && return 0; sleep 2; done
  return 1
}
same_rev() { local r; r=$(rev "$1"); for i in "$@"; do [ -n "$r" ] && [ "$(rev "$i")" = "$r" ] || return 1; done; }

# move FROM CHANNEL: FROM sends the profile it runs, on CHANNEL, to switch
# in 15 s.
move() {
  cp "$RUN/node$1/profile.yaml" "$RUN/push.yaml"
  ctl "$1" profile band 5g --channel "$2" "$RUN/push.yaml" >/dev/null
  ctl "$1" profile push --in 15 --yes "$RUN/push.yaml" 2>&1 | tail -1
}

# know NODE...: each listed node has every other listed one in its gossip
# view. batman forms in some 20 s, gossip takes longer: a change sent
# before reaches only the nodes its sender knows (none, then it moves
# alone), and a node not yet heard is not in the roster that tells a node
# it is in the smaller part.
know() {
  local i j ids
  for i in "$@"; do
    ids=" $(jsonq "$i" "' '.join(str(n['node_id']) for n in s.get('nodes') or [])") "
    for j in "$@"; do [ "$i" = "$j" ] || [[ "$ids" == *" $j "* ]] || return 1; done
  done
}
# F1 ---------------------------------------------------------------------
if wait_for 120 meshed 1 2 3 4 5 36 && wait_for 120 know 1 2 3 4 5; then
  pass "F1 $NODES nodes mesh on ch 36 and know each other in gossip"
else
  fail "F1 setup" "$(where)"; echo "== hwsim findmesh results: $PASS passed, $FAIL failed"; exit 1
fi

# F2 ---------------------------------------------------------------------
stop_node 5
out=$(move 1 44)
if wait_for 60 meshed 1 2 3 4 44 && same_rev 1 2 3 4; then
  pass "F2 node 5 off; node 1 moved the mesh to ch 44: nodes 1-4 switched (revision $(rev 1))"
else
  fail "F2 move with node 5 off" "push: $out; $(where)"
fi

# F4 (F3 watched meanwhile) ----------------------------------------------
start_node 5
looked=""
back() {
  local i w
  for i in 1 2 3 4; do w=$(warned "$i"); [ -n "$w" ] && [[ "$looked" != *" node$i:"* ]] && looked="$looked node$i:$w"; done
  meshed 1 2 3 4 5 44 && same_rev 1 2 3 4 5
}
t0=$SECONDS
if wait_for 300 back; then
  pass "F4 node 5, back on ch 36 alone, found the mesh on ch 44 and joined it on revision $(rev 5) ($((SECONDS - t0)) s)"
else
  fail "F4 node 5 back" "$(where); node 5's warnings: $(warned 5)"
fi
if [ -z "$looked" ]; then
  pass "F3 the larger part never looked for the mesh while node 5 was away"
else
  fail "F3 larger part" "searched:$looked"
fi

# F5 ---------------------------------------------------------------------
wait_for 120 know 1 2 3 4 5 || echo "== note: not every node knows every other before F5: $(where)"
stop_node 4; stop_node 5
out=$(move 1 149)
if ! wait_for 60 meshed 1 2 3 149; then
  fail "F5 move with nodes 4 and 5 off" "push: $out; $(where)"
else
  start_node 4; start_node 5
  t0=$SECONDS
  if wait_for 400 meshed 1 2 3 4 5 149 && same_rev 1 2 3 4 5; then
    pass "F5 nodes 4 and 5, back on ch 44 hearing only each other, found the mesh on ch 149: every node on revision $(rev 1) ($((SECONDS - t0)) s)"
  else
    fail "F5 two nodes back" "$(where); warnings: node4 $(warned 4), node5 $(warned 5)"
  fi
fi

echo "== hwsim findmesh results: $PASS passed, $FAIL failed (logs: $RUN)"
[ "$FAIL" = 0 ]
