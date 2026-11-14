#!/usr/bin/env bash
# Radio roles over virtual radios: mac80211_hwsim radios created and removed
# on demand (lab/hwsimctl.py) in one node's namespace, with meshd assigning
# roles (architecture §5.2). Root
# only.
#
#   sudo lab/hwsim-roles.sh
#
# Every radio has a fixed, globally administered MAC, so its fingerprint is
# `mac:...` whatever order it is created in. The mesh band is 2.4 GHz and
# the client AP is enabled (5 GHz).
#
#   O1  three identical radios, created in three different orders under new
#       wiphy names, one meshd restart each: every fingerprint gets the same
#       role every time.
#   O2  removing the mesh radio moves the mesh to another radio; the AP keeps
#       a radio of its own while one is free.
#   O3  removing the new mesh radio leaves only the AP's radio: the mesh takes
#       it from the AP, which now shares it (mesh+ap, ap-shares-mesh-radio).
#   O4  a radio added while the mesh is up, ranked above the mesh radio
#       (max_power_dbm in node.yaml), takes the AP role, which was only
#       shared, and never the mesh; meshd still recommends it for the mesh.
#   O5  a lone radio that can mesh but has no AP interface type: role mesh,
#       warning no-ap-radio.
#   O6  adding an AP-capable radio to O5 fills the empty AP role; the mesh
#       stays where it is.
#   O7  a radio with a locally administered ("random") MAC falls back to
#       another fingerprint source, and it and a normal radio keep their
#       roles across two meshd restarts.
#
# Not covered: a radio that supports mesh and AP but not both at once.
# hwsim's interface combinations always allow both; the planner unit test
# TestMeshLossTakesAPRadio (internal/radio) covers that case.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
BIN="${TRELLIS_BIN:-$ROOT/bin/trellis}"
RUN="${LAB_RUN:-$ROOT/lab/run}/hwsim-roles"
NSN=roles
PASS=0; FAIL=0
pass() { PASS=$((PASS+1)); echo "  PASS  $1"; }
fail() { FAIL=$((FAIL+1)); echo "  FAIL  $1: $2"; }
[ "$(id -u)" -eq 0 ] || { echo "root required" >&2; exit 2; }
[ -x "$BIN" ] || { echo "binary $BIN missing; run make build" >&2; exit 2; }
for t in wpa_supplicant iw python3; do command -v "$t" >/dev/null || { echo "$t missing" >&2; exit 2; }; done
command -v hostapd >/dev/null || echo "== note: hostapd is not installed; roles are still assigned, but meshd warns that it can't start the AP"

. "$HERE/hwsim-lib.sh"
hwsim_preflight "$NSN"
modprobe -r mac80211_hwsim 2>/dev/null
modprobe mac80211_hwsim radios=0 || { echo "cannot load mac80211_hwsim" >&2; exit 2; }
modprobe batman-adv || exit 2
# lab/run is shared with the unprivileged lab: if this run creates it, make
# it the sudo user's, not root's.
if [ ! -d "$(dirname "$RUN")" ]; then
  mkdir -p "$(dirname "$RUN")"
  [ -n "${SUDO_UID:-}" ] && chown "$SUDO_UID:${SUDO_GID:-$SUDO_UID}" "$(dirname "$RUN")"
fi
rm -rf "$RUN"; mkdir -p "$RUN/state" "$RUN/run"

NS() { ip netns exec "$NSN" "${@:2}"; }
radio() { ip netns exec "$NSN" python3 -I "$HERE/hwsimctl.py" "$@" >> "$RUN/hwsimctl.log" 2>&1 || { echo "hwsimctl $*: $(tail -1 "$RUN/hwsimctl.log")" >&2; return 1; }; }
ctl() { "$BIN" meshctl --socket "$RUN/run/meshd.sock" "$@"; }
jsonq() { ctl status --json 2>/dev/null | python3 -I -c "import json,sys
s=json.load(sys.stdin)
print($1)" 2>/dev/null; }

cleanup() { hwsim_cleanup "$NSN"; }
trap cleanup EXIT

# Radios: letter → MAC. D gets a power override that ranks it first.
declare -A MAC=([A]=00:c0:ca:7e:00:01 [B]=00:c0:ca:7e:00:02 [C]=00:c0:ca:7e:00:03
                [D]=00:c0:ca:7e:00:04 [E]=00:c0:ca:7e:00:05 [F]=00:c0:ca:7e:00:06
                [G]=02:5e:7e:00:00:07 [H]=00:c0:ca:7e:00:08)
fp() { echo "mac:${MAC[$1]}"; }
declare -A NAME   # letter → current wiphy name

cat > "$RUN/profile.yaml" <<YAML
schema: 0
name: hwsim-roles
mesh_id: trellis-hwsim-roles
sae_passphrase: roles-passphrase-1234
regulatory: {mode: country, country: US}
radio: {band: 2g, channel: 1}
batman: {orig_interval_ms: 500}
YAML
cat > "$RUN/node.yaml" <<YAML
schema: 0
callsign: hwsim-roles
node_id: 1
profile: $RUN/profile.yaml
radios:
  devices:
    - match: {fingerprint: "$(fp D)"}
      label: strongest
      max_power_dbm: 40
ap: {enabled: true, band: 5g}
YAML

ip netns add "$NSN"
NS "$NSN" ip link set lo up

start_meshd() {
  bg_in "$NSN" "$BIN" meshd --node "$RUN/node.yaml" --state-dir "$RUN/state" \
      --run-dir "$RUN/run" --interval 2s --log-level debug --no-host-config >> "$RUN/meshd.log" 2>&1
  echo $! > "$RUN/pid"
}
stop_meshd() {
  [ -f "$RUN/pid" ] || return 0
  stop_pid "$(cat "$RUN/pid")"; rm -f "$RUN/pid"
}
add() { # letter [wiphy name] [--no-ap]
  local l="$1" n="${2:-r$1}"; shift 2 2>/dev/null || shift
  radio new "$n" --addr "${MAC[$l]}" "$@" && NAME[$l]="$n"
}
del() { radio del "${NAME[$1]}" && unset "NAME[$1]"; }

roles() { jsonq "' '.join(sorted(r['fingerprint'][-2:]+'='+r['role'] for r in s['radios']))"; }   # e.g. "01=mesh 02=ap 03=unused"
role_of() { jsonq "next((r['role'] for r in s['radios'] if r['fingerprint']=='$(fp "$1")'), 'absent')"; }
rec_of() { jsonq "next((r['recommended'] for r in s['radios'] if r['fingerprint']=='$(fp "$1")'), 'absent')"; }
holder() { jsonq "next((r['fingerprint'] for r in s['radios'] if r['role'] in ('$1',)), 'none')"; }
warns() { jsonq "' '.join(sorted(set(w['code'] for w in s['warnings'])))"; }
phys() { jsonq "' '.join(sorted(r['phy'] for r in s['radios']))"; }
letter_of() { local f="$1" l; for l in "${!MAC[@]}"; do [ "$(fp "$l")" = "$f" ] && { echo "$l"; return; }; done; echo "?"; }

# wait_until TIMEOUT EXPR: evaluate the shell test EXPR until it succeeds.
wait_until() {
  local deadline=$((SECONDS + $1)) expr="$2"
  while [ $SECONDS -lt $deadline ]; do
    eval "$expr" && return 0
    sleep 1
  done
  return 1
}
nradios() { jsonq "len(s['radios'])"; }
converged() { [ "$(jsonq "s['reconcile']['converged']")" = True ]; }
# Settled: roles assigned and meshd idle. Radios are only removed from a
# settled meshd: removing one while meshd is still creating its interfaces
# trips a kernel warning (wiphy_unregister with interfaces left).
settled3() { [ "$(nradios)" = 3 ] && [ "$(holder mesh)" != none ] && [ "$(holder ap)" != none ] && converged; }

# O1 -------------------------------------------------------------------------
declare -a seen
round=0
for order in "A B C" "C A B" "B C A"; do
  round=$((round+1))
  for l in $order; do add "$l" "o${round}${l,,}" || break; done
  start_meshd
  if wait_until 40 settled3; then
    seen+=("$(roles)|$(phys)")
  else
    seen+=("unsettled: $(roles)|$(phys)")
  fi
  stop_meshd
  for l in A B C; do del "$l"; done
done
r1="${seen[0]%%|*}"; r2="${seen[1]%%|*}"; r3="${seen[2]%%|*}"
if [ -n "$r1" ] && [ "${r1#unsettled}" = "$r1" ] && [ "$r1" = "$r2" ] && [ "$r2" = "$r3" ]; then
  pass "O1 roles follow fingerprints across 3 creation orders and wiphy names (${seen[0]#*|} / ${seen[1]#*|} / ${seen[2]#*|}): $r1"
else
  fail "O1 roles by creation order" "round 1: ${seen[0]}; round 2: ${seen[1]}; round 3: ${seen[2]}"
fi

# O2, O3, O4 -------------------------------------------------------------------
for l in A B C; do add "$l"; done
start_meshd
if ! wait_until 40 settled3; then
  fail "O2 setup" "roles never settled: $(roles); warnings: $(warns)"
else
  m=$(letter_of "$(holder mesh)"); a=$(letter_of "$(holder ap)")
  del "$m"
  # The radio keeps its role for a 3 s grace period, then roles move.
  if wait_until 30 '[ "$(nradios)" = 2 ] && [ "$(holder mesh)" != none ] && [ "$(holder mesh)" != "$(fp "$m")" ] && [ "$(holder ap)" != none ] && converged'; then
    m2=$(letter_of "$(holder mesh)"); a2=$(letter_of "$(holder ap)")
    case " $(warns) " in
      *" ap-shares-mesh-radio "*) fail "O2 mesh radio removed" "the AP shares the mesh radio although a radio was free: $(roles)" ;;
      *) pass "O2 mesh radio $m removed: mesh moved to $m2, the AP stays on a radio of its own ($a2; it was on $a): $(roles)" ;;
    esac
    del "$m2"
    if wait_until 30 '[ "$(nradios)" = 1 ] && [ "$(role_of "$a2")" = "mesh+ap" ]'; then
      case " $(warns) " in
        *" ap-shares-mesh-radio "*) pass "O3 with only the AP's radio $a2 left, the mesh takes it and the AP shares it (mesh+ap, ap-shares-mesh-radio)" ;;
        *) fail "O3 one radio left" "role mesh+ap but no ap-shares-mesh-radio warning: $(warns)" ;;
      esac
      add D
      if wait_until 30 '[ "$(nradios)" = 2 ] && [ "$(role_of D)" = ap ]'; then
        if [ "$(role_of "$a2")" = mesh ]; then
          pass "O4 added radio D (ranked first) took the shared AP role, not the mesh: $(roles); meshd recommends D for: $(rec_of D)"
        else
          fail "O4 added radio" "mesh moved: $(roles)"
        fi
      else
        fail "O4 added radio" "D did not take the AP role: $(roles); warnings: $(warns)"
      fi
    else
      fail "O3 one radio left" "expected $a2 as mesh+ap: $(roles); warnings: $(warns)"
    fi
  else
    fail "O2 mesh radio removed" "roles did not move: $(roles); warnings: $(warns)"
  fi
fi
stop_meshd
for l in "${!NAME[@]}"; do del "$l"; done

# O5, O6 ---------------------------------------------------------------------
add E rE --no-ap
start_meshd
if wait_until 40 '[ "$(role_of E)" = mesh ]'; then
  case " $(warns) " in
    *" no-ap-radio "*) pass "O5 a lone radio without the AP interface type carries the mesh only (warning no-ap-radio)" ;;
    *) fail "O5 mesh-only radio" "no no-ap-radio warning: $(warns)" ;;
  esac
  add F
  if wait_until 30 '[ "$(role_of F)" = ap ] && [ "$(role_of E)" = mesh ]'; then
    pass "O6 an added AP-capable radio fills the empty AP role; the mesh stays on E: $(roles)"
  else
    fail "O6 added AP radio" "$(roles); warnings: $(warns)"
  fi
else
  fail "O5 mesh-only radio" "E did not take the mesh: $(roles); warnings: $(warns)"
fi

stop_meshd
for l in "${!NAME[@]}"; do del "$l"; done

# O7 -------------------------------------------------------------------------
add G; add H
both() { [ "$(nradios)" = 2 ] && [ "$(holder mesh)" != none ] && [ "$(holder ap)" != none ]; }
# G's fingerprint isn't mac:..., so read roles by wiphy name here.
byname() { jsonq "' '.join(sorted(r['phy']+'='+r['role']+'/'+r['fingerprint_source'] for r in s['radios']))"; }
declare -a seen7
for _ in 1 2 3; do
  start_meshd
  if wait_until 40 both; then seen7+=("$(byname)"); else seen7+=("unsettled: $(byname)"); fi
  stop_meshd
done
gsrc=$(echo "${seen7[0]}" | tr ' ' '\n' | sed -n 's/^rG=[^/]*\///p')
if [ "${seen7[0]#unsettled}" = "${seen7[0]}" ] && [ "${seen7[0]}" = "${seen7[1]}" ] && [ "${seen7[1]}" = "${seen7[2]}" ] && [ -n "$gsrc" ] && [ "$gsrc" != permanent-mac ]; then
  pass "O7 a radio with a random MAC is fingerprinted by $gsrc, and both radios keep their roles across 2 restarts: ${seen7[0]}"
else
  fail "O7 random-MAC fingerprint" "start 1: ${seen7[0]}; start 2: ${seen7[1]}; start 3: ${seen7[2]}"
fi

echo "== hwsim role results: $PASS passed, $FAIL failed (logs: $RUN)"
[ "$FAIL" = 0 ]
