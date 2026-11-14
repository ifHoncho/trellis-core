#!/bin/sh
# Build Morse Micro's Wi-Fi HaLow driver (morse.ko) for one kernel, with SPI
# as the host interface. Run by DKMS (see dkms.conf), for each kernel it
# builds for:
#
#   build-morse.sh KERNEL_VERSION KERNEL_BUILD_DIR
#
# Source: github.com/MorseMicro/morse_driver at the commit pinned below
# (release mm6108-2.2.2, 2026-09-23) and its rate-control submodule, fetched
# once into /var/cache/trellis/morse/<commit>. Commits are pinned by hash, so
# a moved tag can't change what is built. The firmware that
# install-wm6108-halow.sh installs must be from the same release line (2.2).
#
# Patches (patches/, applied in order):
#   0001  The chip enters SPI mode on a burst of clocks sent with chip select
#         high. Morse does that through a patch to the kernel's SPI core; a
#         stock kernel ignores the driver's request and sends the burst with
#         the chip selected, and the probe fails ("failed to init SPI with
#         CMD63"). The patch asks for SPI_NO_CS during the burst instead,
#         which every kernel honours.
#   0002  Drop -Werror: a newer compiler's warnings must not stop DKMS from
#         rebuilding the driver after a kernel update.
set -eu

KVER="$1"
KDIR="$2"
HERE="$(cd "$(dirname "$0")" && pwd)"
CACHE="${TRELLIS_MORSE_CACHE:-/var/cache/trellis/morse}"
DRIVER_REPO="${TRELLIS_MORSE_DRIVER_REPO:-https://github.com/MorseMicro/morse_driver.git}"
DRIVER_REF=b566725bd000aa1854ab406bae786d36014839c2   # tag mm6108-2.2.2
MMRC_REPO="${TRELLIS_MORSE_MMRC_REPO:-https://github.com/MorseMicro/mm_rate_control.git}"
MMRC_REF=32646409c58ca2b143d430aef3e43823e4420cde     # morse_driver's mmrc-submodule at that tag

say() { echo "trellis-morse: $*"; }
die() { echo "trellis-morse: $*" >&2; exit 1; }

# fetch REPO COMMIT DIR: a shallow checkout of exactly that commit.
fetch() {
  rm -rf "$3"
  mkdir -p "$3"
  git -C "$3" init -q
  git -C "$3" remote add origin "$1"
  git -C "$3" fetch -q --depth 1 origin "$2" || die "cannot fetch $2 from $1 (is the network up?)"
  git -C "$3" -c advice.detachedHead=false checkout -q FETCH_HEAD
  [ "$(git -C "$3" rev-parse HEAD)" = "$2" ] || die "$1: fetched commit is not $2"
}

SRC="$CACHE/$DRIVER_REF"
if ! [ -f "$SRC/.trellis-complete" ]; then
  say "fetching the driver source ($DRIVER_REF)"
  fetch "$DRIVER_REPO" "$DRIVER_REF" "$SRC"
  rm -rf "$SRC/mmrc-submodule"
  fetch "$MMRC_REPO" "$MMRC_REF" "$SRC/mmrc-submodule"
  touch "$SRC/.trellis-complete"
fi

cd "$HERE"
rm -rf src out
mkdir -p src out
# The source without its git metadata: patches are applied to a plain tree.
(cd "$SRC" && tar -cf - --exclude=.git --exclude=.trellis-complete .) | (cd src && tar -xf -)
[ -f src/spi.c ] && [ -f src/mmrc-submodule/src/core/mmrc.c ] || die "the fetched source is incomplete"
for p in "$HERE"/patches/*.patch; do
  [ -f "$p" ] || die "no patches beside $0"
  say "applying $(basename "$p")"
  (cd src && git apply -p1 "$p") || die "$(basename "$p") does not apply to $DRIVER_REF"
done

# Same compiler major version as the kernel (see trellis-build.sh).
CC_SEL="$(sh "$HERE/trellis-build.sh" --print-cc "$KDIR")"
cpus="$(nproc 2>/dev/null || echo 1)"
mem_mb="$(awk '/^MemAvailable:/ {print int($2/1024)}' /proc/meminfo 2>/dev/null || echo 0)"
J=$((mem_mb / 350)); [ "$J" -lt 1 ] && J=1; [ "$J" -gt "$cpus" ] && J="$cpus"

say "building morse.ko for $KVER with ${CC_SEL:-gcc}, $J job(s)"
# SPI only. USER_ACCESS and VENDOR_COMMAND are what Morse's tools (morse_cli,
# wpa_supplicant_s1g) talk to. The country is set when the module loads
# (modprobe.d), not here.
set -- -j"$J" -C "$KDIR" M="$HERE/src" \
  KERNEL_SRC="$KDIR" MORSE_TRACE_PATH="$HERE/src" \
  CONFIG_WLAN_VENDOR_MORSE=m CONFIG_MORSE_SPI=y \
  CONFIG_MORSE_USER_ACCESS=y CONFIG_MORSE_VENDOR_COMMAND=y
[ -n "$CC_SEL" ] && set -- "$@" CC="$CC_SEL"
make "$@" modules
cp src/morse.ko out/
say "built out/morse.ko"
