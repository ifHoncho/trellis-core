#!/bin/sh
# Bring a node up to date with the build in this directory. Runs on the
# node, as root; scripts/deploy.sh copies this directory over and runs it.
#
#   sudo ~/trellis-deploy/scripts/node-update.sh
#
# Every step is idempotent and skipped when already done, so a routine
# update is: stop meshd, install the binary, start meshd, show status.
#   1. stop meshd (a loaded bat0 would block a batman-adv module reload)
#   2. install binary, systemd unit, udev rule (install-node.sh)
#   3. batman-adv: load the pinned DKMS build, building it only if needed
#   3a. MT7915/MT7916 PCIe Wi-Fi: build mt7915e (DKMS) if a card is fitted
#       and the kernel has no driver for it (Raspberry Pi OS)
#   3b. radio driver options from the hardware database (modprobe.d), with
#       a module reload where the loaded value differs
#   4. regulatory database: install the fallback only if the kernel rejects
#      the current one
#   5. start meshd if the node is configured, and print its status
set -eu
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
BIN="$ROOT/bin/trellis"
PIN="$(sed -n 's/^BATADV_VERSION="${BATADV_VERSION:-\(.*\)}"$/\1/p' "$HERE/install-batman-adv-dkms.sh")"

[ "$(id -u)" -eq 0 ] || { echo "run as root (sudo)" >&2; exit 1; }

# Run detached from the SSH session that started us. Stopping meshd can take
# away the address that session uses (a node whose only uplink address is
# meshd's own DHCP lease), and the hangup would then kill this script with
# meshd stopped, leaving the node off the network. Detached, the update
# finishes and restarts meshd regardless; the session just follows the log.
LOG=/var/log/trellis-node-update.log
if [ -z "${TRELLIS_UPDATE_DETACHED:-}" ] && command -v setsid >/dev/null 2>&1; then
  : > "$LOG"
  TRELLIS_UPDATE_DETACHED=1 setsid -w "$0" "$@" >> "$LOG" 2>&1 < /dev/null &
  pid=$!
  tail -n +1 -f --pid="$pid" "$LOG" 2>/dev/null &
  tpid=$!
  rc=0
  wait "$pid" || rc=$?
  wait "$tpid" 2>/dev/null || true
  [ "$rc" -eq 0 ] || echo "(node-update exited with status $rc; full log: $LOG)" >&2
  exit "$rc"
fi
[ -x "$BIN" ] || { echo "no binary at $BIN" >&2; exit 1; }
step() { printf '\n== %s\n' "$*"; }

step "Trellis $("$BIN" version | cut -d' ' -f2) on $(hostname)"

if systemctl is-active --quiet trellis-meshd; then
  echo "-- stopping trellis-meshd"
  systemctl stop trellis-meshd
fi

step "Install"
"$HERE/install-node.sh" "$BIN"

step "batman-adv $PIN"
loaded="$(cat /sys/module/batman_adv/version 2>/dev/null || true)"
avail="$(modinfo -F version batman-adv 2>/dev/null || true)"
path="$(modinfo -n batman-adv 2>/dev/null || true)"
case "$path" in */updates/*) dkms_mod=1 ;; *) dkms_mod=0 ;; esac
if [ "$loaded" = "$PIN" ] && [ "$dkms_mod" = 1 ]; then
  echo "-- $PIN (DKMS build) is loaded"
elif [ "$avail" = "$PIN" ] && [ "$dkms_mod" = 1 ]; then
  echo "-- loading the installed $PIN DKMS build (was: ${loaded:-not loaded})"
  modprobe -r batman-adv 2>/dev/null || true
  modprobe batman-adv
  echo "-- loaded: $(cat /sys/module/batman_adv/version)"
else
  echo "-- not installed for this kernel (loaded: ${loaded:-none}); building it (a few minutes)"
  "$HERE/install-batman-adv-dkms.sh"
fi

step "MT7915/MT7916 driver"
# MT7915/MT7916 cards need mt7915e, which Raspberry Pi OS kernels lack.
# Built once; DKMS rebuilds it after kernel updates. On a Pi 4/CM4 the
# script also sets the overlays the card needs and may ask for a reboot.
mt79=""
for dev in /sys/bus/pci/devices/*; do
  [ "$(cat "$dev/vendor" 2>/dev/null)" = 0x14c3 ] || continue
  case "$(cat "$dev/device" 2>/dev/null)" in 0x7915|0x7906|0x790a) mt79="$mt79 $(basename "$dev")" ;; esac
done
if [ -z "$mt79" ]; then
  echo "-- no MT7915/MT7916 card"
elif modinfo mt7915e >/dev/null 2>&1; then
  echo "-- card(s)$mt79; mt7915e available: $(modinfo -n mt7915e)"
else
  echo "-- card(s)$mt79 but no mt7915e for this kernel; building it (a few minutes)"
  # Not fatal: a failed build must not leave the node with meshd stopped.
  "$HERE/install-mt7915e-dkms.sh" || echo "!! mt7915e install failed (see above); meshd starts without the card"
fi

step "Radio driver options"
# Module parameters some radios need for a working encrypted mesh, from the
# hardware database (for example ath9k_htc nohwcrypt=1). A loaded module
# with the wrong value is reloaded, which re-probes its adapters.
opts="$("$BIN" meshctl driver-options)"
conf=/etc/modprobe.d/trellis.conf
want="# Written by Trellis node-update.sh from the hardware database.
$opts"
if [ -z "$opts" ]; then
  rm -f "$conf"
elif ! [ -f "$conf" ] || [ "$(cat "$conf")" != "$want" ]; then
  printf '%s\n' "$want" > "$conf"
  echo "-- wrote $conf"
fi
printf '%s\n' "$opts" | while read -r _ mod params; do
  [ -n "${mod:-}" ] && [ -d "/sys/module/$mod" ] || continue
  for kv in $params; do
    k="${kv%%=*}"; v="${kv#*=}"
    have="$(cat "/sys/module/$mod/parameters/$k" 2>/dev/null || echo "")"
    case "$have" in Y) have=1 ;; N) have=0 ;; esac
    if [ "$have" != "$v" ]; then
      case "$mod" in
        ath9k_htc)
          # The AR9271 keeps running its firmware across a module reload,
          # and the reloaded driver then fails to upload it: the radio
          # vanishes until replugged. So only ask.
          echo "!! $mod is loaded with $k=$have and needs $v: unplug and replug the adapter (or reboot) once to apply it"
          ;;
        *)
          echo "-- reloading $mod ($k=$have, needs $v)"
          modprobe -r "$mod" && modprobe "$mod" && echo "-- $mod reloaded: $k=$(cat "/sys/module/$mod/parameters/$k")" \
            || echo "!! could not reload $mod; unplug and replug the adapter, or reboot"
          ;;
      esac
      break
    fi
  done
done
[ -z "$opts" ] || echo "-- in place: $(printf '%s' "$opts" | tr '\n' ';')"

step "Regulatory rules"
# Trellis answers the kernel's regulatory requests (country and unrestricted
# mode alike). On a kernel without CRDA support this fails and the kernel's
# own database stays in charge, with the old fallback for kernels that
# reject current databases.
rc=0
"$HERE/install-regulatory.sh" || rc=$?
case "$rc" in
  0) ;;
  10) "$HERE/install-regdb-fallback.sh" ;;
  *) echo "!! install-regulatory.sh failed ($rc); the regulatory rules are as they were" ;;
esac

step "Configuration"
# The deploy carries the mesh profile when mesh-profile.yaml exists on the
# development machine. It is installed only where none exists yet.
DEPLOY_PROFILE="$ROOT/profile.yaml"
if [ -f "$DEPLOY_PROFILE" ]; then
  if ! [ -f /etc/trellis/profile.yaml ]; then
    install -D -m 0600 "$DEPLOY_PROFILE" /etc/trellis/profile.yaml
    echo "-- installed the mesh profile from the deploy"
  # Compared by what they say: the web UI and meshctl rewrite the file
  # (comments dropped, defaults written out), which changes no setting.
  elif ! why="$(/usr/local/bin/meshctl profile same "$DEPLOY_PROFILE" /etc/trellis/profile.yaml 2>&1)"; then
    echo "!! /etc/trellis/profile.yaml differs from the deploy's mesh-profile.yaml; left as is."
    echo "   ${why#error: }"
    echo "   Nodes with different profiles cannot join each other. To replace it:"
    echo "     sudo install -m 0600 $DEPLOY_PROFILE /etc/trellis/profile.yaml && sudo systemctl restart trellis-meshd"
  else
    echo "-- mesh profile matches the deploy"
  fi
fi
if ! [ -f /etc/trellis/node.yaml ] && [ -f /etc/trellis/profile.yaml ]; then
  # Callsign from the hostname, made a valid DNS label.
  cs="$(hostname | tr 'A-Z_.' 'a-z--' | tr -cd 'a-z0-9-' | sed 's/^-*//; s/-*$//' | cut -c1-63)"
  [ -n "$cs" ] || cs="node"
  /usr/local/bin/meshctl node new --callsign "$cs" -o /etc/trellis/node.yaml
  echo "-- created /etc/trellis/node.yaml with callsign $cs and an automatic node ID;"
  echo "   edit it to pin node_id (1-254, unique per mesh), then: sudo meshctl reload"
fi

step "meshd"
# Everything installed above goes to the disk before meshd touches a radio.
# A kernel crash seconds after an update can leave the new binary, the unit
# files and the mesh profile as empty or half-written files, still in the
# page cache.
sync
if [ -f /etc/trellis/node.yaml ] && ! /usr/local/bin/meshctl config validate /etc/trellis/node.yaml >/dev/null 2>&1; then
  /usr/local/bin/meshctl config validate /etc/trellis/node.yaml || true
  echo "!! configuration invalid; meshd not started" >&2
  exit 1
fi
systemctl enable --quiet trellis-meshd
systemctl start trellis-meshd
for _ in $(seq 1 30); do
  /usr/local/bin/meshctl status >/dev/null 2>&1 && break
  sleep 0.5
done
if ! [ -f /etc/trellis/profile.yaml ]; then
  # No mesh profile: meshd runs in setup mode, alone, and serves the setup
  # wizard on its own Wi-Fi and Ethernet ports.
  echo "-- no mesh profile yet: the node waits to be set up. Either:"
  echo "   - in its web UI: join the Wi-Fi \"trellis-setup-XXXX\" (passphrase trellis-setup), or plug into"
  echo "     one of its Ethernet ports, and open https://10.73.0.1"
  echo "   - put the mesh's profile in mesh-profile.yaml on the development machine and deploy again"
  echo "     (from a running node: ssh <node> sudo meshctl profile show > mesh-profile.yaml)"
  echo "   - on the node: sudo meshctl profile install FILE   (or a profile code from: meshctl profile share)"
  exit 0
fi
sleep "${SETTLE:-8}"   # let the mesh link come up before showing status
/usr/local/bin/meshctl status

step "Web UI"
/usr/local/bin/meshctl web || true
