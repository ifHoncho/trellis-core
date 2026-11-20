#!/bin/sh
# Install mt7915e, the driver for MediaTek MT7915 and MT7916 PCIe Wi-Fi, as a
# DKMS module on kernels that ship mt76 without it (Raspberry Pi OS is one:
# its kernel has "# CONFIG_MT7915E is not set"), and on kernels whose own
# mt7915e predates Linux 6.14: that driver registers both bands of an
# MT7916's 5/6 GHz radio at once, and the radio then works on neither. The
# build carries 6.14's fix, with its module option (enable_6ghz=1 for
# 6 GHz; 5 GHz otherwise). A kernel whose own driver has the fix is left
# alone. Run as root on a Debian-family system.
#
#   scripts/install-mt7915e-dkms.sh               Pi 4/CM4: PCIe link at Gen1
#   scripts/install-mt7915e-dkms.sh --pcie-gen2   Pi 4/CM4: keep Gen2
#
# On a BCM2711 board (Pi 4, CM4, Pi 400) with an MT7915/MT7916 card it also
# enables the overlays the card needs (see below), and by default limits the
# PCIe link to Gen1 (2.5 GT/s, 2 Gb/s): behind some M.2 adapters the card
# makes the kernel panic within seconds at Gen2. Gen1 is still more than the card moves
# in practice. --pcie-gen2 keeps (or restores) full speed; try it on a board
# with a native M.2 A+E slot.
#
# DKMS rebuilds it after kernel updates, each time from that kernel's own
# mt76 source (packaging/dkms-mt7915e/build-mt7915e.sh explains why), so a
# kernel update needs the network. The module lands in
# /lib/modules/<kernel>/updates/dkms/.
set -eu

PKG_VERSION=2   # bump when packaging/dkms-mt7915e changes
PCIE_GEN=1
for arg in "$@"; do
  case "$arg" in
    --pcie-gen2) PCIE_GEN=2 ;;
    --pcie-gen1) PCIE_GEN=1 ;;
    *) echo "usage: $0 [--pcie-gen2]" >&2; exit 2 ;;
  esac
done
NAME=trellis-mt7915e
SRC="/usr/src/${NAME}-${PKG_VERSION}"
HERE="$(cd "$(dirname "$0")" && pwd)"
KREL="$(uname -r)"
BUILD="/lib/modules/${KREL}/build"

if [ "$(id -u)" -ne 0 ]; then
  echo "run as root" >&2; exit 1
fi

echo "== Trellis: mt7915e via DKMS for kernel ${KREL}"

KARCH="$(uname -m)"
UARCH="$(dpkg --print-architecture 2>/dev/null || echo unknown)"
if [ "$KARCH" = aarch64 ] && [ "$UARCH" = armhf ]; then
  echo "!! The kernel is 64-bit (aarch64) but the operating system is 32-bit (armhf)," >&2
  echo "   so kernel modules cannot be built here. Install the 64-bit OS." >&2
  exit 1
fi

[ -r /proc/config.gz ] || modprobe configs 2>/dev/null || true
CFG=""
if [ -r /proc/config.gz ]; then CFG="$(zcat /proc/config.gz)"
elif [ -r "/boot/config-${KREL}" ]; then CFG="$(cat "/boot/config-${KREL}")"
elif [ -r "$BUILD/.config" ]; then CFG="$(cat "$BUILD/.config")"
fi
[ -n "$CFG" ] || { echo "!! cannot read this kernel's configuration" >&2; exit 1; }
has() { echo "$CFG" | grep -qE "^$1=[ym]"; }

# A kernel with an mt7915e of its own: fine when that one selects the band
# of a 5/6 GHz radio (Linux 6.14 on), replaced by this build when not.
if has CONFIG_MT7915E; then
  if echo "$CFG" | grep -q '^CONFIG_MT7915E=y'; then
    if [ -e /sys/module/mt7915e/parameters/enable_6ghz ]; then
      echo "-- this kernel has mt7915e built in, with band selection; nothing to do"
      exit 0
    fi
    echo "!! This kernel has an mt7915e older than Linux 6.14 built in: it cannot be replaced." >&2
    echo "   An MT7916 radio with 5 and 6 GHz works on neither with it; use a kernel that builds mt7915e as a module, or 6.14 or newer." >&2
    exit 1
  fi
  OWN="$(find "/lib/modules/${KREL}/kernel" -name 'mt7915e.ko*' 2>/dev/null | head -n 1)"
  if [ -n "$OWN" ] && modinfo -F parm "$OWN" 2>/dev/null | grep -q '^enable_6ghz:'; then
    echo "-- this kernel's mt7915e selects the band of a 5/6 GHz radio already; nothing to do"
    exit 0
  fi
  echo "-- this kernel's mt7915e predates Linux 6.14's band selection; building one with it, loaded in its place"
fi
missing=""
for opt in CONFIG_PCI CONFIG_MAC80211 CONFIG_MT76_CORE CONFIG_MT76_CONNAC_LIB CONFIG_RELAY; do
  has "$opt" || missing="$missing $opt"
done
if [ -n "$missing" ]; then
  echo "!! This kernel lacks what mt7915e builds on:$missing" >&2
  echo "   mt7915e can only be added to a kernel that has mt76 and mt76-connac-lib" >&2
  echo "   (for example because it builds mt7921e or mt7925e); this one needs a rebuilt kernel." >&2
  exit 1
fi

if ! [ -f "$BUILD/Makefile" ] && command -v apt-get >/dev/null 2>&1; then
  echo "-- no kernel headers for ${KREL}; trying to install them"
  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends "linux-headers-${KREL}" >/dev/null 2>&1 || true
fi
if ! [ -f "$BUILD/Makefile" ] || ! [ -f "$BUILD/Module.symvers" ]; then
  echo "!! No usable kernel headers at $BUILD (Makefile and Module.symvers needed)." >&2
  echo "   Install the headers package matching kernel ${KREL}, then rerun." >&2
  exit 1
fi

if command -v apt-get >/dev/null 2>&1; then
  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends dkms git build-essential >/dev/null
fi

# Firmware: mt7915e loads mediatek/mt7916_*.bin (or mt7915_*.bin); Debian
# and Raspberry Pi OS package them as firmware-mediatek (Debian 12 and
# earlier: firmware-misc-nonfree).
if ! ls /lib/firmware/mediatek/mt7916_wm.bin* >/dev/null 2>&1; then
  echo "-- MT7916 firmware missing; trying to install it"
  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends firmware-mediatek >/dev/null 2>&1 \
    || DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends firmware-misc-nonfree >/dev/null 2>&1 \
    || echo "!! could not install MediaTek firmware; the driver will build but the card will not start" >&2
fi

rm -rf "$SRC"
install -d "$SRC"
sed "s/@VERSION@/${PKG_VERSION}/" "$HERE/../packaging/dkms-mt7915e/dkms.conf" > "$SRC/dkms.conf"
install -m 0755 "$HERE/../packaging/dkms-mt7915e/build-mt7915e.sh" "$SRC/build-mt7915e.sh"
install -m 0755 "$HERE/../packaging/dkms-mt7915e/band-select.sh" "$SRC/band-select.sh"
install -m 0755 "$HERE/../packaging/dkms/trellis-build.sh" "$SRC/trellis-build.sh"

# Remove other versions of this package (see install-batman-adv-dkms.sh for
# the dkms status formats), and re-add this one so DKMS copies the new files.
dkms status 2>/dev/null | grep "^${NAME}[/,]" \
  | sed -E "s|^${NAME}[/,] *([^,:]+).*|\1|" | sort -u | while read -r v; do
  [ -n "$v" ] && dkms remove "${NAME}/${v}" --all || true
done

diagnose() {
  LOG="/var/lib/dkms/${NAME}/${PKG_VERSION}/build/make.log"
  echo "" >&2
  echo "!! DKMS build failed. Last lines of $LOG:" >&2
  tail -n 30 "$LOG" >&2 2>/dev/null || true
  if grep -qiE 'exec format error|cannot execute binary' "$LOG" 2>/dev/null; then
    echo "   Cause: the kernel headers contain tools built for another architecture." >&2
    echo "   Fix:   sudo scripts/repair-kernel-headers.sh   then rerun this script." >&2
  fi
  echo "   Please send the whole $LOG when reporting this." >&2
  exit 1
}

dkms add "${NAME}/${PKG_VERSION}"
dkms build "${NAME}/${PKG_VERSION}" -k "$KREL" || diagnose
dkms install --force "${NAME}/${PKG_VERSION}" -k "$KREL"
depmod -a

cards=""
reboot_needed=0
for dev in /sys/bus/pci/devices/*; do
  [ "$(cat "$dev/vendor" 2>/dev/null)" = 0x14c3 ] || continue
  case "$(cat "$dev/device")" in 0x7915|0x7906|0x790a) cards="$cards $dev" ;; esac
done

# BCM2711 (Pi 4, CM4, Pi 400): the PCIe controller puts RAM at bus address
# 0x4_0000_0000, out of reach of mt7915e's 32-bit DMA mask, and the probe
# fails with -ENOMEM (error -12). Raspberry Pi's pcie-32bit-dma overlay maps
# RAM below 4 GB on the bus instead. With 2 GB of RAM or less it costs
# nothing; above 3 GB the kernel bounces DMA through low memory.
if [ -n "$cards" ] && tr '\0' '\n' < /proc/device-tree/compatible 2>/dev/null | grep -qx 'brcm,bcm2711'; then
  CONFIG_TXT=/boot/firmware/config.txt
  [ -f "$CONFIG_TXT" ] || CONFIG_TXT=/boot/config.txt
  if ! grep -q '^dtoverlay=pcie-32bit-dma$' "$CONFIG_TXT"; then
    cp "$CONFIG_TXT" "$CONFIG_TXT.trellis-bak"
    printf '\n[all]\n# Trellis: MT7915/MT7916 Wi-Fi can only DMA to 32-bit addresses (install-mt7915e-dkms.sh)\ndtoverlay=pcie-32bit-dma\n' >> "$CONFIG_TXT"
    echo "-- added dtoverlay=pcie-32bit-dma to $CONFIG_TXT (backup: $CONFIG_TXT.trellis-bak)"
  fi
  # Not live until a reboot: this boot still maps RAM at 0x4_0000_0000.
  if dmesg 2>/dev/null | grep 'brcm-pcie' | grep 'IB MEM' | grep -q -- '-> 0x0400000000'; then
    reboot_needed=1
  fi

  # PCIe link speed: packaging/overlays/trellis-pcie-gen1-overlay.dts sets
  # max-link-speed on the controller, so the link trains at Gen1 from boot.
  OVERLAYS="$(dirname "$CONFIG_TXT")/overlays"
  LIVE_GEN="$(od -An -tu1 /proc/device-tree/scb/pcie@7d500000/max-link-speed 2>/dev/null | awk '{print $NF}')"
  if [ "$PCIE_GEN" = 1 ]; then
    if ! command -v dtc >/dev/null 2>&1 && command -v apt-get >/dev/null 2>&1; then
      DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends device-tree-compiler >/dev/null
    fi
    dtc -@ -q -I dts -O dtb -o "$OVERLAYS/trellis-pcie-gen1.dtbo" "$HERE/../packaging/overlays/trellis-pcie-gen1-overlay.dts"
    if grep -q '^#dtoverlay=trellis-pcie-gen1$' "$CONFIG_TXT"; then
      sed -i 's/^#dtoverlay=trellis-pcie-gen1$/dtoverlay=trellis-pcie-gen1/' "$CONFIG_TXT"
      echo "-- re-enabled dtoverlay=trellis-pcie-gen1 in $CONFIG_TXT"
    elif ! grep -q '^dtoverlay=trellis-pcie-gen1$' "$CONFIG_TXT"; then
      printf '# Trellis: PCIe at Gen1; MT7915/MT7916 cards can make the kernel panic at Gen2 behind M.2 adapters (install-mt7915e-dkms.sh --pcie-gen2 to undo)\ndtoverlay=trellis-pcie-gen1\n' >> "$CONFIG_TXT"
      echo "-- added dtoverlay=trellis-pcie-gen1 to $CONFIG_TXT"
    fi
    if [ "$LIVE_GEN" != 1 ]; then
      # Loading the driver on this Gen2 link could panic the kernel now.
      echo "== REBOOT NEEDED: the driver is installed but not loaded; it starts after a reboot, with the PCIe link at Gen1"
      exit 0
    fi
  elif grep -q '^dtoverlay=trellis-pcie-gen1$' "$CONFIG_TXT"; then
    sed -i 's/^dtoverlay=trellis-pcie-gen1$/#dtoverlay=trellis-pcie-gen1/' "$CONFIG_TXT"
    echo "-- disabled dtoverlay=trellis-pcie-gen1 in $CONFIG_TXT: the link runs at Gen2 after a reboot"
  fi
fi

# An mt7915e already loaded is another build (the kernel's own, or an
# earlier one of this package): it gives way to the new one.
if [ -d /sys/module/mt7915e ]; then
  NEW="$(modinfo -F srcversion mt7915e 2>/dev/null || true)"
  CUR="$(cat /sys/module/mt7915e/srcversion 2>/dev/null || true)"
  if [ -n "$NEW" ] && [ "$NEW" != "$CUR" ]; then
    echo "-- unloading the mt7915e in use, to load the new build"
    if ! modprobe -r mt7915e 2>/dev/null; then
      echo "== REBOOT NEEDED: the new mt7915e is installed; the one in use could not be unloaded, so it loads at the next boot"
      exit 0
    fi
  fi
fi
echo "-- loading mt7915e"
modprobe mt7915e
echo "-- module file: $(modinfo -n mt7915e)"

# Which MT7915/MT7916 cards the driver took.
found=0
for dev in $cards; do
  found=1
  drv="$(basename "$(readlink "$dev/driver" 2>/dev/null)" 2>/dev/null || true)"
  if [ "$drv" = mt7915e ]; then
    echo "== OK: $(basename "$dev") is driven by mt7915e: $(ls "$dev/ieee80211" 2>/dev/null | tr '\n' ' ')"
  elif [ "$reboot_needed" = 1 ]; then
    echo "== REBOOT NEEDED: the driver is installed; $(basename "$dev") starts after a reboot, once pcie-32bit-dma is live"
    exit 0
  else
    echo "!! $(basename "$dev") is not bound to mt7915e (driver: ${drv:-none}); see: dmesg | grep -i mt79" >&2
    exit 1
  fi
done
[ "$found" = 1 ] || echo "-- no MT7915/MT7916 card found; the driver is installed for when one is fitted"
