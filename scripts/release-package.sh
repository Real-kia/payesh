#!/bin/sh
set -eu

# Keep the release builder in Go so archive creation and cross-build behavior
# are identical on macOS and Linux. All arguments are passed through.
exec go run ./scripts/release-package "$@"
