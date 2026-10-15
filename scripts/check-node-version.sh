#!/bin/sh
set -eu

# Warns when code the node agent links changed since the previous release but
# NODE_VERSION did not move, which would leave nodes running old code without
# an update being offered. Usage: check-node-version.sh [previous-tag]
# Exits 1 only with STRICT=1; otherwise it reports and exits 0 because the
# check is a heuristic (hub-side code in a shared package also triggers it).

current_tag="${GITHUB_REF_NAME:-}"
previous="${1:-}"
if [ -z "$previous" ]; then
  previous="$(git tag --list 'v*.*.*' --sort=-v:refname | grep -vx "${current_tag:-none}" | head -n 1)"
fi
if [ -z "$previous" ]; then
  echo "check-node-version: no previous release tag; nothing to compare"
  exit 0
fi

module="$(go list -m)"
dirs="$(go list -deps -f '{{.ImportPath}}' ./cmd/payesh-agent | grep "^$module/" | sed "s|^$module/||" | sort -u)"
changed=""
for dir in $dirs; do
  files="$(git diff --name-only "$previous" HEAD -- "$dir" | grep '\.go$' | grep -v '_test\.go$' || true)"
  [ -n "$files" ] && changed="$changed
$files"
done

old="$(git show "$previous:NODE_VERSION" 2>/dev/null | tr -d '[:space:]' || true)"
new="$(tr -d '[:space:]' < NODE_VERSION)"
if [ -z "$changed" ]; then
  echo "check-node-version: node code unchanged since $previous; NODE_VERSION $new"
  exit 0
fi
if [ "$old" != "$new" ]; then
  echo "check-node-version: node code changed since $previous and NODE_VERSION moved ($old -> $new)"
  exit 0
fi
echo "::warning title=NODE_VERSION unchanged::Node agent code changed since $previous but NODE_VERSION is still $new, so nodes will not be offered an update." >&2
echo "Changed files linked into the agent:$changed" >&2
[ "${STRICT:-0}" = "1" ] && exit 1
exit 0
