#!/bin/sh
# Bring up a Seeed Wio-WM6108 Wi-Fi HaLow module (Quectel FGH100M-H, Morse
# Micro MM6108, SPI host interface) fitted in a Seeed WM1302 Pi HAT, on a
# Raspberry Pi 3, 4 or CM4 running a stock Raspberry Pi OS kernel. Run as
# root on the node; safe to run again.
#
#   scripts/install-wm6108-halow.sh [--country CC] [--spi-hz N]
#
#   --country CC   regulatory country for the Morse driver (default US: the
#                  Wio-WM6108 is a 902-928 MHz, US-only module)
#   --spi-hz N     SPI clock in Hz (default: the overlay's 20000000). 15 to
#                  31 MHz work and 42 MHz and up do not;
#                  below 20 MHz the script also widens the driver's gap
#                  between data blocks, which the chip needs (see the overlay)
#
# What it does:
#   1. Driver: builds Morse Micro's out-of-tree driver for SPI with DKMS
#      (packaging/dkms-morse/; pinned source, two Trellis patches). DKMS
#      rebuilds it after kernel updates.
#   2. Firmware: mm6108.bin and the module's board configuration file (BCF,
#      its RF calibration and power tables) into /lib/firmware/morse/, from
#      github.com/MorseMicro/morse-firmware at a pinned commit, checked
#      against the hashes below.
#   3. Module options: /etc/modprobe.d/trellis-morse.conf.
#   4. Keeps NetworkManager off the radio (a udev rule).
#   5. Device tree: packaging/overlays/trellis-wm6108-spi-overlay.dts tells
#      the kernel where the chip is (an SPI device can't be discovered), and
#      a config.txt line enables it. This needs one reboot.
#
# What it does not do: Morse's wpa_supplicant_s1g and morse_cli (see
# install-halow-tools.sh), and anything in meshd. The driver registers the
# radio's channels as 5 GHz ones (Morse's design; its tools translate);
# meshd's hardware database knows the radio is HaLow, and meshd runs it on a
# profile's s1g link.
#
# FIT THE ANTENNA FIRST. Transmitting into an open connector can damage the
# module's front end.
#
# To undo: comment out "dtoverlay=trellis-wm6108-spi" in config.txt and
# reboot; "dkms remove trellis-morse/<version> --all" removes the driver.
set -eu

PKG_VERSION=1   # bump when packaging/dkms-morse changes
NAME=trellis-morse
COUNTRY=US
SPI_HZ=""
while [ $# -gt 0 ]; do
  case "$1" in
    --country) COUNTRY="${2:-}"; shift 2 ;;
    --spi-hz) SPI_HZ="${2:-}"; shift 2 ;;
    *) echo "usage: $0 [--country CC] [--spi-hz N]" >&2; exit 2 ;;
  esac
done
echo "$COUNTRY" | grep -qE '^[A-Z]{2}$' || { echo "--country takes a two-letter code such as US" >&2; exit 2; }
if [ -n "$SPI_HZ" ]; then
  echo "$SPI_HZ" | grep -qE '^[0-9]+$' && [ "$SPI_HZ" -ge 1000000 ] && [ "$SPI_HZ" -le 50000000 ] \
    || { echo "--spi-hz takes 1000000 to 50000000" >&2; exit 2; }
fi

# Firmware and BCF: Morse Micro's 2.2 release line, matching the driver
# (packaging/dkms-morse/build-morse.sh, mm6108-2.2.2).
# Pinned in packaging/dkms-morse/firmware.pins, which trellis firstboot
# reads too (the files an image's card carries).
. "$(cd "$(dirname "$0")" && pwd)/../packaging/dkms-morse/firmware.pins"
FW_REPO="${TRELLIS_MORSE_FIRMWARE_REPO:-$FW_REPO}"
FW_CACHE="${TRELLIS_MORSE_FIRMWARE_CACHE:-/var/cache/trellis/morse-firmware}"
FW_DIR=/lib/firmware/morse

SRC="/usr/src/${NAME}-${PKG_VERSION}"
HERE="$(cd "$(dirname "$0")" && pwd)"
PKG="$HERE/../packaging"
# TRELLIS_IMAGE_KERNEL=<kernel release>: building an image
# (scripts/build-image.sh) in a chroot. That kernel's configuration and
# headers are used; this machine's kernel, hardware and modules are left
# alone, and nothing is loaded.
IMAGE_KERNEL="${TRELLIS_IMAGE_KERNEL:-}"
KREL="${IMAGE_KERNEL:-$(uname -r)}"
BUILD="/lib/modules/${KREL}/build"
OVERLAY=trellis-wm6108-spi
# The chip's node once the overlay is live.
DT_NODE=""
for n in /proc/device-tree/soc/spi@*/mm6108@0; do [ -d "$n" ] && DT_NODE="$n"; done

if [ "$(id -u)" -ne 0 ]; then
  echo "run as root" >&2; exit 1
fi

echo "== Trellis: Wio-WM6108 HaLow (Morse MM6108 over SPI) for kernel ${KREL}"

KARCH="$(uname -m)"
UARCH="$(dpkg --print-architecture 2>/dev/null || echo unknown)"
if [ "$KARCH" = aarch64 ] && [ "$UARCH" = armhf ]; then
  echo "!! The kernel is 64-bit (aarch64) but the operating system is 32-bit (armhf)," >&2
  echo "   so kernel modules cannot be built here. Install the 64-bit OS." >&2
  exit 1
fi

# The overlay is written for the BCM283x/BCM2711 GPIO controller. An
# image's board is known from its board file instead.
if [ -z "$IMAGE_KERNEL" ]; then
  compat="$(tr '\0' '\n' < /proc/device-tree/compatible 2>/dev/null || true)"
  if echo "$compat" | grep -qx 'brcm,bcm2712'; then
    echo "!! This is a Raspberry Pi 5 family board. Its header pins belong to the RP1" >&2
    echo "   chip, which this overlay does not describe; not supported yet." >&2
    exit 1
  fi
  if ! echo "$compat" | grep -qxE 'brcm,bcm2711|brcm,bcm2837|brcm,bcm2836|brcm,bcm2835'; then
    echo "!! Not a Raspberry Pi with a BCM283x/BCM2711 SoC; this script only knows the" >&2
    echo "   WM1302 Pi HAT's wiring on those boards." >&2
    exit 1
  fi
  CONFIG_TXT=/boot/firmware/config.txt
  [ -f "$CONFIG_TXT" ] || CONFIG_TXT=/boot/config.txt
  [ -f "$CONFIG_TXT" ] || { echo "!! no config.txt under /boot/firmware or /boot" >&2; exit 1; }
  OVERLAYS="$(dirname "$CONFIG_TXT")/overlays"
fi

[ -n "$IMAGE_KERNEL" ] || [ -r /proc/config.gz ] || modprobe configs 2>/dev/null || true
CFG=""
if [ -z "$IMAGE_KERNEL" ] && [ -r /proc/config.gz ]; then CFG="$(zcat /proc/config.gz)"
elif [ -r "/boot/config-${KREL}" ]; then CFG="$(cat "/boot/config-${KREL}")"
elif [ -r "$BUILD/.config" ]; then CFG="$(cat "$BUILD/.config")"
fi
[ -n "$CFG" ] || { echo "!! cannot read this kernel's configuration" >&2; exit 1; }
has() { echo "$CFG" | grep -qE "^$1=[ym]"; }

missing=""
for opt in CONFIG_MAC80211 CONFIG_MAC80211_MESH CONFIG_SPI_BCM2835 CONFIG_OF_GPIO; do
  has "$opt" || missing="$missing $opt"
done
# The driver uses the numbered GPIO calls, which newer kernels make optional.
if echo "$CFG" | grep -q '^# CONFIG_GPIOLIB_LEGACY is not set'; then
  missing="$missing CONFIG_GPIOLIB_LEGACY"
fi
if [ -n "$missing" ]; then
  echo "!! This kernel lacks what the Morse driver builds on:$missing" >&2
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
  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends dkms git build-essential device-tree-compiler >/dev/null
fi

# --- 2. Firmware and BCF (before the driver, which loads them at probe) ----
sha_ok() { [ "$(sha256sum "$1" 2>/dev/null | cut -d' ' -f1)" = "$2" ]; }
FW_NAME="$(basename "$FW_FILE")"
BCF_NAME="$(basename "$BCF_FILE")"
if [ -n "$IMAGE_KERNEL" ]; then
  # An image carries the firmware (its build adds it), never the BCF:
  # that comes from the card, the uplink or another node.
  echo "-- firmware: the image build adds it; BCF: card, uplink or a mesh node"
elif sha_ok "$FW_DIR/$FW_NAME" "$FW_SHA256" && sha_ok "$FW_DIR/$BCF_NAME" "$BCF_SHA256"; then
  echo "-- firmware and BCF already installed in $FW_DIR"
else
  FWSRC="$FW_CACHE/$FW_REF"
  if ! [ -f "$FWSRC/.trellis-complete" ]; then
    echo "-- fetching Morse Micro firmware ($FW_REF)"
    rm -rf "$FWSRC"
    mkdir -p "$FWSRC"
    git -C "$FWSRC" init -q
    git -C "$FWSRC" remote add origin "$FW_REPO"
    git -C "$FWSRC" fetch -q --depth 1 origin "$FW_REF" \
      || { echo "!! cannot fetch $FW_REF from $FW_REPO (is the network up?)" >&2; exit 1; }
    git -C "$FWSRC" -c advice.detachedHead=false checkout -q FETCH_HEAD
    touch "$FWSRC/.trellis-complete"
  fi
  sha_ok "$FWSRC/$FW_FILE" "$FW_SHA256" || { echo "!! $FW_FILE does not have the expected SHA-256; not installing it" >&2; exit 1; }
  sha_ok "$FWSRC/$BCF_FILE" "$BCF_SHA256" || { echo "!! $BCF_FILE does not have the expected SHA-256; not installing it" >&2; exit 1; }
  install -d "$FW_DIR"
  install -m 0644 "$FWSRC/$FW_FILE" "$FWSRC/$BCF_FILE" "$FW_DIR/"
  [ -f "$FWSRC/firmware/LICENSE" ] && install -m 0644 "$FWSRC/firmware/LICENSE" "$FW_DIR/LICENSE"
  echo "-- installed $FW_NAME and $BCF_NAME in $FW_DIR"
fi

# --- 1. Driver ---------------------------------------------------------------
rm -rf "$SRC"
install -d "$SRC/patches"
sed "s/@VERSION@/${PKG_VERSION}/" "$PKG/dkms-morse/dkms.conf" > "$SRC/dkms.conf"
install -m 0755 "$PKG/dkms-morse/build-morse.sh" "$SRC/build-morse.sh"
install -m 0755 "$PKG/dkms/trellis-build.sh" "$SRC/trellis-build.sh"
install -m 0644 "$PKG"/dkms-morse/patches/*.patch "$SRC/patches/"

# Remove other versions of this package (see install-batman-adv-dkms.sh for
# the dkms status formats), and re-add this one so DKMS copies the new files.
dkms status 2>/dev/null | grep "^${NAME}[/,]" \
  | sed -E "s|^${NAME}[/,] *([^,:]+).*|\1|" | sort -u | while read -r v; do
  [ -n "$v" ] && dkms remove "${NAME}/${v}" --all || true
done

diagnose() {
  LOG="/var/lib/dkms/${NAME}/${PKG_VERSION}/build/make.log"
  echo "" >&2
  echo "!! DKMS build failed. The errors in $LOG:" >&2
  grep -nE 'error|Error|\*\*\*|trellis-morse:' "$LOG" 2>/dev/null | head -n 40 >&2 || true
  echo "   ... and its last lines:" >&2
  tail -n 15 "$LOG" >&2 2>/dev/null || true
  echo "   Please send the whole $LOG when reporting this." >&2
  exit 1
}

echo "-- building the driver (a few minutes on a Pi)"
dkms add "${NAME}/${PKG_VERSION}"
dkms build "${NAME}/${PKG_VERSION}" -k "$KREL" || diagnose
dkms install --force "${NAME}/${PKG_VERSION}" -k "$KREL"
depmod -a "$KREL"
echo "-- module file: $(modinfo -k "$KREL" -n morse)"

# --- 3. Module options -------------------------------------------------------
# - bcf: the chip on this module has no board type programmed, so the driver
#   can't pick the BCF itself; without one it uses a "failsafe" that doesn't
#   transmit usefully.
# - enable_ps=0: a mesh node never sleeps, and this takes the wake and busy
#   lines out of the picture. (The driver logs that this option "must only be
#   used for testing"; that is expected.)
# - macaddr_suffix: without a MAC address in the chip, the driver makes up a
#   new one at every load; meshd identifies radios by MAC. Derived once from
#   the machine id, and ignored by the driver when the chip has its own.
MODCONF=/etc/modprobe.d/trellis-morse.conf
suffix="$(sed -n 's/.*macaddr_suffix=\([0-9a-f:]*\).*/\1/p' "$MODCONF" 2>/dev/null | head -1)"
if [ -n "$IMAGE_KERNEL" ]; then
  # Every node of an image would share the build's machine id: trellis
  # firstboot adds the suffix from the node's own.
  suffix=""
elif ! echo "$suffix" | grep -qE '^[0-9a-f]{2}(:[0-9a-f]{2}){2}$'; then
  h="$(sha256sum /etc/machine-id | cut -c1-6)"
  suffix="$(echo "$h" | sed 's/\(..\)\(..\)\(..\)/\1:\2:\3/')"
  [ "$suffix" = "00:00:00" ] && suffix="00:00:01"   # 00:00:00 means "random" to the driver
fi
opts="country=$COUNTRY bcf=$BCF_NAME enable_ps=0${suffix:+ macaddr_suffix=$suffix}"
if [ -n "$SPI_HZ" ]; then
  opts="$opts spi_clock_speed=$SPI_HZ"
  # The chip wants a number of clock bytes between data blocks (50 are
  # too few, 75 enough). The driver allows 40 us, which is 100 bytes at
  # 20 MHz and fewer below; with too few, the firmware download fails
  # ("cmd53_write ... ret:-71").
  [ "$SPI_HZ" -lt 20000000 ] && opts="$opts spi_inter_block_delay_bytes=100"
fi
want="# Written by Trellis (scripts/install-wm6108-halow.sh): Morse Micro HaLow driver.
options morse $opts"
if [ "$(cat "$MODCONF" 2>/dev/null)" != "$want" ]; then
  printf '%s\n' "$want" > "$MODCONF"
  echo "-- wrote $MODCONF: options morse $opts"
fi

# --- 4. NetworkManager -------------------------------------------------------
UDEV=/etc/udev/rules.d/71-trellis-halow.rules
udev_want='# Written by Trellis (scripts/install-wm6108-halow.sh). The HaLow radio is not a
# Wi-Fi client device: NetworkManager must not scan or connect with it.
SUBSYSTEM=="net", DRIVERS=="morse_spi", ENV{NM_UNMANAGED}="1"'
if [ "$(cat "$UDEV" 2>/dev/null)" != "$udev_want" ]; then
  printf '%s\n' "$udev_want" > "$UDEV"
  udevadm control --reload 2>/dev/null || true
  echo "-- wrote $UDEV"
fi

if [ -n "$IMAGE_KERNEL" ]; then
  # The overlay and its config.txt line are the image's (packaging/image/boards/).
  echo "== OK: the Morse driver is installed for ${KREL}"
  exit 0
fi

# --- 5. Device tree ----------------------------------------------------------
dtc -@ -q -I dts -O dtb -o "$OVERLAYS/$OVERLAY.dtbo" "$PKG/overlays/$OVERLAY-overlay.dts"
# Another SPI0 overlay would fight over the chip select.
conflicts="$(grep -nE '^dtoverlay=spi0-' "$CONFIG_TXT" || true)"
if [ -n "$conflicts" ]; then
  echo "!! $CONFIG_TXT has another SPI0 overlay; remove it, this one sets SPI0 up itself:" >&2
  echo "$conflicts" | sed 's/^/     /' >&2
fi
if grep -q "^#dtoverlay=$OVERLAY\$" "$CONFIG_TXT"; then
  sed -i "s/^#dtoverlay=$OVERLAY\$/dtoverlay=$OVERLAY/" "$CONFIG_TXT"
  echo "-- re-enabled dtoverlay=$OVERLAY in $CONFIG_TXT"
elif ! grep -q "^dtoverlay=$OVERLAY\(,.*\)\{0,1\}\$" "$CONFIG_TXT"; then
  [ -f "$CONFIG_TXT.trellis-bak" ] || cp "$CONFIG_TXT" "$CONFIG_TXT.trellis-bak"
  printf '\n[all]\n# Trellis: Wio-WM6108 HaLow module in a WM1302 Pi HAT, on SPI0 (install-wm6108-halow.sh)\ndtoverlay=%s\n' "$OVERLAY" >> "$CONFIG_TXT"
  echo "-- added dtoverlay=$OVERLAY to $CONFIG_TXT"
fi

sync   # before the driver touches the chip: a crash must not cost the files above
if [ -z "$DT_NODE" ]; then
  echo "== REBOOT NEEDED: everything is installed; the kernel learns where the chip is at the next boot."
  echo "   After the reboot, run this script again (or: dmesg | grep -i morse) to see the result."
  exit 0
fi

# --- Result ------------------------------------------------------------------
# The overlay is live: (re)load the driver so new options and a new build
# take effect, unless an interface of the radio is up (meshd may use it one day).
SPIDEV="/sys/bus/spi/devices/spi0.0"
busy=0
for net in "$SPIDEV"/net/*; do
  [ -r "$net/flags" ] || continue
  [ $(( $(cat "$net/flags") & 1 )) -eq 1 ] && busy=1   # IFF_UP
done
if [ "$busy" = 1 ]; then
  echo "-- an interface of the radio is up; not reloading the driver. Reboot to apply changes."
else
  modprobe -r morse 2>/dev/null || true
  modprobe morse || { echo "!! modprobe morse failed; see: dmesg | grep -i morse" >&2; exit 1; }
  # The probe loads firmware over SPI, which takes a few seconds.
  i=0
  while [ $i -lt 20 ] && ! ls "$SPIDEV/ieee80211" >/dev/null 2>&1; do sleep 1; i=$((i + 1)); done
fi

drv="$(basename "$(readlink "$SPIDEV/driver" 2>/dev/null)" 2>/dev/null || true)"
phy="$(ls "$SPIDEV/ieee80211" 2>/dev/null | head -1)"
if [ "$drv" = morse_spi ] && [ -n "$phy" ]; then
  ifs="$(ls "$SPIDEV/net" 2>/dev/null | tr '\n' ' ')"
  echo "== OK: the HaLow radio is up: $phy, interface(s): ${ifs:-none}"
  hz="$(cat /sys/module/morse/parameters/spi_clock_speed 2>/dev/null || echo 0)"
  if [ "$hz" = 0 ]; then hz="set by the overlay"; else hz="$hz Hz (module option)"; fi
  echo "   SPI clock: $hz; country: $(cat /sys/module/morse/parameters/country 2>/dev/null)"
  dmesg 2>/dev/null | grep -iE 'morse.*(Loaded firmware|Loaded BCF|driver registration)' | tail -n 3 | sed 's/^/   /'
  if dmesg 2>/dev/null | grep -qi 'failsafe BCF\|BCF may not be appropriate'; then
    echo "!! the driver did not accept $BCF_NAME; the radio will not transmit properly" >&2
  fi
else
  echo "!! The chip did not come up (spi0.0 driver: ${drv:-none}, radio: ${phy:-none}). Driver messages:" >&2
  dmesg 2>/dev/null | grep -iE 'morse|spi0' | tail -n 25 | sed 's/^/   /' >&2
  echo "   If these are cmd53_write errors, the SPI clock is the first suspect: try --spi-hz 15000000." >&2
  echo "   Otherwise check that the module is seated, the HAT is on all 40 pins, and the power supply holds:" >&2
  echo "   vcgencmd get_throttled   (anything but 0x0 means undervoltage)" >&2
  exit 1
fi
