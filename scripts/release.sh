#!/usr/bin/env bash
# Build a release: dist/release/<version>/ with the signed packages for
# every architecture, the batman-adv package, trellis-deploy for Linux,
# macOS and Windows, and the checksums of them all.
#
#   TRELLIS_RELEASE_KEY=~/keys/trellis-release.key scripts/release.sh
#
# The release is the tag at HEAD (vX.Y.Z) on a clean tree. Its packages are
# signed with the release key (tools/mkdeb keygen makes one), whose public
# line must be in internal/release/trusted.keys: that is what the nodes this
# release installs on will trust for the next one.
#
#   GPG_KEY=<key id>   also sign SHA256SUMS with gpg (SHA256SUMS.asc), for
#                      people checking a download by hand
#   ALLOW_DEV=1        build from an untagged or modified tree (a trial run;
#                      signed only with a key the nodes do not trust)
#   UNSIGNED=1         build without a release key (a trial run)
#
# A release needs its section in CHANGELOG.md ("## X.Y.Z: …"), checked
# before anything is built: it becomes the notes of the release page that
# scripts/publish.sh (make publish) makes afterwards.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

desc="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
VERSION="${desc#v}"
if ! [[ "$desc" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  [ -n "${ALLOW_DEV:-}" ] || { echo "HEAD is $desc, not a release tag (vX.Y.Z) on a clean tree. Tag it, or set ALLOW_DEV=1 for a trial run." >&2; exit 1; }
  echo "-- trial run: $desc is not a release"
else
  scripts/release-notes.sh "$VERSION" > /dev/null
fi
KEY="${TRELLIS_RELEASE_KEY:-}"
if [ -z "$KEY" ]; then
  [ -n "${UNSIGNED:-}" ] || { echo "set TRELLIS_RELEASE_KEY to the release secret key file (go run ./tools/mkdeb keygen --out FILE makes one), or UNSIGNED=1 for a trial run" >&2; exit 1; }
  echo "-- trial run: the packages will not be signed"
else
  pub="$(go run ./tools/mkdeb pubkey --key "$KEY" | cut -d' ' -f1-3)"
  trusted=no
  grep -q -F "$pub" internal/release/trusted.keys && trusted=yes
  if [ -n "${ALLOW_DEV:-}" ] && [ "$trusted" = yes ]; then
    # Signed by a key nodes trust, a trial build would be installed by
    # every node it is offered to, the web UI included.
    echo "A trial run must not be signed with a release key the nodes trust: use a throwaway key (go run ./tools/mkdeb keygen) or UNSIGNED=1." >&2
    exit 1
  fi
  if [ "$trusted" = no ] && [ -z "${ALLOW_DEV:-}" ]; then
    echo "The release key's public line is not in internal/release/trusted.keys:" >&2
    echo "  $(go run ./tools/mkdeb pubkey --key "$KEY")" >&2
    echo "Nodes built from this source would refuse the next release. Add the line, commit, tag again." >&2
    exit 1
  fi
fi

make -s check
OUT="dist/release/$VERSION"
rm -rf "$OUT"
# A trial run signs what is not a release only because it was asked to.
signflags=""; [ -n "${ALLOW_DEV:-}" ] && signflags=--sign-dev
make -s deb VERSION="$VERSION" DEB_OUT="$OUT" TRELLIS_RELEASE_KEY="$KEY" DEB_SIGN_FLAGS="$signflags"
if [ -n "$KEY" ]; then
  go run ./tools/mkdeb pubkey --key "$KEY" > "$OUT/release-key.pub"
  go run ./tools/mkdeb verify --pub "$OUT/release-key.pub" "$OUT"/*.deb
fi
# trellis-deploy, for the operator's computer: covered by the checksums
# (and their gpg signature), not by the packages' Ed25519 signature.
make -s deploy-tool VERSION="$VERSION" DEPLOY_TOOL_OUT="$OUT"
( cd "$OUT" && sha256sum -- *.deb trellis-deploy_* > SHA256SUMS )
if [ -n "${GPG_KEY:-}" ]; then
  gpg --batch --yes --local-user "$GPG_KEY" --armor --detach-sign --output "$OUT/SHA256SUMS.asc" "$OUT/SHA256SUMS"
fi
echo
echo "== release $VERSION in $OUT"
ls -l "$OUT"
if [[ "$desc" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] && [ -n "$KEY" ]; then
  echo
  echo "Next: try it on a test node (sudo meshctl upgrade $OUT/trellis_${VERSION}_<arch>.deb), then: make publish"
fi
