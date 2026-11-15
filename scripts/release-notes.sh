#!/usr/bin/env bash
# Print CHANGELOG.md's section for a release, for its release page: the
# lines under "## X.Y.Z…" up to the next "## ", without the heading (the
# page has a title), with links to the repository's files made absolute,
# since a release page does not resolve relative ones. Fails when the
# changelog has no section for the version.
#
#   scripts/release-notes.sh 0.2.0
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
V="${1:?usage: scripts/release-notes.sh VERSION}"

# The repository on GitHub, from the remote (git@github.com:owner/repo.git
# or https://github.com/owner/repo).
repo="$(git remote get-url origin 2>/dev/null | sed -E 's#^(git@github\.com:|https://github\.com/)##; s#\.git$##')"
[ -n "$repo" ] || repo="ifHoncho/trellis-core"

body="$(awk -v v="$V" '
  /^## / {
    f = 0; h = substr($0, 4)
    if (h == v || index(h, v ":") == 1 || index(h, v " ") == 1) { f = 1; next }
  }
  f' CHANGELOG.md | sed '/./,$!d')"
if [ -z "$(printf '%s' "$body" | tr -d '[:space:]')" ]; then
  echo "CHANGELOG.md has no section for $V: add one headed \"## $V: …\" above the last release's, commit it, and tag again." >&2
  exit 1
fi
printf '%s\n' "$body" |
  sed -E "s#\]\((((docs|scripts|packaging|internal|tools|configs|lab)/[^)]*)|[A-Za-z]+\.md)\)#](https://github.com/$repo/blob/v$V/\1)#g"
