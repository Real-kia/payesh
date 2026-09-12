#!/bin/sh
set -eu

script=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)/configure-public-access.sh
output=$($script 2>&1)
printf '%s\n' "$output" | grep -q 'WARNING: no domain was supplied'
printf '%s\n' "$output" | grep -q 'ssh -N -L 8787:127.0.0.1:8787'
printf '%s\n' "$output" | grep -q 'publicly trusted browser TLS cannot be configured'
if "$script" --domain 'bad domain' >/dev/null 2>&1; then
  printf 'invalid domain was accepted\n' >&2
  exit 1
fi
if "$script" --upstream 0.0.0.0:8787 >/dev/null 2>&1; then
  printf 'public upstream was accepted\n' >&2
  exit 1
fi
if "$script" --upstream '127.0.0.1:not-a-port' >/dev/null 2>&1; then
  printf 'invalid upstream port was accepted\n' >&2
  exit 1
fi
if "$script" --email 'bad email' >/dev/null 2>&1; then
  printf 'invalid email was accepted\n' >&2
  exit 1
fi
printf 'configure_public_access=PASS\n'
