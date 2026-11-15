package deploy

// The scripts the run copies to a node and runs there as root, from the
// directory it copied the files to ($1). Lines starting "@@" are for the
// tool: "@@ step", "@@ warn", "@@ fail", "@@ reboot", "@@ done"; the rest
// goes to the node's log on the operator's computer.

// installScript: a node with no Trellis. $2 callsign, $3 node ID (0: the
// node derives one).
const installScript = `#!/bin/sh
set -u
D="$1"; CS="$2"; ID="$3"
export DEBIAN_FRONTEND=noninteractive
inst() {
  apt-get install -y --no-install-recommends \
    -o Dpkg::Options::=--force-confold -o Dpkg::Options::=--force-confdef "$@"
}
set -- "$D/trellis.deb"
[ -f "$D/dkms.deb" ] && set -- "$@" "$D/dkms.deb"
if [ -f "$D/node-key.pub" ]; then
  install -D -m 0644 "$D/node-key.pub" /etc/trellis/release-keys/deploy-dev.pub
  echo "-- trusts the deploy's release key (/etc/trellis/release-keys/deploy-dev.pub)"
fi

echo "@@ step installing the packages"
if ! inst "$@" > "$D/apt.log" 2>&1; then
  echo "-- apt could not install them; refreshing the package lists and trying again"
  apt-get update -q > /dev/null 2>&1 || true
  if ! inst "$@" > "$D/apt.log" 2>&1; then
    tail -n 25 "$D/apt.log"
    echo "@@ fail the packages did not install (see the log)"
    exit 1
  fi
fi
cat "$D/apt.log"
echo "-- installed: trellis $(dpkg-query -W -f='${Version}' trellis)"

echo "@@ step preparing the system (a few minutes when modules are built)"
/usr/bin/meshctl prepare --no-doctor || echo "@@ warn meshctl prepare reported problems (see the log)"

echo "@@ step configuration"
if [ -f "$D/profile.yaml" ]; then
  if [ -e /etc/trellis/profile.yaml ]; then
    echo "-- a mesh profile is in place already: kept"
  else
    install -D -m 0600 "$D/profile.yaml" /etc/trellis/profile.yaml && echo "-- mesh profile installed"
  fi
  rm -f "$D/profile.yaml"
fi
if [ ! -e /etc/trellis/node.yaml ] && [ -e /etc/trellis/profile.yaml ]; then
  if [ "$ID" = 0 ]; then
    /usr/bin/meshctl node new --callsign "$CS" -o /etc/trellis/node.yaml
  else
    /usr/bin/meshctl node new --callsign "$CS" --id "$ID" -o /etc/trellis/node.yaml
  fi && echo "-- node configuration made: callsign $CS"
fi
[ -e /run/reboot-required ] && echo "@@ reboot"
echo "@@ done"
`

// upgradeScript: a node with Trellis already, $2 the mode:
//   - upgrade: meshctl upgrade, which checks the node and goes back when it
//     is not healthy; downgrade: the same to an older build;
//   - reinstall: the same version again (--force), with apt;
//   - install: with apt, no health check (--no-health-check, for a
//     developer's quick iterations), then meshctl prepare, and meshd runs.
//
// The batman-adv package comes first when it changed, and meshctl prepare
// afterwards loads the new module. The script detaches from the session:
// restarting meshd can take away the path the session runs over (a node
// reached through the mesh), and a hangup must not stop apt halfway. The
// session follows its log; $D/exit holds the outcome for a tool that lost
// it.
const upgradeScript = `#!/bin/sh
set -u
D="$1"; MODE="${2:-upgrade}"
if [ -z "${TD_DETACHED:-}" ] && command -v setsid > /dev/null 2>&1; then
  rm -f "$D/exit"; : > "$D/run.log"
  TD_DETACHED=1 setsid -w sh "$0" "$@" >> "$D/run.log" 2>&1 < /dev/null &
  pid=$!
  tail -n +1 -f --pid="$pid" "$D/run.log" 2> /dev/null &
  tpid=$!
  rc=0; wait "$pid" || rc=$?
  wait "$tpid" 2> /dev/null || true
  exit "$rc"
fi
export DEBIAN_FRONTEND=noninteractive
finish() { echo "@@ upgrade-exit $1"; echo "$1" > "$D/exit"; exit "$1"; }
inst() {
  apt-get install -y --no-install-recommends --allow-downgrades \
    -o Dpkg::Options::=--force-confold -o Dpkg::Options::=--force-confdef "$@"
}
if [ -f "$D/node-key.pub" ]; then
  install -D -m 0644 "$D/node-key.pub" /etc/trellis/release-keys/deploy-dev.pub
  echo "-- trusts the deploy's release key (/etc/trellis/release-keys/deploy-dev.pub)"
fi
if [ -f "$D/dkms.deb" ] && [ "$MODE" != install ]; then
  echo "@@ step the batman-adv package"
  inst "$D/dkms.deb" > "$D/apt.log" 2>&1 ||
    { tail -n 15 "$D/apt.log"; echo "@@ warn trellis-batman-adv-dkms did not install; the module in place stays"; }
fi
rc=0
case "$MODE" in
  install)
    echo "@@ step installing (no health check)"
    set -- "$D/trellis.deb"
    [ -f "$D/dkms.deb" ] && set -- "$@" "$D/dkms.deb"
    if ! inst "$@" > "$D/apt.log" 2>&1; then
      apt-get update -q > /dev/null 2>&1 || true
      inst "$@" > "$D/apt.log" 2>&1 || { tail -n 25 "$D/apt.log"; echo "error: the packages did not install"; finish 1; }
    fi
    echo "-- installed: trellis $(dpkg-query -W -f='${Version}' trellis)"
    echo "@@ step preparing the system"
    /usr/bin/meshctl prepare --no-doctor || echo "@@ warn meshctl prepare reported problems (see the log)"
    if [ ! -d /run/systemd/system ]; then
      echo "@@ warn no systemd here: meshd is not started"
    elif [ -e /etc/trellis/profile.yaml ]; then
      systemctl start trellis-meshd 2> /dev/null || true
      for _ in $(seq 1 30); do /usr/bin/meshctl status > /dev/null 2>&1 && break; sleep 1; done
      /usr/bin/meshctl status > /dev/null 2>&1 || { echo "error: meshd does not answer after the install (sudo meshctl doctor)"; finish 1; }
    fi
    ;;
  reinstall)
    echo "@@ step reinstalling"
    inst --reinstall "$D/trellis.deb" > "$D/apt.log" 2>&1 || { tail -n 25 "$D/apt.log"; echo "error: the package did not reinstall"; rc=1; }
    ;;
  downgrade)
    echo "@@ step installing an older build (the node checks itself, and goes back if it is not healthy)"
    /usr/bin/meshctl upgrade --allow-downgrade "$D/trellis.deb" || rc=$?
    ;;
  *)
    echo "@@ step upgrading (the node checks itself, and goes back if it is not healthy)"
    /usr/bin/meshctl upgrade "$D/trellis.deb" || rc=$?
    ;;
esac
if [ "$rc" = 0 ] && [ -f "$D/dkms.deb" ] && [ "$MODE" != install ]; then
  echo "@@ step loading the new batman-adv"
  /usr/bin/meshctl prepare --no-doctor || echo "@@ warn meshctl prepare reported problems (see the log)"
fi
finish "$rc"
`
