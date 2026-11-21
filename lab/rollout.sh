#!/usr/bin/env bash
# Trellis rollout lab: 4 nodes in a line (1-2-3-4), each running a real
# meshd with the lab's stand-in for the upgrade (meshd --test-upgrader).
# Node 4 is given signed packages and sends them through the mesh. Checks
# the plan (order by distance, a node that does not take rollouts left
# out), a good release reaching every node one at a time with the packages
# fetched over the mesh, a node that missed the rollout asking for it, a
# node restarted while it waits, a release that rolls back halting the
# rest, and cancel.
#
#   lab/rollout.sh [--keep] [--verbose]
#
# Needs make build, and root or an unprivileged user namespace with
# batman_adv loaded, as lab/lab.sh.
set -u
KEEP=0; VERBOSE=0; ARGS=("$@")
while [ $# -gt 0 ]; do
  case "$1" in
    --keep) KEEP=1; shift ;;
    --verbose) VERBOSE=1; shift ;;
    -h|--help) sed -n '2,14p' "$0"; exit 0 ;;
    *) echo "unknown argument $1" >&2; exit 2 ;;
  esac
done
NODES=4
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
BIN="${TRELLIS_BIN:-$ROOT/bin/trellis}"
RUN="${LAB_RUN:-$ROOT/lab/run}/rollout"
PASS=0; FAIL=0; RESULTS=()
say()  { printf '%s\n' "$*"; }
dbg()  { [ "$VERBOSE" = 1 ] && printf '    %s\n' "$*" >&2; return 0; }
pass() { PASS=$((PASS+1)); RESULTS+=("PASS $1"); say "  PASS  $1"; }
fail() { FAIL=$((FAIL+1)); RESULTS+=("FAIL $1: $2"); say "  FAIL  $1: $2"; }

[ -x "$BIN" ] || { echo "$BIN not found; run make build" >&2; exit 2; }
if ! [ -e /sys/module/batman_adv ]; then
  if [ "$(id -u)" -eq 0 ]; then modprobe batman-adv || exit 2
  else echo "batman_adv is not loaded: sudo modprobe batman-adv" >&2; exit 2; fi
fi
# The packages, built and signed before entering the namespace (go needs
# the user's caches).
if [ -z "${TRELLIS_LAB_UNSHARED:-}" ]; then
  rm -rf "$RUN"; mkdir -p "$RUN/pkg"
  ( cd "$ROOT" && go build -o "$RUN/mkdeb" ./tools/mkdeb ) || exit 2
  "$RUN/mkdeb" keygen --out "$RUN/lab.key" --comment "rollout lab" > "$RUN/lab.pub" 2>/dev/null || exit 2
  for v in 9.0.1 9.0.2 9.0.3 9.0.4; do
    "$RUN/mkdeb" trellis --root "$ROOT" --out "$RUN/pkg" --key "$RUN/lab.key" --sign-dev \
      --version "$v" --arch amd64 --binary "$BIN" > /dev/null || exit 2
  done
  "$RUN/mkdeb" trellis --root "$ROOT" --out "$RUN/pkg/unsigned" --version 9.0.1 --arch amd64 --binary "$BIN" > /dev/null || exit 2
fi
if [ "$(id -u)" -ne 0 ] && [ -z "${TRELLIS_LAB_UNSHARED:-}" ]; then
  export TRELLIS_LAB_UNSHARED=1
  exec unshare -Urmn --propagation private bash -c '
    mount -t tmpfs none /run && mkdir -p /run/netns
    exec "$0" "$@"' "$0" "${ARGS[@]}"
fi
export TRELLIS_LAB_UNSHARED=1

NS() { ip netns exec "$1" "${@:2}"; }
ctl() { "$BIN" meshctl --socket "$RUN/node$1/run/meshd.sock" "${@:2}"; }
jsonq() {
  ctl "$1" status --json 2>/dev/null | python3 -I -c "import json,sys
s=json.load(sys.stdin)
print($2)" 2>/dev/null
}
# The rollout as node $1 sees it: "callsign:phase ..." in order.
phases() { jsonq "$1" "' '.join(n['callsign']+':'+(n.get('phase') or '-') for n in (s.get('rollout') or {}).get('nodes',[]))"; }
ended()  { [ "$(jsonq "$1" "bool((s.get('rollout') or {}).get('ended'))")" = True ]; }
pkg() { echo "$RUN/pkg/trellis_$1_amd64.deb"; }

cleanup() {
  [ "$KEEP" = 1 ] && { say "== keeping the rollout lab up (--keep)"; return; }
  for f in "$RUN"/node*/pid; do [ -f "$f" ] && kill "$(cat "$f")" 2>/dev/null; done
  sleep 1
  for i in $(seq 1 $NODES); do ip netns del "node$i" 2>/dev/null; done
  ip netns del air 2>/dev/null
}
trap cleanup EXIT

# ---------------------------------------------------------------- topology
say "== building $NODES nodes in a line"
ip netns add air || { echo "cannot create namespaces" >&2; exit 2; }
NS air ip link add air0 type bridge
NS air ip link set air0 up
NS air sh -c 'echo 0 > /sys/class/net/air0/bridge/multicast_snooping' 2>/dev/null
cat > "$RUN/profile.yaml" <<YAML
schema: 0
name: rollout-lab
mesh_id: trellis-rollout-lab
sae_passphrase: lab-passphrase-not-secret
regulatory: {mode: country, country: US}
radio: {band: 5g, channel: 149}
prefix: 10.73.0.0/16
batman: {orig_interval_ms: 500, bridge_loop_avoidance: false}
YAML
start_meshd() {
  ip netns exec "node$1" "$BIN" meshd --node "$RUN/node$1/node.yaml" --state-dir "$RUN/node$1/state" \
      --run-dir "$RUN/node$1/run" --interval 2s --log-level debug --no-host-config \
      --test-port-manager "$RUN/node$1/pm" --test-upgrader "$RUN/node$1/upg" >> "$RUN/node$1.log" 2>&1 &
  echo $! > "$RUN/node$1/pid"
}
for i in $(seq 1 $NODES); do
  mkdir -p "$RUN/node$i/state" "$RUN/node$i/run" "$RUN/node$i/pm" "$RUN/node$i/upg/keys"
  cp "$RUN/lab.pub" "$RUN/node$i/upg/keys/lab.pub"
  echo 9.0.0 > "$RUN/node$i/upg/installed"
  echo 45 > "$RUN/node$i/upg/stall"   # a node's turn may go unmoved 45 s (15 min on a real mesh)
  printf 'mac: "02:00:00:00:02:%02x"\n' "$i" > "$RUN/node$i/state/identity.yaml"
  cat > "$RUN/node$i/node.yaml" <<YAML
schema: 0
callsign: lab-$i
node_id: $i
profile: $RUN/profile.yaml
radios:
  roles: {mesh: disabled, ap: disabled}
ports:
  - {name: eth1, role: mesh}
ap: {enabled: false}
YAML
  ip netns add "node$i"
  ip link add eth1 netns "node$i" type veth peer name "p$i" netns air
  # meshd raises the mesh port's MTU for batman-adv's header; the air
  # side must carry those frames too, or full-size packets vanish.
  NS air ip link set "p$i" mtu 1600
  NS air ip link set "p$i" master air0 up
  NS "node$i" ip link set lo up
  NS "node$i" ip link set eth1 up
  start_meshd "$i"
done
# Only neighbours in the line hear each other.
{
  echo 'table bridge line {'
  echo ' chain cut {'
  echo '  type filter hook forward priority 0; policy accept;'
  for a in $(seq 1 $NODES); do for b in $(seq 1 $NODES); do
    d=$((a > b ? a - b : b - a)); [ "$d" -gt 1 ] && echo "iifname \"p$a\" oifname \"p$b\" drop"
  done; done
  echo ' }'
  echo '}'
} | NS air nft -f -

# ---------------------------------------------------------------- tests
say "== tests"
deadline=$((SECONDS + 90)); n=""
while [ $SECONDS -lt $deadline ]; do
  n=$(jsonq 4 "len(s['nodes'])")
  conv=$(for i in $(seq 1 $NODES); do jsonq "$i" "s['reconcile']['converged'] and s['batman']['present']"; done | sort -u)
  [ "$n" = 3 ] && [ "$conv" = True ] && break
  sleep 1
done
if [ "$n" = 3 ] && [ "$conv" = True ]; then
  pass "R0 four nodes in a line; node 4 knows the other three"
else
  fail "R0 line" "nodes=$n converged=[$conv]"; exit 1
fi
# The topology (hops) needs every node's link report: let a round pass.
sleep 2

# R1: the plan. Node 3 does not take rollouts for now.
echo "not installed from its package (lab)" > "$RUN/node3/upg/refuse"
sleep 4
out=$(ctl 4 upgrade --mesh "$(pkg 9.0.1)" 2>&1)
dbg "$out"
order=$(echo "$out" | awk '$1 ~ /^[0-9]+$/ {printf "%s:%s ", $1, $2}')
skip3=$(echo "$out" | grep -c 'lab-3 .*skipped: does not take')
started=$(jsonq 4 "s.get('rollout')")
if [ "$order" = "1:lab-1 2:lab-2 3:lab-4 " ] && [ "$skip3" = 1 ] && [ "$started" = None ]; then
  pass "R1.1 plan: farthest first, this node last; a node that does not take rollouts left out with why; nothing started"
else
  fail "R1.1 plan" "order=[$order] skip3=$skip3 started=[$started]: $(echo "$out" | tr '\n' '|')"
fi
out=$(ctl 4 upgrade --mesh "$RUN/pkg/unsigned/trellis_9.0.1_amd64.deb" 2>&1)
if echo "$out" | grep -q 'not signed\|no signature\|signed'; then
  pass "R1.2 an unsigned package is refused at the origin"
else
  fail "R1.2 unsigned" "$out"
fi
rm -f "$RUN/node3/upg/refuse"
sleep 4

# R2: a good release. Node 1 takes 10 s (node 2 is restarted while it
# waits); node 1 drops the origin's rollout messages, so it only gets it
# by asking another node.
echo 10 > "$RUN/node1/upg/delay"
NS node1 nft -f - <<'NFT'
table inet labdrop {
  chain in {
    type filter hook input priority 0; policy accept;
    ip saddr 10.73.4.1 udp dport 7373 @th,64,32 0x54525531 counter drop
  }
}
NFT
out=$(ctl 4 upgrade --mesh --yes "$(pkg 9.0.1)" 2>&1)
dbg "$out"
order=$(echo "$out" | awk '$1 ~ /^[0-9]+$/ {printf "%s:%s ", $1, $2}')
[ "$order" = "1:lab-1 2:lab-2 3:lab-3 4:lab-4 " ] || fail "R2.0 order" "[$order]"
if [ "$VERBOSE" = 1 ]; then
  sleep 3
  sha=$(sha256sum "$(pkg 9.0.1)" | cut -d' ' -f1)
  dbg "node 2 -> origin: $(NS node2 curl -m 10 -sS -o /dev/null -w '%{http_code} %{size_download}' "http://10.73.4.1:7373/release/$sha" 2>&1)"
  dbg "node 2 -> origin ping: $(NS node2 ping -c1 -W2 -s 1400 10.73.4.1 2>&1 | grep -o '[0-9]* received')"
fi
restarted=0
deadline=$((SECONDS + 180))
while [ $SECONDS -lt $deadline ]; do
  p=$(phases 4); dbg "$p"
  if [ "$restarted" = 0 ] && echo "$p" | grep -q 'lab-1:upgrading' && echo "$p" | grep -q 'lab-2:ready'; then
    kill "$(cat "$RUN/node2/pid")"; sleep 1; start_meshd 2; restarted=1
    dbg "restarted node 2's meshd"
  fi
  ended 4 && break
  sleep 1
done
p=$(phases 4)
if [ "$p" = "lab-1:done lab-2:done lab-3:done lab-4:done" ]; then
  pass "R2.1 a good release reaches every node ($p)"
else
  fail "R2.1 good release" "[$p]; see $RUN/node*.log"
fi
inst=$(cat "$RUN"/node{1,2,3,4}/upg/installed | sort -u | tr '\n' ' ')
[ "$inst" = "9.0.1 " ] && pass "R2.2 every node runs 9.0.1" || fail "R2.2 installed" "[$inst]"
# One at a time, in order: each began after the one before it finished.
okseq=$(python3 -I - "$RUN" <<'PY'
import json, sys, datetime
run = sys.argv[1]
def t(s): return datetime.datetime.fromisoformat(s.strip().replace('Z', '+00:00'))
prev = None
for i in (1, 2, 3, 4):
    start = t(open(f"{run}/node{i}/upg/started").read())
    end = t(json.load(open(f"{run}/node{i}/upg/last.json"))["finished"])
    if prev and start < prev:
        print(f"node {i} began before node {i-1} finished"); sys.exit()
    prev = end
print("ok")
PY
)
[ "$okseq" = ok ] && pass "R2.3 one node at a time, in the order" || fail "R2.3 sequence" "$okseq"
sha=$(sha256sum "$(pkg 9.0.1)" | cut -d' ' -f1)
fetched=0; for i in 1 2 3; do [ -f "$RUN/node$i/state/upgrade/releases/$sha.deb" ] && fetched=$((fetched+1)); done
[ "$fetched" = 3 ] && pass "R2.4 nodes 1-3 fetched the package over the mesh" || fail "R2.4 fetched" "$fetched of 3"
asked=$(grep -c 'does not know; asking it' "$RUN/node1.log")
drops=$(NS node1 nft list table inet labdrop 2>/dev/null | grep -o 'packets [0-9]*' | head -1)
if [ "$asked" -ge 1 ] && [ "${drops#packets }" -ge 1 ]; then
  pass "R2.5 node 1, cut off from the origin's messages ($drops dropped), asked another node for the rollout"
else
  fail "R2.5 catch-up" "asked=$asked drops=[$drops]"
fi
if [ "$restarted" = 1 ] && grep -q 'kept from before the restart' "$RUN/node2.log"; then
  pass "R2.6 node 2, restarted while it waited, picked its part up from disk"
else
  fail "R2.6 restart" "restarted=$restarted"
fi
NS node1 nft delete table inet labdrop 2>/dev/null
rm -f "$RUN/node1/upg/delay"

# R3: a release that rolls back on node 2 halts nodes 3 and 4.
echo rolled-back > "$RUN/node2/upg/outcome"
ctl 4 upgrade --mesh --yes "$(pkg 9.0.2)" > "$RUN/r3.txt" 2>&1
deadline=$((SECONDS + 120))
until [ $SECONDS -ge $deadline ] || ended 4; do sleep 1; done
p=$(phases 4)
inst=$(cat "$RUN"/node{1,2,3,4}/upg/installed | tr '\n' ' ')
if [ "$p" = "lab-1:done lab-2:rolled-back lab-3:halted lab-4:halted" ] && [ "$inst" = "9.0.2 9.0.1 9.0.1 9.0.1 " ]; then
  pass "R3 a release that rolls back on node 2 halts the rest ($p)"
else
  fail "R3 rollback halts" "[$p] installed [$inst]"
fi
why=$(jsonq 3 "[n.get('reason','') for n in s['rollout']['nodes'] if n['callsign']=='lab-3'][0]")
dbg "node 3 halted: $why"
rm -f "$RUN/node2/upg/outcome"

# R4: cancel while node 1 upgrades: it finishes; the rest stop.
echo 12 > "$RUN/node1/upg/delay"
ctl 4 upgrade --mesh --yes "$(pkg 9.0.3)" > "$RUN/r4.txt" 2>&1
deadline=$((SECONDS + 60))
until [ $SECONDS -ge $deadline ] || phases 4 | grep -q 'lab-1:upgrading'; do sleep 0.5; done
c=$(ctl 4 upgrade --mesh --cancel 2>&1)
deadline=$((SECONDS + 90))
until [ $SECONDS -ge $deadline ] || ended 4; do sleep 1; done
p=$(phases 4)
if [ "$p" = "lab-1:done lab-2:halted lab-3:halted lab-4:halted" ]; then
  pass "R4 cancel: the node upgrading finishes, the rest stop ($p)"
else
  fail "R4 cancel" "[$p] ($c)"
fi
# Every node says the same at the end.
same=$(for i in 1 2 3 4; do phases "$i"; done | sort -u | wc -l)
[ "$same" = 1 ] && pass "R4.1 every node sees the same outcome" || fail "R4.1 views" "$(for i in 1 2 3 4; do phases "$i"; echo; done)"

# R6: node 1 (first in the order) is cut off just as the rollout starts.
# The others halt once its turn has gone unmoved for the stall time, the
# rollout ends after another, and node 1, back, halts instead of upgrading
# by itself.
# Cut both ways: nothing to or from node 1 crosses the air, and its port
# goes down.
NS air nft -f - <<'NFT'
table bridge r6 {
  chain cut {
    type filter hook forward priority -10; policy accept;
    iifname "p1" drop
    oifname "p1" drop
  }
}
NFT
NS air ip link set p1 down
cut1=$(NS node1 ip -br link show eth1 | awk '{print $2}')
ctl 4 upgrade --mesh --yes "$(pkg 9.0.4)" > "$RUN/r6.txt" 2>&1
in1=$(grep -c 'lab-1 ' "$RUN/r6.txt")
t0=$SECONDS
deadline=$((SECONDS + 240))
until [ $SECONDS -ge $deadline ] || ended 4; do sleep 2; done
took=$((SECONDS - t0))
# Over on the origin can come a moment before the last of the rest halts:
# each counts the stall from when it got the rollout, by its own clock.
for _ in $(seq 1 15); do
  p=$(phases 4)
  [ "$p" = "lab-1:- lab-2:halted lab-3:halted lab-4:halted" ] && break; sleep 1
done
follow=$(timeout 20 "$BIN" meshctl --socket "$RUN/node4/run/meshd.sock" upgrade --mesh --follow 2>&1); frc=$?
if [ "$in1" -ge 1 ] && ended 4 && [ "$p" = "lab-1:- lab-2:halted lab-3:halted lab-4:halted" ] && [ "$frc" -ne 0 ] && echo "$follow" | grep -q 'halted: lab-1 did not move'; then
  pass "R6.1 a node silent on its turn halts the rest, and the rollout ends (${took} s; follow returns: $(echo "$follow" | tail -1))"
else
  fail "R6.1 rollout with a silent node ends" "in order: $in1; node 1's eth1 when cut: $cut1; ended after ${took} s: $(ended 4 && echo yes || echo no); [$p]; follow ($frc): $(echo "$follow" | tail -2 | tr '\n' '|')"
fi
NS air ip link set p1 up
NS air nft delete table bridge r6
deadline=$((SECONDS + 120))
until [ $SECONDS -ge $deadline ] || [ "$(jsonq 1 "(s.get('rollout') or {}).get('phase','')")" = halted ]; do sleep 2; done
ph1=$(jsonq 1 "(s.get('rollout') or {}).get('phase','')")
why1=$(jsonq 1 "(s.get('rollout') or {}).get('reason','')")
inst1=$(cat "$RUN/node1/upg/installed")
if [ "$ph1" = halted ] && [ "$inst1" = 9.0.3 ] && echo "$why1" | grep -q 'lab-1 did not move'; then
  pass "R6.2 node 1, back, takes the rollout and halts ($why1) instead of upgrading; it still runs 9.0.3"
else
  fail "R6.2 node back after a halt" "phase=$ph1 why=[$why1] installed=$inst1; node 1's eth1 when cut: $cut1; node 1's log: $(grep -n 'mesh-wide upgrade' "$RUN/node1.log" | tail -6 | tr '\n' '|')"
fi

# ---------------------------------------------------------------- summary
say ""
say "== $PASS passed, $FAIL failed"
for r in "${RESULTS[@]}"; do case "$r" in FAIL*) say "  $r" ;; esac; done
[ "$FAIL" -eq 0 ]
