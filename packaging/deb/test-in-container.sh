#!/bin/sh
# Runs inside a throwaway Debian-family container (scripts/deb-test.sh
# starts it): installs, upgrades, removes and purges the packages with the
# distribution's own apt and dpkg, and checks what each step leaves behind.
# /debs holds: old/ and new/ (two versions of trellis), and the batman-adv
# package. There is no systemd and no matching kernel here: services are
# not started and the module is not built, which is itself part of the
# test (an install must survive both).
set -eu
fail() { echo "FAIL: $*" >&2; exit 1; }
ok() { echo "ok   $*"; }
export DEBIAN_FRONTEND=noninteractive
. /etc/os-release
echo "== $PRETTY_NAME, dpkg $(dpkg --version | sed -n '1s/.*version \([^ ]*\).*/\1/p')"

old="$(ls /debs/old/trellis_*.deb)"; new="$(ls /debs/new/trellis_*.deb)"; bat="$(ls /debs/trellis-batman-adv-dkms_*.deb)"

# dpkg reads the packages, signature member and all.
for d in "$old" "$new" "$bat"; do
  dpkg-deb --info "$d" >/dev/null || fail "dpkg-deb --info $d"
  dpkg-deb --contents "$d" >/dev/null || fail "dpkg-deb --contents $d"
done
grep -q '_trellis-sig' "$new" || fail "the new package is not signed"
ok "dpkg-deb reads the signed package"
dpkg-deb --contents "$new" | grep -E ' \./etc/' | grep -v -E '\./etc/(chrony/(conf\.d/(trellis\.conf)?)?)?$' && fail "the package ships files under /etc other than the chrony drop-in"
dpkg-deb --contents "$new" | grep -q '/etc/trellis' && fail "the package ships /etc/trellis"
ok "no configuration in the package"

apt-get update -qq >/dev/null
apt-get install -y -qq --no-install-recommends sudo >/dev/null 2>&1 || fail "cannot install sudo"

# A node installed by the earlier scripts.
mkdir -p /usr/local/bin /etc/systemd/system /etc/sudoers.d /etc/udev/rules.d
echo old > /usr/local/bin/trellis; chmod +x /usr/local/bin/trellis
for l in meshd meshctl webd; do ln -sf trellis /usr/local/bin/$l; done
echo old > /etc/systemd/system/trellis-meshd.service
echo old > /etc/udev/rules.d/70-trellis-names.rules
mkdir -p /etc/chrony/conf.d
echo "# the script install's copy" > /etc/chrony/conf.d/trellis.conf
echo 'pi ALL=(root) NOPASSWD: /home/pi/trellis-deploy/scripts/node-update.sh, /usr/local/bin/meshctl' > /etc/sudoers.d/trellis-deploy
# ...with its configuration, profile and identity.
mkdir -p /etc/trellis /var/lib/trellis
echo 'callsign: keep-me' > /etc/trellis/node.yaml
echo 'name: keep-me-too' > /etc/trellis/profile.yaml; chmod 600 /etc/trellis/profile.yaml
echo 'node_id: 77' > /var/lib/trellis/identity.yaml
sums() { sha256sum /etc/trellis/node.yaml /etc/trellis/profile.yaml /var/lib/trellis/identity.yaml; stat -c '%a %n' /etc/trellis/profile.yaml; }
before="$(sums)"

echo "== install $(basename "$old") and $(basename "$bat")"
apt-get install -y --no-install-recommends "$old" "$bat" > /tmp/install.log 2>&1 || { tail -40 /tmp/install.log; fail "apt-get install"; }
grep -E '^trellis' /tmp/install.log | head -30 || true
dpkg -s trellis | grep -q '^Status: install ok installed' || fail "trellis not fully installed"
dpkg -s trellis-batman-adv-dkms | grep -q '^Status: install ok installed' || fail "trellis-batman-adv-dkms not fully installed (its build must not fail the install)"
ok "both packages installed and configured"
[ -x /usr/bin/trellis ] && [ "$(readlink /usr/bin/meshctl)" = trellis ] || fail "binary or links missing"
/usr/bin/trellis version >/dev/null || fail "the binary does not run"
ok "binary: $(/usr/bin/trellis version)"
[ "$(sums)" = "$before" ] || fail "install changed the configuration, profile or identity"
ok "configuration, profile and identity untouched"
[ ! -e /usr/local/bin/trellis ] && [ ! -e /usr/local/bin/meshctl ] && [ ! -e /etc/systemd/system/trellis-meshd.service ] \
  && [ ! -e /etc/udev/rules.d/70-trellis-names.rules ] || fail "script install not taken over"
grep -q '/usr/bin/meshctl' /etc/sudoers.d/trellis-deploy && ! grep -q '/usr/local/bin/meshctl' /etc/sudoers.d/trellis-deploy || fail "sudo rule not moved"
ok "script install taken over, sudo rule follows"
getent group trellis >/dev/null || fail "no trellis group"
# webd's unit names it (the voice console's socket), voice or no voice.
getent group trellis-voice >/dev/null || fail "no trellis-voice group"
grep -q '^SupplementaryGroups=trellis trellis-voice$' /usr/lib/systemd/system/trellis-webd.service || fail "webd is not in the trellis-voice group"
[ -L /etc/systemd/system/multi-user.target.wants/trellis-meshd.service ] || fail "meshd not enabled"
[ -L /etc/systemd/system/sockets.target.wants/trellis-webd.socket ] || fail "web socket not enabled"
[ "$(readlink /etc/systemd/system/hostapd.service)" = /dev/null ] || fail "hostapd.service not masked"
ok "group, units enabled, hostapd masked"
# Masked by Trellis (the image's hostapd package did not): unmasked again
# at removal.
masked_by_trellis=no
[ -f /var/lib/trellis/hostapd-masked ] && masked_by_trellis=yes
dpkg-deb --fsys-tarfile "$old" | tar -xO ./etc/chrony/conf.d/trellis.conf > /tmp/dropin
cmp -s /tmp/dropin /etc/chrony/conf.d/trellis.conf || fail "the script install's chrony drop-in was kept instead of the package's"
grep -q "script install's copy" /etc/chrony/conf.d/trellis.conf.script-install || fail "the script install's drop-in was not kept aside"
ok "chrony drop-in taken over (the old copy kept aside)"
for f in deploy.sh deploy-test.sh release.sh release-notes.sh publish.sh deb-test.sh; do
  [ ! -e "/usr/share/trellis/scripts/$f" ] || fail "development script $f is in the package"
done
[ -x /usr/share/trellis/scripts/install-regulatory.sh ] && [ -x /usr/share/trellis/scripts/install-batman-adv-dkms.sh ] || fail "node scripts missing"
ok "only the node's scripts are packaged"
grep -q 'trellis crda' /etc/udev/rules.d/85-trellis-regulatory.rules || fail "regulatory udev rule missing"
/usr/share/trellis/scripts/install-regulatory.sh --status | grep -q 'set aside' || fail "regulatory.db not set aside: $(/usr/share/trellis/scripts/install-regulatory.sh --status)"
ok "regulatory rules in Trellis's hands"
# One source tree, named by version: a glob inside [ -f ] would test
# the pattern itself if it matched none, and fail on two.
set -- /usr/src/trellis-batman-adv-2026.3-*
[ $# = 1 ] || fail "batman-adv source: want one tree, found: $*"
bsrc=$1
[ -f "$bsrc/net/batman-adv/main.c" ] && [ -f "$bsrc/dkms.conf" ] || fail "batman-adv source missing"
grep -q '^PATCH\[0\]="0001-' "$bsrc/dkms.conf" && [ -f "$bsrc/patches/0001-set-priority-of-forwarded-multicast-packets.patch" ] || fail "Trellis's batman-adv patch is not packaged"
ok "batman-adv source with Trellis's patch: $(ls -d /usr/src/trellis-batman-adv-*)"
dkms status 2>/dev/null | grep -q '^trellis-batman-adv' && ok "dkms knows trellis-batman-adv: $(dkms status | head -1)" || ok "dkms: nothing added (no headers for the host's kernel here; reported, not fatal)"
# meshctl prepare runs every step and reports the ones a container cannot
# do (no headers for the host's kernel, no radios) instead of stopping.
rc=0
out="$(/usr/bin/meshctl prepare --no-doctor 2>&1)" || rc=$?
for h in "== batman-adv 2026.3" "== MT7915/MT7916 driver" "== Radio driver options" "== Regulatory rules" "== Summary"; do
  printf '%s\n' "$out" | grep -q -F "$h" || { printf '%s\n' "$out" | tail -30; fail "meshctl prepare: no step '$h'"; }
done
case "$rc" in 0|1) ;; *) printf '%s\n' "$out" | tail -30; fail "meshctl prepare exited $rc" ;; esac
ok "meshctl prepare ran every step (exit $rc: $(printf '%s\n' "$out" | tail -1 | cut -c1-90))"

# Voice was a package of its own before 0.3: a node may have it. Made
# here as that package was (its files, and Depends: trellis).
mkdir -p /tmp/tv/DEBIAN /tmp/tv/usr/bin /tmp/tv/usr/lib/systemd/system
printf 'Package: trellis-voice\nVersion: 0.9.0\nArchitecture: all\nMaintainer: test <test@example.com>\nDepends: trellis\nDescription: the old voice package\n' > /tmp/tv/DEBIAN/control
printf '#!/bin/sh\necho old voice\n' > /tmp/tv/usr/bin/trellis-voiced; chmod 755 /tmp/tv/usr/bin/trellis-voiced
echo '# old' > /tmp/tv/usr/lib/systemd/system/trellis-voiced.service
dpkg-deb --build /tmp/tv /tmp/trellis-voice_0.9.0_all.deb >/dev/null || fail "cannot build the old trellis-voice package"
dpkg -i /tmp/trellis-voice_0.9.0_all.deb >/dev/null 2>&1 || fail "cannot install the old trellis-voice package"
ok "the old, separate trellis-voice package installed"

echo "== operator edits the chrony drop-in, then upgrades to $(basename "$new") with dpkg alone (as meshctl upgrade does)"
echo '# operator edit' >> /etc/chrony/conf.d/trellis.conf
drop="$(sha256sum < /etc/chrony/conf.d/trellis.conf)"
dpkg -i --force-confold --force-confdef "$new" > /tmp/upgrade.log 2>&1 || { tail -40 /tmp/upgrade.log; fail "upgrade with dpkg -i (it must need no new package)"; }
dpkg -s trellis | grep -q '^Status: install ok installed' || fail "trellis not configured after upgrade"
[ "$(dpkg-query -W -f='${Version}' trellis)" = "$(dpkg-deb -f "$new" Version)" ] || fail "version after upgrade"
[ "$(sums)" = "$before" ] || fail "upgrade changed the configuration, profile or identity"
[ "$(sha256sum < /etc/chrony/conf.d/trellis.conf)" = "$drop" ] || fail "upgrade replaced the edited chrony drop-in"
[ -n "$(dpkg-query -W -f='${Trellis-Version}' trellis)" ] || fail "Trellis-Version field lost"
ok "upgrade keeps configuration, identity and the edited drop-in"
dpkg -s trellis-voice 2>/dev/null | grep -q '^Status: install ok installed' && fail "the old trellis-voice package is still installed"
[ "$(dpkg -S /usr/bin/trellis-voiced 2>/dev/null | cut -d: -f1)" = trellis ] || fail "trellis-voiced does not belong to trellis: $(dpkg -S /usr/bin/trellis-voiced 2>&1)"
grep -q '^# old' /usr/lib/systemd/system/trellis-voiced.service && fail "the old voice unit is still there"
ok "the merged package took the old trellis-voice's files and removed it"

echo "== voice"
dpkg-deb -f "$new" Recommends | grep -q 'libopus0' && ! dpkg-deb -f "$new" Depends | grep -q 'libopus' || fail "the voice libraries must be recommended, not required"
apt-get install -y --no-install-recommends libopus0 > /tmp/voice.log 2>&1 || fail "install libopus0"
apt-get install -y --no-install-recommends libasound2t64 >> /tmp/voice.log 2>&1 || apt-get install -y --no-install-recommends libasound2 >> /tmp/voice.log 2>&1 || { tail -20 /tmp/voice.log; fail "install libasound2"; }
v="$(/usr/bin/trellis-voiced version)" || fail "trellis-voiced does not run or misses a library: $v"
[ -f /usr/lib/systemd/system/trellis-voiced.service ] && [ -f /usr/lib/udev/rules.d/70-trellis-voice.rules ] || fail "voice unit or udev rule missing"
# The voice service takes packets from any device: it must not be in the
# group that opens meshd's API.
grep -q '^Group=trellis-voice$' /usr/lib/systemd/system/trellis-voiced.service || fail "trellis-voiced is not in its own group"
grep -Eq '^(Group|SupplementaryGroups)=(.* )?trellis( |$)' /usr/lib/systemd/system/trellis-voiced.service && fail "trellis-voiced is in the trellis group: meshd's API is open to it"
ok "voice is in the trellis package and runs with its libraries: $v"

echo "== downgrade (what a rollback does)"
apt-get install -y --allow-downgrades --no-install-recommends "$old" > /tmp/down.log 2>&1 || { tail -40 /tmp/down.log; fail "downgrade"; }
[ "$(dpkg-query -W -f='${Version}' trellis)" = "$(dpkg-deb -f "$old" Version)" ] || fail "version after downgrade"
[ "$(sums)" = "$before" ] || fail "downgrade changed the configuration"
ok "downgrade"

echo "== remove"
apt-get remove -y trellis trellis-batman-adv-dkms > /tmp/remove.log 2>&1 || { tail -40 /tmp/remove.log; fail "remove"; }
[ ! -e /usr/bin/trellis ] && [ ! -e /usr/bin/trellis-voiced ] || fail "binary left after remove"
[ "$(sums)" = "$before" ] || fail "remove changed the configuration"
[ ! -e /etc/udev/rules.d/85-trellis-regulatory.rules ] || fail "regulatory udev rule left after remove"
/usr/share/trellis/scripts/install-regulatory.sh --status 2>/dev/null | grep -q 'set aside' && fail "regulatory.db still set aside after remove"
[ -e /lib/firmware/regulatory.db ] || [ -L /lib/firmware/regulatory.db ] || fail "the distribution's regulatory.db is not back"
[ ! -d "$bsrc/net" ] || fail "batman-adv source left after remove"
if [ "$masked_by_trellis" = yes ]; then
  [ "$(readlink /etc/systemd/system/hostapd.service 2>/dev/null)" != /dev/null ] || fail "hostapd.service still masked after remove (Trellis masked it)"
  [ ! -e /var/lib/trellis/hostapd-masked ] || fail "hostapd mask note left after remove"
fi
echo "-- hostapd masked by Trellis: $masked_by_trellis"
ok "remove keeps configuration and gives the regulatory rules back"

echo "== purge"
apt-get purge -y trellis trellis-batman-adv-dkms > /tmp/purge.log 2>&1 || { tail -40 /tmp/purge.log; fail "purge"; }
[ ! -e /etc/trellis ] && [ ! -e /var/lib/trellis ] || fail "purge left configuration or state"
getent group trellis >/dev/null && fail "purge left the group"
getent group trellis-voice >/dev/null && fail "purge left the trellis-voice group"
ok "purge removes configuration, state and the group"
echo "== PASS on $PRETTY_NAME"
