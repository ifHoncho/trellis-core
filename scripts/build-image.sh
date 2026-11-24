#!/bin/sh
# Build a Trellis image for a board: Raspberry Pi OS Lite with the trellis
# packages, the board's drivers built for its kernel, and its overlays. A
# HaLow board's image carries Morse's firmware, never the BCF. The team's
# mesh profile and the BCF are not in it: they go on the card when it is
# flashed, and trellis firstboot installs them (internal/firstboot).
#
#   sudo scripts/build-image.sh BOARD DEB_DIR [OUT_DIR]
#
#   BOARD     a file in packaging/image/boards/ (without .conf): cm4-reference
#   DEB_DIR   holds the arm64 trellis package and trellis-batman-adv-dkms
#             (a signed release's, or make deb's)
#   OUT_DIR   default dist/image: <name>.img.xz, .img.xz.sha256 and
#             .manifest.txt (the image's packages)
#
# Runs as root on an arm64 Linux machine with the network (GitHub's arm64
# runners in CI), never under emulation: the image's own compiler
# builds its drivers in a chroot. Needs curl, xz, losetup, sfdisk, parted,
# e2fsck and resize2fs (e2fsprogs 1.47 or newer). Takes 30-60 minutes.
#
# Environment:
#   IMAGE_CACHE        where base images are kept (default dist/cache)
#   IMAGE_WORK         scratch space, about 8 GB (default /var/tmp)
#   SOURCE_DATE_EPOCH  the build time recorded in the image (default: now)
#   IMAGE_XZ=0         leave the image uncompressed (.img): for a build host
#                      that cannot take a long all-core compression
#   KEEP_WORK=1        keep the scratch directory (the uncompressed image);
#                      kept anyway when the build fails
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/.." && pwd)"
say() { echo "== $*"; }
die() { echo "!! $*" >&2; exit 1; }

[ $# -ge 2 ] && [ $# -le 3 ] || die "usage: sudo $0 BOARD DEB_DIR [OUT_DIR]"
BOARD="$1"
DEBS="$(cd "$2" && pwd)"
OUT="${3:-$REPO/dist/image}"
CONF="$REPO/packaging/image/boards/$BOARD.conf"
[ -f "$CONF" ] || die "no board $BOARD (packaging/image/boards/)"
# shellcheck source=/dev/null
. "$CONF"

[ "$(id -u)" -eq 0 ] || die "run as root"
[ "$(uname -m)" = aarch64 ] || die "run on an arm64 machine: the image's drivers are built in it, natively"
for t in curl xz losetup sfdisk partx parted e2fsck resize2fs dumpe2fs chroot sha256sum; do
  command -v "$t" >/dev/null 2>&1 || die "$t is missing"
done
# Debian 13 images use ext4's orphan_file feature, which e2fsck reads only
# from e2fsprogs 1.47 (Ubuntu 24.04 and later; 22.04 has 1.46).
e2v="$(e2fsck -V 2>&1 | sed -n 's/^e2fsck \([0-9]*\.[0-9]*\).*/\1/p' | head -1)"
printf '1.47\n%s\n' "$e2v" | sort -V -C || die "e2fsprogs $e2v is too old for the image's root filesystem: 1.47 or newer needed (Ubuntu 24.04, Debian 13)"

TRELLIS_DEB="$(ls "$DEBS"/trellis_*_arm64.deb 2>/dev/null || true)"
BATADV_DEB="$(ls "$DEBS"/trellis-batman-adv-dkms_*.deb 2>/dev/null || true)"
[ "$(echo "$TRELLIS_DEB" | grep -c .)" = 1 ] || die "$DEBS must hold one trellis_*_arm64.deb"
[ "$(echo "$BATADV_DEB" | grep -c .)" = 1 ] || die "$DEBS must hold one trellis-batman-adv-dkms_*.deb"
VERSION="$(basename "$TRELLIS_DEB" | sed -n 's/^trellis_\(.*\)_arm64\.deb$/\1/p')"
IMAGE_NAME="trellis-$VERSION-$BOARD"
SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-$(date +%s)}"

CACHE="${IMAGE_CACHE:-$REPO/dist/cache}"
mkdir -p "$CACHE" "$OUT"
WORK="$(mktemp -d "${IMAGE_WORK:-/var/tmp}/trellis-image.XXXXXX")"
IMG="$WORK/$IMAGE_NAME.img"
R="$WORK/root"
LOOP=""
RESOLV_SAVED=""

cleanup() {
  rc=$?
  set +e
  if mountpoint -q "$R" 2>/dev/null; then
    [ -z "$RESOLV_SAVED" ] || { rm -f "$R/etc/resolv.conf"; mv "$R/etc/resolv.conf.trellis-image" "$R/etc/resolv.conf"; }
    umount -R "$R" 2>/dev/null || umount -lR "$R"
  fi
  [ -z "$LOOP" ] || losetup -d "$LOOP"
  if [ "${KEEP_WORK:-}" = 1 ] || [ "$rc" -ne 0 ]; then echo "-- scratch kept: $WORK"; else rm -rf "$WORK"; fi
}
trap cleanup EXIT
trap 'exit 130' INT TERM

say "$IMAGE_NAME: $BOARD_DESC"

# --- The base image ------------------------------------------------------------
BASE="$CACHE/$(basename "$BASE_URL")"
if ! [ -f "$BASE" ]; then
  say "fetching $(basename "$BASE_URL")"
  curl -fSL --retry 3 -o "$BASE.part" "$BASE_URL"
  mv "$BASE.part" "$BASE"
fi
say "unpacking and checking the base image"
xz -dc "$BASE" > "$IMG"
got="$(sha256sum "$IMG" | cut -d' ' -f1)"
[ "$got" = "$BASE_IMG_SHA256" ] || die "base image SHA-256 $got, not the board's $BASE_IMG_SHA256 (rm $BASE to fetch it again)"

# --- Room to build in ------------------------------------------------------------
truncate -s "+${GROW_MB}M" "$IMG"
parted -s "$IMG" resizepart 2 100%
LOOP="$(losetup -fP --show "$IMG")"
e2fsck -fy "${LOOP}p2" >/dev/null || [ $? -le 1 ] || die "e2fsck failed on the root filesystem"
resize2fs "${LOOP}p2" >/dev/null

mkdir -p "$R"
mount "${LOOP}p2" "$R"
mount "${LOOP}p1" "$R/boot/firmware"
mount --bind /dev "$R/dev"
mount --bind /dev/pts "$R/dev/pts"
mount -t proc proc "$R/proc"
mount -t sysfs sysfs "$R/sys"
# An empty /run: nothing in the chroot believes systemd is running.
mount -t tmpfs tmpfs "$R/run"
mount -t tmpfs tmpfs "$R/tmp"
# This machine's resolver while the image is built; the image's own back after.
if [ -e "$R/etc/resolv.conf" ] || [ -L "$R/etc/resolv.conf" ]; then
  mv "$R/etc/resolv.conf" "$R/etc/resolv.conf.trellis-image"
  RESOLV_SAVED=1
fi
cat /etc/resolv.conf > "$R/etc/resolv.conf"

# --- Inside the image ------------------------------------------------------------
W="$R/tmp/trellis-image"
mkdir -p "$W/debs"
cp "$TRELLIS_DEB" "$BATADV_DEB" "$W/debs/"
cp "$CONF" "$W/board.conf"
cp "$REPO/packaging/image/customize.sh" "$W/customize.sh"
say "customizing (packages, drivers for the image's kernel, boot partition)"
chroot "$R" /usr/bin/env -i PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
  HOME=/root LANG=C.UTF-8 IMAGE_NAME="$IMAGE_NAME" IMAGE_BOARD="$BOARD" TRELLIS_VERSION="$VERSION" \
  SOURCE_DATE_EPOCH="$SOURCE_DATE_EPOCH" /bin/sh /tmp/trellis-image/customize.sh
cp "$W/manifest.txt" "$WORK/manifest.txt"

rm -f "$R/etc/resolv.conf"
if [ -n "$RESOLV_SAVED" ]; then
  mv "$R/etc/resolv.conf.trellis-image" "$R/etc/resolv.conf"
  RESOLV_SAVED=""
fi

# Free space zeroed, so it compresses to nothing.
say "zeroing free space"
for d in "$R" "$R/boot/firmware"; do
  dd if=/dev/zero of="$d/.trellis-zero" bs=1M 2>/dev/null || true
  rm -f "$d/.trellis-zero"
done
sync
umount -R "$R"

# --- Back to size ------------------------------------------------------------
# The smallest root filesystem plus 256 MB; Raspberry Pi OS grows it to
# fill the card at the first boot.
say "shrinking"
e2fsck -fy "${LOOP}p2" >/dev/null || [ $? -le 1 ] || die "e2fsck failed after customizing"
bs="$(dumpe2fs -h "${LOOP}p2" 2>/dev/null | sed -n 's/^Block size: *//p')"
min="$(resize2fs -P "${LOOP}p2" 2>/dev/null | sed -n 's/.*: *\([0-9]*\)$/\1/p')"
[ -n "$bs" ] && [ -n "$min" ] || die "cannot read the root filesystem's size"
blocks=$((min + 256 * 1024 * 1024 / bs))
resize2fs "${LOOP}p2" "$blocks" >/dev/null
losetup -d "$LOOP"
LOOP=""
start="$(partx -g -n 2 -o START "$IMG" | tr -d ' ')"
[ -n "$start" ] || die "cannot read partition 2's start"
sectors=$((blocks * bs / 512))
echo "$start,$sectors" | sfdisk -N 2 --no-reread -q "$IMG" >/dev/null
truncate -s $(((start + sectors) * 512)) "$IMG"

EXT=img.xz
if [ "${IMAGE_XZ:-1}" = 0 ]; then
  EXT=img
  mv "$IMG" "$OUT/$IMAGE_NAME.img"
else
  say "compressing"
  xz -T0 -6 -c "$IMG" > "$OUT/$IMAGE_NAME.img.xz.part"
  mv "$OUT/$IMAGE_NAME.img.xz.part" "$OUT/$IMAGE_NAME.img.xz"
fi
(cd "$OUT" && sha256sum "$IMAGE_NAME.$EXT" > "$IMAGE_NAME.$EXT.sha256")
{
  echo "# $IMAGE_NAME: $BOARD_DESC"
  echo "# base: $(basename "$BASE_URL") (img sha256 $BASE_IMG_SHA256)"
  cat "$WORK/manifest.txt"
} > "$OUT/$IMAGE_NAME.manifest.txt"
say "done: $OUT/$IMAGE_NAME.$EXT ($(du -h "$OUT/$IMAGE_NAME.$EXT" | cut -f1))"
