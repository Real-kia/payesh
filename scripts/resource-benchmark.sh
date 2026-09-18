#!/bin/sh
set -eu

exec go run ./scripts/resource-benchmark "$@"
