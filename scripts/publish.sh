#!/usr/bin/env bash
# Publish the release scripts/release.sh built, on GitHub: its packages,
# public key, checksums and their signature, with CHANGELOG.md's section
# as the notes. Everything is checked first; nothing is published if a
# check fails.
#
#   make publish                  (scripts/publish.sh)
#   DRY_RUN=1 make publish        check, show the notes and the command, publish nothing
#   STAGE=beta make publish       name the stage in the title
#   STAGE=final make publish      a full release, not a pre-release
#
# While the major number is 0 a release is published as a pre-release,
# titled "Trellis X.Y.Z (alpha)" unless STAGE says otherwise.
#
# Needs the GitHub CLI, logged in once: sudo dnf install gh; gh auth login.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
fail() { echo "publish: $*" >&2; exit 1; }

command -v gh >/dev/null || fail "needs the GitHub CLI: sudo dnf install gh, then gh auth login"
gh auth status >/dev/null 2>&1 || fail "the GitHub CLI is not logged in: gh auth login"

tag="$(git describe --tags --always --dirty 2>/dev/null || true)"
[[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail "HEAD is $tag, not a release tag on a clean tree: check out the tag the release was built from"
V="${tag#v}"
OUT="dist/release/$V"
[ -f "$OUT/SHA256SUMS" ] || fail "no release built for $V in $OUT: run make release first"

echo "== checks"
# The tag must be on GitHub, on this commit: the release page is made from it.
remote="$(git ls-remote origin "refs/tags/$tag^{}" | cut -f1)"
[ -n "$remote" ] || remote="$(git ls-remote origin "refs/tags/$tag" | cut -f1)"
[ "$remote" = "$(git rev-parse HEAD)" ] || fail "the tag $tag is not on GitHub at this commit: git push origin $tag"
echo "ok   tag $tag on GitHub"
# Every package signed by a key the released source trusts.
go run ./tools/mkdeb verify --pub internal/release/trusted.keys "$OUT"/*.deb >/dev/null ||
  fail "a package in $OUT is not signed by a key in internal/release/trusted.keys"
echo "ok   packages signed by a built-in release key"
( cd "$OUT" && sha256sum --quiet -c SHA256SUMS ) || fail "the packages do not match SHA256SUMS"
echo "ok   checksums"
if [ -f "$OUT/SHA256SUMS.asc" ]; then
  gpg --verify "$OUT/SHA256SUMS.asc" "$OUT/SHA256SUMS" 2>/dev/null || fail "SHA256SUMS.asc does not verify"
  echo "ok   checksums signed with gpg"
else
  echo "--   no SHA256SUMS.asc (make release without GPG_KEY): published without it"
fi
notes="$(mktemp)"
trap 'rm -f "$notes"' EXIT
scripts/release-notes.sh "$V" > "$notes"
echo "ok   notes from CHANGELOG.md ($(wc -l < "$notes") lines)"
if gh release view "$tag" >/dev/null 2>&1; then
  fail "$tag is published already (gh release view $tag). To replace a file: gh release upload $tag FILE --clobber; to change the notes: gh release edit $tag --notes-file FILE"
fi

stage="${STAGE:-}"
[ -n "$stage" ] || { [ "${V%%.*}" = 0 ] && stage=alpha; }
title="Trellis $V"
flags=(--verify-tag)
if [ -n "$stage" ] && [ "$stage" != final ]; then
  title="$title ($stage)"
  flags+=(--prerelease)
fi
files=("$OUT"/*)
cmd=(gh release create "$tag" "${files[@]}" "${flags[@]}" --title "$title" --notes-file "$notes")

echo
echo "== $title, $([ "${flags[*]}" = "--verify-tag" ] && echo "a full release" || echo "a pre-release"), with ${#files[@]} files:"
printf '   %s\n' "${files[@]#"$OUT"/}"
if [ -n "${DRY_RUN:-}" ]; then
  echo
  echo "== notes"
  cat "$notes"
  echo
  echo "== dry run: would run"
  printf ' %q' "${cmd[@]}"
  echo
  exit 0
fi
"${cmd[@]}"
echo
gh release view "$tag"
