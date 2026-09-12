#!/usr/bin/env bash
set -Eeuo pipefail

# Disposable installer acceptance. This never touches /, a service manager,
# or a real host: the Go gate creates a private filesystem root and injects
# account/supervisor boundaries. Use this before a live-host acceptance run.
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  cat <<'EOF'
Usage: scripts/install-acceptance.sh

Runs the clean install, replacement upgrade, failed-verification recovery,
resume, and data-preserving/explicit-data-removal acceptance cycle in an
isolated temporary filesystem fixture.
EOF
  exit 0
fi
command -v go >/dev/null 2>&1 || { echo 'required command is unavailable: go' >&2; exit 2; }
cd -- "$repo_root"
cache_root=${GOCACHE:-${TMPDIR:-/tmp}/payesh-go-cache}
mkdir -p -- "$cache_root"
echo '[install-acceptance] disposable clean-host lifecycle'
GOCACHE="$cache_root" go test ./internal/install -run '^TestDisposableInstallUpgradeRecoveryUninstallAcceptance$' -count=1 -v
echo '[install-acceptance] traversal/symlink safety'
GOCACHE="$cache_root" go test ./internal/install -run '^TestUninstallRefusesSymlinkOwnedPath$' -count=1 -v
echo 'install_acceptance=PASS'
