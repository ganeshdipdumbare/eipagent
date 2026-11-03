#!/bin/sh
# Prepares a release commit for VERSION (e.g. v0.4.2):
#   sdk.Version                       sdk/secrets.go
#   go get / go install ...@vX.Y.Z     README.md, docs/site.html
#   the version badge                  docs/site.html
#   "## Unreleased" -> "## vX.Y.Z — date" in CHANGELOG.md, with a fresh
#   empty "## Unreleased" above it
# and writes that version's changelog section to release-notes.md.
# It does not commit, tag or push; .github/workflows/release.yml does.
set -eu
cd "$(dirname "$0")/.."

v=${1:?usage: scripts/release.sh vX.Y.Z}
case $v in
v[0-9]*.[0-9]*.[0-9]*) ;;
*) echo "version must look like v1.2.3, got $v" >&2; exit 1 ;;
esac
if git rev-parse -q --verify "refs/tags/$v" >/dev/null; then
  echo "tag $v already exists" >&2
  exit 1
fi

notes=$(awk '/^## Unreleased/{on=1; next} on && /^## /{exit} on' CHANGELOG.md)
if [ -z "$(printf '%s' "$notes" | tr -d '[:space:]')" ]; then
  echo "CHANGELOG.md has nothing under \"## Unreleased\"" >&2
  exit 1
fi
printf '%s\n' "$notes" | sed -e '/./,$!d' > release-notes.md

V=$v DATE=$(date -u +%F) perl -0pi -e 's/^## Unreleased\n/## Unreleased\n\n## $ENV{V} — $ENV{DATE}\n/m' CHANGELOG.md
V=${v#v} perl -pi -e 's/^const Version = ".*"/const Version = "$ENV{V}"/' sdk/secrets.go
V=$v perl -pi -e 's{(eipagent(?:/cmd/eip)?\@)v\d+\.\d+\.\d+}{$1$ENV{V}}g' README.md docs/site.html
V=$v perl -pi -e 's{<b>v\d+\.\d+\.\d+</b>}{<b>$ENV{V}</b>}g' docs/site.html

echo "prepared $v"
git --no-pager diff --stat
