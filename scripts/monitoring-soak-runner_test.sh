#!/bin/sh
set -eu
script=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)/monitoring-soak-runner.sh
if "$script" start --binary /bin/false --state-dir / >/dev/null 2>&1; then
  printf 'broad state directory was accepted\n' >&2
  exit 1
fi
if "$script" start --binary relative --state-dir /tmp/payesh-runner-test >/dev/null 2>&1; then
  printf 'invalid binary was accepted\n' >&2
  exit 1
fi
printf 'monitoring_soak_runner=PASS\n'
