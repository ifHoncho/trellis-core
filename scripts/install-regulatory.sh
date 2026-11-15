#!/bin/sh
# Put the node's regulatory rules in Trellis's hands (architecture §5.1,
# "How unrestricted is implemented"): the kernel finds no regulatory.db it
# can use, so it asks userspace for each country, and udev runs
# "trellis crda" to answer: the country's own rules in country mode, the
# permissive ones in unrestricted mode.
#
#   sudo scripts/install-regulatory.sh          install (idempotent)
#   sudo scripts/install-regulatory.sh --undo   give the rules back to the kernel's database
#   scripts/install-regulatory.sh --status      say what is in place
#
# Exit status 10: the kernel cannot ask userspace (no CRDA support), and
# nothing was changed; the kernel's own database stays in charge.
#
# Order matters: the kernel stops asking after ten unanswered requests, so
# the udev rule goes in before the database goes away. A kernel keeps a
# database it has already loaded: the switch takes effect at the next boot.
set -eu
HERE="$(cd "$(dirname "$0")" && pwd)"
FW=/lib/firmware
DB="$FW/regulatory.db"
SIG="$FW/regulatory.db.p7s"
RULE=/etc/udev/rules.d/85-trellis-regulatory.rules
SHARE=/usr/share/trellis
NONE="$SHARE/regulatory.db.none"
NONE_SIG="$SHARE/regulatory.db.p7s.none"
BIN=/usr/bin/trellis
STATE=/run/trellis-crda/regdom.json

say() { printf '%s\n' "$*"; }

kernel_config() {
  for f in "/boot/config-$(uname -r)" /proc/config.gz; do
    [ -r "$f" ] && { zcat -f "$f" 2>/dev/null; return 0; }
  done
  modprobe configs 2>/dev/null && [ -r /proc/config.gz ] && { zcat /proc/config.gz; return 0; }
  return 1
}

# How the distribution installs regulatory.db: alternatives (Debian), dpkg
# (Ubuntu; "diverted" once moved aside), or none of those.
layout() {
  if [ -L "$DB" ] && [ "$(readlink "$DB")" = /etc/alternatives/regulatory.db ]; then
    echo alternatives
  elif command -v dpkg-divert >/dev/null && dpkg-divert --list "$DB" 2>/dev/null | grep -q "$DB.distrib"; then
    echo diverted
  elif command -v dpkg >/dev/null && dpkg -S "$DB" >/dev/null 2>&1; then
    echo dpkg
  elif [ -e "$DB" ]; then
    echo file
  else
    echo absent
  fi
}

status() {
  if [ -f "$RULE" ]; then say "udev rule: $RULE"; else say "udev rule: not installed"; fi
  case "$(layout)" in
    alternatives)
      if [ "$(readlink -f /etc/alternatives/regulatory.db)" = "$NONE" ]; then
        say "regulatory.db: set aside (update-alternatives points it to $NONE)"
      else
        say "regulatory.db: the kernel's ($(readlink -f /etc/alternatives/regulatory.db))"
      fi ;;
    diverted)
      if [ -e "$DB" ]; then say "regulatory.db: diverted to $DB.distrib, but a file is in its place (the old fallback)"
      else say "regulatory.db: set aside (diverted to $DB.distrib)"; fi ;;
    dpkg|file) say "regulatory.db: the kernel's ($DB)" ;;
    absent) say "regulatory.db: none" ;;
  esac
  if [ -f "$STATE" ]; then
    say "since boot, trellis crda has answered: $(tr -d '\n' < "$STATE" | sed 's/  */ /g')"
  else
    say "since boot, trellis crda has answered nothing (the kernel reads its own database, or nothing asked yet)"
  fi
}

case "${1:-}" in
  --status) status; exit 0 ;;
  --undo|"") ;;
  *) say "usage: $0 [--undo|--status]" >&2; exit 2 ;;
esac
[ "$(id -u)" -eq 0 ] || { say "run as root" >&2; exit 1; }

if [ "${1:-}" = "--undo" ]; then
  case "$(layout)" in
    alternatives)
      update-alternatives --remove regulatory.db "$NONE" >/dev/null 2>&1 || true
      say "-- regulatory.db: back to the distribution's alternative" ;;
    diverted)
      rm -f "$DB" "$SIG"
      dpkg-divert --local --rename --divert "$SIG.distrib" --remove "$SIG" >/dev/null
      dpkg-divert --local --rename --divert "$DB.distrib" --remove "$DB" >/dev/null
      say "-- regulatory.db: the distribution's restored" ;;
  esac
  rm -f "$RULE"
  udevadm control --reload 2>/dev/null || true
  say "-- udev rule removed"
  # Old kernels reject current databases (packaging/regdb/README.md).
  if [ -x "$HERE/install-regdb-fallback.sh" ]; then "$HERE/install-regdb-fallback.sh" || true; fi
  iw reg reload 2>/dev/null || true
  say "-- the kernel reads its database again (iw reg reload)"
  exit 0
fi

# 1. The kernel must be able to ask userspace.
if cfg="$(kernel_config)"; then
  if ! printf '%s\n' "$cfg" | grep -q '^CONFIG_CFG80211_CRDA_SUPPORT=y'; then
    say "!! this kernel ($(uname -r)) has no CFG80211_CRDA_SUPPORT: it cannot ask userspace for its rules."
    say "   Left as is: the kernel's own regulatory.db stays in charge, and unrestricted mode is unavailable here."
    exit 10
  fi
else
  say "-- cannot read this kernel's configuration; assuming CFG80211_CRDA_SUPPORT (every kernel seen has it)"
fi
[ -x "$BIN" ] || { say "!! $BIN is missing: install Trellis first" >&2; exit 1; }
"$BIN" crda --country US --mode country --print >/dev/null 2>&1 \
  || say "!! trellis crda finds no regulatory.db to read for country mode; country mode would get no rules"

# 2. The udev rule, before the database goes.
mkdir -p "$(dirname "$RULE")"
cat > "$RULE.new" <<EOF
# Trellis: answer the kernel's regulatory requests (scripts/install-regulatory.sh).
ACTION=="change", KERNEL=="regulatory*", ENV{COUNTRY}=="?*", RUN+="$BIN crda"
EOF
if cmp -s "$RULE.new" "$RULE" 2>/dev/null; then rm -f "$RULE.new"; else mv "$RULE.new" "$RULE"; say "-- installed $RULE"; fi
udevadm control --reload 2>/dev/null || true

# 3. A regulatory.db the kernel cannot use.
changed=no
case "$(layout)" in
  alternatives)
    # Another alternative, chosen in auto mode by its priority, so package
    # upgrades keep it. The kernel rejects its content (no RGDB magic).
    mkdir -p "$SHARE"
    printf 'not a regulatory database: Trellis answers the kernel (scripts/install-regulatory.sh)\n' > "$NONE"
    printf 'none\n' > "$NONE_SIG"
    if [ "$(readlink -f /etc/alternatives/regulatory.db)" != "$NONE" ]; then
      update-alternatives --install "$DB" regulatory.db "$NONE" 1000 --slave "$SIG" regulatory.db.p7s "$NONE_SIG" >/dev/null
      changed=yes
    fi ;;
  dpkg)
    dpkg-divert --local --rename --divert "$DB.distrib" --add "$DB" >/dev/null
    dpkg-divert --local --rename --divert "$SIG.distrib" --add "$SIG" >/dev/null
    changed=yes ;;
  diverted)
    # Diverted already (the old fallback): remove the copy it put in place.
    if [ -e "$DB" ] || [ -e "$SIG" ]; then rm -f "$DB" "$SIG"; changed=yes; fi ;;
  file)
    mv "$DB" "$DB.distrib"; [ -e "$SIG" ] && mv "$SIG" "$SIG.distrib"
    changed=yes ;;
  absent) ;;
esac
[ "$changed" = yes ] && say "-- regulatory.db set aside: the kernel asks Trellis for its rules"

# In effect when the kernel failed to load its database this boot. If
# nothing has been answered yet that meshd can see (a record from an older
# build, in a directory systemd empties), have the kernel ask once more.
kernel_asks() {
  { dmesg 2>/dev/null || journalctl -k -b --no-pager 2>/dev/null; } \
    | grep -q -E 'failed to load regulatory.db|regulatory.db is malformed or signature is missing/invalid'
}
# trellis crda's own record of an answer since boot proves it (the kernel's
# boot message may have rotated out of its log on a node up for long).
if [ -f "$STATE" ] || kernel_asks; then
  if [ ! -f "$STATE" ]; then
    cur="$(iw reg get 2>/dev/null | sed -n 's/^country \([A-Z0-9][A-Z0-9]\):.*/\1/p' | head -1)"
    if [ -n "$cur" ] && [ "$cur" != 00 ]; then
      iw reg set 00 2>/dev/null && iw reg set "$cur" 2>/dev/null || true
    fi
  fi
  say "-- in effect: the kernel asks trellis crda for its rules since boot"
else
  say "!! takes effect at the next boot: the kernel keeps the database it loaded at start"
fi
