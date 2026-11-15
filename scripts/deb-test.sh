#!/usr/bin/env bash
# Install, upgrade, remove and purge the packages in throwaway containers
# of the distributions Trellis supports (make deb-test). Needs podman (or
# docker) and network access for the distributions' own packages.
#
#   scripts/deb-test.sh [image ...]      default: debian:13 ubuntu:22.04
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
RT="$(command -v podman || command -v docker || true)"
[ -n "$RT" ] || { echo "needs podman or docker" >&2; exit 2; }
IMAGES=("$@"); [ ${#IMAGES[@]} -gt 0 ] || IMAGES=(docker.io/library/debian:13 docker.io/library/ubuntu:22.04)
case "$(uname -m)" in x86_64) arch=amd64 goarch=amd64 ;; aarch64) arch=arm64 goarch=arm64 ;; *) echo "unsupported build machine" >&2; exit 2 ;; esac

W="$(mktemp -d)"; trap 'rm -rf "$W"' EXIT
mkdir -p "$W/debs/old" "$W/debs/new"
tarball="dist/cache/batman-adv-$(sed -n 's/^BATADV_VERSION=//p' packaging/dkms/source).tar.gz"
[ -f "$tarball" ] || make -s "$tarball"
CGO_ENABLED=0 GOOS=linux GOARCH=$goarch go build -trimpath -ldflags "-s -w -X main.version=0.9.1" -o "$W/trellis" ./cmd/trellis
go run ./tools/mkdeb keygen --out "$W/key" --comment "deb-test" > "$W/key.pub" 2>/dev/null
# The old version unsigned, the new one signed: dpkg must take both.
go run ./tools/mkdeb trellis --version 0.9.0 --arch $arch --binary "$W/trellis" --out "$W/debs/old" >/dev/null
go run ./tools/mkdeb trellis --version 0.9.1 --arch $arch --binary "$W/trellis" --out "$W/debs/new" --key "$W/key" >/dev/null
go run ./tools/mkdeb batman-adv --source "$tarball" --out "$W/debs" --key "$W/key" >/dev/null
go run ./tools/mkdeb verify --pub "$W/key.pub" "$W"/debs/new/*.deb "$W"/debs/trellis-batman-adv-dkms_*.deb >/dev/null
cp packaging/deb/test-in-container.sh "$W/debs/test.sh"

rc=0
for img in "${IMAGES[@]}"; do
  printf '\n### %s\n' "$img"
  "$RT" run --rm -v "$W/debs:/debs:ro,Z" "$img" sh /debs/test.sh || rc=1
done
exit $rc
