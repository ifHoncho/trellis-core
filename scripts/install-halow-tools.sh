#!/bin/sh
# Build and install Morse Micro's userspace tools for Wi-Fi HaLow, matching
# the driver that install-wm6108-halow.sh installs (release mm6108-2.2.2).
# Run as root on the node; safe to run again (it skips what is installed).
#
#   scripts/install-halow-tools.sh [--force]
#
# Installs into /usr/local/sbin:
#   morse_cli            Morse's diagnostic tool (version, channel, statistics)
#   wpa_supplicant_s1g   Morse's fork of wpa_supplicant: it knows S1G channels
#   wpa_cli_s1g          and how to tell the driver which one a mesh, IBSS or
#   wpa_passphrase_s1g   station uses. Built with 802.11s mesh (CONFIG_MESH),
#                        which Morse's default build leaves out.
#
# The names end in _s1g, so the system's wpa_supplicant is untouched.
#
# Sources: github.com/MorseMicro/morse_cli and github.com/MorseMicro/hostap,
# pinned by commit and cached in /var/cache/trellis/morse-tools. Both build
# with -Werror upstream; that is removed, so a newer compiler's warnings
# don't break the build.
set -eu

FORCE=0
for arg in "$@"; do
  case "$arg" in
    --force) FORCE=1 ;;
    *) echo "usage: $0 [--force]" >&2; exit 2 ;;
  esac
done

RELEASE=mm6108-2.2.2
CLI_REPO="${TRELLIS_MORSE_CLI_REPO:-https://github.com/MorseMicro/morse_cli.git}"
CLI_REF=aba52d926cfe5ef4407a6c29bf90436e0ca6279d      # tag mm6108-2.2.2
HOSTAP_REPO="${TRELLIS_MORSE_HOSTAP_REPO:-https://github.com/MorseMicro/hostap.git}"
HOSTAP_REF=3cd11f9d702df6d59cac0f57ebb2a217b0868a58   # tag mm6108-2.2.2
CACHE="${TRELLIS_MORSE_TOOLS_CACHE:-/var/cache/trellis/morse-tools}"
BINDIR=/usr/local/sbin
STAMP="$BINDIR/.trellis-halow-tools"   # what is installed: "<cli commit> <hostap commit>"

if [ "$(id -u)" -ne 0 ]; then
  echo "run as root" >&2; exit 1
fi

echo "== Trellis: Morse Micro HaLow tools ($RELEASE)"

if [ "$FORCE" = 0 ] && [ "$(cat "$STAMP" 2>/dev/null)" = "$CLI_REF $HOSTAP_REF" ] \
   && [ -x "$BINDIR/morse_cli" ] && [ -x "$BINDIR/wpa_supplicant_s1g" ] && [ -x "$BINDIR/wpa_cli_s1g" ]; then
  echo "-- already installed in $BINDIR (--force rebuilds)"
  exit 0
fi

if command -v apt-get >/dev/null 2>&1; then
  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    git build-essential pkg-config libnl-3-dev libnl-genl-3-dev libnl-route-3-dev libssl-dev >/dev/null
fi

# fetch REPO COMMIT DIR: a shallow checkout of exactly that commit.
fetch() {
  [ -f "$3/.trellis-complete" ] && return 0
  echo "-- fetching $(basename "$1" .git) ($2)"
  rm -rf "$3"
  mkdir -p "$3"
  git -C "$3" init -q
  git -C "$3" remote add origin "$1"
  git -C "$3" fetch -q --depth 1 origin "$2" \
    || { echo "!! cannot fetch $2 from $1 (is the network up?)" >&2; exit 1; }
  git -C "$3" -c advice.detachedHead=false checkout -q FETCH_HEAD
  [ "$(git -C "$3" rev-parse HEAD)" = "$2" ] || { echo "!! $1: fetched commit is not $2" >&2; exit 1; }
  touch "$3/.trellis-complete"
}

cpus="$(nproc 2>/dev/null || echo 1)"
mem_mb="$(awk '/^MemAvailable:/ {print int($2/1024)}' /proc/meminfo 2>/dev/null || echo 0)"
J=$((mem_mb / 250)); [ "$J" -lt 1 ] && J=1; [ "$J" -gt "$cpus" ] && J="$cpus"

# build NAME LOGFILE COMMAND...: run a build quietly; on failure show its errors.
build() {
  name="$1"; log="$2"; shift 2
  if ! "$@" > "$log" 2>&1; then
    echo "!! building $name failed. The errors in $log:" >&2
    grep -nE 'error|Error|\*\*\*|undefined reference' "$log" | head -n 30 >&2 || true
    echo "   ... and its last lines:" >&2
    tail -n 10 "$log" >&2
    exit 1
  fi
}

# --- morse_cli ----------------------------------------------------------------
CLI="$CACHE/morse_cli-$CLI_REF"
fetch "$CLI_REPO" "$CLI_REF" "$CLI"
echo "-- building morse_cli"
git -C "$CLI" checkout -q -- .
sed -i 's/ -Werror$//' "$CLI/Makefile"
make -C "$CLI" clean >/dev/null 2>&1 || true
# nl80211 is the transport to a driver on this machine; the USB transport
# (for Morse's USB evaluation kits) would need libusb.
build morse_cli "$CACHE/morse_cli.log" \
  make -C "$CLI" -j"$J" CONFIG_MORSE_TRANS_NL80211=1 CONFIG_MORSE_USB=0 morse_cli

# --- wpa_supplicant_s1g -------------------------------------------------------
HOSTAP="$CACHE/hostap-$HOSTAP_REF"
fetch "$HOSTAP_REPO" "$HOSTAP_REF" "$HOSTAP"
echo "-- building wpa_supplicant_s1g (several minutes on a Pi)"
WS="$HOSTAP/wpa_supplicant"
git -C "$HOSTAP" checkout -q -- .
sed -i 's/^EXTRA_CFLAGS += -Wextra -Werror$/EXTRA_CFLAGS += -Wextra/' "$WS/Makefile"
# Morse's default configuration, with:
#   + 802.11s mesh
#   - MACsec and the wired driver (not Wi-Fi; MACsec needs extra headers)
#   - the D-Bus introspection stub (no D-Bus interface is built anyway)
sed -e 's/^#CONFIG_MESH=y$/CONFIG_MESH=y/' \
    -e 's/^\(CONFIG_MACSEC=y\)$/#\1/' \
    -e 's/^\(CONFIG_DRIVER_MACSEC_LINUX=y\)$/#\1/' \
    -e 's/^\(CONFIG_DRIVER_WIRED=y\)$/#\1/' \
    -e 's/^\(CONFIG_CTRL_IFACE_DBUS_INTRO=y\)$/#\1/' \
    "$WS/defconfig" > "$WS/.config"
grep -q '^CONFIG_MESH=y$' "$WS/.config" || { echo "!! could not enable CONFIG_MESH in Morse's defconfig" >&2; exit 1; }
make -C "$WS" clean >/dev/null 2>&1 || true
build wpa_supplicant_s1g "$CACHE/wpa_supplicant_s1g.log" \
  make -C "$WS" -j"$J" wpa_supplicant_s1g wpa_cli_s1g wpa_passphrase_s1g

# --- install ------------------------------------------------------------------
install -d "$BINDIR"
install -m 0755 "$CLI/morse_cli" "$WS/wpa_supplicant_s1g" "$WS/wpa_cli_s1g" "$WS/wpa_passphrase_s1g" "$BINDIR/"
echo "$CLI_REF $HOSTAP_REF" > "$STAMP"
echo "== OK: installed in $BINDIR: morse_cli wpa_supplicant_s1g wpa_cli_s1g wpa_passphrase_s1g"
"$BINDIR/wpa_supplicant_s1g" -v 2>/dev/null | head -n 1 | sed 's/^/   /' || true
"$BINDIR/morse_cli" --version 2>/dev/null | head -n 1 | sed 's/^/   /' || true
