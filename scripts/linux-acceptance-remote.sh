#!/usr/bin/env bash
set -Eeuo pipefail

current_stage=bootstrap
profile="${PAYESH_ACCEPTANCE_PROFILE:-all}"
profile_has() {
  [[ "$profile" == all || "$profile" == "$1" || ( "$profile" == modules && "$1" == core ) ||
     ( "$profile" == kernel && ( "$1" == cpu || "$1" == bandwidth || "$1" == nft ) ) ]]
}
on_error() {
  local status=$?
  printf '[error] stage=%s line=%s status=%s command=%q\n' "$current_stage" "${BASH_LINENO[0]:-unknown}" "$status" "${BASH_COMMAND:-unknown}" >&2
  return "$status"
}
trap on_error ERR

base=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# Keep state outside the artifact directory; cleanup must never remove a
# caller's checkout or shared binary directory.
work=$(mktemp -d "${TMPDIR:-/tmp}/payesh-remote-acceptance.XXXXXX")
chmod 700 "$work"
server_pid=''
module_pid=''
http_pid=''
cleanup() {
  for pid in "$module_pid" "$http_pid" "$server_pid"; do
    if [[ -n "$pid" ]]; then kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; fi
  done
  rm -rf -- "$work"
}
trap cleanup EXIT

unsupported_count=0
mark_unsupported() {
  printf '%s=UNSUPPORTED reason=%s\n' "$1" "$2"
  unsupported_count=$((unsupported_count + 1))
}

if [[ -r /etc/os-release ]]; then
  source /etc/os-release
else
  ID=unknown
  VERSION_ID=unknown
fi
init_name=unknown
if command -v ps >/dev/null 2>&1; then init_name=$(ps -p 1 -o comm= | tr -d ' ' || true); fi
printf 'host_os=%s %s kernel=%s init=%s arch=%s\n' "$ID" "$VERSION_ID" "$(uname -r)" "$init_name" "$(uname -m)"
if command -v nft >/dev/null 2>&1; then nft --version; else echo 'nftables=UNAVAILABLE'; fi
if command -v tc >/dev/null 2>&1; then tc -V 2>&1 | head -n 1; else echo 'tc=UNAVAILABLE'; fi
controllers=''
if [[ -r /sys/fs/cgroup/cgroup.controllers ]]; then controllers=$(cat /sys/fs/cgroup/cgroup.controllers); fi
if grep -qw cpu <<<"$controllers"; then
  echo 'cpu_cgroup_v2=AVAILABLE'
else
  echo 'cpu_cgroup_v2=UNSUPPORTED reason=no-delegated-cpu-controller'
fi
if [[ -r /sys/fs/cgroup/cgroup.subtree_control ]] && grep -qw cpu /sys/fs/cgroup/cgroup.subtree_control; then
  echo 'cpu_cgroup_v2_delegated=AVAILABLE'
else
  echo 'cpu_cgroup_v2_delegated=UNSUPPORTED reason=cpu-not-enabled-for-child-cgroups'
fi
if (( EUID == 0 )); then
  echo 'privileges=root'
else
  echo 'privileges=UNSUPPORTED reason=root-or-cap_net_admin-required-for-kernel-tests'
fi

core_ready=1
if ! profile_has core; then core_ready=0; fi
for command_name in python3 curl; do
  if ! command -v "$command_name" >/dev/null 2>&1; then
    mark_unsupported "core_$command_name" "command-not-installed"
    core_ready=0
  fi
done

identity="$work/server-id"
db="$work/payesh.db"
server_id=''
if (( core_ready == 1 )); then
  current_stage=collector
  "$base/payesh-agent-linux" -identity-file "$identity" -root / > "$work/sample-1.json"
  "$base/payesh-agent-linux" -identity-file "$identity" -root / > "$work/sample-2.json"
  python3 - "$work/sample-1.json" "$work/sample-2.json" "$identity" <<'PY'
import json, pathlib, sys
a, b = [json.load(open(p)) for p in sys.argv[1:3]]
assert a['server_id'] == b['server_id']
assert a['collector_epoch'] != b['collector_epoch']
assert a['sequence'] == '0' and b['sequence'] == '0'
assert any(k.startswith('net.') and k.endswith('.rx_bytes') for k in a['counters'])
assert pathlib.Path(sys.argv[3]).read_text().strip() == a['server_id']
print('collector=PASS identity_epoch=PASS network_counters=PASS')
PY
  server_id=$(tr -d '\n' < "$identity")
  "$base/payesh-linux" register-server --db "$db" --identity-file "$identity" --ensure --name 'Reusable acceptance' --capabilities metrics,logs,traffic >/dev/null
  "$base/payesh-linux" ingest --db "$db" --identity-file "$identity" < "$work/sample-1.json" >/dev/null
  "$base/payesh-linux" ingest --db "$db" --identity-file "$identity" < "$work/sample-2.json" >/dev/null
  read -r from to < <(python3 - <<'PY'
from datetime import datetime, timedelta, timezone
now = datetime.now(timezone.utc)
print((now - timedelta(minutes=2)).strftime('%Y-%m-%dT%H:%M:%SZ'), (now + timedelta(minutes=2)).strftime('%Y-%m-%dT%H:%M:%SZ'))
PY
  )
  "$base/payesh-linux" metrics --db "$db" --server-id "$server_id" --from "$from" --to "$to" --limit 10 > "$work/metrics.json"
  python3 - "$work/metrics.json" <<'PY'
import json, sys
p=json.load(open(sys.argv[1]))
assert len(p['samples']) == 2
network_keys = [key for key in p['coverage'] if key.startswith('net.') and key.endswith('.rx_bytes')]
assert network_keys and any(p['coverage'][key] == 1 for key in network_keys)
print('sqlite_ingest_query=PASS')
PY
else
  if profile_has core; then
    echo 'core_acceptance=SKIP reason=missing-required-command'
  else
    echo "core_acceptance=SKIP reason=profile-$profile"
  fi
fi

if (( core_ready == 1 )); then
  current_stage=local_api
  # Port 0 delegates selection and reservation to the kernel. The server
  # announces the resolved endpoint only after binding, avoiding a
  # check-then-bind race with unrelated host services.
  PAYESH_LOCAL_TOKEN=acceptance-token "$base/payesh-server-linux" -db "$db" -listen 127.0.0.1:0 >/dev/null 2>"$work/server.err" & server_pid=$!
  server_port=''
  server_ready=0
  for _ in $(seq 1 40); do
    if [[ -z "$server_port" ]]; then
      server_port=$(sed -nE 's/.*payesh-server listening on 127\.0\.0\.1:([0-9]+).*/\1/p' "$work/server.err" | tail -n 1 || true)
    fi
    if [[ -n "$server_port" ]] && curl -fsS "http://127.0.0.1:$server_port/healthz" >"$work/health.json" 2>/dev/null; then
      server_ready=1
      break
    fi
    sleep 0.2
  done
  if (( server_ready == 0 )); then
    cat "$work/server.err" >&2 || true
    echo 'local_api=FAIL server did not become ready' >&2
    exit 1
  fi
  curl -sS -o "$work/unauth.json" -w '%{http_code}' "http://127.0.0.1:$server_port/api/v1/servers" >"$work/unauth.code"
  curl -fsS -H 'Authorization: Bearer acceptance-token' "http://127.0.0.1:$server_port/api/v1/servers?limit=10" >"$work/servers.json"
  python3 - "$work/health.json" "$work/unauth.code" "$work/servers.json" "$server_id" <<'PY'
import json, sys
assert json.load(open(sys.argv[1]))['status'] == 'ok'
assert open(sys.argv[2]).read() == '401'
assert any(x['id'] == sys.argv[4] for x in json.load(open(sys.argv[3]))['items'])
print('local_api_auth=PASS')
PY
  kill "$server_pid" 2>/dev/null || true; wait "$server_pid" 2>/dev/null || true; server_pid=''
fi

cpu_acceptance_ready=1
if ! profile_has cpu; then
  cpu_acceptance_ready=0
elif (( EUID != 0 )); then
  mark_unsupported cpu_control "root-required"
  cpu_acceptance_ready=0
elif ! grep -qw cpu <<<"$controllers"; then
  mark_unsupported cpu_control "cgroup-v2-cpu-controller-unavailable"
  cpu_acceptance_ready=0
elif command -v systemd-run >/dev/null 2>&1 && [[ "$(cat /proc/1/comm 2>/dev/null || true)" == systemd ]]; then
  # The test is launched below a transient Delegate=yes unit. Its delegated
  # cgroup, rather than the SSH session's root cgroup, owns the subtree-control
  # boundary that the probe is allowed to change.
  :
elif [[ ! -r /sys/fs/cgroup/cgroup.subtree_control ]] || ! grep -qw cpu /sys/fs/cgroup/cgroup.subtree_control; then
  mark_unsupported cpu_control "cgroup-v2-cpu-controller-not-delegated"
  cpu_acceptance_ready=0
fi
if (( cpu_acceptance_ready == 1 )); then
  current_stage=cpu_control
  cpu_test_output="$work/cpucontrol-acceptance.log"
  cpu_command=("$base/cpucontrol-acceptance" -test.run TestLinuxCPUControlAcceptance -test.v)
  if command -v systemd-run >/dev/null 2>&1 && [[ "$(cat /proc/1/comm 2>/dev/null || true)" == systemd ]]; then
    # Delegate a transient service subtree so the test can create only its own
    # child cgroup. The Go probe derives this service's cgroup from procfs.
    cpu_command=(systemd-run --quiet --wait --pipe --collect --property=Delegate=yes --setenv=PAYESH_LINUX_ACCEPTANCE=1 --setenv=PAYESH_ACCEPTANCE_ENABLE_CPU_CONTROLLER=1 "${cpu_command[@]}")
  else
    cpu_command=(env PAYESH_LINUX_ACCEPTANCE=1 "${cpu_command[@]}")
  fi
  if ! "${cpu_command[@]}" >"$cpu_test_output" 2>&1; then
    cat "$cpu_test_output" >&2
    echo 'cpu_control=FAIL' >&2
    exit 1
  fi
  cat "$cpu_test_output"
  if grep -q -- '--- SKIP:' "$cpu_test_output"; then
    mark_unsupported cpu_control "acceptance-test-reported-unsupported"
  else
    echo 'cpu_control=PASS'
  fi
fi

if ! profile_has bandwidth; then
  :
elif (( EUID != 0 )); then
  mark_unsupported bandwidth_tc "root-required"
elif command -v ip >/dev/null 2>&1 && command -v tc >/dev/null 2>&1; then
  current_stage=bandwidth_tc
  bash "$base/linux-network-acceptance.sh" --run-isolated env PAYESH_LINUX_ACCEPTANCE=1 "$base/bandwidth-acceptance" -test.run TestLinuxTCBackendAcceptance -test.v
  echo 'foreign_host_qdisc=NOT_TESTED reason=isolated-network-namespace'
  echo 'bandwidth_tc=PASS'
  if [[ "${PAYESH_LINUX_THROUGHPUT:-0}" == "1" ]]; then
    throughput_output="$work/throughput.log"
    if ! bash "$base/linux-network-acceptance.sh" --run-isolated env PAYESH_LINUX_THROUGHPUT=1 "$base/bandwidth-acceptance" -test.run TestLinuxTCThroughputAcceptance -test.v >"$throughput_output" 2>&1; then
      cat "$throughput_output" >&2
      echo 'bandwidth_throughput=FAIL' >&2
      exit 1
    fi
    cat "$throughput_output"
    if grep -q -- '--- SKIP:' "$throughput_output"; then
      mark_unsupported bandwidth_throughput "traffic-path-or-capability-unavailable"
    else
      echo 'bandwidth_throughput=PASS'
    fi
  fi
  if [[ "${PAYESH_LINUX_QUOTA_OVERSHOOT:-0}" == "1" ]]; then
    quota_output="$work/quota-overshoot.log"
    if ! bash "$base/linux-network-acceptance.sh" --run-isolated env PAYESH_LINUX_QUOTA_OVERSHOOT=1 "$base/bandwidth-acceptance" -test.run TestBandwidthQuotaOvershootAcceptance -test.v >"$quota_output" 2>&1; then
      cat "$quota_output" >&2
      echo 'bandwidth_quota_overshoot=FAIL' >&2
      exit 1
    fi
    cat "$quota_output"
    if grep -q -- '--- SKIP:' "$quota_output"; then
      mark_unsupported bandwidth_quota_overshoot "ledger-acceptance-capability-unavailable"
    else
      echo 'bandwidth_quota_overshoot=PASS'
    fi
  fi
else
  mark_unsupported bandwidth_tc "ip-or-tc-not-installed"
fi
if ! profile_has nft; then
  :
elif (( EUID != 0 )); then
  mark_unsupported port_traffic_nft "root-required"
elif command -v nft >/dev/null 2>&1 && nft list tables >/dev/null 2>&1; then
  current_stage=port_traffic_nft
  nft_output="$work/nftables.log"
  if ! bash "$base/linux-network-acceptance.sh" --run-isolated env PAYESH_LINUX_ACCEPTANCE=1 "$base/porttraffic-acceptance" -test.run TestLinuxNftBackendAcceptance -test.v >"$nft_output" 2>&1; then
    cat "$nft_output" >&2
    echo 'port_traffic_nft=FAIL' >&2
    exit 1
  fi
  cat "$nft_output"
  if grep -q -- '--- SKIP:' "$nft_output"; then
    mark_unsupported port_traffic_nft "ownership-or-kernel-capability-unavailable"
  else
    echo 'port_traffic_nft=PASS'
    accounting_output="$work/nft-accounting.log"
    if ! bash "$base/linux-network-acceptance.sh" --run-isolated env PAYESH_LINUX_ACCOUNTING=1 "$base/porttraffic-acceptance" -test.run '^TestLinuxNftUDPAccountingAcceptance$' -test.v >"$accounting_output" 2>&1; then
      cat "$accounting_output" >&2
      echo 'port_traffic_udp_accounting=FAIL' >&2
      exit 1
    fi
    cat "$accounting_output"
    if grep -q -- '--- SKIP:' "$accounting_output"; then
      mark_unsupported port_traffic_udp_accounting "accounting-fixture-reported-unsupported"
    else
      echo 'port_traffic_udp_accounting=PASS topology=local-ipv4-loopback'
    fi
  fi
else
  if command -v nft >/dev/null 2>&1; then
    mark_unsupported port_traffic_nft "nft-permission-or-cap_net_admin"
  else
    mark_unsupported port_traffic_nft "nft-not-installed"
  fi
fi

if profile_has network; then
  current_stage=network_topology
  network_output="$work/network.log"
  if ! bash "$base/linux-network-acceptance.sh" >"$network_output" 2>&1; then
    cat "$network_output" >&2
    echo 'network_acceptance=FAIL' >&2
    exit 1
  fi
  cat "$network_output"
  if grep -q '^network_acceptance=PARTIAL' "$network_output"; then
    mark_unsupported network_topology "capability-or-runtime-unavailable"
  else
    echo 'network_topology=PASS'
  fi
fi

if profile_has modules; then
  current_stage=bandwidth_module_socket
  # Bind the disposable management endpoint directly on port 0 and publish
  # the selected port only after bind. This cannot collide with a live host
  # service and has no probe-then-release window.
  python3 - "$work/http-port" >/dev/null 2>"$work/http.err" <<'PY' & http_pid=$!
import http.server, pathlib, sys
server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), http.server.SimpleHTTPRequestHandler)
path = pathlib.Path(sys.argv[1])
path.write_text(str(server.server_address[1]), encoding="ascii")
server.serve_forever()
PY
  http_port=''
  for _ in $(seq 1 40); do
    if [[ -s "$work/http-port" ]]; then http_port=$(tr -d '[:space:]' < "$work/http-port"); break; fi
    sleep 0.05
  done
  [[ "$http_port" =~ ^[0-9]+$ ]] || { echo 'management HTTP listener did not become ready' >&2; exit 1; }
  "$base/payesh-bandwidth-module-linux" --db "$db" --server-id "$server_id" --state-dir "$work/bandwidth" --socket "$work/bandwidth.sock" --management-confirm-url "http://127.0.0.1:$http_port" >/dev/null 2>"$work/module.err" & module_pid=$!
  module_ready=0
  for _ in $(seq 1 40); do
    if [[ -S "$work/bandwidth.sock" ]]; then
      module_ready=1
      break
    fi
    sleep 0.2
  done
  if (( module_ready == 0 )); then
    cat "$work/module.err" >&2 || true
    echo 'bandwidth_module_socket=FAIL module did not create its socket' >&2
    exit 1
  fi
  curl --silent --show-error --fail --unix-socket "$work/bandwidth.sock" "http://localhost/api/v1/servers/$server_id/bandwidth-policies" > "$work/policies.json"
  python3 - "$work/policies.json" <<'PY'
import json, sys
assert json.load(open(sys.argv[1]))['items'] == []
print('bandwidth_module_socket=PASS')
PY
  kill "$module_pid" "$http_pid" 2>/dev/null || true
  wait "$module_pid" 2>/dev/null || true
  wait "$http_pid" 2>/dev/null || true
  module_pid=''; http_pid=''
fi
current_stage=summary
if (( unsupported_count > 0 )); then
  echo "linux_acceptance=PARTIAL unsupported_capabilities=$unsupported_count"
else
  echo 'linux_acceptance=PASS'
fi
