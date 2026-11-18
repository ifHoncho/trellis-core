#!/bin/sh
# Runs inside a throwaway Debian-family container (scripts/apt-test.sh
# starts it). /srv holds Trellis apt repositories built by tools/aptrepo:
# repo1 (trellis 0.9.0), repo2 (0.9.1 added), bad (repo2 with an index
# changed after signing), other (repo2 signed by another key), and
# keyring.gpg, the archive key. The repositories are read as file: URIs,
# just as apt reads them over https.
set -eu
arch="$1"
fail() { echo "FAIL: $*" >&2; exit 1; }
ok() { echo "ok   $*"; }
export DEBIAN_FRONTEND=noninteractive
. /etc/os-release
echo "== $PRETTY_NAME, apt $(apt-get --version | sed -n '1s/^apt \([^ ]*\).*/\1/p')"

install -m 644 /srv/keyring.gpg /usr/share/keyrings/trellis-archive-keyring.gpg
use() { echo "deb [signed-by=/usr/share/keyrings/trellis-archive-keyring.gpg] file:/srv/$1 stable main" > /etc/apt/sources.list.d/trellis.list; }

use repo1
apt-get update > /tmp/update.log 2>&1 || { tail -20 /tmp/update.log; fail "apt-get update with the repository"; }
grep -qiE 'not signed|NO_PUBKEY|insecure' /tmp/update.log && { cat /tmp/update.log; fail "apt complained about the signature"; }
[ "$(apt-cache policy trellis | sed -n 's/^  Candidate: //p')" = 0.9.0 ] || { apt-cache policy trellis; fail "candidate is not 0.9.0"; }
ok "apt update takes the signed repository; trellis 0.9.0 is offered"

apt-get install -y --no-install-recommends trellis trellis-batman-adv-dkms > /tmp/install.log 2>&1 || { tail -30 /tmp/install.log; fail "apt-get install trellis"; }
dpkg -s trellis | grep -q '^Version: 0.9.0$' || fail "trellis 0.9.0 not installed"
/usr/bin/trellis version | grep -q 0.9.0 || fail "the installed binary is not 0.9.0"
ok "trellis and trellis-batman-adv-dkms installed from it, with their dependencies"

use repo2
apt-get update -qq > /dev/null 2>&1 || fail "apt-get update after 0.9.1 was added"
apt-get install -y --only-upgrade trellis > /tmp/upgrade.log 2>&1 || { tail -30 /tmp/upgrade.log; fail "apt-get upgrade"; }
dpkg -s trellis | grep -q '^Version: 0.9.1$' || fail "trellis was not upgraded to 0.9.1"
/usr/bin/trellis version | grep -q 0.9.1 || fail "the installed binary is not 0.9.1"
apt-cache madison trellis | grep -q ' 0.9.0 ' || fail "0.9.0 is no longer listed"
ok "0.9.1 added to the repository: apt upgrades to it; 0.9.0 stays available"

use bad
if apt-get update > /tmp/bad.log 2>&1 && ! grep -qiE 'hash sum mismatch|failed|^E:|^W:.*(Hash|mismatch)' /tmp/bad.log; then
  cat /tmp/bad.log; fail "a tampered index was taken"
fi
apt-cache policy trellis | grep -q 0.9.2 && fail "the tampered version is offered"
ok "an index changed after signing is refused"

use other
if apt-get update > /tmp/other.log 2>&1 && ! grep -qiE 'NO_PUBKEY|not signed|signature|^W:|^E:' /tmp/other.log; then
  cat /tmp/other.log; fail "a repository signed by another key was taken"
fi
grep -qiE 'NO_PUBKEY|signature|not signed' /tmp/other.log || { cat /tmp/other.log; fail "apt gave no signature error for the other key"; }
ok "a repository signed by another key is refused"
echo "== all passed"
