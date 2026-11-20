#!/usr/bin/env bash
# Trellis trials lab: one node running a real meshd, with a stand-in for a
# driver (meshd --test-trial): a file that plays the module, and files that
# say whether its radio is present, its link up and a peer back. Boots are
# stood in for by --boot-id, and a crash by kill -9. Checks that a good
# trial is confirmed and kept, that one the node restarts on is undone by
# the boot hook and latched off, the re-arm, that one that never gets
# healthy is undone by meshd, a trial armed for the next boot, a boot
# where the hook did not run, and that a restart without a shutdown is in
# the node's reset history.
#
#   lab/trial.sh [--keep] [--verbose]
#
# Needs make build, and root or an unprivileged user namespace with
# batman_adv loaded, as lab/lab.sh.
set -u
KEEP=0; VERBOSE=0; ARGS=("$@")
while [ $# -gt 0 ]; do
  case "$1" in
    --keep) KEEP=1; shift ;;
    --verbose) VERBOSE=1; shift ;;
    -h|--help) sed -n '2,16p' "$0"; exit 0 ;;
    *) echo "unknown argument $1" >&2; exit 2 ;;
  esac
done
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
BIN="${TRELLIS_BIN:-$ROOT/bin/trellis}"
RUN="${LAB_RUN:-$ROOT/lab/run}/trial"
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
if [ "$(id -u)" -ne 0 ] && [ -z "${TRELLIS_LAB_UNSHARED:-}" ]; then
  export TRELLIS_LAB_UNSHARED=1
  exec unshare -Urmn --propagation private bash -c '
    mount -t tmpfs none /run && mkdir -p /run/netns
    exec "$0" "$@"' "$0" "${ARGS[@]}"
fi
export TRELLIS_LAB_UNSHARED=1

N="$RUN/node1"
STATE="$N/state"
TD="$N/trial"                      # the stand-in radio's health, and its reloads
MODULE="$RUN/root/lib/standin.ko"  # the stand-in module, as installed
ctl()   { "$BIN" meshctl --socket "$N/run/meshd.sock" "$@"; }
trial() { ctl trial "$1" --state-dir "$STATE" --boot-id "$BOOT" "${@:2}"; }
jsonq() {
  ctl status --json 2>/dev/null | python3 -I -c "import json,sys
s=json.load(sys.stdin)
print($1)" 2>/dev/null
}
# The trial named standin as status has it: one field.
tfield() { jsonq "next((str(t.get('$1','')) for t in s.get('trials',[]) if t['name']=='standin'),'none')"; }
warned() { [ "$(jsonq "any(w['code']=='$1' for w in s['warnings'])")" = True ]; }
installed() { cat "$MODULE" 2>/dev/null; }
newfile() { mkdir -p "$RUN/new"; printf '%s\n' "$1" > "$RUN/new/$1.ko"; echo "$RUN/new/$1.ko"; }
# Wait until the trial's state is $1, at most $2 seconds.
wait_state() {
  local deadline=$((SECONDS + $2))
  until [ "$(tfield state)" = "$1" ] || [ $SECONDS -ge $deadline ]; do sleep 1; done
  [ "$(tfield state)" = "$1" ]
}

cleanup() {
  [ "$KEEP" = 1 ] && { say "== keeping the trials lab up (--keep)"; return; }
  [ -f "$N/pid" ] && kill "$(cat "$N/pid")" 2>/dev/null
  sleep 1
  ip netns del node1 2>/dev/null
}
trap cleanup EXIT

start_meshd() {
  ip netns exec node1 "$BIN" meshd --node "$N/node.yaml" --state-dir "$STATE" --run-dir "$N/run" \
      --interval 2s --log-level debug --no-host-config --test-port-manager "$N/pm" \
      --test-trial "$TD" --boot-id "$BOOT" --trial-healthy-for 6s --trial-deadline 14s >> "$RUN/node1.log" 2>&1 &
  echo $! > "$N/pid"
  local deadline=$((SECONDS + 40))
  until [ "$(jsonq "s['reconcile']['passes'] > 1")" = True ] || [ $SECONDS -ge $deadline ]; do sleep 1; done
}
stop_meshd() { kill "$(cat "$N/pid")" 2>/dev/null; wait "$(cat "$N/pid")" 2>/dev/null; rm -f "$N/pid"; }
# The node goes down without a shutdown.
crash_meshd() { kill -9 "$(cat "$N/pid")" 2>/dev/null; wait "$(cat "$N/pid")" 2>/dev/null; rm -f "$N/pid"; }

# ---------------------------------------------------------------- the node
say "== one node, a stand-in driver"
rm -rf "$RUN"; mkdir -p "$STATE" "$N/run" "$N/pm" "$TD" "$(dirname "$MODULE")"
echo v1 > "$MODULE"
touch "$TD/present" "$TD/joined" "$TD/peer"
cat > "$RUN/profile.yaml" <<YAML
schema: 0
name: trial-lab
mesh_id: trellis-trial-lab
sae_passphrase: lab-passphrase-not-secret
regulatory: {mode: country, country: US}
radio: {band: 5g, channel: 149}
prefix: 10.73.0.0/16
batman: {orig_interval_ms: 500, bridge_loop_avoidance: false}
YAML
printf 'mac: "02:00:00:00:05:01"\n' > "$STATE/identity.yaml"
cat > "$N/node.yaml" <<YAML
schema: 0
callsign: lab-1
node_id: 1
profile: $RUN/profile.yaml
radios:
  roles: {mesh: disabled, ap: disabled}
ports:
  - {name: eth1, role: mesh}
ap: {enabled: false}
YAML
ip netns add node1 || { echo "cannot create namespaces" >&2; exit 2; }
ip link add eth1 netns node1 type veth peer name p1 netns node1
ip netns exec node1 ip link set lo up
ip netns exec node1 ip link set eth1 up
ip netns exec node1 ip link set p1 up
BOOT=boot-1
start_meshd

say "== tests"
if [ "$(jsonq "s['batman']['present']")" = True ] && [ "$(jsonq "s['trials']")" = "[]" ] && [ "$(jsonq "s['resets']")" = "[]" ]; then
  pass "T0 the node is up: no trials, no resets on record"
else
  fail "T0 node up" "batman=$(jsonq "s['batman']['present']") trials=$(jsonq "s['trials']") resets=$(jsonq "s['resets']")"; exit 1
fi

# T1: a good trial is confirmed by meshd and kept.
out=$(trial begin standin --file "$MODULE=$(newfile v2)" --module standin --kind module --version 2 2>&1); rc=$?
dbg "$out"
sleep 3
st=$(tfield state); left=$(tfield confirm_after_s)
if [ $rc -eq 0 ] && [ "$(installed)" = v2 ] && [ "$st" = pending ] && [ "${left:-0}" -gt 0 ] 2>/dev/null && warned trial-pending; then
  pass "T1.1 a trial begins: new file in place, pending in status, counting down (${left} s left)"
else
  fail "T1.1 trial begins" "rc=$rc installed=$(installed) state=$st left=$left: $out"
fi
if wait_state confirmed 20 && [ "$(installed)" = v2 ] && ! warned trial-pending; then
  pass "T1.2 meshd confirms it after the healthy period; the new file stays"
else
  fail "T1.2 confirmed" "state=$(tfield state) installed=$(installed)"
fi
out=$(trial revert standin 2>&1); rc=$?
sleep 3
if [ $rc -eq 0 ] && [ "$(installed)" = v1 ] && [ "$(tfield state)" = reverted ] && [ "$(tfield latched)" != True ] && ! warned trial-reverted; then
  pass "T1.3 the version it replaced was kept: a revert by hand puts it back, without a latch"
else
  fail "T1.3 revert by hand" "rc=$rc installed=$(installed) state=$(tfield state) latched=$(tfield latched): $out"
fi

# T2: the node goes down on a trial; the boot hook undoes it and latches.
out=$(trial begin standin --file "$MODULE=$(newfile v3)" --module standin --version 3 2>&1); rc=$?
sleep 3
st=$(tfield state)
crash_meshd
BOOT=boot-2
hook=$(trial boot 2>&1); hrc=$?
dbg "$hook"
if [ $rc -eq 0 ] && [ "$st" = pending ] && [ $hrc -eq 0 ] && [ "$(installed)" = v1 ] && echo "$hook" | grep -q 'standin 3 reverted'; then
  pass "T2.1 a restart with the trial pending: the boot hook puts the old file back before meshd runs"
else
  fail "T2.1 boot hook" "begin rc=$rc state=$st hook rc=$hrc installed=$(installed): $hook"
fi
start_meshd
reason=$(tfield reason)
if [ "$(tfield state)" = reverted ] && [ "$(tfield latched)" = True ] && warned trial-reverted && echo "$reason" | grep -q 'reboot before'; then
  pass "T2.2 status says what happened: undone and latched ($reason)"
else
  fail "T2.2 status after the hook" "state=$(tfield state) latched=$(tfield latched) reason=[$reason]"
fi
verdict=$(jsonq "s['resets'][0]['verdict'] if s['resets'] else 'none'")
if { [ "$verdict" = power-loss ] || [ "$verdict" = unrecorded ]; } && warned node-reset; then
  pass "T2.3 the restart without a shutdown is in the reset history ($verdict)"
else
  fail "T2.3 reset history" "verdict=$verdict resets=$(jsonq "s['resets']")"
fi
out=$(trial begin standin --file "$MODULE=$(newfile v3)" --module standin --version 3 2>&1); rc=$?
if [ $rc -ne 0 ] && [ "$(installed)" = v1 ] && echo "$out" | grep -q 're-armed'; then
  pass "T2.4 the same version is refused while latched"
else
  fail "T2.4 latch" "rc=$rc installed=$(installed): $out"
fi
out=$(trial rearm standin 2>&1); rc=$?
out2=$(trial begin standin --file "$MODULE=$(newfile v3)" --module standin --version 3 2>&1); rc2=$?
if [ $rc -eq 0 ] && [ $rc2 -eq 0 ] && [ "$(installed)" = v3 ] && wait_state confirmed 25; then
  pass "T2.5 re-armed, it is tried again, and this time kept"
else
  fail "T2.5 rearm" "rearm rc=$rc begin rc=$rc2 installed=$(installed) state=$(tfield state): $out | $out2"
fi

# T3: a trial that never gets healthy is undone by meshd at the deadline.
rm -f "$TD/joined"
out=$(trial begin standin --file "$MODULE=$(newfile v4)" --module standin --version 4 2>&1); rc=$?
sleep 4
st=$(tfield state); left=$(tfield confirm_after_s)
if wait_state reverted 25 && [ "$(installed)" = v3 ] && [ "$(tfield latched)" = True ] && tfield reason | grep -q 'not healthy' && grep -qx standin "$TD/reloads"; then
  pass "T3.1 a trial whose link never comes up is undone at the deadline, latched, and the module reloaded ($(tfield reason))"
else
  fail "T3.1 unhealthy trial" "begin rc=$rc was=$st left=$left now=$(tfield state) installed=$(installed) latched=$(tfield latched) reason=[$(tfield reason)]"
fi
touch "$TD/joined"

# T4: a trial armed for the next boot, kept after it.
trial rearm standin > /dev/null 2>&1
out=$(trial begin standin --file "$MODULE=$(newfile v5)" --module standin --version 5 --reboot 2>&1); rc=$?
sleep 3
st=$(tfield state)
stop_meshd
BOOT=boot-3
hook=$(trial boot 2>&1)
start_meshd
if [ $rc -eq 0 ] && [ "$st" = armed ] && echo "$hook" | grep -q 'on trial' && wait_state confirmed 25 && [ "$(installed)" = v5 ]; then
  pass "T4.1 a trial armed for the next boot becomes pending there and is kept"
else
  fail "T4.1 armed trial" "begin rc=$rc was=$st hook=[$hook] now=$(tfield state) installed=$(installed)"
fi
if [ "$(jsonq "len(s['resets'])")" = 1 ]; then
  pass "T4.2 a stop and a new boot is not a reset"
else
  fail "T4.2 clean restart" "resets=$(jsonq "s['resets']")"
fi

# T5: the boot hook does not run (a system without the unit): meshd finds
# the trial pending from another boot and undoes it itself.
out=$(trial begin standin --file "$MODULE=$(newfile v6)" --module standin --version 6 2>&1); rc=$?
sleep 3
crash_meshd
BOOT=boot-4
start_meshd
if [ $rc -eq 0 ] && wait_state reverted 20 && [ "$(installed)" = v5 ] && [ "$(tfield latched)" = True ] && tfield reason | grep -q 'boot hook'; then
  pass "T5.1 without the boot hook, meshd undoes the trial after boot and says the hook did not run"
else
  fail "T5.1 hook missed" "begin rc=$rc state=$(tfield state) installed=$(installed) reason=[$(tfield reason)]"
fi

# ---------------------------------------------------------------- summary
say ""
say "== $PASS passed, $FAIL failed"
for r in "${RESULTS[@]}"; do case "$r" in FAIL*) say "  $r" ;; esac; done
[ "$FAIL" -eq 0 ]
