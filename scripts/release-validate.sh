#!/bin/sh
set -eu

exec go run ./scripts/release-validate "$@"
