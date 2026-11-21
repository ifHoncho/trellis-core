#!/usr/bin/env bash
# Trellis voice lab: 4 nodes in a line (1-2-3-4, so 3 batman
# hops end to end), each running a real meshd and trellis-voiced with file
# audio. Checks talk groups kept apart, two channels at once, busy
# refusal, latency and loss through the network, and the packets' marking.
#
#   lab/voice.sh [--keep] [--verbose]
#
# Needs make build (bin/trellis and bin/trellis-voiced), libopus, and root
# or an unprivileged user namespace with batman_adv loaded, as lab/lab.sh.
set -u
KEEP=0; VERBOSE=0; ARGS=("$@")
while [ $# -gt 0 ]; do
  case "$1" in
    --keep) KEEP=1; shift ;;
    --verbose) VERBOSE=1; shift ;;
    -h|--help) sed -n '2,10p' "$0"; exit 0 ;;
    *) echo "unknown argument $1" >&2; exit 2 ;;
  esac
done
NODES=4
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
BIN="${TRELLIS_BIN:-$ROOT/bin/trellis}"
VOICED="${TRELLIS_VOICED:-$ROOT/bin/trellis-voiced}"
RUN="${LAB_RUN:-$ROOT/lab/run}/voice"
PASS=0; FAIL=0; SKIP=0; RESULTS=()
say()  { printf '%s\n' "$*"; }
dbg()  { [ "$VERBOSE" = 1 ] && printf '    %s\n' "$*" >&2; return 0; }
pass() { PASS=$((PASS+1)); RESULTS+=("PASS $1"); say "  PASS  $1"; }
fail() { FAIL=$((FAIL+1)); RESULTS+=("FAIL $1: $2"); say "  FAIL  $1: $2"; }
skip() { SKIP=$((SKIP+1)); RESULTS+=("SKIP $1: $2"); say "  SKIP  $1: $2"; }

for b in "$BIN" "$VOICED"; do [ -x "$b" ] || { echo "$b not found; run make build" >&2; exit 2; }; done
if ! [ -e /sys/module/batman_adv ]; then
  if [ "$(id -u)" -eq 0 ]; then modprobe batman-adv || exit 2
  else echo "batman_adv is not loaded: sudo modprobe batman-adv" >&2; exit 2; fi
fi
if [ "$(id -u)" -ne 0 ] && [ -z "${TRELLIS_LAB_UNSHARED:-}" ]; then
  export TRELLIS_LAB_UNSHARED=1
  exec unshare -Urmn --propagation private bash -c '
    mount -t tmpfs none /run && mkdir -p /run/netns
    exec "$0" "$@"' "$0" "${ARGS[@]}"
fi
rm -rf "$RUN"; mkdir -p "$RUN"

NS() { ip netns exec "$1" "${@:2}"; }
ctl() { "$BIN" meshctl --socket "$RUN/node$1/run/meshd.sock" "${@:2}"; }
vctl() { "$VOICED" "$2" --control "$RUN/node$1/voice.sock" "${@:3}"; }
jsonq() {
  ctl "$1" status --json 2>/dev/null | python3 -I -c "import json,sys
s=json.load(sys.stdin)
print($2)" 2>/dev/null
}
vq() {
  vctl "$1" status 2>/dev/null | python3 -I -c "import json,sys
s=json.load(sys.stdin)
print($2)" 2>/dev/null
}

cleanup() {
  [ "$KEEP" = 1 ] && { say "== keeping the voice lab up (--keep)"; return; }
  for f in "$RUN"/node*/pid "$RUN"/node*/voice.pid "$RUN"/node*/webd.pid; do [ -f "$f" ] && kill "$(cat "$f")" 2>/dev/null; done
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
name: voice-lab
mesh_id: trellis-voice-lab
sae_passphrase: lab-passphrase-not-secret
regulatory: {mode: country, country: US}
radio: {band: 5g, channel: 149}
prefix: 10.73.0.0/16
batman: {orig_interval_ms: 500, bridge_loop_avoidance: false}
voice:
  groups:
    - {number: 1, name: Command}
    - {number: 2, name: Alpha}
    - {number: 3, name: Bravo}
YAML
# Each node's voice settings (node.yaml): node 1 has group 1 in the left
# ear and group 2 in the right; node 2 is on group 3 only; nodes 3 and 4
# talk on groups 2 and 1.
voice_yaml() {
  case "$1" in
    1) echo '{enable: true, tones: false, channels: {a: {group: 1, ear: left, volume: 100}, b: {group: 2, ear: right, volume: 100}}}' ;;
    2) echo '{enable: true, tones: false, channels: {a: {group: 3, volume: 100}}}' ;;
    3) echo '{enable: true, tones: false, channels: {a: {group: 2, volume: 100}}}' ;;
    4) echo '{enable: true, tones: false, channels: {a: {group: 1, volume: 100}}}' ;;
  esac
}
for i in $(seq 1 $NODES); do
  mkdir -p "$RUN/node$i/state" "$RUN/node$i/run" "$RUN/node$i/pm"
  printf 'mac: "02:00:00:00:01:%02x"\n' "$i" > "$RUN/node$i/state/identity.yaml"
  cat > "$RUN/node$i/node.yaml" <<YAML
schema: 0
callsign: voice-$i
node_id: $i
profile: $RUN/profile.yaml
radios:
  roles: {mesh: disabled, ap: disabled}
ports:
  - {name: eth1, role: mesh}
ap: {enabled: false}
voice: $(voice_yaml "$i")
YAML
  ip netns add "node$i"
  ip link add eth1 netns "node$i" type veth peer name "p$i" netns air
  # meshd raises the mesh port's MTU for batman-adv's header: the air
  # side must carry those frames too.
  NS air ip link set "p$i" mtu 1600 master air0 up
  NS "node$i" ip link set lo up
  NS "node$i" ip link set eth1 up
  ip netns exec "node$i" "$BIN" meshd --node "$RUN/node$i/node.yaml" --state-dir "$RUN/node$i/state" \
      --run-dir "$RUN/node$i/run" --interval 2s --log-level debug --no-host-config \
      --test-port-manager "$RUN/node$i/pm" --voice-socket "$RUN/node$i/voice.sock" >> "$RUN/node$i.log" 2>&1 &
  echo $! > "$RUN/node$i/pid"
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
mac4="02:00:00:00:01:04"
deadline=$((SECONDS + 90)); hops=""
while [ $SECONDS -lt $deadline ]; do
  hops=$(NS node1 batctl meshif bat0 traceroute -n "$mac4" 2>/dev/null | grep -cE '^ *[0-9]+: ')
  conv=$(for i in $(seq 1 $NODES); do jsonq "$i" "s['reconcile']['converged'] and s['batman']['present']"; done | sort -u)
  [ "$hops" = 3 ] && [ "$conv" = True ] && break
  sleep 1
done
if [ "$hops" = 3 ] && [ "$conv" = True ]; then
  pass "V1.0 four nodes in a line; node 1 reaches node 4 in 3 hops (batctl traceroute)"
else
  fail "V1.0 line topology" "hops=$hops converged=[$conv]"; exit 1
fi

# Voice on every node, configured by meshd from the profile and node.yaml.
capture() { case "$1" in 4) echo tone:440 ;; 3) echo tone:1000 ;; *) echo silence ;; esac; }
for i in $(seq 1 $NODES); do
  ip netns exec "node$i" "$VOICED" run --state "$RUN/node$i/run/voice.json" --iface br-mesh \
    --control "$RUN/node$i/voice.sock" --console "$RUN/node$i/console.sock" --capture "$(capture "$i")" --playback "$RUN/node$i/rec.wav" \
    --log-level debug >> "$RUN/node$i-voice.log" 2>&1 &
  echo $! > "$RUN/node$i/voice.pid"
done
deadline=$((SECONDS + 15))
until [ $SECONDS -ge $deadline ] || { [ -S "$RUN/node1/voice.sock" ] && [ -S "$RUN/node4/voice.sock" ] && [ -S "$RUN/node3/voice.sock" ]; }; do sleep 0.3; done
deadline=$((SECONDS + 15))
until [ $SECONDS -ge $deadline ] || vctl 1 status >/dev/null 2>&1; do sleep 0.3; done
if ! vctl 1 status >/dev/null 2>&1; then
  fail "V1 voiced" "not running; see $RUN/node1-voice.log: $(tail -2 "$RUN/node1-voice.log" | tr '\n' ' ')"; exit 1
fi
sleep 5   # IGMP reports, then batman-adv announces each node's listeners

# Marking, seen on the way: the TOS byte at node 1's bat0, and the frame
# priority at the origin's and each relay's mesh port (nft netdev egress
# counters on eth1: batman frames with priority 0:105 = 256+5, the value
# batman-adv sets from DSCP EF).
for i in 2 3 4; do
  NS "node$i" nft -f - 2>/dev/null <<'NFT' || dbg "node$i: netdev egress hook not available"
table netdev vprio {
  chain out {
    type filter hook egress device "eth1" priority 0;
    ether type 0x4305 meta priority 0:105 counter
    ether type 0x4305 counter
  }
}
NFT
done
NS node1 timeout 4 tcpdump -Z root -vnli bat0 'udp and dst host 239.73.80.1' > "$RUN/tos.txt" 2>/dev/null &
tp=$!

vctl 4 ptt a down > "$RUN/ptt4.txt" 2>&1
vctl 3 ptt a down > "$RUN/ptt3.txt" 2>&1
sleep 1
busy=$(vctl 1 ptt a down 2>&1); vctl 1 ptt a up >/dev/null 2>&1
sleep 3
vctl 4 ptt a up >/dev/null; vctl 3 ptt a up >/dev/null
sleep 1.5
wait "$tp" 2>/dev/null

# V1.1/V1.2: what node 1 and node 2 heard.
c1=$(python3 -I "$HERE/voicecheck.py" "$RUN/node1/rec.wav" 440 1000)
c2=$(python3 -I "$HERE/voicecheck.py" "$RUN/node2/rec.wav" 440 1000)
dbg "node1: $c1"; dbg "node2: $c2"
ok=$(printf '%s\n' "$c1" | python3 -I -c "
import sys
d={}
for l in sys.stdin:
    f=l.split(); d[f[0]]={k:float(v) for k,v in (x.split('=') for x in f[1:])}
L,R=d['left'],d['right']
print(L['frames']>=150 and R['frames']>=150 and L['440']>0.95 and R['1000']>0.95)")
if [ "$ok" = True ]; then
  pass "V1.1 two channels at once on node 1: group 1 (node 4's 440 Hz, 3 hops) in the left ear, group 2 (node 3's 1000 Hz, 2 hops) in the right; $(echo "$c1" | tr '\n' ';')"
else
  fail "V1.1 two channels" "node 1 heard: $(echo "$c1" | tr '\n' ';')"
fi
if echo "$c2" | grep -q '^left frames=0 ' && echo "$c2" | grep -q '^right frames=0 '; then
  pass "V1.2 groups kept apart: node 2, on group 3 only, relayed both talks and heard nothing"
else
  fail "V1.2 groups apart" "node 2 heard: $(echo "$c2" | tr '\n' ';')"
fi

# V1.3: busy refusal.
ref=$(vq 1 "sum(1 for e in s['events'] if e['kind']=='ptt-refused' and e['group']==1)")
if echo "$busy" | grep -q '"granted":false' && [ "$ref" = 1 ]; then
  pass "V1.3 PTT on group 1 at node 1 while node 4 talks on it: refused (busy)"
else
  fail "V1.3 busy refusal" "answer: $busy; refusals: $ref"
fi

# V1.4: latency, first frame sent to first frame played, same clock; and loss.
lat=$(for n in 1 3 4; do vctl "$n" status; echo; done | python3 -I -c "
import json, sys
from datetime import datetime
st=[json.loads(l) for l in sys.stdin if l.strip()]
ev=[e for s in st for e in s['events']]
import re
t=lambda e: datetime.fromisoformat(re.sub(r'(\\.\\d{6})\\d+', r'\\1', e['at']).replace('Z','+00:00')).timestamp()
out=[]
for g,frm in ((1,'node 4, 3 hops'),(2,'node 3, 2 hops')):
    sent=[e for e in ev if e['kind']=='talk-sent' and e['group']==g]
    heard=[e for e in ev if e['kind']=='talk-heard' and e['group']==g and e['ssrc']==(sent[0]['ssrc'] if sent else -1)]
    out.append('%d:%s' % (g, '%.0f' % ((t(heard[0])-t(sent[0]))*1000) if sent and heard else 'none'))
g=st[0]['groups']
out.append('rx1=%d lost1=%d late1=%d rx2=%d lost2=%d late2=%d talker1=%s' % (g[0]['received'],g[0]['lost'],g[0]['late'],g[1]['received'],g[1]['lost'],g[1]['late'],
    [e['talker'] for e in st[0]['events'] if e['kind']=='talk-heard' and e['group']==1][:1]))
print(' '.join(out))")
dbg "latency: $lat"
l1=$(echo "$lat" | sed -n 's/.*\b1:\([0-9]*\).*/\1/p'); l2=$(echo "$lat" | sed -n 's/.*\b2:\([0-9]*\).*/\1/p')
lost=$(echo "$lat" | sed -n 's/.*lost1=\([0-9]*\).*lost2=\([0-9]*\).*/\1 \2/p')
if [ -n "$l1" ] && [ -n "$l2" ] && [ "$l1" -lt 150 ] && [ "$l2" -lt 150 ] && [ "$lost" = "0 0" ] && echo "$lat" | grep -q "voice-4"; then
  pass "V1.4 network latency (first frame sent to first played, 60 ms jitter buffer included): group 1 ${l1} ms over 3 hops, group 2 ${l2} ms over 2 hops; no loss; talker named by RTCP; $lat"
else
  fail "V1.4 latency and loss" "$lat"
fi

# V1.5: marking. The TOS byte as node 1 receives it; then each talker
# alone for 2 s, counting at every node's mesh port the batman frames sent
# with priority 261 (256 + 5: batman-adv's mapping of DSCP EF) against all
# batman frames sent.
tos=$(grep -c 'tos 0xb8' "$RUN/tos.txt"); pk=$(grep -c '239.73.80.1' "$RUN/tos.txt")
cnt() { NS "node$1" nft list table netdev vprio 2>/dev/null | sed -n 's/.*packets \([0-9]*\).*/\1/p' | tr '\n' ' '; }
prio=""
for t in 4 3; do
  declare -A b=()
  for i in 2 3 4; do b[$i]=$(cnt "$i"); done
  vctl "$t" ptt a down >/dev/null; sleep 2; vctl "$t" ptt a up >/dev/null; sleep 0.5
  prio="$prio talker node$t ->"
  for i in 4 3 2; do
    read -r p0 a0 <<<"${b[$i]}"; read -r p1 a1 <<<"$(cnt "$i")"
    [ -n "$p0" ] && [ -n "$p1" ] && prio="$prio node$i $((p1 - p0))/$((a1 - a0))"
  done
  prio="$prio;"
done
dbg "priority: $prio"
if [ "$pk" -gt 0 ] && [ "$tos" -eq "$pk" ]; then
  pass "V1.5 voice reaches node 1 marked DSCP EF (tos 0xb8 on $tos/$pk packets). Frames at priority 261 / all batman frames sent, per mesh port, 2 s of talk (~100 voice frames) each:$prio"
else
  fail "V1.5 marking" "tos 0xb8 on $tos of $pk packets; priorities:$prio"
fi

# V1.6: meshd runs voice. It hands voiced its configuration (node 2
# moves its channel A to group 1 by node.yaml and a reload; voiced
# restarts with it) and passes its status on (meshctl voice).
sed -i 's/channels: {a: {group: 3, volume: 100}}/channels: {a: {group: 1, volume: 100}}/' "$RUN/node2/node.yaml"
ctl 2 reload >/dev/null
deadline=$((SECONDS + 15)); g2=""
while [ $SECONDS -lt $deadline ]; do
  g2=$(vq 2 "s['channels'][0]['group']"); [ "$g2" = 1 ] && break; sleep 0.5
done
vctl 4 ptt a down >/dev/null; sleep 1.5
mv=$(ctl 1 voice 2>&1)
vctl 4 ptt a up >/dev/null; sleep 0.5
rx2=$(vq 2 "s['groups'][0]['received']")
dbg "meshctl voice on node 1: $mv"
if [ "$g2" = 1 ] && [ "${rx2:-0}" -gt 30 ] && echo "$mv" | grep -q '^voice: running' && echo "$mv" | grep -qE '^1 +Command .* voice-4 '; then
  pass "V1.6 meshd runs voice: node 2's channel A moved to group 1 by node.yaml and a reload, voiced followed and heard node 4 ($rx2 packets); meshctl voice on node 1 shows voice-4 talking on Command"
else
  fail "V1.6 meshd and voiced" "node 2 channel A group=$g2, received $rx2; meshctl voice: $(echo "$mv" | tr '\n' ';')"
fi

# V1.7: a browser console on node 1, through webd: it monitors
# group 1 while node 4 talks there (3 hops), then keys group 2 with a
# 1 kHz tone, which node 3 (on group 2) hears under the console's name.
CC="$RUN/consoleclient"
go build -o "$CC" "$ROOT/tools/consoleclient" 2>/dev/null || cp "$ROOT/bin/consoleclient" "$CC" 2>/dev/null
echo "lab-console-password" | ctl 1 web password --stdin > /dev/null 2>&1
ip netns exec node1 "$BIN" webd --socket "$RUN/node1/run/meshd.sock" --voice-console "$RUN/node1/console.sock" \
  --listen 127.0.0.1:8480 --plain-http >> "$RUN/webd1.log" 2>&1 &
echo $! > "$RUN/node1/webd.pid"
sleep 1.5
vctl 4 ptt a down >/dev/null
cc=$(ip netns exec node1 "$CC" --url http://127.0.0.1:8480 --password lab-console-password --name lab --monitor 1 \
  --listen 3s --talk 2 --tone 1000 --talk-for 2s --hear-hz 440 2>&1)
vctl 4 ptt a up >/dev/null
sleep 1
kill "$(cat "$RUN/node1/webd.pid")" 2>/dev/null
dbg "console: $cc"
heard3=$(vq 3 "[e['talker'] for e in s['events'] if e['kind']=='talk-heard' and e['group']==2][-1:]")
ok=$(printf '%s' "$cc" | python3 -I -c "
import json,sys
try: o=json.loads(sys.stdin.read())
except Exception: print(False); raise SystemExit
h=o['heard'].get('1',{})
print(h.get('frames',0)>=100 and h.get('share',0)>0.8 and o.get('granted') is True and o.get('console')=='voice-1/lab')")
if [ "$ok" = True ] && [ "$heard3" = "['voice-1/lab']" ]; then
  pass "V1.7 browser console through webd on node 1: heard node 4's 440 Hz on group 1 (3 hops) as 16 kHz audio, keyed group 2, and node 3 heard voice-1/lab; $cc"
else
  fail "V1.7 browser console" "client: $cc; node 3 heard: $heard3; webd log: $(tail -3 "$RUN/webd1.log" | tr '\n' ' ')"
fi

# V1.8: an all-call from the console: every group at once, taking over
# group 1 while node 4 talks there; node 2 (moved to group 1 in V1.6) and
# node 3 (group 2) hear the console.
ip netns exec node1 "$BIN" webd --socket "$RUN/node1/run/meshd.sock" --voice-console "$RUN/node1/console.sock" \
  --listen 127.0.0.1:8480 --plain-http >> "$RUN/webd1.log" 2>&1 &
echo $! > "$RUN/node1/webd.pid"
sleep 1.5
vctl 4 ptt a down >/dev/null
sleep 1.5
ip netns exec node1 "$CC" --url http://127.0.0.1:8480 --password lab-console-password --name cmd \
  --listen 0.5s --all --tone 700 --talk-for 2s > "$RUN/allcall.txt" 2>&1 &
acp=$!
sleep 1.8
mid2=$(vq 2 "s['groups'][0]")
wait "$acp"
ac=$(cat "$RUN/allcall.txt")
sleep 0.5
vctl 4 ptt a up >/dev/null
sleep 1
kill "$(cat "$RUN/node1/webd.pid")" 2>/dev/null
last() { vq "$1" "[e.get('talker','?') for e in s['events'] if e['kind']=='talk-heard' and e['group']==$2][-$3:]"; }
h2=$(last 2 1 3); h3=$(last 3 2 1)
took=$(printf '%s' "$mid2" | python3 -I -c "import ast,sys; g=ast.literal_eval(sys.stdin.read()); print(g.get('talker')=='voice-1/cmd' and g.get('all_call') is True)" 2>/dev/null)
dbg "all-call: $ac; node 2 on group 1: $h2; node 3 on group 2: $h3"
if echo "$ac" | grep -q '"granted":true' && [ "$took" = True ] && [ "$h2" = "['voice-4', 'voice-1/cmd', 'voice-4']" ] && [ "$h3" = "['voice-1/cmd']" ]; then
  pass "V1.8 all-call from the console: granted on every group; node 2 was hearing node 4 on group 1, switched to the all-call (shown as one), and back to node 4 after it; node 3 heard it on group 2"
else
  fail "V1.8 all-call" "client: $ac; node 2 group 1 last heard: $h2; node 3 group 2: $h3; node 2 mid-call: $mid2; node 2 log: $(grep -i 'all-call\|talk-heard\|rtcp' "$RUN/node2-voice.log" | tail -4 | tr '\n' ' ')"
fi

# V1.9: meshd restarts on node 3 (a deploy does that): br-mesh comes back
# with a new index; voiced must notice and start again on it, so node 3
# talks on group 2 and node 1 hears it, with no hand on voiced.
idx0=$(NS node3 cat /sys/class/net/br-mesh/ifindex 2>/dev/null)
kill "$(cat "$RUN/node3/pid")" 2>/dev/null
sleep 3
ip netns exec node3 "$BIN" meshd --node "$RUN/node3/node.yaml" --state-dir "$RUN/node3/state" \
    --run-dir "$RUN/node3/run" --interval 2s --log-level debug --no-host-config \
    --test-port-manager "$RUN/node3/pm" --voice-socket "$RUN/node3/voice.sock" >> "$RUN/node3.log" 2>&1 &
echo $! > "$RUN/node3/pid"
deadline=$((SECONDS + 40)); idx1=""
while [ $SECONDS -lt $deadline ]; do
  idx1=$(NS node3 cat /sys/class/net/br-mesh/ifindex 2>/dev/null)
  [ -n "$idx1" ] && grep -q "mesh interface changed" "$RUN/node3-voice.log" && vctl 3 status >/dev/null 2>&1 && break
  sleep 1
done
sleep 6   # the mesh settles, the multicast listener is announced again
rx0=$(vq 1 "s['groups'][1]['received']")
vctl 3 ptt a down >/dev/null; sleep 2; vctl 3 ptt a up >/dev/null; sleep 1
rx1=$(vq 1 "s['groups'][1]['received']")
if [ -n "$idx0" ] && [ "$idx1" != "$idx0" ] && grep -q "mesh interface changed" "$RUN/node3-voice.log" && [ $((rx1 - rx0)) -gt 50 ]; then
  pass "V1.9 node 3's meshd restarted (br-mesh $idx0 -> $idx1): voiced started again on the new interface by itself, and node 1 heard node 3's next talk ($((rx1 - rx0)) packets)"
else
  fail "V1.9 voice after a meshd restart" "br-mesh $idx0 -> $idx1; voiced noticed: $(grep -c 'mesh interface changed' "$RUN/node3-voice.log"); node 1 received $rx0 -> $rx1"
fi

say ""
say "== results: $PASS passed, $FAIL failed, $SKIP skipped"
for r in "${RESULTS[@]}"; do case "$r" in SKIP*|FAIL*) say "   $r";; esac; done
[ "$FAIL" = 0 ]
