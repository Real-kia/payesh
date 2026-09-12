#!/bin/sh
set -eu

# Signing is an explicit owner action. The key path and key ID are required
# arguments; no key is generated or stored in this repository.
exec go run ./scripts/release-sign "$@"
