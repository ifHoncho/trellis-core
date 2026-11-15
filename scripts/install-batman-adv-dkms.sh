#!/bin/sh
# Install the pinned batman-adv release as a DKMS module, so it is built for
# the running kernel now and rebuilt automatically after kernel updates
# (architecture §6.1). Run as root on a Debian-family system.
#
#   scripts/install-batman-adv-dkms.sh            # the pinned release
#   BATADV_VERSION=2026.2 scripts/install-batman-adv-dkms.sh
#
# The release, unless BATADV_VERSION names one: that of the installed
# trellis-batman-adv-dkms package (which owns its source), else the pin in
# packaging/dkms/source, the one place Trellis pins it.
#
# The module lands in /lib/modules/<kernel>/updates/dkms/, which takes
# precedence over the kernel's own copy.
#
# The trellis-batman-adv-dkms package ships the source in /usr/src and runs
# this script when it is installed (TRELLIS_IN_DPKG=1: no package may be
# installed from inside dpkg, so missing kernel headers or compilers are
# named instead). Run by hand, it fetches what is missing.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
if [ -z "${BATADV_VERSION:-}" ]; then
  v="$(dpkg-query -W -f='${db:Status-Abbrev} ${Version}' trellis-batman-adv-dkms 2>/dev/null || true)"
  case "$v" in ii*) BATADV_VERSION="${v##* }"; BATADV_VERSION="${BATADV_VERSION%-*}" ;; esac
fi
if [ -z "${BATADV_VERSION:-}" ]; then
  BATADV_VERSION="$(sed -n 's/^BATADV_VERSION=//p' "$HERE/../packaging/dkms/source" 2>/dev/null || true)"
fi
[ -n "${BATADV_VERSION:-}" ] || { echo "no batman-adv release to install: set BATADV_VERSION" >&2; exit 2; }
REPO="${BATADV_REPO:-https://github.com/open-mesh-mirror/batman-adv.git}"
IN_DPKG="${TRELLIS_IN_DPKG:-}"
# apt-get, where there is one and this is not running inside dpkg.
can_apt() { [ -z "$IN_DPKG" ] && command -v apt-get >/dev/null 2>&1; }
NAME=trellis-batman-adv
SRC="/usr/src/${NAME}-${BATADV_VERSION}"
KREL="$(uname -r)"

if [ "$(id -u)" -ne 0 ]; then
  echo "run as root" >&2; exit 1
fi

echo "== Trellis: batman-adv ${BATADV_VERSION} via DKMS for kernel ${KREL}"

# A 64-bit kernel under a 32-bit userland (possible on Raspberry Pi) cannot
# build modules: the native compiler targets the wrong architecture.
KARCH="$(uname -m)"
UARCH="$(dpkg --print-architecture 2>/dev/null || echo unknown)"
if [ "$KARCH" = aarch64 ] && [ "$UARCH" = armhf ]; then
  echo "!! The kernel is 64-bit (aarch64) but the operating system is 32-bit (armhf)," >&2
  echo "   so kernel modules cannot be built here. Either install the 64-bit OS, or boot" >&2
  echo "   the 32-bit kernel: add arm_64bit=0 to /boot/firmware/config.txt and reboot." >&2
  exit 1
fi

# Kernel headers: Debian, Ubuntu and Raspberry Pi OS package them as
# linux-headers-$(uname -r); older Raspberry Pi OS as raspberrypi-kernel-headers;
# FriendlyElec images keep a .deb under /opt/archives.
if ! [ -f "/lib/modules/${KREL}/build/Makefile" ] && can_apt; then
  echo "-- no kernel headers for ${KREL}; trying to install them"
  export DEBIAN_FRONTEND=noninteractive
  if apt-get install -y --no-install-recommends "linux-headers-${KREL}" >/dev/null 2>&1; then
    echo "-- installed linux-headers-${KREL}"
  elif apt-cache show raspberrypi-kernel-headers >/dev/null 2>&1 \
       && apt-get install -y --no-install-recommends raspberrypi-kernel-headers >/dev/null 2>&1; then
    echo "-- installed raspberrypi-kernel-headers"
  else
    for deb in /opt/archives/linux-headers-*.deb; do
      [ -f "$deb" ] || continue
      case "$(dpkg-deb -f "$deb" Package 2>/dev/null)" in
        *"${KREL}"*) echo "-- installing $deb (FriendlyElec)"; dpkg -i "$deb" >/dev/null ;;
      esac
    done
  fi
fi
if ! [ -f "/lib/modules/${KREL}/build/Makefile" ]; then
  echo "!! No kernel headers at /lib/modules/${KREL}/build, and none could be installed." >&2
  echo "   Install the headers package matching kernel ${KREL} from the board vendor, then rerun." >&2
  ls /opt/archives/linux-headers-*.deb 2>/dev/null | sed 's/^/   available: /' >&2 || true
  exit 1
fi

# Kernel configuration: /proc/config.gz (Raspberry Pi kernels build it as the
# "configs" module), /boot/config-*, or the headers' own .config.
[ -r /proc/config.gz ] || modprobe configs 2>/dev/null || true
CFG=""
if [ -r /proc/config.gz ]; then CFG="$(zcat /proc/config.gz)"
elif [ -r "/boot/config-${KREL}" ]; then CFG="$(cat "/boot/config-${KREL}")"
elif [ -r "/lib/modules/${KREL}/build/.config" ]; then CFG="$(cat "/lib/modules/${KREL}/build/.config")"
fi
if [ -n "$CFG" ] && echo "$CFG" | grep -q '^CONFIG_BATMAN_ADV=y'; then
  echo "!! CONFIG_BATMAN_ADV=y: batman-adv is built into this kernel and cannot be replaced." >&2
  echo "   This board needs a Trellis-built kernel (see docs/hardware.md)." >&2
  exit 1
fi

if can_apt && ! { command -v dkms && command -v make && command -v gcc; } >/dev/null 2>&1; then
  export DEBIAN_FRONTEND=noninteractive
  apt-get install -y --no-install-recommends dkms build-essential >/dev/null
fi

# The source: shipped by the package, left by an earlier run, or fetched.
if [ -f "$SRC/Makefile" ] && [ -f "$SRC/net/batman-adv/main.c" ]; then
  echo "-- source present at $SRC"
else
  if ! command -v git >/dev/null 2>&1; then
    can_apt || { echo "!! no batman-adv source at $SRC and no git to fetch it" >&2; exit 1; }
    apt-get install -y --no-install-recommends git >/dev/null
  fi
  rm -rf "$SRC"
  echo "-- fetching v${BATADV_VERSION} from ${REPO}"
  git clone --quiet --branch "v${BATADV_VERSION}" --depth 1 "$REPO" "$SRC"
fi
sed "s/@VERSION@/${BATADV_VERSION}/" "$HERE/../packaging/dkms/dkms.conf" > "$SRC/dkms.conf"
install -m 0755 "$HERE/../packaging/dkms/trellis-build.sh" "$SRC/trellis-build.sh"


BUILD="/lib/modules/${KREL}/build"

# Use the gcc major version the kernel was built with (see trellis-build.sh).
WANT_GCC="$(sed -n 's/^CONFIG_GCC_VERSION=\([0-9]*\)$/\1/p' "$BUILD/include/config/auto.conf" "$BUILD/.config" 2>/dev/null | head -1)"
if [ -n "$WANT_GCC" ] && [ "$WANT_GCC" -gt 0 ]; then
  WANT_GCC=$((WANT_GCC / 10000))
  HAVE_GCC="$(gcc -dumpversion 2>/dev/null | cut -d. -f1)"
  if [ "$HAVE_GCC" != "$WANT_GCC" ] && ! command -v "gcc-$WANT_GCC" >/dev/null 2>&1; then
    if can_apt && apt-cache show "gcc-$WANT_GCC" >/dev/null 2>&1; then
      echo "-- the kernel was built with gcc $WANT_GCC; installing gcc-$WANT_GCC (default here is gcc $HAVE_GCC)"
      apt-get install -y --no-install-recommends "gcc-$WANT_GCC" >/dev/null || true
    else
      echo "-- the kernel was built with gcc $WANT_GCC, which this OS does not package; using gcc $HAVE_GCC"
      echo "   (usually fine; if the build fails on an unrecognized option, the log below says which)"
    fi
  fi
fi

# Fail early, with a clear reason, on headers that cannot build modules here.
headers_runnable() {
  for tool in scripts/basic/fixdep scripts/mod/modpost; do
    [ -x "$BUILD/$tool" ] || return 1
    rc=0; "$BUILD/$tool" >/dev/null 2>&1 || rc=$?
    [ "$rc" -eq 126 ] && return 1
  done
  return 0
}
if ! headers_runnable; then
  echo "-- the kernel headers' build tools are missing or cannot run here (cross-compiled headers)."
  echo "-- rebuilding them natively with scripts/repair-kernel-headers.sh"
  "$HERE/repair-kernel-headers.sh" "$BUILD" || {
    echo "!! header repair failed; see the messages above" >&2; exit 1; }
  headers_runnable || { echo "!! header tools still cannot run after repair" >&2; exit 1; }
fi

diagnose() {
  LOG="/var/lib/dkms/${NAME}/${BATADV_VERSION}/build/make.log"
  echo "" >&2
  echo "!! DKMS build failed. Last lines of $LOG:" >&2
  tail -n 30 "$LOG" >&2 2>/dev/null || true
  echo "" >&2
  if grep -qiE 'exec format error|cannot execute binary' "$LOG" 2>/dev/null; then
    echo "   Cause: the kernel headers contain tools built for another architecture." >&2
    echo "   Fix:   sudo scripts/repair-kernel-headers.sh   then rerun this script." >&2
  elif grep -qE "unrecognized (command-line )?option|unknown argument|error: unknown" "$LOG" 2>/dev/null; then
    echo "   Cause: this compiler ($(gcc --version | head -1)) does not accept a flag the kernel was built with." >&2
    echo "   The kernel was built by: $(cat /proc/version)" >&2
    echo "   Fix:   install that gcc version, or rebuild the kernel with this one." >&2
  elif grep -qE 'No such file or directory' "$LOG" 2>/dev/null && grep -qE '\.h|include/' "$LOG" 2>/dev/null; then
    echo "   Cause: the headers package is incomplete (a kernel header is missing)." >&2
    echo "   Fix:   install the full headers package for $KREL from the board vendor." >&2
  elif grep -qE 'Module.symvers' "$LOG" 2>/dev/null; then
    echo "   Cause: the headers package has no Module.symvers." >&2
  fi
  echo "   Please send the whole $LOG when reporting this." >&2
  exit 1
}

# dkms status lines look like "name/version, kernel, arch: state",
# "name/version: added" or, on old dkms, "name, version, kernel, arch:
# state". Matched as fixed strings: kernel names carry "+" and versions ".".
dkms_has() { # version [kernel]: registered (for that kernel)
  if [ -n "${2:-}" ]; then
    dkms status 2>/dev/null | grep -F -e "${NAME}/$1, $2," -e "${NAME}, $1, $2," | grep -q -F ': installed'
  else
    dkms status 2>/dev/null | grep -q -F -e "${NAME}/$1," -e "${NAME}/$1:" -e "${NAME}, $1,"
  fi
}

# Other releases of this package, so that only the pinned one is active:
# those dkms lists, and the leftovers of those it no longer can (a package
# upgrade deletes the old source before this runs). A release whose source
# a package owns is left alone: removing it is that package's business.
# Prints what it removed.
remove_others() {
  {
    dkms status 2>/dev/null | grep -F -e "${NAME}/" -e "${NAME}, " | sed -E "s|^${NAME}[/,] *([^,:]+).*|\1|"
    for d in /var/lib/dkms/"${NAME}"/[0-9]* /usr/src/"${NAME}"-[0-9]*; do
      [ -d "$d" ] && basename "$d" | sed "s|^${NAME}-||"
    done
  } | sort -u | while read -r v; do
    if [ -n "$v" ] && [ "$v" != "$BATADV_VERSION" ]; then
      if dpkg-query -S "/usr/src/${NAME}-${v}" >/dev/null 2>&1; then
        echo "-- left batman-adv $v alone: the trellis-batman-adv-dkms package owns it" >&2
        continue
      fi
      dkms remove "${NAME}/${v}" --all >/dev/null 2>&1 || true
      rm -rf "/var/lib/dkms/${NAME}/${v}" "/usr/src/${NAME}-${v}"
      echo "$v"
    fi
  done
}

if dkms_has "$BATADV_VERSION" "$KREL"; then
  echo "-- already built and installed for ${KREL}"
  # Others left over from an earlier run: removing one may take the module
  # file it shares with this release, so install this one again after.
  if [ -n "$(remove_others)" ]; then
    dkms install --force "${NAME}/${BATADV_VERSION}" -k "$KREL"
  fi
else
  dkms_has "$BATADV_VERSION" || dkms add "${NAME}/${BATADV_VERSION}"
  # Built first: a release that fails to build leaves the node with the
  # module it had. The others go before the install, which then puts its
  # own module file in place.
  dkms build "${NAME}/${BATADV_VERSION}" -k "$KREL" || diagnose
  for v in $(remove_others); do echo "-- removed batman-adv $v"; done
  dkms install --force "${NAME}/${BATADV_VERSION}" -k "$KREL"
fi
depmod -a

# Unloading batman-adv deletes every mesh interface of it. With a mesh
# running (a package installed on a live node), the new module waits.
loaded="$(cat /sys/module/batman_adv/version 2>/dev/null || true)"
if [ "$loaded" = "$BATADV_VERSION" ]; then
  echo "-- batman-adv $loaded is loaded"
elif ip -o link show type batadv 2>/dev/null | grep -q .; then
  echo "-- batman-adv ${loaded:-?} carries the running mesh; $BATADV_VERSION loads at the next boot, or now with:"
  echo "   sudo systemctl stop trellis-meshd && sudo modprobe -r batman-adv && sudo systemctl start trellis-meshd"
else
  echo "-- loading the new module"
  modprobe -r batman-adv 2>/dev/null || true
  modprobe batman-adv
  echo "-- loaded batman-adv version: $(cat /sys/module/batman_adv/version)"
fi
echo "-- module file: $(modinfo -n batman-adv)"
case "$(modinfo -n batman-adv)" in
  */updates/*) echo "== OK: the DKMS build is installed" ;;
  *) echo "!! the kernel's own copy is still being loaded; check 'dkms status' and depmod" >&2; exit 1 ;;
esac
