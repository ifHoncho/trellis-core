#!/usr/bin/env bash
# The apt repository end to end (make apt-test): tools/aptrepo builds a
# repository signed with a throwaway gpg key, and apt in throwaway
# containers adds it, installs from it, upgrades from it, and refuses it
# when its index is tampered with or signed by another key. Needs podman
# (or docker), gpg, and network access for the distributions' own packages.
#
#   scripts/apt-test.sh [image ...]      default: debian:13 ubuntu:22.04
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
RT="$(command -v podman || command -v docker || true)"
[ -n "$RT" ] || { echo "needs podman or docker" >&2; exit 2; }
command -v gpg >/dev/null || { echo "needs gpg" >&2; exit 2; }
IMAGES=("$@"); [ ${#IMAGES[@]} -gt 0 ] || IMAGES=(docker.io/library/debian:13 docker.io/library/ubuntu:22.04)
case "$(uname -m)" in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; *) echo "unsupported build machine" >&2; exit 2 ;; esac

W="$(mktemp -d)"
export GNUPGHOME="$W/gnupg"
cleanup() { gpgconf --kill gpg-agent 2>/dev/null || true; [ -n "${APT_TEST_KEEP:-}" ] && cp -a "$W/serve" "$APT_TEST_KEEP"; rm -rf "$W"; }
trap cleanup EXIT
mkdir -m 700 "$GNUPGHOME"
mkdir -p "$W/debs" "$W/serve"

# Packages: trellis 0.9.0 and 0.9.1 and the batman-adv package, signed
# with a throwaway release key.
tarball="dist/cache/batman-adv-$(sed -n 's/^BATADV_VERSION=//p' packaging/dkms/source).tar.gz"
[ -f "$tarball" ] || make -s "$tarball"
go run ./tools/mkdeb keygen --out "$W/key" --comment "apt-test" > "$W/key.pub" 2>/dev/null
for v in 0.9.0 0.9.1; do
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags "-s -w -X main.version=$v" -o "$W/trellis" ./cmd/trellis
  go run ./tools/mkdeb trellis --version $v --arch $arch --binary "$W/trellis" --out "$W/debs" --key "$W/key" >/dev/null
done
go run ./tools/mkdeb batman-adv --source "$tarball" --out "$W/debs" --key "$W/key" >/dev/null

# Two gpg keys: the archive's, and another that must not be taken for it.
for who in archive other; do
  gpg --batch --quiet --passphrase "" --quick-gen-key "Trellis $who (apt-test)" ed25519 sign never 2>/dev/null
done
fpr() { gpg --batch --with-colons --list-keys "$1" 2>/dev/null | awk -F: '$1=="fpr"{print $10; exit}'; }
akey="$(fpr 'Trellis archive')"; okey="$(fpr 'Trellis other')"

repo() { go run ./tools/aptrepo --allow-dev --trusted "$W/key.pub" "$@"; }
# repo1: 0.9.0. repo2: repo1 with 0.9.1 added. bad: repo2 with an index
# changed after signing. other: repo2 signed by the other key.
repo --dir "$W/serve/repo1" --gpg-key "$akey" "$W"/debs/trellis_0.9.0_$arch.deb "$W"/debs/trellis-batman-adv-dkms_*.deb 2>/dev/null
cp -a "$W/serve/repo1" "$W/serve/repo2"
repo --dir "$W/serve/repo2" --gpg-key "$akey" "$W"/debs/trellis_0.9.1_$arch.deb 2>/dev/null
cmp -s "$W/serve/repo1/pool/main/t/trellis/trellis_0.9.0_$arch.deb" "$W/serve/repo2/pool/main/t/trellis/trellis_0.9.0_$arch.deb"
cp -a "$W/serve/repo2" "$W/serve/bad"
bp="$W/serve/bad/dists/stable/main/binary-$arch/Packages"
sed -i 's/^Version: 0.9.1$/Version: 0.9.2/' "$bp"
gzip -9nc "$bp" > "$bp.gz"
cp -a "$W/serve/repo2" "$W/serve/other"
rm -f "$W/serve/other/trellis-archive-keyring."*
go run ./tools/aptrepo --dir "$W/serve/other" --gpg-key "$okey" 2>/dev/null
cp "$W/serve/repo1/trellis-archive-keyring.gpg" "$W/serve/keyring.gpg"
cp packaging/deb/apt-in-container.sh "$W/serve/test.sh"
chmod -R a+rX "$W/serve"

rc=0
for img in "${IMAGES[@]}"; do
  printf '\n### %s\n' "$img"
  "$RT" run --rm -v "$W/serve:/srv:ro,Z" "$img" sh /srv/test.sh "$arch" || rc=1
done
exit $rc
