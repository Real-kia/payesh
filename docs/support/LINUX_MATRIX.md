# Linux support matrix (foundation CI)

Package 01 records these explicit representatives for compatibility checks:

| Family | Oldest representative | Current representative |
| --- | --- | --- |
| Ubuntu | 22.04 | 24.04 |
| Debian | 12 | 13 |
| Fedora | 42 | 43 |
| Rocky Linux | 9 | 10 |
| AlmaLinux | 9 | 10 |
| Alpine/OpenRC | 3.22 | 3.23 |

Ubuntu runs the Go test/lint and amd64/arm64 build matrix. The
`linux-distributions` CI job pulls the pinned distribution images, verifies
their release metadata and package-manager/init surfaces, and launches the
minimal Linux agent smoke artifact. It does not claim that privileged service, cgroup,
firewall, or OpenRC behavior is proven; those require disposable Linux VM
tests in the package that owns the behavior. No distribution outside this
matrix is a release gate.

`cmd/payesh-install` is intentionally side-effect-free and reports the target
OS, kernel, init system, package manager, cgroup mode, privilege, and
`ip`/`tc`/`nft` availability before any artifact or credential handling.
Non-Linux hosts and unsupported Linux releases fail closed with a specific
reason; missing optional control tools are warnings so monitoring-only roles
can still be evaluated.

## Disposable server acceptance runner

The repository includes `scripts/linux-acceptance.sh`. It detects the remote
CPU architecture (amd64, arm64, or 32-bit ARM), builds matching Linux
artifacts locally, streams one compressed bundle over SSH, runs collector,
SQLite/API, bandwidth/tc, nftables, module-socket, and capability checks, and
removes its dedicated `/tmp/payesh-linux-acceptance-*` directory on the remote
host. Credentials are supplied only through the SSH agent/key or `SSHPASS`; no
password is stored in the script.

Disposable TCP listeners in the remote runner bind to `127.0.0.1:0` and use
the address reported after the bind. The module management HTTP fixture and
namespace round-trip probes use the same bind-before-publish pattern. This
avoids a probe-then-release race with unrelated services on a shared host;
the runner never claims or rewrites an operator's existing listener.

Example:

```sh
SSHPASS='...' PAYESH_SSH_HOST=203.0.113.10 \
  scripts/linux-acceptance.sh
```

Set `PAYESH_SSH_BIND_ADDRESS` only when the local address is currently present.
Set `PAYESH_GOARCH`/`PAYESH_GOARM` only when overriding architecture detection.
For high-latency or rate-limited links, tune `PAYESH_SSH_SERVER_ALIVE_INTERVAL`,
`PAYESH_SSH_SERVER_ALIVE_COUNT`, `PAYESH_SSH_RETRIES`, and
`PAYESH_SSH_TRANSFER_TIMEOUT` (the upload/remote-run wall-clock limit).
Set `PAYESH_LINUX_THROUGHPUT=1` to run the optional bounded tc throughput
probe, or `PAYESH_ACCEPTANCE_REUSE_OWNED_PORT_TABLE=1` only when the existing
nftables table is already Payesh-owned. These opt-ins are forwarded to the
remote runner and are never enabled implicitly.
The runner reports unsupported nftables/tc tools, missing cgroup-v2 CPU
delegation, foreign qdiscs/tables, missing optional commands, and other
capability limits (including non-root or missing `CAP_NET_ADMIN`) explicitly.
A host with skipped capabilities ends with `linux_acceptance=PARTIAL`; actual
test failures still return non-zero. Uploads and remote execution are bounded
by `PAYESH_SSH_TRANSFER_TIMEOUT`, and timeout cleanup removes only the exact
dedicated remote path.

Slow links can split the bundle with `PAYESH_ACCEPTANCE_PROFILE=core`,
`kernel`, `cpu`, `bandwidth`, `nft`, `network`, or `modules`; `all` remains the default.
The `kernel` profile is the aggregate of its three focused subprofiles. The runner reports compressed
bytes and elapsed progress. It uses a run-local `accept-new` known-hosts file
instead of disabling host-key checking, emits stage/line/command diagnostics,
and continues to restrict cleanup to the validated disposable `/tmp` path.

On 2026-09-12 the supplied Ubuntu 22 amd64 host passed the complete `core`
profile: collector identity/epoch, SQLite ingest/query, and authenticated API
startup against the populated database. The earlier one-off SIGSEGV did not
reproduce across repeated local launches or the fresh remote run. Subsequent
two-host testing demonstrated that slow uploads could leave an executable
truncated or open for writing; remote artifacts must be checksum-verified and
atomically published before execution.
The `kernel` profile passed the real tc backend. A later focused `cpu` profile
uses a transient systemd `Delegate=yes` unit, safely enables the controller
only in that disposable subtree, and passed real 200m quota enforcement
(201274 microseconds of CPU usage over 900 milliseconds). The first nft probe revealed
that nftables 1.0.2 omits persisted table and counter comments from JSON even
though text output retains them. After adding exact, fail-closed text
fallbacks, the focused `nft` profile passed real guarded
apply/snapshot/remove; the table and disposable directory were absent after
cleanup. The `network` profile creates PID-scoped veth namespaces, a bridged
veth pair, routed namespaces with an isolated nft masquerade table, and a TUN
device. On the supplied Ubuntu 22 host (2026-09-12), those veth/namespace,
bridge, NAT, and TUN checks passed. Podman 3.4.4 was then installed with its
minimal Ubuntu dependencies; the network profile reported
`container_runtime=podman`, and a disposable Alpine bridge container passed
outbound connectivity. The profile remains `PARTIAL` only for
`network_offload=UNSUPPORTED reason=disposable-veth-unavailable`. The
forwarding sysctl was restored to `0`, package-created Podman background units
were disabled, and no probe links, namespaces, nft table, container, image,
or temporary directory remained. This is kernel/Podman evidence, not
Docker-specific or provider-specific overlay-tunnel acceptance.

The read-only `scripts/shared-server-audit.sh` can inventory a live shared
host without installing or mutating anything. On 2026-09-12 it confirmed a
Debian 12 arm64/systemd host with cgroup v2, Docker, active container bridges,
and existing listeners/sessions. This is capability evidence only; shared-host
traffic and resource snapshots do not replace disposable firewall, qdisc,
cgroup, service, reboot, or load acceptance.
