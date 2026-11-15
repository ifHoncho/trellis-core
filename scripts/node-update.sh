#!/bin/sh
# Bring a node up to date with the build in this directory. Runs on the
# node, as root; scripts/deploy.sh copies this directory over and runs it.
#
#   sudo ~/trellis-deploy/scripts/node-update.sh
#
# Every step is idempotent and skipped when already done, so a routine
# update is: stop meshd, install the package, start meshd, show status.
#   1. stop meshd (a loaded bat0 would block a batman-adv module reload)
#   2. install the trellis package the deploy built (deb/), with apt, so
#      whatever it depends on comes with it; a node installed by the
#      earlier scripts is taken over by the package
#   3. batman-adv: the trellis-batman-adv-dkms package, then load the
#      pinned build, building it only if needed
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
DEB="$(ls "$ROOT"/deb/trellis_*.deb 2>/dev/null | head -1)"
BATDEB="$(ls "$ROOT"/deb/trellis-batman-adv-dkms_*.deb 2>/dev/null | head -1)"
BIN=/usr/bin/trellis
PIN="$(sed -n 's/^BATADV_VERSION=//p' "$ROOT/packaging/dkms/source")"

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
# On the way out: if this script stopped meshd and fails before starting
# it again (apt cannot fetch a dependency, a build fails), start it anyway,
# so the node is never left off the mesh: a node reached over the mesh could
# not be reached again to repair it. Then the log's last line says how the
# update ended, for whoever lost the session that started it (deploy.sh
# reconnects and reads it).
STOPPED_MESHD=""
on_exit() {
  rc=$?
  if [ -n "$STOPPED_MESHD" ] && ! systemctl is-active --quiet trellis-meshd; then
    echo "!! the update failed with meshd stopped; starting it again on what is installed"
    systemctl start trellis-meshd || true
  fi
  [ -z "${TRELLIS_UPDATE_DETACHED:-}" ] || echo "== node-update exit $rc"
}
trap on_exit EXIT
[ -n "$DEB" ] || { echo "no trellis package in $ROOT/deb" >&2; exit 1; }
step() { printf '\n== %s\n' "$*"; }
# apt, never asking: it installs what the package depends on, keeps every
# configuration file as it is, and takes a development build whatever its
# version (they do not sort).
apt_install() {
  DEBIAN_FRONTEND=noninteractive apt-get install -y --allow-downgrades --no-install-recommends \
    -o Dpkg::Options::=--force-confold -o Dpkg::Options::=--force-confdef "$@"
}

step "Trellis $(dpkg-deb -f "$DEB" Trellis-Version) on $(hostname)"

if systemctl is-active --quiet trellis-meshd; then
  echo "-- stopping trellis-meshd"
  systemctl stop trellis-meshd
  STOPPED_MESHD=1
fi

step "Install"
if ! apt_install "$DEB" > /tmp/trellis-apt.log 2>&1; then
  # Most often a package it depends on is not in the node's stale lists.
  echo "-- apt could not install it; refreshing the package lists and trying again"
  apt-get update >/dev/null 2>&1 || true
  apt_install "$DEB" > /tmp/trellis-apt.log 2>&1 || { tail -n 25 /tmp/trellis-apt.log; echo "!! the trellis package did not install" >&2; exit 1; }
fi
grep -E '^trellis: ' /tmp/trellis-apt.log || true
echo "-- installed: trellis $(dpkg-query -W -f='${Version}' trellis)"
command -v batctl >/dev/null 2>&1 || apt_install batctl >/dev/null 2>&1 || true

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
  BATADV_VERSION="$PIN" "$HERE/install-batman-adv-dkms.sh"
fi
# The package that owns the source and has DKMS rebuild it after kernel
# updates. Installed after the module is in place, so its own build step
# finds nothing left to do.
if [ -n "$BATDEB" ] && [ "$(dpkg-query -W -f='${Version}' trellis-batman-adv-dkms 2>/dev/null)" != "$(dpkg-deb -f "$BATDEB" Version)" ]; then
  if apt_install "$BATDEB" > /tmp/trellis-apt.log 2>&1; then
    echo "-- installed: trellis-batman-adv-dkms $(dpkg-deb -f "$BATDEB" Version)"
  else
    tail -n 15 /tmp/trellis-apt.log; echo "!! trellis-batman-adv-dkms did not install; the module in place is used"
  fi
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
# The package wrote them to /etc/modprobe.d/trellis.conf.
opts="$("$BIN" meshctl driver-options)"
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
  elif ! why="$(/usr/bin/meshctl profile same "$DEPLOY_PROFILE" /etc/trellis/profile.yaml 2>&1)"; then
    echo "!! /etc/trellis/profile.yaml differs from the deploy's mesh-profile.yaml; left as is."
    echo "   ${why#error: }"
    echo "   Nodes with different profiles cannot join each other. To replace it:"
    echo "     sudo install -m 0600 $DEPLOY_PROFILE /etc/trellis/profile.yaml && sudo systemctl restart trellis-meshd"
  else
    echo "-- mesh profile matches the deploy"
  fi
fi
# The deploy carries a development release key when dev_key.pub exists on
# the development machine: the node then trusts packages signed with it.
# This one file is the deploy's own, so a deploy without the key removes it.
DEPLOY_DEV_KEY="$ROOT/dev-key.pub"
DEV_KEY_FILE=/etc/trellis/release-keys/deploy-dev.pub
if [ -f "$DEPLOY_DEV_KEY" ]; then
  if ! cmp -s "$DEPLOY_DEV_KEY" "$DEV_KEY_FILE"; then
    install -D -m 0644 "$DEPLOY_DEV_KEY" "$DEV_KEY_FILE"
    echo "-- trusts the deploy's development release key ($DEV_KEY_FILE)"
  fi
elif [ -f "$DEV_KEY_FILE" ]; then
  rm -f "$DEV_KEY_FILE"
  echo "-- removed the deploy's development release key"
fi
if ! [ -f /etc/trellis/node.yaml ] && [ -f /etc/trellis/profile.yaml ]; then
  # Callsign from the hostname, made a valid DNS label.
  cs="$(hostname | tr 'A-Z_.' 'a-z--' | tr -cd 'a-z0-9-' | sed 's/^-*//; s/-*$//' | cut -c1-63)"
  [ -n "$cs" ] || cs="node"
  /usr/bin/meshctl node new --callsign "$cs" -o /etc/trellis/node.yaml
  echo "-- created /etc/trellis/node.yaml with callsign $cs and an automatic node ID;"
  echo "   edit it to pin node_id (1-254, unique per mesh), then: sudo meshctl reload"
fi

step "meshd"
# Everything installed above goes to the disk before meshd touches a radio.
# A kernel crash seconds after an update can leave the new binary, the unit
# files and the mesh profile as empty or half-written files, still in the
# page cache.
sync
if [ -f /etc/trellis/node.yaml ] && ! /usr/bin/meshctl config validate /etc/trellis/node.yaml >/dev/null 2>&1; then
  /usr/bin/meshctl config validate /etc/trellis/node.yaml || true
  echo "!! configuration invalid; meshd not started" >&2
  exit 1
fi
systemctl enable --quiet trellis-meshd
systemctl start trellis-meshd
STOPPED_MESHD=""
for _ in $(seq 1 30); do
  /usr/bin/meshctl status >/dev/null 2>&1 && break
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
/usr/bin/meshctl status

step "Web UI"
/usr/bin/meshctl web || true
