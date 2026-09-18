#!/usr/bin/env bash
set -Eeuo pipefail

# Disposable Linux network acceptance probe.  It exercises kernel topology
# primitives in private network namespaces and only creates names containing
# this process' PID.  It never flushes a ruleset or changes a foreign link.
#
# The probe intentionally reports a missing container runtime as unsupported:
# a network namespace is a useful kernel-level substitute, but it is not proof
# of Docker/Podman integration.

stage=bootstrap
work=$(mktemp -d "${TMPDIR:-/tmp}/payesh-network-acceptance.XXXXXX")
tag="${$}"
client_ns="pyacc_c_${tag}"
server_ns="pyacc_s_${tag}"
bridge_a="pyacc_a_${tag}"
bridge_b="pyacc_b_${tag}"
bridge="pyacc_br_${tag}"
nat_client="pyacc_nc_${tag}"
nat_server="pyacc_ns_${tag}"
tun="pyacc_tun_${tag}"
nat_table="payesh_acceptance_nat_${tag}"
forwarding_before=''
unsupported=0

on_error() {
  local status=$?
  printf '[error] network stage=%s line=%s status=%s command=%q\n' \
    "$stage" "${BASH_LINENO[0]:-unknown}" "$status" "${BASH_COMMAND:-unknown}" >&2
  return "$status"
}
trap on_error ERR

mark_unsupported() {
  printf '%s=UNSUPPORTED reason=%s\n' "$1" "$2"
  unsupported=$((unsupported + 1))
}

cleanup() {
  local pid
  for pid in ${server_pid:-} ${nat_server_pid:-}; do
    [[ -n "$pid" ]] && kill "$pid" 2>/dev/null || true
  done
  nft delete table ip "$nat_table" 2>/dev/null || true
  ip link del "$tun" 2>/dev/null || true
  ip link del "$bridge" 2>/dev/null || true
  ip link del "$bridge_a" 2>/dev/null || true
  ip link del "$bridge_b" 2>/dev/null || true
  ip link del "$nat_client" 2>/dev/null || true
  ip link del "$nat_server" 2>/dev/null || true
  ip netns del "$client_ns" 2>/dev/null || true
  ip netns del "$server_ns" 2>/dev/null || true
  if [[ -n "$forwarding_before" ]]; then
    sysctl -q -w "net.ipv4.ip_forward=$forwarding_before" 2>/dev/null || true
  fi
  rm -rf -- "$work"
}
trap cleanup EXIT

require_commands() {
  local command_name
  for command_name in ip nft sysctl python3; do
    if ! command -v "$command_name" >/dev/null 2>&1; then
      mark_unsupported network_primitives "${command_name}-not-installed"
      return 1
    fi
  done
  if (( EUID != 0 )); then
    mark_unsupported network_primitives root-or-cap-net-admin-required
    return 1
  fi
  return 0
}

ensure_absent() {
  local kind=$1 name=$2
  case "$kind" in
    link) ! ip link show dev "$name" >/dev/null 2>&1 ;;
    ns) ! ip netns list | awk '{print $1}' | grep -Fxq "$name" ;;
    table) ! nft list table ip "$name" >/dev/null 2>&1 ;;
    *) return 1 ;;
  esac
}

add_veth_to_ns() {
  local host_if=$1 peer_if=$2 ns=$3
  ensure_absent link "$host_if" && ensure_absent link "$peer_if" || return 1
  ip link add "$host_if" type veth peer name "$peer_if" || return 1
  ip link set "$peer_if" netns "$ns" || return 1
  ip link set "$host_if" up || return 1
}

namespace_tcp_roundtrip() {
  local listen_ns=$1 listen_addr=$2 client_ns_arg=$3 output=$4
  local pid=''
  local port_file="$output.port"
  local listen_ip="${listen_addr%:*}"
  local server_command=(python3)
  if [[ "$listen_ns" != host ]]; then
    server_command=(ip netns exec "$listen_ns" python3)
  fi
  rm -f -- "$port_file"
  "${server_command[@]}" -c \
    'import pathlib,socket,sys
s=socket.socket(socket.AF_INET,socket.SOCK_STREAM)
s.bind((sys.argv[1],0)); s.listen(1)
pathlib.Path(sys.argv[3]).write_text(str(s.getsockname()[1]), encoding="ascii")
c,peer=s.accept(); print(peer[0],flush=True); c.recv(16); c.sendall(b"payesh-ok"); c.close(); s.close()' \
    "$listen_ip" 0 "$port_file" >"$output" 2>&1 &
  pid=$!
  local port=''
  for _ in $(seq 1 40); do
    if [[ -s "$port_file" ]]; then port=$(tr -d '[:space:]' < "$port_file"); break; fi
    sleep 0.05
  done
  if [[ ! "$port" =~ ^[0-9]+$ ]]; then
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
    return 1
  fi
  ip netns exec "$client_ns_arg" python3 -c \
    'import socket,sys
s=socket.create_connection((sys.argv[1],int(sys.argv[2])),3); s.sendall(b"probe"); data=s.recv(32); s.close()
assert data == b"payesh-ok", data' "$listen_ip" "$port" || {
      kill "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
      return 1
    }
  wait "$pid" || return 1
  rm -f -- "$port_file"
}

test_veth_namespace() {
  stage=veth_namespace
  ip netns add "$client_ns" || return 1
  add_veth_to_ns "pyacc_vh_${tag}" "pyacc_vp_${tag}" "$client_ns" || return 1
  ip addr add 198.18.110.1/30 dev "pyacc_vh_${tag}" || return 1
  ip netns exec "$client_ns" ip link set lo up || return 1
  ip netns exec "$client_ns" ip addr add 198.18.110.2/30 dev "pyacc_vp_${tag}" || return 1
  ip netns exec "$client_ns" ip link set "pyacc_vp_${tag}" up || return 1
  namespace_tcp_roundtrip host 198.18.110.1:18110 "$client_ns" "$work/veth.log" || return 1
  printf 'network_veth_namespace=PASS\n'
}

test_bridge() {
  stage=bridge_namespace
  ip netns add "$server_ns" || return 1
  add_veth_to_ns "$bridge_a" "pyacc_ap_${tag}" "$client_ns" || return 1
  add_veth_to_ns "$bridge_b" "pyacc_bp_${tag}" "$server_ns" || return 1
  ip link add "$bridge" type bridge || return 1
  ip link set "$bridge" up || return 1
  ip link set "$bridge_a" master "$bridge" || return 1
  ip link set "$bridge_b" master "$bridge" || return 1
  ip netns exec "$client_ns" ip link set lo up || return 1
  ip netns exec "$server_ns" ip link set lo up || return 1
  ip netns exec "$client_ns" ip addr add 198.18.111.1/24 dev "pyacc_ap_${tag}" || return 1
  ip netns exec "$server_ns" ip addr add 198.18.111.2/24 dev "pyacc_bp_${tag}" || return 1
  ip netns exec "$client_ns" ip link set "pyacc_ap_${tag}" up || return 1
  ip netns exec "$server_ns" ip link set "pyacc_bp_${tag}" up || return 1
  namespace_tcp_roundtrip "$server_ns" 198.18.111.2:18111 "$client_ns" "$work/bridge.log" || return 1
  printf 'network_bridge_veth=PASS\n'
}

test_nat() {
  stage=nat_namespace
  # Reuse the two namespaces, but replace the bridge topology with routed
  # veths.  The bridge and its peers are removed before this function runs.
  ip link del "$bridge" 2>/dev/null || true
  ip link del "$bridge_a" 2>/dev/null || true
  ip link del "$bridge_b" 2>/dev/null || true
  ip netns del "$client_ns" 2>/dev/null || true
  ip netns del "$server_ns" 2>/dev/null || true
  ip netns add "$client_ns" || return 1
  ip netns add "$server_ns" || return 1
  add_veth_to_ns "$nat_client" "pyacc_ncp_${tag}" "$client_ns" || return 1
  add_veth_to_ns "$nat_server" "pyacc_nsp_${tag}" "$server_ns" || return 1
  ip addr add 198.18.112.1/30 dev "$nat_client" || return 1
  ip addr add 198.18.113.1/30 dev "$nat_server" || return 1
  ip netns exec "$client_ns" ip link set lo up || return 1
  ip netns exec "$server_ns" ip link set lo up || return 1
  ip netns exec "$client_ns" ip addr add 198.18.112.2/30 dev "pyacc_ncp_${tag}" || return 1
  ip netns exec "$server_ns" ip addr add 198.18.113.2/30 dev "pyacc_nsp_${tag}" || return 1
  ip netns exec "$client_ns" ip link set "pyacc_ncp_${tag}" up || return 1
  ip netns exec "$server_ns" ip link set "pyacc_nsp_${tag}" up || return 1
  ip netns exec "$client_ns" ip route add default via 198.18.112.1 || return 1
  ip netns exec "$server_ns" ip route add default via 198.18.113.1 || return 1
  forwarding_before=$(sysctl -n net.ipv4.ip_forward) || return 1
  sysctl -q -w net.ipv4.ip_forward=1 || return 1
  ensure_absent table "$nat_table" || return 1
  nft -f - <<EOF || return 1
add table ip $nat_table
add chain ip $nat_table postrouting { type nat hook postrouting priority 100; policy accept; }
add rule ip $nat_table postrouting oifname "$nat_server" ip saddr 198.18.112.0/30 masquerade
EOF
  namespace_tcp_roundtrip "$server_ns" 198.18.113.2:18112 "$client_ns" "$work/nat.log" || return 1
  # The server writes the peer address.  It must be the router-side address,
  # demonstrating source NAT rather than a mere routed namespace connection.
  if ! grep -Fqx '198.18.113.1' "$work/nat.log"; then
    printf 'unexpected NAT peer: ' >&2
    sed -n '1,2p' "$work/nat.log" >&2 || true
    return 1
  fi
  printf 'network_nat=PASS\n'
}

test_tunnel() {
  stage=tunnel_device
  if [[ ! -c /dev/net/tun ]]; then
    mark_unsupported network_tunnel /dev/net/tun-missing
    return 0
  fi
  ensure_absent link "$tun" || return 1
  ip tuntap add dev "$tun" mode tun user root || return 2
  ip addr add 198.18.114.1/32 dev "$tun" || return 1
  ip link set "$tun" up || return 1
  ip -d link show dev "$tun" | grep -q 'tun' || return 1
  printf 'network_tun=PASS\n'
}

test_offload() {
  stage=offload_paths
  if ! command -v ethtool >/dev/null 2>&1; then
    mark_unsupported network_offload ethtool-not-installed
    return 0
  fi
  # Read only a disposable veth created by test_veth_namespace. Do not turn
  # off checksumming/GSO/GRO on a provider or production interface: changing
  # those flags could disrupt an unrelated workload.
  local iface="pyacc_vh_${tag}"
  if ! ip link show dev "$iface" >/dev/null 2>&1; then
    mark_unsupported network_offload disposable-veth-unavailable
    return 0
  fi
  local features
  if ! features=$(ethtool -k "$iface" 2>&1); then
    mark_unsupported network_offload ethtool-query-failed
    return 0
  fi
  local observed=''
  for feature in tx-checksumming generic-segmentation-offload generic-receive-offload; do
    if grep -Eq "^${feature}:[[:space:]]+(on|off)" <<<"$features"; then
      observed+="${feature%%-*}=$(awk -v key="$feature" '$1 == key":" {print $2; exit}' <<<"$features"),"
    fi
  done
  observed=${observed%,}
  if [[ -z "$observed" ]]; then
    mark_unsupported network_offload driver-features-not-exposed
    return 0
  fi
  printf 'network_offload=PASS iface=%s mode=read-only features=%s\n' "$iface" "$observed"
}

test_container_runtime() {
  stage=container_runtime
  local runtime=''
  if command -v docker >/dev/null 2>&1; then runtime=docker; elif command -v podman >/dev/null 2>&1; then runtime=podman; fi
  if [[ -z "$runtime" ]]; then
    mark_unsupported container_runtime docker-or-podman-not-installed
    # The namespace and bridge tests above still provide kernel-level
    # container-network evidence; this result must not be mislabeled runtime.
    return 0
  fi
  if command -v timeout >/dev/null 2>&1; then
    if ! timeout 8 "$runtime" info >/dev/null 2>&1; then
      mark_unsupported container_runtime "${runtime}-daemon-unavailable"
      return 0
    fi
  elif ! "$runtime" info >/dev/null 2>&1; then
    mark_unsupported container_runtime "${runtime}-daemon-unavailable"
    return 0
  fi
  local version
  version=$("$runtime" version --format '{{.Server.Version}}' 2>/dev/null || true)
  if [[ -z "$version" ]]; then version=unknown; fi
  printf 'container_runtime=%s version=%s\n' "$runtime" "$version"
}

if ! require_commands; then
  printf 'network_acceptance=PARTIAL unsupported_capabilities=%s\n' "$unsupported"
  exit 0
fi

if test_veth_namespace; then
  # The NAT stage below tears down the first namespace topology, so inspect
  # the disposable host-side veth while it still exists.
  test_offload
else
  mark_unsupported network_veth_namespace capability-or-kernel-rejected
fi
test_bridge || { mark_unsupported network_bridge_veth capability-or-kernel-rejected; }
test_nat || { mark_unsupported network_nat capability-or-kernel-rejected; }
test_tunnel
test_container_runtime

if (( unsupported > 0 )); then
  printf 'network_acceptance=PARTIAL unsupported_capabilities=%s\n' "$unsupported"
else
  printf 'network_acceptance=PASS\n'
fi
