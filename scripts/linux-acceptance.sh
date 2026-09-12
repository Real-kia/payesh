#!/usr/bin/env bash
set -Eeuo pipefail

failed_command=''
on_error() {
  local status=$?
  failed_command=${BASH_COMMAND:-unknown}
  printf '[error] stage=local line=%s status=%s command=%q\n' "${BASH_LINENO[0]:-unknown}" "$status" "$failed_command" >&2
  return "$status"
}
trap on_error ERR

# Reusable disposable-Linux acceptance runner.
#
# Passwords are never stored here. Use an SSH key/agent, or provide SSHPASS in
# the environment when sshpass is installed:
#
#   PAYESH_SSH_HOST=203.0.113.10 \
#   PAYESH_SSH_BIND_ADDRESS=192.0.2.10 \
#   SSHPASS='...' scripts/linux-acceptance.sh
#
# Optional environment:
#   PAYESH_SSH_USER (default: root)
#   PAYESH_SSH_PORT (default: 22)
#   PAYESH_GOARCH (default: detected from remote uname -m)
#   PAYESH_GOARM (for 32-bit ARM hosts, default: 7)
#   PAYESH_SSH_RETRIES (default: 2 transport retries)
#   PAYESH_SSH_CONNECT_TIMEOUT (default: 20 seconds)
#   PAYESH_SSH_SERVER_ALIVE_INTERVAL (default: 15 seconds)
#   PAYESH_SSH_SERVER_ALIVE_COUNT (default: 3 probes)
#   PAYESH_SSH_TRANSFER_TIMEOUT (default: 180 seconds)
#   PAYESH_REMOTE_TMP (default: generated /tmp directory)
#   PAYESH_LINUX_THROUGHPUT=1 (also run the bounded tc throughput probe)
#   PAYESH_LINUX_QUOTA_OVERSHOOT=1 (also measure durable quota-sample overshoot)
#   PAYESH_ACCEPTANCE_REUSE_OWNED_PORT_TABLE=1 (allow an already-owned nft table)
#   PAYESH_ACCEPTANCE_PROFILE=all|core|kernel|cpu|bandwidth|nft|network|modules (default: all)

usage() {
  cat <<'EOF'
Usage: PAYESH_SSH_HOST=<host> [SSHPASS=<password>] scripts/linux-acceptance.sh

The runner detects the remote CPU architecture, builds matching Linux
artifacts locally, uploads one compressed bundle to a unique disposable /tmp
directory, runs monitoring, CPU cgroup, tc, nftables, disposable network-topology,
module-socket, and capability checks,
then removes all remote state.
EOF
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  usage
  exit 0
fi

host="${PAYESH_SSH_HOST:-}"
user="${PAYESH_SSH_USER:-root}"
port="${PAYESH_SSH_PORT:-22}"
bind_address="${PAYESH_SSH_BIND_ADDRESS:-}"
connect_timeout="${PAYESH_SSH_CONNECT_TIMEOUT:-20}"
server_alive_interval="${PAYESH_SSH_SERVER_ALIVE_INTERVAL:-15}"
server_alive_count="${PAYESH_SSH_SERVER_ALIVE_COUNT:-3}"
transfer_timeout="${PAYESH_SSH_TRANSFER_TIMEOUT:-180}"
retries="${PAYESH_SSH_RETRIES:-2}"
profile="${PAYESH_ACCEPTANCE_PROFILE:-all}"
if [[ -z "$host" ]]; then
  usage >&2
  exit 2
fi
for required in go tar ssh mktemp; do
  if ! command -v "$required" >/dev/null 2>&1; then
    echo "required local command is unavailable: $required" >&2
    exit 2
  fi
done
if ! [[ "$port" =~ ^[0-9]+$ ]]; then
  echo "PAYESH_SSH_PORT must be numeric" >&2
  exit 2
fi
for timeout_value in "$connect_timeout" "$server_alive_interval" "$server_alive_count" "$transfer_timeout"; do
  if ! [[ "$timeout_value" =~ ^[1-9][0-9]*$ ]]; then
    echo "SSH timeout/keepalive values must be positive integers" >&2
    exit 2
  fi
done
if ! [[ "$retries" =~ ^[1-9][0-9]*$ ]]; then
  echo "PAYESH_SSH_RETRIES must be a positive integer" >&2
  exit 2
fi
case "$profile" in all|core|kernel|cpu|bandwidth|nft|network|modules) ;; *) echo "PAYESH_ACCEPTANCE_PROFILE must be all, core, kernel, cpu, bandwidth, nft, network, or modules" >&2; exit 2;; esac
remote_acceptance_env="PAYESH_ACCEPTANCE_PROFILE=$profile "
case "${PAYESH_LINUX_THROUGHPUT:-0}" in
  0|'') ;;
  1) remote_acceptance_env+="PAYESH_LINUX_THROUGHPUT=1 ";;
  *) echo "PAYESH_LINUX_THROUGHPUT must be 0 or 1" >&2; exit 2 ;;
esac
case "${PAYESH_LINUX_QUOTA_OVERSHOOT:-0}" in
  0|'') ;;
  1) remote_acceptance_env+="PAYESH_LINUX_QUOTA_OVERSHOOT=1 ";;
  *) echo "PAYESH_LINUX_QUOTA_OVERSHOOT must be 0 or 1" >&2; exit 2 ;;
esac
case "${PAYESH_ACCEPTANCE_REUSE_OWNED_PORT_TABLE:-0}" in
  0|'') ;;
  1) remote_acceptance_env+="PAYESH_ACCEPTANCE_REUSE_OWNED_PORT_TABLE=1 ";;
  *) echo "PAYESH_ACCEPTANCE_REUSE_OWNED_PORT_TABLE must be 0 or 1" >&2; exit 2 ;;
esac

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
scratch=$(mktemp -d "${TMPDIR:-/tmp}/payesh-linux-acceptance.XXXXXX")
known_hosts="$scratch/known_hosts"
remote_dir="${PAYESH_REMOTE_TMP:-/tmp/payesh-linux-acceptance-$(date +%s)-$$}"
if ! [[ "$remote_dir" =~ ^/tmp/payesh-linux-acceptance-[A-Za-z0-9._-]+$ ]]; then
  echo "PAYESH_REMOTE_TMP must be a dedicated path under /tmp/payesh-linux-acceptance-*" >&2
  exit 2
fi

cleanup_local() {
  rm -rf -- "$scratch"
}
trap cleanup_local EXIT

ssh_options=(
  -o StrictHostKeyChecking=accept-new
  -o UserKnownHostsFile="$known_hosts"
  -o ConnectTimeout="$connect_timeout"
  -o ServerAliveInterval="$server_alive_interval"
  -o ServerAliveCountMax="$server_alive_count"
  -p "$port"
)
if [[ -n "$bind_address" ]]; then
  ssh_options+=(-b "$bind_address")
fi
ssh_target="$user@$host"
cleanup_ssh_options=(
  -o StrictHostKeyChecking=accept-new
  -o UserKnownHostsFile="$known_hosts"
  -o ConnectTimeout=5
  -o ServerAliveInterval=2
  -o ServerAliveCountMax=1
  -p "$port"
)
if [[ -n "$bind_address" ]]; then
  cleanup_ssh_options+=(-b "$bind_address")
fi

cleanup_remote() {
  # The remote script has its own EXIT trap. Keep this outer fallback bounded
  # so a broken SSH banner cannot strand the local runner during cleanup.
  local cleanup_cmd="rm -rf -- '$remote_dir'"
  if [[ -n "${SSHPASS:-}" ]]; then
    sshpass -e ssh "${cleanup_ssh_options[@]}" "$ssh_target" "$cleanup_cmd" >/dev/null 2>&1 || true
  else
    ssh "${cleanup_ssh_options[@]}" "$ssh_target" "$cleanup_cmd" >/dev/null 2>&1 || true
  fi
}

cleanup_all() {
  cleanup_remote
  cleanup_local
}

remote() {
  if [[ -n "${SSHPASS:-}" ]]; then
    command -v sshpass >/dev/null 2>&1 || { echo "SSHPASS is set but sshpass is unavailable" >&2; return 2; }
    sshpass -e ssh "${ssh_options[@]}" "$ssh_target" "$@"
  else
    ssh "${ssh_options[@]}" "$ssh_target" "$@"
  fi
}

remote_arch=$(remote 'uname -m')
goarch="${PAYESH_GOARCH:-}"
goarm="${PAYESH_GOARM:-}"
if [[ -z "$goarch" ]]; then
  case "$remote_arch" in
    x86_64|amd64) goarch=amd64 ;;
    aarch64|arm64) goarch=arm64 ;;
    armv7l|armv7*) goarch=arm; goarm="${goarm:-7}" ;;
    armv6l|armv6*) goarch=arm; goarm="${goarm:-6}" ;;
    *)
      echo "unsupported remote architecture: $remote_arch (set PAYESH_GOARCH to override)" >&2
      exit 2
      ;;
  esac
fi
case "$goarch" in
  amd64|arm64) ;;
  arm)
    if [[ -z "$goarm" ]]; then goarm=7; fi
    if ! [[ "$goarm" =~ ^[6-7]$ ]]; then
      echo "PAYESH_GOARM must be 6 or 7 for GOARCH=arm" >&2
      exit 2
    fi
    ;;
  *)
    echo "unsupported PAYESH_GOARCH=$goarch (supported: amd64, arm64, arm)" >&2
    exit 2
    ;;
esac
arch_label="$goarch"
if [[ "$goarch" == "arm" ]]; then arch_label+="v$goarm"; fi
echo "[build] Linux $arch_label acceptance binaries (remote=$remote_arch)"
cache="${GOCACHE:-$scratch/go-cache}"
goarm_env=()
if [[ "$goarch" == "arm" ]]; then goarm_env+=(GOARM="$goarm"); fi
if [[ "$profile" == all || "$profile" == core || "$profile" == modules ]]; then
  for target in payesh-agent payesh payesh-server; do
    env GOCACHE="$cache" GOOS=linux GOARCH="$goarch" "${goarm_env[@]}" go build -trimpath -ldflags='-s -w' -o "$scratch/$target-linux" "./cmd/$target"
  done
fi
if [[ "$profile" == all || "$profile" == kernel || "$profile" == nft ]]; then
  env GOCACHE="$cache" GOOS=linux GOARCH="$goarch" "${goarm_env[@]}" go test -c -ldflags='-s -w' -o "$scratch/porttraffic-acceptance" ./internal/porttraffic
fi
if [[ "$profile" == all || "$profile" == kernel || "$profile" == bandwidth ]]; then
  env GOCACHE="$cache" GOOS=linux GOARCH="$goarch" "${goarm_env[@]}" go test -c -ldflags='-s -w' -o "$scratch/bandwidth-acceptance" ./internal/bandwidth
fi
if [[ "$profile" == all || "$profile" == kernel || "$profile" == cpu ]]; then
  env GOCACHE="$cache" GOOS=linux GOARCH="$goarch" "${goarm_env[@]}" go test -c -ldflags='-s -w' -o "$scratch/cpucontrol-acceptance" ./internal/cpucontrol
fi
if [[ "$profile" == all || "$profile" == modules ]]; then
  env GOCACHE="$cache" GOOS=linux GOARCH="$goarch" "${goarm_env[@]}" go build -trimpath -ldflags='-s -w' -o "$scratch/payesh-bandwidth-module-linux" ./cmd/payesh-bandwidth-module
fi
cp "$repo_root/scripts/linux-acceptance-remote.sh" "$scratch/linux-acceptance-remote.sh"
cp "$repo_root/scripts/linux-network-acceptance.sh" "$scratch/linux-network-acceptance.sh"

echo "[upload] $ssh_target:$remote_dir"
trap cleanup_all EXIT
bundle_files=(linux-acceptance-remote.sh linux-network-acceptance.sh)
if [[ "$profile" == all || "$profile" == core || "$profile" == modules ]]; then bundle_files+=(payesh-agent-linux payesh-linux payesh-server-linux); fi
if [[ "$profile" == all || "$profile" == kernel || "$profile" == nft ]]; then bundle_files+=(porttraffic-acceptance); fi
if [[ "$profile" == all || "$profile" == kernel || "$profile" == bandwidth ]]; then bundle_files+=(bandwidth-acceptance); fi
if [[ "$profile" == all || "$profile" == kernel || "$profile" == cpu ]]; then bundle_files+=(cpucontrol-acceptance); fi
if [[ "$profile" == all || "$profile" == modules ]]; then bundle_files+=(payesh-bandwidth-module-linux); fi
bundle="$scratch/bundle.tar.gz"
tar -C "$scratch" -czf "$bundle" "${bundle_files[@]}"
bundle_bytes=$(wc -c < "$bundle" | tr -d ' ')
echo "[upload] compressed_bytes=$bundle_bytes timeout=${transfer_timeout}s attempts=$retries"

run_upload_with_timeout() {
  local pid elapsed next_progress
  if [[ -n "${SSHPASS:-}" ]]; then
    sshpass -e ssh "${ssh_options[@]}" "$ssh_target" \
      "rm -rf -- '$remote_dir' && mkdir -m 700 -- '$remote_dir' && tar -C '$remote_dir' -xzf - && ${remote_acceptance_env}bash '$remote_dir/linux-acceptance-remote.sh'" < "$bundle" &
  else
    ssh "${ssh_options[@]}" "$ssh_target" \
      "rm -rf -- '$remote_dir' && mkdir -m 700 -- '$remote_dir' && tar -C '$remote_dir' -xzf - && ${remote_acceptance_env}bash '$remote_dir/linux-acceptance-remote.sh'" < "$bundle" &
  fi
  pid=$!
  elapsed=0
  next_progress=30
  while kill -0 "$pid" 2>/dev/null; do
    if (( elapsed >= transfer_timeout )); then
      echo "[timeout] SSH transfer exceeded ${transfer_timeout}s" >&2
      if command -v pkill >/dev/null 2>&1; then pkill -TERM -P "$pid" 2>/dev/null || true; fi
      kill -TERM "$pid" 2>/dev/null || true
      sleep 1
      if command -v pkill >/dev/null 2>&1; then pkill -KILL -P "$pid" 2>/dev/null || true; fi
      kill -KILL "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
      return 124
    fi
    if (( elapsed >= next_progress )); then
      echo "[upload] still transferring/running elapsed=${elapsed}s compressed_bytes=$bundle_bytes" >&2
      next_progress=$((next_progress + 30))
    fi
    sleep 1
    elapsed=$((elapsed + 1))
  done
  wait "$pid"
}

attempt=1
while :; do
  set +e
  run_upload_with_timeout
  status=$?
  set -e
  if (( status == 0 )); then break; fi
  if (( (status == 255 || status == 124) && attempt < retries )); then
    echo "[retry] SSH transfer failed or timed out; retrying upload ($((attempt + 1))/$retries)" >&2
    attempt=$((attempt + 1))
    sleep 2
    continue
  fi
  exit "$status"
done

echo "[done] remote temporary state removed: $remote_dir"
