#!/bin/sh
# Turn a Raspberry Pi OS image into a Trellis node's. Run by
# scripts/build-image.sh inside the image (a chroot on an arm64 machine),
# never on a node. It finds in /tmp/trellis-image:
#
#   board.conf   the board (packaging/image/boards/)
#   debs/        the trellis and trellis-batman-adv-dkms packages
#
# and writes the image's package list to /tmp/trellis-image/manifest.txt.
# Environment: IMAGE_NAME, IMAGE_BOARD, TRELLIS_VERSION, SOURCE_DATE_EPOCH.
set -eu
W=/tmp/trellis-image
# shellcheck source=/dev/null
. "$W/board.conf"
export DEBIAN_FRONTEND=noninteractive
say() { echo "image: $*"; }
apt_get() { apt-get -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold "$@"; }

# Nothing starts in the chroot: no init runs here, and a service started
# would hold the image's mounts.
printf '#!/bin/sh\nexit 101\n' > /usr/sbin/policy-rc.d
chmod 0755 /usr/sbin/policy-rc.d

say "packages: remove, upgrade and add"
# The board's REMOVE_PACKAGES are dpkg patterns: first, so the upgrade does
# not bring new versions of them.
if [ -n "${REMOVE_PACKAGES:-}" ]; then
  set -f   # the patterns are dpkg's, not the shell's
  # shellcheck disable=SC2086
  remove="$(dpkg-query -W -f '${db:Status-Abbrev} ${Package}\n' $REMOVE_PACKAGES 2>/dev/null | awk '$1 ~ /^.i/ {print $2}')"
  set +f
  # shellcheck disable=SC2086
  [ -z "$remove" ] || apt_get -y -q purge $remove
fi
apt_get update -q
apt_get -y -q full-upgrade
# shellcheck disable=SC2086
apt_get -y -q install --no-install-recommends $PACKAGES
apt_get -y -q autoremove --purge

# The kernel the image boots, per flavour of the board's: the newest, which
# must have its headers (the drivers are built for it; an older kernel
# kept instead would not be the one the board boots). apt keeps the one
# the upgrade replaced; it goes, so drivers are built once and the image
# carries one kernel.
KERNELS=""
for f in $KERNEL_FLAVOURS; do
  keep=""
  for d in $(ls -1d /lib/modules/*-"$f" 2>/dev/null | sort -rV); do
    k="$(basename "$d")"
    if [ -z "$keep" ]; then
      [ -f "$d/build/Makefile" ] || { echo "image: kernel $k has no headers (linux-headers-$k)" >&2; exit 1; }
      keep="$k"
      KERNELS="$KERNELS $k"
    else
      old="$(dpkg-query -W -f '${Package}\n' "linux-image-$k" "linux-headers-$k" 2>/dev/null || true)"
      # shellcheck disable=SC2086
      [ -z "$old" ] || apt_get -y -q purge $old
      rm -rf "$d"
    fi
  done
done
KERNELS="${KERNELS# }"
[ -n "$KERNELS" ] || { echo "image: no kernel of flavour '$KERNEL_FLAVOURS' with headers" >&2; exit 1; }
FIRST="${KERNELS%% *}"
say "kernels: $KERNELS"

say "trellis: install"
# The packages' scripts build for the image's kernel, not this machine's
# (TRELLIS_IMAGE_KERNEL; scripts/install-*.sh).
TRELLIS_IMAGE_KERNEL="$FIRST" apt_get -y -q install --no-install-recommends "$W"/debs/*.deb
S=/usr/share/trellis/scripts
for k in $KERNELS; do
  [ "$k" = "$FIRST" ] || TRELLIS_IMAGE_KERNEL="$k" "$S/install-batman-adv-dkms.sh"
  for drv in $DRIVERS; do
    case "$drv" in
      mt7915e) TRELLIS_IMAGE_KERNEL="$k" "$S/install-mt7915e-dkms.sh" ;;
      morse) TRELLIS_IMAGE_KERNEL="$k" "$S/install-wm6108-halow.sh" ;;
      *) echo "image: unknown driver $drv" >&2; exit 1 ;;
    esac
  done
done
if [ "${HALOW_FIRMWARE:-}" = yes ]; then
  # shellcheck source=/dev/null
  . /usr/share/trellis/packaging/dkms-morse/firmware.pins
  base="https://raw.githubusercontent.com/$(echo "$FW_REPO" | sed 's#^https://github.com/##; s#\.git$##')/$FW_REF"
  install -d /lib/firmware/morse
  curl -fsSL --retry 3 -o /tmp/mm6108.bin "$base/$FW_FILE"
  [ "$(sha256sum /tmp/mm6108.bin | cut -d' ' -f1)" = "$FW_SHA256" ] || { echo "image: $FW_FILE is not the pinned file" >&2; exit 1; }
  install -m 0644 /tmp/mm6108.bin "/lib/firmware/morse/$(basename "$FW_FILE")"
  curl -fsSL --retry 3 -o /lib/firmware/morse/LICENSE "$base/$LICENSE_FILE"
  say "Morse firmware $(basename "$FW_FILE") and its licence in the image (no BCF)"
fi
if [ "${HALOW_TOOLS:-}" = yes ]; then
  "$S/install-halow-tools.sh"
fi

# Kernel updates wait until the DKMS modules build for the new kernel:
# apt leaves the kernel as it is.
for f in $KERNEL_FLAVOURS; do
  for p in "linux-image-$f" "linux-headers-$f"; do
    if dpkg -s "$p" >/dev/null 2>&1; then apt-mark hold "$p" >/dev/null; fi
  done
done
for k in $KERNELS; do
  for p in "linux-image-$k" "linux-headers-$k"; do
    if dpkg -s "$p" >/dev/null 2>&1; then apt-mark hold "$p" >/dev/null; fi
  done
done

say "boot partition: overlays, config.txt, cmdline.txt"
BOOT=/boot/firmware
for o in $OVERLAYS; do
  dtc -@ -q -I dts -O dtb -o "$BOOT/overlays/$o.dtbo" "/usr/share/trellis/packaging/overlays/$o-overlay.dts"
done
{
  printf '\n[all]\n# Trellis image (%s): %s\n' "$IMAGE_NAME" "$BOARD_DESC"
  printf '%s\n' "$CONFIG_TXT"
} >> "$BOOT/config.txt"
if [ "${CONSOLE_OFF_UART:-}" = yes ]; then
  sed -i -E 's/(^| )console=(serial0|ttyAMA0|ttyS0),[0-9]+//g; s/^ +//' "$BOOT/cmdline.txt"
  printf '# Trellis image: a GPS on the console UART must not reach Magic SysRq.\nkernel.sysrq = 0\n' \
    > /etc/sysctl.d/90-trellis-no-sysrq.conf
fi
# shellcheck disable=SC2086
[ -z "${MASK_UNITS:-}" ] || systemctl mask $MASK_UNITS >/dev/null 2>&1
if [ -n "${GPSD_DEVICES:-}" ]; then
  cat > /etc/default/gpsd <<EOF
# Trellis image ($IMAGE_NAME): the board's GPS, read only.
START_DAEMON="true"
USBAUTO="true"
DEVICES="$GPSD_DEVICES"
GPSD_OPTIONS="$GPSD_OPTIONS"
EOF
  systemctl enable gpsd.service >/dev/null 2>&1 || true
fi

say "system: SSH on, Trellis's setup only, a name to replace"
# SSH is always on; the operator account trellis is made by the
# setup wizard or from the card (trellis firstboot). Host keys are made
# at each node's first boot (sshd-keygen), never shared by an image.
systemctl enable ssh.service >/dev/null 2>&1
rm -f /etc/ssh/ssh_host_*
# One way to set a node up: Trellis's. cloud-init (Raspberry Pi
# Imager's path) and Raspberry Pi OS's console account prompt go.
apt_get -y -q purge cloud-init rpi-cloud-init-mods
rm -rf /etc/cloud /var/lib/cloud
rm -f /boot/firmware/user-data /boot/firmware/meta-data /boot/firmware/network-config
systemctl mask userconfig.service >/dev/null 2>&1 || true
# Its SSH banner ("SSH may not work until a valid user has been set up")
# belongs to that prompt: the operator account is Trellis's.
for f in /etc/ssh/sshd_config.d/*; do
  if [ -f "$f" ] && grep -q userconf "$f"; then rm -f "$f"; fi
done
echo trellis > /etc/hostname
if grep -q '^127\.0\.1\.1' /etc/hosts; then
  sed -i 's/^127\.0\.1\.1.*/127.0.1.1\ttrellis/' /etc/hosts
else
  printf '127.0.1.1\ttrellis\n' >> /etc/hosts
fi
# Each node makes its own machine id at first boot (trellis firstboot
# derives its hostname and HaLow MAC suffix from it). "uninitialized", as
# Raspberry Pi OS ships it: systemd's first-boot units (SSH host keys among
# them) run only then.
printf 'uninitialized\n' > /etc/machine-id
rm -f /var/lib/dbus/machine-id

install -d /usr/lib/trellis
BUILT="$(date -u -d "@${SOURCE_DATE_EPOCH}" +%Y-%m-%dT%H:%M:%SZ)"
cat > /usr/lib/trellis/image-release <<EOF
IMAGE_NAME=$IMAGE_NAME
IMAGE_VERSION=$TRELLIS_VERSION
IMAGE_BOARD=$IMAGE_BOARD
IMAGE_KERNEL=$FIRST
IMAGE_BASE=$(basename "$BASE_URL")
IMAGE_BUILT=$BUILT
IMAGE_HOSTNAME=trellis
EOF

say "clean up"
dpkg-query -W -f '${Package}\t${Version}\n' | sort > "$W/manifest.txt"
apt-mark showhold | sed 's/^/held\t/' >> "$W/manifest.txt"
apt_get clean
rm -rf /var/lib/apt/lists/* /var/cache/trellis /var/cache/debconf/*-old /var/log/*.log /var/log/apt/* /root/.cache
rm -f /usr/sbin/policy-rc.d
# Nothing of a node may be in the image: no identity, profile or state.
[ -z "$(ls -A /etc/trellis 2>/dev/null)" ] || { echo "image: /etc/trellis is not empty" >&2; ls -A /etc/trellis >&2; exit 1; }
[ -z "$(find /var/lib/trellis -mindepth 1 ! -name hostapd-masked -print -quit 2>/dev/null)" ] || { echo "image: /var/lib/trellis holds node state" >&2; ls -A /var/lib/trellis >&2; exit 1; }
say "done: $IMAGE_NAME"
