#!/usr/bin/env bash
set -euo pipefail

# Read-only shared-host inventory. This script never installs, binds, kills,
# reconfigures, or writes to the host being inspected.

usage() {
  cat <<'USAGE'
Usage: shared-server-audit.sh [options]

Read-only host inventory and passive resource snapshot.

Options:
  -start PORT       first candidate for -unused-port (default: 18000)
  -end PORT         last candidate for -unused-port (default: 18999)
  -unused-port      print one currently unbound TCP/UDP port and exit
  -sample SECONDS   take a second passive /proc/net/dev sample (default: 0)
  -h                show this help

The unused-port result is advisory only: it does not bind or reserve a port,
and another process can claim it immediately after the check.
USAGE
}

start=18000
end=18999
sample=0
unused=0

while (($#)); do
  case "$1" in
    -start) [[ $# -ge 2 ]] || { echo "-start requires a port" >&2; exit 2; }; start=$2; shift 2 ;;
    -end) [[ $# -ge 2 ]] || { echo "-end requires a port" >&2; exit 2; }; end=$2; shift 2 ;;
    -unused-port) unused=1; shift ;;
    -sample) [[ $# -ge 2 ]] || { echo "-sample requires seconds" >&2; exit 2; }; sample=$2; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
done

is_uint() { [[ "$1" =~ ^[0-9]+$ ]]; }
is_uint "$start" && is_uint "$end" && is_uint "$sample" || { echo "ports and sample must be unsigned integers" >&2; exit 2; }
start_n=$((10#$start))
end_n=$((10#$end))
sample_n=$((10#$sample))
(( start_n >= 1 && start_n <= 65535 && end_n >= start_n && end_n <= 65535 )) || { echo "invalid port range" >&2; exit 2; }
(( sample_n <= 3600 )) || { echo "sample is limited to 3600 seconds" >&2; exit 2; }

have() { command -v "$1" >/dev/null 2>&1; }

port_in_use() {
  local port=$1
  # Fail closed when ss is unavailable; parsing /proc socket tables is easy to
  # get wrong across kernel versions and could select an occupied port.
  have ss || return 0
  ss -H -lntu 2>/dev/null | awk -v needle=":$port" '
    { addr=$4; sub(/%.*/, "", addr); if (addr ~ needle "$" ) found=1 }
    END { exit found ? 0 : 1 }
  '
}

find_unused_port() {
  have ss || { echo "ss is required for a fail-closed port check" >&2; return 1; }
  local port
  for ((port=start_n; port<=end_n; port++)); do
    if ! port_in_use "$port"; then
      printf '%s\n' "$port"
      return 0
    fi
  done
  echo "no currently unbound port in $start_n-$end_n" >&2
  return 1
}

if (( unused )); then
  find_unused_port
  exit $?
fi

section() { printf '\n===== %s =====\n' "$1"; }
section identity
id 2>/dev/null || true
hostname 2>/dev/null || true
uptime 2>/dev/null || true

section os_kernel_init
cat /etc/os-release 2>/dev/null || true
uname -a 2>/dev/null || true
readlink /proc/1/exe 2>/dev/null || true
ps -p 1 -o pid=,comm=,args= 2>/dev/null || true

section listening_sockets
if have ss; then ss -lntup 2>/dev/null || ss -lntu 2>/dev/null || true; else echo "ss=absent (port inventory unavailable)"; fi

section interfaces_routes
if have ip; then ip -brief addr 2>/dev/null || true; ip route show 2>/dev/null || true; else echo "ip=absent"; fi

section resources
cat /proc/loadavg 2>/dev/null || true
free -h 2>/dev/null || true
df -hT 2>/dev/null || true
cat /proc/pressure/cpu /proc/pressure/memory 2>/dev/null || true

section processes
ps -eo pid=,user=,stat=,pcpu=,pmem=,rss=,etime=,comm= --sort=-pcpu 2>/dev/null | sed -n '1,16p' || true

section cgroups_and_init
stat -fc %T /sys/fs/cgroup 2>/dev/null || true
if have systemctl; then systemctl is-system-running 2>&1 || true; fi

section available_tools
for command_name in ss ip nft iptables-save tc ethtool systemctl journalctl podman docker lsof fuser nsenter curl openssl; do
  if have "$command_name"; then printf '%-16s %s\n' "$command_name" "$(command -v "$command_name")"; else printf '%-16s absent\n' "$command_name"; fi
done

section passive_network_counters
cat /proc/net/dev 2>/dev/null || true
if (( sample_n > 0 )); then
  sleep "$sample_n"
  printf '\n--- after %ss ---\n' "$sample_n"
  cat /proc/net/dev 2>/dev/null || true
fi
