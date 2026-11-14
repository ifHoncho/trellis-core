#!/bin/sh
# Build mt7915e for one kernel from that kernel's own mt76 source. Run by
# DKMS (see dkms.conf), for each kernel it builds for:
#
#   build-mt7915e.sh KERNEL_VERSION KERNEL_BUILD_DIR
#
# Where the source comes from:
#   - Raspberry Pi OS kernels name their raspberrypi/linux commit in
#     /usr/share/doc/linux-image-<version>/changelog.Debian.gz
#     ("Linux commit: <sha>"); that commit is fetched.
#   - Otherwise the upstream stable tag for the version (v6.18.60) is used,
#     with a warning: a distribution kernel may carry its own mt76 patches.
#
# Only drivers/net/wireless/mediatek/mt76 is downloaded (about 3.5 MB), into
# /var/cache/trellis/mt76/<commit or tag>. Building needs the network the
# first time a kernel is seen, which is normally the case right after apt
# installs it.
set -eu

KVER="$1"
KDIR="$2"
HERE="$(cd "$(dirname "$0")" && pwd)"
CACHE="${TRELLIS_MT76_CACHE:-/var/cache/trellis/mt76}"
MT76=drivers/net/wireless/mediatek/mt76
RPI_REPO="${TRELLIS_RPI_LINUX_REPO:-https://github.com/raspberrypi/linux.git}"
STABLE_REPO="${TRELLIS_STABLE_LINUX_REPO:-https://git.kernel.org/pub/scm/linux/kernel/git/stable/linux.git}"

say() { echo "trellis-mt7915e: $*"; }
die() { echo "trellis-mt7915e: $*" >&2; exit 1; }

# x.y.z from the kernel release ("6.18.60+rpt-rpi-v8" -> 6.18.60).
NUMVER="$(echo "$KVER" | sed -n 's/^\([0-9][0-9]*\.[0-9][0-9]*\(\.[0-9][0-9]*\)\{0,1\}\).*/\1/p')"
[ -n "$NUMVER" ] || die "cannot read a version number from kernel release $KVER"

URL="" REF=""
CL="/usr/share/doc/linux-image-$KVER/changelog.Debian.gz"
if [ -r "$CL" ]; then
  # First changelog entry only: the lines up to its " -- maintainer" line.
  REF="$(zcat "$CL" | sed -n '1,/^ -- /s/^ *\* Linux commit: \([0-9a-f]\{40\}\) *$/\1/p' | head -1)"
  [ -n "$REF" ] && URL="$RPI_REPO"
fi
if [ -z "$REF" ]; then
  case "$NUMVER" in
    *.*.0) REF="v${NUMVER%.0}" ;;
    *) REF="v$NUMVER" ;;
  esac
  URL="$STABLE_REPO"
  say "WARNING: $KVER does not say which source it was built from; using upstream $REF."
  say "         If this distribution patches mt76, the driver may not match the kernel's mt76.ko."
fi

SRC="$CACHE/$REF"
if ! [ -f "$SRC/.trellis-complete" ]; then
  say "fetching $MT76 from $URL at $REF"
  rm -rf "$SRC"
  mkdir -p "$SRC"
  git -C "$SRC" init -q
  git -C "$SRC" remote add origin "$URL"
  case "$REF" in
    v*) FETCH="refs/tags/$REF" ;;
    *) FETCH="$REF" ;;
  esac
  git -C "$SRC" fetch -q --depth 1 --filter=blob:none origin "$FETCH" \
    || die "cannot fetch $REF from $URL (is the network up?)"
  git -C "$SRC" sparse-checkout set --no-cone "/$MT76/" /Makefile >/dev/null
  git -C "$SRC" checkout -q FETCH_HEAD
  touch "$SRC/.trellis-complete"
fi

# The fetched tree must be the kernel's version.
TREEVER="$(awk -F' = ' '$1=="VERSION"{v=$2} $1=="PATCHLEVEL"{p=$2} $1=="SUBLEVEL"{s=$2} END{print v"."p"."s}' "$SRC/Makefile")"
case "$NUMVER" in *.*.*) want="$NUMVER" ;; *) want="$NUMVER.0" ;; esac
[ "$TREEVER" = "$want" ] || die "source at $REF is Linux $TREEVER, but the kernel is $want; refusing to build a mismatched driver"

cd "$HERE"
rm -rf src out
mkdir -p src out
cp -r "$SRC/$MT76/." src/
[ -f src/mt7915/Makefile ] || die "$REF has no mt7915 driver"

# Same compiler major version as the kernel (see trellis-build.sh).
CC_SEL="$(sh "$HERE/trellis-build.sh" --print-cc "$KDIR")"
cpus="$(nproc 2>/dev/null || echo 1)"
mem_mb="$(awk '/^MemAvailable:/ {print int($2/1024)}' /proc/meminfo 2>/dev/null || echo 0)"
J=$((mem_mb / 350)); [ "$J" -lt 1 ] && J=1; [ "$J" -gt "$cpus" ] && J="$cpus"

say "building mt7915e for $KVER from $REF with ${CC_SEL:-gcc}, $J job(s)"
# Only the mt7915 directory is built; mt76.ko and mt76-connac-lib.ko are the
# kernel's own, and modpost resolves their symbols from its Module.symvers.
if [ -n "$CC_SEL" ]; then
  make -j"$J" -C "$KDIR" M="$HERE/src/mt7915" CONFIG_MT7915E=m CC="$CC_SEL" modules
else
  make -j"$J" -C "$KDIR" M="$HERE/src/mt7915" CONFIG_MT7915E=m modules
fi
cp src/mt7915/mt7915e.ko out/
say "built out/mt7915e.ko"
