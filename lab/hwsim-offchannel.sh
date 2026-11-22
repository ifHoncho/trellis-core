#!/usr/bin/env bash
# The off-channel guard over virtual radios: a mesh radio follows any
# peer's channel switch announcement, in the kernel, so one announced
# outside a mesh-wide change (a peer with a stuck driver does this)
# pulls the nodes that hear it off the profile's channel; meshd puts
# them back (meshwatch.go). Root only, in a VM (lab/hwsim-lib.sh).
#
#   sudo lab/hwsim-offchannel.sh
#
# G1  setup: 3 nodes form an 802.11s + SAE mesh on 5 GHz ch 36
# G2  a switch announced with iw on node 1: nodes 1 and 2 follow it, and
#     every node is back on the profile's channel within a minute
set -u
NODES=3
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
BIN="${TRELLIS_BIN:-$ROOT/bin/trellis}"
RUN="${LAB_RUN:-$ROOT/lab/run}/hwsim-offchannel"
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

start_node() { # i [extra meshd flags]
  bg_in "node$1" "$BIN" meshd "${@:2}" --node "$RUN/node$1/node.yaml" --state-dir "$RUN/node$1/state" \
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

chan() { jsonq "$1" "s['mesh_link'].get('channel')"; }
origs() { jsonq "$1" "sum(1 for o in s['batman']['originators'] if o['best'])"; }
where() { for i in $(seq 1 "$NODES"); do printf 'node%s=ch%s/%s ' "$i" "$(chan "$i")" "$(origs "$i")"; done; }

# meshed NODE... CHANNEL: each listed node is joined on CHANNEL and has at
# least as many originators as there are other listed nodes.
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

# G1 ---------------------------------------------------------------------
if wait_for 120 meshed 1 2 3 36; then
  pass "G1 $NODES nodes mesh on ch 36"
else
  fail "G1 setup" "$(where)"; echo "== hwsim off-channel results: $PASS passed, $FAIL failed"; exit 1
fi

# G2 ---------------------------------------------------------------------
if meshed 1 2 3 36; then
  NS node1 iw dev mesh0 switch freq 5220 HT20 beacons 10 2>&1 | head -1
  t0=$SECONDS; moved=no
  for _ in $(seq 1 20); do
    [ "$(NS node2 iw dev mesh0 info | sed -n 's/.*channel \([0-9]*\) .*/\1/p')" = 44 ] && { moved=yes; break; }
    sleep 0.5
  done
  back=""
  for _ in $(seq 1 45); do
    c1=$(NS node1 iw dev mesh0 info | sed -n 's/.*channel \([0-9]*\) .*/\1/p')
    c2=$(NS node2 iw dev mesh0 info | sed -n 's/.*channel \([0-9]*\) .*/\1/p')
    c3=$(NS node3 iw dev mesh0 info | sed -n 's/.*channel \([0-9]*\) .*/\1/p')
    if [ "$moved" = yes ] && [ "$c1 $c2 $c3" = "36 36 36" ] && meshed 1 2 3 36; then back=$((SECONDS - t0)); break; fi
    sleep 2
  done
  if [ -n "$back" ] && grep -q "moved off its channel by a channel switch" "$RUN/node2.log"; then
    pass "G2 a switch announced outside a profile change pulled nodes 1 and 2 to ch 44; every node was back on ch 36 after ${back} s"
  else
    fail "G2 back on the profile's channel" "moved=$moved channels now: $c1 $c2 $c3; $(where)"
  fi
else
  fail "G2 setup" "not all on ch 36: $(where)"
fi

echo "== hwsim off-channel results: $PASS passed, $FAIL failed (logs: $RUN)"
[ "$FAIL" = 0 ]
